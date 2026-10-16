package optimizer

// Port of src/optimizer/optimizer.py of github.com/evcc-io/optimizer (branch
// blampe/optimizer-redesign, e2b5f03). Function boundaries and names follow the Python, so a
// change there can be carried over line by line; comments starting with "py:" point at the
// Python statement a block reproduces. The Python comments explaining the model are not repeated
// here, read them next to this file.
//
// Expressions are built with the same PuLP operations, in the same order, as the Python builds
// them (see pulp.go): `a * v` becomes affine(v).mul(a), `n - v` rsub(n, affine(v)), `v - n`
// affine(v).addConst(n, -1), `e1 <= e2` le(e1, e2) and `e <= n` leNum(e, n). A nil *lpExpr stands
// for Python's int 0 before the first `+=` turns an accumulator into an expression.

import (
	"context"
	"fmt"
	"math"
	"time"
)

type OptimizationStrategy struct {
	ChargingStrategy    string
	DischargingStrategy string
}

// PeakStrategySides is PEAK_STRATEGY_SIDES
var peakStrategySides = map[string][]string{
	"attenuate_demand_peaks": {"imp"},
	"attenuate_feedin_peaks": {"exp"},
	"attenuate_grid_peaks":   {"imp", "exp"},
}

// ChargingStrategies and DischargingStrategies are CHARGING_STRATEGIES and DISCHARGING_STRATEGIES
var (
	ChargingStrategies    = []string{"none", "charge_before_export", "attenuate_demand_peaks", "attenuate_feedin_peaks", "attenuate_grid_peaks"}
	DischargingStrategies = []string{"none", "discharge_before_import"}
)

// objectiveTarget is OBJECTIVE_TARGET
const objectiveTarget = 1e6

// objectiveScaleOverride is OBJECTIVE_SCALE, nil derives the factor. Tests pin it.
var objectiveScaleOverride *float64

// objectiveScale is objective_scale()
func objectiveScale(objective *lpExpr) float64 {
	if objectiveScaleOverride != nil {
		return *objectiveScaleOverride
	}
	largest, found := 0.0, false
	if objective != nil {
		for _, t := range objective.terms {
			if t.c != 0 {
				if c := math.Abs(t.c); !found || c > largest {
					largest, found = c, true
				}
			}
		}
	}
	if !found {
		return 1.0
	}
	return objectiveTarget / largest
}

const (
	commitmentBound      = "commitment_bound"
	commitmentBoundSlack = 1e-5
	costBound            = "cost_bound"
	costBoundSlack       = 1e-5
	costBoundTolerance   = 1e-4
	integralityTolerance = 1e-5
	probeShare           = 0.2
	preferenceTimeShare  = 0.25
)

// peakSideVariables is PEAK_SIDE_VARIABLES
var peakSideVariables = map[string][2]string{
	"imp": {"n", "e_imp_lim_exc"},
	"exp": {"e", "e_exp_lim_exc"},
}

// GridConfig mirrors the Python dataclass, nil is None
type GridConfig struct {
	PMaxImp    *float64
	PMaxExp    *float64
	PrcPExcImp *float64
	PrcEExcImp *float64
	PrcEExcExp *float64
}

// BatteryConfig mirrors the Python dataclass. A nil slice or pointer is None; a non-nil empty
// slice is an empty list, which the Python treats differently in places (see cMaxAt).
type BatteryConfig struct {
	ChargeFromGrid  bool
	DischargeToGrid bool
	SCapacity       float64
	SMin            float64
	SMax            float64
	SInitial        float64
	CMin            float64
	CMax            float64
	DMax            float64
	PA              float64
	PDemand         []float64
	SGoal           []float64
	CMaxT           []float64
	SDrainT         []float64
	CPriority       int
	PrcReserve      *float64
	PrcGoal         *float64
	ReserveForLoad  bool
	ServeHomeFirst  bool
	Charging        bool     // already charging as the horizon starts
	EtaC, EtaD      *float64 // this battery's efficiencies, nil for the global ones
}

// etaCOf is battery i's charge efficiency: its own, else the global one
func (o *Optimizer) etaCOf(i int) float64 {
	return orDefault(o.batteries[i].EtaC, o.etaC)
}

// etaDOf is battery i's discharge efficiency: its own, else the global one
func (o *Optimizer) etaDOf(i int) float64 {
	return orDefault(o.batteries[i].EtaD, o.etaD)
}

// cMaxAt is c_max_at()
func cMaxAt(bat *BatteryConfig, t int) float64 {
	if len(bat.CMaxT) > 0 {
		return bat.CMaxT[t]
	}
	return bat.CMax
}

// sDrainAt is s_drain_at()
func sDrainAt(bat *BatteryConfig, t int) float64 {
	if len(bat.SDrainT) > 0 {
		return bat.SDrainT[t]
	}
	return 0.0
}

type TimeSeriesData struct {
	Dt []float64
	Gt []float64
	Ft []float64
	PN []float64
	PE []float64
}

// Settings is OptimizerSettings (settings.py), without the service-only fields
type Settings struct {
	// GapAbs is the absolute MIP gap of the cost stage in currency units, nil solves to optimality
	GapAbs *float64
	// ProbeSeconds for the joint solve, nil derives it from the time limit
	ProbeSeconds *float64
	// PreferenceBudget is the money the preference stage may spend
	PreferenceBudget float64
}

// DefaultSettings are the OptimizerSettings defaults
func DefaultSettings() Settings {
	return Settings{GapAbs: fptr(0.01)}
}

// pyMax and pyMin are Python's max(a, b) and min(a, b) for two numbers
func pyMax(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

func pyMin(a, b float64) float64 {
	if b < a {
		return b
	}
	return a
}

type variables struct {
	c, d, s          [][]*lpVar
	sGoalPen         [][]*lpVar
	drive            [][]*lpVar
	pDemandPen       [][]*lpVar
	zPDemand         [][]*lpVar
	zSMaxReached     [][]*lpVar
	sMaxPen, sMinPen [][]*lpVar
	n, e             []*lpVar
	eImpLimExc       []*lpVar
	zImpLim          []*lpVar
	eExpLimExc       []*lpVar
	zExpLim          []*lpVar
	pMaxImpExc       *lpVar
	dExcess          []*lpVar
	homeImport       []*lpVar
	peak             map[string]*lpVar
	y                []*lpVar
	zC               [][]*lpVar
	zStart           [][]*lpVar
	zCD              [][]*lpVar
}

// Optimizer is the Optimizer class
type Optimizer struct {
	settings Settings
	// timeLimit of the whole optimization in seconds (the context's deadline at Solve), nil for none
	timeLimit  *float64
	strategy   OptimizationStrategy
	grid       GridConfig
	batteries  []BatteryConfig
	timeSeries TimeSeriesData
	etaC, etaD float64
	m          float64
	t          int

	problem              *lpProblem
	objectiveScale       float64
	commitmentObjective  *lpExpr
	costObjective        *lpExpr
	preferenceObjective  *lpExpr
	commitmentStageValue *float64
	preferenceStage      string
	costStageValue       *float64
	solvePath            string
	v                    variables

	sMinProfile [][]float64

	isGridDemandRateActive bool
	minImportPrice         float64
	maxImportPrice         float64
	maxExportPrice         float64

	prcReserve []float64
	prcGoal    []float64
	prcEExcImp float64
	prcEExcExp float64
	prcPPeak   float64
	peakSides  []string

	serveHomeFirst []int

	// probeThenSplitHook replaces probeThenSplit in tests
	probeThenSplitHook func(ctx context.Context, deadline *time.Time) error
}

// NewOptimizer is Optimizer.__init__. It fails where the Python constructor raises.
func NewOptimizer(strategy OptimizationStrategy, grid GridConfig, batteries []BatteryConfig, ts TimeSeriesData,
	etaC, etaD, m float64, settings Settings,
) (*Optimizer, error) {
	o := &Optimizer{
		settings:    settings,
		strategy:    strategy,
		grid:        grid,
		batteries:   batteries,
		timeSeries:  ts,
		etaC:        etaC,
		etaD:        etaD,
		m:           m,
		t:           len(ts.Gt),
		sMinProfile: make([][]float64, len(batteries)),
	}

	o.isGridDemandRateActive = grid.PMaxImp != nil && grid.PrcPExcImp != nil

	// np.min / np.max
	if len(ts.PN) == 0 {
		return nil, fmt.Errorf("zero-size array to reduction operation minimum which has no identity")
	}
	if len(ts.PE) == 0 {
		return nil, fmt.Errorf("zero-size array to reduction operation maximum which has no identity")
	}
	o.minImportPrice, o.maxImportPrice = npMin(ts.PN), npMax(ts.PN)
	o.maxExportPrice = npMax(ts.PE)

	o.setupPrices()

	o.prcPPeak = float64(pyMax(o.maxImportPrice, 0.1e-3) * 1e-3)
	o.peakSides = peakStrategySides[strategy.ChargingStrategy]

	return o, nil
}

func npMin(x []float64) float64 {
	r := x[0]
	for _, v := range x[1:] {
		if v < r || math.IsNaN(v) {
			r = v
		}
	}
	return r
}

func npMax(x []float64) float64 {
	r := x[0]
	for _, v := range x[1:] {
		if v > r || math.IsNaN(v) {
			r = v
		}
	}
	return r
}

// setupPrices is _setup_prices
func (o *Optimizer) setupPrices() {
	worstImport := pyMax(o.maxImportPrice, 0.1e-3)
	worstExport := pyMax(o.maxExportPrice, 0.1e-3)

	for _, b := range o.batteries {
		o.prcReserve = append(o.prcReserve, orDefault(b.PrcReserve, worstImport))
		o.prcGoal = append(o.prcGoal, orDefault(b.PrcGoal, worstImport))
	}

	o.prcEExcImp = orDefault(o.grid.PrcEExcImp, worstImport)
	o.prcEExcExp = orDefault(o.grid.PrcEExcExp, worstExport)
}

func orDefault(p *float64, def float64) float64 {
	if p != nil {
		return *p
	}
	return def
}

// setupReserve is _setup_reserve
func (o *Optimizer) setupReserve() {
	gt, ft := o.timeSeries.Gt, o.timeSeries.Ft

	reserve := make([]float64, o.t)
	run := 0.0
	for t := o.t - 1; t >= 0; t-- {
		if ft[t] >= gt[t] {
			run = 0.0
		} else {
			run += gt[t] - ft[t]
		}
		reserve[t] = run
	}

	for i := range o.batteries {
		bat := &o.batteries[i]
		profile := make([]float64, o.t)
		for t := range o.t {
			if bat.ReserveForLoad {
				profile[t] = pyMin(bat.SCapacity, pyMax(bat.SMin, reserve[t]/o.etaDOf(i)))
			} else if len(bat.CMaxT) > 0 && cMaxAt(bat, t) == 0 {
				profile[t] = 0.0
			} else {
				profile[t] = bat.SMin
			}
		}
		o.sMinProfile[i] = profile
	}
}

// CreateModel is create_model
func (o *Optimizer) CreateModel() {
	o.problem = newProblem("EV_Charging_Optimization", lpMaximize)

	o.setupReserve()
	o.setupVariables()
	o.setupTargetFunction()
	o.addEnergyBalanceConstraints()
	o.addBatteryConstraints()
}

func (o *Optimizer) dt(t int) float64 { return o.timeSeries.Dt[t] }

// energy is `power * dt[t] / 3600.`, the Wh a power in W moves over slot t
func (o *Optimizer) energy(power float64, t int) float64 {
	return float64(power*o.dt(t)) / 3600.
}

func perBattery(nb, nt int, f func(i, t int) *lpVar) [][]*lpVar {
	res := make([][]*lpVar, nb)
	for i := range nb {
		res[i] = make([]*lpVar, nt)
		for t := range nt {
			res[i][t] = f(i, t)
		}
	}
	return res
}

func perSlot(nt int, f func(t int) *lpVar) []*lpVar {
	res := make([]*lpVar, nt)
	for t := range nt {
		res[t] = f(t)
	}
	return res
}

// setupVariables is _setup_variables
func (o *Optimizer) setupVariables() {
	nb, nt := len(o.batteries), o.t
	v := &o.v
	zero := fptr(0)

	v.c = perBattery(nb, nt, func(i, t int) *lpVar {
		return newVar(fmt.Sprintf("c_%d_%d", i, t), zero, fptr(o.energy(cMaxAt(&o.batteries[i], t), t)))
	})
	v.d = perBattery(nb, nt, func(i, t int) *lpVar {
		return newVar(fmt.Sprintf("d_%d_%d", i, t), zero, fptr(o.energy(o.batteries[i].DMax, t)))
	})
	v.s = perBattery(nb, nt, func(i, t int) *lpVar {
		return newVar(fmt.Sprintf("s_%d_%d", i, t), zero, fptr(o.batteries[i].SCapacity))
	})

	v.sGoalPen = perBattery(nb, nt, func(i, t int) *lpVar {
		if bat := &o.batteries[i]; bat.SGoal != nil && bat.SGoal[t] > 0 {
			return newVar(fmt.Sprintf("s_goal_pen_%d_%d", i, t), zero, nil)
		}
		return nil
	})

	v.drive = make([][]*lpVar, nb)
	for i := range o.batteries {
		bat := &o.batteries[i]
		if bat.SDrainT != nil {
			v.drive[i] = perSlot(nt, func(t int) *lpVar {
				return newVar(fmt.Sprintf("drive_%d_%d", i, t), zero, fptr(o.energy(sDrainAt(bat, t), t)))
			})
		}
	}

	v.pDemandPen = perBattery(nb, nt, func(i, t int) *lpVar { return nil })
	v.zPDemand = perBattery(nb, nt, func(i, t int) *lpVar { return nil })
	v.zSMaxReached = perBattery(nb, nt, func(i, t int) *lpVar { return nil })
	for i := range o.batteries {
		if o.batteries[i].PDemand != nil {
			for t := range nt {
				v.pDemandPen[i][t] = newVar(fmt.Sprintf("p_demand_pen_%d_%d", i, t), zero, nil)
				v.zPDemand[i][t] = newBinary(fmt.Sprintf("z_p_demand_%d_%d", i, t))
				v.zSMaxReached[i][t] = newBinary(fmt.Sprintf("z_s_max_reached_%d_%d", i, t))
			}
		}
	}

	v.sMaxPen = perBattery(nb, nt, func(i, t int) *lpVar { return newVar(fmt.Sprintf("s_max_pen_%d_%d", i, t), zero, nil) })
	v.sMinPen = perBattery(nb, nt, func(i, t int) *lpVar { return newVar(fmt.Sprintf("s_min_pen_%d_%d", i, t), zero, nil) })

	v.n = perSlot(nt, func(t int) *lpVar { return newVar(fmt.Sprintf("n_%d", t), zero, nil) })
	v.e = perSlot(nt, func(t int) *lpVar { return newVar(fmt.Sprintf("e_%d", t), zero, nil) })

	if o.grid.PMaxImp != nil {
		v.eImpLimExc = perSlot(nt, func(t int) *lpVar { return newVar(fmt.Sprintf("p_imp_pen_%d", t), zero, nil) })
		v.zImpLim = perSlot(nt, func(t int) *lpVar { return newBinary(fmt.Sprintf("z_imp_lim_%d", t)) })
	}

	if o.grid.PMaxExp != nil {
		v.eExpLimExc = perSlot(nt, func(t int) *lpVar { return newVar(fmt.Sprintf("e_exp_lim_exc_%d", t), zero, nil) })
		v.zExpLim = perSlot(nt, func(t int) *lpVar { return newBinary(fmt.Sprintf("z_exp_lim_%d", t)) })
	}

	if o.isGridDemandRateActive {
		v.pMaxImpExc = newVar("p_max_imp_exc", zero, nil)
	}

	o.serveHomeFirst = nil
	for i := range o.batteries {
		if o.batteries[i].ServeHomeFirst {
			o.serveHomeFirst = append(o.serveHomeFirst, i)
		}
	}
	if len(o.serveHomeFirst) > 0 {
		v.dExcess = perSlot(nt, func(t int) *lpVar { return newVar(fmt.Sprintf("d_excess_%d", t), zero, nil) })
		v.homeImport = perSlot(nt, func(t int) *lpVar { return newVar(fmt.Sprintf("home_import_%d", t), zero, nil) })
	}

	v.peak = make(map[string]*lpVar)
	for _, side := range o.peakSides {
		v.peak[side] = newVar(fmt.Sprintf("p_%s_peak", side), zero, nil)
	}

	v.y = perSlot(nt, func(t int) *lpVar { return newBinary(fmt.Sprintf("y_%d", t)) })

	v.zC = make([][]*lpVar, nb)
	for i := range o.batteries {
		if o.batteries[i].CMin > 0 {
			v.zC[i] = perSlot(nt, func(t int) *lpVar { return newBinary(fmt.Sprintf("z_c_%d_%d", i, t)) })
		}
	}

	// py: z_start >= z_c[t] - (z_c[t - 1] if t > 0 else 0)
	v.zStart = make([][]*lpVar, nb)
	for i, zc := range v.zC {
		if zc == nil {
			continue
		}
		v.zStart[i] = perSlot(nt, func(t int) *lpVar { return newVar(fmt.Sprintf("z_start_%d_%d", i, t), zero, fptr(1)) })
		for t := range nt {
			// charging in the first slot continues a running session, it is no start
			if t == 0 && o.batteries[i].Charging {
				continue
			}
			rhs := affine(zc[t])
			if t > 0 {
				rhs.addVar(zc[t-1], -1)
			}
			o.problem.add(ge(affine(v.zStart[i][t]), rhs), "")
		}
	}

	v.zCD = perBattery(nb, nt, func(i, t int) *lpVar { return newBinary(fmt.Sprintf("z_cd_%d_%d", i, t)) })
}

// iadd is `acc += x` with acc possibly still Python's int 0 (nil)
func iadd(acc **lpExpr, x *lpExpr) {
	if *acc == nil {
		*acc = x.copy()
		return
	}
	(*acc).addExpr(x, 1)
}

// isub is `acc -= x` with acc possibly still Python's int 0 (nil)
func isub(acc **lpExpr, x *lpExpr) {
	if *acc == nil {
		*acc = x.neg()
		return
	}
	(*acc).addExpr(x, -1)
}

// plusAny is `a + b` where b may be Python's int 0 (nil)
func plusAny(a, b *lpExpr) *lpExpr {
	if b == nil {
		return a.copy()
	}
	return plus(a, b)
}

// value is pulp.value() of an expression that may still be Python's int 0 (nil)
func value(e *lpExpr) *float64 {
	if e == nil {
		return fptr(0)
	}
	return e.value()
}

func anyNonzero(e *lpExpr) bool { return e != nil && e.anyNonzero() }

// setupTargetFunction is _setup_target_function
func (o *Optimizer) setupTargetFunction() {
	v := &o.v
	T := o.t

	// L1 - commitment
	var commitment *lpExpr
	for i := range o.batteries {
		bat := &o.batteries[i]
		// py: commitment += - prc_reserve[i] * (s_max_pen[i][t] + s_min_pen[i][t])
		for t := range T {
			iadd(&commitment, affine(v.sMaxPen[i][t]).addVar(v.sMinPen[i][t], 1).mul(-o.prcReserve[i]))
		}

		if bat.SGoal != nil {
			for t := range T {
				if bat.SGoal[t] > 0 {
					iadd(&commitment, affine(v.sGoalPen[i][t]).mul(-o.prcGoal[i]))
				}
			}
		}

		// py: commitment += - prc_goal[i] * p_demand_pen[i][t] * (1 + (T - t) / T)
		if bat.PDemand != nil {
			for t := range T {
				w := 1 + float64(T-t)/float64(T)
				iadd(&commitment, affine(v.pDemandPen[i][t]).mul(-o.prcGoal[i]).mul(w))
			}
		}
	}

	// serve_home_first: the home self-consumes ahead of money, so energy the house
	// will need before the pack can refill is never sold, whatever the export price.
	// Weighted below the reserve penalty (scaled by eta_d, the house gets eta_d Wh
	// per stored Wh) so serving the house never justifies breaching s_min.
	if len(o.serveHomeFirst) > 0 {
		prcHome := math.Inf(1)
		for _, i := range o.serveHomeFirst {
			prcHome = min(prcHome, o.prcReserve[i])
		}
		prcHome *= 0.5
		for t := range T {
			iadd(&commitment, affine(v.homeImport[t]).mul(-prcHome))
		}
	}
	o.commitmentObjective = commitment

	// L2 - money
	var objective *lpExpr
	for t := range T {
		if o.grid.PMaxImp != nil {
			// py: objective -= (n[t] + e_imp_lim_exc[t]) * p_N[t]
			isub(&objective, affine(v.n[t]).addVar(v.eImpLimExc[t], 1).mul(o.timeSeries.PN[t]))
		} else {
			isub(&objective, affine(v.n[t]).mul(o.timeSeries.PN[t]))
		}
	}

	for t := range T {
		iadd(&objective, affine(v.e[t]).mul(o.timeSeries.PE[t]))
	}

	for i := range o.batteries {
		iadd(&objective, affine(v.s[i][T-1]).mul(o.terminalValue(i)))
	}

	for i := range o.batteries {
		if v.drive[i] != nil {
			for t := range T {
				iadd(&objective, affine(v.drive[i][t]).mul(o.prcGoal[i]/o.etaCOf(i)))
			}
		}
	}

	if o.isGridDemandRateActive {
		iadd(&objective, affine(v.pMaxImpExc).mul(-*o.grid.PrcPExcImp))
	}

	for t := range T {
		if o.grid.PMaxImp != nil && !o.isGridDemandRateActive {
			iadd(&objective, affine(v.eImpLimExc[t]).mul(-o.prcEExcImp))
		}

		// py: objective += - prc_e_exc_exp * (1.0 - t * 1e-5) * e_exp_lim_exc[t]
		if o.grid.PMaxExp != nil {
			f := float64(-o.prcEExcExp * float64(1.0-float64(float64(t)*1e-5)))
			iadd(&objective, affine(v.eExpLimExc[t]).mul(f))
		}
	}
	o.costObjective = objective

	// L3 - preferences
	var preference *lpExpr

	if o.strategy.ChargingStrategy == "charge_before_export" {
		for range o.batteries {
			for t := range T {
				// py: preference += - e[t] * min_import_price * 2e-5 * (T - t)
				iadd(&preference, affine(v.e[t]).neg().mul(o.minImportPrice).mul(2e-5).mul(float64(T-t)))
			}
		}
	}

	for _, side := range o.peakSides {
		iadd(&preference, affine(v.peak[side]).neg().mul(o.prcPPeak))
	}

	if o.strategy.DischargingStrategy == "discharge_before_import" {
		for range o.batteries {
			for t := range T {
				iadd(&preference, affine(v.n[t]).neg().mul(o.minImportPrice).mul(5e-6).mul(float64(T-t)))
			}
		}
	}

	if len(o.serveHomeFirst) > 0 {
		prcExcess := float64(o.minImportPrice * 1e-2)
		for t := range T {
			iadd(&preference, affine(v.dExcess[t]).neg().mul(prcExcess))
		}
	}

	// py: preference += - pulp.lpSum(starts) * prc_start
	prcStart := float64(pyMax(o.minImportPrice, 0.1e-3) * 10)
	for _, starts := range v.zStart {
		if starts == nil {
			continue
		}
		sum := newExpr()
		for _, s := range starts {
			sum.addVar(s, 1)
		}
		iadd(&preference, sum.neg().mul(prcStart))
	}

	for i := range o.batteries {
		cp := float64(o.batteries[i].CPriority)
		for t := range T {
			iadd(&preference, affine(v.c[i][t]).mul(o.minImportPrice).mul(5e-5).mul(float64(T-t)).mul(cp))
			iadd(&preference, affine(v.d[i][t]).mul(o.minImportPrice).mul(5e-5).mul(float64(T-t)).mul(cp))
		}
	}
	o.preferenceObjective = preference

	o.objectiveScale = objectiveScale(plusAny(objective, preference))
	o.problem.setObjective(plusAny(objective, preference).mul(o.objectiveScale))
}

// terminalValue is terminal_value()
func (o *Optimizer) terminalValue(i int) float64 {
	return o.batteries[i].PA
}

// addEnergyBalanceConstraints is _add_energy_balance_constraints
func (o *Optimizer) addEnergyBalanceConstraints() {
	v := &o.v
	ts := &o.timeSeries
	p := o.problem

	// Constraint (2): power balance
	for t := range o.t {
		var batteryNetDischarge *lpExpr
		for i := range o.batteries {
			// py: battery_net_discharge += (- c[i][t] + d[i][t])
			iadd(&batteryNetDischarge, affine(v.c[i][t]).neg().addVar(v.d[i][t], 1))
		}

		eGridImp := affine(v.n[t])
		if o.grid.PMaxImp != nil {
			eGridImp.addVar(v.eImpLimExc[t], 1)
		}

		eGridExp := affine(v.e[t])
		if o.grid.PMaxExp != nil {
			eGridExp.addVar(v.eExpLimExc[t], 1)
		}

		// py: battery_net_discharge + ft[t] + e_grid_imp == e_grid_exp + gt[t]
		lhs := newExpr()
		if batteryNetDischarge != nil {
			lhs = batteryNetDischarge.copy()
		}
		lhs.addConst(ts.Ft[t], 1)
		lhs.addExpr(eGridImp, 1)
		rhs := eGridExp.addConst(ts.Gt[t], 1)
		p.add(eq(lhs, rhs), "")
	}

	// Constraints (4)-(5): grid flow direction
	capDExp := 0.0
	for _, b := range o.batteries {
		if b.DischargeToGrid {
			capDExp += b.DMax
		}
	}
	for t := range o.t {
		dth := o.dt(t) / 3600.
		capCImp := 0.0
		for i := range o.batteries {
			if b := &o.batteries[i]; b.ChargeFromGrid {
				capCImp += cMaxAt(b, t)
			}
		}
		mExp, mImp := o.m, o.m
		if o.grid.PMaxImp == nil {
			mExp = ts.Ft[t] + float64(capDExp*dth)
		}
		if o.grid.PMaxExp == nil {
			mImp = ts.Gt[t] + float64(capCImp*dth)
		}
		p.add(le(affine(v.e[t]), affine(v.y[t]).mul(mExp)), "")
		p.add(le(affine(v.n[t]), rsub(1, affine(v.y[t])).mul(mImp)), "")
	}

	// grid import limit, the same rows with and without a demand rate
	if o.grid.PMaxImp != nil {
		for t := range o.t {
			lim := o.energy(*o.grid.PMaxImp, t)
			p.add(leNum(affine(v.n[t]), lim), "")
			p.add(le(rsub(lim, affine(v.n[t])), affine(v.zImpLim[t]).mul(lim)), "")
			p.add(le(affine(v.eImpLimExc[t]), rsub(1, affine(v.zImpLim[t])).mul(o.m)), "")
		}
	}

	// grid export limit
	if o.grid.PMaxExp != nil {
		for t := range o.t {
			lim := o.energy(*o.grid.PMaxExp, t)
			p.add(leNum(affine(v.e[t]), lim), "")
			p.add(le(rsub(lim, affine(v.e[t])), affine(v.zExpLim[t]).mul(lim)), "")
			p.add(le(affine(v.eExpLimExc[t]), rsub(1, affine(v.zExpLim[t])).mul(o.m)), "")
		}
	}

	// serve_home_first: d_excess >= discharge - deficit
	if len(o.serveHomeFirst) > 0 {
		for t := range o.t {
			deficit := pyMax(0.0, ts.Gt[t]-ts.Ft[t])
			discharge := newExpr()
			for _, i := range o.serveHomeFirst {
				discharge.addVar(v.d[i][t], 1)
			}
			p.add(ge(affine(v.dExcess[t]), discharge.copy().addConst(deficit, -1)), "")
		}

		// home_import >= import - charging of the other batteries (the cars): grid
		// energy the house (or the home pack) takes, which an outage would cut off
		home := make(map[int]bool, len(o.serveHomeFirst))
		for _, i := range o.serveHomeFirst {
			home[i] = true
		}
		for t := range o.t {
			homeImport := affine(v.n[t])
			for i := range o.batteries {
				if !home[i] {
					homeImport.addVar(v.c[i][t], -1)
				}
			}
			p.add(ge(affine(v.homeImport[t]), homeImport), "")
		}
	}

	for _, side := range o.peakSides {
		names := peakSideVariables[side]
		gridVar, limExc := v.n, v.eImpLimExc
		if names[0] == "e" {
			gridVar, limExc = v.e, v.eExpLimExc
		}
		pGrid := make([]*lpExpr, o.t)
		for t := range o.t {
			eGrid := affine(gridVar[t])
			if limExc != nil {
				eGrid.addVar(limExc[t], 1)
			}
			// py: e_grid * 3600 / dt[t]
			pGrid[t] = eGrid.mul(3600).div(o.dt(t))
		}
		for t := range o.t {
			p.add(le(pGrid[t], affine(v.peak[side])), "")
		}
	}

	// py: e_imp_lim_exc[t] <= p_max_imp_exc * dt[t] / 3600
	if o.isGridDemandRateActive {
		for t := range o.t {
			p.add(le(affine(v.eImpLimExc[t]), affine(v.pMaxImpExc).mul(o.dt(t)).div(3600)), "")
		}
	}
}

// addBatteryConstraints is _add_battery_constraints
func (o *Optimizer) addBatteryConstraints() {
	v := &o.v
	p := o.problem

	for i := range o.batteries {
		bat := &o.batteries[i]
		for t := range o.t {
			// py: s_max_pen >= s - s_max
			p.add(ge(affine(v.sMaxPen[i][t]), affine(v.s[i][t]).addConst(bat.SMax, -1)), "")
			// py: s_min_pen >= s_min_profile[i][t] - s
			p.add(ge(affine(v.sMinPen[i][t]), rsub(o.sMinProfile[i][t], affine(v.s[i][t]))), "")
		}
	}

	// Constraint (3): battery dynamics
	for i := range o.batteries {
		bat := &o.batteries[i]

		// `expr - drive_at(t)`
		minusDrive := func(e *lpExpr, t int) *lpExpr {
			e = e.copy()
			if v.drive[i] != nil {
				e.addVar(v.drive[i][t], -1)
			}
			return e
		}

		// py: s[0] == s_initial + eta_c * c[0] - (1 / eta_d) * d[0] - drive_at(0)
		if o.t > 0 {
			rhs := affine(v.c[i][0]).mul(o.etaCOf(i)).copy().addConst(bat.SInitial, 1)
			rhs = sub(rhs, affine(v.d[i][0]).mul(1/o.etaDOf(i)))
			p.add(eq(affine(v.s[i][0]), minusDrive(rhs, 0)), "")
		}

		// py: s[t] == s[t - 1] + eta_c * c[t] - (1 / eta_d) * d[t] - drive_at(t)
		for t := 1; t < o.t; t++ {
			rhs := plus(affine(v.s[i][t-1]), affine(v.c[i][t]).mul(o.etaCOf(i)))
			rhs = sub(rhs, affine(v.d[i][t]).mul(1/o.etaDOf(i)))
			p.add(eq(affine(v.s[i][t]), minusDrive(rhs, t)), "")
		}

		// Constraint (6): s + s_goal_pen >= s_goal
		if bat.SGoal != nil {
			for t := 1; t < o.t; t++ {
				if bat.SGoal[t] > 0 {
					p.add(geNum(affine(v.s[i][t]).addVar(v.sGoalPen[i][t], 1), bat.SGoal[t]), "")
				}
			}
		}

		if bat.PDemand != nil {
			for t := range o.t {
				if bat.PDemand[t] > 0 {
					pDemand := pyMin(o.energy(cMaxAt(bat, t), t), bat.PDemand[t])

					// py: (s_max - s) <= (1 - z_p_demand) * s_max + c_min * dt / 3600.
					p.add(le(rsub(bat.SMax, affine(v.s[i][t])),
						rsub(1, affine(v.zPDemand[i][t])).mul(bat.SMax).addConst(o.energy(bat.CMin, t), 1)), "")

					// py: (s_max - s) <= (1 - z_s_max_reached) * s_max
					p.add(le(rsub(bat.SMax, affine(v.s[i][t])), rsub(1, affine(v.zSMaxReached[i][t])).mul(bat.SMax)), "")

					// py: c + p_demand_pen >= (1 - z_s_max_reached) * p_demand
					p.add(ge(affine(v.c[i][t]).addVar(v.pDemandPen[i][t], 1), rsub(1, affine(v.zSMaxReached[i][t])).mul(pDemand)), "")
				} else {
					p.add(leNum(affine(v.zPDemand[i][t]), 0), "")
				}

				if bat.CMin > 0 {
					k := o.energy(bat.CMin, t)
					// py: c + c_min * dt / 3600. * z_p_demand >= c_min * dt / 3600. * z_c
					p.add(ge(affine(v.c[i][t]).addExpr(affine(v.zPDemand[i][t]).mul(k), 1), affine(v.zC[i][t]).mul(k)), "")
					p.add(le(affine(v.c[i][t]), affine(v.zC[i][t]).mul(o.energy(cMaxAt(bat, t), t))), "")
				}
			}
		} else if bat.CMin > 0 {
			// Constraint (7)
			for t := range o.t {
				p.add(ge(affine(v.c[i][t]), affine(v.zC[i][t]).mul(o.energy(bat.CMin, t))), "")
				p.add(le(affine(v.c[i][t]), affine(v.zC[i][t]).mul(o.energy(cMaxAt(bat, t), t))), "")
			}
		}

		if !bat.ChargeFromGrid {
			for t := range o.t {
				p.add(le(affine(v.c[i][t]), affine(v.y[t]).mul(o.energy(cMaxAt(bat, t), t))), "")
			}
		}

		if !bat.DischargeToGrid {
			for t := range o.t {
				p.add(le(affine(v.d[i][t]), rsub(1, affine(v.y[t])).mul(o.energy(bat.DMax, t))), "")
			}
		}

		for t := range o.t {
			p.add(le(affine(v.d[i][t]), affine(v.zCD[i][t]).mul(o.energy(bat.DMax, t))), "")
			p.add(le(affine(v.c[i][t]), rsub(1, affine(v.zCD[i][t])).mul(o.energy(cMaxAt(bat, t), t))), "")
		}
	}
}

// solveStage is problem.solve(self._solver(tmpdir, timeLimit=..., gapAbs=...))
func (o *Optimizer) solveStage(ctx context.Context, timeLimit, gapAbs *float64) error {
	return o.problem.solveWith(ctx, solverOptions{timeLimit: timeLimit, gapAbs: gapAbs})
}

type snapshot map[*lpVar]*float64

func (o *Optimizer) snapshot() snapshot {
	s := make(snapshot)
	for _, v := range o.problem.vars() {
		s[v] = v.value
	}
	return s
}

func (s snapshot) restore() {
	for v, val := range s {
		v.value = val
	}
}

func mustValue(e *lpExpr) float64 {
	v := value(e)
	if v == nil {
		// a TypeError in the Python, which answers 500
		panic("unsupported operand type(s): 'NoneType'")
	}
	return *v
}

// remaining is `deadline - time.monotonic()` in seconds
func remaining(deadline *time.Time) float64 {
	return time.Until(*deadline).Seconds()
}

// solvePreferences is _solve_preferences
func (o *Optimizer) solvePreferences(ctx context.Context, deadline *time.Time) error {
	if !anyNonzero(o.preferenceObjective) {
		o.preferenceStage = "none"
		return nil
	}

	var rem *float64
	if deadline != nil {
		r := remaining(deadline)
		if r <= 0 {
			o.preferenceStage = "no time"
			return nil
		}
		r = pyMin(r, float64(*o.timeLimit*preferenceTimeShare))
		rem = &r
	}

	solution := o.snapshot()
	cost := mustValue(o.costObjective)
	undecided := mustValue(o.preferenceObjective)

	budget := o.settings.PreferenceBudget + costBoundSlack
	if c, ok := o.problem.byName[costBound]; !ok {
		o.problem.add(geNum(o.costObjective, cost-budget), costBound)
	} else {
		c.constant = -(cost - budget)
	}

	o.problem.setObjective(o.preferenceObjective.mul(objectiveScale(o.preferenceObjective)))
	if err := o.solveStage(ctx, rem, nil); err != nil {
		return err
	}

	o.preferenceStage = lpStatus[o.problem.status]
	integral := o.problem.solStatus == lpSolutionOptimal || o.problem.solStatus == lpSolutionIntegerFeasible
	improved := integral &&
		mustValue(o.preferenceObjective) > undecided &&
		mustValue(o.costObjective) >= cost-budget-costBoundTolerance
	if !improved {
		o.preferenceStage += ", kept the first stage"
		solution.restore()
	}
	o.problem.status = lpStatusOptimal
	return nil
}

// solveCommitments is _solve_commitments
func (o *Optimizer) solveCommitments(ctx context.Context, deadline *time.Time) error {
	if !anyNonzero(o.commitmentObjective) {
		o.commitmentStageValue = fptr(0.0)
		return nil
	}

	var rem *float64
	if deadline != nil {
		rem = fptr(pyMax(remaining(deadline), 0.1))
	}
	scale := objectiveScale(o.commitmentObjective)
	o.problem.setObjective(o.commitmentObjective.mul(scale))
	if err := o.solveStage(ctx, rem, nil); err != nil {
		return err
	}

	o.commitmentStageValue = fptr(mustValue(o.commitmentObjective))

	floor := *o.commitmentStageValue - commitmentBoundSlack
	if c, ok := o.problem.byName[commitmentBound]; !ok {
		o.problem.add(geNum(o.commitmentObjective, floor), commitmentBound)
	} else {
		c.constant = -floor
	}
	return nil
}

// probeThenSplit is _probe_then_split
func (o *Optimizer) probeThenSplit(ctx context.Context, deadline *time.Time) error {
	o.problem.setObjective(plusAny(o.costObjective, o.preferenceObjective).mul(o.objectiveScale))

	probe := o.settings.ProbeSeconds
	if probe == nil && o.timeLimit != nil {
		probe = fptr(float64(*o.timeLimit * probeShare))
	}
	probing := probe == nil || *probe != 0
	if probing {
		if err := o.solveStage(ctx, probe, nil); err != nil {
			return err
		}
	}

	if probing && o.problem.solStatus == lpSolutionOptimal {
		o.solvePath = "joint"
		o.costStageValue = fptr(mustValue(o.costObjective))
		o.preferenceStage = "not needed"
		return nil
	}

	o.solvePath = "split"
	var fallback snapshot
	if o.problem.solStatus == lpSolutionOptimal || o.problem.solStatus == lpSolutionIntegerFeasible {
		fallback = o.snapshot()
	}

	gapAbs := o.settings.GapAbs
	scale := o.objectiveScale
	o.problem.setObjective(o.costObjective.mul(scale))
	var rem *float64
	if deadline != nil {
		rem = fptr(pyMax(remaining(deadline), 0.1))
	}
	var gap *float64
	if gapAbs != nil {
		gap = fptr(float64(*gapAbs * scale))
	}
	if err := o.solveStage(ctx, rem, gap); err != nil {
		return err
	}

	if lpStatus[o.problem.status] == "Optimal" {
		o.costStageValue = fptr(mustValue(o.costObjective))
		return o.solvePreferences(ctx, deadline)
	} else if len(fallback) > 0 {
		o.solvePath = "split, kept the probe"
		fallback.restore()
		o.problem.status = lpStatusOptimal
	}
	return nil
}

// isIntegral is _is_integral
func (o *Optimizer) isIntegral() bool {
	for _, v := range o.problem.vars() {
		if v.integer && v.value != nil && math.Abs(*v.value-math.RoundToEven(*v.value)) > integralityTolerance {
			return false
		}
	}
	return true
}

// LimitViolations is the limit_violations object of the response
type LimitViolations struct {
	GridImportLimitExceeded bool `json:"grid_import_limit_exceeded"`
	GridExportLimitHit      bool `json:"grid_export_limit_hit"`
}

// BatteryResult is an element of the batteries list of the response. A nil element is None.
type BatteryResult struct {
	ChargingPower    []*float64 `json:"charging_power"`
	DischargingPower []*float64 `json:"discharging_power"`
	StateOfCharge    []*float64 `json:"state_of_charge"`
}

// Result is the dict solve() returns, in full float64 precision, field for field
type Result struct {
	Status              string          `json:"status"`
	ObjectiveValue      *float64        `json:"objective_value"`
	LimitViolations     LimitViolations `json:"limit_violations"`
	Batteries           []BatteryResult `json:"batteries"`
	GridImport          []*float64      `json:"grid_import"`
	GridExport          []*float64      `json:"grid_export"`
	FlowDirection       []int           `json:"flow_direction"`
	GridImportOvershoot []*float64      `json:"grid_import_overshoot"`
	GridExportOvershoot []*float64      `json:"grid_export_overshoot"`
}

func values(vs []*lpVar) []*float64 {
	res := make([]*float64, len(vs))
	for t, v := range vs {
		res[t] = v.value
	}
	return res
}

// anyPositive is np.max(values) > 0
func anyPositive(vs []*float64) bool {
	m := math.Inf(-1)
	for _, v := range vs {
		if v == nil {
			panic("'>=' not supported between instances of 'NoneType' and 'float'")
		}
		if *v > m || math.IsNaN(*v) {
			m = *v
		}
	}
	return m > 0
}

// Solve is solve()
func (o *Optimizer) Solve(ctx context.Context) (res Result, err error) {
	if o.problem == nil {
		o.CreateModel()
	}

	// The service's OPTIMIZER_TIME_LIMIT is the context's deadline here: the whole
	// optimization shares it, and each stage gets its share as Python passes -sec.
	var deadline *time.Time
	o.timeLimit = nil
	if d, ok := ctx.Deadline(); ok {
		limit := time.Until(d).Seconds()
		o.timeLimit = &limit
		deadline = &d
	}

	if err := o.solveCommitments(ctx, deadline); err != nil {
		return res, err
	}
	probeThenSplit := o.probeThenSplit
	if o.probeThenSplitHook != nil {
		probeThenSplit = o.probeThenSplitHook
	}
	if err := probeThenSplit(ctx, deadline); err != nil {
		return res, err
	}

	o.problem.setObjective(plusAny(o.costObjective, o.preferenceObjective).mul(o.objectiveScale))

	status := lpStatus[o.problem.status]
	if status == "Optimal" && o.problem.solStatus != lpSolutionOptimal {
		status = "Feasible"
	}

	if (status == "Optimal" || status == "Feasible") && !o.isIntegral() {
		status = "Not Solved"
	}

	// py: everything up to the result dict is evaluated whatever the status
	v := &o.v
	eGridImport := values(v.n)
	eGridExport := values(v.e)
	if o.isGridDemandRateActive {
		for t := range o.t {
			if eGridImport[t] == nil || v.eImpLimExc[t].value == nil {
				panic("unsupported operand type(s) for +=: 'NoneType'")
			}
			eGridImport[t] = fptr(*eGridImport[t] + *v.eImpLimExc[t].value)
		}
	}

	var violations LimitViolations
	eGridImpOvershoot, eGridExpOvershoot := []*float64{}, []*float64{}
	if o.grid.PMaxImp != nil {
		eGridImpOvershoot = values(v.eImpLimExc)
		violations.GridImportLimitExceeded = anyPositive(eGridImpOvershoot)
	}
	if o.grid.PMaxExp != nil {
		eGridExpOvershoot = values(v.eExpLimExc)
		violations.GridExportLimitHit = anyPositive(eGridExpOvershoot)
	}

	if status != "Optimal" && status != "Feasible" {
		return Result{
			Status:              status,
			Batteries:           []BatteryResult{},
			GridImport:          []*float64{},
			GridExport:          []*float64{},
			FlowDirection:       []int{},
			GridImportOvershoot: []*float64{},
			GridExportOvershoot: []*float64{},
		}, nil
	}

	res = Result{
		Status:              status,
		LimitViolations:     violations,
		Batteries:           []BatteryResult{},
		GridImport:          eGridImport,
		GridExport:          eGridExport,
		GridImportOvershoot: eGridImpOvershoot,
		GridExportOvershoot: eGridExpOvershoot,
	}

	res.ObjectiveValue = fptr(o.cleanObjectiveValue())

	for i := range o.batteries {
		res.Batteries = append(res.Batteries, BatteryResult{
			ChargingPower:    values(v.c[i]),
			DischargingPower: values(v.d[i]),
			StateOfCharge:    values(v.s[i]),
		})
	}

	res.FlowDirection = make([]int, o.t)
	for t, y := range v.y {
		if y.value != nil {
			res.FlowDirection[t] = int(*y.value)
		}
	}

	return res, nil
}

func varValue(v *lpVar) float64 {
	if v.value == nil {
		panic("unsupported operand type(s): 'NoneType'")
	}
	return *v.value
}

// cleanObjectiveValue is get_clean_objective_value
func (o *Optimizer) cleanObjectiveValue() float64 {
	v := &o.v
	ts := &o.timeSeries
	clean := 0.0

	for t := range o.t {
		if o.grid.PMaxImp != nil {
			clean -= float64((varValue(v.n[t]) + varValue(v.eImpLimExc[t])) * ts.PN[t])
		} else {
			clean -= float64(varValue(v.n[t]) * ts.PN[t])
		}
	}

	for t := range o.t {
		clean += float64(varValue(v.e[t]) * ts.PE[t])
	}

	for i := range o.batteries {
		bat := &o.batteries[i]
		clean += float64((varValue(v.s[i][o.t-1]) - bat.SInitial) * bat.PA)

		if v.drive[i] != nil {
			sum := 0.0
			for _, d := range v.drive[i] {
				sum += varValue(d)
			}
			clean += float64(sum * bat.PA)
		}
	}

	if o.isGridDemandRateActive {
		clean += float64(-*o.grid.PrcPExcImp * varValue(v.pMaxImpExc))
	}

	return clean
}
