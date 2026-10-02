package optimizer

import (
	"bufio"
	"context"
	"math"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	scenarioOnce   sync.Once
	scenarioHashes map[string][]string
)

// pyTest ties a Go test to the Python test it ports: every MPS file the Go test hands to CBC must
// hash to the one PuLP wrote, in the same order, when the Python test ran (testdata/
// scenarios.sha256). uncaptured is the number of leading Go solves the Python ran on a CBC the
// recording did not see (pulp's bundled binary).
func pyTest(t *testing.T, id string, uncaptured ...int) {
	t.Helper()
	scenarioOnce.Do(func() {
		f, err := os.Open("testdata/scenarios.sha256")
		require.NoError(t, err)
		defer f.Close()
		scenarioHashes = make(map[string][]string)
		s := bufio.NewScanner(f)
		for s.Scan() {
			fields := strings.Fields(s.Text())
			scenarioHashes[fields[1]] = append(scenarioHashes[fields[1]], fields[0])
		}
	})

	want, ok := scenarioHashes[id]
	require.True(t, ok, "no recorded models for %s", id)

	var got []string
	mpsHook = func(b []byte) { got = append(got, sha(b)) }
	t.Cleanup(func() {
		mpsHook = nil
		if len(uncaptured) > 0 {
			got = got[min(uncaptured[0], len(got)):]
		}
		assert.Equal(t, want, got, "MPS files differ from PuLP's for %s", id)
	})
}

// newOptimizer is Optimizer(...) with the constructor defaults the Python tests rely on
func newOptimizer(t *testing.T, strategy OptimizationStrategy, grid GridConfig, batteries []BatteryConfig, ts TimeSeriesData, eta ...float64) *Optimizer {
	t.Helper()
	etaC, etaD := 0.95, 0.95
	if len(eta) == 2 {
		etaC, etaD = eta[0], eta[1]
	}
	o, err := NewOptimizer(strategy, grid, batteries, ts, etaC, etaD, 1e6, DefaultSettings())
	require.NoError(t, err)
	return o
}

func solveOK(t *testing.T, o *Optimizer) Result {
	t.Helper()
	res, err := o.Solve(context.Background())
	require.NoError(t, err)
	return res
}

func strat(charging, discharging string) OptimizationStrategy {
	return OptimizationStrategy{ChargingStrategy: charging, DischargingStrategy: discharging}
}

func repeat(v float64, n int) []float64 {
	res := make([]float64, n)
	for i := range res {
		res[i] = v
	}
	return res
}

func cat(a ...[]float64) []float64 {
	var res []float64
	for _, x := range a {
		res = append(res, x...)
	}
	return res
}

func f(vs []*float64) []float64 {
	res := make([]float64, len(vs))
	for i, v := range vs {
		res[i] = *v
	}
	return res
}

func sum(vs []float64) float64 {
	s := 0.0
	for _, v := range vs {
		s += v
	}
	return s
}

// approx is pytest.approx(want, abs=abs) for scalars
func approx(t *testing.T, want, got, abs float64, msg ...any) {
	t.Helper()
	assert.InDelta(t, want, got, abs, msg...)
}

// approxRel is pytest.approx(want, rel=rel)
func approxRel(t *testing.T, want, got, rel float64, msg ...any) {
	t.Helper()
	assert.LessOrEqual(t, math.Abs(got-want), rel*math.Abs(want), msg...)
}

// approxAll is pytest.approx(want, abs=abs) for lists
func approxAll(t *testing.T, want, got []float64, abs float64, msg ...any) {
	t.Helper()
	require.Len(t, got, len(want), msg...)
	for i := range want {
		assert.InDelta(t, want[i], got[i], abs, msg...)
	}
}

func stddev(x []float64) float64 {
	m := sum(x) / float64(len(x))
	s := 0.0
	for _, v := range x {
		s += (v - m) * (v - m)
	}
	return math.Sqrt(s / float64(len(x)))
}
