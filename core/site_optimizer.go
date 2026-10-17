package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/metrics"
	evopt "github.com/evcc-io/evcc/core/optimizer"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/hems/hems"
	"github.com/evcc-io/evcc/messenger"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/sponsor"
	optimizer "github.com/evcc-io/optimizer/client"
	"github.com/jinzhu/now"
	"github.com/samber/lo"
	"golang.org/x/exp/constraints"
)

const (
	// eta is the efficiency of the battery charging/discharging
	eta = 0.9

	// batteryPower is the default power of the battery in W
	batteryPower = 6000
)

// optimizerStrategyServeHomeFirst is the "serve home first" secondary goal. It is not an
// optimizer charging strategy but evcc's own: the optimizer gets no charging strategy
// ("none") and every home battery asks for the serve_home_first tie break instead - of
// schedules that cost the same, the pack feeds the house and the grid the car (see
// batteryRequest).
const optimizerStrategyServeHomeFirst = "serve_home_first"

// optimizerChargingStrategies are the valid secondary goals (the UI's "secondary goal"
// dropdown): the optimizer's grid charging strategies plus serve home first. The first
// entry is the default and preserves the previous hard-coded behavior.
var optimizerChargingStrategies = []string{
	string(optimizer.OptimizerStrategyChargingStrategyChargeBeforeExport),
	optimizerStrategyServeHomeFirst,
	string(optimizer.OptimizerStrategyChargingStrategyAttenuateDemandPeaks),
	string(optimizer.OptimizerStrategyChargingStrategyAttenuateFeedinPeaks),
	string(optimizer.OptimizerStrategyChargingStrategyAttenuateGridPeaks),
	string(optimizer.OptimizerStrategyChargingStrategyNone),
}

const defaultOptimizerChargingStrategy = string(optimizer.OptimizerStrategyChargingStrategyChargeBeforeExport)

// optimizerDecaySlots is the number of slots over which measured values decay into the forecast
const optimizerDecaySlots = 4

// optimizerResult wraps the optimizer publish payload to implement BytesMarshaler.
// This ensures publishComplex serializes it as a single JSON message instead of
// recursively decomposing each struct field and array element into individual MQTT
// topics (~1,500 messages per optimizer run).
type optimizerResult struct {
	Updated time.Time                    `json:"updated"`
	Req     optimizer.OptimizationInput  `json:"req"`
	Res     optimizer.OptimizationResult `json:"res"`
	Details requestDetails               `json:"details"`
}

var _ api.BytesMarshaler = (*optimizerResult)(nil)

func (r optimizerResult) MarshalBytes() ([]byte, error) {
	return json.Marshal(r)
}

type batteryType string

const (
	OPTIMIZER_URI = "https://optimizer.evcc.io"

	batteryTypeLoadpoint batteryType = "loadpoint"
	batteryTypeVehicle   batteryType = "vehicle"
	batteryTypeBattery   batteryType = "battery"
)

type batteryDetail struct {
	Type     batteryType `json:"type"`
	Title    string      `json:"title,omitempty"`
	Name     string      `json:"name,omitempty"`
	Capacity float64     `json:"capacity,omitempty"`

	loadpoint    *int // originating loadpoint id for loadpoint/vehicle entries
	controllable bool // device can act on suggestions
}

// batteryKey and loadpointKey build the canonical device keys used for
// suggestion routing and notifications
func batteryKey(name string) string { return "battery:" + name }
func loadpointKey(id int) string    { return fmt.Sprintf("loadpoint:%d", id) }

// key identifies the device across optimizer runs; an empty key means the
// device can't act on a suggestion.
func (d batteryDetail) key() string {
	switch {
	case d.Type == batteryTypeBattery:
		return batteryKey(d.Name)
	case d.loadpoint != nil:
		return loadpointKey(*d.loadpoint)
	default:
		return ""
	}
}

// currentAction returns the device's current operating mode for suggestion
// comparison. Must only be called for devices with a non-empty key.
func (d batteryDetail) currentAction(site *Site) string {
	if d.Type == batteryTypeBattery {
		return site.GetBatteryMode().String()
	}
	return loadpointCurrentAction(site.loadpoints[*d.loadpoint])
}

type batteryResult struct {
	batteryDetail
	Full  time.Time `json:"full,omitzero"`
	Empty time.Time `json:"empty,omitzero"`
}

// suggestionThreshold ignores numerical noise around zero power (W)
const suggestionThreshold = 50

// advisory actions for a loadpoint/vehicle slot; battery actions use api.BatteryMode
const (
	actionStop   = "stop"
	actionCharge = "charge"
)

// actionDischarge is the battery-to-grid discharge advisory. It maps to
// api.BatteryDischarge, which control applies to actively export the battery.
const actionDischarge = "discharge"

// charge sources classify where a loadpoint's charge comes from in the
// optimizer's plan (see types.Suggestion.Source). A non-surplus source charges
// the car at the requested power in PV mode instead of deferring to real-time
// surplus:
//   - grid:    the slot imports (a grid-sourced fast charge)
//   - battery: the home battery discharges to feed the car (no grid flow), so
//     measured export surplus is ~0 and would otherwise starve the car
//   - surplus: real-time PV export; PV mode tracks it directly
const (
	sourceSurplus = "surplus"
	sourceGrid    = "grid"
	sourceBattery = "battery"
)

// evChargeSource refines a car's charge source: a surplus charge where the home
// battery is discharging (and nothing is exported) is actually battery-fed. PV
// mode must then charge at the planned power instead of deferring to measured
// export surplus, which won't reflect the battery's contribution.
func evChargeSource(base string, batteryDischarging, gridExporting bool) string {
	if base == sourceSurplus && batteryDischarging && !gridExporting {
		return sourceBattery
	}
	return base
}

// evSuggestion notifies when the optimizer's advisory action for a device changes
const evSuggestion = "suggestion"

// pendingSuggestion pairs a device's current-run suggestion with the
// notification event to emit if it represents an actionable change.
type pendingSuggestion struct {
	suggestion types.Suggestion
	event      messenger.Event
}

// suggestionEvent builds the notification event for a device suggestion
func suggestionEvent(detail batteryDetail, s types.Suggestion) messenger.Event {
	ev := messenger.Event{Event: evSuggestion, Attributes: map[string]any{
		"suggestionAction": s.Action,
		"suggestionTitle":  detail.Title,
	}}

	switch {
	case detail.Type == batteryTypeBattery:
		ev.Attributes["suggestionName"] = detail.Name
	case detail.loadpoint != nil:
		id := *detail.loadpoint
		ev.Loadpoint = &id
	}

	return ev
}

// optimizerFrontRunLead is how early we may switch the home battery into an export
// mode before the next slot boundary, to cover the Powerwall's ~2min autonomous-mode
// TOU pickup — otherwise the pickup eats the start of the export slot.
const optimizerFrontRunLead = 2 * time.Minute

// isSpikeAction reports whether a battery action switches the Powerwall to
// autonomous mode + the export spike (the transition with the ~2min pickup lag).
func isSpikeAction(action string) bool {
	return action == api.BatteryHoldCharge.String() || action == actionDischarge
}

// currentSlotSuggestion maps the optimizer's slot-idx corner result onto an advisory action.
// Because the optimization is linear, the slot is at an operating-range extreme, so it maps
// cleanly onto the discrete battery mode / loadpoint intent that control would later apply.
// An idle battery is interpreted from the grid flow: importing means discharge is withheld
// (hold), exporting means charging is withheld (holdcharge). idx selects the slot (0 = current,
// 1 = next, for front-running the export-mode transition).
func currentSlotSuggestion(detail batteryDetail, res optimizer.BatteryResult, idx int, gridImporting, gridExporting, noGridImport bool, slotHours float64) types.Suggestion {
	if slotHours <= 0 || idx < 0 || idx >= len(res.ChargingPower) || idx >= len(res.DischargingPower) {
		return types.Suggestion{}
	}

	charge := float64(res.ChargingPower[idx]) / slotHours
	discharge := float64(res.DischargingPower[idx]) / slotHours

	s := types.Suggestion{Charge: charge, Discharge: discharge}

	if detail.Type == batteryTypeBattery {
		idle := charge <= suggestionThreshold && discharge <= suggestionThreshold
		switch {
		case charge > suggestionThreshold && gridImporting:
			// charging while importing means grid charging
			s.Action = api.BatteryCharge.String()
		case idle && gridImporting:
			// idle while importing: discharge is deliberately withheld
			s.Action = api.BatteryHold.String()
		case idle && gridExporting:
			// idle while exporting: surplus is exported instead of charged
			s.Action = api.BatteryHoldCharge.String()
		case discharge > suggestionThreshold && gridExporting:
			// discharging while exporting means battery-to-grid discharge
			s.Action = actionDischarge
		default:
			s.Action = api.BatteryNormal.String()
		}

		// self-consumption (no grid import allowed): don't HOLD — backup mode blocks
		// discharge and forces grid import when solar dips. holdcharge is NOT
		// remapped; its controller decides by SoC whether to spike-and-hold (prevent
		// charging below 100%) or fall through to normal (top off at high SoC).
		if noGridImport && s.Action == api.BatteryHold.String() {
			s.Action = api.BatteryNormal.String()
		}
	} else if charge > suggestionThreshold {
		s.Action = actionCharge
		// classify the charge source so PV mode can honor a grid-sourced fast
		// charge instead of only ever charging from surplus. In the default
		// no-import config gridImporting is ~always false, so this stays "surplus"
		// unless the user allows import and the optimizer chose to import.
		if gridImporting {
			s.Source = sourceGrid
		} else {
			s.Source = sourceSurplus
		}
	} else {
		s.Action = actionStop
	}

	return s
}

// loadpointCurrentAction returns the loadpoint's current operating mode for
// suggestion comparison, reusing chargeGoalReached so a loadpoint left
// enabled while idle (e.g. vehicle finished at its limit) is treated as
// stopped instead of triggering a spurious pause suggestion.
func loadpointCurrentAction(lp *Loadpoint) string {
	lp.RLock()
	enabled := lp.enabled
	lp.RUnlock()

	if enabled && !lp.chargeGoalReached(enabled) {
		return actionCharge
	}
	return actionStop
}

// suggestionSlot is one slot of a device's suggestion plan: the suggestion that
// applies until `until`, the slot's end. A zero `until` never expires.
type suggestionSlot struct {
	until      time.Time
	suggestion types.Suggestion
}

// currentSuggestion resolves the plan slot in effect at `now`. Resolving by end
// time keeps a short first slot (a run seconds before a boundary) from governing
// the following slot, and an exhausted plan yields no suggestion instead of a
// stale one.
func currentSuggestion(plan []suggestionSlot, now time.Time) (types.Suggestion, bool) {
	i := slices.IndexFunc(plan, func(s suggestionSlot) bool { return s.until.IsZero() || s.until.After(now) })
	if i < 0 || plan[i].suggestion.Action == "" {
		return types.Suggestion{}, false
	}
	return plan[i].suggestion, true
}

// setSuggestions replaces the suggestions with fixed ones that never expire
func (site *Site) setSuggestions(suggestions map[string]types.Suggestion) {
	var plans map[string][]suggestionSlot
	if suggestions != nil {
		plans = make(map[string][]suggestionSlot, len(suggestions))
		for key, s := range suggestions {
			plans[key] = []suggestionSlot{{suggestion: s}}
		}
	}
	site.setSuggestionPlans(plans)
}

// setSuggestionPlans replaces the per-slot suggestion plans applied on each publish
func (site *Site) setSuggestionPlans(plans map[string][]suggestionSlot) {
	site.Lock()
	defer site.Unlock()

	site.suggestions = plans
}

// suggestionComputedAt returns when the current suggestions' input snapshot was
// taken, or the zero time when suggestions were set outside an optimizer run.
func (site *Site) suggestionComputedAt() time.Time {
	site.RLock()
	defer site.RUnlock()
	return site.suggestionsComputedAt
}

// suggestion returns the optimizer suggestion for the given device key.
// The actionable flag is evaluated on read against the device's current
// action since that changes between optimizer runs.
func (site *Site) suggestion(key, currentAction string) *types.Suggestion {
	site.RLock()
	plan := site.suggestions[key]
	site.RUnlock()

	s, ok := currentSuggestion(plan, time.Now())
	if !ok {
		return nil
	}

	s.Actionable = s.Action != currentAction

	return &s
}

// optimizerBatteryMode returns the battery mode the optimizer suggests for the
// site's controllable batteries, or api.BatteryUnknown when there is no
// suggestion. The optimizer is always-on: whenever it produces a suggestion,
// that suggestion drives battery control (taking precedence over the static
// grid-charge / discharge-control heuristics, but not over an external/manual
// mode). If controllable batteries disagree it returns Unknown, since the site
// applies a single mode.
func (site *Site) optimizerBatteryMode() api.BatteryMode {
	now := time.Now()

	site.RLock()
	schedules := site.batterySchedules
	site.RUnlock()

	res := api.BatteryUnknown
	for _, dev := range site.batteryMeters {
		if dev == nil {
			continue
		}
		name := dev.Config().Name
		action := currentBatteryAction(schedules[batteryKey(name)], now, site.batteryDeviceSoc(name))
		if action == "" {
			continue
		}
		m, err := api.BatteryModeString(action)
		if err != nil {
			continue
		}
		if res != api.BatteryUnknown && res != m {
			return api.BatteryUnknown
		}
		res = m
	}
	return res
}

// loadpointOptimizerAction returns the optimizer's advisory action for the loadpoint
// ("charge"/"stop"/""), its requested charge power (W, for a "charge" action), and the
// charge source ("surplus"/"grid"). Both actions are honored (always-on): "stop" pauses
// the car so surplus is exported / the battery is reserved for a higher-value export
// slot. A "charge" is surplus-only in PV mode unless the source is "grid" (a grid-sourced
// fast charge), in which case PV mode charges at the requested power; MinPV always
// charges at the requested power.
func (site *Site) loadpointOptimizerAction(id int) (string, float64, string) {
	lp := site.loadpoints[id]
	if lp == nil {
		return "", 0, ""
	}
	s := site.suggestion(loadpointKey(id), loadpointCurrentAction(lp))
	if s == nil {
		return "", 0, ""
	}

	// Ignore a suggestion computed before the car connected: it reflects the
	// disconnected loadpoint (zero current-slot availability -> "stop") and would
	// pause a car that just plugged in until the connect-triggered re-optimization
	// lands. Falling through to no action lets normal PV/welcome charging run in the
	// meantime; the next run (reflecting the connection) supersedes it.
	if computedAt := site.suggestionComputedAt(); !computedAt.IsZero() && lp.connectedAfter(computedAt) {
		return "", 0, ""
	}

	action := s.Action

	// A battery-sourced boost drains the home battery into the car. Once the pack
	// reaches the plan's reserve it stops discharging, so continuing to charge at
	// the requested power would import from the grid at (peak) prices - the boost
	// is only ever meant to consume the pack's surplus above the reserve. Stop the
	// car there. The stopped car also releases the pack's reserve pin
	// (optimizerReservePinned), so the pack goes on serving the house instead
	// of holding at the target while the house imports.
	if action == actionCharge && s.Source == sourceBattery && site.batteryBoostReserveReached(s.Charge, lp.GetChargePower()) {
		action = actionStop
	}

	return action, s.Charge, s.Source
}

// boostReserveLagCycles is how many control cycles of boost draw the reserve trip
// looks ahead. The guard sees cycle-stale soc/grid readings (the action is computed
// before the meters refresh) and the charger takes ~a further cycle to ramp down, so
// without a margin the pack crosses the reserve and the car's draw spills to the grid
// for ~this long. Two cycles ≈ the observed stale-read + charger-ramp latency.
const boostReserveLagCycles = 2

// batteryBoostReserveReached reports whether an active battery boost has driven
// the home battery to its planned reserve, so the boost must stop charging the
// car instead of importing from the grid to keep it going. requestedPower is the
// boost's requested power, actualPower the car's measured charge power.
//
// It trips when the (power-integrated) pack soc has reached the plan's target
// reserve plus a reaction-lag margin (so the car stops before the pack crosses the
// reserve, not a cycle after it has already spilled to the grid), or - as a real-
// time backstop - when the boost's own draw is already spilling to the grid. It
// latches for the current slot once tripped: at the reserve the pack won't recover
// within the slot, so re-enabling the car would only oscillate it on and off around
// the boundary (car off -> grid import clears -> car on -> pack still at reserve ...).
//
// Reads last-cycle gridPower/soc/chargePower (meters refresh later in sitePower);
// a one-cycle reaction is acceptable and dampens the oscillation.
func (site *Site) batteryBoostReserveReached(requestedPower, actualPower float64) bool {
	slot := time.Now().Truncate(tariff.SlotDuration)
	if slot.Equal(site.batteryBoostReserveSlot) {
		return true // already tripped this slot
	}

	tripped := false

	// Stop the boost a lookahead-margin ABOVE the plan's reserve, not exactly at it.
	// The soc/grid readings the guard sees are a cycle stale (the action is computed
	// before the meters refresh) and the charger takes ~a further cycle to ramp down,
	// so waiting for soc <= target lets the pack cross the reserve mid-cycle and the
	// car's draw spill to the grid at (peak) prices for ~a cycle. The margin is the SoC
	// the boost pulls over that reaction lag - derived from the boost power, pack
	// capacity and loop cadence, so it self-scales instead of being a fixed offset - so
	// the pack coasts down to the reserve only after the car is already off.
	soc := site.battery.Soc
	var margin float64
	if capacity := site.battery.Capacity; capacity > 0 && requestedPower > 0 && site.interval > 0 {
		lag := boostReserveLagCycles * site.interval
		margin = requestedPower * lag.Hours() / (capacity * 1e3) * 100 // Wh drawn -> SoC points
	}
	for _, dev := range site.batteryMeters {
		if dev == nil {
			continue
		}
		if target := site.optimizerBatteryTargetSoc(dev.Config().Name); target > 0 && soc <= target+margin {
			tripped = true
			break
		}
	}

	// real-time safety net: the car is drawing the boost power but a large share of
	// it is coming from the grid, i.e. the pack stopped discharging at its reserve.
	// Gate on the car actually drawing (actualPower) so pre-boost grid import - e.g.
	// from a hold state the boost is just leaving - can't false-trip and latch the
	// car off before it even starts. Half the boost power keeps ordinary home-load
	// import from tripping it (at the reserve grid import ~= the full boost power).
	if !tripped && requestedPower > 0 && actualPower > requestedPower/2 && site.gridPower > requestedPower/2 {
		tripped = true
	}

	if tripped {
		// logged once per slot (set only on the first trip), not every cycle
		site.log.DEBUG.Printf("optimizer: battery boost reached reserve (soc %.0f%%, grid %.0fW) - stopping car to avoid grid import", soc, site.gridPower)
		site.batteryBoostReserveSlot = slot

		// the plan expected the boost to continue; re-plan from where it stopped
		site.requestReplan("battery boost reached reserve")
	}
	return tripped
}

// replanner re-plans when execution deviates from the optimizer's plan
type replanner interface {
	requestReplan(reason string) bool
}

var _ replanner = (*Site)(nil)

// requestReplan re-runs the optimizer because execution deviated from the plan,
// at most once per slot and reason: a deviation that persists can't be planned
// away within the slot, and the next slot re-plans anyway. It reports whether a
// run was requested.
func (site *Site) requestReplan(reason string) bool {
	slot := time.Now().Truncate(tariff.SlotDuration)

	site.Lock()
	if site.replanSlots == nil {
		site.replanSlots = make(map[string]time.Time)
	}
	if site.replanSlots[reason].Equal(slot) {
		site.Unlock()
		return false
	}
	site.replanSlots[reason] = slot
	site.Unlock()

	site.log.DEBUG.Printf("optimizer: re-plan, %s", reason)
	go site.optimizerUpdateAsync(0)

	return true
}

// planSocTolerance is how far (SoC points) a battery may stray outside its
// planned slot trajectory before execution counts as deviating: the resolution
// most batteries report SoC at.
const planSocTolerance = 1.0

// socOffPlan reports whether soc (percent) lies outside the slot's planned
// trajectory, between its start and end SoC, by more than planSocTolerance.
func socOffPlan(slot batterySlot, soc float64) bool {
	if soc <= 0 || slot.soc <= 0 || slot.startSoc <= 0 {
		return false
	}
	lo, hi := min(slot.startSoc, slot.soc), max(slot.startSoc, slot.soc)
	return soc < lo-planSocTolerance || soc > hi+planSocTolerance
}

// checkBatteryPlan re-plans when a home battery's soc has left its planned
// trajectory for the current slot, e.g. a car drew the pack below the plan's
// target or the pack did not discharge as planned.
func (site *Site) checkBatteryPlan(now time.Time) {
	site.RLock()
	schedules := site.batterySchedules
	site.RUnlock()

	for _, dev := range site.batteryMeters {
		if dev == nil {
			continue
		}
		name := dev.Config().Name
		plan := schedules[batteryKey(name)]
		i := slices.IndexFunc(plan, func(s batterySlot) bool { return s.until.After(now) })
		if i < 0 {
			continue
		}
		if soc := site.batteryDeviceSoc(name); socOffPlan(plan[i], soc) &&
			site.requestReplan(fmt.Sprintf("battery %s off plan", name)) {
			site.log.DEBUG.Printf("optimizer: battery %s soc %.1f%% outside planned %.1f%%..%.1f%%", name, soc, plan[i].startSoc, plan[i].soc)
		}
	}
}

// publishSuggestions publishes the loadpoints' suggestions
func (site *Site) publishSuggestions() {
	for id, lp := range site.loadpoints {
		if lp == nil {
			continue
		}

		var val any
		if s := site.suggestion(loadpointKey(id), loadpointCurrentAction(lp)); s != nil {
			val = *s
		}
		site.publishLoadpoint(id, keys.Suggestion, val)
	}
}

// clearSuggestions removes all suggestions and the battery forecast when the
// optimizer result is stale
func (site *Site) clearSuggestions() {
	site.setSuggestions(nil)
	site.battery.Forecast = nil

	site.publishBattery()
	site.publishSuggestions()

	site.Lock()
	site.suggestionActions = nil
	site.batterySchedules = nil
	site.Unlock()
}

// pendingSuggestions collects the stored suggestions with their actionable flag
// evaluated against the devices' current operating mode
func (site *Site) pendingSuggestions(details []batteryDetail) map[string]pendingSuggestion {
	pending := make(map[string]pendingSuggestion, len(details))

	for _, detail := range details {
		key := detail.key()
		if key == "" {
			continue
		}

		s := site.suggestion(key, detail.currentAction(site))
		if s == nil {
			continue
		}

		pending[key] = pendingSuggestion{suggestion: *s, event: suggestionEvent(detail, *s)}
	}

	return pending
}

// diffSuggestions updates the tracked actionable optimizer suggestions and
// returns the events to send for devices whose actionable action changed since
// the last run. Non-actionable or vanished devices are pruned so a later
// actionable change re-notifies.
func (site *Site) diffSuggestions(pending map[string]pendingSuggestion) []messenger.Event {
	site.Lock()
	defer site.Unlock()

	if site.suggestionActions == nil {
		site.suggestionActions = make(map[string]string)
	}

	// prune devices that are gone or no longer actionable
	for key := range site.suggestionActions {
		if p, ok := pending[key]; !ok || !p.suggestion.Actionable {
			delete(site.suggestionActions, key)
		}
	}

	var events []messenger.Event
	for key, p := range pending {
		if !p.suggestion.Actionable || site.suggestionActions[key] == p.suggestion.Action {
			continue
		}
		site.suggestionActions[key] = p.suggestion.Action
		events = append(events, p.event)
	}
	return events
}

type requestDetails struct {
	Timestamps     []time.Time     `json:"timestamp"`
	BatteryDetails []batteryDetail `json:"batteryDetails"`
}

// optimizerBattery pairs a battery request entry with its device detail
type optimizerBattery struct {
	cfg    optimizer.BatteryConfig
	detail batteryDetail
}

// optimizerURI is the optimizer service configured with OPTIMIZER_URI. Without one the
// optimizer runs in process (see optimize).
func optimizerURI() string {
	return os.Getenv("OPTIMIZER_URI")
}

const (
	// optimizerTimeLimit is the deadline of an in-process solve, whose stages share it.
	// The hosted service's 10s left a two-day horizon's preference stage unfinished.
	optimizerTimeLimit = 30 * time.Second

	// optimizerTimeout bounds a whole optimizer run, as the HTTP client's timeout did
	optimizerTimeout = 90 * time.Second
)

const slotsPerHour = float64(time.Hour / tariff.SlotDuration)

// errOptimizerNotReady means battery measurements aren't available yet (e.g. at
// startup); the slot gate is left open so the next cycle retries.
var errOptimizerNotReady = errors.New("battery measurements not ready")

// batteriesReported returns errOptimizerNotReady until every configured battery meter
// with a soc has reported one at least once. Planning without a pack that simply hasn't
// been read yet - typically right after startup, when a car's soc arrives before the
// pack's - produces a plan with no schedule for that pack, and that plan then stands
// for a whole slot until the next run. Retrying each cycle instead costs a few seconds.
//
// Only the first report is waited for: a pack that has reported before and drops out
// later (e.g. its cloud API is down) is left out of the plan as before, rather than
// holding the optimizer - and the house's stale suggestions - hostage for the outage.
// A meter without a soc can never report one, so it is not waited for.
//
// Must be called under optimizerMu.
func (site *Site) batteriesReported(battery []types.Measurement) error {
	if site.batteriesSeen == nil {
		site.batteriesSeen = make(map[string]bool)
	}

	var missing []string
	for i, dev := range site.batteryMeters {
		name := dev.Config().Name
		if i < len(battery) && battery[i].Soc != nil {
			site.batteriesSeen[name] = true
			continue
		}
		if !site.batteriesSeen[name] && api.HasCap[api.Battery](dev.Instance()) {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		site.log.DEBUG.Printf("optimizer: waiting for battery soc: %s", strings.Join(missing, ", "))
		return errOptimizerNotReady
	}
	return nil
}

// frontRunScheduleSlots caps how many upcoming plan slots are retained per
// battery for front-running. The optimizer re-runs at least every tariff slot
// (plus on vehicle/setting changes), so a handful covers the current slot plus
// lookahead across a boundary.
const frontRunScheduleSlots = 4

// batterySlot is one slot of the optimizer's battery-mode plan: the action to
// apply until `until` (the slot's end boundary) and the planned end-of-slot SoC
// (percent), which the controller uses e.g. as the grid-discharge reserve floor.
type batterySlot struct {
	until  time.Time
	action string
	soc    float64 // optimizer's planned end-of-slot SoC (percent), 0 if unknown

	startSoc float64 // planned SoC (percent) at the slot's start, 0 if unknown
}

// currentBatterySlot resolves the plan slot in effect at `now` for a battery at
// the given soc (percent; 0 when unknown). Slots are indexed by end time, which
// keeps the current slot correct even when a boundary passes between optimizer
// runs. Two front-runs adopt an upcoming slot early:
//
//   - by soc: a charge/discharge slot whose end-of-slot target the pack has
//     already reached is skipped, so the optimizer's next slot applies now. The
//     optimizer plans a full per-slot sequence, so reaching a discharge target
//     ahead of its time boundary just means the pack is ahead of the plan and
//     the next slot (e.g. HoldCharge, covering the house from the pack) is what
//     it decided to do next — otherwise the pack sits pinned at the reserve and
//     the house imports from the grid until the boundary. Reacting on the
//     estimator-smoothed soc lets a max-rate export that hits the target
//     mid-slot transition immediately instead of minutes late.
//   - by time: within optimizerFrontRunLead of the upcoming boundary, if the
//     next slot flips export-ness, adopt it to cover the Powerwall's ~2min
//     autonomous pickup.
func currentBatterySlot(plan []batterySlot, now time.Time, soc float64) (batterySlot, bool) {
	i := slices.IndexFunc(plan, func(s batterySlot) bool { return s.until.After(now) })
	if i < 0 {
		return batterySlot{}, false
	}

	// skip slots whose soc target is already met: the pack is ahead of the plan,
	// so the next slot's action/target is what applies now
	for i+1 < len(plan) && batterySlotSatisfied(plan[i], soc) {
		i++
	}

	cur := plan[i]
	if i+1 < len(plan) {
		next := plan[i+1]
		if cur.until.Sub(now) <= optimizerFrontRunLead && isSpikeAction(cur.action) != isSpikeAction(next.action) {
			return next, true
		}
	}
	return cur, true
}

// batterySlotSatisfied reports whether the pack at soc (percent) has already
// reached the plan slot's end-of-slot target in the direction of its action: a
// discharge that has fallen to/below its target, or a charge that has risen
// to/above it. Only these two actions are soc-gated; holding actions (hold,
// holdcharge, normal) hold in place and are never treated as satisfied. A
// missing soc or target (0) is not satisfied, falling back to time resolution.
func batterySlotSatisfied(s batterySlot, soc float64) bool {
	if soc <= 0 || s.soc <= 0 {
		return false
	}
	switch s.action {
	case actionDischarge:
		return soc <= s.soc
	case api.BatteryCharge.String():
		return soc >= s.soc
	default:
		return false
	}
}

// currentBatteryAction resolves the battery-mode action for `now` from the plan.
func currentBatteryAction(plan []batterySlot, now time.Time, soc float64) string {
	if s, ok := currentBatterySlot(plan, now, soc); ok {
		return s.action
	}
	return ""
}

// batteryDeviceSoc returns the freshest known soc (percent) for the named
// battery from the latest per-device measurements, or 0 when unknown. This is
// the estimator-smoothed value (see BatteryEstimator), so plan advancement
// reacts to a reached target without the raw sensor's minutes-long lag.
func (site *Site) batteryDeviceSoc(name string) float64 {
	for _, d := range site.battery.Devices {
		if d.Name == name && d.Soc != nil {
			return *d.Soc
		}
	}
	return 0
}

// optimizerBatteryTargetSoc returns the optimizer's planned SoC (percent) for the
// battery's current plan slot, or 0 when there is no plan. Control passes it to a
// BatteryModeController so e.g. a grid discharge stops at the target.
func (site *Site) optimizerBatteryTargetSoc(name string) float64 {
	now := time.Now()

	site.RLock()
	plan := site.batterySchedules[batteryKey(name)]
	site.RUnlock()

	if s, ok := currentBatterySlot(plan, now, site.batteryDeviceSoc(name)); ok {
		return s.soc
	}
	return 0
}

// setBatterySchedules replaces the per-battery mode plans used for front-running
func (site *Site) setBatterySchedules(schedules map[string][]batterySlot) {
	site.Lock()
	defer site.Unlock()
	site.batterySchedules = schedules
}

// optimizerDue reports whether a run is due at now after the last one: once
// minAge has passed, or as soon as a new slot starts. Re-planning at the boundary
// keeps the first slot a full slot of fresh state instead of drifting off the
// slot grid, where a run lands seconds before a boundary.
func optimizerDue(last, now time.Time, minAge time.Duration) bool {
	return now.Sub(last) >= minAge || !now.Truncate(tariff.SlotDuration).Equal(last.Truncate(tariff.SlotDuration))
}

// optimizerUpdateAsync runs the optimizer unless the last run is younger than
// minAge and still in the current slot (see optimizerDue). Pass 0 to force a run,
// e.g. when a changed setting should take effect without waiting for the next
// slot. It is a no-op when the optimizer is not active. A forced run requested
// while one is in progress runs once that one finishes.
func (site *Site) optimizerUpdateAsync(minAge time.Duration) {
	if !sponsor.IsAuthorized() || !optimizerEnabled() {
		return
	}

	site.optimizerExclusive(minAge == 0, func(force bool) {
		if force {
			minAge = 0
		}
		site.optimizerRun(minAge)
	})
}

// optimizerExclusive calls run under optimizerMu, skipping it while another run is
// in progress. A forced request that finds one in progress is kept: that run may
// have read its inputs before the change, so run is called again, forced, after it.
func (site *Site) optimizerExclusive(force bool, run func(force bool)) {
	for {
		if !site.optimizerMu.TryLock() {
			if force {
				site.optimizerRerun.Store(true)
			}
			return
		}
		site.optimizerRerun.Store(false)
		run(force)
		site.optimizerMu.Unlock()

		if !site.optimizerRerun.CompareAndSwap(true, false) {
			return
		}
		force = true
	}
}

// optimizerRun runs the optimizer unless minAge says it is not due. Called under optimizerMu.
func (site *Site) optimizerRun(minAge time.Duration) {
	if minAge == 0 {
		// keep the gate open so a not-ready run is retried on the next cycle
		site.optimizerUpdated = time.Time{}
	} else if !optimizerDue(site.optimizerUpdated, time.Now(), minAge) {
		return
	}

	var err error

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic %v", r)
		}

		// not ready yet: keep the gate open for an immediate retry next cycle
		if errors.Is(err, errOptimizerNotReady) {
			return
		}

		site.optimizerUpdated = time.Now()

		if err != nil {
			site.log.ERROR.Println("optimizer:", err)

			// stale advice must not linger
			site.clearSuggestions()
		}
	}()

	err = site.optimizerUpdate(site.battery.Devices)
}

// optimizerRequest assembles the optimizer request and the matching device
// details from tariffs, home profile, loadpoints and battery meters
func (site *Site) optimizerRequest(battery []types.Measurement) (optimizer.OptimizationInput, requestDetails, error) {
	var req optimizer.OptimizationInput
	var details requestDetails

	solarTariff := site.GetTariff(api.TariffUsageSolar)
	solar := currentRates(solarTariff)

	// value import at its realized NEM3 marginal cost: for a net-generation exporter
	// the generation half of an import voids surplus at NSC, not retail, so full
	// retail overstates import cost and the TOU arbitrage spread (see realizedImport).
	// Optimizer-only: the published/persisted grid price stays the real retail rate.
	grid := site.realizedImportRates(currentRates(site.GetTariff(api.TariffUsageGrid)))
	// value export at its realized NEM3 worth (delivery EEC + net-position/NSC
	// generation), the single export rate also shown in the UI and written back to
	// the Powerwall — see realizedFeedInRates.
	feedIn := site.realizedFeedInRates()

	minLen := lo.Min([]int{len(grid), len(feedIn)})
	// exclude empty solar forecast from minLen
	if solarTariff != nil && len(solar) > 0 {
		minLen = min(minLen, len(solar))
	}

	// the horizon of the hosted service, which the in-process optimizer shares
	if uri := optimizerURI(); uri == "" || uri == OPTIMIZER_URI {
		minLen = slotsUntil(grid, optimizerHorizon(time.Now()), minLen)
	}

	if expectedSlots := 8; minLen < expectedSlots {
		if solarTariff != nil {
			return req, details, fmt.Errorf("not enough forecast slots for meaningful optimization: %d < %d (grid=%d, feedIn=%d, solar=%d)", minLen, expectedSlots, len(grid), len(feedIn), len(solar))
		}
		return req, details, fmt.Errorf("not enough forecast slots for meaningful optimization: %d < %d (grid=%d, feedIn=%d)", minLen, expectedSlots, len(grid), len(feedIn))
	}

	now := time.Now()
	dt := timeSteps(minLen, now)
	firstSlotDuration := time.Duration(dt[0]) * time.Second

	site.log.DEBUG.Printf("optimizer: optimizing %d slots until %v: grid=%d, feedIn=%d, solar=%d, first slot: %v",
		minLen,
		grid[minLen-1].End.Local(),
		len(grid), len(feedIn), len(solar),
		firstSlotDuration,
	)

	gt, err := site.homeProfile(minLen)
	if err != nil {
		return req, details, err
	}

	// blend measured energy of the last metrics slot into the first slots
	if v := site.measuredSlotEnergy(metrics.Home); v > 0 {
		orig := slices.Clone(gt[:min(optimizerDecaySlots, len(gt))])
		blendMeasured(gt, v, optimizerDecaySlots)
		site.log.DEBUG.Printf("optimizer: home slots updated with measured %.0fWh: %.0f -> %.0f", v, orig, gt[:len(orig)])
	}

	// allow empty solar forecast
	ft := lo.RepeatBy(minLen, func(i int) float32 { return float32(0) })
	if solarTariff != nil && len(solar) > 0 {
		solarEnergy, err := solarRatesToEnergy(solar)
		if err != nil {
			return req, details, err
		}

		scale := site.effectiveSolarScale()
		ftSlots := scaleAndPrune(solarEnergy, scale, minLen)

		// decay the scale derived from measured vs forecasted energy of the last completed slot
		if pv, fcst := site.measuredSlotEnergy(site.Meters.PVMetersRef...), site.measuredSlotEnergy(metrics.Forecast)*scale; pv > 0 && fcst > 0 {
			orig := slices.Clone(ftSlots[:min(optimizerDecaySlots, len(ftSlots))])
			blendScale(ftSlots, pv/fcst, optimizerDecaySlots)
			site.log.DEBUG.Printf("optimizer: pv slots updated with scale %.2f: %.0f -> %.0f", pv/fcst, orig, ftSlots[:len(orig)])
		}
		ft = prorate(ftSlots, firstSlotDuration)
	}

	req = optimizer.OptimizationInput{
		Strategy: optimizer.OptimizerStrategy{
			ChargingStrategy:    site.optimizerRequestChargingStrategy(),
			DischargingStrategy: optimizer.OptimizerStrategyDischargingStrategyDischargeBeforeImport,
		},
		EtaC: eta,
		EtaD: eta,
		TimeSeries: optimizer.TimeSeries{
			Dt: dt,
			Gt: prorate(gt, firstSlotDuration),
			Ft: ft,
			PN: scaleAndPrune(grid, 0.001, minLen),
			PE: scaleAndPrune(feedIn, 0.001, minLen),
		},
	}

	// end of horizon Wh value
	pa := lo.Min(req.TimeSeries.PN) * eta * 0.99

	details = requestDetails{
		Timestamps: asTimestamps(dt, now),
	}

	if site.circuit != nil {
		if pMaxImp := site.circuit.GetMaxPower(); pMaxImp > 0 {
			// hard grid import limit if no price penalty is set by PrcPExcImp
			req.Grid.PMaxImp = float32(pMaxImp)
		}
	}

	// self-consumption is no longer forced with a near-zero import cap. On true
	// prices it falls out of cost minimization (covering the home from the battery
	// beats importing whenever import > export). The case a cap used to force -
	// keeping enough battery for the home instead of dumping it at an export peak -
	// no longer arises now that export is valued at its realized worth (~NSC, see
	// realizedFeedInRates): the pack exports only when that genuinely beats holding,
	// so no blanket reserve is needed (see batteryRequest). Only a real physical
	// import limit (the circuit cap above) sets PMaxImp now; ordinary home import is
	// priced at the true grid price.

	// static grid export limit configured in the UI: export is capped at this
	// power, excess PV is curtailed instead of exported
	if limit := site.GetGridExportLimit(); limit > 0 {
		req.Grid.PMaxExp = float32(limit)
	}

	// soft grid feed-in cap from active HEMS curtailment (e.g. German 70% rule)
	// wins over the static limit while active
	if curtailed := hems.Curtailed(site.hems); curtailed != nil && *curtailed {
		if pMaxExp := site.hems.MaxProductionPower(); pMaxExp != nil {
			req.Grid.PMaxExp = float32(*pMaxExp)
		}
	}

	var batteries []optimizerBattery

	// uncontrollable power of loadpoints that cannot be modelled as storage
	var unmodelled float64

	for id, lp := range site.ActiveLoadpoints() {
		// no vehicle capacity and no session energy limit to model against: account for
		// the consumption as uncontrollable load. A disconnected loadpoint with no default
		// vehicle also lands here (nil vehicle) and draws ~0W.
		if v := lp.GetVehicle(); v == nil || (v.Capacity() == 0 && lp.GetLimitEnergy() == 0) {
			unmodelled += unmodelledPower(lp)
			continue
		}

		// skip disabled loadpoints (ModeOff sets CMax to 0)
		cfg, detail := site.loadpointRequest(lp, minLen, firstSlotDuration, grid)
		if cfg.CMax <= 0 {
			continue
		}

		// gate the per-slot charge power by when the car is (predicted to be) plugged in, so
		// the plan charges while the car is actually connected rather than deferring to a
		// theoretical future slot. The car is always modelled now, connected or not, so its
		// plan always renders; unavailable slots get CMaxT 0.
		cfg.CMaxT = site.vehicleAvailability(id, lp, cfg.CMax, minLen)

		// while the car is away it drains, so start the plan's reconnect charging from the
		// expected post-drive SoC rather than the frozen disconnect SoC.
		site.applyConsumption(&cfg, lp)

		detail.loadpoint = &id
		batteries = append(batteries, optimizerBattery{cfg, detail})
	}

	// home profile subtracts all loadpoint power, so unmodelled loadpoints would
	// leave the optimizer planning against surplus that is already consumed. Their
	// forecast is zero, so the measured power only decays into the near slots -
	// without a capacity there is no fill point to assert it any further.
	if unmodelled > 0 {
		load := make([]float64, minLen)
		blendMeasured(load, unmodelled/slotsPerHour, optimizerDecaySlots)

		site.log.DEBUG.Printf("optimizer: home slots updated with unmodelled %.0fW loadpoint load: %.0f", unmodelled, load[:min(optimizerDecaySlots, len(load))])

		for i, v := range prorate(load, firstSlotDuration) {
			req.TimeSeries.Gt[i] += v
		}
	}

	if err := site.batteriesReported(battery); err != nil {
		return req, details, err
	}

	for i, dev := range site.batteryMeters {
		// measurements may lag the configured meters on an off-cycle trigger
		if i >= len(battery) {
			break
		}
		b := battery[i]

		if b.Capacity == nil || *b.Capacity == 0 || b.Soc == nil {
			continue
		}

		cfg, detail := site.batteryRequest(dev, b, grid, minLen, firstSlotDuration)
		batteries = append(batteries, optimizerBattery{cfg, detail})

		// the pack's own standby comes out of storage, unseen by the battery meter and so
		// missing from the home profile: the pack covers it while it has charge, PV while it
		// charges, the grid (topping up to reserve) when it sits at the floor
		if m, ok := site.batteryEfficiencyModel(dev.Config().Name, *b.Capacity); ok && m.Standby > 0 {
			addStandby(&req.TimeSeries, m.Standby)
			site.log.DEBUG.Printf("optimizer: home slots updated with %.0fW %s standby", m.Standby, dev.Config().Name)
		}
	}

	// all batteries value leftover stored energy at pa (~the cheapest grid price). The EV is no longer
	// boosted above this: keeping a plugged-in car charged is handled by its forecast-driving
	// commitment (s_drain_t, see applyConsumption), not by making stored energy look valuable - so the
	// car charges exactly for its forecast trips at the cheapest times, instead of filling toward full
	// off cheap-but-unneeded grid the way the elevated end-of-horizon value used to pull it.
	for _, b := range batteries {
		b.cfg.PA = pa
		req.Batteries = append(req.Batteries, b.cfg)
		details.BatteryDetails = append(details.BatteryDetails, b.detail)
	}

	return req, details, nil
}

// addStandby adds a constant draw (W) to every home slot as energy (Wh)
func addStandby(ts *optimizer.TimeSeries, standby float64) {
	for t, dt := range ts.Dt {
		ts.Gt[t] += float32(standby * float64(dt) / 3600)
	}
}

func (site *Site) optimizerUpdate(battery []types.Measurement) error {
	// stamp the input snapshot time before building the request: a suggestion
	// computed before a car connected does not describe the connected loadpoint
	// (see loadpointOptimizerAction).
	computedAt := time.Now()

	req, details, err := site.optimizerRequest(battery)
	if err != nil {
		return err
	}

	if len(req.Batteries) == 0 {
		// meters configured but measurements not in yet: retry instead of
		// consuming the slot gate
		if len(site.batteryMeters) > 0 {
			return errOptimizerNotReady
		}
		return nil // nothing to optimize
	}

	res, err := site.optimize(req)
	if err != nil {
		return err
	}

	// publish before the status check so the optimizer page stays available
	// for diagnosing non-optimal results
	site.publish("evopt", optimizerResult{
		Updated: time.Now(),
		Req:     req,
		Res:     res,
		Details: details,
	})

	// feasible results are usable, they are just not proven optimal
	if status := res.Status; status != optimizer.Optimal && status != optimizer.Feasible {
		return errors.New(string(status))
	}

	site.applyOptimizerResult(req, details.BatteryDetails, res, computedAt)

	return nil
}

// optimize solves the request with the optimizer service configured with OPTIMIZER_URI, or in
// process with the port of that service when none is configured
func (site *Site) optimize(req optimizer.OptimizationInput) (optimizer.OptimizationResult, error) {
	if uri := optimizerURI(); uri != "" {
		ctx, cancel := context.WithTimeout(context.Background(), optimizerTimeout)
		defer cancel()
		return site.optimizeRemote(ctx, uri, req)
	}

	ctx, cancel := context.WithTimeout(context.Background(), optimizerTimeLimit)
	defer cancel()
	return evopt.Optimize(ctx, req, evopt.WithoutProbe())
}

// optimizeRemote posts the request to the optimizer service at uri
func (site *Site) optimizeRemote(ctx context.Context, uri string, req optimizer.OptimizationInput) (optimizer.OptimizationResult, error) {
	httpClient := request.NewClient(site.log)
	httpClient.Timeout = optimizerTimeout

	apiClient, err := optimizer.NewClientWithResponses(uri, optimizer.WithHTTPClient(httpClient))
	if err != nil {
		return optimizer.OptimizationResult{}, err
	}

	resp, err := apiClient.PostOptimizeChargeScheduleWithResponse(ctx, req, func(_ context.Context, req *http.Request) error {
		if sponsor.IsAuthorized() {
			req.Header.Set("Authorization", "Bearer "+sponsor.Token)
		}
		return nil
	})
	if err != nil {
		return optimizer.OptimizationResult{}, err
	}

	if resp.StatusCode() != http.StatusOK {
		return optimizer.OptimizationResult{}, apiError(resp)
	}

	return *resp.JSON200, nil
}

// gridImportAt returns the total grid import at slot j in Wh: the regular
// grid_import plus grid_import_overshoot. When a grid import limit (Grid.PMaxImp)
// is set and the schedule has to exceed it, the optimizer books the excess into
// grid_import_overshoot instead of grid_import. Reading grid_import alone then
// under-reads the actual import - reporting "not importing" while the home is in
// fact drawing from the grid - which mis-drives the battery suggestion and
// understates the planned grid flow.
func gridImportAt(res optimizer.OptimizationResult, j int) float32 {
	var v float32
	if j < len(res.GridImport) {
		v = res.GridImport[j]
	}
	if j < len(res.GridImportOvershoot) {
		v += res.GridImportOvershoot[j]
	}
	return v
}

// chosenImportAt returns slot j's grid import in W, net of the import the optimizer
// had no discretion over because a battery it charged was pinned at its minimum
// charge power.
//
// c_min is indivisible: a battery the plan charges draws at least that much or
// nothing at all. Where the minimum exceeds the slot's solar surplus, the difference
// has to come from the grid - the plan did not choose to import, the minimum chose
// for it. Reading the raw import as "the plan imports" then classifies the car's
// charge source as grid rather than surplus, which is circular: the car's own
// minimum manufactures the import that reclassifies the car as grid-fed, and
// sourceGrid makes PV mode hold the car at that minimum regardless of the surplus
// actually available (see optimizerFastCharge in loadpoint.go).
//
// Import beyond the forced part is a genuine choice and still reads as import, so a
// slot that pushes a battery above its minimum - or that imports with nothing
// charging at all - is unaffected.
func chosenImportAt(req optimizer.OptimizationInput, res optimizer.OptimizationResult, j int) float64 {
	if j < 0 || j >= len(req.TimeSeries.Dt) {
		return 0
	}
	slotHours := (time.Duration(req.TimeSeries.Dt[j]) * time.Second).Hours()
	if slotHours <= 0 {
		return 0
	}

	// solar left for the batteries after the home load
	var surplus float64
	if j < len(req.TimeSeries.Ft) && j < len(req.TimeSeries.Gt) {
		surplus = max(0, float64(req.TimeSeries.Ft[j]-req.TimeSeries.Gt[j]))
	}

	// the indivisible part of what the plan charges in this slot
	var atMinimum float64
	for i, b := range req.Batteries {
		if b.CMin <= 0 || i >= len(res.Batteries) {
			continue
		}
		charge := res.Batteries[i].ChargingPower
		if j >= len(charge) || charge[j] <= 0 {
			continue
		}
		atMinimum += min(float64(charge[j]), float64(b.CMin)*slotHours)
	}

	forced := max(0, atMinimum-surplus)

	return max(0, float64(gridImportAt(res, j))-forced) / slotHours
}

// applyOptimizerResult maps the optimizer response onto suggestions, battery
// forecast and notifications
func (site *Site) applyOptimizerResult(req optimizer.OptimizationInput, details []batteryDetail, res optimizer.OptimizationResult, computedAt time.Time) {
	dt := req.TimeSeries.Dt
	slotHours := (time.Duration(dt[0]) * time.Second).Hours()
	gridImporting := gridImportAt(res, 0) > 0
	gridExporting := len(res.GridExport) > 0 && res.GridExport[0] > 0

	// the near-zero import cap is gone (self-consumption comes from economics + the
	// load-aware reserve, see optimizerRequest), so there is no no-import mode to
	// mirror here: a HOLD suggestion is left as HOLD.
	noGridImport := false

	// export that exceeds solar in a slot must come from the battery (barely any
	// from pass-through import): the direct "battery dumped to grid" signal. If
	// this is ~0, the battery is only serving the home and all export is solar.
	var battToGrid float32
	for i, e := range res.GridExport {
		if i < len(req.TimeSeries.Ft) && e > req.TimeSeries.Ft[i] {
			battToGrid += e - req.TimeSeries.Ft[i]
		}
	}

	// visibility into the planned grid flows. Ft = solar forecast, Gt = home load.
	// battToGrid = export exceeding solar = battery discharged to grid.
	site.log.DEBUG.Printf("optimizer grid: slot=%d solar=%.0fWh load=%.0fWh import=%.0fWh export=%.0fWh battToGrid=%.0fWh",
		len(req.TimeSeries.Gt),
		lo.Sum(req.TimeSeries.Ft), lo.Sum(req.TimeSeries.Gt),
		lo.Sum(res.GridImport)+lo.Sum(res.GridImportOvershoot), lo.Sum(res.GridExport), battToGrid)

	// raw prices the optimizer sees (undo the internal ×0.001 scaling). If export
	// (PE) shows near the import (PN) range, the feed-in tariff is misconfigured —
	// which would (wrongly) make dumping the battery look profitable.
	site.log.DEBUG.Printf("optimizer prices: import=%.3g..%.3g export=%.3g..%.3g",
		lo.Min(req.TimeSeries.PN)*1000, lo.Max(req.TimeSeries.PN)*1000,
		lo.Min(req.TimeSeries.PE)*1000, lo.Max(req.TimeSeries.PE)*1000)

	// battery-mode and suggestion plan slots are keyed by their end time, so the
	// control loop applies the slot in effect regardless of when the optimizer last
	// ran. Derived from the request's first slot rather than the clock after the
	// solve, which may already have crossed into the next slot.
	firstBoundary := planFirstBoundary(computedAt, dt)

	var batteries []batteryResult
	plans := make(map[string][]suggestionSlot, len(req.Batteries))
	schedules := make(map[string][]batterySlot)

	for i, batReq := range req.Batteries {
		batRes := res.Batteries[i]
		detail := details[i]

		batteries = append(batteries, batteryResult{
			batteryDetail: detail,
			Full: matchSoc(batRes.StateOfCharge, func(soc float32) bool {
				return soc >= batReq.SMax
			}),
			Empty: matchSoc(batRes.StateOfCharge, func(soc float32) bool {
				return soc <= batReq.SMin
			}),
		})

		key := detail.key()
		if key == "" || !detail.controllable {
			// uncontrollable devices can't act on a suggestion
			continue
		}

		plans[key] = suggestionPlan(i, req, res, details, firstBoundary, noGridImport)

		// battery-mode plan (slot end + action) so the control loop front-runs
		// export-mode transitions independent of the optimizer cadence. Home
		// battery only (the ~2min autonomous pickup is PW3-specific).
		if detail.Type == batteryTypeBattery {
			schedules[key] = batteryModePlan(detail, batReq, batRes, res, dt, firstBoundary, noGridImport)
		}
	}

	site.publish("evopt-batteries", batteries)

	// log the current-slot suggestions so it's visible what the optimizer wants
	site.log.DEBUG.Printf("optimizer suggestions: gridImport=%t (chosen %.0fW of %.0fW) gridExport=%t",
		gridImporting, chosenImportAt(req, res, 0), float64(gridImportAt(res, 0))/slotHours, gridExporting)
	for key, plan := range plans {
		if s, ok := currentSuggestion(plan, computedAt); ok {
			site.log.DEBUG.Printf("optimizer %q: %s (charge %.0fW discharge %.0fW)", key, s.Action, s.Charge, s.Discharge)
		}
	}

	site.setSuggestionPlans(plans)
	site.Lock()
	site.suggestionsComputedAt = computedAt
	site.Unlock()
	site.setBatterySchedules(schedules)
	site.battery.Forecast = site.addBatteryForecastTotals(req.Batteries, res.Batteries)

	site.publishBattery()

	// publish for all loadpoints so suggestions of dropped-out loadpoints clear
	site.publishSuggestions()

	// The optimizer runs asynchronously, so a just-computed suggestion would
	// otherwise wait for the next control tick (up to the site interval) to be
	// applied - notably a connect-triggered run would not charge until then. Poke
	// the modelled loadpoints so the fresh suggestion is consumed promptly.
	for _, detail := range details {
		if detail.loadpoint == nil {
			continue
		}
		if lp := site.loadpoints[*detail.loadpoint]; lp != nil {
			lp.requestUpdate()
		}
	}

	// notify on actionable suggestion changes (advisory only, see #31903)
	for _, ev := range site.diffSuggestions(site.pendingSuggestions(details)) {
		site.pushEvent(ev)
	}
}

// planFirstBoundary returns the end of the request's first slot, which starts
// at computedAt and lasts dt[0] seconds (a full slot when computedAt sits on a
// boundary, see timeSteps).
func planFirstBoundary(computedAt time.Time, dt []int) time.Time {
	if len(dt) == 0 {
		return computedAt.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	}
	return computedAt.Add(time.Duration(dt[0]) * time.Second).Round(tariff.SlotDuration)
}

// suggestionPlan builds device i's per-slot suggestions for the first
// frontRunScheduleSlots slots of the result. Slot j ends at
// firstBoundary+j·slot. A slot without an action is kept so it resolves to no
// suggestion rather than falling through to a later slot.
func suggestionPlan(i int, req optimizer.OptimizationInput, res optimizer.OptimizationResult, details []batteryDetail, firstBoundary time.Time, noGridImport bool) []suggestionSlot {
	if i < 0 || i >= len(res.Batteries) || i >= len(details) {
		return nil
	}
	detail := details[i]
	batRes := res.Batteries[i]
	dt := req.TimeSeries.Dt

	n := min(len(batRes.ChargingPower), len(dt), frontRunScheduleSlots)
	plan := make([]suggestionSlot, 0, n)
	for j := range n {
		slotHours := (time.Duration(dt[j]) * time.Second).Hours()
		exporting := len(res.GridExport) > j && res.GridExport[j] > 0

		// A car pinned at its own minimum charge power is not grid-fed just because
		// that minimum outran the surplus (see chosenImportAt). The home battery keeps
		// the raw reading: its reserve, not this flag, is what bounds its discharge.
		importing := gridImportAt(res, j) > 0
		if importing && detail.Type != batteryTypeBattery {
			importing = chosenImportAt(req, res, j) > suggestionThreshold
		}

		s := currentSlotSuggestion(detail, batRes, j, importing, exporting, noGridImport, slotHours)
		if detail.Type == batteryTypeVehicle && s.Action == actionCharge {
			s.Source = evChargeSource(s.Source, homeDischargeAt(res, details, j, slotHours) > suggestionThreshold, exporting)
		}

		plan = append(plan, suggestionSlot{
			until:      firstBoundary.Add(time.Duration(j) * tariff.SlotDuration),
			suggestion: s,
		})
	}
	return plan
}

// homeDischargeAt returns the home batteries' aggregate discharge in slot j (W).
// If the pack is discharging while the car charges (and nothing is imported or
// exported), the car is being fed from the battery, not from real-time PV
// surplus, so PV mode must charge at the planned power instead of deferring to
// measured export.
func homeDischargeAt(res optimizer.OptimizationResult, details []batteryDetail, j int, slotHours float64) float64 {
	if slotHours <= 0 {
		return 0
	}
	var sum float64
	for i, detail := range details {
		if detail.Type != batteryTypeBattery || i >= len(res.Batteries) {
			continue
		}
		if d := res.Batteries[i].DischargingPower; j < len(d) {
			sum += float64(d[j]) / slotHours
		}
	}
	return sum
}

// batteryModePlan builds the per-slot battery-mode plan (slot end time + mode
// action) that the control loop resolves each cycle to front-run export-mode
// transitions. Slot j spans [firstBoundary+(j-1)·slot, firstBoundary+j·slot];
// slot 0 is the current partial slot ending at firstBoundary. A HOLD slot that
// opens onto an export slot is relabeled to HoldCharge so a within-peak lull
// doesn't drop the Powerwall out of autonomous (see the relabel loop below).
func batteryModePlan(detail batteryDetail, batReq optimizer.BatteryConfig, batRes optimizer.BatteryResult, res optimizer.OptimizationResult, dt []int, firstBoundary time.Time, noGridImport bool) []batterySlot {
	n := min(len(batRes.ChargingPower), len(dt), frontRunScheduleSlots)
	plan := make([]batterySlot, 0, n)
	for j := 0; j < n; j++ {
		importing := gridImportAt(res, j) > 0
		exporting := len(res.GridExport) > j && res.GridExport[j] > 0
		slotHours := (time.Duration(dt[j]) * time.Second).Hours()
		action := currentSlotSuggestion(detail, batRes, j, importing, exporting, noGridImport, slotHours).Action
		until := firstBoundary.Add(time.Duration(j) * tariff.SlotDuration)
		startSoc := slotSocPercent(batReq, batRes, j-1)
		if j == 0 && batReq.SCapacity > 0 {
			startSoc = float64(batReq.SInitial) / float64(batReq.SCapacity) * 100
		}
		plan = append(plan, batterySlot{until: until, action: action, soc: slotSocPercent(batReq, batRes, j), startSoc: startSoc})
	}

	// Bridge a within-peak lull: a HOLD slot that opens onto an export slot is a
	// gap where the pack already met this slot's discharge target, so the residual
	// house load reads as a small grid import and the slot classifies idle+import →
	// HOLD. Left as HOLD it maps to PW3 backup mode + the real tariff, which drops
	// the export spike, serves the house from the grid, and re-incurs the ~2min
	// autonomous pickup lag when the next export slot resumes. Relabel it to
	// HoldCharge (autonomous + sell spike + pv_only): the pack covers the house
	// from the battery instead of the grid and the Powerwall never leaves
	// autonomous across the peak. Only the slot abutting the export is bridged, so
	// a genuine longer hold ahead of a peak still preserves the pack.
	for j := 0; j+1 < len(plan); j++ {
		if plan[j].action == api.BatteryHold.String() && isSpikeAction(plan[j+1].action) {
			plan[j].action = api.BatteryHoldCharge.String()
		}
	}

	return plan
}

// slotSocPercent converts the optimizer's planned end-of-slot SoC (Wh) for slot j
// to a percentage of the battery capacity, 0 when capacity or the slot is unknown.
func slotSocPercent(batReq optimizer.BatteryConfig, batRes optimizer.BatteryResult, j int) float64 {
	if batReq.SCapacity <= 0 || j < 0 || j >= len(batRes.StateOfCharge) {
		return 0
	}
	return float64(batRes.StateOfCharge[j]) / float64(batReq.SCapacity) * 100
}

func (site *Site) addBatteryForecastTotals(req []optimizer.BatteryConfig, resp []optimizer.BatteryResult) *types.BatteryForecast {
	if len(resp) == 0 || len(resp[0].StateOfCharge) == 0 {
		return nil
	}

	high, low := batteryForecastSocExtremes(req, resp)
	if high == nil && low == nil {
		return nil
	}

	cutoff := time.Now()
	now := cutoff.Round(tariff.SlotDuration)
	point := func(p *batteryForecastSlot) *types.BatteryForecastPoint {
		if p == nil {
			return nil
		}
		ts := now.Add(time.Duration(p.slot) * tariff.SlotDuration)
		if !ts.After(cutoff) {
			return nil
		}
		return &types.BatteryForecastPoint{Soc: p.soc, Time: ts, Limit: p.limit}
	}

	res := types.BatteryForecast{
		Highest: point(high),
		Lowest:  point(low),
	}
	if res.Highest == nil && res.Lowest == nil {
		return nil
	}
	return &res
}

type batteryForecastSlot struct {
	slot  int
	soc   float64 // percent
	limit bool    // true when SMax (highest) or SMin (lowest) boundary reached
}

// batteryForecastSocExtremes returns the highest and lowest aggregate SOC
// points across home batteries (SCapacity > 0) over the forecast horizon.
// The Limit flag indicates whether the SOC reached the configured SMax (for
// the highest point) or SMin (for the lowest point) boundary - in which case
// the battery is forecasted to become fully charged or empty.
// Returns nil for either point when no home battery is present or when the
// battery already is at the respective limit.
func batteryForecastSocExtremes(req []optimizer.BatteryConfig, resp []optimizer.BatteryResult) (*batteryForecastSlot, *batteryForecastSlot) {
	homeIndices := lo.FilterMap(req, func(b optimizer.BatteryConfig, i int) (int, bool) {
		return i, b.SCapacity > 0
	})
	if len(homeIndices) == 0 || len(resp) == 0 {
		return nil, nil
	}

	totalCapacity := lo.SumBy(homeIndices, func(i int) float32 { return req[i].SCapacity })
	totalSMax := lo.SumBy(homeIndices, func(i int) float32 { return req[i].SMax })
	totalSMin := lo.SumBy(homeIndices, func(i int) float32 { return req[i].SMin })

	var high, low *batteryForecastSlot
	for i := range resp[homeIndices[0]].StateOfCharge {
		sum := lo.SumBy(homeIndices, func(idx int) float32 { return resp[idx].StateOfCharge[i] })
		soc := float64(sum/totalCapacity) * 100
		fullReached := totalSMax > 0 && sum >= totalSMax
		emptyReached := sum <= totalSMin

		// first slot at SMax wins for highest
		if high == nil || (!high.limit && (soc > high.soc || fullReached)) {
			high = &batteryForecastSlot{slot: i, soc: soc, limit: fullReached}
		}
		// first slot at SMin wins for lowest
		if low == nil || (!low.limit && (soc < low.soc || emptyReached)) {
			low = &batteryForecastSlot{slot: i, soc: soc, limit: emptyReached}
		}
	}

	// battery is already at the limit - announcing it will become full/empty is pointless
	if high != nil && high.limit && high.slot == 0 {
		high = nil
	}
	if low != nil && low.limit && low.slot == 0 {
		low = nil
	}

	return high, low
}

func (site *Site) loadpointRequest(lp loadpoint.API, minLen int, firstSlotDuration time.Duration, grid api.Rates) (optimizer.BatteryConfig, batteryDetail) {
	bat := optimizer.BatteryConfig{
		ChargeFromGrid: true,
		CMin:           float32(lp.EffectiveMinPower()),
		CMax:           float32(lp.EffectiveMaxPower()),
		DMax:           0,
		SMin:           0,
		// PA:             pa,

		// a running session continues rather than counting as a fresh start, so the
		// plan favors keeping it on over stopping it to restart later
		Charging: lp.GetStatus() == api.StatusC,
	}

	if profile := loadpointProfile(lp, minLen); profile != nil {
		bat.PDemand = prorate(profile, firstSlotDuration)
	}

	detail := batteryDetail{
		Type:         batteryTypeLoadpoint,
		Title:        lp.GetTitle(),
		controllable: true,
	}

	// vehicle
	v := lp.GetVehicle()

	capacity := v.Capacity() // kWh
	soc := lp.GetSoc()       // percent

	// without capacity or soc there is no battery state to model, but a session energy
	// limit still bounds the charge- use charged energy as state (see remainingLimitEnergy)
	if limit := lp.GetLimitEnergy(); limit > 0 && (capacity == 0 || soc == 0) {
		bat.SInitial = float32(lp.GetChargedEnergy())    // Wh
		bat.SMax = max(bat.SInitial, float32(limit*1e3)) // prevent infeasible if limit already exceeded
	} else {
		maxSoc := capacity * float64(lp.EffectiveLimitSoc()) * 10 // Wh
		bat.SInitial = float32(capacity * soc * 10)               // Wh
		bat.SMax = max(bat.SInitial, float32(maxSoc))             // prevent infeasible if current soc above maximum

		// hold the configured minimum SoC as a floor so the plan keeps the car above it instead of
		// coasting toward empty between trips (the optimizer otherwise only charges for forecast
		// driving). It is a soft L1 reserve, so a car below it stays feasible and is charged up to
		// it as soon as it can be, like evcc's min soc fast charge.
		if minSoc := lp.EffectiveMinSoc(); minSoc > 0 {
			bat.SMin = float32(capacity * float64(minSoc) * 10) // Wh
		}
	}

	detail.Type = batteryTypeVehicle
	detail.Capacity = capacity

	if vt := v.GetTitle(); vt != "" {
		if detail.Title != "" {
			detail.Title += " (" + vt + ")"
		} else {
			detail.Title = vt
		}
	}

	// find vehicle name/id
	for _, dev := range config.Vehicles().Devices() {
		if dev.Instance() == v {
			detail.Name = dev.Config().Name
		}
	}

	var demand []float32

	switch lp.GetMode() {
	case api.ModeOff:
		// disable charging
		bat.CMax = 0

	case api.ModeNow:
		// forced max charging
		demand = continuousDemand(lp, minLen)

	case api.ModeMinPV:
		// forced min charging
		demand = continuousDemand(lp, minLen)
		// add smartcost limit, precondition and plan goal, if configured
		demand = applySmartCostLimit(lp, demand, grid, minLen)
		demand = applyPrecondition(lp, demand, minLen)
		site.applyPlanGoal(lp, &bat, minLen)

	case api.ModePV:
		// add smartcost limit, precondition and plan goal, if configured
		demand = applySmartCostLimit(lp, nil, grid, minLen)
		demand = applyPrecondition(lp, demand, minLen)
		site.applyPlanGoal(lp, &bat, minLen)
	}

	if demand != nil {
		// after prorate, so the shortened first slot counts with the energy it really carries
		bat.PDemand = clearDemandWhenFull(prorate(demand, firstSlotDuration), bat.SMax-bat.SInitial)
	}

	bat.CPriority = site.evChargePriority()

	return bat, detail
}

// clearDemandWhenFull zeroes the charge demand from the slot the accumulated energy fills the
// vehicle. The optimizer drops the demand at s_max anyway, but pays two binaries per slot to
// detect it, so slots that cannot bind are worth not asking about. Losses are accounted for.
//
// The cut assumes the demand is met every slot. A grid import limit can throttle charging below
// it, moving the real fill point later than the estimate - the next request corrects that from
// the measured soc, and the near slots are never affected because the cut sits a full charge away.
func clearDemandWhenFull(demand []float32, headroom float32) []float32 {
	res := slices.Clone(demand)

	var acc float32
	for i, d := range res {
		if acc >= headroom {
			res[i] = 0
			continue
		}
		acc += d * eta
	}

	return res
}

// evChargePriority returns the optimizer charge priority for the EV. Under
// charge_before_export it is the maximum (2) so the optimizer's (T-t)-weighted
// early-charge reward (optimizer.py:304) biases the EV to charge from solar
// sooner rather than deferring across the multi-day horizon — PA (stored-energy
// value) is realized only at horizon end, so charging is otherwise
// timing-neutral. It is a soft nudge: the reward is far below the grid price, so
// it never forces grid charging, only tips otherwise-cost-neutral surplus
// charging earlier.
func (site *Site) evChargePriority() int {
	if site.optimizerChargeFirst() {
		return 2
	}
	return 0
}

// optimizerRequestChargingStrategy maps the selected secondary goal onto the optimizer's
// charging strategy: serve home first is evcc's own goal, carried by the batteries'
// serve_home_first flag, so the optimizer itself gets none.
func (site *Site) optimizerRequestChargingStrategy() optimizer.OptimizerStrategyChargingStrategy {
	if site.optimizerServeHomeFirst() {
		return optimizer.OptimizerStrategyChargingStrategyNone
	}
	return optimizer.OptimizerStrategyChargingStrategy(site.GetOptimizerChargingStrategy())
}

// optimizerServeHomeFirst reports whether the serve home first secondary goal is selected.
func (site *Site) optimizerServeHomeFirst() bool {
	return site.GetOptimizerChargingStrategy() == optimizerStrategyServeHomeFirst
}

// optimizerChargeFirst reports whether the charge-before-export ("battery first")
// strategy is active.
func (site *Site) optimizerChargeFirst() bool {
	return site.GetOptimizerChargingStrategy() == string(optimizer.OptimizerStrategyChargingStrategyChargeBeforeExport)
}

func (site *Site) batteryRequest(dev config.Device[api.Meter], b types.Measurement, grid api.Rates, minLen int, firstSlotDuration time.Duration) (optimizer.BatteryConfig, batteryDetail) {
	bat := optimizer.BatteryConfig{
		CMax:      batteryPower,
		DMax:      batteryPower,
		SCapacity: float32(*b.Capacity * 1e3),         // Wh
		SInitial:  float32(*b.Capacity * *b.Soc * 10), // Wh
		// PA:       pa,
		// No load-aware backup reserve. It was a workaround for the overvalued
		// sell_tariff: it fenced the home's overnight energy off so the pack would not
		// dump it at a fake export peak and re-import. But it self-sizes to the whole
		// night at dusk, which pins the pack and forces the home to IMPORT through the
		// evening peak while the reserve is drained overnight (cheap) - the opposite of
		// what we want. With export now valued at its realized worth (~NSC, see
		// realizedFeedInRates) self-consumption falls out of cost minimization on its
		// own: the pack discharges at the expensive peak and re-imports overnight when
		// cheap, exporting only when export genuinely beats that (a VPP event, or a
		// summer-evening delivery-EEC peak). MinSoc still floors backup. Defaults false.

		// Serve home first (a secondary goal): the home self-consumes ahead of money. The
		// optimizer minimizes grid import not charging the car before cost, so the pack keeps
		// what the house needs until it can refill - a grid outage never finds it empty for
		// want of energy sold to a VPP event or given to the car - and only the surplus goes
		// to export or the car.
		ServeHomeFirst: site.optimizerServeHomeFirst(),
	}

	// measured efficiencies, so the plan knows what a stored Wh really delivers
	if m, ok := site.batteryEfficiencyModel(dev.Config().Name, *b.Capacity); ok {
		bat.EtaC = new(float32(m.EtaC))
		bat.EtaD = new(float32(m.EtaD))
	}

	instance := dev.Instance()

	controllable := api.HasCap[api.BatteryController](instance)
	if controllable {
		// Full grid flexibility: the PW3 may charge from and discharge to grid. With
		// export priced at its realized worth, the optimizer dumps to grid only when
		// that genuinely beats serving the home or holding (a VPP event, or a
		// summer-evening delivery-EEC peak); on ordinary evenings self-consumption
		// wins and the pack covers the home instead of importing at peak.
		bat.ChargeFromGrid = true
		bat.DischargeToGrid = true
	}

	// a battery that forbids grid import (allowGridImport=false) is modelled with
	// charge_from_grid off, so the optimizer never plans to charge it from the grid
	// - it may still charge from solar surplus, and other batteries (e.g. the EV)
	// keep their own grid-charge flag
	if c, ok := api.Cap[api.GridChargeController](instance); ok && !c.AllowGridImport() {
		bat.ChargeFromGrid = false
	}

	if m, ok := api.Cap[api.BatteryPowerLimiter](instance); ok {
		charge, discharge := m.GetPowerLimits()
		bat.CMax = float32(charge)
		bat.DMax = float32(discharge)
	}

	if m, ok := api.Cap[api.BatterySocLimiter](instance); ok {
		minSoc, maxSoc := m.GetSocLimits()
		if maxSoc == 0 {
			maxSoc = 100 // empty/unset maxsoc means no upper limit
		}
		// clamp against current soc to prevent infeasible if it is outside the configured limits
		bat.SMin = min(bat.SInitial, float32(*b.Capacity*minSoc*10)) // Wh
		bat.SMax = max(bat.SInitial, float32(*b.Capacity*maxSoc*10)) // Wh
	}

	detail := batteryDetail{
		Type:         batteryTypeBattery,
		Name:         dev.Config().Name,
		Title:        deviceProperties(dev).Title,
		Capacity:     *b.Capacity,
		controllable: controllable,
	}

	// tariff forecast-based grid charging demand
	if bat.ChargeFromGrid {
		if demand := site.applyBatteryGridChargeLimit(bat.CMax, grid, minLen); demand != nil {
			bat.PDemand = prorate(demand, firstSlotDuration)
		}
	}

	return bat, detail
}

func matchSoc(ts []float32, fun func(float32) bool) time.Time {
	for i, soc := range ts {
		if fun(soc) {
			// TODO first slot
			return time.Now().Add(time.Duration(i+1) * tariff.SlotDuration).Round(time.Second)
		}
	}

	return time.Time{}
}

// continuousDemand creates a slice of power demands depending on loadpoint mode
func continuousDemand(lp loadpoint.API, minLen int) []float32 {
	if lp.GetStatus() != api.StatusC {
		return nil
	}

	pwr := lp.EffectiveMaxPower()
	if lp.GetMode() == api.ModeMinPV {
		pwr = lp.EffectiveMinPower()
	}

	return lo.RepeatBy(minLen, func(i int) float32 {
		return float32(pwr / slotsPerHour)
	})
}

// loadpointProfile returns the loadpoint's charging profile in Wh
// TODO consider charging efficiency
func loadpointProfile(lp loadpoint.API, minLen int) []float64 {
	mode := lp.GetMode()
	status := lp.GetStatus()

	if status != api.StatusC || (mode != api.ModeMinPV && mode != api.ModeNow) {
		return nil
	}

	power := lp.GetChargePower()
	if minP := lp.EffectiveMinPower(); mode == api.ModeMinPV && minP < power {
		power = minP
	}

	energy := lp.GetRemainingEnergy() * 1e3 // Wh
	energyKnown := energy > 0

	res := make([]float64, 0, minLen)
	for range minLen {
		deltaEnergy := power * float64(tariff.SlotDuration) / float64(time.Hour) // Wh
		if energyKnown && deltaEnergy >= energy {
			deltaEnergy = energy
		}
		energy -= deltaEnergy

		res = append(res, deltaEnergy)
	}

	return res
}

// unmodelledPower returns the uncontrollable power of a connected loadpoint that
// cannot be modelled as storage because the vehicle capacity is unknown
func unmodelledPower(lp loadpoint.API) float64 {
	power := lp.GetChargePower()

	// minpv keeps drawing at least min power while the vehicle is connected,
	// even before the charge meter has caught up
	if lp.GetMode() == api.ModeMinPV && lp.GetStatus() == api.StatusC {
		power = max(power, lp.EffectiveMinPower())
	}

	return max(0, power)
}

// forecastDays is the history window fed to the hybrid forecast: long enough to
// resolve multi-week cycles, capped so the daily refit stays inexpensive.
const forecastDays = 90

type forecastEntry struct {
	forecast *metrics.HybridForecast
	day      time.Time
}

// cachedForecast returns the hybrid forecast for a collector, refitting at most once
// per calendar day. The detected periodicities are stable over a day and the fit is
// the only expensive step (~seconds) while evaluation is cheap. It runs under
// optimizerMu, so the cache needs no further locking.
func (site *Site) cachedForecast(ref string) (*metrics.HybridForecast, error) {
	c, ok := site.collectors[ref]
	if !ok {
		return nil, fmt.Errorf("no collector %q", ref)
	}

	day := now.BeginningOfDay()
	if e, ok := site.forecasts[ref]; ok && e.day.Equal(day) {
		return e.forecast, nil
	}

	f, err := c.Forecast(day.AddDate(0, 0, -forecastDays))
	if err != nil {
		return nil, err
	}

	site.forecasts[ref] = forecastEntry{forecast: f, day: day}
	return f, nil
}

// forecastSlots evaluates a collector's hybrid forecast for the next minLen 15min
// slots, aligned to the current slot as the optimizer request expects.
func (site *Site) forecastSlots(ref string, minLen int) ([]float64, error) {
	f, err := site.cachedForecast(ref)
	if err != nil {
		return nil, err
	}

	base := time.Now().Truncate(tariff.SlotDuration)
	res := make([]float64, minLen)
	for i := range res {
		res[i] = f.At(base.Add(time.Duration(i) * tariff.SlotDuration))
	}
	return res, nil
}

// homeProfile returns the predicted home base load in Wh for the next minLen slots,
// from the hybrid forecast (civil-time daily seasonal plus multi-day harmonic residual).
func (site *Site) homeProfile(minLen int) ([]float64, error) {
	res, err := site.forecastSlots(metrics.Home, minLen)
	if err != nil {
		return nil, err
	}

	// kWh -> Wh, floored at 0 (a low slot's residual can dip the prediction negative)
	for i, v := range res {
		res[i] = max(0, v*1e3)
	}
	return res, nil
}

// vehicleAvailability returns a per-slot charge-power cap in W for the vehicle,
// gating realCMax by when the car is (predicted to be) plugged in:
//   - the current and next slot follow the real, current connection state, so a
//     connected car can charge now and a disconnected one cannot;
//   - further slots follow the recorded connection profile - available when the
//     car was connected more than half of that time-of-day slot on average;
//   - without enough history the far slots default to available, so a plan still
//     renders and the behaviour degrades to the old always-connected model until
//     the profile fills in.
//
// The result feeds BatteryConfig.CMaxT, which overrides the scalar CMax per slot;
// it is a power cap (not energy), so it is not prorated - the optimizer converts
// it per slot via dt.
func (site *Site) vehicleAvailability(id int, lp loadpoint.API, realCMax float32, minLen int) []float32 {
	connected := false
	if s := lp.GetStatus(); s == api.StatusB || s == api.StatusC {
		connected = true
	}
	return vehicleAvailabilitySlots(connected, site.connectedProfile(id, minLen), realCMax, minLen)
}

// vehicleAvailabilitySlots builds the per-slot charge-power cap from the current
// connection state and the aligned connection profile (per-slot connected
// minutes, or nil when unavailable). Split out from vehicleAvailability so the
// slot logic is testable without the metrics store. See vehicleAvailability.
func vehicleAvailabilitySlots(connected bool, profile []float64, realCMax float32, minLen int) []float32 {
	res := make([]float32, minLen)
	threshold := tariff.SlotDuration.Minutes() / 2 // connected more than half the slot

	for i := range res {
		var available bool
		switch {
		case i < 2:
			// current and next slot reflect the real, current connection state
			available = connected
		case profile != nil:
			available = profile[i] > threshold
		default:
			// no ≥30-day history yet: assume available so the plan still renders
			available = true
		}
		if available {
			res[i] = realCMax
		}
	}

	return res
}

// connectedProfile returns the predicted connected minutes (0..15) for the next
// minLen slots from the hybrid forecast, or nil when the collector is missing or
// lacks enough history. Mirrors homeProfile but keeps the value in minutes.
func (site *Site) connectedProfile(id, minLen int) []float64 {
	res, err := site.forecastSlots(connectedKey(id), minLen)
	if err != nil {
		return nil
	}

	// clamp to a slot's 0..15 connected minutes
	for i, v := range res {
		res[i] = min(max(v, 0), tariff.SlotDuration.Minutes())
	}
	return res
}

// measuredSlotEnergy returns the summed energy in Wh of the last completed
// metrics slot for the given collector refs, 0 when not available
func (site *Site) measuredSlotEnergy(refs ...string) float64 {
	var sum float64
	for _, ref := range refs {
		c, ok := site.collectors[ref]
		if !ok {
			return 0
		}

		v, ok := c.LastSlotEnergy()
		if !ok {
			return 0
		}
		sum += v
	}

	return sum * 1e3
}

// blendMeasured decays the first slots from the measured value into the
// forecast. Slot 0 uses the measured value, the forecast takes over from
// slot decaySlots on.
func blendMeasured[T constraints.Float](slots []T, measured T, decaySlots int) {
	for i := range min(decaySlots, len(slots)) {
		w := T(decaySlots-i) / T(decaySlots)
		slots[i] = w*measured + (1-w)*slots[i]
	}
}

// blendScale decays a scale factor towards 1 over the first slots.
// Slot 0 is scaled by the full factor, from slot decaySlots on it is 1.
func blendScale[T constraints.Float](slots []T, scale float64, decaySlots int) {
	for i := range min(decaySlots, len(slots)) {
		w := float64(decaySlots-i) / float64(decaySlots)
		slots[i] = T(float64(slots[i]) * (w*scale + (1 - w)))
	}
}

// prorate adjusts the first slot's energy amount according to remaining duration
func prorate[T constraints.Float](slots []T, firstSlotDuration time.Duration) []float32 {
	// return empty slice instead of nil to make api happy
	if len(slots) == 0 {
		return []float32{}
	}

	res := slices.Clone(slots)
	res[0] = res[0] * T(firstSlotDuration) / T(tariff.SlotDuration)
	return lo.Map(res, func(f T, _ int) float32 {
		return float32(f)
	})
}

func solarRatesToEnergy(rr api.Rates) (api.Rates, error) {
	res := make(api.Rates, 0, len(rr))

	for _, r := range rr {
		energy := solarEnergy(rr, r.Start, r.End)
		if energy < 0 {
			return nil, fmt.Errorf("negative solar energy from %v to %v: %.3f", r.Start, r.End, energy)
		}

		res = append(res, api.Rate{
			Start: r.Start,
			End:   r.End,
			Value: energy,
		})
	}

	return res, nil
}

func currentRates(tariff api.Tariff) api.Rates {
	if tariff == nil {
		return nil
	}

	rates, err := tariff.Rates()
	if err != nil {
		return nil
	}

	// filter past slots
	now := time.Now()
	return lo.Filter(rates, func(slot api.Rate, _ int) bool {
		return slot.End.After(now)
	})
}

// optimizerHorizon is the timeframe the hosted optimizer is limited to for sake
// of performance: 48 hours, extended to the end of that day. In the early hours
// the extension would add almost a full day, hence it only applies past 6:00.
func optimizerHorizon(t time.Time) time.Time {
	horizon := t.Add(48 * time.Hour)
	if t.Hour() < 6 {
		return horizon
	}
	return now.With(horizon).EndOfDay()
}

// slotsUntil limits maxLen to the slots starting before the given horizon
func slotsUntil(rates api.Rates, horizon time.Time, maxLen int) int {
	if i := slices.IndexFunc(rates[:min(maxLen, len(rates))], func(slot api.Rate) bool {
		return slot.Start.After(horizon)
	}); i >= 0 {
		return i
	}
	return maxLen
}

func timeSteps(minLen int, now time.Time) []int {
	res := make([]int, 0, minLen)

	eos := now.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	if d := eos.Sub(now); d > time.Second && d < tariff.SlotDuration {
		res = append(res, int(d.Seconds()))
	}

	for i := len(res); i < minLen; i++ {
		res = append(res, int(tariff.SlotDuration.Seconds())) // 15min slots
	}

	return res
}

func asTimestamps(dt []int, now time.Time) []time.Time {
	res := make([]time.Time, 0, len(dt))

	eos := now.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	res = append(res, eos.Add(-time.Duration(dt[0])*time.Second))

	for i := range len(dt) - 1 {
		res = append(res, res[i].Add(time.Duration(dt[i])*time.Second))
	}

	return res
}

func scaleAndPrune(rates api.Rates, scale float64, maxLen int) []float32 {
	res := make([]float32, 0, maxLen)

	for _, slot := range rates {
		res = append(res, float32(slot.Value*scale))
		if len(res) >= maxLen {
			break
		}
	}

	return res
}

// applyPlanGoal sets the optimizer charge goals (SGoal) from the loadpoint's
// departure plans. Every plan due within the horizon is applied, including ones the
// vehicle meets right now: driving before a later deadline can still take it below
// that plan. Each is a hard target the optimizer will pay grid or drain the home
// battery to reach. With no plan, no goal is set: a plugged-in car is instead kept
// charged by its forecast-driving commitment (SDrainT, see applyConsumption), so it
// covers its predicted trips from PV surplus and the cheapest grid hours without
// ever being force-filled to its ceiling off expensive grid.
func (site *Site) applyPlanGoal(lp loadpoint.API, bat *optimizer.BatteryConfig, minLen int) {
	goals, socBased := lp.GetPlanGoals(time.Now().Add(time.Duration(minLen) * tariff.SlotDuration))

	// Convert to Wh
	scale := 1000.0
	if vehicle := lp.GetVehicle(); socBased {
		if vehicle == nil {
			return
		}
		scale = vehicle.Capacity() * 10
	}

	for _, g := range goals {
		goal := float32(g.Goal * scale)
		if goal <= 0 {
			continue
		}

		// TODO precise slot placement
		slot := int(time.Until(g.Time) / tariff.SlotDuration)
		if slot < 0 || slot >= minLen {
			site.log.DEBUG.Printf("plan beyond forecast range or overrun: %.1f at %v slot %d", goal, g.Time.Round(time.Minute), slot)
			continue
		}

		if bat.SGoal == nil {
			bat.SGoal = make([]float32, minLen)
		}
		bat.SGoal[slot] = max(bat.SGoal[slot], goal)
		bat.SMax = max(bat.SMax, goal)
	}
}

// TODO remove once smart cost limit usage becomes obsolete
func applySmartCostLimit(lp loadpoint.API, demand []float32, grid api.Rates, minLen int) []float32 {
	costLimit := lp.GetSmartCostLimit()
	if costLimit == nil {
		return demand
	}

	maxLen := min(minLen, len(grid))

	// Check if any slots meet the cost limit
	if hasAffordableSlots := slices.ContainsFunc(grid[:maxLen], func(r api.Rate) bool {
		return r.Value <= *costLimit
	}); !hasAffordableSlots {
		return demand
	}

	maxPower := lp.EffectiveMaxPower()

	if demand == nil {
		demand = make([]float32, minLen)
	}

	for i := range maxLen {
		if grid[i].Value <= *costLimit {
			demand[i] = float32(maxPower / slotsPerHour)
		}
		// else: keep existing demand (either 0 or minPower from ModeMinPV)
	}

	return demand
}

// applyPrecondition forces max charging power during the planner's precondition window
// ("late charging"), i.e. the last precondition duration before the plan time
func applyPrecondition(lp loadpoint.API, demand []float32, minLen int) []float32 {
	precondition := lp.EffectivePlanStrategy().Precondition
	if precondition <= 0 {
		return demand
	}

	ts := lp.EffectivePlanTime()
	if ts.IsZero() {
		return demand
	}

	// TODO precise slot placement
	end := time.Until(ts)
	start := end - precondition
	if end <= 0 {
		return demand
	}

	first := max(int(start/tariff.SlotDuration), 0)
	if first >= minLen {
		return demand
	}

	if demand == nil {
		demand = make([]float32, minLen)
	}

	energy := float32(lp.EffectiveMaxPower() / slotsPerHour)

	for i := first; i < minLen; i++ {
		slotStart := time.Duration(i) * tariff.SlotDuration
		overlap := min(end, slotStart+tariff.SlotDuration) - max(start, slotStart)
		if overlap <= 0 {
			break
		}

		demand[i] = max(demand[i], energy*float32(overlap)/float32(tariff.SlotDuration))
	}

	return demand
}

func (site *Site) applyBatteryGridChargeLimit(cMax float32, grid api.Rates, minLen int) []float32 {
	limit := site.GetBatteryGridChargeLimit()
	if limit == nil {
		return nil
	}

	maxLen := min(minLen, len(grid))

	if hasAffordableSlots := slices.ContainsFunc(grid[:maxLen], func(r api.Rate) bool {
		return r.Value <= *limit
	}); !hasAffordableSlots {
		return nil
	}

	demand := make([]float32, minLen)
	for i := range maxLen {
		if grid[i].Value <= *limit {
			demand[i] = float32(float64(cMax) / slotsPerHour)
		}
	}

	return demand
}

// apiError extracts error message from optimizer API response
func apiError(resp *optimizer.PostOptimizeChargeScheduleResponse) error {
	var errObj *optimizer.Error
	switch resp.StatusCode() {
	case http.StatusBadRequest:
		errObj = resp.JSON400
	case http.StatusInternalServerError:
		errObj = resp.JSON500
	}

	if errObj == nil {
		return fmt.Errorf("invalid status: %d: %s", resp.StatusCode(), resp.Body)
	}

	if len(errObj.Details) > 0 {
		var details []string
		for field, msg := range errObj.Details {
			details = append(details, fmt.Sprintf("%s: %s", field, msg))
		}
		slices.Sort(details)
		return fmt.Errorf("%s (%s)", errObj.Message, strings.Join(details, ", "))
	}

	return errors.New(errObj.Message)
}
