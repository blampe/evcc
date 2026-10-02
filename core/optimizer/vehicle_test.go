package optimizer

// Ports of tests/test_serve_home_first.py, test_s_drain_t.py, test_s_drain_plan.py,
// test_driving_commitment.py and test_c_max_t.py.

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test_serve_home_first.py

const shfT = 8

var (
	shfDT = repeat(3600, shfT)
	shfGT = repeat(500, shfT)
	shfFT = repeat(0, shfT)
	shfPN = repeat(0.0003, shfT)
	shfPE = repeat(0.00005, shfT)
)

const (
	shfEta      = 0.9
	shfHouseWh  = 500 * shfT
	shfMargin   = 50.
	shfEnough   = 1000. + shfHouseWh/shfEta + shfMargin
	shfSpareWh  = shfMargin * shfEta
	shfFullPack = 13500.
)

func shfHomePack(sInitial float64, serveHomeFirst bool) BatteryConfig {
	return BatteryConfig{
		SCapacity: 13500, SMin: 1000, SMax: 13500, SInitial: sInitial,
		CMax: 5000, DMax: 10000, PA: 0.0001, ServeHomeFirst: serveHomeFirst,
	}
}

func shfCar(cPriority int) BatteryConfig {
	return BatteryConfig{
		ChargeFromGrid: true,
		SCapacity:      60000, SMax: 60000, SInitial: 20000,
		CMax: 7000, PA: 0.0001, CPriority: cPriority,
		SGoal: cat(repeat(0, shfT-1), []float64{23000}),
	}
}

func shfSolve(t *testing.T, packInitial float64, serveHomeFirst bool, cPriority int) Result {
	return solveOK(t, newOptimizer(t, strat("none", "discharge_before_import"), GridConfig{},
		[]BatteryConfig{shfCar(cPriority), shfHomePack(packInitial, serveHomeFirst)},
		TimeSeriesData{Dt: shfDT, Gt: shfGT, Ft: shfFT, PN: shfPN, PE: shfPE}, shfEta, shfEta))
}

func shfCost(r Result) float64 {
	s := 0.0
	for t, n := range r.GridImport {
		s += *n * shfPN[t]
	}
	return s
}

func shfHouseImport(r Result) float64 {
	s := 0.0
	for t, n := range r.GridImport {
		s += math.Max(0, *n-*r.Batteries[0].ChargingPower[t])
	}
	return s
}

func shfPackToOtherLoads(r Result) float64 {
	s := 0.0
	for t, d := range r.Batteries[1].DischargingPower {
		s += math.Max(0, *d-shfGT[t])
	}
	return s
}

func TestServeHomeFirstBaselineDrainsThePackIntoTheCar(t *testing.T) {
	pyTest(t, "test_serve_home_first.py::test_baseline_drains_the_pack_into_the_car")
	r := shfSolve(t, shfEnough, false, 2)
	require.Equal(t, "Optimal", r.Status)
	assert.Greater(t, shfPackToOtherLoads(r), 1000.)
	assert.Greater(t, shfHouseImport(r), 1000.)
}

func TestServeHomeFirstPackCarriesTheHouseAndTheGridTheCar(t *testing.T) {
	pyTest(t, "test_serve_home_first.py::test_pack_carries_the_house_and_the_grid_the_car")
	r := shfSolve(t, shfEnough, true, 2)
	require.Equal(t, "Optimal", r.Status)
	approx(t, shfSpareWh, shfPackToOtherLoads(r), 1)
	approx(t, 0, shfHouseImport(r), 1)
	approx(t, 23000, *r.Batteries[0].StateOfCharge[shfT-1], 10)
}

func TestServeHomeFirstTieBreakCostsNothing(t *testing.T) {
	pyTest(t, "test_serve_home_first.py::test_tie_break_costs_nothing")
	off := shfSolve(t, shfEnough, false, 2)
	on := shfSolve(t, shfEnough, true, 2)
	approx(t, shfCost(off), shfCost(on), 1e-4)
	approx(t, sum(f(off.GridImport)), sum(f(on.GridImport)), 1)
}

func TestServeHomeFirstHoldsWithoutAChargePriority(t *testing.T) {
	pyTest(t, "test_serve_home_first.py::test_holds_without_a_charge_priority")
	r := shfSolve(t, shfEnough, true, 0)
	require.Equal(t, "Optimal", r.Status)
	approx(t, 0, shfHouseImport(r), 1)
}

func TestServeHomeFirstSpareEnergyStillGoesToTheCar(t *testing.T) {
	pyTest(t, "test_serve_home_first.py::test_spare_energy_still_goes_to_the_car")
	off := shfSolve(t, shfFullPack, false, 2)
	on := shfSolve(t, shfFullPack, true, 2)
	require.Equal(t, "Optimal", on.Status)
	approx(t, shfCost(off), shfCost(on), 1e-4)
	spare := (shfFullPack-1000.)*shfEta - shfHouseWh
	approxRel(t, math.Min(spare, 3000./shfEta), shfPackToOtherLoads(on), 0.02)
	approx(t, 0, shfHouseImport(on), 1)
}

func TestServeHomeFirstFlagOffIsTheDefault(t *testing.T) {
	pyTest(t, "test_serve_home_first.py::test_flag_off_is_the_default")
	pack := BatteryConfig{SCapacity: 13500, SMin: 1000, SMax: 13500, SInitial: shfEnough, CMax: 5000, DMax: 10000, PA: 0.0001}
	def := solveOK(t, newOptimizer(t, strat("none", "discharge_before_import"), GridConfig{},
		[]BatteryConfig{shfCar(2), pack},
		TimeSeriesData{Dt: shfDT, Gt: shfGT, Ft: shfFT, PN: shfPN, PE: shfPE}, shfEta, shfEta))
	explicit := shfSolve(t, shfEnough, false, 2)
	approxAll(t, f(explicit.GridImport), f(def.GridImport), 1)
}

// --- test_s_drain_t.py

func sDrainBuild(t *testing.T, sDrainT []float64) *Optimizer {
	return newOptimizer(t, strat("none", "none"), GridConfig{},
		[]BatteryConfig{{
			ChargeFromGrid: true, SCapacity: 40000, SMax: 40000, SInitial: 20000, PA: 0.0001, SDrainT: sDrainT,
		}},
		TimeSeriesData{Dt: repeat(3600, 6), Gt: repeat(0, 6), Ft: repeat(0, 6), PN: repeat(0.0003, 6), PE: repeat(0.00001, 6)})
}

func TestSDrainNoSDrainHoldsSoc(t *testing.T) {
	pyTest(t, "test_s_drain_t.py::test_no_s_drain_holds_soc")
	r := solveOK(t, sDrainBuild(t, nil))
	require.Equal(t, "Optimal", r.Status)
	soc := f(r.Batteries[0].StateOfCharge)
	approx(t, 20000, soc[0], 1)
	approx(t, 20000, soc[5], 1)
}

func TestSDrainLowersSocPerSlot(t *testing.T) {
	pyTest(t, "test_s_drain_t.py::test_s_drain_lowers_soc_per_slot")
	r := solveOK(t, sDrainBuild(t, []float64{1000, 1000, 0, 0, 0, 0}))
	require.Equal(t, "Optimal", r.Status)
	soc := f(r.Batteries[0].StateOfCharge)
	approx(t, 19000, soc[0], 1)
	approx(t, 18000, soc[1], 1)
	approx(t, 18000, soc[5], 1)
}

func TestSDrainIsExogenousNotAGridDraw(t *testing.T) {
	pyTest(t, "test_s_drain_t.py::test_drain_is_exogenous_not_a_grid_draw")
	r := solveOK(t, sDrainBuild(t, []float64{1000, 1000, 0, 0, 0, 0}))
	require.Equal(t, "Optimal", r.Status)
	for _, c := range f(r.Batteries[0].ChargingPower) {
		approx(t, 0, c, 1e-6)
	}
}

// --- test_s_drain_plan.py

func sDrainPlanBuild(t *testing.T, sDrainT []float64) *Optimizer {
	return newOptimizer(t, strat("none", "none"), GridConfig{},
		[]BatteryConfig{{
			ChargeFromGrid: true, SCapacity: 20000, SMax: 20000, CMax: 10000, PA: 0.0001,
			SGoal: []float64{0, 0, 0, 20000, 0, 0}, SDrainT: sDrainT,
		}},
		TimeSeriesData{Dt: repeat(3600, 6), Gt: repeat(0, 6), Ft: repeat(0, 6), PN: repeat(0.0001, 6), PE: repeat(0.00001, 6)})
}

func TestSDrainPlanGoalWithoutDrain(t *testing.T) {
	pyTest(t, "test_s_drain_plan.py::test_goal_without_drain")
	approx(t, 20000, *solveOK(t, sDrainPlanBuild(t, nil)).Batteries[0].StateOfCharge[3], 200)
}

func TestSDrainPlanGoalWithDrainAfterDeadline(t *testing.T) {
	pyTest(t, "test_s_drain_plan.py::test_goal_with_drain_after_deadline")
	approx(t, 20000, *solveOK(t, sDrainPlanBuild(t, []float64{0, 0, 0, 0, 3000, 3000})).Batteries[0].StateOfCharge[3], 200)
}

func TestSDrainPlanGoalWithLargeDrainAfterDeadline(t *testing.T) {
	pyTest(t, "test_s_drain_plan.py::test_goal_with_large_drain_after_deadline")
	approx(t, 20000, *solveOK(t, sDrainPlanBuild(t, []float64{0, 0, 0, 0, 10000, 10000})).Batteries[0].StateOfCharge[3], 200)
}

// --- test_driving_commitment.py

const dcC = 11000.0

var (
	dcPN    = []float64{0.0001, 0.0001, 0.0001, 0.0005, 0.0005, 0.0003, 0.0003, 0.0003}
	dcCMaxT = []float64{dcC, dcC, dcC, 0, 0, dcC, dcC, dcC}
	dcDrain = []float64{0, 0, 0, 2000, 2000, 0, 0, 0}
)

type dcOpts struct {
	sDrainT, cMaxT, ft, pN []float64
}

func dcBuild(t *testing.T, o dcOpts) *Optimizer {
	if o.ft == nil {
		o.ft = repeat(0, 8)
	}
	if o.pN == nil {
		o.pN = dcPN
	}
	return newOptimizer(t, strat("none", "none"), GridConfig{},
		[]BatteryConfig{{
			ChargeFromGrid: true, SCapacity: 40000, SMax: 40000, CMax: dcC,
			SDrainT: o.sDrainT, CMaxT: o.cMaxT,
		}},
		TimeSeriesData{Dt: repeat(3600, 8), Gt: repeat(0, 8), Ft: o.ft, PN: o.pN, PE: repeat(0.00001, 8)})
}

func dcSolve(t *testing.T, o dcOpts) Result {
	if o.sDrainT == nil {
		o.sDrainT = dcDrain
	}
	if o.cMaxT == nil {
		o.cMaxT = dcCMaxT
	}
	m := dcBuild(t, o)
	m.CreateModel()
	r := solveOK(t, m)
	require.Equal(t, "Optimal", r.Status)
	return r
}

func driven(r Result) float64 {
	soc := r.Batteries[0].StateOfCharge
	return *soc[2] - *soc[4]
}

func TestDrivingCoversTheTripFromCheapGrid(t *testing.T) {
	pyTest(t, "test_driving_commitment.py::test_covers_the_trip_from_cheap_grid")
	r := dcSolve(t, dcOpts{})
	soc, imp := f(r.Batteries[0].StateOfCharge), f(r.GridImport)
	approx(t, 4000, soc[2], 5)
	approx(t, 4000, driven(r), 5)
	approx(t, 0, soc[4], 5)
	approx(t, 4000/0.95, sum(imp[0:3]), 10)
	approx(t, 0, sum(imp[5:8]), 1)
}

func TestDrivingFillsFromPvSurplusInsteadOfGrid(t *testing.T) {
	pyTest(t, "test_driving_commitment.py::test_fills_from_pv_surplus_instead_of_grid")
	r := dcSolve(t, dcOpts{ft: []float64{0, 0, 5000, 0, 0, 0, 0, 0}})
	approx(t, 4000, driven(r), 5)
	approx(t, 0, sum(f(r.GridImport)), 5)
}

func TestDrivingDoesNotChargeBeyondTheTrip(t *testing.T) {
	pyTest(t, "test_driving_commitment.py::test_does_not_charge_beyond_the_trip")
	r := dcSolve(t, dcOpts{})
	soc := f(r.Batteries[0].StateOfCharge)
	m := soc[0]
	for _, s := range soc {
		m = math.Max(m, s)
	}
	approx(t, 4000, m, 5)
}

func TestDrivingCoversTheTripOnAnExpensiveDay(t *testing.T) {
	pyTest(t, "test_driving_commitment.py::test_covers_the_trip_on_an_expensive_day")
	r := dcSolve(t, dcOpts{pN: []float64{0.0007, 0.0007, 0.0007, 0.0009, 0.0009, 0.0008, 0.0008, 0.0008}})
	approx(t, 4000, driven(r), 5)
}

func TestDrivingArrivesShortWhenItCannotChargeInTime(t *testing.T) {
	pyTest(t, "test_driving_commitment.py::test_arrives_short_when_it_cannot_charge_in_time")
	r := dcSolve(t, dcOpts{cMaxT: []float64{1000, 1000, 1000, 0, 0, 1000, 1000, 1000}})
	total := driven(r)
	assert.Less(t, total, 4000.)
	approx(t, 3*1000*0.95, total, 15)
	approx(t, 0, *r.Batteries[0].StateOfCharge[4], 5)
}

func TestDrivingMinSocFloorHeldBetweenTripsButNotDuringThem(t *testing.T) {
	pyTest(t, "test_driving_commitment.py::test_min_soc_floor_held_between_trips_but_not_during_them")
	sMin, sMax := 8000.0, 12000.0
	m := newOptimizer(t, strat("none", "none"), GridConfig{},
		[]BatteryConfig{{
			ChargeFromGrid: true, SCapacity: 40000, SMin: sMin, SMax: sMax, SInitial: sMin, CMax: dcC,
			SDrainT: []float64{0, 0, 0, 4000, 4000, 0, 0, 0}, CMaxT: dcCMaxT,
		}},
		TimeSeriesData{Dt: repeat(3600, 8), Gt: repeat(0, 8), Ft: repeat(0, 8), PN: dcPN, PE: repeat(0.00001, 8)})
	m.CreateModel()
	r := solveOK(t, m)
	require.Equal(t, "Optimal", r.Status)
	soc := f(r.Batteries[0].StateOfCharge)
	approx(t, 8000, soc[2]-soc[4], 10)
	assert.Less(t, soc[4], sMin-100)
	for _, slot := range []int{0, 1, 2, 5, 6, 7} {
		assert.GreaterOrEqual(t, soc[slot], sMin-10, "slot %d", slot)
	}
}

func TestDrivingNoDriveForecastIsUntouched(t *testing.T) {
	pyTest(t, "test_driving_commitment.py::test_no_drive_forecast_is_untouched")
	m := dcBuild(t, dcOpts{})
	m.CreateModel()
	assert.Nil(t, m.v.drive[0])
	r := solveOK(t, m)
	require.Equal(t, "Optimal", r.Status)
	approx(t, 0, sum(f(r.GridImport)), 1)
	approx(t, 0, *r.Batteries[0].StateOfCharge[7], 1)
}

// --- test_c_max_t.py

func cMaxTBuild(t *testing.T, cMaxT []float64) *Optimizer {
	return newOptimizer(t, strat("none", "none"), GridConfig{},
		[]BatteryConfig{{
			ChargeFromGrid: true, SCapacity: 40000, SMax: 40000, CMax: 10000, PA: 0.0001,
			SGoal: []float64{0, 0, 0, 0, 0, 20000}, CMaxT: cMaxT,
		}},
		TimeSeriesData{
			Dt: repeat(3600, 6), Gt: repeat(0, 6), Ft: repeat(0, 6),
			PN: []float64{0.0003, 0.0003, 0.0003, 0.00001, 0.00001, 0.0003}, PE: repeat(0.00001, 6),
		})
}

func TestCMaxTBaselineChargesInTheCheapSlots(t *testing.T) {
	pyTest(t, "test_c_max_t.py::test_baseline_charges_in_the_cheap_slots")
	r := solveOK(t, cMaxTBuild(t, nil))
	require.Equal(t, "Optimal", r.Status)
	c := f(r.Batteries[0].ChargingPower)
	assert.True(t, c[3] > 0 && c[4] > 0)
	approx(t, 20000, *r.Batteries[0].StateOfCharge[5], 100)
}

func TestCMaxTZeroForbidsChargingInThoseSlots(t *testing.T) {
	pyTest(t, "test_c_max_t.py::test_zero_c_max_t_forbids_charging_in_those_slots")
	r := solveOK(t, cMaxTBuild(t, []float64{10000, 10000, 10000, 0, 0, 10000}))
	require.Equal(t, "Optimal", r.Status)
	c := f(r.Batteries[0].ChargingPower)
	approx(t, 0, c[3], 1e-6)
	approx(t, 0, c[4], 1e-6)
	approx(t, 20000, *r.Batteries[0].StateOfCharge[5], 100)
}

func TestCMaxTPerSlotCapThrottlesChargePower(t *testing.T) {
	pyTest(t, "test_c_max_t.py::test_per_slot_cap_throttles_charge_power")
	r := solveOK(t, cMaxTBuild(t, []float64{10000, 10000, 10000, 2000, 2000, 10000}))
	require.Equal(t, "Optimal", r.Status)
	c := f(r.Batteries[0].ChargingPower)
	assert.LessOrEqual(t, c[3], 2000+1e-6)
	assert.LessOrEqual(t, c[4], 2000+1e-6)
}

func TestCMaxTUniformMatchesTheScalar(t *testing.T) {
	pyTest(t, "test_c_max_t.py::test_uniform_c_max_t_matches_the_scalar")
	scalar := solveOK(t, cMaxTBuild(t, nil))
	array := solveOK(t, cMaxTBuild(t, repeat(10000, 6)))
	require.Equal(t, "Optimal", array.Status)
	approxAll(t, f(scalar.Batteries[0].ChargingPower), f(array.Batteries[0].ChargingPower), 1)
}
