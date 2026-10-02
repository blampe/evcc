package optimizer

// Ports of tests/test_consolidate_charging.py, test_peak_leveling.py,
// test_charge_goal_vs_import_penalty.py, test_clean_objective.py, test_load_aware_reserve.py and
// test_penalty_base.py.

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test_consolidate_charging.py

func consolidateBuild(t *testing.T, serveHomeFirst bool) *Optimizer {
	const T = 24
	car := BatteryConfig{ChargeFromGrid: true, SCapacity: 70000, SMax: 70000, SInitial: 30000, CMin: 1840, CMax: 9600, PA: 0.0003}
	pack := BatteryConfig{SCapacity: 13500, SMin: 1350, SMax: 13500, SInitial: 13500, CMax: 5000, DMax: 11500, PA: 0.0001, ServeHomeFirst: serveHomeFirst}
	return newOptimizer(t, strat("none", "none"), GridConfig{}, []BatteryConfig{car, pack},
		TimeSeriesData{Dt: repeat(900, T), Gt: repeat(150, T), Ft: repeat(0, T), PN: repeat(0.0003, T), PE: repeat(0.00009, T)})
}

func chargingStarts(charge []float64) int {
	n := 0
	for t, c := range charge {
		if c > 1 && (t == 0 || charge[t-1] <= 1) {
			n++
		}
	}
	return n
}

func solvePinned(t *testing.T, charge []float64) (float64, float64) {
	o := consolidateBuild(t, false)
	o.CreateModel()
	for i, c := range charge {
		// py: optimizer.problem += optimizer.variables['c'][0][t] == c
		o.problem.add(newConstraintRHS(affine(o.v.c[0][i]), lpConstraintEQ, c), "")
	}
	r := solveOK(t, o)
	require.Equal(t, "Optimal", r.Status)
	return *value(o.costObjective), *value(o.preferenceObjective)
}

func TestConsolidateOneBlockBeatsTheSameEnergySplit(t *testing.T) {
	pyTest(t, "test_consolidate_charging.py::test_one_block_beats_the_same_energy_split")
	block, split := repeat(0, 24), repeat(0, 24)
	block[4], block[5] = 1200, 1200
	split[4], split[12] = 1200, 1200

	blockCost, blockPref := solvePinned(t, block)
	splitCost, splitPref := solvePinned(t, split)

	approxRel(t, splitCost, blockCost, 1e-9, "the test needs a tie on money")
	assert.Greater(t, blockPref, splitPref)
}

func TestConsolidateSparePackEnergyGoesToTheCarInOneBlock(t *testing.T) {
	pyTest(t, "test_consolidate_charging.py::test_spare_pack_energy_goes_to_the_car_in_one_block")
	for _, shf := range []bool{false, true} {
		r := solveOK(t, consolidateBuild(t, shf))
		require.Equal(t, "Optimal", r.Status)
		charge := f(r.Batteries[0].ChargingPower)
		assert.Greater(t, sum(charge), 1840.*900/3600)
		assert.Equal(t, 1, chargingStarts(charge), "serve_home_first=%v: %v", shf, charge)
	}
}

// --- test_peak_leveling.py

type peakOpts struct {
	strategy string
	gt       []float64
}

func peakBuild(t *testing.T, o peakOpts) *Optimizer {
	if o.strategy == "" {
		o.strategy = "attenuate_demand_peaks"
	}
	const n = 8
	dt := repeat(3600, n)
	if o.gt == nil {
		o.gt = make([]float64, n)
		for i := range o.gt {
			o.gt[i] = float64(500.*dt[i]) / 3600.
		}
	}
	return newOptimizer(t, strat(o.strategy, "none"), GridConfig{},
		[]BatteryConfig{{ChargeFromGrid: true, SCapacity: 40000, SMax: 40000, SInitial: 8000, CMax: 8000, PA: 0.0001}},
		TimeSeriesData{Dt: dt, Gt: o.gt, Ft: repeat(0, n), PN: repeat(0.0003, n), PE: repeat(0.0001, n)})
}

func importPower(o *Optimizer, r Result) []float64 {
	p := f(r.GridImport)
	for i := range p {
		p[i] = p[i] * 3600 / o.timeSeries.Dt[i]
	}
	return p
}

func TestPeakNoRampTermIsLeftInTheModel(t *testing.T) {
	o := peakBuild(t, peakOpts{strategy: "attenuate_grid_peaks"})
	o.CreateModel()
	for _, v := range o.problem.vars() {
		assert.False(t, strings.HasPrefix(v.name, "p_imp_ramp") || strings.HasPrefix(v.name, "p_exp_ramp"), v.name)
	}
}

func TestPeakVariablesExistOnlyForTheLeveledSides(t *testing.T) {
	demand := peakBuild(t, peakOpts{strategy: "attenuate_demand_peaks"})
	both := peakBuild(t, peakOpts{strategy: "attenuate_grid_peaks"})
	none := peakBuild(t, peakOpts{strategy: "none"})
	for _, o := range []*Optimizer{demand, both, none} {
		o.CreateModel()
	}
	assert.Contains(t, demand.v.peak, "imp")
	assert.NotContains(t, demand.v.peak, "exp")
	assert.Contains(t, both.v.peak, "imp")
	assert.Contains(t, both.v.peak, "exp")
	assert.Empty(t, none.v.peak)
}

func TestPeakTermSpreadsTheChargeOverTheHorizon(t *testing.T) {
	pyTest(t, "test_peak_leveling.py::test_the_peak_term_spreads_the_charge_over_the_horizon")
	o := peakBuild(t, peakOpts{})
	o.batteries[0].SGoal = []float64{0, 0, 0, 0, 0, 20000, 0, 0}
	r := solveOK(t, o)
	require.Equal(t, "Optimal", r.Status)
	power := importPower(o, r)
	assert.Less(t, stddev(power[:6]), 1.)
	m := power[0]
	for _, p := range power {
		m = math.Max(m, p)
	}
	assert.Less(t, m, 5000.)
}

func TestPeakPinnedPeakLeavesTheStepsBelowItUnordered(t *testing.T) {
	pyTest(t, "test_peak_leveling.py::test_a_pinned_peak_leaves_the_steps_below_it_unordered")
	o := peakBuild(t, peakOpts{gt: []float64{500, 500, 6000, 500, 500, 500, 500, 500}})
	o.batteries[0].SGoal = []float64{0, 0, 0, 0, 0, 20000, 0, 0}
	r := solveOK(t, o)
	require.Equal(t, "Optimal", r.Status)
	power := importPower(o, r)
	m := power[0]
	for _, p := range power {
		m = math.Max(m, p)
	}
	approx(t, 6000, m, 1)

	flatOut := []float64{6000, 6000, 6000, 1044, 1044, 1044, 500, 500}
	spread := []float64{3026, 3026, 6000, 3026, 3026, 3026, 500, 500}
	approxRel(t, sum(spread)*3600, sum(flatOut)*3600, 1e-3)
	assert.Less(t, stddev(spread), stddev(flatOut)/1.5)
}

// --- test_charge_goal_vs_import_penalty.py

func cgEV(sGoalFinal float64) BatteryConfig {
	goal := repeat(0, 8)
	goal[7] = sGoalFinal
	return BatteryConfig{ChargeFromGrid: true, SCapacity: 60000, SMax: 48000, SInitial: 18000, CMax: 11000, PA: 0.0003, SGoal: goal}
}

func cgHome(sInitial, sMin float64) BatteryConfig {
	return BatteryConfig{SCapacity: 13500, SMin: sMin, SMax: 13500, SInitial: sInitial, CMax: 5000, DMax: 5000, PA: 0.0003}
}

func cgSolve(t *testing.T, batteries []BatteryConfig) Result {
	o := newOptimizer(t, strat("none", "discharge_before_import"), GridConfig{PMaxImp: fptr(1)}, batteries,
		TimeSeriesData{Dt: repeat(3600, 8), Gt: repeat(1000, 8), Ft: repeat(0, 8), PN: repeat(0.0003, 8), PE: repeat(0.0001, 8)},
		0.9, 0.9)
	o.CreateModel()
	return solveOK(t, o)
}

func TestChargeGoalIsMetByImportingDespiteTheImportPenalty(t *testing.T) {
	pyTest(t, "test_charge_goal_vs_import_penalty.py::test_charge_goal_is_met_by_importing_despite_the_import_penalty")
	r := cgSolve(t, []BatteryConfig{cgEV(48000)})
	require.Equal(t, "Optimal", r.Status)
	approx(t, 48000, *r.Batteries[0].StateOfCharge[7], 100)
}

func TestChargeGoalNoGoalIsNotForceChargedFromPenalizedGrid(t *testing.T) {
	pyTest(t, "test_charge_goal_vs_import_penalty.py::test_no_goal_is_not_force_charged_from_penalized_grid")
	r := cgSolve(t, []BatteryConfig{cgEV(0)})
	require.Equal(t, "Optimal", r.Status)
	approx(t, 18000, *r.Batteries[0].StateOfCharge[7], 100)
}

func TestChargeGoalReserveFloorIsNotBreachedToReachAGoal(t *testing.T) {
	pyTest(t, "test_charge_goal_vs_import_penalty.py::test_reserve_floor_is_not_breached_to_reach_a_goal")
	r := cgSolve(t, []BatteryConfig{cgHome(6750, 3375), cgEV(48000)})
	require.Equal(t, "Optimal", r.Status)
	approx(t, 48000, *r.Batteries[1].StateOfCharge[7], 100)
	for _, s := range f(r.Batteries[0].StateOfCharge) {
		assert.GreaterOrEqual(t, s, 3375-1.0)
	}
}

// --- test_clean_objective.py

const (
	coPN       = 0.0002
	coPE       = 0.0001
	coPA       = 0.0004
	coSInitial = 1000.0
)

func coBuild(t *testing.T, sInitial, sMax, cMax, dMax float64) *Optimizer {
	return newOptimizer(t, strat("none", "none"), GridConfig{},
		[]BatteryConfig{{ChargeFromGrid: true, DischargeToGrid: true, SCapacity: 5000, SMax: sMax, SInitial: sInitial, CMax: cMax, DMax: dMax, PA: coPA}},
		TimeSeriesData{Dt: repeat(3600, 2), Gt: repeat(0, 2), Ft: repeat(0, 2), PN: repeat(coPN, 2), PE: repeat(coPE, 2)})
}

func economicValue(r Result, sInitial float64) float64 {
	soc := f(r.Batteries[0].StateOfCharge)
	return -sum(f(r.GridImport))*coPN + sum(f(r.GridExport))*coPE + (soc[len(soc)-1]-sInitial)*coPA
}

func TestCleanObjectiveStoredEnergyIsValuedAgainstTheInitialSoc(t *testing.T) {
	pyTest(t, "test_clean_objective.py::test_stored_energy_is_valued_against_the_initial_soc")
	r := solveOK(t, coBuild(t, coSInitial, 5000, 1000, 0))
	require.Greater(t, *r.Batteries[0].ChargingPower[0], 0.)
	approxRel(t, economicValue(r, coSInitial), *r.ObjectiveValue, 1e-6)
}

func TestCleanObjectiveStoredEnergyValueSurvivesADischargingFirstStep(t *testing.T) {
	pyTest(t, "test_clean_objective.py::test_stored_energy_value_survives_a_discharging_first_step")
	r := solveOK(t, coBuild(t, 4000, 2000, 0, 1000))
	require.Greater(t, *r.Batteries[0].DischargingPower[0], 0.)
	approxRel(t, economicValue(r, 4000), *r.ObjectiveValue, 1e-6)
}

func TestCleanObjectiveForecastDrivingIsNotACost(t *testing.T) {
	pyTest(t, "test_clean_objective.py::test_forecast_driving_is_not_a_cost")
	drive := []float64{1000, 500, 0}
	r := solveOK(t, newOptimizer(t, strat("none", "none"), GridConfig{},
		[]BatteryConfig{{SCapacity: 5000, SMax: 5000, SInitial: coSInitial * 3, PA: coPN / 2, SDrainT: drive}},
		TimeSeriesData{Dt: repeat(3600, 3), Gt: repeat(0, 3), Ft: repeat(0, 3), PN: repeat(coPN, 3), PE: repeat(coPE, 3)}))
	approx(t, coSInitial*3-sum(drive), *r.Batteries[0].StateOfCharge[2], 1)
	approx(t, 0, sum(f(r.GridImport)), 1e-6)
	approx(t, 0, *r.ObjectiveValue, 1e-6)
}

// --- test_load_aware_reserve.py

var (
	larDT      = repeat(3600, 6)
	larGT      = []float64{200, 1000, 1000, 1000, 200, 200}
	larFT      = []float64{0, 0, 0, 0, 1000, 1000}
	larPE      = []float64{0.005, 0.0001, 0.0001, 0.0001, 0.0001, 0.0001}
	larPN      = repeat(0.001, 6)
	larProfile = []float64{3368.4, 3157.9, 2105.3, 1052.6, 200.0, 200.0}
)

func larBattery(reserveForLoad bool, sInitial, sMin, sCapacity float64) BatteryConfig {
	return BatteryConfig{
		DischargeToGrid: true, SCapacity: sCapacity, SMin: sMin, SMax: sCapacity, SInitial: sInitial,
		CMax: 3000, DMax: 3000, ReserveForLoad: reserveForLoad,
	}
}

func larSolveWith(t *testing.T, bat BatteryConfig, gt, ft []float64) (Result, *Optimizer) {
	o := newOptimizer(t, strat("none", "none"), GridConfig{}, []BatteryConfig{bat},
		TimeSeriesData{Dt: larDT, Gt: gt, Ft: ft, PN: larPN, PE: larPE})
	o.CreateModel()
	return solveOK(t, o), o
}

func larSolve(t *testing.T, reserveForLoad bool, sInitial float64) (Result, *Optimizer) {
	r, o := larSolveWith(t, larBattery(reserveForLoad, sInitial, 200, 8000), larGT, larFT)
	require.Equal(t, "Optimal", r.Status)
	return r, o
}

func TestLoadAwareReserveProfileMatchesTheForecast(t *testing.T) {
	pyTest(t, "test_load_aware_reserve.py::test_reserve_profile_matches_the_forecast")
	_, o := larSolve(t, true, 3600)
	approxAll(t, larProfile, o.sMinProfile[0], 0.1)
}

func TestLoadAwareFlagOffLeavesTheFlatFloor(t *testing.T) {
	pyTest(t, "test_load_aware_reserve.py::test_flag_off_leaves_the_flat_floor")
	_, o := larSolve(t, false, 3600)
	assert.Equal(t, repeat(200, 6), o.sMinProfile[0])
}

func TestLoadAwareReserveHoldsTheOvernightNeedAtTheSpike(t *testing.T) {
	pyTest(t, "test_load_aware_reserve.py::test_a_reserve_holds_the_overnight_need_at_the_spike")
	off, _ := larSolve(t, false, 3600)
	on, _ := larSolve(t, true, 3600)

	nightOff, nightOn := sum(f(off.GridImport)[1:4]), sum(f(on.GridImport)[1:4])

	minOff := math.Inf(1)
	for _, s := range f(off.Batteries[0].StateOfCharge) {
		minOff = math.Min(minOff, s)
	}
	approx(t, 200, minOff, 1)
	assert.Greater(t, nightOff, 2500.)

	approxAll(t, larProfile, f(on.Batteries[0].StateOfCharge), 1)
	assert.Less(t, nightOn, nightOff/3)
	for _, s := range f(on.Batteries[0].StateOfCharge) {
		assert.GreaterOrEqual(t, s, 200-1.0)
	}
	assert.Less(t, *on.GridExport[0], *off.GridExport[0])
}

func TestLoadAwareReserveIsHighAtDuskAndLowOncePvResumes(t *testing.T) {
	pyTest(t, "test_load_aware_reserve.py::test_b_reserve_is_high_at_dusk_and_low_once_pv_resumes")
	_, o := larSolve(t, true, 3600)
	p := o.sMinProfile[0]
	assert.True(t, p[0] > p[1] && p[1] > p[2] && p[2] > p[3] && p[3] > p[4])
	approx(t, 3368.4, p[0], 0.5)
	approx(t, 200, p[4], 0.5)
	approx(t, 200, p[5], 0.5)

	r, _ := larSolve(t, true, 3600)
	soc := f(r.Batteries[0].StateOfCharge)
	approxAll(t, p, soc, 1)
	assert.Greater(t, soc[0], soc[5])
}

func TestLoadAwareSurplusAboveTheReserveStillExportsAtTheSpike(t *testing.T) {
	pyTest(t, "test_load_aware_reserve.py::test_c_surplus_above_the_reserve_still_exports_at_the_spike")
	reserve0 := larProfile[0]
	tight, _ := larSolve(t, true, reserve0)
	rich, _ := larSolve(t, true, reserve0+2000)
	approx(t, 0, *tight.GridExport[0], 1)
	approx(t, 1700, *rich.GridExport[0], 5)
	approx(t, reserve0, *rich.Batteries[0].StateOfCharge[0], 1)
}

func TestLoadAwareFlagOffIsIdenticalToTheDefaultBattery(t *testing.T) {
	pyTest(t, "test_load_aware_reserve.py::test_d_flag_off_is_identical_to_the_default_battery")
	explicit, _ := larSolve(t, false, 3600)
	def, _ := larSolveWith(t, BatteryConfig{DischargeToGrid: true, SCapacity: 8000, SMin: 200, SMax: 8000, SInitial: 3600, CMax: 3000, DMax: 3000}, larGT, larFT)
	approxAll(t, f(explicit.Batteries[0].StateOfCharge), f(def.Batteries[0].StateOfCharge), 1)
	approxAll(t, f(explicit.GridImport), f(def.GridImport), 1)
	approxAll(t, f(explicit.GridExport), f(def.GridExport), 1)
}

func TestLoadAwareReserveNeverExceedsCapacity(t *testing.T) {
	pyTest(t, "test_load_aware_reserve.py::test_reserve_never_exceeds_capacity")
	r, o := larSolveWith(t, larBattery(true, 1000, 200, 1000), larGT, larFT)
	require.Equal(t, "Optimal", r.Status)
	for _, p := range o.sMinProfile[0] {
		assert.LessOrEqual(t, p, 1000.)
	}
	approx(t, 1000, o.sMinProfile[0][0], 0.1)
}

func TestLoadAwareReserveFlooredAtSMinWhenPvAlwaysCoversLoad(t *testing.T) {
	pyTest(t, "test_load_aware_reserve.py::test_reserve_floored_at_s_min_when_pv_always_covers_load")
	_, o := larSolveWith(t, larBattery(true, 2000, 500, 8000), repeat(100, 6), repeat(100, 6))
	assert.Equal(t, repeat(500, 6), o.sMinProfile[0])
}

func TestLoadAwareReserveCoversASecondNightAfterAPvGap(t *testing.T) {
	pyTest(t, "test_load_aware_reserve.py::test_reserve_covers_a_second_night_after_a_pv_gap")
	_, o := larSolveWith(t, larBattery(true, 4000, 100, 8000),
		[]float64{1000, 1000, 200, 1000, 1000, 200}, []float64{0, 0, 1000, 0, 0, 1000})
	p := o.sMinProfile[0]
	approx(t, 2000/0.95, p[0], 0.5)
	approx(t, 1000/0.95, p[1], 0.5)
	approx(t, 100, p[2], 0.5)
	approx(t, 2000/0.95, p[3], 0.5)
	approx(t, 1000/0.95, p[4], 0.5)
	approx(t, 100, p[5], 0.5)
}

// --- test_penalty_base.py

func pbBuild(t *testing.T, dt, gt []float64, sCapacity, sMin, sInitial, dMax float64) *Optimizer {
	n := len(dt)
	return newOptimizer(t, strat("none", "none"), GridConfig{PMaxImp: fptr(500), PrcPExcImp: fptr(1.0)},
		[]BatteryConfig{{SCapacity: sCapacity, SMin: sMin, SMax: sCapacity, SInitial: sInitial, DMax: dMax}},
		TimeSeriesData{Dt: dt, Gt: gt, Ft: repeat(0, n), PN: repeat(0.0003, n), PE: repeat(0.0003, n)})
}

func TestPenaltySocMinFloorHoldsAgainstALargeDemandRateIncentive(t *testing.T) {
	pyTest(t, "test_penalty_base.py::test_soc_min_floor_holds_against_a_large_demand_rate_incentive")
	o := pbBuild(t, []float64{3600}, []float64{2000}, 1000, 500, 1000, 2000)
	o.CreateModel()
	r := solveOK(t, o)
	require.Equal(t, "Optimal", r.Status)
	assert.GreaterOrEqual(t, *r.Batteries[0].StateOfCharge[0], 500-1.0)
	approx(t, 475, *r.Batteries[0].DischargingPower[0], 1)
	approx(t, 1025, *r.GridImportOvershoot[0], 1)
}

func TestPenaltySocMinFloorHoldsWhenTheDemandPeakSitsInAShortLateStep(t *testing.T) {
	pyTest(t, "test_penalty_base.py::test_soc_min_floor_holds_when_the_demand_peak_sits_in_a_short_late_step")
	dt := cat(repeat(3600, 23), []float64{36})
	gt := cat(repeat(0, 23), []float64{1000})
	r := solveOK(t, pbBuild(t, dt, gt, 100000, 50000, 50000, 200000))
	require.Equal(t, "Optimal", r.Status)
	for _, s := range f(r.Batteries[0].StateOfCharge) {
		assert.GreaterOrEqual(t, s, 50000-1.0)
	}
}
