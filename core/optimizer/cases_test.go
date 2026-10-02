package optimizer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The stored cases of the optimizer repository (test_cases/*.json) and, per case, what the Python
// service answered and every MPS file PuLP handed to CBC on the way, recorded by running the
// service on the case with cbc-go's cmd/cbc as its CBC (see testdata/README).

type testCase struct {
	Strict           bool            `json:"strict"`
	Request          json.RawMessage `json:"request"`
	ExpectedResponse map[string]any  `json:"expected_response"`
}

func readCases(t *testing.T) []string {
	files, err := filepath.Glob("testdata/cases/*.json")
	require.NoError(t, err)
	require.Len(t, files, 20)
	return files
}

// pulpMPS is testdata/mps.sha256: the hashes of the MPS files PuLP wrote, per case in solve order
func pulpMPS(t *testing.T) map[string][]string {
	f, err := os.Open("testdata/mps.sha256")
	require.NoError(t, err)
	defer f.Close()

	res := make(map[string][]string)
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		res[fields[1]] = append(res[fields[1]], fields[0])
	}
	require.NoError(t, s.Err())
	return res
}

// captureMPS records the MPS files of the solves run until the returned function is called
func captureMPS(t *testing.T) func() [][]byte {
	var mps [][]byte
	mpsHook = func(b []byte) { mps = append(mps, append([]byte(nil), b...)) }
	t.Cleanup(func() { mpsHook = nil })
	return func() [][]byte {
		mpsHook = nil
		return mps
	}
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// asJSON is v as decoded JSON, with every number a float64
func asJSON(t *testing.T, v any) any {
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var res any
	require.NoError(t, json.Unmarshal(b, &res))
	return res
}

// isclose is numpy.isclose
func isclose(a, b, rtol, atol float64) bool {
	return math.Abs(a-b) <= atol+rtol*math.Abs(b)
}

func TestCases(t *testing.T) {
	hashes := pulpMPS(t)

	for _, file := range readCases(t) {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(file)
			require.NoError(t, err)
			var tc testCase
			require.NoError(t, json.Unmarshal(b, &tc))

			done := captureMPS(t)
			res, err := OptimizeJSON(context.Background(), tc.Request)
			require.NoError(t, err)
			mps := done()

			// the model: byte for byte the MPS files PuLP wrote, the second one carrying the
			// commitment bound read back from the first solve's solution
			want := hashes[name]
			require.Len(t, mps, len(want), "number of solves")
			for i, m := range mps {
				if sha(m) != want[i] {
					out := filepath.Join(t.TempDir(), name+"."+string(rune('0'+i))+".mps")
					_ = os.WriteFile(out, m, 0o644)
					t.Errorf("solve %d: MPS differs from PuLP's, written to %s", i, out)
				}
			}

			// the response: exactly what the Python service answered
			golden, err := os.ReadFile(filepath.Join("testdata/golden", name+".json"))
			require.NoError(t, err)
			var want_ any
			require.NoError(t, json.Unmarshal(golden, &want_))
			if got := asJSON(t, res); !reflect.DeepEqual(got, want_) {
				t.Errorf("response differs from the Python service's\n got: %v\nwant: %v", got, want_)
			}

			// and what tests/test_app.py checks against the stored expectation
			checkTestApp(t, name, tc, res)
		})
	}
}

// checkTestApp is tests/test_app.py::test_optimizer
func checkTestApp(t *testing.T, name string, tc testCase, res Result) {
	exp := tc.ExpectedResponse
	if exp != nil {
		assert.Equal(t, exp["status"], res.Status, "optimizer status")
		if res.ObjectiveValue != nil && exp["objective_value"] != nil {
			assert.True(t, isclose(*res.ObjectiveValue, exp["objective_value"].(float64), 1e-5, 1e-4),
				"objective value: %v, expected was: %v", *res.ObjectiveValue, exp["objective_value"])
		}
	}

	// 027 is a tie between two export profiles of equal worth: the stored expectation is the
	// one reference CBC picked when it was recorded, cbc-go (like current reference CBC) lands
	// on the other one, and so does the Python service with it.
	if tc.Strict && name != "027-attenuate-feedin-peaks-below-cap" {
		allclose := func(key string, got []*float64, want any) {
			for i, w := range want.([]any) {
				assert.True(t, isclose(*got[i], w.(float64), 0, 1), "%s[%d]: %v, expected %v", key, i, *got[i], w)
			}
		}
		allclose("grid_import", res.GridImport, exp["grid_import"])
		allclose("grid_export", res.GridExport, exp["grid_export"])
		for i, b := range exp["batteries"].([]any) {
			bat := b.(map[string]any)
			allclose("charging_power", res.Batteries[i].ChargingPower, bat["charging_power"])
			allclose("discharging_power", res.Batteries[i].DischargingPower, bat["discharging_power"])
		}
	}

	var req map[string]any
	require.NoError(t, json.Unmarshal(tc.Request, &req))
	dt := req["time_series"].(map[string]any)["dt"].([]any)

	// c_min invariant
	if res.Status == "Optimal" {
		for i, b := range req["batteries"].([]any) {
			bat := b.(map[string]any)
			cMin, _ := bat["c_min"].(float64)
			if cMin == 0 {
				continue
			}
			for t_, c := range res.Batteries[i].ChargingPower {
				step := cMin * dt[t_].(float64) / 3600
				room := bat["s_max"].(float64) - *res.Batteries[i].StateOfCharge[t_]
				assert.True(t, *c <= 1 || *c >= step-1 || room <= step+1,
					"battery %d t=%d: charges %v below c_min step %v with %v Wh room to s_max", i, t_, *c, step, room)
			}
		}
	}

	// reserve invariant
	if res.Status == "Optimal" || res.Status == "Feasible" {
		for i, b := range req["batteries"].([]any) {
			bat := b.(map[string]any)
			sMin, _ := bat["s_min"].(float64)
			if sMin <= 0 || bat["s_initial"].(float64) < sMin {
				continue
			}
			for t_, level := range res.Batteries[i].StateOfCharge {
				assert.GreaterOrEqual(t, *level, sMin-1, "battery %d t=%d", i, t_)
			}
		}
	}
}
