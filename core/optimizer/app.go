package optimizer

// Port of src/optimizer/app.py: request validation and parsing, the solve, and the response the
// service sends. Optimize takes the request the way evcc used to send it over HTTP and returns
// what the client used to decode from the response, so the service and this package can be
// swapped for each other.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	client "github.com/evcc-io/optimizer/client"
)

// Error is a request the service would have answered with an error status: 400 for a request
// that fails validation or parsing, 500 for a failed optimization.
type Error struct {
	Code    int
	Message string
	Details map[string]string
}

// Error formats like evcc's apiError did for the service's error responses
func (e *Error) Error() string {
	if len(e.Details) > 0 {
		var details []string
		for field, msg := range e.Details {
			details = append(details, fmt.Sprintf("%s: %s", field, msg))
		}
		slices.Sort(details)
		return fmt.Sprintf("%s (%s)", e.Message, strings.Join(details, ", "))
	}
	return e.Message
}

// Option configures a solve
type Option func(*Settings)

// WithTimeLimit is the service's OPTIMIZER_TIME_LIMIT: the wall clock all solve stages share. Each
// stage gets its share as a context deadline, as the Python passes it as CBC's -sec.
func WithTimeLimit(d time.Duration) Option {
	return func(s *Settings) { s.TimeLimit = fptr(d.Seconds()) }
}

// WithSettings replaces the settings wholesale
func WithSettings(settings Settings) Option {
	return func(s *Settings) { *s = settings }
}

// Optimize solves the request in process, the way the optimizer service answers
// POST /optimize/charge-schedule. The request goes through the same JSON encoding it did on its
// way to the service, so a zero value the client omits is a default here as well, and the
// float32 fields reach the model as the float64 the service parsed from their decimal form.
func Optimize(ctx context.Context, in client.OptimizationInput, opts ...Option) (client.OptimizationResult, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return client.OptimizationResult{}, err
	}

	res, err := OptimizeJSON(ctx, body, opts...)
	if err != nil {
		return client.OptimizationResult{}, err
	}

	return res.toClient(), nil
}

// OptimizeJSON solves a request given as the JSON body the service receives
func OptimizeJSON(ctx context.Context, body []byte, opts ...Option) (Result, error) {
	settings := DefaultSettings()
	for _, o := range opts {
		o(&settings)
	}

	var data map[string]any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return Result{}, &Error{Code: 400, Message: "Failed to decode JSON object: " + err.Error()}
	}

	if details := validate(data, optimizationInputModel, ""); len(details) > 0 {
		return Result{}, &Error{Code: 400, Message: "Input payload validation failed", Details: details}
	}

	o, err := parse(data, settings)
	if err != nil {
		return Result{}, err
	}

	return solve(ctx, o)
}

// solve is the second try block of OptimizeCharging.post: any failure is a 500
func solve(ctx context.Context, o *Optimizer) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprint(r)
			if re, ok := r.(runtime.Error); ok && strings.Contains(re.Error(), "index out of range") {
				msg = "list index out of range"
			}
			err = &Error{Code: 500, Message: "Optimization failed: " + msg}
		}
	}()

	res, err = o.Solve(ctx)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return res, err
		}
		return res, &Error{Code: 500, Message: "Optimization failed: " + err.Error()}
	}
	return res, nil
}

// parse is the first try block of OptimizeCharging.post plus the Optimizer constructor
func parse(data map[string]any, settings Settings) (*Optimizer, error) {
	strat, _ := data["strategy"].(map[string]any)
	strategy := OptimizationStrategy{
		ChargingStrategy:    strOr(strat, "charging_strategy", "none"),
		DischargingStrategy: strOr(strat, "discharging_strategy", "none"),
	}

	g, _ := data["grid"].(map[string]any)
	grid := GridConfig{
		PMaxImp:    optNum(g, "p_max_imp"),
		PMaxExp:    optNum(g, "p_max_exp"),
		PrcPExcImp: optNum(g, "prc_p_exc_imp"),
		PrcEExcImp: optNum(g, "prc_e_exc_imp"),
		PrcEExcExp: optNum(g, "prc_e_exc_exp"),
	}

	var batteries []BatteryConfig
	for _, b := range data["batteries"].([]any) {
		bat := b.(map[string]any)
		sMax := num(bat["s_max"])
		batteries = append(batteries, BatteryConfig{
			ChargeFromGrid:  boolOr(bat, "charge_from_grid"),
			DischargeToGrid: boolOr(bat, "discharge_to_grid"),
			SCapacity:       orDefault(optNum(bat, "s_capacity"), sMax),
			SMin:            num(bat["s_min"]),
			SMax:            sMax,
			SInitial:        num(bat["s_initial"]),
			PDemand:         optList(bat, "p_demand"),
			SGoal:           optList(bat, "s_goal"),
			CMin:            num(bat["c_min"]),
			CMax:            num(bat["c_max"]),
			CMaxT:           optList(bat, "c_max_t"),
			SDrainT:         optList(bat, "s_drain_t"),
			DMax:            num(bat["d_max"]),
			PA:              num(bat["p_a"]),
			CPriority:       intOr(bat, "c_priority"),
			PrcReserve:      optNum(bat, "prc_reserve"),
			PrcGoal:         optNum(bat, "prc_goal"),
			ReserveForLoad:  boolOr(bat, "reserve_for_load"),
			ServeHomeFirst:  boolOr(bat, "serve_home_first"),
		})
	}

	series := data["time_series"].(map[string]any)
	ts := TimeSeriesData{
		Dt: list(series["dt"]),
		Gt: list(series["gt"]),
		Ft: list(series["ft"]),
		PN: list(series["p_N"]),
		PE: list(series["p_E"]),
	}

	lengths := []int{len(ts.Gt), len(ts.Ft), len(ts.PN), len(ts.PE)}
	for _, b := range batteries {
		if b.PDemand != nil {
			lengths = append(lengths, len(b.PDemand))
		}
	}
	for _, b := range batteries {
		if b.SGoal != nil {
			lengths = append(lengths, len(b.SGoal))
		}
	}
	for _, b := range batteries {
		if b.CMaxT != nil {
			lengths = append(lengths, len(b.CMaxT))
		}
	}
	for _, b := range batteries {
		if b.SDrainT != nil {
			lengths = append(lengths, len(b.SDrainT))
		}
	}
	for _, l := range lengths {
		if l != lengths[0] {
			// api.abort(400, ...) inside the try block is caught by its own `except Exception` and
			// re-raised with the werkzeug default description in place of the message
			return nil, &Error{Code: 400, Message: "Invalid data format: 400 Bad Request: The browser (or proxy) sent a request that this server could not understand."}
		}
	}

	o, err := NewOptimizer(strategy, grid, batteries, ts,
		orDefault(optNum(data, "eta_c"), 0.95), orDefault(optNum(data, "eta_d"), 0.95), 1e6, settings)
	if err != nil {
		return nil, &Error{Code: 500, Message: "Optimization failed: " + err.Error()}
	}

	// every slot indexes dt, an IndexError while the model is built
	if o.t > 0 && len(ts.Dt) < o.t {
		return nil, &Error{Code: 500, Message: "Optimization failed: list index out of range"}
	}

	return o, nil
}

func num(v any) float64 {
	f, err := v.(json.Number).Float64()
	if err != nil {
		panic(err)
	}
	return f
}

func optNum(m map[string]any, key string) *float64 {
	if v, ok := m[key]; ok && v != nil {
		return fptr(num(v))
	}
	return nil
}

func list(v any) []float64 {
	vs := v.([]any)
	res := make([]float64, len(vs))
	for i, x := range vs {
		res[i] = num(x)
	}
	return res
}

func optList(m map[string]any, key string) []float64 {
	if v, ok := m[key]; ok && v != nil {
		return list(v)
	}
	return nil
}

func strOr(m map[string]any, key, def string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return def
}

func boolOr(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func intOr(m map[string]any, key string) int {
	if v, ok := m[key].(json.Number); ok {
		i, err := strconv.Atoi(v.String())
		if err != nil {
			panic(err)
		}
		return i
	}
	return 0
}

// the request schema flask-restx validates against (api.model definitions in app.py)
type fieldKind int

const (
	kindNumber fieldKind = iota
	kindInteger
	kindBoolean
	kindString
	kindNumberList
	kindObject
	kindObjectList
)

type field struct {
	kind     fieldKind
	required bool
	enum     []string
	model    model
}

type model map[string]field

var (
	strategyModel = model{
		"charging_strategy":    {kind: kindString, enum: ChargingStrategies},
		"discharging_strategy": {kind: kindString, enum: DischargingStrategies},
	}
	gridModel = model{
		"p_max_imp":     {kind: kindNumber},
		"p_max_exp":     {kind: kindNumber},
		"prc_p_exc_imp": {kind: kindNumber},
		"prc_e_exc_imp": {kind: kindNumber},
		"prc_e_exc_exp": {kind: kindNumber},
	}
	batteryConfigModel = model{
		"charge_from_grid":  {kind: kindBoolean},
		"discharge_to_grid": {kind: kindBoolean},
		"s_capacity":        {kind: kindNumber},
		"s_min":             {kind: kindNumber, required: true},
		"s_max":             {kind: kindNumber, required: true},
		"s_initial":         {kind: kindNumber, required: true},
		"p_demand":          {kind: kindNumberList},
		"s_goal":            {kind: kindNumberList},
		"c_min":             {kind: kindNumber, required: true},
		"c_max":             {kind: kindNumber, required: true},
		"d_max":             {kind: kindNumber, required: true},
		"p_a":               {kind: kindNumber, required: true},
		"c_priority":        {kind: kindInteger},
		"prc_reserve":       {kind: kindNumber},
		"prc_goal":          {kind: kindNumber},
		"reserve_for_load":  {kind: kindBoolean},
		"serve_home_first":  {kind: kindBoolean},
	}
	timeSeriesModel = model{
		"dt":  {kind: kindNumberList, required: true},
		"gt":  {kind: kindNumberList, required: true},
		"ft":  {kind: kindNumberList, required: true},
		"p_N": {kind: kindNumberList, required: true},
		"p_E": {kind: kindNumberList, required: true},
	}
	optimizationInputModel = model{
		"strategy":    {kind: kindObject, model: strategyModel},
		"grid":        {kind: kindObject, model: gridModel},
		"batteries":   {kind: kindObjectList, required: true, model: batteryConfigModel},
		"time_series": {kind: kindObject, required: true, model: timeSeriesModel},
		"eta_c":       {kind: kindNumber},
		"eta_d":       {kind: kindNumber},
	}
)

// pyRepr is Python's repr() of a decoded JSON value, as jsonschema quotes it
func pyRepr(v any) string {
	switch v := v.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return "'" + v + "'"
	case json.Number:
		return v.String()
	case []any:
		var s []string
		for _, x := range v {
			s = append(s, pyRepr(x))
		}
		return "[" + strings.Join(s, ", ") + "]"
	case map[string]any:
		var s []string
		for _, k := range slices.Sorted(maps.Keys(v)) {
			s = append(s, "'"+k+"': "+pyRepr(v[k]))
		}
		return "{" + strings.Join(s, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func isNumber(v any) bool {
	_, ok := v.(json.Number)
	return ok
}

func isInteger(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	if _, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return true
	}
	f, err := n.Float64()
	return err == nil && f == float64(int64(f))
}

func typeError(v any, typ string) string {
	return fmt.Sprintf("%s is not of type '%s'", pyRepr(v), typ)
}

// validate checks data against m and returns flask-restx's error details: the dotted path of
// each offending value and the jsonschema message.
func validate(data any, m model, path string) map[string]string {
	errs := make(map[string]string)
	obj, ok := data.(map[string]any)
	if !ok {
		errs[strings.TrimSuffix(path, ".")] = typeError(data, "object")
		return errs
	}

	for _, name := range slices.Sorted(maps.Keys(m)) {
		f := m[name]
		v, present := obj[name]
		p := path + name
		if !present {
			if f.required {
				errs[p] = fmt.Sprintf("'%s' is a required property", name)
			}
			continue
		}

		switch f.kind {
		case kindNumber:
			if !isNumber(v) {
				errs[p] = typeError(v, "number")
			}
		case kindInteger:
			if !isInteger(v) {
				errs[p] = typeError(v, "integer")
			}
		case kindBoolean:
			if _, ok := v.(bool); !ok {
				errs[p] = typeError(v, "boolean")
			}
		case kindString:
			s, ok := v.(string)
			if !ok {
				errs[p] = typeError(v, "string")
			} else if f.enum != nil && !slices.Contains(f.enum, s) {
				var quoted []string
				for _, e := range f.enum {
					quoted = append(quoted, "'"+e+"'")
				}
				errs[p] = fmt.Sprintf("%s is not one of [%s]", pyRepr(s), strings.Join(quoted, ", "))
			}
		case kindNumberList:
			items, ok := v.([]any)
			if !ok {
				errs[p] = typeError(v, "array")
				continue
			}
			for i, x := range items {
				if !isNumber(x) {
					errs[fmt.Sprintf("%s.%d", p, i)] = typeError(x, "number")
				}
			}
		case kindObject:
			maps.Copy(errs, validate(v, f.model, p+"."))
		case kindObjectList:
			items, ok := v.([]any)
			if !ok {
				errs[p] = typeError(v, "array")
				continue
			}
			for i, x := range items {
				maps.Copy(errs, validate(x, f.model, fmt.Sprintf("%s.%d.", p, i)))
			}
		}
	}

	return errs
}

// f32 is the float32 the client decodes from the service's JSON for a float64 value: Python
// writes its shortest repr, encoding/json parses that to the nearest float32.
func f32(f float64) float32 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'g', -1, 64), 32)
	return float32(r)
}

func f32s(fs []*float64) []float32 {
	res := make([]float32, len(fs))
	for i, f := range fs {
		if f != nil {
			res[i] = f32(*f)
		}
	}
	return res
}

// toClient is the response as the generated client decodes it
func (r Result) toClient() client.OptimizationResult {
	res := client.OptimizationResult{
		Status:              client.OptimizationResultStatus(r.Status),
		GridImport:          f32s(r.GridImport),
		GridExport:          f32s(r.GridExport),
		GridImportOvershoot: f32s(r.GridImportOvershoot),
		GridExportOvershoot: f32s(r.GridExportOvershoot),
		Batteries:           []client.BatteryResult{},
		FlowDirection:       []client.OptimizationResultFlowDirection{},
		LimitViolations: client.LimitViolationResult{
			GridImportLimitExceeded: r.LimitViolations.GridImportLimitExceeded,
			GridExportLimitHit:      r.LimitViolations.GridExportLimitHit,
		},
	}
	if r.ObjectiveValue != nil {
		res.ObjectiveValue = f32(*r.ObjectiveValue)
	}
	for _, b := range r.Batteries {
		res.Batteries = append(res.Batteries, client.BatteryResult{
			ChargingPower:    f32s(b.ChargingPower),
			DischargingPower: f32s(b.DischargingPower),
			StateOfCharge:    f32s(b.StateOfCharge),
		})
	}
	for _, d := range r.FlowDirection {
		res.FlowDirection = append(res.FlowDirection, client.OptimizationResultFlowDirection(d))
	}
	return res
}
