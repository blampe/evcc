package metrics

import (
	"time"

	"github.com/evcc-io/evcc/tariff"
)

// AwayEpisode is a period a vehicle was away from the charger with the amount it
// consumed while gone. The unit is whatever the caller supplies (kWh, km driven, or
// SoC points) - the forecast is unit-agnostic. Episodes are derived from the gaps
// between consecutive charging sessions (departure = previous session's disconnect,
// return = next session's connect), so no new metrics collector is needed.
type AwayEpisode struct {
	Departs  time.Time
	Returns  time.Time
	Consumed float64
}

// minConsumptionEpisodes is the fewest away-episodes before the consumption forecast is
// trusted; below it NewConsumptionForecast returns nil and the caller leaves the
// reconnect SoC unadjusted.
const minConsumptionEpisodes = 20

// ConsumptionForecast predicts how much a vehicle uses while away, from a per-time-of-
// day seasonal fitted to its driving history (the gaps between charging sessions).
// Time-of-day plus the away-window length carry the signal; day-of-week and harmonic
// structure did not help in backtests (they are already implicit in when/how-long the
// vehicle is out). The seasonal is away-conditional (see awayRateSeasonal): it averages
// only the slots the vehicle was actually out, because a forecast is only ever requested
// for a vehicle that IS away, so diluting by the probability it is home would under-
// predict a known trip.
type ConsumptionForecast struct {
	seasonal [96]float64 // recency-weighted per-time-of-day away-rate per slot (AwayEpisode unit)
}

// NewConsumptionForecast fits a forecast from a vehicle's away-episodes (which must be
// sorted by departure), or returns nil when there are too few to be reliable.
func NewConsumptionForecast(eps []AwayEpisode) *ConsumptionForecast {
	if len(eps) < minConsumptionEpisodes {
		return nil
	}
	start := eps[0].Departs.Truncate(tariff.SlotDuration)
	n := int(eps[len(eps)-1].Returns.Sub(start)/tariff.SlotDuration) + 1
	if n <= 0 {
		return nil
	}
	series := consumptionSeries(eps, start, n)
	return &ConsumptionForecast{seasonal: awayRateSeasonal(series, start, seasonalHalflife)}
}

// Drain returns the predicted consumption (in the episodes' unit) over the away-window
// [departs, returns] - the typical drain summed across those time-of-day slots.
func (f *ConsumptionForecast) Drain(departs, returns time.Time) float64 {
	return seasonalConsumption(f.seasonal, AwayEpisode{Departs: departs, Returns: returns})
}

// RateAt returns the expected consumption (in the episodes' unit) for the single slot at
// t - the away-conditional rate for that time of day.
func (f *ConsumptionForecast) RateAt(t time.Time) float64 {
	return f.seasonal[todIndex(t)]
}

// consumptionSeries turns lumpy away-episodes into the dense per-slot signal the
// forecaster expects: it spreads each episode's Consumed uniformly across the slots the
// vehicle was away and leaves connected slots at 0, over n slots from start. The result
// is high while away on a driving day and 0 at home - a periodic series (daily +
// weekly) fit for recencySeasonal / newHybridForecast.
func consumptionSeries(eps []AwayEpisode, start time.Time, n int) []float64 {
	series := make([]float64, n)
	step := tariff.SlotDuration
	for _, ep := range eps {
		from := int(ep.Departs.Sub(start) / step)
		to := int(ep.Returns.Sub(start) / step)
		if to <= from {
			continue
		}
		rate := ep.Consumed / float64(to-from) // per away-slot, over the full episode
		for i := max(from, 0); i < min(to, n); i++ {
			series[i] += rate
		}
	}
	return series
}

// awayRateSeasonal is a recency-weighted per-time-of-day average that includes only
// the away-slots (the nonzero entries of a consumption series). It estimates the
// expected rate CONDITIONAL on the vehicle being away at that time-of-day, so summing
// it over a known away-window is unbiased. By contrast recencySeasonal averages in the
// home-slot zeros, which biases a known-away prediction low by the probability the
// vehicle is out. A time-of-day never seen away stays 0.
func awayRateSeasonal(series []float64, start time.Time, halflife time.Duration) [96]float64 {
	wsum, wtot := weightedTodBuckets(start, series, halflife, true) // skip home slots (v == 0)

	var s [96]float64
	for k := range s {
		if wtot[k] > 0 {
			s[k] = wsum[k] / wtot[k]
		}
	}
	return s
}

// awaySum accumulates a per-slot predictor over the slots an episode is away.
func awaySum(ep AwayEpisode, at func(time.Time) float64) float64 {
	step := tariff.SlotDuration
	var sum float64
	for t := ep.Departs.Truncate(step); t.Before(ep.Returns); t = t.Add(step) {
		sum += at(t)
	}
	return sum
}

// seasonalConsumption predicts an episode's consumption from a per-time-of-day rate
// seasonal (the Phase-1 model): the recency-weighted average rate summed over the slots
// the vehicle is away.
func seasonalConsumption(seasonal [96]float64, ep AwayEpisode) float64 {
	return awaySum(ep, func(t time.Time) float64 { return seasonal[todIndex(t)] })
}

// hybridConsumption predicts an episode's consumption from a hybrid forecast (the
// Phase-2 model): the seasonal plus the harmonic day-of-week/multi-day residual, summed
// over the away slots.
func hybridConsumption(hf *HybridForecast, ep AwayEpisode) float64 {
	return awaySum(ep, hf.At)
}
