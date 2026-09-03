package metrics

import (
	"math"
	"time"

	"github.com/evcc-io/evcc/core/metrics/harmonic"
	"github.com/evcc-io/evcc/tariff"
)

// residualValidate holds out this fraction of the residual history to select harmonic
// cycles by out-of-sample gain, so only genuinely predictive cycles survive.
const residualValidate = 0.2

// seasonalHalflife recency-weights the daily seasonal: a day's contribution to the
// time-of-day average decays with this half-life, so the profile follows a changed
// routine instead of lagging a flat multi-month mean. ~21 days is the backtested sweet
// spot - steady-state RMSE indistinguishable from a flat average on real home load, yet
// ~2x faster to recognize a shifted schedule; shorter half-lives adapt faster but add
// real steady-state noise.
const seasonalHalflife = 21 * 24 * time.Hour

// HybridForecast predicts a recorded series at future times by combining a recency-
// weighted civil-time daily seasonal (a per-local-time-of-day average, DST-correct by
// construction, that follows a changed routine) with a harmonic model of the residual.
// The seasonal carries the daily level; the residual carries what the average misses -
// both the day-of-week modulation of the daily shape (e.g. a weekend that looks
// different from a weekday) and longer emergent cycles (weekly, biweekly, ...). See
// Collector.Forecast.
type HybridForecast struct {
	seasonal [96]float64     // recency-weighted civil time-of-day average, collector's unit
	residual *harmonic.Model // residual in the same unit
}

// At returns the predicted value at t: the daily average for t's local time-of-day
// plus the residual model's contribution.
func (f *HybridForecast) At(t time.Time) float64 {
	return f.seasonal[todIndex(t)] + f.residual.At(t)
}

// todIndex maps a time to its local 15min time-of-day slot (0..95).
func todIndex(t time.Time) int {
	return (t.Hour()*60 + t.Minute()) / int(tariff.SlotDuration.Minutes())
}

// recencySeasonal builds a per-local-time-of-day average of the evenly sampled series
// (sample i is the slot at start + i·slot), weighting each sample by
// exp(-ln2·age/halflife) so recent days count more. A halflife <= 0 gives a flat
// (equal-weight) average. It returns ErrIncomplete when the series does not span a full
// day, i.e. some time-of-day slot has no sample.
func recencySeasonal(start time.Time, series []float64, halflife time.Duration) ([96]float64, error) {
	wsum, wtot := weightedTodBuckets(start, series, halflife, false)

	var s [96]float64
	for k := range s {
		if wtot[k] == 0 {
			return s, ErrIncomplete
		}
		s[k] = wsum[k] / wtot[k]
	}
	return s, nil
}

// weightedTodBuckets accumulates an evenly sampled series (sample i is the slot at start +
// i·slot) into per-local-time-of-day sum and weight buckets, weighting each sample by
// exp(-ln2·age/halflife) so recent days count more (halflife <= 0 gives equal weight). With
// skipZero the zero samples are excluded, yielding a conditional average over the nonzero
// slots (see awayRateSeasonal); otherwise every sample counts (see recencySeasonal).
func weightedTodBuckets(start time.Time, series []float64, halflife time.Duration, skipZero bool) (wsum, wtot [96]float64) {
	n := len(series)
	halflifeDays := halflife.Hours() / 24
	for i, v := range series {
		if skipZero && v == 0 {
			continue
		}
		k := todIndex(start.Add(time.Duration(i) * tariff.SlotDuration))
		w := 1.0
		if halflifeDays > 0 {
			w = math.Exp(-math.Ln2 * float64(n-1-i) / 96 / halflifeDays) // 96 slots per day
		}
		wsum[k] += w * v
		wtot[k] += w
	}
	return wsum, wtot
}

// Forecast fits a hybrid model to the entity's history since from: a recency-weighted
// daily seasonal plus a cross-validated harmonic model of the residual. It returns
// ErrIncomplete when there is not enough history for the seasonal.
func (c *Collector) Forecast(from time.Time) (*HybridForecast, error) {
	start, series, err := meterSeries(c.entity, from)
	if err != nil {
		return nil, err
	}
	return newHybridForecast(start, series)
}

// newHybridForecast builds a hybrid model from an evenly sampled series (sample i is
// the slot at start + i·slot). Split from Forecast so a synthesized series (e.g. vehicle
// consumption derived from sessions) can be forecast the same way.
func newHybridForecast(start time.Time, series []float64) (*HybridForecast, error) {
	// the residual is taken against the flat (equal-weight) time-of-day mean. Subtracting
	// the per-time-of-day mean makes it orthogonal to every pure daily harmonic, so the
	// fit cannot re-model the daily cycle even with no period floor - what it picks up
	// near the daily frequency is the day-of-week modulation of the daily shape
	// (sidebands), the weekend-differs-from-weekday signal.
	flat, err := recencySeasonal(start, series, 0)
	if err != nil {
		return nil, err
	}

	resid := make([]float64, len(series))
	for i := range series {
		t := start.Add(time.Duration(i) * tariff.SlotDuration)
		resid[i] = series[i] - flat[todIndex(t)]
	}

	model := harmonic.Fit(start, tariff.SlotDuration, resid, harmonic.Options{
		Validate: residualValidate,
	})

	// the forecast level uses the recency-weighted mean instead of the flat one, so a
	// changed routine is followed within weeks rather than lagging the multi-month mean.
	// The residual stays orthogonal (fit against the flat mean) and only adds the shape.
	seasonal, err := recencySeasonal(start, series, seasonalHalflife)
	if err != nil {
		return nil, err
	}

	return &HybridForecast{seasonal: seasonal, residual: model}, nil
}
