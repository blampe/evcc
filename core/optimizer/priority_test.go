package optimizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPriorityDefersGridCharge gives a priority car a goal at the end of the horizon
// that only the grid can cover, at a flat price: importing now or later costs the same.
// A priority is a claim on the site's own energy, not on the grid, so the early-charge
// reward must not pull the import forward; it lands as late as possible.
func TestPriorityDefersGridCharge(t *testing.T) {
	const (
		T      = 8
		slotWh = 9600. / 4
	)

	car := BatteryConfig{
		ChargeFromGrid: true,
		SCapacity:      60000, SMax: 60000, SInitial: 20000,
		CMin: 1840, CMax: 9600, PA: 0.0001, CPriority: 2,
		SGoal: cat(repeat(0, T-1), []float64{20000 + 2*slotWh*0.9}),
	}

	r := solveOK(t, newOptimizer(t, strat("charge_before_export", "discharge_before_import"), GridConfig{},
		[]BatteryConfig{car},
		TimeSeriesData{Dt: repeat(900, T), Gt: repeat(50, T), Ft: repeat(0, T), PN: repeat(0.0003, T), PE: repeat(0.00005, T)}, 0.9, 0.9))

	charge := f(r.Batteries[0].ChargingPower)
	approx(t, 2*slotWh, sum(charge), 1, "the car gets its goal")
	for i := range T - 2 {
		assert.Zero(t, charge[i], "slot %d: grid charge waits", i)
	}
	approx(t, slotWh, charge[T-2], 1, "the import lands in the last slots")
	approx(t, slotWh, charge[T-1], 1, "the import lands in the last slots")
}
