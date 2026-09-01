package metrics

import (
	"time"

	"github.com/evcc-io/evcc/core/metrics/harmonic"
	"github.com/evcc-io/evcc/tariff"
)

// residualValidate holds out this fraction of the residual history to select harmonic
// cycles by out-of-sample gain, so only genuinely predictive cycles survive.
const residualValidate = 0.2

// HybridForecast predicts a recorded series at future times by combining a phase-locked
// civil-time daily seasonal (a per-local-time-of-day average, DST-correct by
// construction) with a harmonic model of the residual. The seasonal carries the average
// daily shape; the residual carries what the average misses - both the day-of-week
// modulation of that shape (e.g. a weekend that looks different from a weekday) and
// longer emergent cycles (weekly, biweekly, ...). See Collector.Forecast.
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

	// the residual is what the daily seasonal does not explain. Subtracting the
	// per-time-of-day mean makes it orthogonal to every pure daily harmonic, so the fit
	// cannot re-model the daily cycle even with no period floor - what it picks up near
	// the daily frequency is the day-of-week modulation of the daily shape (sidebands),
	// which is exactly the weekend-differs-from-weekday signal.
	resid := make([]float64, len(series))
	for i := range series {
		t := start.Add(time.Duration(i) * tariff.SlotDuration)
		resid[i] = series[i] - seasonal[todIndex(t)]
	}

	model := harmonic.Fit(start, tariff.SlotDuration, resid, harmonic.Options{
		Validate: residualValidate,
	})

	return &HybridForecast{seasonal: *seasonal, residual: model}, nil
}
