package core

import (
	"database/sql"
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nfloat(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }

func TestSessionStartSoc(t *testing.T) {
	assert.Equal(t, 76.0, *sessionStartSoc(nil, 76), "first reading")

	stale := 100.0
	assert.Equal(t, 76.0, *sessionStartSoc(&stale, 76), "stale high reading yields to the real low")

	low := 76.0
	got := sessionStartSoc(&low, 80)
	assert.Equal(t, 76.0, *got, "charging up keeps the minimum")
	assert.Same(t, &low, got, "unchanged returns the same pointer (skips persist)")
}

func TestAwayEpisodes(t *testing.T) {
	t0 := time.Date(2025, 1, 6, 8, 0, 0, 0, time.UTC)
	rows := []sessionRow{
		// departure session ends at SoC 100 (odometer flat, so the energy balance is used)
		{Finished: t0, SocEnd: nfloat(100), Odometer: nfloat(1000)},
		// away 2h; SocStart is the stale 100, but the drain comes from the energy balance:
		// return ends at 90 having charged 10kWh -> reconnect 90-10*1.5=75 -> drain 25
		{Created: t0.Add(2 * time.Hour), Finished: t0.Add(3 * time.Hour), SocStart: nfloat(100), SocEnd: nfloat(90), Charged: nfloat(10), Odometer: nfloat(1000)},
		// away 1h; odometer 1000->1060 = 60km * 0.3 = 18 (odometer preferred where it moves)
		{Created: t0.Add(4 * time.Hour), Odometer: nfloat(1060)},
	}

	eps := awayEpisodes(rows, 0.3, 1.5)
	require.Len(t, eps, 2)
	assert.InDelta(t, 25.0, eps[0].Consumed, 1e-9, "energy balance ignores the stale SocStart")
	assert.InDelta(t, 18.0, eps[1].Consumed, 1e-9, "odometer distance")
}

func TestAwayEpisodesFilters(t *testing.T) {
	t0 := time.Date(2025, 1, 6, 8, 0, 0, 0, time.UTC)
	tc := []struct {
		name string
		rows []sessionRow
	}{
		{"multi-day gap", []sessionRow{{Finished: t0, SocEnd: nfloat(100)}, {Created: t0.Add(40 * time.Hour), SocEnd: nfloat(80), Charged: nfloat(0)}}},
		{"no drain", []sessionRow{{Finished: t0, SocEnd: nfloat(80)}, {Created: t0.Add(2 * time.Hour), SocEnd: nfloat(80), Charged: nfloat(0)}}},
		{"implausible drain", []sessionRow{{Finished: t0, SocEnd: nfloat(90)}, {Created: t0.Add(2 * time.Hour), SocEnd: nfloat(20), Charged: nfloat(0)}}},
	}
	for _, c := range tc {
		assert.Empty(t, awayEpisodes(c.rows, 0.3, 1.5), c.name)
	}
}

func TestDrainProfile(t *testing.T) {
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	// away every slot -> a constant 0.1%/slot rate at every time of day
	var eps []metrics.AwayEpisode
	for d := range 30 {
		day := start.AddDate(0, 0, d)
		eps = append(eps, metrics.AwayEpisode{Departs: day, Returns: day.AddDate(0, 0, 1), Consumed: 9.6})
	}
	fc := metrics.NewConsumptionForecast(eps)
	require.NotNil(t, fc)

	const capacity = 66.5
	unit := float32(0.1 / 100 * capacity * 1000 / 0.25) // 0.1%/slot expressed as W over a 15min slot

	// two away-periods (slots 0-1 and 3-5) with a connected slot (2) between them
	cMaxT := []float32{0, 0, 11000, 0, 0, 0}
	d := drainProfile(fc, start, cMaxT, capacity, 0.15, 100) // current window capped at 0.15%, later at pack headroom

	assert.InDelta(t, unit, d[0], 1, "current away-period drains")
	assert.InDelta(t, unit*0.5, d[1], 1, "current window capped to the SoC now (availNow)")
	assert.Equal(t, float32(0), d[2], "connected slot: no drain")
	assert.InDelta(t, unit, d[3], 1, "a later away-period drains too")
	assert.InDelta(t, unit, d[5], 1)
}

func TestCalibrateSocPerKwh(t *testing.T) {
	// clean sessions: +15 SoC per 10 kWh charged = 1.5 SoC/kWh
	var rows []sessionRow
	for range minCalibration + 1 {
		rows = append(rows, sessionRow{SocStart: nfloat(60), SocEnd: nfloat(75), Charged: nfloat(10)})
	}

	assert.InDelta(t, 1.5, calibrateSocPerKwh(rows, 66.5), 1e-9, "measured from clean SoC rises")
	assert.InDelta(t, chargeEfficiency*100/66.5, calibrateSocPerKwh(rows[:2], 66.5), 1e-9, "too few -> nominal efficiency")
}
