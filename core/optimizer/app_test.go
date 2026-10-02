package optimizer

// Ports of tests/test_unknown_strategy_is_rejected.py and the request handling tests of
// tests/test_app.py, plus the error answers of the service and the client conversion.
// test_subject_logging_is_configurable and test_slow_requests_are_dumped cover service-only
// features (JWT, the slow request dump) and have no counterpart here.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	client "github.com/evcc-io/optimizer/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseRequest() map[string]any {
	return map[string]any{
		"batteries": []any{map[string]any{
			"s_min": 0, "s_max": 10000, "s_initial": 5000,
			"c_min": 0, "c_max": 5000, "d_max": 5000, "p_a": 0.1,
		}},
		"time_series": map[string]any{
			"dt":  []any{3600, 3600},
			"gt":  []any{1000, 1000},
			"ft":  []any{0, 0},
			"p_N": []any{0.3, 0.3},
			"p_E": []any{0.1, 0.1},
		},
	}
}

func post(t *testing.T, req map[string]any) (Result, error) {
	t.Helper()
	b, err := json.Marshal(req)
	require.NoError(t, err)
	return OptimizeJSON(context.Background(), b)
}

func TestUnknownStrategyIsRejected(t *testing.T) {
	for _, s := range []map[string]any{
		{"charging_strategy": "attenuate_fedin_peaks"},
		{"discharging_strategy": "discharge_before_export"},
		{"charging_strategy": "discharge_before_import"},
		{"charging_strategy": "not_a_real_strategy"},
	} {
		req := baseRequest()
		req["strategy"] = s
		_, err := post(t, req)
		var e *Error
		require.ErrorAs(t, err, &e, "%v was accepted", s)
		assert.Equal(t, 400, e.Code)
		b, _ := json.Marshal(e.Details)
		assert.Contains(t, string(b), "not one of")
	}

	_, err := post(t, baseRequest())
	require.NoError(t, err)
	for _, c := range ChargingStrategies {
		for _, d := range DischargingStrategies {
			req := baseRequest()
			req["strategy"] = map[string]any{"charging_strategy": c, "discharging_strategy": d}
			_, err := post(t, req)
			assert.NoError(t, err, "%s/%s was rejected", c, d)
		}
	}
}

// The service's answers to these requests, recorded from the Python app
func TestErrorAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mod     func(map[string]any)
		code    int
		message string
		details map[string]string
	}{
		{"length mismatch", func(r map[string]any) { r["time_series"].(map[string]any)["p_E"] = []any{0.1} },
			400, "Invalid data format: 400 Bad Request: The browser (or proxy) sent a request that this server could not understand.", nil},
		{"goal length", func(r map[string]any) { r["batteries"].([]any)[0].(map[string]any)["s_goal"] = []any{1.0} },
			400, "Invalid data format: 400 Bad Request: The browser (or proxy) sent a request that this server could not understand.", nil},
		{"strategy", func(r map[string]any) {
			r["strategy"] = map[string]any{"charging_strategy": "x", "discharging_strategy": "y"}
		}, 400, "Input payload validation failed", map[string]string{
			"strategy.charging_strategy":    "'x' is not one of ['none', 'charge_before_export', 'attenuate_demand_peaks', 'attenuate_feedin_peaks', 'attenuate_grid_peaks']",
			"strategy.discharging_strategy": "'y' is not one of ['none', 'discharge_before_import']",
		}},
		{"empty strategy", func(r map[string]any) { r["strategy"] = map[string]any{"charging_strategy": ""} },
			400, "Input payload validation failed", map[string]string{
				"strategy.charging_strategy": "'' is not one of ['none', 'charge_before_export', 'attenuate_demand_peaks', 'attenuate_feedin_peaks', 'attenuate_grid_peaks']",
			}},
		{"null batteries", func(r map[string]any) { r["batteries"] = nil },
			400, "Input payload validation failed", map[string]string{"batteries": "None is not of type 'array'"}},
		{"null dt", func(r map[string]any) { r["time_series"].(map[string]any)["dt"] = nil },
			400, "Input payload validation failed", map[string]string{"time_series.dt": "None is not of type 'array'"}},
		{"null gt", func(r map[string]any) { r["time_series"].(map[string]any)["gt"] = nil },
			400, "Input payload validation failed", map[string]string{"time_series.gt": "None is not of type 'array'"}},
		{"missing s_min", func(r map[string]any) { delete(r["batteries"].([]any)[0].(map[string]any), "s_min") },
			400, "Input payload validation failed", map[string]string{"batteries.0.s_min": "'s_min' is a required property"}},
		{"short dt", func(r map[string]any) { r["time_series"].(map[string]any)["dt"] = []any{3600} },
			500, "Optimization failed: list index out of range", nil},
		{"empty series", func(r map[string]any) {
			r["time_series"] = map[string]any{"dt": []any{}, "gt": []any{}, "ft": []any{}, "p_N": []any{}, "p_E": []any{}}
		}, 500, "Optimization failed: zero-size array to reduction operation minimum which has no identity", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			tc.mod(req)
			_, err := post(t, req)
			var e *Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, tc.code, e.Code)
			assert.Equal(t, tc.message, e.Message)
			assert.Equal(t, tc.details, e.Details)
		})
	}
}

// The service's answers to two small requests, recorded from the Python app
func TestSmallRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(map[string]any)
		golden string
	}{
		{"ok", func(map[string]any) {}, `{"status": "Optimal", "objective_value": -210.52632, "limit_violations": {"grid_import_limit_exceeded": false, "grid_export_limit_hit": false}, "batteries": [{"charging_power": [0.0, 0.0], "discharging_power": [1000.0, 1000.0], "state_of_charge": [3947.3684, 2894.7368]}], "grid_import": [0.0, 0.0], "grid_export": [0.0, 0.0], "flow_direction": [0, 0], "grid_import_overshoot": [], "grid_export_overshoot": []}`},
		{"no batteries", func(r map[string]any) { r["batteries"] = []any{} }, `{"status": "Optimal", "objective_value": -600.0, "limit_violations": {"grid_import_limit_exceeded": false, "grid_export_limit_hit": false}, "batteries": [], "grid_import": [1000.0, 1000.0], "grid_export": [0.0, 0.0], "flow_direction": [0, 0], "grid_import_overshoot": [], "grid_export_overshoot": []}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			tc.mod(req)
			res, err := post(t, req)
			require.NoError(t, err)
			var want any
			require.NoError(t, json.Unmarshal([]byte(tc.golden), &want))
			assert.Equal(t, want, asJSON(t, res))
		})
	}
}

func TestAbortReturnsJSONMessage(t *testing.T) {
	req := baseRequest()
	req["time_series"].(map[string]any)["p_E"] = []any{0.1}
	_, err := post(t, req)
	var e *Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, 400, e.Code)
	assert.NotEmpty(t, e.Message)
}

func TestUnprovedSolutionsReportFeasible(t *testing.T) {
	pyTest(t, "test_app.py::test_unproved_solutions_report_feasible")

	var tc testCase
	b, err := os.ReadFile("testdata/cases/012-early-charging-not-perfect.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &tc))

	res, err := OptimizeJSON(context.Background(), tc.Request)
	require.NoError(t, err)
	assert.Equal(t, "Optimal", res.Status)

	solveFunc = func(ctx context.Context, p *lpProblem, opts solverOptions) error {
		err := solveCBC(ctx, p, opts)
		p.solStatus = lpSolutionIntegerFeasible
		return err
	}
	defer func() { solveFunc = solveCBC }()

	res, err = OptimizeJSON(context.Background(), tc.Request)
	require.NoError(t, err)
	assert.Equal(t, "Feasible", res.Status)
	assert.NotNil(t, res.ObjectiveValue)
	var req map[string]any
	require.NoError(t, json.Unmarshal(tc.Request, &req))
	n := len(req["time_series"].(map[string]any)["dt"].([]any))
	assert.Len(t, res.Batteries[0].ChargingPower, n)
	assert.Len(t, res.GridImport, n)
}

func TestReserveFloorHoldsAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("solves a full horizon")
	}
	b, err := os.ReadFile("testdata/data/discharge_to_grid_full_request.json")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := OptimizeJSON(ctx, b)
	require.NoError(t, err)
	require.Contains(t, []string{"Optimal", "Feasible"}, res.Status)

	var req map[string]any
	require.NoError(t, json.Unmarshal(b, &req))
	for i, x := range req["batteries"].([]any) {
		bat := x.(map[string]any)
		sMin, _ := bat["s_min"].(float64)
		if sMin <= 0 || bat["s_initial"].(float64) < sMin {
			continue
		}
		for _, s := range f(res.Batteries[i].StateOfCharge) {
			assert.GreaterOrEqual(t, s, sMin-1, "battery %d", i)
		}
	}
}

// Optimize takes the client request the way it went over the wire: zero values the client omits
// are the service's defaults, and the response converts like the client decoded the JSON.
func TestOptimizeClient(t *testing.T) {
	in := client.OptimizationInput{
		Strategy: client.OptimizerStrategy{DischargingStrategy: client.OptimizerStrategyDischargingStrategyDischargeBeforeImport},
		EtaC:     0.9,
		EtaD:     0.9,
		Batteries: []client.BatteryConfig{{
			ChargeFromGrid: true, SMax: 10000, SInitial: 5000, CMax: 5000, DMax: 5000, PA: 0.0001,
			SGoal: []float32{0, 0, 9000},
		}},
		TimeSeries: client.TimeSeries{
			Dt: []int{3600, 3600, 3600},
			Gt: []float32{1000, 1000, 1000},
			Ft: []float32{0, 2500, 0},
			PN: []float32{0.0003, 0.0002, 0.0003},
			PE: []float32{0.0001, 0.0001, 0.0001},
		},
	}

	got, err := Optimize(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, client.Optimal, got.Status)

	// the same request as the JSON the service received
	body, err := json.Marshal(in)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "s_capacity", "zero values are omitted")
	res, err := OptimizeJSON(context.Background(), body)
	require.NoError(t, err)

	// what the client decodes from the service's JSON answer
	answer, err := json.Marshal(res)
	require.NoError(t, err)
	var want client.OptimizationResult
	require.NoError(t, json.Unmarshal(answer, &want))
	assert.Equal(t, want, got)

	// an error answer
	in.TimeSeries.PE = in.TimeSeries.PE[:1]
	_, err = Optimize(context.Background(), in)
	var e *Error
	require.True(t, errors.As(err, &e))
	assert.Equal(t, 400, e.Code)
}
