package soc

import (
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/require"
)

// 10 kWh capacity => 100 Wh per soc percent
const testCapacity = 10.0

func newTestBatteryEstimator(clk clock.Clock) *BatteryEstimator {
	e := NewBatteryEstimator(util.NewLogger("foo"))
	e.clck = clk
	return e
}

func TestBatteryEstimatorInterpolatesBetweenUpdates(t *testing.T) {
	clk := clock.NewMock()
	e := newTestBatteryEstimator(clk)

	// first sample initializes to the raw value
	require.Equal(t, 50.0, e.Soc(50, 0, testCapacity))

	// raw held at 50, charging at 1 kW for 6 min => +100 Wh => +1%
	clk.Add(6 * time.Minute)
	require.InDelta(t, 51.0, e.Soc(50, -1000, testCapacity), 1e-6)

	// another 6 min charging => +1% more
	clk.Add(6 * time.Minute)
	require.InDelta(t, 52.0, e.Soc(50, -1000, testCapacity), 1e-6)

	// raw catches up: snap to it and restart integration
	clk.Add(1 * time.Minute)
	require.Equal(t, 53.0, e.Soc(53, -1000, testCapacity))

	// now discharging at 2 kW for 3 min => -100 Wh => -1%
	clk.Add(3 * time.Minute)
	require.InDelta(t, 52.0, e.Soc(53, 2000, testCapacity), 1e-6)
}

func TestBatteryEstimatorClamps(t *testing.T) {
	clk := clock.NewMock()
	e := newTestBatteryEstimator(clk)

	require.Equal(t, 90.0, e.Soc(90, 0, testCapacity))

	// charge hard for a long time while raw is stuck => clamp at 100
	clk.Add(2 * time.Hour)
	require.Equal(t, 100.0, e.Soc(90, -5000, testCapacity))

	// raw drops and resets the baseline
	clk.Add(time.Minute)
	require.Equal(t, 40.0, e.Soc(40, 0, testCapacity))

	// discharge hard while raw is stuck => clamp at 0
	clk.Add(2 * time.Hour)
	require.Equal(t, 0.0, e.Soc(40, 5000, testCapacity))
}

func TestBatteryEstimatorNoCapacity(t *testing.T) {
	clk := clock.NewMock()
	e := newTestBatteryEstimator(clk)

	require.Equal(t, 50.0, e.Soc(50, 0, 0))

	// without a capacity the raw value passes through unchanged
	clk.Add(30 * time.Minute)
	require.Equal(t, 50.0, e.Soc(50, -5000, 0))
}

func TestBatteryEstimatorTimelySensorIsNoop(t *testing.T) {
	clk := clock.NewMock()
	e := newTestBatteryEstimator(clk)

	require.Equal(t, 50.0, e.Soc(50, -1000, testCapacity))

	// a sensor that advances every cycle tracks the raw value exactly
	for _, raw := range []float64{51, 52, 53} {
		clk.Add(time.Minute)
		require.Equal(t, raw, e.Soc(raw, -1000, testCapacity))
	}
}
