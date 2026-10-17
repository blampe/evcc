package optimizer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// chargingSolve plans a car that needs two minimum-power slots at a flat price,
// so when it charges is a cost tie that only the start penalty can break.
func chargingSolve(t *testing.T, charging bool) (Result, *Optimizer) {
	o := chargingOptimizer(t, charging)
	return solveOK(t, o), o
}

func chargingOptimizer(t *testing.T, charging bool) *Optimizer {
	const T = 8
	car := BatteryConfig{
		ChargeFromGrid: true,
		SCapacity:      60000, SMax: 60000, SInitial: 20000,
		CMin: 1840, CMax: 9600, PA: 0.0001,
		SGoal:    cat(repeat(0, T-1), []float64{20000 + 2*460*0.9}),
		Charging: charging,
	}
	return newOptimizer(t, strat("none", "none"), GridConfig{},
		[]BatteryConfig{car},
		TimeSeriesData{Dt: repeat(900, T), Gt: repeat(0, T), Ft: repeat(0, T), PN: repeat(0.0003, T), PE: repeat(0.00005, T)}, 0.9, 0.9)
}

// TestChargingSolvesInStages writes each stage's MPS on its own: a running session
// must leave the first start variable in some row, or CBC refuses its bound.
func TestChargingSolvesInStages(t *testing.T) {
	o := chargingOptimizer(t, true)
	WithoutProbe()(&o.settings)
	solveOK(t, o)
	assert.Equal(t, "split", o.solvePath)
	assert.Zero(t, mustValue(affine(o.v.zStart[0][0])), "charging on is no start")
}

func TestChargingContinuesTheRunningSession(t *testing.T) {
	r, o := chargingSolve(t, true)
	assert.Positive(t, *r.Batteries[0].ChargingPower[0], "a running session keeps charging")
	assert.Zero(t, mustValue(affine(o.v.zStart[0][0])), "charging on is no start")

	r, o = chargingSolve(t, false)
	assert.Positive(t, *r.Batteries[0].ChargingPower[0])
	assert.Equal(t, 1.0, mustValue(affine(o.v.zStart[0][0])), "charging from idle is a start")
}

func TestChargingDoesNotChangeTheCost(t *testing.T) {
	running, _ := chargingSolve(t, true)
	idle, _ := chargingSolve(t, false)
	approx(t, sum(f(idle.GridImport)), sum(f(running.GridImport)), 1e-3, "the session state only breaks ties")
}
