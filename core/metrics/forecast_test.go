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
