package optimizer

// This file holds the subset of PuLP 3.3.2 (pulp/pulp.py, pulp/mps_lp.py, pulp/apis/coin_api.py)
// the optimizer uses, reproduced closely enough that the model CBC sees is byte for byte the MPS
// file PuLP writes, and that the solution is read back the way PuLP reads CBC's solution file.
//
// Two things make this more than a convenience layer:
//
//   - PuLP's LpAffineExpression is an insertion ordered dict. The order of its terms decides the
//     order of the coefficients in the MPS file and the order in which pulp.value() sums them up,
//     and the coefficients themselves are the result of a particular sequence of float operations.
//     The expression type below keeps both, and the model code in optimizer.go performs the same
//     operations in the same order as the Python does.
//   - Go may fuse a multiply and an add into one FMA instruction, Python never does. Every product
//     that feeds an addition is therefore wrapped in an explicit float64() conversion, which the Go
//     spec defines as a rounding point that prevents fusion. Keep it that way when editing.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blampe/cbc"
)

// constraint senses, pulp.const.LpConstraintLE/EQ/GE
const (
	lpConstraintLE = -1
	lpConstraintEQ = 0
	lpConstraintGE = 1
)

// problem senses, pulp.const.LpMinimize/LpMaximize
const (
	lpMinimize = 1
	lpMaximize = -1
)

// problem status, pulp.const.LpStatus*
const (
	lpStatusNotSolved  = 0
	lpStatusOptimal    = 1
	lpStatusInfeasible = -1
	lpStatusUnbounded  = -2
	lpStatusUndefined  = -3
)

// lpStatus is pulp.LpStatus
var lpStatus = map[int]string{
	lpStatusNotSolved:  "Not Solved",
	lpStatusOptimal:    "Optimal",
	lpStatusInfeasible: "Infeasible",
	lpStatusUnbounded:  "Unbounded",
	lpStatusUndefined:  "Undefined",
}

// solution status, pulp.const.LpSolution*
const (
	lpSolutionNoSolutionFound = 0
	lpSolutionOptimal         = 1
	lpSolutionIntegerFeasible = 2
	lpSolutionInfeasible      = -1
	lpSolutionUnbounded       = -2
)

// lpVar is pulp.LpVariable. A nil bound is Python's None, a nil value is varValue None.
type lpVar struct {
	name    string
	low, up *float64
	integer bool
	value   *float64
}

func fptr(f float64) *float64 { return &f }

// newVar is pulp.LpVariable(name, lowBound=low, upBound=up)
func newVar(name string, low, up *float64) *lpVar {
	return &lpVar{name: name, low: low, up: up}
}

// newBinary is pulp.LpVariable(name, cat='Binary'): an integer bounded to [0, 1]
func newBinary(name string) *lpVar {
	return &lpVar{name: name, low: fptr(0), up: fptr(1), integer: true}
}

type lpTerm struct {
	v *lpVar
	c float64
}

// lpExpr is pulp.LpAffineExpression: an insertion ordered map of variables to coefficients plus
// a constant.
type lpExpr struct {
	terms    []lpTerm
	index    map[*lpVar]int
	constant float64
}

// newExpr is LpAffineExpression(), an empty expression
func newExpr() *lpExpr {
	return &lpExpr{index: make(map[*lpVar]int)}
}

// affine is LpAffineExpression(v), the expression 1*v
func affine(v *lpVar) *lpExpr {
	e := newExpr()
	e.set(v, 1)
	return e
}

// constExpr is LpAffineExpression(c)
func constExpr(c float64) *lpExpr {
	e := newExpr()
	e.constant = c
	return e
}

func (e *lpExpr) set(v *lpVar, c float64) {
	if i, ok := e.index[v]; ok {
		e.terms[i].c = c
		return
	}
	e.index[v] = len(e.terms)
	e.terms = append(e.terms, lpTerm{v, c})
}

func (e *lpExpr) has(v *lpVar) bool {
	_, ok := e.index[v]
	return ok
}

func (e *lpExpr) get(v *lpVar) float64 {
	return e.terms[e.index[v]].c
}

func (e *lpExpr) len() int { return len(e.terms) }

// addTerm is LpAffineExpression.addterm
func (e *lpExpr) addTerm(v *lpVar, c float64) {
	if i, ok := e.index[v]; ok {
		e.terms[i].c += c
		return
	}
	e.set(v, c)
}

// copy is LpAffineExpression.copy
func (e *lpExpr) copy() *lpExpr {
	c := &lpExpr{
		terms:    slices.Clone(e.terms),
		index:    make(map[*lpVar]int, len(e.terms)),
		constant: e.constant,
	}
	for i, t := range c.terms {
		c.index[t.v] = i
	}
	return c
}

// addExpr is LpAffineExpression.addInPlace(other, sign) for an expression
func (e *lpExpr) addExpr(o *lpExpr, sign float64) *lpExpr {
	e.constant += float64(o.constant * sign)
	for _, t := range o.terms {
		e.addTerm(t.v, float64(t.c*sign))
	}
	return e
}

// addVar is LpAffineExpression.addInPlace(other, sign) for a variable
func (e *lpExpr) addVar(v *lpVar, sign float64) *lpExpr {
	e.addTerm(v, sign)
	return e
}

// addConst is LpAffineExpression.addInPlace(other, sign) for a number. Python skips an int 0
// entirely, which only differs from adding 0.0 in the sign of a zero constant; that never reaches
// the MPS file (a zero RHS is written as 0) nor a value that is compared or reported.
func (e *lpExpr) addConst(c float64, sign float64) *lpExpr {
	e.constant += float64(c * sign)
	return e
}

// mul is LpAffineExpression.__mul__ with a number: an empty expression for 0, which drops the
// variables from the expression altogether.
func (e *lpExpr) mul(c float64) *lpExpr {
	r := newExpr()
	if c != 0 {
		r.constant = e.constant * c
		r.terms = make([]lpTerm, len(e.terms))
		for i, t := range e.terms {
			r.terms[i] = lpTerm{t.v, c * t.c}
			r.index[t.v] = i
		}
	}
	return r
}

// div is LpAffineExpression.__truediv__ with a number
func (e *lpExpr) div(c float64) *lpExpr {
	r := newExpr()
	r.constant = e.constant / c
	r.terms = make([]lpTerm, len(e.terms))
	for i, t := range e.terms {
		r.terms[i] = lpTerm{t.v, t.c / c}
		r.index[t.v] = i
	}
	return r
}

// neg is LpAffineExpression.__neg__
func (e *lpExpr) neg() *lpExpr {
	r := newExpr()
	r.constant = -e.constant
	r.terms = make([]lpTerm, len(e.terms))
	for i, t := range e.terms {
		r.terms[i] = lpTerm{t.v, -t.c}
		r.index[t.v] = i
	}
	return r
}

// plus is a + b for two expressions, sub is a - b
func plus(a, b *lpExpr) *lpExpr { return a.copy().addExpr(b, 1) }
func sub(a, b *lpExpr) *lpExpr  { return a.copy().addExpr(b, -1) }

// rsub is `c - e` for a number c, LpAffineExpression.__rsub__
func rsub(c float64, e *lpExpr) *lpExpr { return e.neg().addConst(c, 1) }

// anyNonzero is any(LpAffineExpression(e).values())
func (e *lpExpr) anyNonzero() bool {
	for _, t := range e.terms {
		if t.c != 0 {
			return true
		}
	}
	return false
}

// truthy is LpAffineExpression.__bool__
func (e *lpExpr) truthy() bool {
	return e.constant != 0 || len(e.terms) > 0
}

// value is LpAffineExpression.value: nil when any variable has no value
func (e *lpExpr) value() *float64 {
	s := e.constant
	for _, t := range e.terms {
		if t.v.value == nil {
			return nil
		}
		s += float64(*t.v.value * t.c)
	}
	return &s
}

// lpConstraint is pulp.LpConstraint
type lpConstraint struct {
	expr     *lpExpr
	sense    int
	constant float64
	name     string
}

// newConstraint is LpConstraint(e, sense): the expression is shared, not copied
func newConstraint(e *lpExpr, sense int) *lpConstraint {
	return &lpConstraint{expr: e, sense: sense, constant: e.constant}
}

// newConstraintRHS is LpConstraint(e, sense, rhs=rhs)
func newConstraintRHS(e *lpExpr, sense int, rhs float64) *lpConstraint {
	c := newConstraint(e, sense)
	c.constant -= rhs
	return c
}

// Python comparison operators. With a number on the right side PuLP keeps the left expression
// and moves the number to the constant, with an expression it subtracts the two.
func le(a, b *lpExpr) *lpConstraint            { return newConstraint(sub(a, b), lpConstraintLE) }
func ge(a, b *lpExpr) *lpConstraint            { return newConstraint(sub(a, b), lpConstraintGE) }
func eq(a, b *lpExpr) *lpConstraint            { return newConstraint(sub(a, b), lpConstraintEQ) }
func leNum(a *lpExpr, b float64) *lpConstraint { return newConstraintRHS(a, lpConstraintLE, b) }
func geNum(a *lpExpr, b float64) *lpConstraint { return newConstraintRHS(a, lpConstraintGE, b) }

// lpProblem is pulp.LpProblem
type lpProblem struct {
	name        string
	sense       int
	objective   *lpExpr
	constraints []*lpConstraint
	byName      map[string]*lpConstraint
	variables   []*lpVar
	registered  map[*lpVar]bool
	status      int
	solStatus   int
	statusText  string // the solver's status line of the last solve
	lastUnused  int
	dummy       *lpVar

	// solve runs CBC on the problem, replaced in tests
	solve func(ctx context.Context, p *lpProblem, opts solverOptions) error
}

func newProblem(name string, sense int) *lpProblem {
	return &lpProblem{
		name:       name,
		sense:      sense,
		byName:     make(map[string]*lpConstraint),
		registered: make(map[*lpVar]bool),
		status:     lpStatusNotSolved,
		solStatus:  lpSolutionNoSolutionFound,
		solve:      solveFunc,
	}
}

// add is LpProblem.__iadd__ / addConstraint
func (p *lpProblem) add(c *lpConstraint, name string) {
	if name != "" {
		c.name = name
	}
	if c.name == "" {
		c.name = p.unusedConstraintName()
	}
	if _, ok := p.byName[c.name]; ok {
		panic("overlapping constraint names: " + c.name)
	}
	p.constraints = append(p.constraints, c)
	p.byName[c.name] = c
	p.register(c.expr)
}

func (p *lpProblem) unusedConstraintName() string {
	p.lastUnused++
	for {
		s := fmt.Sprintf("_C%d", p.lastUnused)
		if _, ok := p.byName[s]; !ok {
			return s
		}
		p.lastUnused++
	}
}

func (p *lpProblem) register(e *lpExpr) {
	for _, t := range e.terms {
		if !p.registered[t.v] {
			p.registered[t.v] = true
			p.variables = append(p.variables, t.v)
		}
	}
}

// setObjective is LpProblem.setObjective
func (p *lpProblem) setObjective(e *lpExpr) {
	p.objective = e
}

// vars is LpProblem.variables(): everything the objective and the constraints reference, plus
// what was registered before, sorted by name.
func (p *lpProblem) vars() []*lpVar {
	if p.objective != nil && p.objective.truthy() {
		p.register(p.objective)
	}
	for _, c := range p.constraints {
		p.register(c.expr)
	}
	slices.SortStableFunc(p.variables, func(a, b *lpVar) int { return strings.Compare(a.name, b.name) })
	return p.variables
}

// variablesDict is LpProblem.variablesDict
func (p *lpProblem) variablesDict() map[string]*lpVar {
	res := make(map[string]*lpVar)
	if p.objective != nil && p.objective.truthy() {
		for _, t := range p.objective.terms {
			res[t.v.name] = t.v
		}
	}
	for _, c := range p.constraints {
		for _, t := range c.expr.terms {
			res[t.v.name] = t.v
		}
	}
	return res
}

// fixObjective is LpProblem.fixObjective: an objective without variables gets a dummy fixed at 0
func (p *lpProblem) fixObjective() (wasNone bool, dummy *lpVar) {
	if p.objective == nil {
		p.objective = constExpr(0)
		wasNone = true
	}
	if p.objective.len() == 0 {
		if p.dummy == nil {
			p.dummy = newVar("__dummy", fptr(0), fptr(0))
		}
		dummy = p.dummy
		p.objective.addVar(dummy, 1)
	}
	return wasNone, dummy
}

// restoreObjective is LpProblem.restoreObjective
func (p *lpProblem) restoreObjective(wasNone bool, dummy *lpVar) {
	if wasNone {
		p.objective = nil
	} else if dummy != nil {
		p.objective.addVar(dummy, -1)
	}
}

// e12 is Python's "% .12e"
func e12(f float64) string {
	return fmt.Sprintf("% .12e", f)
}

// writeMPS is pulp.mps_lp.writeMPS(lp, filename, rename=1): it returns the variables in column
// order and writes the file PuLP hands to CBC.
func (p *lpProblem) writeMPS(b *bytes.Buffer) []*lpVar {
	wasNone, dummy := p.fixObjective()
	defer p.restoreObjective(wasNone, dummy)

	// normalisedNames
	constrNames := make(map[*lpConstraint]string, len(p.constraints))
	for i, c := range p.constraints {
		constrNames[c] = fmt.Sprintf("C%07d", i)
	}
	vs := p.vars()
	varNames := make(map[*lpVar]string, len(vs))
	for i, v := range vs {
		varNames[v] = fmt.Sprintf("X%07d", i)
	}
	const objName = "OBJ"

	sense := "Minimize"
	if p.sense == lpMaximize {
		sense = "Maximize"
	}
	fmt.Fprintf(b, "*SENSE:%s\n", sense)
	b.WriteString("NAME          MODEL\n")
	b.WriteString("ROWS\n")
	fmt.Fprintf(b, " N  %s\n", objName)
	for _, c := range p.constraints {
		typ := map[int]string{lpConstraintLE: "L", lpConstraintEQ: "E", lpConstraintGE: "G"}[c.sense]
		fmt.Fprintf(b, " %s  %s\n", typ, constrNames[c])
	}

	// coefs[variable][constraint], in constraint order
	type coef struct {
		row string
		c   float64
	}
	coefs := make(map[*lpVar][]coef, len(vs))
	for _, c := range p.constraints {
		k := constrNames[c]
		for _, t := range c.expr.terms {
			coefs[t.v] = append(coefs[t.v], coef{k, t.c})
		}
	}

	b.WriteString("COLUMNS\n")
	for _, v := range vs {
		name := varNames[v]
		if v.integer {
			b.WriteString("    MARK      'MARKER'                 'INTORG'\n")
		}
		for _, c := range coefs[v] {
			fmt.Fprintf(b, "    %-8s  %-8s  %s\n", name, c.row, e12(c.c))
		}
		if p.objective.has(v) {
			fmt.Fprintf(b, "    %-8s  %-8s  %s\n", name, objName, e12(p.objective.get(v)))
		}
		if v.integer {
			b.WriteString("    MARK      'MARKER'                 'INTEND'\n")
		}
	}

	b.WriteString("RHS\n")
	for _, c := range p.constraints {
		rhs := 0.0
		if c.constant != 0 {
			rhs = -c.constant
		}
		fmt.Fprintf(b, "    RHS       %-8s  %s\n", constrNames[c], e12(rhs))
	}

	b.WriteString("BOUNDS\n")
	for _, v := range vs {
		writeMPSBoundLines(b, varNames[v], v)
	}
	b.WriteString("ENDATA\n")

	return vs
}

// writeMPSBoundLines is pulp.mps_lp.writeMPSBoundLines with mip=1
func writeMPSBoundLines(b *bytes.Buffer, name string, v *lpVar) {
	if v.low != nil && v.up != nil && *v.low == *v.up {
		fmt.Fprintf(b, " FX BND       %-8s  %s\n", name, e12(*v.low))
		return
	}
	if v.low != nil && *v.low == 0 && v.up != nil && *v.up == 1 && v.integer {
		fmt.Fprintf(b, " BV BND       %-8s\n", name)
		return
	}
	if v.low != nil {
		if *v.low != 0 || (v.integer && v.up == nil) {
			fmt.Fprintf(b, " LO BND       %-8s  %s\n", name, e12(*v.low))
		}
	} else if v.up != nil {
		fmt.Fprintf(b, " MI BND       %-8s\n", name)
	} else {
		fmt.Fprintf(b, " FR BND       %-8s\n", name)
	}
	if v.up != nil {
		fmt.Fprintf(b, " UP BND       %-8s  %s\n", name, e12(*v.up))
	}
}

// solverOptions are the COIN_CMD options the optimizer passes. threads is never set (the service
// default, and cbc-go has no threaded search), msg=0, timeMode elapsed.
type solverOptions struct {
	// timeLimit in seconds, nil for none. PuLP passes it as -sec, here it is a context deadline.
	timeLimit *float64
	// gapAbs, nil for none. PuLP passes it as -allow.
	gapAbs *float64
}

// errSolver is PulpSolverError
var errSolver = errors.New("pulp: error while executing cbc")

// solveProblem is LpProblem.solve(COIN_CMD(...))
func (p *lpProblem) solveWith(ctx context.Context, opts solverOptions) error {
	wasNone, dummy := p.fixObjective()
	defer p.restoreObjective(wasNone, dummy)
	return p.solve(ctx, p, opts)
}

// solveFunc is the solver new problems use, replaced in tests
var solveFunc = solveCBC

// mpsHook, when set, receives every MPS file handed to CBC. Tests use it to compare the model
// against the one PuLP writes.
var mpsHook func(mps []byte)

// solveCBC is COIN_CMD.solve_CBC: write the MPS file, solve it, read the solution file back.
func solveCBC(ctx context.Context, p *lpProblem, opts solverOptions) error {
	var b bytes.Buffer
	vs := p.writeMPS(&b)
	if mpsHook != nil {
		mpsHook(b.Bytes())
	}

	model, err := cbc.ReadMPSFrom(&b, "pulp.mps")
	if err != nil {
		return fmt.Errorf("%w: %v", errSolver, err)
	}
	if len(model.Variables) != len(vs) {
		return fmt.Errorf("%w: %d columns read, %d written", errSolver, len(model.Variables), len(vs))
	}
	// -max
	if p.sense == lpMaximize {
		model.Sense = cbc.Maximize
	}

	var copts []cbc.Option
	if opts.gapAbs != nil {
		copts = append(copts, cbc.WithAbsoluteGap(*opts.gapAbs))
	}
	if opts.timeLimit != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*opts.timeLimit*float64(time.Second)))
		defer cancel()
	}

	res, err := cbc.Solve(ctx, model, copts...)
	if err != nil {
		return fmt.Errorf("%w: %v", errSolver, err)
	}

	status, solStatus := getStatus(res.StatusText + " - objective value " + fmt.Sprintf("%.8f", res.Objective))

	// readsol_MPS + assignVarsVals: every column of the solution file, as CBC printed it
	values := make(map[string]float64, len(vs))
	for j, v := range vs {
		values[v.name] = solutionFileValue(res.Primal[j], model.Variables[j].Integer)
	}
	dict := p.variablesDict()
	for _, v := range vs {
		if v.name == "__dummy" {
			continue
		}
		dv, ok := dict[v.name]
		if !ok {
			// a KeyError in assignVarsVals
			return fmt.Errorf("%q", v.name)
		}
		val := values[v.name]
		dv.value = &val
	}

	p.status, p.solStatus = status, solStatus
	p.statusText = res.StatusText
	return nil
}

// solutionFileValue is a column value the way CbcSolver::writeSolution prints it with
// -printingOptions all ("%15.8g", integers within 1e-8 as integers) and PuLP parses it back.
func solutionFileValue(x float64, integer bool) float64 {
	nearest := math.Floor(x + 0.5)
	if !integer || math.Abs(x-nearest) > 1.0e-8 {
		f, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'g', 8, 64), 64)
		return f
	}
	return float64(int64(nearest))
}

// getStatus is COIN_CMD.get_status on the first line of the solution file
func getStatus(line string) (int, int) {
	cbcStatus := map[string]int{
		"Optimal":    lpStatusOptimal,
		"Infeasible": lpStatusInfeasible,
		"Integer":    lpStatusInfeasible,
		"Unbounded":  lpStatusUnbounded,
		"Stopped":    lpStatusNotSolved,
	}
	cbcSolStatus := map[string]int{
		"Optimal":    lpSolutionOptimal,
		"Infeasible": lpSolutionInfeasible,
		"Unbounded":  lpSolutionUnbounded,
		"Stopped":    lpSolutionNoSolutionFound,
	}

	statusstrs := strings.Fields(line)
	status, ok := cbcStatus[statusstrs[0]]
	if !ok {
		status = lpStatusUndefined
	}
	solStatus, ok := cbcSolStatus[statusstrs[0]]
	if !ok {
		solStatus = lpSolutionNoSolutionFound
	}
	if status == lpStatusNotSolved && len(statusstrs) >= 5 && statusstrs[4] == "objective" {
		status = lpStatusOptimal
		solStatus = lpSolutionIntegerFeasible
	}
	return status, solStatus
}
