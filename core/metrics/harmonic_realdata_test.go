//go:build realdata

package metrics

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/metrics/harmonic"
	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/tariff"
	"github.com/stretchr/testify/require"
)

// TestRealDataHarmonic validates the harmonic detector against a real evcc
// database. It is excluded from normal builds (build tag realdata). Run with:
//
//	go test ./core/metrics -tags realdata -run RealData -v
//	EVCC_DB=/path/to/evcc.db go test ./core/metrics -tags realdata -run RealData -v
//
// It copies the database to a temp file first, so the live database is never
// written (NewInstance runs migrations).

// realDB copies the live evcc database (EVCC_DB or ~/.evcc/evcc.db) to a temp file
// and points db.Instance at the copy, so tests never write the live database.
func realDB(t *testing.T) {
	t.Helper()
	src := os.Getenv("EVCC_DB")
	if src == "" {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".evcc", "evcc.db")
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no database at %s (set EVCC_DB)", src)
	}
	tmp := filepath.Join(t.TempDir(), "evcc.db")
	require.NoError(t, copyFile(src, tmp))
	require.NoError(t, db.NewInstance("sqlite", tmp))
}

func TestRealDataHarmonic(t *testing.T) {
	realDB(t)

	var e entity
	require.NoError(t, db.Instance.Where(`"group" = ?`, Home).First(&e).Error)

	from := time.Now().AddDate(0, 0, -120) // up to ~4 months of history
	start, series, err := meterSeries(e, from)
	require.NoError(t, err)

	n := len(series)
	t.Logf("home series: %d slots (%.1f days) from %s", n, float64(n)/96, start.Format("2006-01-02 15:04"))

	m := harmonic.Fit(start, tariff.SlotDuration, series, harmonic.Options{})
	t.Logf("detected %d periods (longest first):", len(m.Periods()))
	for _, p := range m.Periods() {
		t.Logf("  %6.2f days  (%.1f h)", p.Hours()/24, p.Hours())
	}

	// production path: the wired Collector.Forecast (DST-correct civil-time seasonal +
	// CV harmonic residual). Confirm it assembles and predicts finite, sane values.
	col, err := NewCollector(Home, Home, Home)
	require.NoError(t, err)
	hf, err := col.Forecast(from)
	require.NoError(t, err)

	slotBase := time.Now().Truncate(tariff.SlotDuration)
	minV, maxV := math.Inf(1), math.Inf(-1)
	for i := range 7 * 96 {
		v := hf.At(slotBase.Add(time.Duration(i) * tariff.SlotDuration))
		require.False(t, math.IsNaN(v) || math.IsInf(v, 0), "finite forecast at slot %d", i)
		minV, maxV = math.Min(minV, v), math.Max(maxV, v)
	}
	t.Logf("Collector.Forecast next-7-day range: %.3f .. %.3f kWh/slot", minV, maxV)

	// holdout backtest: fit on all but the last 7 days, predict them, and compare
	// RMSE to the time-of-day-average baseline (what the optimizer uses today)
	const hold = 7 * 96
	if n <= hold+14*96 {
		t.Skip("not enough history for a 7-day holdout")
	}

	train := series[:n-hold]

	// pure harmonic, significance (FAP) selection
	mFap := harmonic.Fit(start, tariff.SlotDuration, train, harmonic.Options{})
	// pure harmonic, cross-validated selection
	mCV := harmonic.Fit(start, tariff.SlotDuration, train, harmonic.Options{Validate: 0.2})

	// baseline: phase-locked average by time-of-day slot (relative to start) over train
	var todSum, todCnt [96]float64
	for i, v := range train {
		todSum[i%96] += v
		todCnt[i%96]++
	}
	tod := func(i int) float64 { return todSum[i%96] / todCnt[i%96] }

	// hybrid: phase-locked daily average + CV-selected harmonic model of the residual,
	// which only carries the multi-day structure the average is blind to (MinPeriod
	// skips the 24h family the average already removes)
	resid := make([]float64, len(train))
	for i := range train {
		resid[i] = train[i] - tod(i)
	}
	mr := harmonic.Fit(start, tariff.SlotDuration, resid, harmonic.Options{MinPeriod: 36 * time.Hour, Validate: 0.2})
	t.Logf("hybrid residual (multi-day) periods kept: %d", len(mr.Periods()))
	for _, p := range mr.Periods() {
		t.Logf("  %6.2f days", p.Hours()/24)
	}

	rmse := func(pred func(i int, ts time.Time) float64) float64 {
		var e float64
		for i := n - hold; i < n; i++ {
			ts := start.Add(time.Duration(i) * tariff.SlotDuration)
			d := pred(i, ts) - series[i]
			e += d * d
		}
		return math.Sqrt(e / hold)
	}

	base := rmse(func(i int, _ time.Time) float64 { return tod(i) })
	t.Logf("7-day holdout RMSE:")
	t.Logf("  time-of-day-avg (baseline) = %.4f", base)
	t.Logf("  harmonic (FAP)             = %.4f  (%.0f%%)", rmse(func(_ int, ts time.Time) float64 { return mFap.At(ts) }), 100*rmse(func(_ int, ts time.Time) float64 { return mFap.At(ts) })/base)
	t.Logf("  harmonic (CV)              = %.4f  (%.0f%%)", rmse(func(_ int, ts time.Time) float64 { return mCV.At(ts) }), 100*rmse(func(_ int, ts time.Time) float64 { return mCV.At(ts) })/base)
	t.Logf("  hybrid = ToD + CV residual = %.4f  (%.0f%%)", rmse(func(i int, ts time.Time) float64 { return tod(i) + mr.At(ts) }), 100*rmse(func(i int, ts time.Time) float64 { return tod(i) + mr.At(ts) })/base)
}

// TestRealDataConnectedForecast demonstrates the EV connection forecast on the
// backfilled connected data (M-F 07:15-17:00 disconnected, weekends connected). It
// contrasts the time-of-day-average baseline - which blends weekday and weekend into
// one useless midday value - against the hybrid, which should recover the weekday vs
// weekend distinction at midday.
func TestRealDataConnectedForecast(t *testing.T) {
	realDB(t)

	col, err := NewCollector(Connected, "lp-1", "Home")
	require.NoError(t, err)

	from := time.Now().AddDate(0, 0, -forecastWindowDays)
	seasonal, err := energyProfile(col.entity, from)
	require.NoError(t, err)
	hf, err := col.Forecast(from)
	require.NoError(t, err)

	// predicted connected minutes at local noon for the next 8 days: the seasonal alone
	// is flat (weekday/weekend blended), the hybrid should split ~0 on weekdays and ~15
	// on weekends
	n := time.Now()
	noon := time.Date(n.Year(), n.Month(), n.Day(), 12, 0, 0, 0, time.Local)
	t.Logf("predicted connected minutes at 12:00 (seasonal-only vs hybrid):")
	for d := range 8 {
		day := noon.AddDate(0, 0, d)
		t.Logf("  %s: seasonal=%5.1f  hybrid=%5.1f", day.Format("Mon 01-02"), seasonal[todIndex(day)], hf.At(day))
	}
}

const forecastWindowDays = 90

// weightedSeasonal computes a per-time-of-day average over the first nTrain samples,
// weighting each day by exp(-ln2 * ageDays / halflife) so recent days count more. A
// halflife <= 0 means a flat average (every day weighted equally), i.e. today's code.
func weightedSeasonal(series []float64, start time.Time, nTrain int, halflife float64) [96]float64 {
	var wsum, wtot [96]float64
	last := float64(nTrain - 1)
	for i := range nTrain {
		k := todIndex(start.Add(time.Duration(i) * tariff.SlotDuration))
		w := 1.0
		if halflife > 0 {
			w = math.Exp(-math.Ln2 * (last - float64(i)) / 96.0 / halflife)
		}
		wsum[k] += w * series[i]
		wtot[k] += w
	}
	var s [96]float64
	for k := range s {
		if wtot[k] > 0 {
			s[k] = wsum[k] / wtot[k]
		}
	}
	return s
}

// TestRealDataRecencyWeighting sweeps exponential half-lives for the home-load daily
// seasonal and reports 7-day-holdout RMSE, seasonal-only and full hybrid, against the
// flat average (today's behavior). It answers whether recency weighting helps.
func TestRealDataRecencyWeighting(t *testing.T) {
	realDB(t)

	var e entity
	require.NoError(t, db.Instance.Where(`"group" = ?`, Home).First(&e).Error)

	start, series, err := meterSeries(e, time.Now().AddDate(0, 0, -forecastWindowDays))
	require.NoError(t, err)
	n := len(series)
	const hold = 7 * 96
	require.Greater(t, n, hold+14*96, "need enough history for a 7-day holdout")
	nTrain := n - hold

	rmse := func(pred func(i int, ts time.Time) float64) float64 {
		var se float64
		for i := nTrain; i < n; i++ {
			d := pred(i, start.Add(time.Duration(i)*tariff.SlotDuration)) - series[i]
			se += d * d
		}
		return math.Sqrt(se / hold)
	}

	t.Logf("home-load 7-day holdout RMSE by seasonal half-life (%.0f days history):", float64(n)/96)
	t.Logf("  %-12s %-12s %-12s", "half-life", "seasonal", "hybrid")
	for _, hl := range []float64{0, 45, 30, 21, 14, 7} { // 0 = flat (current)
		seasonal := weightedSeasonal(series, start, nTrain, hl)

		resid := make([]float64, nTrain)
		for i := range nTrain {
			k := todIndex(start.Add(time.Duration(i) * tariff.SlotDuration))
			resid[i] = series[i] - seasonal[k]
		}
		mr := harmonic.Fit(start, tariff.SlotDuration, resid, harmonic.Options{Validate: 0.2})

		seaRMSE := rmse(func(i int, ts time.Time) float64 { return seasonal[todIndex(ts)] })
		hybRMSE := rmse(func(i int, ts time.Time) float64 { return seasonal[todIndex(ts)] + mr.At(ts) })
		label := "flat"
		if hl > 0 {
			label = fmt.Sprintf("%.0fd", hl)
		}
		t.Logf("  %-12s %-12.4f %-12.4f", label, seaRMSE, hybRMSE)
	}
}

// synthConn builds a connection series (connected minutes/slot, 0..15): the car is
// gone leaveHour:00-17:00 every day, connected otherwise. Days on/after changeDay
// leave at newLeaveHour instead - a departure that shifts earlier partway through.
func synthConn(start time.Time, days, leaveHour, changeDay, newLeaveHour int) []float64 {
	y := make([]float64, days*96)
	for i := range y {
		ts := start.Add(time.Duration(i) * tariff.SlotDuration)
		leave := leaveHour
		if i/96 >= changeDay {
			leave = newLeaveHour
		}
		mod := ts.Hour()*60 + ts.Minute()
		if mod >= leave*60 && mod < 17*60 {
			y[i] = 0 // gone
		} else {
			y[i] = 15 // home
		}
	}
	return y
}

// TestSyntheticRecencyAdaptation is the adaptation benchmark: a daily departure that
// shifts 08:00 -> 07:00 starting `elapsed` days ago. It reports the predicted
// connected-minutes at the newly-vacated 07:00-08:00 window for each seasonal
// half-life. Ground truth after the change is 0 (gone), and < 7.5 means the model now
// predicts "gone" there - so lower/sooner is faster adaptation. This is what the
// steady-state RMSE test could not show.
func TestSyntheticRecencyAdaptation(t *testing.T) {
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC) // arbitrary; DST-free
	const histDays = 90
	window := []int{28, 29, 30, 31} // 07:00,07:15,07:30,07:45

	avgWindow := func(s [96]float64) float64 {
		var sum float64
		for _, k := range window {
			sum += s[k]
		}
		return sum / float64(len(window))
	}

	halflives := []float64{0, 45, 30, 14, 7} // 0 = flat (current behavior)
	hdr := "  days-since-change"
	for _, hl := range halflives {
		if hl == 0 {
			hdr += fmt.Sprintf("  %8s", "flat")
		} else {
			hdr += fmt.Sprintf("  %7.0fd", hl)
		}
	}
	t.Logf("predicted connected-min at the vacated 07:00-08:00 window (truth after change = 0;")
	t.Logf("< 7.5 => model now predicts 'gone'; lower/sooner = faster adaptation):")
	t.Log(hdr)
	for _, elapsed := range []int{0, 7, 14, 21, 28, 45} {
		hist := synthConn(start, histDays, 8, histDays-elapsed, 7)
		row := fmt.Sprintf("  %17d", elapsed)
		for _, hl := range halflives {
			row += fmt.Sprintf("  %8.1f", avgWindow(weightedSeasonal(hist, start, len(hist), hl)))
		}
		t.Log(row)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
