package metrics

import (
	"time"

	"github.com/evcc-io/evcc/core/metrics/harmonic"
	"github.com/evcc-io/evcc/tariff"
)

// residualMinPeriod keeps the harmonic residual above the daily family that the
// civil-time seasonal already removes, so the two layers do not double-count the day.
const residualMinPeriod = 36 * time.Hour

// residualValidate holds out this fraction of the residual history to select harmonic
// cycles by out-of-sample gain, so only genuinely predictive multi-day cycles survive.
const residualValidate = 0.2

// HybridForecast predicts a recorded series at future times by combining a phase-locked
// civil-time daily seasonal (a per-local-time-of-day average, DST-correct by
// construction) with a harmonic model of the residual that captures whatever multi-day
// cycles the data contains (weekly, biweekly, ...). See Collector.Forecast.
type HybridForecast struct {
	seasonal [96]float64     // civil time-of-day average, in the collector's stored unit
	residual *harmonic.Model // multi-day residual in the same unit
}

// At returns the predicted value at t: the daily average for t's local time-of-day
// plus the residual model's multi-day contribution.
func (f *HybridForecast) At(t time.Time) float64 {
	return f.seasonal[todIndex(t)] + f.residual.At(t)
}

// todIndex maps a time to its local 15min time-of-day slot (0..95), matching the
// localtime grouping energyProfile uses to build the seasonal.
func todIndex(t time.Time) int {
	return (t.Hour()*60 + t.Minute()) / int(tariff.SlotDuration.Minutes())
}

// Forecast fits a hybrid model to the entity's history since from: a civil-time daily
// seasonal plus a cross-validated harmonic model of the residual. It returns
// ErrIncomplete when there is not enough history for the seasonal.
func (c *Collector) Forecast(from time.Time) (*HybridForecast, error) {
	seasonal, err := energyProfile(c.entity, from)
	if err != nil {
		return nil, err
	}

	start, series, err := meterSeries(c.entity, from)
	if err != nil {
		return nil, err
	}

	// the residual carries only what the daily seasonal does not explain
	resid := make([]float64, len(series))
	for i := range series {
		t := start.Add(time.Duration(i) * tariff.SlotDuration)
		resid[i] = series[i] - seasonal[todIndex(t)]
	}

	model := harmonic.Fit(start, tariff.SlotDuration, resid, harmonic.Options{
		MinPeriod: residualMinPeriod,
		Validate:  residualValidate,
	})

	return &HybridForecast{seasonal: *seasonal, residual: model}, nil
}
