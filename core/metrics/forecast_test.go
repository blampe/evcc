package metrics

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/metrics/harmonic"
	"github.com/evcc-io/evcc/tariff"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTodIndex(t *testing.T) {
	tc := []struct {
		h, m, want int
	}{
		{0, 0, 0},
		{0, 15, 1},
		{12, 0, 48},
		{23, 45, 95},
	}
	for _, c := range tc {
		got := todIndex(time.Date(2025, 1, 1, c.h, c.m, 0, 0, time.UTC))
		assert.Equal(t, c.want, got, "%02d:%02d", c.h, c.m)
	}
}

func TestConsumptionSeries(t *testing.T) {
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	// away from slot 4 to slot 8 (4 slots), consuming 8 units total
	eps := []AwayEpisode{{Departs: start.Add(4 * tariff.SlotDuration), Returns: start.Add(8 * tariff.SlotDuration), Consumed: 8}}

	series := consumptionSeries(eps, start, 12)

	assert.Equal(t, 0.0, series[0], "home slot")
	assert.Equal(t, 2.0, series[4], "away slot carries the spread rate")
	assert.Equal(t, 2.0, series[7])
	assert.Equal(t, 0.0, series[8], "back home")

	var total float64
	for _, v := range series {
		total += v
	}
	assert.InDelta(t, 8.0, total, 1e-9, "spread preserves total consumption")
}

func TestSeasonalConsumption(t *testing.T) {
	var seasonal [96]float64
	for i := range seasonal {
		seasonal[i] = 1.5 // constant rate per slot
	}
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	ep := AwayEpisode{Departs: start.Add(4 * tariff.SlotDuration), Returns: start.Add(12 * tariff.SlotDuration)} // 8 slots

	assert.InDelta(t, 8*1.5, seasonalConsumption(seasonal, ep), 1e-9)
}

func TestAwayRateSeasonal(t *testing.T) {
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	// two days; at time-of-day slot 4 the car is away (rate 2) on day 0 and home (0) on
	// day 1. Conditional averaging sees only the away day; the plain mean dilutes with 0.
	series := make([]float64, 2*96)
	series[4] = 2

	uncond, err := recencySeasonal(start, series, 0) // flat, equal weight
	require.NoError(t, err)
	cond := awayRateSeasonal(series, start, 0)

	assert.InDelta(t, 1.0, uncond[4], 1e-9, "plain mean dilutes with the home-day zero")
	assert.InDelta(t, 2.0, cond[4], 1e-9, "conditional averages only the away slot")
	assert.Equal(t, 0.0, cond[5], "a time-of-day never seen away stays 0")
}

func TestHybridConsumption(t *testing.T) {
	// a constant-rate series has no cycle, so the hybrid is just its mean rate per slot
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	series := make([]float64, 30*96)
	for i := range series {
		series[i] = 2
	}
	hf, err := newHybridForecast(start, series)
	require.NoError(t, err)

	ep := AwayEpisode{Departs: start.Add(4 * tariff.SlotDuration), Returns: start.Add(12 * tariff.SlotDuration)} // 8 slots
	assert.InDelta(t, 8*2.0, hybridConsumption(hf, ep), 0.1)
}

func TestConsumptionForecast(t *testing.T) {
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC) // Monday
	// 30 days away 08:00-18:00 consuming 20 units each day
	var eps []AwayEpisode
	for d := range 30 {
		day := start.AddDate(0, 0, d)
		eps = append(eps, AwayEpisode{Departs: day.Add(8 * time.Hour), Returns: day.Add(18 * time.Hour), Consumed: 20})
	}

	fc := NewConsumptionForecast(eps)
	require.NotNil(t, fc)

	// a future episode over the same window predicts ~the learned drain
	fut := start.AddDate(0, 0, 40)
	assert.InDelta(t, 20.0, fc.Drain(fut.Add(8*time.Hour), fut.Add(18*time.Hour)), 0.5, "same window predicts the daily drain")
	assert.Less(t, fc.Drain(fut.Add(8*time.Hour), fut.Add(13*time.Hour)), 15.0, "a shorter away-window drains less")
	assert.Equal(t, 0.0, fc.Drain(fut.Add(20*time.Hour), fut.Add(22*time.Hour)), "a window the car is never away drains nothing")
}

func TestConsumptionForecastTooFewEpisodes(t *testing.T) {
	assert.Nil(t, NewConsumptionForecast(make([]AwayEpisode, minConsumptionEpisodes-1)), "too little history -> no forecast")
}

func TestConsumptionForecastRateAt(t *testing.T) {
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	var eps []AwayEpisode
	for d := range 30 {
		day := start.AddDate(0, 0, d)
		eps = append(eps, AwayEpisode{Departs: day.Add(8 * time.Hour), Returns: day.Add(9 * time.Hour), Consumed: 4})
	}

	fc := NewConsumptionForecast(eps)
	require.NotNil(t, fc)

	assert.InDelta(t, 1.0, fc.RateAt(start.Add(8*time.Hour)), 0.01, "away-slot rate: 4 consumed over 4 away slots")
	assert.Equal(t, 0.0, fc.RateAt(start), "a home time-of-day drains nothing")
}

func TestRecencySeasonal(t *testing.T) {
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	const days = 30

	// the noon slot reads 10 for the first 23 days, then drops to 0 for the last 7;
	// every other slot is a constant filler so all 96 time-of-day slots are covered
	series := make([]float64, days*96)
	for i := range series {
		if todIndex(start.Add(time.Duration(i)*tariff.SlotDuration)) == 48 { // 12:00
			series[i] = 10
			if i/96 >= 23 {
				series[i] = 0
			}
		} else {
			series[i] = 5
		}
	}

	flat, err := recencySeasonal(start, series, 0)
	require.NoError(t, err)
	weighted, err := recencySeasonal(start, series, 21*24*time.Hour)
	require.NoError(t, err)

	// flat average of noon: 10 on 23 of 30 days -> 7.67
	assert.InDelta(t, 10.0*23/30, flat[48], 0.05, "flat average is the plain mean")
	// recency weighting leans toward the recent value (0), so it sits meaningfully lower
	assert.Less(t, weighted[48], flat[48]-0.5, "recency weighting follows the recent drop")
	// unchanged slots are identical either way
	assert.InDelta(t, 5.0, weighted[0], 1e-9)
}

func TestRecencySeasonalIncomplete(t *testing.T) {
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	// only 3 hours of history -> most time-of-day slots have no sample
	_, err := recencySeasonal(start, make([]float64, 12), 21*24*time.Hour)
	assert.ErrorIs(t, err, ErrIncomplete)
}

func TestHybridForecastAt(t *testing.T) {
	var seasonal [96]float64
	for i := range seasonal {
		seasonal[i] = float64(i) // distinct value per time-of-day slot
	}
	// a flat (mean-only) residual contributes nothing, so At is the seasonal value
	// for the time's local time-of-day
	residual := harmonic.Fit(time.Time{}, 15*time.Minute, make([]float64, 8), harmonic.Options{})
	f := &HybridForecast{seasonal: seasonal, residual: residual}

	assert.Equal(t, seasonal[48], f.At(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)))
	assert.Equal(t, seasonal[0], f.At(time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)))
}
