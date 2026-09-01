package harmonic

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var harmonicStart = time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC) // a Monday 00:00

const slot = 15 * time.Minute

type wave struct {
	periodSlots float64
	amp         float64
	phase       float64
}

// synth builds an evenly sampled series of n slots: mean plus the given sinusoids
// plus optional gaussian noise (seeded for determinism).
func synth(n int, mean float64, sigma float64, waves ...wave) []float64 {
	rng := rand.New(rand.NewSource(1))
	y := make([]float64, n)
	for i := range y {
		v := mean
		for _, c := range waves {
			v += c.amp * math.Cos(2*math.Pi*float64(i)/c.periodSlots+c.phase)
		}
		if sigma > 0 {
			v += rng.NormFloat64() * sigma
		}
		y[i] = v
	}
	return y
}

// nearestPeriod returns the detected period (in slots) closest to want, for
// tolerance checks that don't care about the other lines.
func nearestPeriod(m *Model, wantSlots float64) float64 {
	best := math.Inf(1)
	for _, p := range m.Periods() {
		got := p.Seconds() / slot.Seconds()
		if math.Abs(got-wantSlots) < math.Abs(best-wantSlots) {
			best = got
		}
	}
	return best
}

func TestHarmonicRecoversSingleCycle(t *testing.T) {
	const n, period = 2000, 100.0
	y := synth(n, 5, 0, wave{period, 3, 0.7})

	m := Fit(harmonicStart, slot, y, Options{})

	require.NotEmpty(t, m.comps)
	assert.InEpsilon(t, period, nearestPeriod(m, period), 0.02, "period within 2%")

	// in-sample reconstruction is near-exact for a clean sinusoid
	for i := 0; i < n; i += 37 {
		assert.InDelta(t, y[i], m.At(harmonicStart.Add(time.Duration(i)*slot)), 0.05)
	}
}

func TestHarmonicForecastsDailyPlusWeekly(t *testing.T) {
	const day, week = 96.0, 672.0
	const n = 28 * 96 // 28 days
	y := synth(n, 4, 0.15, wave{day, 2, 0}, wave{week, 1, 1.2})

	m := Fit(harmonicStart, slot, y, Options{})

	assert.InEpsilon(t, day, nearestPeriod(m, day), 0.03, "daily line")
	assert.InEpsilon(t, week, nearestPeriod(m, week), 0.03, "weekly line")

	// forecast the next two days against the noise-free generating function;
	// horizon << span, so frequency-resolution drift is negligible
	truth := func(i int) float64 {
		return 4 + 2*math.Cos(2*math.Pi*float64(i)/day) + math.Cos(2*math.Pi*float64(i)/week+1.2)
	}
	var maxErr float64
	for i := n; i < n+2*96; i++ {
		got := m.At(harmonicStart.Add(time.Duration(i) * slot))
		maxErr = math.Max(maxErr, math.Abs(got-truth(i)))
	}
	assert.Less(t, maxErr, 0.3, "2-day forecast tracks the true signal")
}

func TestHarmonicDiscoversFortnightlyCycle(t *testing.T) {
	// "gone every other Thursday" is a 14-day period - not daily, not weekly.
	// It must be discovered from the data, not assumed.
	const fortnight = 14 * 96.0
	const n = 56 * 96 // 8 weeks, 4 cycles of the fortnight
	y := synth(n, 3, 0.1, wave{96, 1.5, 0}, wave{fortnight, 1, 0.4})

	// wider MinPeriod keeps the grid small and fast; daily and fortnightly still fit
	m := Fit(harmonicStart, slot, y, Options{MinPeriod: 12 * time.Hour})

	assert.InEpsilon(t, fortnight, nearestPeriod(m, fortnight), 0.05, "14-day line discovered")
}

func TestHarmonicIgnoresNoise(t *testing.T) {
	y := synth(3000, 7, 1.0) // pure noise around a mean

	m := Fit(harmonicStart, slot, y, Options{FAP: 0.001})

	assert.Empty(t, m.comps, "no spurious cycles fitted to white noise")
	// with no components At is flat at the sample mean, which sits within noise of 7
	flat := m.At(harmonicStart.Add(1000 * slot))
	assert.Equal(t, m.At(harmonicStart.Add(9999*slot)), flat, "flat without components")
	assert.InDelta(t, 7, flat, 0.1)
}

func TestHarmonicEmptySeries(t *testing.T) {
	m := Fit(harmonicStart, slot, nil, Options{})
	assert.Empty(t, m.comps)
	assert.Equal(t, 0.0, m.At(harmonicStart))
}

// BenchmarkFit measures a fit at the real-data scale (~120 days at 15min): the cost
// of a recompute, which is intended to run about once a day, not per optimizer cycle.
func BenchmarkFit(b *testing.B) {
	y := synth(120*96, 4, 0.15, wave{96, 2, 0}, wave{672, 1, 1.2})
	for b.Loop() {
		Fit(harmonicStart, slot, y, Options{})
	}
}

// BenchmarkAt measures a single forecast evaluation, which runs per slot per cycle.
func BenchmarkAt(b *testing.B) {
	y := synth(120*96, 4, 0.15, wave{96, 2, 0}, wave{672, 1, 1.2})
	m := Fit(harmonicStart, slot, y, Options{})
	var i int
	for b.Loop() {
		m.At(harmonicStart.Add(time.Duration(i) * slot))
		i++
	}
}

func TestHarmonicTooLittleData(t *testing.T) {
	// a clean daily sinusoid, but only 3 days - far too short to resolve the multi-day
	// cycles asked for. The model degrades to a flat mean instead of inventing a cycle,
	// which is the safe fallback (the caller keeps using the time-of-day average).
	y := synth(3*96, 5, 0, wave{96, 3, 0})

	m := Fit(harmonicStart, slot, y, Options{MinPeriod: 36 * time.Hour})

	require.Empty(t, m.comps, "no cycle fitted without enough span")
	mean := m.At(harmonicStart)
	assert.InDelta(t, 5, mean, 0.01, "flat at the mean")
	assert.Equal(t, mean, m.At(harmonicStart.Add(500*slot)), "flat everywhere - no invented periodicity")
	assert.Equal(t, mean, m.At(harmonicStart.Add(10000*slot)))
}

func TestHarmonicCVInsufficientData(t *testing.T) {
	// cross-validated selection with too few training samples to hold out returns a
	// flat mean-only model rather than panicking on the empty split
	y := synth(18, 2, 0, wave{6, 1, 0})

	m := Fit(harmonicStart, slot, y, Options{Validate: 0.2, MinPeriod: 30 * time.Minute})

	assert.Empty(t, m.comps)
	assert.InDelta(t, 2, m.At(harmonicStart), 0.01)
}

// carOccupancy builds a car-like connection series (0..1): plugged in overnight and
// on weekends, gone 09:00-17:00 on weekdays, and gone all day every other Thursday -
// exactly the weekday/weekend + biweekly structure the time-of-day average cannot see.
func carOccupancy(n int) []float64 {
	rng := rand.New(rand.NewSource(2))
	y := make([]float64, n)
	for i := range y {
		day := i / 96
		dow := day % 7 // 0 = Monday (harmonicStart is a Monday)
		sod := i % 96
		v := 1.0
		switch {
		case dow >= 5: // weekend: home all day
		case dow == 3 && (day/7)%2 == 0: // every other Thursday: gone all day
			v = 0
		case sod >= 36 && sod < 68: // weekday 09:00-17:00: at work
			v = 0
		}
		y[i] = v + rng.NormFloat64()*0.05
	}
	return y
}

// TestHarmonicCarLikeBeatsTimeOfDayAverage is the payoff case: on a signal whose
// structure is weekly and biweekly (not daily), the CV-selected hybrid - phase-locked
// time-of-day average plus a harmonic model of the residual - forecasts the held-out
// week markedly better than the time-of-day average alone, which blends weekday and
// weekend into a useless midday value.
func TestHarmonicCarLikeBeatsTimeOfDayAverage(t *testing.T) {
	const n, hold = 56 * 96, 7 * 96 // 8 weeks, hold out the last
	y := carOccupancy(n)
	train := y[:n-hold]

	// time-of-day average (relative to start), the current approach
	var todSum, todCnt [96]float64
	for i, v := range train {
		todSum[i%96] += v
		todCnt[i%96]++
	}
	tod := func(i int) float64 { return todSum[i%96] / todCnt[i%96] }

	// hybrid: daily average + CV-selected harmonic residual (multi-day cycles only)
	resid := make([]float64, len(train))
	for i := range train {
		resid[i] = train[i] - tod(i)
	}
	mr := Fit(harmonicStart, slot, resid, Options{MinPeriod: 36 * time.Hour, Validate: 0.2})

	var rmseBase, rmseHybrid float64
	for i := n - hold; i < n; i++ {
		ts := harmonicStart.Add(time.Duration(i) * slot)
		eb := tod(i) - y[i]
		eh := tod(i) + mr.At(ts) - y[i]
		rmseBase += eb * eb
		rmseHybrid += eh * eh
	}
	rmseBase = math.Sqrt(rmseBase / hold)
	rmseHybrid = math.Sqrt(rmseHybrid / hold)
	t.Logf("car holdout RMSE: time-of-day-avg=%.3f hybrid=%.3f (%.0f%% of baseline), residual cycles: %v",
		rmseBase, rmseHybrid, 100*rmseHybrid/rmseBase, mr.Periods())

	assert.NotEmpty(t, mr.comps, "hybrid must discover the weekly/biweekly structure")
	assert.Less(t, rmseHybrid, 0.9*rmseBase, "hybrid clearly beats the blended average")
}
