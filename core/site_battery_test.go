package core

import (
	"errors"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// TestBatterySocRetainOnReadError guards that a failed soc read keeps the last
// known soc instead of reporting the pack as empty (discussion #26560).
func TestBatterySocRetainOnReadError(t *testing.T) {
	ctrl := gomock.NewController(t)

	meter := api.NewMockMeter(ctrl)
	meter.EXPECT().CurrentPower().Return(0.0, nil).AnyTimes()

	battery := api.NewMockBattery(ctrl)
	battery.EXPECT().Soc().Return(0.0, errors.New("read failed")).AnyTimes()

	var bat api.Meter = &struct {
		api.Meter
		api.Battery
	}{
		Meter:   meter,
		Battery: battery,
	}

	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
	}
	site.battery.Soc = 84

	site.updateBatteryMeters()

	assert.Equal(t, 84.0, site.battery.Soc, "soc retained when the read fails")
}

func TestApplyBatteryMode(t *testing.T) {
	for _, tc := range []struct {
		internal, expected api.BatteryMode
	}{
		{api.BatteryUnknown, api.BatteryUnknown}, // no change required
		{api.BatteryNormal, api.BatteryUnknown},  // no change required
		{api.BatteryHold, api.BatteryNormal},
		{api.BatteryCharge, api.BatteryNormal},
	} {
		t.Logf("%+v", tc)

		ctrl := gomock.NewController(t)

		var bat api.Meter
		batCon := api.NewMockBatteryController(ctrl)

		bat = &struct {
			api.Meter
			api.BatteryController
		}{
			BatteryController: batCon,
		}

		site := &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
			batteryMode:   tc.internal,
		}

		// verify mode applied to battery
		if tc.expected != api.BatteryUnknown {
			batCon.EXPECT().SetBatteryMode(tc.expected).Times(1)
		}
		site.updateBatteryMode(false, api.Rate{})

		if tc.internal != api.BatteryNormal {
			assert.Equal(t, tc.expected, site.batteryMode)
		}

		ctrl.Finish()
	}
}

func TestRequiredExternalBatteryMode(t *testing.T) {
	for _, tc := range []struct {
		internal, external, new api.BatteryMode
	}{
		{api.BatteryUnknown, api.BatteryUnknown, api.BatteryUnknown},
		{api.BatteryUnknown, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryUnknown, api.BatteryCharge, api.BatteryCharge},

		{api.BatteryNormal, api.BatteryUnknown, api.BatteryUnknown},
		{api.BatteryNormal, api.BatteryNormal, api.BatteryUnknown}, // no change required
		{api.BatteryNormal, api.BatteryCharge, api.BatteryCharge},

		{api.BatteryCharge, api.BatteryUnknown, api.BatteryNormal},
		{api.BatteryCharge, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryCharge, api.BatteryCharge, api.BatteryUnknown}, // no change required
	} {
		t.Logf("%+v", tc)

		site := &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{nil},
		}

		site.batteryMode = tc.internal
		site.batteryModeExternal = tc.external

		mode := site.requiredBatteryMode(false, api.Rate{})
		assert.Equal(t, tc.new.String(), mode.String(), "internal mode expected %s got %s", tc.new, mode)
	}
}

func TestExternalBatteryModeChange(t *testing.T) {
	for _, tc := range []struct {
		internal, external, expected api.BatteryMode
	}{
		{api.BatteryUnknown, api.BatteryUnknown, api.BatteryUnknown},
		{api.BatteryUnknown, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryUnknown, api.BatteryCharge, api.BatteryCharge},

		{api.BatteryNormal, api.BatteryUnknown, api.BatteryUnknown},
		{api.BatteryNormal, api.BatteryNormal, api.BatteryUnknown},
		{api.BatteryNormal, api.BatteryCharge, api.BatteryCharge},

		{api.BatteryHold, api.BatteryUnknown, api.BatteryNormal}, // return to normal
		{api.BatteryHold, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryHold, api.BatteryHold, api.BatteryUnknown},

		{api.BatteryCharge, api.BatteryUnknown, api.BatteryNormal}, // return to normal
		{api.BatteryCharge, api.BatteryNormal, api.BatteryNormal},
		{api.BatteryCharge, api.BatteryCharge, api.BatteryUnknown},
	} {
		t.Logf("%+v", tc)

		ctrl := gomock.NewController(t)

		var bat api.Meter
		batCon := api.NewMockBatteryController(ctrl)

		bat = &struct {
			api.Meter
			api.BatteryController
		}{
			BatteryController: batCon,
		}

		site := &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
			batteryMode:   tc.internal,
		}

		// 1. set required external mode
		site.SetBatteryModeExternal(tc.external)
		assert.Equal(t, site.batteryModeExternal, tc.external, "external mode expected %s got %s", tc.external, site.batteryModeExternal)
		assert.Equal(t, site.batteryMode, tc.internal, "internal mode expected unchanged %s got %s", tc.internal, site.batteryMode)

		// 2. verify external mode applied to battery
		if tc.expected != api.BatteryUnknown {
			batCon.EXPECT().SetBatteryMode(tc.expected).Times(1)
		}
		site.updateBatteryMode(false, api.Rate{})
		if !ctrl.Satisfied() {
			ctrl.Finish()
		}

		// 3. verify required external mode only applied once
		site.updateBatteryMode(false, api.Rate{})
		if !ctrl.Satisfied() {
			ctrl.Finish()
		}

		// 4. verify timer expiry
		site.batteryModeExternalTimer = site.batteryModeExternalTimer.Add(-time.Hour)
		site.batteryModeWatchdogExpired()

		// mode reverted to unknown, timer still active
		assert.Equal(t, site.batteryModeExternal, api.BatteryUnknown)
		assert.False(t, site.batteryModeExternalTimer.IsZero())

		// battery switched back to normal mode
		batCon.EXPECT().SetBatteryMode(api.BatteryNormal).Times(1)
		site.updateBatteryMode(false, api.Rate{})

		// timer disabled
		assert.True(t, site.batteryModeExternalTimer.IsZero())

		ctrl.Finish()
	}
}

func TestForcedBatteryChargeLimits(t *testing.T) {
	limit := 80.0

	for _, tc := range []struct {
		internal, expected api.BatteryMode
		soc                float64
	}{
		{api.BatteryUnknown, api.BatteryCharge, 50},
		{api.BatteryUnknown, api.BatteryHold, 90},

		{api.BatteryNormal, api.BatteryCharge, 50},
		{api.BatteryNormal, api.BatteryHold, 90},

		{api.BatteryHold, api.BatteryCharge, 50},
		{api.BatteryHold, api.BatteryHold, 90}, // TODO make this api.BatteryUnknown

		{api.BatteryCharge, api.BatteryUnknown, 50},
		{api.BatteryCharge, api.BatteryHold, 90},
	} {
		t.Logf("%+v", tc)

		ctrl := gomock.NewController(t)

		var bat api.Meter
		batSoc := api.NewMockBattery(ctrl)
		batCon := api.NewMockBatteryController(ctrl)
		batSocLimit := api.NewMockBatterySocLimiter(ctrl)

		bat = &struct {
			api.Meter
			api.Battery
			api.BatteryController
			api.BatterySocLimiter
		}{
			Meter:             bat,
			Battery:           batSoc,
			BatteryController: batCon,
			BatterySocLimiter: batSocLimit,
		}

		site := &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{}, bat)},
			batteryMode:   tc.internal,
		}

		batSoc.EXPECT().Soc().Return(tc.soc, nil).Times(1)
		batSocLimit.EXPECT().GetSocLimits().Return(0.0, limit).Times(1)

		if tc.expected != api.BatteryUnknown {
			batCon.EXPECT().SetBatteryMode(tc.expected).Times(1)
		}

		site.updateBatteryMode(true, api.Rate{})

		ctrl.Finish()
	}
}

func optimizerSite(internal api.BatteryMode, suggestions map[string]types.Suggestion, names ...string) *Site {
	var meters []config.Device[api.Meter]
	for _, n := range names {
		meters = append(meters, config.NewStaticDevice(config.Named{Name: n}, api.Meter(nil)))
	}
	// one far-future plan slot per battery: currentBatteryAction resolves it as the
	// current slot with no next slot, so no front-run — just the mapped action
	until := time.Now().Add(time.Hour)
	schedules := make(map[string][]batterySlot, len(suggestions))
	for name, s := range suggestions {
		schedules[batteryKey(name)] = []batterySlot{{until: until, action: s.Action}}
	}
	return &Site{
		log:              util.NewLogger("foo"),
		batteryMeters:    meters,
		batteryMode:      internal,
		batterySchedules: schedules,
	}
}

// TestOptimizerBatteryMode verifies the optimizer's per-battery suggestion is
// mapped to a battery mode, including the new discharge action.
func TestOptimizerBatteryMode(t *testing.T) {
	// each suggestion action maps to its battery mode
	for action, want := range map[string]api.BatteryMode{
		"normal":     api.BatteryNormal,
		"hold":       api.BatteryHold,
		"charge":     api.BatteryCharge,
		"holdcharge": api.BatteryHoldCharge,
		"discharge":  api.BatteryDischarge,
	} {
		s := optimizerSite(api.BatteryUnknown, map[string]types.Suggestion{"bat1": {Action: action}}, "bat1")
		assert.Equal(t, want, s.optimizerBatteryMode(), action)
	}

	// no suggestion => Unknown (existing behaviour preserved)
	assert.Equal(t, api.BatteryUnknown, optimizerSite(api.BatteryUnknown, nil, "bat1").optimizerBatteryMode())

	// agreeing batteries => that mode
	agree := map[string]types.Suggestion{"bat1": {Action: "discharge"}, "bat2": {Action: "discharge"}}
	assert.Equal(t, api.BatteryDischarge, optimizerSite(api.BatteryUnknown, agree, "bat1", "bat2").optimizerBatteryMode())

	// disagreeing batteries => Unknown (a single site-wide mode can't represent both)
	disagree := map[string]types.Suggestion{"bat1": {Action: "discharge"}, "bat2": {Action: "charge"}}
	assert.Equal(t, api.BatteryUnknown, optimizerSite(api.BatteryUnknown, disagree, "bat1", "bat2").optimizerBatteryMode())
}

// TestRequiredBatteryModeOptimizer verifies the always-on optimizer suggestion
// drives requiredBatteryMode: it applies the suggested mode, doesn't re-apply
// once reached, reverts to normal when the suggestion stops, takes precedence
// over the static grid-charge heuristic, but yields to an external/manual mode.
func TestRequiredBatteryModeOptimizer(t *testing.T) {
	discharge := map[string]types.Suggestion{"bat1": {Action: "discharge"}}

	// suggests discharge from normal => discharge
	s := optimizerSite(api.BatteryNormal, discharge, "bat1")
	assert.Equal(t, api.BatteryDischarge, s.requiredBatteryMode(false, api.Rate{}))

	// already discharging => Unknown (no re-apply, mode persists)
	s = optimizerSite(api.BatteryDischarge, discharge, "bat1")
	assert.Equal(t, api.BatteryUnknown, s.requiredBatteryMode(false, api.Rate{}))

	// suggestion gone while in a modified mode => revert to normal
	s = optimizerSite(api.BatteryDischarge, nil, "bat1")
	assert.Equal(t, api.BatteryNormal, s.requiredBatteryMode(false, api.Rate{}))

	// optimizer wins over the static grid-charge heuristic
	s = optimizerSite(api.BatteryNormal, discharge, "bat1")
	assert.Equal(t, api.BatteryDischarge, s.requiredBatteryMode(true, api.Rate{}))

	// an external/manual mode still wins over the optimizer
	s = optimizerSite(api.BatteryNormal, discharge, "bat1")
	s.batteryModeExternal = api.BatteryHold
	assert.Equal(t, api.BatteryHold, s.requiredBatteryMode(false, api.Rate{}))

	// battery boost overrides the optimizer: force normal so the home battery
	// discharges into the vehicle instead of the boost pulling from the grid
	s = optimizerSite(api.BatteryHoldCharge, discharge, "bat1")
	s.loadpoints = []*Loadpoint{{batteryBoost: boostStart}}
	assert.Equal(t, api.BatteryNormal, s.requiredBatteryMode(false, api.Rate{}))

	// boost with the battery already normal => no change needed
	s = optimizerSite(api.BatteryNormal, discharge, "bat1")
	s.loadpoints = []*Loadpoint{{batteryBoost: boostStart}}
	assert.Equal(t, api.BatteryUnknown, s.requiredBatteryMode(false, api.Rate{}))
}

func TestOptimizerReservePinned(t *testing.T) {
	s := &Site{}
	assert.False(t, s.optimizerReservePinned(), "no loadpoints")

	s.loadpoints = []*Loadpoint{{optimizerAction: actionCharge, optimizerChargeSource: sourceSurplus}}
	assert.False(t, s.optimizerReservePinned(), "surplus charging is not pinned")

	s.loadpoints = []*Loadpoint{{optimizerChargeSource: sourceSurplus}, {optimizerAction: actionCharge, optimizerChargeSource: sourceBattery}}
	assert.True(t, s.optimizerReservePinned(), "any loadpoint charging from the battery pins")

	// the pack would otherwise feed a grid-planned charge down to MinSoc
	s.loadpoints = []*Loadpoint{{optimizerAction: actionCharge, optimizerChargeSource: sourceGrid}}
	assert.True(t, s.optimizerReservePinned(), "a grid-fed charge pins")

	// the reserve guard stopped the car: the source stays "battery" but nothing is
	// drawn from the pack any more, so there is no boost left to pin the reserve for
	s.loadpoints = []*Loadpoint{{optimizerAction: actionStop, optimizerChargeSource: sourceBattery}}
	assert.False(t, s.optimizerReservePinned(), "a stopped car is not pinned")
}

// recordingBatteryController implements Meter + BatteryController +
// BatteryModeController, recording the last SetBatteryModeSoc so tests can assert
// the reserve target passed during a battery boost.
type recordingBatteryController struct {
	mode api.BatteryMode
	soc  float64
}

func (c *recordingBatteryController) CurrentPower() (float64, error) { return 0, nil }
func (c *recordingBatteryController) SetBatteryMode(m api.BatteryMode) error {
	c.mode, c.soc = m, 0
	return nil
}
func (c *recordingBatteryController) SetBatteryModeSoc(m api.BatteryMode, soc float64) error {
	c.mode, c.soc = m, soc
	return nil
}

// TestBatteryBoostReservePin verifies normal mode pins the pack's reserve to the
// plan's target SoC while boosting (so it stops there instead of draining to
// MinSoc), and restores the MinSoc reserve (target 0) once the boost ends -
// including when the pack stays in normal mode across the transition.
func TestBatteryBoostReservePin(t *testing.T) {
	ctrl := &recordingBatteryController{}
	newSite := func() *Site {
		return &Site{
			log:           util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "home"}, api.Meter(ctrl))},
			batteryMode:   api.BatteryNormal, // self-consumption default
			batterySchedules: map[string][]batterySlot{
				batteryKey("home"): {{until: time.Now().Add(time.Hour), action: "normal", soc: 55}},
			},
		}
	}

	// applyBatteryMode: no boost keeps the pack available to MinSoc (target 0)
	site := newSite()
	assert.NoError(t, site.applyBatteryMode(api.BatteryNormal, false))
	assert.Zero(t, ctrl.soc, "no boost -> reserve stays at MinSoc")

	// applyBatteryMode: boost pins the reserve to the plan's target SoC
	assert.NoError(t, site.applyBatteryMode(api.BatteryNormal, true))
	assert.Equal(t, 55.0, ctrl.soc, "boost -> reserve pinned to plan target")

	// updateBatteryMode: a boost starting while already in normal pins the reserve
	// without a mode change, and records the slot
	site = newSite()
	site.loadpoints = []*Loadpoint{{optimizerAction: actionCharge, optimizerChargeSource: sourceBattery}}
	site.updateBatteryMode(false, api.Rate{})
	assert.Equal(t, 55.0, ctrl.soc, "boost start pins the reserve in normal mode")
	assert.False(t, site.batteryBoostSlot.IsZero(), "boost slot recorded")

	// updateBatteryMode: the boost ending restores the MinSoc reserve and clears
	// the slot, again without a mode change
	site.loadpoints = []*Loadpoint{{optimizerChargeSource: sourceSurplus}}
	site.updateBatteryMode(false, api.Rate{})
	assert.Zero(t, ctrl.soc, "boost end restores the MinSoc reserve")
	assert.True(t, site.batteryBoostSlot.IsZero(), "boost slot cleared")
}

// TestGridChargeKeepsReservePin covers a planned charge that the pack starts
// (boost) and the grid finishes: the plan keeps the pack on a house-only drain
// once the car switches to grid, so the reserve must stay pinned across the
// source change instead of dropping to MinSoc and letting the Powerwall feed the
// car to empty.
func TestGridChargeKeepsReservePin(t *testing.T) {
	ctrl := &recordingBatteryController{}
	lp := &Loadpoint{optimizerAction: actionCharge, optimizerChargeSource: sourceBattery}
	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "home"}, api.Meter(ctrl))},
		batteryMode:   api.BatteryNormal,
		batterySchedules: map[string][]batterySlot{
			batteryKey("home"): {{until: time.Now().Add(time.Hour), action: "normal", soc: 21}},
		},
		loadpoints: []*Loadpoint{lp},
	}

	site.updateBatteryMode(false, api.Rate{})
	assert.Equal(t, 21.0, ctrl.soc, "boost pins the reserve")

	lp.optimizerChargeSource = sourceGrid
	ctrl.soc = -1 // detect any re-apply
	site.updateBatteryMode(false, api.Rate{})
	assert.Equal(t, -1.0, ctrl.soc, "switching to grid keeps the pin (no MinSoc restore)")
	assert.False(t, site.batteryBoostSlot.IsZero(), "pin slot kept")

	// the charge ends: the pack goes back to serving the house down to MinSoc
	lp.optimizerAction = actionStop
	site.updateBatteryMode(false, api.Rate{})
	assert.Zero(t, ctrl.soc, "charge end restores the MinSoc reserve")
}

// TestBatteryBoostTripReleasesPin covers the house being stranded on the grid after
// a boost: once the reserve guard stops the car, the pack's reserve must drop back
// to MinSoc so the pack keeps serving the house, instead of staying pinned at the
// slot's target for the rest of the slot while the house imports.
func TestBatteryBoostTripReleasesPin(t *testing.T) {
	ctrl := &recordingBatteryController{}
	lp := &Loadpoint{}
	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "home"}, api.Meter(ctrl))},
		batteryMode:   api.BatteryNormal,
		batterySchedules: map[string][]batterySlot{
			batteryKey("home"): {{until: time.Now().Add(time.Hour), action: "normal", soc: 55}},
		},
		loadpoints: []*Loadpoint{lp},
	}
	site.setSuggestions(map[string]types.Suggestion{
		loadpointKey(0): {Action: actionCharge, Charge: 7000, Source: sourceBattery},
	})

	// the site loop: the loadpoint's action first, then the battery mode
	cycle := func() {
		lp.optimizerAction, lp.optimizerChargePower, lp.optimizerChargeSource = site.loadpointOptimizerAction(0)
		site.updateBatteryMode(false, api.Rate{})
	}

	// well above the target: boost runs and the reserve is pinned to the target
	site.battery.Soc = 70
	cycle()
	require.Equal(t, actionCharge, lp.optimizerAction)
	assert.Equal(t, 55.0, ctrl.soc, "boosting pins the reserve at the plan target")

	// the pack reaches the target: the guard stops the car and the pin is released
	site.battery.Soc = 55
	cycle()
	require.Equal(t, actionStop, lp.optimizerAction, "guard stops the car at the reserve")
	assert.Equal(t, sourceBattery, lp.optimizerChargeSource, "source is still battery")
	assert.Zero(t, ctrl.soc, "stopped car releases the reserve to MinSoc so the pack serves the house")
	assert.True(t, site.batteryBoostSlot.IsZero(), "boost slot cleared")

	// the guard stays latched for the slot, and so does the release
	cycle()
	assert.Equal(t, actionStop, lp.optimizerAction)
	assert.Zero(t, ctrl.soc, "no re-pin within the tripped slot")
}

// TestBatteryBoostAdvanceMovesPin verifies that when a boost reaches its slot's
// reserve and the plan moves on to the next slot early, the pack's reserve pin is
// rewritten to the next slot's target in the same cycle, so the pack keeps feeding
// the car's (smaller) draw instead of holding at the old target while it spills to
// the grid.
func TestBatteryBoostAdvanceMovesPin(t *testing.T) {
	boundary := time.Now().Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	ctrl := &recordingBatteryController{}
	lp := &Loadpoint{}
	site := &Site{
		log:           util.NewLogger("foo"),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "home"}, api.Meter(ctrl))},
		batteryMode:   api.BatteryNormal,
		batterySchedules: map[string][]batterySlot{
			batteryKey("home"): {
				{until: boundary, action: "normal", soc: 55, startSoc: 70},
				{until: boundary.Add(tariff.SlotDuration), action: "normal", soc: 50, startSoc: 55},
			},
		},
		loadpoints: []*Loadpoint{lp},
	}
	site.setSuggestionPlans(map[string][]suggestionSlot{
		loadpointKey(0): {
			{until: boundary, suggestion: types.Suggestion{Action: actionCharge, Charge: 7000, Source: sourceBattery}},
			{until: boundary.Add(tariff.SlotDuration), suggestion: types.Suggestion{Action: actionCharge, Charge: 3000, Source: sourceBattery}},
		},
	})

	cycle := func() {
		lp.optimizerAction, lp.optimizerChargePower, lp.optimizerChargeSource = site.loadpointOptimizerAction(0)
		site.updateBatteryMode(false, api.Rate{})
	}

	site.battery.Soc = 70
	cycle()
	require.Equal(t, actionCharge, lp.optimizerAction)
	assert.Equal(t, 55.0, ctrl.soc, "boosting pins the reserve at the slot target")

	site.battery.Soc = 55
	cycle()
	require.Equal(t, actionCharge, lp.optimizerAction, "the car keeps charging")
	assert.Equal(t, 3000.0, lp.optimizerChargePower, "at the next slot's power")
	assert.Equal(t, 50.0, ctrl.soc, "the pin follows the plan to the next slot's target")
}
