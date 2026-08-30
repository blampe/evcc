package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/metrics"
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

// optimizerChargingStrategies are the valid grid charging strategies; the first
// entry is the default and preserves the previous hard-coded behavior.
var optimizerChargingStrategies = []string{
	string(optimizer.OptimizerStrategyChargingStrategyChargeBeforeExport),
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
// surplus.
const (
	sourceSurplus = "surplus"
	sourceGrid    = "grid"
)

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

// setSuggestions replaces the suggestions applied on each publish
func (site *Site) setSuggestions(suggestions map[string]types.Suggestion) {
	site.Lock()
	defer site.Unlock()

	site.suggestions = suggestions
}

// suggestion returns the optimizer suggestion for the given device key.
// The actionable flag is evaluated on read against the device's current
// action since that changes between optimizer runs.
func (site *Site) suggestion(key, currentAction string) *types.Suggestion {
	site.RLock()
	s, ok := site.suggestions[key]
	site.RUnlock()

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
		action := currentBatteryAction(schedules[batteryKey(dev.Config().Name)], now)
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
	if s := site.suggestion(loadpointKey(id), loadpointCurrentAction(lp)); s != nil {
		return s.Action, s.Charge, s.Source
	}
	return "", 0, ""
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

func optimizerURI() string {
	return cmp.Or(os.Getenv("OPTIMIZER_URI"), OPTIMIZER_URI)
}

const slotsPerHour = float64(time.Hour / tariff.SlotDuration)

// errOptimizerNotReady means battery measurements aren't available yet (e.g. at
// startup); the slot gate is left open so the next cycle retries.
var errOptimizerNotReady = errors.New("battery measurements not ready")

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
}

// currentBatterySlot resolves the plan slot in effect at `now`. Resolving by slot
// end time keeps the current slot correct even when a boundary passes between
// optimizer runs. It front-runs an export-mode transition: within
// optimizerFrontRunLead of the upcoming boundary, if the next slot flips
// export-ness, it returns the next slot to cover the Powerwall's ~2min
// autonomous pickup.
func currentBatterySlot(plan []batterySlot, now time.Time) (batterySlot, bool) {
	i := slices.IndexFunc(plan, func(s batterySlot) bool { return s.until.After(now) })
	if i < 0 {
		return batterySlot{}, false
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

// currentBatteryAction resolves the battery-mode action for `now` from the plan.
func currentBatteryAction(plan []batterySlot, now time.Time) string {
	if s, ok := currentBatterySlot(plan, now); ok {
		return s.action
	}
	return ""
}

// optimizerBatteryTargetSoc returns the optimizer's planned SoC (percent) for the
// battery's current plan slot, or 0 when there is no plan. Control passes it to a
// BatteryModeController so e.g. a grid discharge stops at the target.
func (site *Site) optimizerBatteryTargetSoc(name string) float64 {
	now := time.Now()

	site.RLock()
	plan := site.batterySchedules[batteryKey(name)]
	site.RUnlock()

	if s, ok := currentBatterySlot(plan, now); ok {
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

// optimizerUpdateAsync runs the optimizer unless the last run is younger than
// minAge. Pass 0 to force a run, e.g. when a changed setting should take effect
// without waiting for the next slot. It is a no-op when the optimizer is not
// active or a run is already in progress; the running update reflects the
// change on its next slot.
func (site *Site) optimizerUpdateAsync(minAge time.Duration) {
	if !sponsor.IsAuthorized() || !optimizerEnabled() {
		return
	}

	if !site.optimizerMu.TryLock() {
		return
	}
	defer site.optimizerMu.Unlock()

	if minAge == 0 {
		// keep the gate open so a not-ready run is retried on the next cycle
		site.optimizerUpdated = time.Time{}
	} else if time.Since(site.optimizerUpdated) < minAge {
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

	grid := currentRates(site.GetTariff(api.TariffUsageGrid))
	feedIn := currentRates(site.GetTariff(api.TariffUsageFeedIn))

	minLen := lo.Min([]int{len(grid), len(feedIn)})
	// exclude empty solar forecast from minLen
	if solarTariff != nil && len(solar) > 0 {
		minLen = min(minLen, len(solar))
	}

	if optimizerURI() == OPTIMIZER_URI {
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
			ChargingStrategy:    optimizer.OptimizerStrategyChargingStrategy(site.GetOptimizerChargingStrategy()),
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

	// no-grid-import cap (self-consumption): cap import at ~0 W and DON'T set
	// PrcPExcImp. The service then applies its built-in per-Wh import-exceedance
	// penalty (prc_e_grid_imp_pen ≈ 0.01 scaled ≈ 10/kWh, optimizer.py:89,273),
	// far above any price — so on TRUE prices the optimizer covers the home from
	// solar+battery (keeping the overnight reserve) and dumps only the surplus,
	// instead of dumping the battery and reimporting. It's SOFT: if import is
	// genuinely unavoidable it still imports (paying the penalty) and reports
	// grid_import_limit_exceeded rather than going infeasible. Setting PrcPExcImp
	// here would instead select the weak peak-power "demand rate" mode, so don't.
	// Lift via OPTIMIZER_ALLOW_GRID_IMPORT=true (winter EV, or full export
	// arbitrage during a price spike).
	if allowGridImport, _ := strconv.ParseBool(os.Getenv("OPTIMIZER_ALLOW_GRID_IMPORT")); !allowGridImport {
		req.Grid.PMaxImp = 1 // W (~0); omitempty needs non-zero, no PrcPExcImp = built-in per-Wh penalty
	}

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
		// ignore disconnected loadpoints, including StatusNone
		if s := lp.GetStatus(); s != api.StatusB && s != api.StatusC {
			continue
		}

		// no vehicle capacity and no session energy limit to model against:
		// account for the consumption as uncontrollable load
		if v := lp.GetVehicle(); v == nil || (v.Capacity() == 0 && lp.GetLimitEnergy() == 0) {
			unmodelled += unmodelledPower(lp)
			continue
		}

		// skip disabled loadpoints
		if cfg, detail := site.loadpointRequest(lp, minLen, firstSlotDuration, grid); cfg.CMax > 0 {
			detail.loadpoint = &id
			batteries = append(batteries, optimizerBattery{cfg, detail})
		}
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
	}

	for _, b := range batteries {
		b.cfg.PA = pa
		req.Batteries = append(req.Batteries, b.cfg)
		details.BatteryDetails = append(details.BatteryDetails, b.detail)
	}

	return req, details, nil
}

func (site *Site) optimizerUpdate(battery []types.Measurement) error {
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

	httpClient := request.NewClient(site.log)
	httpClient.Timeout = 90 * time.Second

	apiClient, err := optimizer.NewClientWithResponses(optimizerURI(), optimizer.WithHTTPClient(httpClient))
	if err != nil {
		return err
	}

	resp, err := apiClient.PostOptimizeChargeScheduleWithResponse(context.TODO(), req, func(_ context.Context, req *http.Request) error {
		if sponsor.IsAuthorized() {
			req.Header.Set("Authorization", "Bearer "+sponsor.Token)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if resp.StatusCode() != http.StatusOK {
		return apiError(resp)
	}

	// publish before the status check so the optimizer page stays available
	// for diagnosing non-optimal results
	site.publish("evopt", optimizerResult{
		Updated: time.Now(),
		Req:     req,
		Res:     *resp.JSON200,
		Details: details,
	})

	// feasible results are usable, they are just not proven optimal
	if status := resp.JSON200.Status; status != optimizer.Optimal && status != optimizer.Feasible {
		return errors.New(string(status))
	}

	site.applyOptimizerResult(req, details.BatteryDetails, *resp.JSON200)

	return nil
}

// applyOptimizerResult maps the optimizer response onto suggestions, battery
// forecast and notifications
func (site *Site) applyOptimizerResult(req optimizer.OptimizationInput, details []batteryDetail, res optimizer.OptimizationResult) {
	dt := req.TimeSeries.Dt
	slotHours := (time.Duration(dt[0]) * time.Second).Hours()
	gridImporting := len(res.GridImport) > 0 && res.GridImport[0] > 0
	gridExporting := len(res.GridExport) > 0 && res.GridExport[0] > 0

	// self-consumption (no grid import) mirrors the request-side cap; it remaps a
	// HOLD suggestion to normal (see currentSlotSuggestion)
	allowGridImport, _ := strconv.ParseBool(os.Getenv("OPTIMIZER_ALLOW_GRID_IMPORT"))
	noGridImport := !allowGridImport

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
		lo.Sum(res.GridImport), lo.Sum(res.GridExport), battToGrid)

	// raw prices the optimizer sees (undo the internal ×0.001 scaling). If export
	// (PE) shows near the import (PN) range, the feed-in tariff is misconfigured —
	// which would (wrongly) make dumping the battery look profitable.
	site.log.DEBUG.Printf("optimizer prices: import=%.3g..%.3g export=%.3g..%.3g",
		lo.Min(req.TimeSeries.PN)*1000, lo.Max(req.TimeSeries.PN)*1000,
		lo.Min(req.TimeSeries.PE)*1000, lo.Max(req.TimeSeries.PE)*1000)

	// first slot boundary; battery-mode plan slots are keyed by their end time so
	// the control loop can front-run an export-mode transition near the boundary
	// regardless of when the optimizer last ran.
	firstBoundary := time.Now().Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)

	var batteries []batteryResult
	suggestions := make(map[string]types.Suggestion, len(req.Batteries))
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

		// current-slot advisory, for the UI and notifications
		if s := currentSlotSuggestion(detail, batRes, 0, gridImporting, gridExporting, noGridImport, slotHours); s.Action != "" {
			suggestions[key] = s
		}

		// battery-mode plan (slot end + action) so the control loop front-runs
		// export-mode transitions independent of the optimizer cadence. Home
		// battery only (the ~2min autonomous pickup is PW3-specific).
		if detail.Type == batteryTypeBattery {
			schedules[key] = batteryModePlan(detail, batReq, batRes, res, dt, firstBoundary, noGridImport)
		}
	}

	site.publish("evopt-batteries", batteries)

	// log the current-slot suggestions so it's visible what the optimizer wants
	site.log.DEBUG.Printf("optimizer suggestions: gridImport=%t gridExport=%t", gridImporting, gridExporting)
	for key, s := range suggestions {
		site.log.DEBUG.Printf("optimizer %q: %s (charge %.0fW discharge %.0fW)", key, s.Action, s.Charge, s.Discharge)
	}

	site.setSuggestions(suggestions)
	site.setBatterySchedules(schedules)
	site.battery.Forecast = site.addBatteryForecastTotals(req.Batteries, res.Batteries)

	site.publishBattery()

	// publish for all loadpoints so suggestions of dropped-out loadpoints clear
	site.publishSuggestions()

	// notify on actionable suggestion changes (advisory only, see #31903)
	for _, ev := range site.diffSuggestions(site.pendingSuggestions(details)) {
		site.pushEvent(ev)
	}
}

// batteryModePlan builds the per-slot battery-mode plan (slot end time + mode
// action) that the control loop resolves each cycle to front-run export-mode
// transitions. Slot j spans [firstBoundary+(j-1)·slot, firstBoundary+j·slot];
// slot 0 is the current partial slot ending at firstBoundary.
func batteryModePlan(detail batteryDetail, batReq optimizer.BatteryConfig, batRes optimizer.BatteryResult, res optimizer.OptimizationResult, dt []int, firstBoundary time.Time, noGridImport bool) []batterySlot {
	n := min(len(batRes.ChargingPower), len(dt), frontRunScheduleSlots)
	plan := make([]batterySlot, 0, n)
	for j := 0; j < n; j++ {
		importing := len(res.GridImport) > j && res.GridImport[j] > 0
		exporting := len(res.GridExport) > j && res.GridExport[j] > 0
		slotHours := (time.Duration(dt[j]) * time.Second).Hours()
		action := currentSlotSuggestion(detail, batRes, j, importing, exporting, noGridImport, slotHours).Action
		until := firstBoundary.Add(time.Duration(j) * tariff.SlotDuration)
		plan = append(plan, batterySlot{until: until, action: action, soc: slotSocPercent(batReq, batRes, j)})
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
	}

	instance := dev.Instance()

	controllable := api.HasCap[api.BatteryController](instance)
	if controllable {
		// Full grid flexibility: the PW3 may charge from and discharge to grid.
		// The no-grid-import cap (see optimizerRequest) is what keeps enough battery
		// for the home, so with DischargeToGrid on the optimizer dumps only the
		// SURPLUS at the export peak — dumping the reserve would force a penalized
		// reimport, so it won't. Lift the cap (OPTIMIZER_ALLOW_GRID_IMPORT) for
		// full dump-and-reimport arbitrage during an export-price spike.
		bat.ChargeFromGrid = true
		bat.DischargeToGrid = true
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

// homeProfile returns the home base load in Wh
func (site *Site) homeProfile(minLen int) ([]float64, error) {
	// kWh over last 30 days
	profile, err := site.collectors[metrics.Home].EnergyProfile(now.BeginningOfDay().AddDate(0, 0, -30))
	if err != nil {
		return nil, err
	}

	// max 4 days
	slots := make([]float64, 0, minLen+1)
	for len(slots) <= minLen+24*4 { // allow for prorating first day
		slots = append(slots, profile[:]...)
	}

	res := profileSlotsFromNow(slots)
	if len(res) < minLen {
		return nil, fmt.Errorf("minimum home profile length %d is less than required %d", len(res), minLen)
	}
	if len(res) > minLen {
		res = res[:minLen]
	}

	// convert to Wh
	return lo.Map(res, func(v float64, i int) float64 {
		return v * 1e3
	}), nil
}

// profileSlotsFromNow strips away any slots before "now".
// The profile contains 48 15min slots (00:00-23:45) that repeat for multiple days.
func profileSlotsFromNow(profile []float64) []float64 {
	firstSlot := int(time.Now().Truncate(tariff.SlotDuration).Sub(now.BeginningOfDay()) / tariff.SlotDuration)
	return profile[firstSlot:]
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

func (site *Site) applyPlanGoal(lp loadpoint.API, bat *optimizer.BatteryConfig, minLen int) {
	goal, socBased := lp.GetPlanGoal()

	if goal > 0 {
		// Convert to Wh
		if vehicle := lp.GetVehicle(); socBased && vehicle != nil {
			goal *= vehicle.Capacity() * 10
		} else {
			goal *= 1000 // Wh
		}

		if ts := lp.EffectivePlanTime(); !ts.IsZero() {
			// TODO precise slot placement
			slot := int(time.Until(ts) / tariff.SlotDuration)
			if slot >= 0 && slot < minLen {
				// a real in-horizon plan goal: honor it exactly, no horizon-end fallback
				bat.SGoal = make([]float32, minLen)
				bat.SGoal[slot] = float32(goal)
				bat.SMax = max(bat.SMax, float32(goal))
				return
			}
			site.log.DEBUG.Printf("plan beyond forecast range or overrun: %.1f at %v slot %d", goal, ts.Round(time.Minute), slot)
		}
	}

	// No in-horizon plan goal was set (no goal, no plan time, or the deadline
	// falls outside [0, minLen)). Fall back to a goal at the vehicle's effective
	// ceiling on the last slot of the horizon so a connected EV is a real
	// in-horizon energy sink: battery surplus is routed into the car instead of
	// being dumped to grid at a low price, rather than the optimizer treating the
	// car as having zero demand.
	//
	// The ceiling is bat.SMax, already set by loadpointRequest to the configured
	// limit SoC (or the session energy limit, or 100% capacity when no limit is
	// set). It is in the request's state frame - absolute soc, or charged session
	// energy when soc is unavailable - so the goal is frame-consistent and never
	// exceeds, hence never overrides, the user's limit. Nothing to target if there
	// is no vehicle (EV-only fallback) or no ceiling.
	if lp.GetVehicle() == nil || bat.SMax <= 0 {
		return
	}
	bat.SGoal = make([]float32, minLen)
	bat.SGoal[minLen-1] = bat.SMax
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
