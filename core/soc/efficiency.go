package soc

import (
	"math"
	"time"

	"github.com/benbjohnson/clock"
)

const (
	// efficiencyMemory is the learner's memory in soc points: older intervals fade
	// with exp(-points/efficiencyMemory). A pack cycles ~50-100 points a day, so
	// this spans weeks - long enough to average out the raw reading's lag and
	// step size, short enough to follow seasons and slow aging.
	efficiencyMemory = 2000

	// efficiencyMinPoints is how many soc points each direction needs before the
	// model is trusted; below it callers keep their defaults.
	efficiencyMinPoints = 50

	// efficiencySettle is how long power must have been steady before an interval
	// may start: longer than the raw reading lags power (a few minutes on the
	// Powerwall Fleet API), so the reading reflects the power being measured.
	efficiencySettle = 15 * time.Minute

	// efficiencyStep is the power ratio treated as a new operating point (a boost,
	// export or charge starting), as opposed to the house load wandering.
	efficiencyStep = 2.0

	// efficiencyRun is how many consecutive accepted intervals make one row of the
	// fit. At high power a point passes every sample or two, so a single
	// interval's energy and time jitter by a sample's worth - noise in the
	// regressors that biases the fit; pooled over a run it shrinks with the run's
	// square length. Rows end on a count, not a duration: ending on a duration
	// selects the interval straddling the deadline, which is likely a long one.
	efficiencyRun = 8
)

// EfficiencyState is the persisted learner state: the decayed normal equations
// of the fit (see EfficiencyLearner) and the points seen per direction.
type EfficiencyState struct {
	XtX          [9]float64 `json:"xtx"`
	XtY          [3]float64 `json:"xty"`
	ChargePts    float64    `json:"chargePts"`
	DischargePts float64    `json:"dischargePts"`
}

// EfficiencyModel is how the pack really converts metered energy to soc.
type EfficiencyModel struct {
	EtaC    float64 // stored per metered Wh going in
	EtaD    float64 // metered Wh coming out per stored Wh
	Standby float64 // W drawn from storage regardless of power (self-consumption)
}

// EfficiencyLearner measures how the pack really converts the energy its meter
// sees into soc. A nominal capacity with a generic efficiency can be well off: a
// Powerwall 3 delivers only ~108 Wh per point it loses overnight, yet ~120 at
// evening export power, because its own standby draw comes out of storage on top
// of what the meter sees. So soc moves as
//
//	discharging:  points × nominal Wh = E / eta_d + standby × T
//	charging:     points × nominal Wh = E × eta_c − standby × T
//
// for metered energy E over time T, which is linear in (1/eta_d, eta_c,
// standby) and is fitted by least squares over the intervals between raw soc
// changes. Under steady power the raw reading's lag only shifts those
// boundaries. Where power steps, stops or turns it no longer holds: for a lag's
// worth of time the reading still reflects the previous power, moving lag × the
// step's energy into the wrong interval. So an interval only counts when it
// starts efficiencySettle after the last such step and holds no other.
type EfficiencyLearner struct {
	clck clock.Clock

	initialized bool
	lastRawSoc  float64
	lastTime    time.Time
	chargeWh    float64       // charged since lastRawSoc
	dischargeWh float64       // discharged since lastRawSoc
	elapsed     time.Duration // since lastRawSoc
	intervalAt  time.Time     // when the current interval started (lastRawSoc changed)
	refPower    float64       // power at the last step
	steadySince time.Time     // time of the last step, sign change or idle sample

	// accepted intervals pooled into the current run
	runWh  float64
	runDur time.Duration
	runPts float64
	runN   int
	runDir int

	state EfficiencyState
}

// NewEfficiencyLearner creates a learner seeded with a persisted state.
func NewEfficiencyLearner(state EfficiencyState) *EfficiencyLearner {
	return &EfficiencyLearner{clck: clock.New(), state: state}
}

// Observe feeds one sample: the raw (unsmoothed) soc in % and the battery power
// in W (negative = charging). It reports whether the fit changed.
func (l *EfficiencyLearner) Observe(rawSoc, power float64) bool {
	now := l.clck.Now()

	if !l.initialized {
		l.initialized = true
		l.lastRawSoc = rawSoc
		l.lastTime = now
		l.intervalAt = now
		l.refPower = power
		l.steadySince = now
		return false
	}

	dt := now.Sub(l.lastTime)
	l.lastTime = now
	l.elapsed += dt
	if power < 0 {
		l.chargeWh += -power * dt.Hours()
	} else {
		l.dischargeWh += power * dt.Hours()
	}

	// a new operating point: the reading lags it for a while
	if r := power / l.refPower; power == 0 || !(r > 0) || r > efficiencyStep || r < 1/efficiencyStep {
		l.refPower = power
		l.steadySince = now
	}

	if rawSoc == l.lastRawSoc {
		return false
	}

	delta := rawSoc - l.lastRawSoc
	charged, discharged, elapsed, start := l.chargeWh, l.dischargeWh, l.elapsed, l.intervalAt
	l.lastRawSoc = rawSoc
	l.chargeWh, l.dischargeWh, l.elapsed, l.intervalAt = 0, 0, 0, now

	// the reading may still be catching up on a previous operating point, or the
	// interval holds a step; a steady interval also only moves one way
	if start.Before(l.steadySince.Add(efficiencySettle)) || (delta > 0) != (l.refPower < 0) {
		l.resetRun()
		return false
	}
	dir := 1
	if delta < 0 {
		dir = -1
	}

	if dir != l.runDir {
		l.resetRun()
		l.runDir = dir
	}
	l.runWh += charged + discharged
	l.runDur += elapsed
	l.runPts += math.Abs(delta)
	if l.runN++; l.runN < efficiencyRun {
		return false
	}

	l.addRow(l.runDir, l.runWh, l.runDur.Hours(), l.runPts)
	l.resetRun()
	l.runDir = dir
	return true
}

func (l *EfficiencyLearner) resetRun() {
	l.runWh, l.runDur, l.runPts, l.runN, l.runDir = 0, 0, 0, 0, 0
}

// addRow adds one run to the fit: points = x · (1/eta_d, eta_c, standby) / nominal,
// nominal being applied in Model.
func (l *EfficiencyLearner) addRow(dir int, wh, h, pts float64) {
	x := [3]float64{wh, 0, h}
	if dir > 0 {
		x = [3]float64{0, wh, -h}
		l.state.ChargePts += pts
	} else {
		l.state.DischargePts += pts
	}

	decay := math.Exp(-pts / efficiencyMemory)
	for i := range 3 {
		for j := range 3 {
			l.state.XtX[3*i+j] = l.state.XtX[3*i+j]*decay + x[i]*x[j]
		}
		l.state.XtY[i] = l.state.XtY[i]*decay + x[i]*pts
	}
}

// State returns the state to persist.
func (l *EfficiencyLearner) State() EfficiencyState {
	return l.state
}

// Model returns the fitted model for a pack of the given nominal capacity (kWh),
// so that capacity × soc stays the stored energy. It reports false while either
// direction has too few points or the fit is not physical (efficiencies outside
// (0, 1], negative standby).
func (l *EfficiencyLearner) Model(capacity float64) (EfficiencyModel, bool) {
	nominal := capacity * 10 // Wh per point
	if nominal <= 0 || l.state.ChargePts < efficiencyMinPoints || l.state.DischargePts < efficiencyMinPoints {
		return EfficiencyModel{}, false
	}

	beta, ok := solve3(l.state.XtX, l.state.XtY)
	if !ok {
		return EfficiencyModel{}, false
	}

	m := EfficiencyModel{
		EtaD:    1 / (beta[0] * nominal),
		EtaC:    beta[1] * nominal,
		Standby: beta[2] * nominal,
	}
	if !(m.EtaD > 0 && m.EtaD <= 1 && m.EtaC > 0 && m.EtaC <= 1 && m.Standby >= 0) {
		return EfficiencyModel{}, false
	}
	return m, true
}

// solve3 solves the row-major 3x3 system a·x = b by Cramer's rule, reporting
// false when a is (near) singular.
func solve3(a [9]float64, b [3]float64) ([3]float64, bool) {
	det := func(m [9]float64) float64 {
		return m[0]*(m[4]*m[8]-m[5]*m[7]) - m[1]*(m[3]*m[8]-m[5]*m[6]) + m[2]*(m[3]*m[7]-m[4]*m[6])
	}

	d := det(a)
	scale := 0.0
	for _, v := range a {
		scale = max(scale, math.Abs(v))
	}
	if scale == 0 || math.Abs(d) <= 1e-12*scale*scale*scale {
		return [3]float64{}, false
	}

	var x [3]float64
	for c := range 3 {
		m := a
		for r := range 3 {
			m[3*r+c] = b[r]
		}
		x[c] = det(m) / d
	}
	return x, true
}
