package soc

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const simCapacity = 13.5 // kWh, 135 Wh per point

func newTestLearner(clk clock.Clock) *EfficiencyLearner {
	l := NewEfficiencyLearner(EfficiencyState{})
	l.clck = clk
	return l
}

// simPack is a pack with true efficiencies and standby draw, reported as a
// whole-percent reading lagging the truth by lag.
type simPack struct {
	clk     *clock.Mock
	l       *EfficiencyLearner
	m       EfficiencyModel
	soc     float64
	lag     time.Duration
	history []float64 // true soc per step, for the lagged reading
	rnd     *rand.Rand
}

const simStep = 90 * time.Second

func newSimPack(lag time.Duration) *simPack {
	clk := clock.NewMock()
	p := &simPack{clk: clk, l: newTestLearner(clk), m: EfficiencyModel{EtaC: 0.97, EtaD: 0.91, Standby: 66}, soc: 50, lag: lag, rnd: rand.New(rand.NewPCG(1, 2))}
	p.l.Observe(50, 0)
	return p
}

// run holds power around the given level, wandering ±20% like a house load
func (p *simPack) run(level float64, d time.Duration) {
	for range int(d / simStep) {
		power := level * (0.9 + 0.2*p.rnd.Float64())
		p.clk.Add(simStep)
		h := simStep.Hours()
		stored := -p.m.Standby * h
		if power < 0 {
			stored += -power * h * p.m.EtaC
		} else {
			stored -= power * h / p.m.EtaD
		}
		p.soc = min(max(p.soc+stored/(simCapacity*10), 0), 100)
		p.history = append(p.history, p.soc)
		lagged := p.history[max(0, len(p.history)-1-int(p.lag/simStep))]
		p.l.Observe(math.Floor(lagged), power)
	}
}

// day is a PV charge, an evening discharge at export power and a night of house load
func (p *simPack) day() {
	p.run(-3000, 3*time.Hour)
	p.run(0, time.Hour)
	p.run(5000, time.Hour)
	p.run(450, 10*time.Hour)
	p.run(-1200, 2*time.Hour+30*time.Minute)
}

func TestEfficiencyLearnerFitsEfficienciesAndStandbyDespiteLag(t *testing.T) {
	p := newSimPack(6 * time.Minute)
	for range 10 {
		p.day()
	}

	m, ok := p.l.Model(simCapacity)
	require.True(t, ok)
	assert.InDelta(t, 0.97, m.EtaC, 0.02)
	assert.InDelta(t, 0.91, m.EtaD, 0.02)
	assert.InDelta(t, 66, m.Standby, 10)
}

func TestEfficiencyLearnerUnknownUntilBothDirectionsSeen(t *testing.T) {
	p := newSimPack(0)
	p.run(-3000, 3*time.Hour) // charging only
	_, ok := p.l.Model(simCapacity)
	assert.False(t, ok)
}

func TestEfficiencyLearnerStatePersists(t *testing.T) {
	p := newSimPack(0)
	for range 3 {
		p.day()
	}
	want, ok := p.l.Model(simCapacity)
	require.True(t, ok)

	got, ok := NewEfficiencyLearner(p.l.State()).Model(simCapacity)
	require.True(t, ok)
	assert.Equal(t, want, got)
}

func TestEfficiencyLearnerWaitsForPowerToSettle(t *testing.T) {
	clk := clock.NewMock()
	l := newTestLearner(clk)
	l.Observe(50, 0)

	// charging starts: intervals within the settle time after the step don't count
	clk.Add(5 * time.Minute)
	l.Observe(51, -1000)
	clk.Add(5 * time.Minute)
	l.Observe(52, -1000) // starts 5m after the step
	clk.Add(10 * time.Minute)
	l.Observe(53, -1000) // starts 10m after the step
	assert.Zero(t, l.runN)
	clk.Add(10 * time.Minute)
	l.Observe(54, -1000) // starts 20m after the step
	assert.Equal(t, 1, l.runN)

	// house load wandering within the step ratio keeps counting
	clk.Add(10 * time.Minute)
	l.Observe(55, -1500)
	assert.Equal(t, 2, l.runN)

	// a step within the interval voids it, the run, and the next settle time
	clk.Add(5 * time.Minute)
	l.Observe(55, -4000)
	clk.Add(5 * time.Minute)
	l.Observe(56, -4000)
	assert.Zero(t, l.runN)
}

func TestEfficiencyLearnerSkipsIdleAndReversals(t *testing.T) {
	clk := clock.NewMock()
	l := newTestLearner(clk)
	l.Observe(50, -1000)
	clk.Add(time.Hour)
	l.Observe(51, -1000) // starts at the step
	clk.Add(time.Hour)
	l.Observe(52, -1000)
	require.Equal(t, 1, l.runN)

	// idle sample within the interval
	clk.Add(30 * time.Minute)
	l.Observe(52, 0)
	clk.Add(30 * time.Minute)
	l.Observe(53, -1000)
	assert.Zero(t, l.runN)

	// discharging, but the reading rises (still catching up)
	clk.Add(time.Hour)
	l.Observe(53, 500)
	clk.Add(time.Hour)
	l.Observe(54, 500)
	assert.Zero(t, l.runN)
}

func TestEfficiencyLearnerRowsEveryRunOfIntervals(t *testing.T) {
	clk := clock.NewMock()
	l := newTestLearner(clk)
	l.Observe(50, -1000)
	clk.Add(time.Hour)
	l.Observe(51, -1000) // starts at the step

	for i := range efficiencyRun {
		clk.Add(10 * time.Minute)
		assert.Equal(t, i == efficiencyRun-1, l.Observe(float64(52+i), -1000))
	}
	assert.Equal(t, float64(efficiencyRun), l.State().ChargePts)
}

func TestSolve3(t *testing.T) {
	x, ok := solve3([9]float64{2, 1, 0, 1, 3, 1, 0, 1, 4}, [3]float64{3, 5, 5})
	require.True(t, ok)
	assert.InDeltaSlice(t, []float64{1, 1, 1}, x[:], 1e-12)

	_, ok = solve3([9]float64{1, 2, 3, 2, 4, 6, 1, 1, 1}, [3]float64{1, 2, 3})
	assert.False(t, ok, "singular")
}
