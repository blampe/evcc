package optimizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestServeHomeFirstDrainsSlowly gives a car four minimum-power slots of energy
// from a full home pack at a flat price: running at the minimum over four slots
// or at full power in one costs the same, and the slow drain wins.
func TestServeHomeFirstDrainsSlowly(t *testing.T) {
	const (
		T      = 8
		cMin   = 1840.
		slotWh = cMin / 4
	)

	car := BatteryConfig{
		ChargeFromGrid: true,
		SCapacity:      60000, SMax: 60000, SInitial: 20000,
		CMin: cMin, CMax: 9600, PA: 0.0001,
		SGoal: cat(repeat(0, T-1), []float64{20000 + 4*slotWh*0.9}),
	}
	pack := BatteryConfig{
		SCapacity: 13500, SMin: 1350, SMax: 13500, SInitial: 13500,
		CMax: 5000, DMax: 11500, PA: 0.0001, ServeHomeFirst: true,
	}

	r := solveOK(t, newOptimizer(t, strat("none", "discharge_before_import"), GridConfig{},
		[]BatteryConfig{car, pack},
		TimeSeriesData{Dt: repeat(900, T), Gt: repeat(50, T), Ft: repeat(0, T), PN: repeat(0.0003, T), PE: repeat(0.00005, T)}, 0.9, 0.9))

	charge := f(r.Batteries[0].ChargingPower)
	approx(t, 4*slotWh, sum(charge), 1, "the car gets its goal")
	approx(t, 0, sum(f(r.GridImport)), 1e-3, "all from the pack")
	for i, c := range charge {
		assert.LessOrEqual(t, c, slotWh+1, "slot %d: the pack feeds the car at its minimum, not in a burst", i)
	}
}

// TestServeHomeFirstDrainsSoon gives a car four minimum-power slots of energy from
// a full home pack with nothing due until the horizon ends. When the pack hands it
// over is a cost tie that serve_home_first alone leaves open; with charge_before_export
// underneath it (the car's early-charge priority) the surplus moves right away, and
// the home-first commitment still keeps it off the grid.
func TestServeHomeFirstDrainsSoon(t *testing.T) {
	const (
		T      = 12
		cMin   = 1840.
		slotWh = cMin / 4
	)

	car := BatteryConfig{
		ChargeFromGrid: true,
		SCapacity:      60000, SMax: 60000, SInitial: 20000,
		CMin: cMin, CMax: 9600, PA: 0.0001, CPriority: 2,
		SGoal: cat(repeat(0, T-1), []float64{20000 + 4*slotWh*0.9}),
	}
	pack := BatteryConfig{
		SCapacity: 13500, SMin: 1350, SMax: 13500, SInitial: 13500,
		CMax: 5000, DMax: 11500, PA: 0.0001, ServeHomeFirst: true,
	}

	r := solveOK(t, newOptimizer(t, strat("charge_before_export", "discharge_before_import"), GridConfig{},
		[]BatteryConfig{car, pack},
		TimeSeriesData{Dt: repeat(900, T), Gt: repeat(50, T), Ft: repeat(0, T), PN: repeat(0.0003, T), PE: repeat(0.00005, T)}, 0.9, 0.9))

	charge := f(r.Batteries[0].ChargingPower)
	approx(t, 0, sum(f(r.GridImport)), 1e-3, "all from the pack")
	for i := range 4 {
		approx(t, slotWh, charge[i], 1, "slot %d: the pack feeds the car from the start, at its minimum", i)
	}
}
