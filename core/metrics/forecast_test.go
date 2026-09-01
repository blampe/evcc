package metrics

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/metrics/harmonic"
	"github.com/stretchr/testify/assert"
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
