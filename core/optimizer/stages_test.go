package optimizer

// Ports of tests/test_objective_scaling.py, test_objective_split.py and
// test_fractional_solution.py.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildFromCase is the build() helper of test_objective_scaling.py and test_objective_split.py:
// a stored case's request with only the fields those helpers pass on. charging overrides the
// stored charging strategy when not empty.
func buildFromCase(t *testing.T, name, charging string) *Optimizer {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/cases", name+".json"))
	require.NoError(t, err)
	var tc struct {
		Request json.RawMessage `json:"request"`
	}
	require.NoError(t, json.Unmarshal(b, &tc))

	dec := json.NewDecoder(strings.NewReader(string(tc.Request)))
	dec.UseNumber()
	var req map[string]any
	require.NoError(t, dec.Decode(&req))

	stored, _ := req["strategy"].(map[string]any)
	if charging == "" {
		charging = strOr(stored, "charging_strategy", "none")
	}
	g, _ := req["grid"].(map[string]any)

	var batteries []BatteryConfig
	for _, x := range req["batteries"].([]any) {
		bat := x.(map[string]any)
		batteries = append(batteries, BatteryConfig{
			ChargeFromGrid:  boolOr(bat, "charge_from_grid"),
			DischargeToGrid: boolOr(bat, "discharge_to_grid"),
			SCapacity:       orDefault(optNum(bat, "s_capacity"), num(bat["s_max"])),
			SMin:            num(bat["s_min"]),
			SMax:            num(bat["s_max"]),
			SInitial:        num(bat["s_initial"]),
			PDemand:         optList(bat, "p_demand"),
			SGoal:           optList(bat, "s_goal"),
			CMin:            num(bat["c_min"]),
			CMax:            num(bat["c_max"]),
			DMax:            num(bat["d_max"]),
			PA:              num(bat["p_a"]),
			CPriority:       intOr(bat, "c_priority"),
		})
	}
	series := req["time_series"].(map[string]any)

	o, err := NewOptimizer(strat(charging, strOr(stored, "discharging_strategy", "none")),
		GridConfig{PMaxImp: optNum(g, "p_max_imp"), PMaxExp: optNum(g, "p_max_exp"), PrcPExcImp: optNum(g, "prc_p_exc_imp")},
		batteries,
		TimeSeriesData{Dt: list(series["dt"]), Gt: list(series["gt"]), Ft: list(series["ft"]), PN: list(series["p_N"]), PE: list(series["p_E"])},
		orDefault(optNum(req, "eta_c"), 0.95), orDefault(optNum(req, "eta_d"), 0.95), 1e6, DefaultSettings())
	require.NoError(t, err)
	return o
}

// --- test_objective_scaling.py

var (
	scalingCases      = []string{"012-early-charging-not-perfect", "019-unexpected-charge-spikes", "021-min-pv-use-case-with-weird-behavior"}
	scalingStrategies = []string{"none", "charge_before_export", "attenuate_grid_peaks"}
)

func solveAt(t *testing.T, name, charging string, scale *float64) float64 {
	objectiveScaleOverride = scale
	defer func() { objectiveScaleOverride = nil }()

	o := buildFromCase(t, name, charging)
	o.settings.GapAbs = nil
	require.Equal(t, "Optimal", solveOK(t, o).Status)
	return *o.costStageValue
}

func TestScalingNeverCostsObjectiveValue(t *testing.T) {
	for _, charging := range scalingStrategies {
		for _, name := range scalingCases {
			t.Run(charging+"-"+name, func(t *testing.T) {
				pyTest(t, fmt.Sprintf("test_objective_scaling.py::test_scaling_never_costs_objective_value[%s-%s]", charging, name))
				plain := solveAt(t, name, charging, fptr(1.0))
				scaled := solveAt(t, name, charging, nil)
				assert.GreaterOrEqual(t, scaled, plain-math.Abs(plain)*1e-6)
			})
		}
	}
}

func TestDerivedScalePlacesTheLargestCoefficient(t *testing.T) {
	for _, file := range readCases(t) {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		for _, charging := range scalingStrategies {
			o := buildFromCase(t, name, charging)
			o.CreateModel()
			largest := 0.0
			for _, term := range o.problem.objective.terms {
				largest = math.Max(largest, math.Abs(term.c))
			}
			assert.True(t, isclose(largest, objectiveTarget, 1e-9, 0), "%s %s: largest coefficient %g", name, charging, largest)
		}
	}
}

// --- test_objective_split.py

var splitCases = []string{"012-early-charging-not-perfect", "017-battery-charge-priotization-2", "026-attenuate-grid-peaks"}

func TestObjectiveSplitCoversTheWholeObjective(t *testing.T) {
	for _, name := range splitCases {
		t.Run(name, func(t *testing.T) {
			pyTest(t, "test_objective_split.py::test_objective_split_covers_the_whole_objective["+name+"]")
			o := buildFromCase(t, name, "")
			solveOK(t, o)
			combined := (*value(o.costObjective) + *value(o.preferenceObjective)) * o.objectiveScale
			assert.True(t, isclose(combined, *o.problem.objective.value(), 1e-9, 0))
		})
	}
}

func TestObjectiveSplitPreferencesAreNotPaidForWithMoney(t *testing.T) {
	for _, name := range splitCases {
		t.Run(name, func(t *testing.T) {
			pyTest(t, "test_objective_split.py::test_preferences_are_not_paid_for_with_money["+name+"]")
			o := buildFromCase(t, name, "")
			o.settings.ProbeSeconds = fptr(0)
			require.Equal(t, "Optimal", solveOK(t, o).Status)
			assert.Equal(t, "split", o.solvePath)
			assert.Equal(t, "Optimal", o.preferenceStage)
			allowed := o.settings.PreferenceBudget + costBoundSlack + 1e-6
			assert.GreaterOrEqual(t, *value(o.costObjective), *o.costStageValue-allowed)
		})
	}
}

func TestObjectiveSplitPreferenceStageDecidesTheTie(t *testing.T) {
	for _, name := range splitCases {
		t.Run(name, func(t *testing.T) {
			// the cost-only solve ran on pulp's bundled CBC in the Python test, not recorded
			pyTest(t, "test_objective_split.py::test_preference_stage_decides_the_tie["+name+"]", 1)

			// solve_cost_only
			undecidedOpt := buildFromCase(t, name, "")
			undecidedOpt.CreateModel()
			undecidedOpt.problem.setObjective(undecidedOpt.costObjective.mul(undecidedOpt.objectiveScale))
			require.NoError(t, undecidedOpt.solveStage(context.Background(), "cost", nil, nil))
			undecided := *value(undecidedOpt.preferenceObjective)

			decided := buildFromCase(t, name, "")
			decided.settings.ProbeSeconds = fptr(0)
			require.Equal(t, "Optimal", solveOK(t, decided).Status)
			assert.Greater(t, *value(decided.preferenceObjective), undecided)
		})
	}
}

func TestObjectiveSplitEasyRequestsNeverReachTheSplit(t *testing.T) {
	for _, name := range splitCases {
		t.Run(name, func(t *testing.T) {
			pyTest(t, "test_objective_split.py::test_easy_requests_never_reach_the_split["+name+"]")
			o := buildFromCase(t, name, "")
			require.Equal(t, "Optimal", solveOK(t, o).Status)
			assert.Equal(t, "joint", o.solvePath)

			split := buildFromCase(t, name, "")
			split.settings.ProbeSeconds = fptr(0)
			solveOK(t, split)
			joint := *value(o.costObjective) + *value(o.preferenceObjective)
			splitTotal := *value(split.costObjective) + *value(split.preferenceObjective)
			assert.GreaterOrEqual(t, joint, splitTotal-1e-9)
		})
	}
}

// --- test_fractional_solution.py

const fractionalCase = "012-early-charging-not-perfect"

func binaries(o *Optimizer) []*lpVar {
	var res []*lpVar
	for _, v := range o.problem.vars() {
		if v.integer {
			res = append(res, v)
		}
	}
	return res
}

func relax(o *Optimizer, value float64) {
	for _, v := range binaries(o) {
		v.value = fptr(value)
	}
}

func TestFractionalPreferenceStageWithoutAnIntegerSolutionIsNotKept(t *testing.T) {
	pyTest(t, "test_fractional_solution.py::test_a_preference_stage_without_an_integer_solution_is_not_kept")
	o := buildFromCase(t, fractionalCase, "")
	o.settings.ProbeSeconds = fptr(0)
	o.CreateModel()

	calls := 0
	real := o.problem.solve
	o.problem.solve = func(ctx context.Context, p *lpProblem, opts solverOptions) error {
		calls++
		if calls < 3 {
			return real(ctx, p, opts)
		}
		relax(o, 0.3)
		p.status = lpStatusNotSolved
		p.solStatus = lpSolutionNoSolutionFound
		return nil
	}
	_, err := o.Solve(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 3, calls)
	assert.True(t, strings.HasSuffix(o.preferenceStage, "kept the first stage"), o.preferenceStage)
	for _, v := range binaries(o) {
		assert.LessOrEqual(t, math.Min(math.Abs(*v.value), math.Abs(*v.value-1)), integralityTolerance, v.name)
	}
}

func TestFractionalSolutionIsReportedAsNoSchedule(t *testing.T) {
	pyTest(t, "test_fractional_solution.py::test_a_fractional_solution_is_reported_as_no_schedule")
	o := buildFromCase(t, fractionalCase, "")
	o.CreateModel()
	o.probeThenSplitHook = func(context.Context, *time.Time) error {
		relax(o, 0.3)
		o.problem.status = lpStatusOptimal
		o.problem.solStatus = lpSolutionIntegerFeasible
		return nil
	}
	r := solveOK(t, o)
	assert.Equal(t, "Not Solved", r.Status)
	assert.Nil(t, r.ObjectiveValue)
	assert.Empty(t, r.Batteries)
}

func TestFractionalSolutionOnTheIntegersPasses(t *testing.T) {
	for _, value := range []float64{0, 1, integralityTolerance / 2, 1 - integralityTolerance/2} {
		o := buildFromCase(t, fractionalCase, "")
		o.CreateModel()
		relax(o, value)
		assert.True(t, o.isIntegral(), "%v", value)
	}
}

// TestPreferenceStageGetsTheRemainingTime hands the preference stage everything the
// earlier stages left of the deadline, not a fixed share of it.
func TestPreferenceStageGetsTheRemainingTime(t *testing.T) {
	const limit = 10 * time.Second

	o := buildFromCase(t, splitCases[0], "")
	o.settings.ProbeSeconds = fptr(0)
	o.CreateModel()

	var limits []float64
	real := o.problem.solve
	o.problem.solve = func(ctx context.Context, p *lpProblem, opts solverOptions) error {
		if opts.timeLimit != nil {
			limits = append(limits, *opts.timeLimit)
		}
		return real(ctx, p, opts)
	}

	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	_, err := o.Solve(ctx)
	require.NoError(t, err)

	require.NotEmpty(t, limits)
	assert.Equal(t, "Optimal", o.preferenceStage)
	assert.Greater(t, limits[len(limits)-1], 0.5*limit.Seconds(), "the preference stage's limit")
}

func TestWithoutProbeGoesStraightToTheSplit(t *testing.T) {
	o := buildFromCase(t, splitCases[0], "")
	WithoutProbe()(&o.settings)
	require.Equal(t, "Optimal", solveOK(t, o).Status)
	assert.Equal(t, "split", o.solvePath, "an easy request would settle in the probe")
}

func TestStageReportNamesEachSolve(t *testing.T) {
	o := buildFromCase(t, splitCases[0], "")
	WithoutProbe()(&o.settings)

	var stages []Stage
	WithStageReport(func(s Stage) { stages = append(stages, s) })(&o.settings)
	solveOK(t, o)

	names := make([]string, 0, len(stages))
	for _, s := range stages {
		names = append(names, s.Name)
		assert.True(t, strings.HasPrefix(s.Status, "Optimal"), "%s: %s", s.Name, s.Status)
		assert.Positive(t, s.Took, s.Name)
	}
	assert.Equal(t, []string{"cost", "preferences"}, names[len(names)-2:])
}

// TestPreferenceStageStoppedOnTimeIsNotOptimal reports a preference stage that
// stopped on time with a solution as feasible, the way CBC means it.
func TestPreferenceStageStoppedOnTimeIsNotOptimal(t *testing.T) {
	o := buildFromCase(t, splitCases[0], "")
	o.settings.ProbeSeconds = fptr(0)
	o.CreateModel()

	real := o.problem.solve
	o.problem.solve = func(ctx context.Context, p *lpProblem, opts solverOptions) error {
		if err := real(ctx, p, opts); err != nil {
			return err
		}
		if o.costStageValue != nil { // the preference stage
			p.solStatus = lpSolutionIntegerFeasible // "Stopped on time - objective value ..."
		}
		return nil
	}
	_, err := o.Solve(context.Background())
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(o.preferenceStage, "Feasible"), o.preferenceStage)
}
