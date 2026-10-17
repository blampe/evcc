package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/soc"
	"github.com/evcc-io/evcc/core/types"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	optimizer "github.com/evcc-io/optimizer/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestLoadpointProfile(t *testing.T) {
	ctrl := gomock.NewController(t)

	lp := loadpoint.NewMockAPI(ctrl)
	lp.EXPECT().GetMode().Return(api.ModeMinPV).AnyTimes()
	lp.EXPECT().GetStatus().Return(api.StatusC).AnyTimes()
	lp.EXPECT().GetChargePower().Return(10000.0).AnyTimes()   //  10 kW
	lp.EXPECT().EffectiveMinPower().Return(1000.0).AnyTimes() //   1 kW
	lp.EXPECT().GetRemainingEnergy().Return(1.8).AnyTimes()   // 1.8 kWh

	// expected slots: 0.25 kWh...
	require.Equal(t, []float64{250, 250, 250, 250, 250, 250, 250, 50}, loadpointProfile(lp, 8))
}

func TestOptimizerHorizon(t *testing.T) {
	ts := time.Date(2025, 1, 1, 10, 30, 0, 0, time.Local)
	horizon := optimizerHorizon(ts)

	// 48h plus end of day
	assert.Equal(t, time.Date(2025, 1, 3, 23, 59, 59, int(time.Second-time.Nanosecond), time.Local), horizon)

	// before 6:00 the day is not extended
	assert.Equal(t, time.Date(2025, 1, 3, 5, 30, 0, 0, time.Local),
		optimizerHorizon(time.Date(2025, 1, 1, 5, 30, 0, 0, time.Local)))

	rates := make(api.Rates, 0, 4*96)
	for slot := ts.Truncate(tariff.SlotDuration); len(rates) < cap(rates); slot = slot.Add(tariff.SlotDuration) {
		rates = append(rates, api.Rate{Start: slot, End: slot.Add(tariff.SlotDuration)})
	}

	// 4 days of slots from 10:30, capped at the last slot of Jan 3rd
	assert.Equal(t, 246, slotsUntil(rates, horizon, len(rates)))
	assert.Equal(t, time.Date(2025, 1, 3, 23, 45, 0, 0, time.Local), rates[245].Start)

	// shorter forecast is not extended
	assert.Equal(t, 8, slotsUntil(rates, horizon, 8))
}

func TestApplyPrecondition(t *testing.T) {
	ctrl := gomock.NewController(t)

	lp := loadpoint.NewMockAPI(ctrl)
	lp.EXPECT().EffectiveMaxPower().Return(8000.0).AnyTimes() // 2 kWh per slot

	// no precondition configured
	lp.EXPECT().EffectivePlanStrategy().Return(api.PlanStrategy{}).Times(1)
	assert.Nil(t, applyPrecondition(lp, nil, 8))

	// no plan
	lp.EXPECT().EffectivePlanStrategy().Return(api.PlanStrategy{Precondition: time.Hour}).Times(1)
	lp.EXPECT().EffectivePlanTime().Return(time.Time{}).Times(1)
	assert.Nil(t, applyPrecondition(lp, nil, 8))

	// plan in 1h, 40min precondition: slots 1 (10min) and 2, 3 (full)
	lp.EXPECT().EffectivePlanTime().Return(time.Now().Add(time.Hour)).Times(1)
	lp.EXPECT().EffectivePlanStrategy().Return(api.PlanStrategy{Precondition: 40 * time.Minute}).Times(1)
	res := applyPrecondition(lp, nil, 8)
	require.Len(t, res, 8)
	assert.InDeltaSlice(t, []float32{0, 2000. / 1.5, 2000, 2000, 0, 0, 0, 0}, res, 1)

	// existing demand is kept where higher
	lp.EXPECT().EffectivePlanTime().Return(time.Now().Add(time.Hour)).Times(1)
	lp.EXPECT().EffectivePlanStrategy().Return(api.PlanStrategy{Precondition: 30 * time.Minute}).Times(1)
	res = applyPrecondition(lp, []float32{3000, 3000, 3000, 3000, 0, 0, 0, 0}, 8)
	assert.InDeltaSlice(t, []float32{3000, 3000, 3000, 3000, 0, 0, 0, 0}, res, 1)

	// plan beyond horizon
	lp.EXPECT().EffectivePlanTime().Return(time.Now().Add(24 * time.Hour)).Times(1)
	lp.EXPECT().EffectivePlanStrategy().Return(api.PlanStrategy{Precondition: time.Hour}).Times(1)
	assert.Nil(t, applyPrecondition(lp, nil, 8))
}

func TestLoadpointCurrentAction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		status  api.ChargeStatus
		soc     float64
		want    string
	}{
		{"charging", true, api.StatusC, 0, actionCharge},
		{"enabled but idle (e.g. vehicle finished at limit)", true, api.StatusB, 0, actionStop},
		{"disabled", false, api.StatusB, 0, actionStop},
		{"charging at 100% soc with no explicit limit", true, api.StatusC, 100, actionStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lp := &Loadpoint{enabled: tc.enabled, status: tc.status, vehicleSoc: tc.soc}
			assert.Equal(t, tc.want, loadpointCurrentAction(lp))
		})
	}
}

func TestAsTimestamps(t *testing.T) {
	// now is 10 minutes into a 15-minute slot
	now := time.Date(2025, 1, 1, 12, 10, 0, 0, time.UTC)

	// dt[0]=300 means first event is 300s (5min) before end of current slot
	// dt[1..] just mark subsequent slot boundaries
	dt := []int{60 * 5, 60 * 15, 60 * 15}

	got := asTimestamps(dt, now)

	// current slot: 12:00–12:15
	// first timestamp: 12:15 - 5min = 12:10
	// subsequent: 12:15, 12:30
	assert.Equal(t, []time.Time{
		time.Date(2025, 1, 1, 12, 10, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 12, 15, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 12, 30, 0, 0, time.UTC),
	}, got)
}

func TestUnmodelledPower(t *testing.T) {
	ctrl := gomock.NewController(t)

	for _, tc := range []struct {
		name            string
		mode            api.ChargeMode
		status          api.ChargeStatus
		power, minPower float64
		expected        float64
	}{
		{"pv charging", api.ModePV, api.StatusC, 4000, 1380, 4000},
		{"pv connected", api.ModePV, api.StatusB, 0, 1380, 0},
		{"minpv floor before meter caught up", api.ModeMinPV, api.StatusC, 0, 4000, 4000},
		{"minpv floor must not lower measured", api.ModeMinPV, api.StatusC, 4000, 1000, 4000},
		{"minpv floor only applies while charging", api.ModeMinPV, api.StatusB, 0, 4000, 0},
		{"negative measurement clamped", api.ModePV, api.StatusC, -100, 0, 0},
	} {
		lp := loadpoint.NewMockAPI(ctrl)
		lp.EXPECT().GetMode().Return(tc.mode).AnyTimes()
		lp.EXPECT().GetStatus().Return(tc.status).AnyTimes()
		lp.EXPECT().GetChargePower().Return(tc.power).AnyTimes()
		lp.EXPECT().EffectiveMinPower().Return(tc.minPower).AnyTimes()

		assert.Equal(t, tc.expected, unmodelledPower(lp), tc.name)
	}
}

func TestBatteryForecastSocExtremes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		req       []optimizer.BatteryConfig
		soc       [][]float32
		high, low *batteryForecastSlot
	}{
		{
			"no home battery",
			[]optimizer.BatteryConfig{{SMax: 80}}, // SCapacity unset → vehicle
			[][]float32{{1000, 2000}},
			nil, nil,
		},
		{
			"single home battery rising — reaches full",
			[]optimizer.BatteryConfig{{SCapacity: 1000, SMax: 1000}},
			[][]float32{{200, 500, 1000}},
			&batteryForecastSlot{slot: 2, soc: 100, limit: true},
			&batteryForecastSlot{slot: 0, soc: 20, limit: false},
		},
		{
			"single home battery falling — reaches empty",
			[]optimizer.BatteryConfig{{SCapacity: 1000, SMax: 1000}},
			[][]float32{{900, 500, 0}},
			&batteryForecastSlot{slot: 0, soc: 90, limit: false},
			&batteryForecastSlot{slot: 2, soc: 0, limit: true},
		},
		{
			"single home battery — local extremes (no limit reached)",
			[]optimizer.BatteryConfig{{SCapacity: 1000, SMax: 900, SMin: 100}},
			[][]float32{{500, 800, 200}},
			&batteryForecastSlot{slot: 1, soc: 80, limit: false},
			&batteryForecastSlot{slot: 2, soc: 20, limit: false},
		},
		{
			"two home batteries aggregated",
			[]optimizer.BatteryConfig{
				{SCapacity: 1000, SMax: 1000},
				{SCapacity: 1000, SMax: 1000},
			},
			[][]float32{
				{200, 400, 1000},
				{800, 400, 1000},
			},
			&batteryForecastSlot{slot: 2, soc: 100, limit: true},
			&batteryForecastSlot{slot: 1, soc: 40, limit: false},
		},
		{
			"vehicle and home battery — vehicle ignored",
			[]optimizer.BatteryConfig{
				{SMax: 80},                    // vehicle
				{SCapacity: 1000, SMax: 1000}, // home
			},
			[][]float32{
				{0, 0, 80},
				{200, 500, 900},
			},
			&batteryForecastSlot{slot: 2, soc: 90, limit: false},
			&batteryForecastSlot{slot: 0, soc: 20, limit: false},
		},
		{
			"first slot at SMax wins for highest",
			[]optimizer.BatteryConfig{{SCapacity: 1000, SMax: 1000}},
			[][]float32{{500, 1000, 1000}},
			&batteryForecastSlot{slot: 1, soc: 100, limit: true},
			&batteryForecastSlot{slot: 0, soc: 50, limit: false},
		},
		{
			"already full — no highest",
			[]optimizer.BatteryConfig{{SCapacity: 1000, SMax: 1000}},
			[][]float32{{1000, 1000, 500}},
			nil,
			&batteryForecastSlot{slot: 2, soc: 50, limit: false},
		},
		{
			"already empty — no lowest",
			[]optimizer.BatteryConfig{{SCapacity: 1000, SMax: 1000, SMin: 100}},
			[][]float32{{100, 100, 500}},
			&batteryForecastSlot{slot: 2, soc: 50, limit: false},
			nil,
		},
		{
			"near SMax is not full",
			[]optimizer.BatteryConfig{{SCapacity: 1000, SMax: 1000}},
			[][]float32{{500, 999, 800}},
			&batteryForecastSlot{slot: 1, soc: 99.9, limit: false},
			&batteryForecastSlot{slot: 0, soc: 50, limit: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := make([]optimizer.BatteryResult, len(tc.soc))
			for i, s := range tc.soc {
				resp[i] = optimizer.BatteryResult{StateOfCharge: s}
			}

			high, low := batteryForecastSocExtremes(tc.req, resp)

			if tc.high == nil {
				assert.Nil(t, high, "high")
			} else {
				require.NotNil(t, high, "high")
				assert.Equal(t, tc.high.slot, high.slot, "high.slot")
				assert.InDelta(t, tc.high.soc, high.soc, 1e-3, "high.soc")
				assert.Equal(t, tc.high.limit, high.limit, "high.limit")
			}
			if tc.low == nil {
				assert.Nil(t, low, "low")
			} else {
				require.NotNil(t, low, "low")
				assert.Equal(t, tc.low.slot, low.slot, "low.slot")
				assert.InDelta(t, tc.low.soc, low.soc, 1e-3, "low.soc")
				assert.Equal(t, tc.low.limit, low.limit, "low.limit")
			}
		})
	}
}

// TestBatteryRequestSocLimitsClamp ensures the reported soc is always clamped into
// the resulting [SMin, SMax] range, even when it lies outside the configured soc
// limits (e.g. right after a firmware update changed the reported soc or the min/max
// soc settings) - otherwise the optimizer is infeasible from the first slot.
func TestBatteryRequestSocLimitsClamp(t *testing.T) {
	newBatteryDevice := func(t *testing.T, minSoc, maxSoc float64) config.Device[api.Meter] {
		ctrl := gomock.NewController(t)

		var meter api.Meter
		batSocLimit := api.NewMockBatterySocLimiter(ctrl)
		batSocLimit.EXPECT().GetSocLimits().Return(minSoc, maxSoc).AnyTimes()

		bat := &struct {
			api.Meter
			api.BatterySocLimiter
		}{
			Meter:             meter,
			BatterySocLimiter: batSocLimit,
		}

		return config.NewStaticDevice(config.Named{}, api.Meter(bat))
	}

	site := &Site{log: util.NewLogger("foo")}
	capacity := 10.0 // kWh

	t.Run("soc below minSoc", func(t *testing.T) {
		soc := 15.0
		dev := newBatteryDevice(t, 20, 100)
		m := types.Measurement{Capacity: &capacity, Soc: &soc}

		req, _ := site.batteryRequest(dev, m, nil, 8, 15*time.Minute)

		assert.Equal(t, float32(1500), req.SMin)
		assert.Equal(t, float32(10000), req.SMax)
		assert.LessOrEqual(t, req.SMin, req.SInitial)
	})

	t.Run("soc above maxSoc", func(t *testing.T) {
		soc := 95.0
		dev := newBatteryDevice(t, 0, 80)
		m := types.Measurement{Capacity: &capacity, Soc: &soc}

		req, _ := site.batteryRequest(dev, m, nil, 8, 15*time.Minute)

		assert.Equal(t, float32(0), req.SMin)
		assert.Equal(t, float32(9500), req.SMax)
		assert.GreaterOrEqual(t, req.SMax, req.SInitial)
	})

	t.Run("soc within limits", func(t *testing.T) {
		soc := 50.0
		dev := newBatteryDevice(t, 20, 80)
		m := types.Measurement{Capacity: &capacity, Soc: &soc}

		req, _ := site.batteryRequest(dev, m, nil, 8, 15*time.Minute)

		assert.Equal(t, float32(2000), req.SMin)
		assert.Equal(t, float32(8000), req.SMax)
	})

	t.Run("empty maxSoc defaults to 100%", func(t *testing.T) {
		soc := 50.0
		dev := newBatteryDevice(t, 20, 0)
		m := types.Measurement{Capacity: &capacity, Soc: &soc}

		req, _ := site.batteryRequest(dev, m, nil, 8, 15*time.Minute)

		assert.Equal(t, float32(2000), req.SMin)
		assert.Equal(t, float32(10000), req.SMax)
	})
}

// gridChargeStub / batteryControllerStub implement the minimal capabilities
// batteryRequest inspects, so tests can vary GridChargeController without gomock.
type gridChargeStub bool

func (g gridChargeStub) AllowGridImport() bool { return bool(g) }

type batteryControllerStub struct{}

func (batteryControllerStub) SetBatteryMode(api.BatteryMode) error { return nil }

// TestBatteryRequestGridCharge verifies a controllable battery that forbids grid
// import is modelled with charge_from_grid off (solar charging and discharge-to-
// grid unaffected), while one that allows it - or lacks the capability - keeps
// grid charging.
func TestBatteryRequestGridCharge(t *testing.T) {
	site := &Site{log: util.NewLogger("foo")}
	capacity, soc := 10.0, 50.0
	m := types.Measurement{Capacity: &capacity, Soc: &soc}

	withGridCharge := func(allow bool) config.Device[api.Meter] {
		return config.NewStaticDevice(config.Named{}, api.Meter(&struct {
			api.Meter
			batteryControllerStub
			gridChargeStub
		}{gridChargeStub: gridChargeStub(allow)}))
	}

	req, _ := site.batteryRequest(withGridCharge(false), m, nil, 8, 15*time.Minute)
	assert.False(t, req.ChargeFromGrid, "allowGridImport=false -> no grid charging")
	assert.True(t, req.DischargeToGrid, "discharge-to-grid is unaffected")

	req, _ = site.batteryRequest(withGridCharge(true), m, nil, 8, 15*time.Minute)
	assert.True(t, req.ChargeFromGrid, "allowGridImport=true -> grid charging")

	// no GridChargeController capability -> grid charging as before
	plain := config.NewStaticDevice(config.Named{}, api.Meter(&struct {
		api.Meter
		batteryControllerStub
	}{}))
	req, _ = site.batteryRequest(plain, m, nil, 8, 15*time.Minute)
	assert.True(t, req.ChargeFromGrid, "no capability -> grid charging unchanged")
}

// TestBatteryRequestServeHomeFirst: the home battery asks the optimizer to break cost ties
// toward feeding the house exactly when "serve home first" is the selected secondary goal.
func TestBatteryRequestServeHomeFirst(t *testing.T) {
	capacity, soc := 10.0, 50.0
	m := types.Measurement{Capacity: &capacity, Soc: &soc}
	plain := config.NewStaticDevice(config.Named{}, api.Meter(&struct{ api.Meter }{}))
	controllable := config.NewStaticDevice(config.Named{}, api.Meter(&struct {
		api.Meter
		batteryControllerStub
	}{}))

	site := &Site{log: util.NewLogger("foo"), optimizerChargingStrategy: optimizerStrategyServeHomeFirst}
	req, _ := site.batteryRequest(plain, m, nil, 8, 15*time.Minute)
	assert.True(t, req.ServeHomeFirst, "serve home first selected")
	req, _ = site.batteryRequest(controllable, m, nil, 8, 15*time.Minute)
	assert.True(t, req.ServeHomeFirst, "independent of battery control")

	for _, strategy := range []optimizer.OptimizerStrategyChargingStrategy{
		optimizer.OptimizerStrategyChargingStrategyChargeBeforeExport,
		optimizer.OptimizerStrategyChargingStrategyAttenuateGridPeaks,
		optimizer.OptimizerStrategyChargingStrategyNone,
	} {
		site := &Site{log: util.NewLogger("foo"), optimizerChargingStrategy: string(strategy)}
		req, _ := site.batteryRequest(plain, m, nil, 8, 15*time.Minute)
		assert.False(t, req.ServeHomeFirst, "%s: other secondary goals leave the tie unordered", strategy)
	}
}

// TestOptimizerRequestChargingStrategy: serve home first is evcc's own secondary goal, not
// an optimizer charging strategy. The optimizer gets fill battery first underneath it, so
// the surplus the home-first commitment leaves reaches the car right away; every other
// goal passes through unchanged.
func TestOptimizerRequestChargingStrategy(t *testing.T) {
	assert.Equal(t, optimizer.OptimizerStrategyChargingStrategyChargeBeforeExport,
		(&Site{optimizerChargingStrategy: optimizerStrategyServeHomeFirst}).optimizerRequestChargingStrategy())

	for _, strategy := range []optimizer.OptimizerStrategyChargingStrategy{
		optimizer.OptimizerStrategyChargingStrategyChargeBeforeExport,
		optimizer.OptimizerStrategyChargingStrategyAttenuateDemandPeaks,
		optimizer.OptimizerStrategyChargingStrategyNone,
	} {
		assert.Equal(t, strategy, (&Site{optimizerChargingStrategy: string(strategy)}).optimizerRequestChargingStrategy())
	}

	// serve home first keeps the car's early-charge priority: the commitment decides where
	// the pack's energy goes, the priority only decides when the surplus moves
	assert.Equal(t, 2, (&Site{optimizerChargingStrategy: optimizerStrategyServeHomeFirst}).evChargePriority())

	// and it is a valid, selectable goal
	site := &Site{log: util.NewLogger("foo")}
	require.NoError(t, site.SetOptimizerChargingStrategy(optimizerStrategyServeHomeFirst))
	assert.Equal(t, optimizerStrategyServeHomeFirst, site.GetOptimizerChargingStrategy())
}

// charge goal for vehicles with and without known capacity/soc, see #32890
func TestLoadpointRequestChargeGoal(t *testing.T) {
	site := &Site{log: util.NewLogger("foo")}

	for _, tc := range []struct {
		name                  string
		capacity, soc         float64 // kWh, percent
		limitSoc              int     // percent
		limitEnergy, charged  float64 // kWh, Wh
		wantInitial, wantSMax float32 // Wh
	}{
		// soc limit: SMax reflects the 80% limit (40 kWh); no plan goal is applied
		{"soc limit", 50, 20, 80, 0, 0, 10000, 40000},
		{"no capacity, energy limit", 0, 0, 100, 10, 0, 0, 10000},
		{"no capacity, energy limit partially charged", 0, 0, 100, 10, 4000, 4000, 10000},
		{"no capacity, limit exceeded", 0, 0, 100, 10, 11000, 11000, 11000},
		{"capacity but no soc, energy limit", 50, 0, 100, 10, 0, 0, 10000},
		{"capacity but no soc, no energy limit", 50, 0, 100, 0, 0, 0, 50000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)

			v := api.NewMockVehicle(ctrl)
			v.EXPECT().Capacity().Return(tc.capacity).AnyTimes()
			v.EXPECT().GetTitle().Return("").AnyTimes()

			lp := loadpoint.NewMockAPI(ctrl)
			lp.EXPECT().GetVehicle().Return(v).AnyTimes()
			lp.EXPECT().GetSoc().Return(tc.soc).AnyTimes()
			lp.EXPECT().EffectiveLimitSoc().Return(tc.limitSoc).AnyTimes()
			lp.EXPECT().EffectiveMinSoc().Return(0).AnyTimes()
			lp.EXPECT().GetLimitEnergy().Return(tc.limitEnergy).AnyTimes()
			lp.EXPECT().GetChargedEnergy().Return(tc.charged).AnyTimes()
			lp.EXPECT().GetTitle().Return("lp").AnyTimes()
			lp.EXPECT().EffectiveMinPower().Return(1380.0).AnyTimes()
			lp.EXPECT().EffectiveMaxPower().Return(11000.0).AnyTimes()
			lp.EXPECT().GetMode().Return(api.ModePV).AnyTimes()
			lp.EXPECT().GetStatus().Return(api.StatusB).AnyTimes()
			lp.EXPECT().GetSmartCostLimit().Return(nil).AnyTimes()
			lp.EXPECT().EffectivePlanStrategy().Return(api.PlanStrategy{}).AnyTimes()
			lp.EXPECT().GetPlanGoals(gomock.Any()).Return(nil, false).AnyTimes()

			req, _ := site.loadpointRequest(lp, 8, 15*time.Minute, nil)

			assert.Equal(t, tc.wantInitial, req.SInitial)
			assert.Equal(t, tc.wantSMax, req.SMax)
			assert.False(t, req.ServeHomeFirst, "a car is fed, it does not serve the house")
		})
	}
}

// TestApplyPlanGoal covers the charge goal: only a real in-horizon plan goal is
// applied (a hard target at its own slot). With no plan - or a deadline outside
// the horizon - no goal is set; a plugged-in car is kept charged by its
// forecast-driving commitment (SDrainT) instead, never a forced horizon-end ceiling.
func TestApplyPlanGoal(t *testing.T) {
	site := &Site{log: util.NewLogger("foo")}
	const minLen = 8
	const capacity = 50.0 // kWh -> 50000 Wh at 100%

	newLp := func(t *testing.T) (*loadpoint.MockAPI, *api.MockVehicle) {
		ctrl := gomock.NewController(t)
		v := api.NewMockVehicle(ctrl)
		v.EXPECT().Capacity().Return(capacity).AnyTimes()

		lp := loadpoint.NewMockAPI(ctrl)
		lp.EXPECT().GetVehicle().Return(v).AnyTimes()
		// SoC-based state model (no session energy limit override)
		lp.EXPECT().GetLimitEnergy().Return(0.0).AnyTimes()
		lp.EXPECT().GetSoc().Return(20.0).AnyTimes()
		return lp, v
	}

	goalAt := func(d time.Duration, goal float64) loadpoint.PlanGoal {
		return loadpoint.PlanGoal{Time: time.Now().Add(d), Goal: goal}
	}

	t.Run("no plan goal -> no goal (pulled by energy value instead)", func(t *testing.T) {
		lp, _ := newLp(t)
		lp.EXPECT().GetPlanGoals(gomock.Any()).Return(nil, true).AnyTimes()

		bat := &optimizer.BatteryConfig{SMax: 40000}
		site.applyPlanGoal(lp, bat, minLen)

		assert.Nil(t, bat.SGoal, "no forced horizon-end goal without a plan")
		assert.Equal(t, float32(40000), bat.SMax, "SMax untouched")
	})

	t.Run("in-horizon plan goal -> real slot, no horizon-end goal", func(t *testing.T) {
		lp, _ := newLp(t)
		// soc plan goal 80% -> 40000 Wh, due in ~1h1m so it lands squarely in slot 4
		lp.EXPECT().GetPlanGoals(gomock.Any()).Return([]loadpoint.PlanGoal{goalAt(time.Hour+time.Minute, 80)}, true).AnyTimes()

		bat := &optimizer.BatteryConfig{SMax: 10000}
		site.applyPlanGoal(lp, bat, minLen)

		require.Len(t, bat.SGoal, minLen)
		assert.Equal(t, float32(40000), bat.SGoal[4], "goal at its real slot")
		assert.Zero(t, bat.SGoal[minLen-1], "no horizon-end fallback when a real goal is set")
		assert.Equal(t, float32(40000), bat.SMax, "SMax bumped to the real goal")
	})

	t.Run("every in-horizon plan -> goal at each slot", func(t *testing.T) {
		lp, _ := newLp(t)
		// a nearer plan the car meets right now must still hold after a later, higher one
		lp.EXPECT().GetPlanGoals(gomock.Any()).Return([]loadpoint.PlanGoal{
			goalAt(time.Hour+time.Minute, 100),
			goalAt(31*time.Minute, 60),
			goalAt(91*time.Minute, 50),
		}, true).AnyTimes()

		bat := &optimizer.BatteryConfig{SMax: 10000}
		site.applyPlanGoal(lp, bat, minLen)

		require.Len(t, bat.SGoal, minLen)
		assert.Equal(t, float32(30000), bat.SGoal[2])
		assert.Equal(t, float32(50000), bat.SGoal[4])
		assert.Equal(t, float32(25000), bat.SGoal[6])
		assert.Equal(t, float32(50000), bat.SMax, "SMax bumped to the highest goal")
	})

	t.Run("plans in the same slot -> highest goal", func(t *testing.T) {
		lp, _ := newLp(t)
		lp.EXPECT().GetPlanGoals(gomock.Any()).Return([]loadpoint.PlanGoal{
			goalAt(time.Hour+time.Minute, 80),
			goalAt(time.Hour+2*time.Minute, 60),
		}, true).AnyTimes()

		bat := &optimizer.BatteryConfig{}
		site.applyPlanGoal(lp, bat, minLen)

		assert.Equal(t, float32(40000), bat.SGoal[4])
	})

	t.Run("plan deadline beyond horizon -> no goal", func(t *testing.T) {
		lp, _ := newLp(t)
		// soc plan goal 80%, but due in 24h -> slot 96 >> minLen
		lp.EXPECT().GetPlanGoals(gomock.Any()).Return([]loadpoint.PlanGoal{goalAt(24*time.Hour, 80)}, true).AnyTimes()

		bat := &optimizer.BatteryConfig{SMax: 40000}
		site.applyPlanGoal(lp, bat, minLen)

		assert.Nil(t, bat.SGoal, "deadline outside the horizon adds no goal")
		assert.Equal(t, float32(40000), bat.SMax)
	})

	t.Run("no vehicle -> no fallback", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lp := loadpoint.NewMockAPI(ctrl)
		lp.EXPECT().GetPlanGoals(gomock.Any()).Return(nil, false).AnyTimes()
		lp.EXPECT().GetVehicle().Return(nil).AnyTimes()

		bat := &optimizer.BatteryConfig{SMax: 40000}
		site.applyPlanGoal(lp, bat, minLen)

		assert.Nil(t, bat.SGoal)
		assert.Equal(t, float32(40000), bat.SMax, "SMax untouched without a vehicle")
	})

	t.Run("energy plan -> goal in Wh", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		lp := loadpoint.NewMockAPI(ctrl)
		lp.EXPECT().GetVehicle().Return(nil).AnyTimes()
		lp.EXPECT().GetPlanGoals(gomock.Any()).Return([]loadpoint.PlanGoal{goalAt(time.Hour+time.Minute, 20)}, false).AnyTimes()

		bat := &optimizer.BatteryConfig{SMax: 10000}
		site.applyPlanGoal(lp, bat, minLen)

		require.Len(t, bat.SGoal, minLen)
		assert.Equal(t, float32(20000), bat.SGoal[4])
		assert.Equal(t, float32(20000), bat.SMax)
	})
}

func TestEvChargeSource(t *testing.T) {
	// a surplus charge is reclassified as battery-fed only when the pack is
	// discharging and nothing is exported
	assert.Equal(t, sourceBattery, evChargeSource(sourceSurplus, true, false), "pack discharging, no export -> battery")
	assert.Equal(t, sourceSurplus, evChargeSource(sourceSurplus, false, false), "pack idle -> surplus")
	assert.Equal(t, sourceSurplus, evChargeSource(sourceSurplus, true, true), "exporting -> real surplus, not battery")
	// grid stays grid: an importing slot is never battery-fed
	assert.Equal(t, sourceGrid, evChargeSource(sourceGrid, true, false), "grid source unchanged")
}

func TestOptimizerChargingStrategy(t *testing.T) {
	site := &Site{log: util.NewLogger("foo")}

	// default when unset
	assert.Equal(t, defaultOptimizerChargingStrategy, site.GetOptimizerChargingStrategy())

	// invalid value rejected, strategy unchanged
	require.Error(t, site.SetOptimizerChargingStrategy("bogus"))
	assert.Equal(t, defaultOptimizerChargingStrategy, site.GetOptimizerChargingStrategy())

	// valid change is applied (re-trigger is gated on sponsor/enabled, not unit-tested here)
	require.NoError(t, site.SetOptimizerChargingStrategy(string(optimizer.OptimizerStrategyChargingStrategyAttenuateGridPeaks)))
	assert.Equal(t, "attenuate_grid_peaks", site.GetOptimizerChargingStrategy())
}

func TestGridExportLimit(t *testing.T) {
	site := &Site{log: util.NewLogger("foo")}

	// disabled by default
	assert.Equal(t, 0.0, site.GetGridExportLimit())

	// negative value rejected, limit unchanged
	require.Error(t, site.SetGridExportLimit(-1))
	assert.Equal(t, 0.0, site.GetGridExportLimit())

	require.NoError(t, site.SetGridExportLimit(7000))
	assert.Equal(t, 7000.0, site.GetGridExportLimit())
}

func TestBlendMeasured(t *testing.T) {
	slots := []float64{100, 100, 100, 100, 100, 100}
	blendMeasured(slots, 200, 4)
	assert.Equal(t, []float64{200, 175, 150, 125, 100, 100}, slots)

	// fewer slots than decay length
	short := []float32{100, 100}
	blendMeasured(short, 200, 4)
	assert.Equal(t, []float32{200, 175}, short)
}

func TestBlendScale(t *testing.T) {
	slots := []float32{100, 100, 100, 100, 100, 100}
	blendScale(slots, 2, 4)
	assert.Equal(t, []float32{200, 175, 150, 125, 100, 100}, slots)

	// fewer slots than decay length
	short := []float64{100, 100}
	blendScale(short, 0.5, 4)
	assert.Equal(t, []float64{50, 62.5}, short)
}

// TestEvChargePriority verifies the EV gets max charge priority (soft early-solar
// bias) under charge_before_export (the default) and none under other strategies.
func TestEvChargePriority(t *testing.T) {
	// default strategy is charge_before_export
	assert.Equal(t, 2, (&Site{}).evChargePriority())

	assert.Equal(t, 2, (&Site{optimizerChargingStrategy: string(optimizer.OptimizerStrategyChargingStrategyChargeBeforeExport)}).evChargePriority())

	assert.Equal(t, 0, (&Site{optimizerChargingStrategy: string(optimizer.OptimizerStrategyChargingStrategyNone)}).evChargePriority())
	assert.Equal(t, 0, (&Site{optimizerChargingStrategy: string(optimizer.OptimizerStrategyChargingStrategyAttenuateGridPeaks)}).evChargePriority())
}

// TestLoadpointOptimizerAction verifies both optimizer loadpoint actions reach the
// loadpoint under charge_before_export (the default) — "stop" is no longer suppressed.
func TestLoadpointOptimizerAction(t *testing.T) {
	site := &Site{loadpoints: []*Loadpoint{
		NewLoadpoint(util.NewLogger("lp0"), nil),
		NewLoadpoint(util.NewLogger("lp1"), nil),
	}}
	require.Equal(t, 2, site.evChargePriority()) // default strategy is charge_before_export

	site.setSuggestions(map[string]types.Suggestion{
		loadpointKey(0): {Action: actionStop},
		loadpointKey(1): {Action: actionCharge, Charge: 7000, Source: sourceGrid},
	})

	// stop is honored (previously dropped under charge_before_export); charge too,
	// carrying the optimizer's requested power and source
	action, power, source := site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action)
	assert.Zero(t, power)
	assert.Empty(t, source)

	action, power, source = site.loadpointOptimizerAction(1)
	assert.Equal(t, actionCharge, action)
	assert.Equal(t, 7000.0, power)
	assert.Equal(t, sourceGrid, source)

	// no suggestion => no action
	site.setSuggestions(nil)
	action, power, source = site.loadpointOptimizerAction(0)
	assert.Empty(t, action)
	assert.Zero(t, power)
	assert.Empty(t, source)
}

// TestLoadpointOptimizerActionStale drops a suggestion computed before the car
// connected: a disconnected loadpoint's stored "stop" must not pause a car that
// just plugged in before the connect-triggered re-optimization lands. A stop
// computed after the connection is a real defer and is still honored.
func TestLoadpointOptimizerActionStale(t *testing.T) {
	site := &Site{loadpoints: []*Loadpoint{
		NewLoadpoint(util.NewLogger("lp0"), nil),
	}}

	connectedAt := time.Now()
	site.loadpoints[0].connectedTime = connectedAt

	site.setSuggestions(map[string]types.Suggestion{
		loadpointKey(0): {Action: actionStop},
	})

	// suggestion computed before the connection => ignored, so the just-connected
	// car falls through to normal PV/welcome charging instead of stopping
	site.suggestionsComputedAt = connectedAt.Add(-time.Minute)
	action, power, source := site.loadpointOptimizerAction(0)
	assert.Empty(t, action, "pre-connect suggestion is ignored")
	assert.Zero(t, power)
	assert.Empty(t, source)

	// a fresh run reflecting the connection is applied
	site.setSuggestions(map[string]types.Suggestion{
		loadpointKey(0): {Action: actionCharge, Charge: 9600, Source: sourceGrid},
	})
	site.suggestionsComputedAt = connectedAt.Add(time.Minute)
	action, power, source = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionCharge, action, "post-connect suggestion is applied")
	assert.Equal(t, 9600.0, power)
	assert.Equal(t, sourceGrid, source)

	// a legit stop computed after the connection is a real defer, not stale
	site.setSuggestions(map[string]types.Suggestion{
		loadpointKey(0): {Action: actionStop},
	})
	site.suggestionsComputedAt = connectedAt.Add(time.Minute)
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action, "post-connect stop is honored")

	// suggestions set outside an optimizer run (zero computedAt) are never stale
	site.suggestionsComputedAt = time.Time{}
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action, "unstamped suggestion is applied")
}

// TestBatteryBoostReserveGate stops a battery-sourced boost once the home
// battery reaches its planned reserve, so the car doesn't keep charging from the
// grid at peak prices. It trips when the pack soc reaches the target, or in real
// time when the boost's own draw spills to the grid, and latches for the slot.
func TestBatteryBoostReserveGate(t *testing.T) {
	newSite := func() *Site {
		s := &Site{
			log:        util.NewLogger("foo"),
			loadpoints: []*Loadpoint{NewLoadpoint(util.NewLogger("lp0"), nil)},
			batteryMeters: []config.Device[api.Meter]{
				config.NewStaticDevice(config.Named{Name: "home"}, api.Meter(&recordingBatteryController{})),
			},
			batterySchedules: map[string][]batterySlot{
				batteryKey("home"): {{until: time.Now().Add(time.Hour), action: "normal", soc: 55}},
			},
		}
		s.battery.Soc = 59 // above the 55% reserve
		s.setSuggestions(map[string]types.Suggestion{
			loadpointKey(0): {Action: actionCharge, Charge: 9600, Source: sourceBattery},
		})
		return s
	}

	// pack above reserve, not importing => boost charges
	site := newSite()
	action, power, source := site.loadpointOptimizerAction(0)
	assert.Equal(t, actionCharge, action, "above reserve: boost charges")
	assert.Equal(t, 9600.0, power)
	assert.Equal(t, sourceBattery, source, "source stays battery so the reserve stays pinned")

	// pack soc reached the reserve and the plan has no next slot to move on to =>
	// stop, keeping the source pinned so the pack holds at the target instead of
	// draining further
	site = newSite()
	site.battery.Soc = 55
	action, _, source = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action, "at reserve: boost stops")
	assert.Equal(t, sourceBattery, source, "source stays battery so the pack holds at the reserve")
	assert.Contains(t, site.replanSlots, "battery boost reached reserve", "the stopped boost re-plans")

	// pack still above reserve but the boost's draw is already spilling to the grid
	// (the pack stopped discharging early) => real-time stop
	site = newSite()
	site.loadpoints[0].chargePower = 9600 // car is drawing the boost power
	site.gridPower = 9000                 // most of it from the grid
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action, "grid import while boosting stops the car")

	// pre-boost grid import (car not drawing yet) must NOT trip the guard, or it
	// would latch the car off before the boost even starts
	site = newSite()
	site.loadpoints[0].chargePower = 0 // boost hasn't started drawing
	site.gridPower = 9000              // e.g. leftover import from the prior hold state
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionCharge, action, "pre-boost grid import doesn't false-trip")

	// latch: once tripped this slot the car stays stopped even if the pack recovers
	site = newSite()
	site.battery.Soc = 55
	action, _, _ = site.loadpointOptimizerAction(0) // trips + latches
	require.Equal(t, actionStop, action)
	site.battery.Soc = 59 // pack "recovered" (shouldn't matter within the slot)
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action, "latched for the slot")

	// preemptive margin: with pack capacity and the control interval known, the boost
	// stops a lookahead-margin ABOVE the raw reserve so the pack never crosses it mid-
	// cycle and spills to the grid. At 9600W over 2×90s on an 11kWh pack the margin is
	// ~4.4% SoC, so a boost is stopped by ~59.4% even though the reserve is 55%.
	site = newSite()
	site.battery.Capacity = 11 // kWh
	site.interval = 90 * time.Second
	site.battery.Soc = 58 // above the 55% reserve but within the reaction-lag margin
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action, "within the reaction-lag margin: boost stops before the pack crosses the reserve")

	// comfortably above reserve+margin => still charging (margin doesn't stop it early)
	site = newSite()
	site.battery.Capacity = 11
	site.interval = 90 * time.Second
	site.battery.Soc = 62
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionCharge, action, "well above reserve+margin: boost keeps charging")

	// a grid-sourced charge is never gated by the home battery reserve
	site = newSite()
	site.battery.Soc = 10 // well below any reserve
	site.setSuggestions(map[string]types.Suggestion{
		loadpointKey(0): {Action: actionCharge, Charge: 7000, Source: sourceGrid},
	})
	action, _, source = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionCharge, action, "grid-sourced charge isn't gated")
	assert.Equal(t, sourceGrid, source)
}

// TestBatteryBoostReserveAdvancesPlan covers the handoff from a boost to the plan's
// next slot: once the pack reaches the boost slot's reserve the car follows the next
// slot's suggestion right away (a smaller battery draw, surplus charging, or a stop)
// instead of switching off until the slot boundary, and the pack's target moves on
// with it. The advance sticks for the slot and does not re-plan.
func TestBatteryBoostReserveAdvancesPlan(t *testing.T) {
	boundary := time.Now().Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	newSite := func(next types.Suggestion) *Site {
		s := &Site{
			log:        util.NewLogger("foo"),
			loadpoints: []*Loadpoint{NewLoadpoint(util.NewLogger("lp0"), nil)},
			batteryMeters: []config.Device[api.Meter]{
				config.NewStaticDevice(config.Named{Name: "home"}, api.Meter(&recordingBatteryController{})),
			},
			batterySchedules: map[string][]batterySlot{
				batteryKey("home"): {
					{until: boundary, action: "normal", soc: 55, startSoc: 70},
					{until: boundary.Add(tariff.SlotDuration), action: "normal", soc: 50, startSoc: 55},
				},
			},
		}
		s.battery.Soc = 59
		s.setSuggestionPlans(map[string][]suggestionSlot{
			loadpointKey(0): {
				{until: boundary, suggestion: types.Suggestion{Action: actionCharge, Charge: 9600, Source: sourceBattery}},
				{until: boundary.Add(tariff.SlotDuration), suggestion: next},
			},
		})
		return s
	}

	// above the reserve: the boost runs at its own power
	site := newSite(types.Suggestion{Action: actionCharge, Charge: 3000, Source: sourceBattery})
	action, power, _ := site.loadpointOptimizerAction(0)
	require.Equal(t, actionCharge, action)
	assert.Equal(t, 9600.0, power)
	assert.Equal(t, 55.0, site.optimizerBatteryTargetSoc("home"))

	// at the reserve: the next slot's smaller boost takes over, with its target
	site.battery.Soc = 55
	action, power, source := site.loadpointOptimizerAction(0)
	assert.Equal(t, actionCharge, action, "the car keeps charging")
	assert.Equal(t, 3000.0, power, "at the next slot's power")
	assert.Equal(t, sourceBattery, source)
	assert.Equal(t, 50.0, site.optimizerBatteryTargetSoc("home"), "the pack's target moves on too")
	assert.Empty(t, site.replanSlots, "following the plan early is not a deviation")

	// the advance sticks for the slot even if the pack reads above the reserve again
	site.battery.Soc = 56
	_, power, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, 3000.0, power, "stays on the next slot")

	// the next slot's reserve reached as well, with nothing planned beyond: stop
	site.battery.Soc = 50
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action)

	// a next slot on surplus is followed as is: it is not gated by the reserve
	site = newSite(types.Suggestion{Action: actionCharge, Charge: 2000, Source: sourceSurplus})
	site.battery.Soc = 55
	action, power, source = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionCharge, action)
	assert.Equal(t, 2000.0, power)
	assert.Equal(t, sourceSurplus, source)

	// a next slot that stops the car stops it
	site = newSite(types.Suggestion{Action: actionStop})
	site.battery.Soc = 55
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action)
}

// TestLoadpointChargeSource classifies a loadpoint charge as grid-sourced when
// the plan imports, else surplus; a non-charge action carries no source.
func TestLoadpointChargeSource(t *testing.T) {
	charging := optimizer.BatteryResult{ChargingPower: []float32{11000}, DischargingPower: []float32{0}}

	// charging while the plan imports => grid-sourced fast charge
	grid := currentSlotSuggestion(batteryDetail{Type: batteryTypeVehicle}, charging, 0, true, false, false, 1)
	assert.Equal(t, actionCharge, grid.Action)
	assert.Equal(t, sourceGrid, grid.Source)

	// charging without import => surplus
	surplus := currentSlotSuggestion(batteryDetail{Type: batteryTypeVehicle}, charging, 0, false, true, false, 1)
	assert.Equal(t, actionCharge, surplus.Action)
	assert.Equal(t, sourceSurplus, surplus.Source)

	// stop carries no source
	idle := optimizer.BatteryResult{ChargingPower: []float32{0}, DischargingPower: []float32{0}}
	stop := currentSlotSuggestion(batteryDetail{Type: batteryTypeVehicle}, idle, 0, true, false, false, 1)
	assert.Equal(t, actionStop, stop.Action)
	assert.Empty(t, stop.Source)
}

func TestCurrentSlotSuggestion(t *testing.T) {
	// slotHours 1 makes the per-slot Wh values map 1:1 to W
	for _, tc := range []struct {
		name              string
		typ               batteryType
		charge, disch     float32
		importing, export bool
		noImport          bool
		want              string
	}{
		{"battery grid charge", batteryTypeBattery, 3000, 0, true, false, false, "charge"},
		{"battery pv charge (no import)", batteryTypeBattery, 3000, 0, false, true, false, "normal"},
		{"battery hold (idle while importing)", batteryTypeBattery, 0, 0, true, false, false, "hold"},
		{"battery holdcharge (idle while exporting)", batteryTypeBattery, 0, 0, false, true, false, "holdcharge"},
		{"battery discharge (self-consumption while importing)", batteryTypeBattery, 0, 2000, true, false, false, "normal"},
		{"battery grid discharge (discharge while exporting)", batteryTypeBattery, 0, 2000, false, true, false, "discharge"},
		{"battery idle balanced", batteryTypeBattery, 0, 0, false, false, false, "normal"},
		// self-consumption (no grid import): hold collapses to normal (backup blocks
		// discharge → import); holdcharge reaches its controller (SoC-dependent
		// spike-and-hold vs top-off); discharge unaffected
		{"no-import hold => normal", batteryTypeBattery, 0, 0, true, false, true, "normal"},
		{"no-import holdcharge stays holdcharge", batteryTypeBattery, 0, 0, false, true, true, "holdcharge"},
		{"no-import discharge unaffected", batteryTypeBattery, 0, 2000, false, true, true, "discharge"},
		{"loadpoint charge", batteryTypeLoadpoint, 11000, 0, false, false, false, "charge"},
		{"loadpoint stop", batteryTypeLoadpoint, 0, 0, false, false, false, "stop"},
		{"vehicle below threshold is stop", batteryTypeVehicle, 40, 0, false, false, false, "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := optimizer.BatteryResult{
				ChargingPower:    []float32{tc.charge},
				DischargingPower: []float32{tc.disch},
			}
			s := currentSlotSuggestion(batteryDetail{Type: tc.typ}, res, 0, tc.importing, tc.export, tc.noImport, 1)
			assert.Equal(t, tc.want, s.Action)
			assert.InDelta(t, tc.charge, s.Charge, 1e-3)
			assert.InDelta(t, tc.disch, s.Discharge, 1e-3)
		})
	}

	// no result yields an empty suggestion
	assert.Empty(t, currentSlotSuggestion(batteryDetail{Type: batteryTypeBattery}, optimizer.BatteryResult{}, 0, true, false, false, 1))

	// idx selects the slot: slot 1 discharging while exporting => discharge
	res := optimizer.BatteryResult{ChargingPower: []float32{0, 0}, DischargingPower: []float32{0, 2000}}
	assert.Equal(t, "normal", currentSlotSuggestion(batteryDetail{Type: batteryTypeBattery}, res, 0, false, false, false, 1).Action)
	assert.Equal(t, actionDischarge, currentSlotSuggestion(batteryDetail{Type: batteryTypeBattery}, res, 1, false, true, false, 1).Action)
	// out-of-range idx => empty
	assert.Empty(t, currentSlotSuggestion(batteryDetail{Type: batteryTypeBattery}, res, 2, false, true, false, 1))
}

// TestIsSpikeAction covers the export-mode predicate used for front-running.
func TestIsSpikeAction(t *testing.T) {
	assert.True(t, isSpikeAction(api.BatteryHoldCharge.String()))
	assert.True(t, isSpikeAction(actionDischarge))
	assert.False(t, isSpikeAction(api.BatteryNormal.String()))
	assert.False(t, isSpikeAction(api.BatteryCharge.String()))
	assert.False(t, isSpikeAction(actionStop))
}

// TestCurrentBatteryAction resolves the current battery-mode action from the
// plan and front-runs an export-mode transition near a slot boundary,
// independent of when the optimizer last ran.
func TestCurrentBatteryAction(t *testing.T) {
	now := time.Now()
	slot := tariff.SlotDuration

	// far from the boundary: the current slot's action
	far := []batterySlot{
		{until: now.Add(10 * time.Minute), action: api.BatteryNormal.String()},
		{until: now.Add(10*time.Minute + slot), action: actionDischarge},
	}
	assert.Equal(t, api.BatteryNormal.String(), currentBatteryAction(far, now, 0))

	// within the lead and the next slot flips into export: adopt it now
	near := []batterySlot{
		{until: now.Add(time.Minute), action: api.BatteryNormal.String()},
		{until: now.Add(time.Minute + slot), action: actionDischarge},
	}
	assert.Equal(t, actionDischarge, currentBatteryAction(near, now, 0))

	// both sides already export (no operating-mode pickup): stay put
	sameExport := []batterySlot{
		{until: now.Add(time.Minute), action: api.BatteryHoldCharge.String()},
		{until: now.Add(time.Minute + slot), action: actionDischarge},
	}
	assert.Equal(t, api.BatteryHoldCharge.String(), currentBatteryAction(sameExport, now, 0))

	// no next slot: nothing to front-run
	single := []batterySlot{{until: now.Add(time.Minute), action: api.BatteryNormal.String()}}
	assert.Equal(t, api.BatteryNormal.String(), currentBatteryAction(single, now, 0))

	// a boundary already passed: the next slot is now the current one
	stale := []batterySlot{
		{until: now.Add(-time.Minute), action: api.BatteryNormal.String()},
		{until: now.Add(10 * time.Minute), action: actionDischarge},
	}
	assert.Equal(t, actionDischarge, currentBatteryAction(stale, now, 0))

	// empty plan: no action
	assert.Empty(t, currentBatteryAction(nil, now, 0))
}

// TestCurrentBatterySlotSocAdvance covers the soc front-run: a discharge slot
// whose end-of-slot target the pack has already reached hands off to the
// optimizer's next slot immediately, well before the time boundary, so the house
// is covered from the pack instead of importing until the boundary.
func TestCurrentBatterySlotSocAdvance(t *testing.T) {
	now := time.Now()
	slot := tariff.SlotDuration

	// discharge-to-50 this slot, then HoldCharge (cover the house from the pack).
	// The time boundary is far off, so time resolution alone stays on discharge.
	plan := []batterySlot{
		{until: now.Add(10 * time.Minute), action: actionDischarge, soc: 50},
		{until: now.Add(10*time.Minute + slot), action: api.BatteryHoldCharge.String(), soc: 50},
	}

	// target not yet reached: keep discharging, reserve pinned at the target
	assert.Equal(t, actionDischarge, currentBatteryAction(plan, now, 55))
	if s, ok := currentBatterySlot(plan, now, 55); assert.True(t, ok) {
		assert.Equal(t, actionDischarge, s.action)
		assert.Equal(t, 50.0, s.soc)
	}

	// target reached mid-slot: advance to HoldCharge now, don't wait for `until`
	assert.Equal(t, api.BatteryHoldCharge.String(), currentBatteryAction(plan, now, 50))
	assert.Equal(t, api.BatteryHoldCharge.String(), currentBatteryAction(plan, now, 48))

	// unknown soc (0): no advancement, fall back to time resolution
	assert.Equal(t, actionDischarge, currentBatteryAction(plan, now, 0))

	// consecutive discharge slots: a met first target advances to the next
	// discharge (keep exporting toward the lower target), not straight to hold
	steps := []batterySlot{
		{until: now.Add(10 * time.Minute), action: actionDischarge, soc: 50},
		{until: now.Add(10*time.Minute + slot), action: actionDischarge, soc: 45},
		{until: now.Add(10*time.Minute + 2*slot), action: api.BatteryHoldCharge.String(), soc: 45},
	}
	assert.Equal(t, actionDischarge, currentBatteryAction(steps, now, 50)) // 50 met, 45 not
	if s, ok := currentBatterySlot(steps, now, 50); assert.True(t, ok) {
		assert.Equal(t, 45.0, s.soc) // reserve now tracks the next target
	}
	assert.Equal(t, api.BatteryHoldCharge.String(), currentBatteryAction(steps, now, 45)) // both met

	// charge is soc-gated symmetrically: a reached charge target advances
	charge := []batterySlot{
		{until: now.Add(10 * time.Minute), action: api.BatteryCharge.String(), soc: 80},
		{until: now.Add(10*time.Minute + slot), action: api.BatteryNormal.String(), soc: 80},
	}
	assert.Equal(t, api.BatteryCharge.String(), currentBatteryAction(charge, now, 79))
	assert.Equal(t, api.BatteryNormal.String(), currentBatteryAction(charge, now, 80))

	// a holding slot is never soc-satisfied: it stays until its time boundary
	held := []batterySlot{
		{until: now.Add(10 * time.Minute), action: api.BatteryHoldCharge.String(), soc: 50},
		{until: now.Add(10*time.Minute + slot), action: actionDischarge, soc: 45},
	}
	assert.Equal(t, api.BatteryHoldCharge.String(), currentBatteryAction(held, now, 40))
}

// TestSlotSocPercent converts the optimizer's planned end-of-slot SoC (Wh) to a
// percentage of capacity, 0 for unknown capacity or an out-of-range slot.
func TestSlotSocPercent(t *testing.T) {
	res := optimizer.BatteryResult{StateOfCharge: []float32{6750, 5400}}
	req := optimizer.BatteryConfig{SCapacity: 13500}

	assert.InDelta(t, 50.0, slotSocPercent(req, res, 0), 1e-6)
	assert.InDelta(t, 40.0, slotSocPercent(req, res, 1), 1e-6)
	assert.Zero(t, slotSocPercent(optimizer.BatteryConfig{}, res, 0), "unknown capacity => 0")
	assert.Zero(t, slotSocPercent(req, res, 5), "out-of-range slot => 0")
}

// TestBatteryModePlanSoc verifies the plan carries the optimizer's planned
// end-of-slot SoC alongside the action.
func TestBatteryModePlanSoc(t *testing.T) {
	now := time.Now()
	firstBoundary := now.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	dt := []int{900, 900}

	req := optimizer.BatteryConfig{SCapacity: 13500}
	batRes := optimizer.BatteryResult{
		ChargingPower:    []float32{0, 0},
		DischargingPower: []float32{2000, 2000},
		StateOfCharge:    []float32{6750, 5400},
	}
	res := optimizer.OptimizationResult{GridExport: []float32{2000, 2000}}

	plan := batteryModePlan(batteryDetail{Type: batteryTypeBattery}, req, batRes, res, dt, firstBoundary, false)
	require.Len(t, plan, 2)
	assert.Equal(t, actionDischarge, plan[0].action)
	assert.InDelta(t, 50.0, plan[0].soc, 1e-6)
	assert.InDelta(t, 40.0, plan[1].soc, 1e-6)
}

// TestGridImportAt sums the regular grid import and the overshoot. When a grid
// import limit is set and exceeded, the optimizer books the excess into the
// overshoot field instead of grid_import, which then reads ~0.
func TestGridImportAt(t *testing.T) {
	res := optimizer.OptimizationResult{
		GridImport:          []float32{0, 5, 0},
		GridImportOvershoot: []float32{100, 0, 0},
	}
	assert.Equal(t, float32(100), gridImportAt(res, 0), "import booked entirely in overshoot")
	assert.Equal(t, float32(5), gridImportAt(res, 1), "regular import")
	assert.Zero(t, gridImportAt(res, 2), "neither field imports")
	assert.Zero(t, gridImportAt(res, 9), "slot out of range")

	// overshoot may be absent or shorter than grid_import
	res2 := optimizer.OptimizationResult{GridImport: []float32{7, 8}}
	assert.Equal(t, float32(8), gridImportAt(res2, 1))
}

// TestChosenImportAt covers stripping the import a battery's indivisible minimum
// charge power forced out of the slot's grid import. The cases are the four import
// shapes observed in a live plan: a car held at its 1840W minimum against three
// different surpluses, and a slot where the plan pushed the car above its minimum.
func TestChosenImportAt(t *testing.T) {
	// slot geometry: the first slot is a partial 828s, the rest full quarter hours
	const (
		partial = 828
		full    = 900
	)

	for _, tc := range []struct {
		name          string
		dt            int
		solar, home   float32 // Wh in the slot
		evCharge      float32 // Wh the plan charges the car
		gridImport    float32 // Wh
		wantChosenW   float64
		wantImporting bool
	}{
		// The car's 1840W minimum outruns the surplus, so the shortfall has to be
		// imported - the plan did not choose it. Import and forced amount agree to
		// within the solver's rounding, leaving nothing chosen.
		{"forced by ev minimum, partial slot", partial, 472, 135, 423, 87, 4.3, false},
		{"forced by ev minimum", full, 695, 261, 460, 26, 0, false},
		{"forced by ev minimum, exact", full, 575, 174, 460, 59, 0, false},

		// Same order of magnitude of import as the case above, but here the plan put
		// the car well above its minimum with surplus to spare: the import is a
		// genuine choice and must survive.
		{"chosen while charging above the minimum", full, 997, 199, 855, 58, 232, true},

		// Nothing charging, so no minimum forces anything: the whole import is the
		// plan choosing grid over the pack.
		{"nothing charging", full, 0, 114, 0, 114, 456, true},

		// Surplus covers the minimum outright, so an import alongside it is chosen.
		{"surplus covers the minimum", full, 2000, 200, 460, 120, 480, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := optimizer.OptimizationInput{
				TimeSeries: optimizer.TimeSeries{
					Dt: []int{tc.dt},
					Ft: []float32{tc.solar},
					Gt: []float32{tc.home},
				},
				Batteries: []optimizer.BatteryConfig{
					{CMin: 1840}, // vehicle
					{CMin: 0},    // home battery, no minimum
				},
			}
			res := optimizer.OptimizationResult{
				GridImport: []float32{tc.gridImport},
				Batteries: []optimizer.BatteryResult{
					{ChargingPower: []float32{tc.evCharge}},
					{ChargingPower: []float32{0}},
				},
			}

			chosen := chosenImportAt(req, res, 0)
			assert.InDelta(t, tc.wantChosenW, chosen, 1, "chosen import")
			assert.Equal(t, tc.wantImporting, chosen > suggestionThreshold, "reads as importing")
		})
	}
}

// TestChosenImportAtBounds covers the out-of-range and degenerate inputs.
func TestChosenImportAtBounds(t *testing.T) {
	req := optimizer.OptimizationInput{
		TimeSeries: optimizer.TimeSeries{Dt: []int{900}, Ft: []float32{0}, Gt: []float32{500}},
	}
	res := optimizer.OptimizationResult{GridImport: []float32{500}}

	assert.Equal(t, float64(2000), chosenImportAt(req, res, 0), "no batteries: nothing forced")
	assert.Zero(t, chosenImportAt(req, res, 1), "slot past the horizon")
	assert.Zero(t, chosenImportAt(req, res, -1), "negative slot")

	// a zero-length slot can't be converted to power
	zero := optimizer.OptimizationInput{TimeSeries: optimizer.TimeSeries{Dt: []int{0}}}
	assert.Zero(t, chosenImportAt(zero, res, 0))

	// missing forecast series must not panic or over-credit surplus
	bare := optimizer.OptimizationInput{
		TimeSeries: optimizer.TimeSeries{Dt: []int{900}},
		Batteries:  []optimizer.BatteryConfig{{CMin: 1840}},
	}
	bareRes := optimizer.OptimizationResult{
		GridImport: []float32{460},
		Batteries:  []optimizer.BatteryResult{{ChargingPower: []float32{460}}},
	}
	assert.Zero(t, chosenImportAt(bare, bareRes, 0), "all of it forced with no surplus known")
}

// TestChosenImportEvSource is the circularity this guards: a car pinned at its own
// minimum charge power manufactures the slot's import, and reading that import back
// as "the plan imports" would classify the car as grid-fed - which makes PV mode hold
// it at that minimum regardless of the surplus actually available.
func TestChosenImportEvSource(t *testing.T) {
	// the live slot: 1840W car minimum, 472Wh solar, 135Wh home, 87Wh imported
	req := optimizer.OptimizationInput{
		TimeSeries: optimizer.TimeSeries{Dt: []int{828}, Ft: []float32{472}, Gt: []float32{135}},
		Batteries:  []optimizer.BatteryConfig{{CMin: 1840}},
	}
	res := optimizer.OptimizationResult{
		GridImport: []float32{87},
		Batteries: []optimizer.BatteryResult{
			{ChargingPower: []float32{423}, DischargingPower: []float32{0}},
		},
	}
	slotHours := 828.0 / 3600

	// raw import is nonzero, so the unfiltered flag would say "importing"
	require.Positive(t, gridImportAt(res, 0))

	importing := chosenImportAt(req, res, 0) > suggestionThreshold
	assert.False(t, importing, "an ev-minimum-forced import is not a chosen import")

	s := currentSlotSuggestion(batteryDetail{Type: batteryTypeVehicle},
		res.Batteries[0], 0, importing, false, false, slotHours)
	assert.Equal(t, actionCharge, s.Action)
	assert.Equal(t, sourceSurplus, s.Source, "surplus-fed, not a grid fast charge")

	// the same slot with the car pushed above its minimum is a real import and still
	// classifies as grid-fed
	res.Batteries[0].ChargingPower[0] = 855
	res.GridImport[0] = 300
	importing = chosenImportAt(req, res, 0) > suggestionThreshold
	assert.True(t, importing, "import beyond the forced part is a choice")
	s = currentSlotSuggestion(batteryDetail{Type: batteryTypeVehicle},
		res.Batteries[0], 0, importing, false, false, slotHours)
	assert.Equal(t, sourceGrid, s.Source)
}

// TestBatteryModePlanImportOvershoot verifies that import booked entirely into
// the overshoot field (a grid import limit exceeded) is still recognised as
// importing: an idle pack while importing must HOLD, not read as idle-balanced
// "normal".
func TestBatteryModePlanImportOvershoot(t *testing.T) {
	now := time.Now()
	firstBoundary := now.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	dt := []int{900}

	req := optimizer.BatteryConfig{SCapacity: 13500}
	batRes := optimizer.BatteryResult{
		ChargingPower:    []float32{0},
		DischargingPower: []float32{0},
		StateOfCharge:    []float32{6750},
	}
	// grid_import ~0, the real import sits in the overshoot field
	res := optimizer.OptimizationResult{
		GridImport:          []float32{0},
		GridImportOvershoot: []float32{2000},
	}

	// noGridImport=false isolates the importing signal (hold is not remapped to
	// normal); an idle pack while importing holds
	plan := batteryModePlan(batteryDetail{Type: batteryTypeBattery}, req, batRes, res, dt, firstBoundary, false)
	require.Len(t, plan, 1)
	assert.Equal(t, "hold", plan[0].action)
}

// TestBatteryModePlanHoldWithinExport verifies that a HOLD slot opening onto an
// export slot is relabeled to HoldCharge. Within an export peak the pack meets a
// slot's discharge target early, so the slot's tail reads idle+import → HOLD;
// left as HOLD it drops the Powerwall to backup mode and imports the house load
// from the grid mid-peak. Relabeling keeps it in autonomous + sell spike.
func TestBatteryModePlanHoldWithinExport(t *testing.T) {
	now := time.Now()
	firstBoundary := now.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	detail := batteryDetail{Type: batteryTypeBattery}
	req := optimizer.BatteryConfig{SCapacity: 13500}

	// [hold, discharge]: the current slot's lull opens onto the next export slot
	// (the log's discharge → hold → discharge). The head HOLD is bridged.
	t.Run("head lull before export", func(t *testing.T) {
		batRes := optimizer.BatteryResult{
			ChargingPower:    []float32{0, 0},
			DischargingPower: []float32{0, 2000},
			StateOfCharge:    []float32{6750, 6750},
		}
		res := optimizer.OptimizationResult{
			GridImport: []float32{100, 0},
			GridExport: []float32{0, 2000},
		}
		plan := batteryModePlan(detail, req, batRes, res, []int{900, 900}, firstBoundary, false)
		require.Len(t, plan, 2)
		assert.Equal(t, api.BatteryHoldCharge.String(), plan[0].action)
		assert.Equal(t, actionDischarge, plan[1].action)
	})

	// [discharge, hold, discharge]: an interior lull sandwiched between exports.
	t.Run("interior lull between exports", func(t *testing.T) {
		batRes := optimizer.BatteryResult{
			ChargingPower:    []float32{0, 0, 0},
			DischargingPower: []float32{2000, 0, 2000},
			StateOfCharge:    []float32{6750, 6750, 6750},
		}
		res := optimizer.OptimizationResult{
			GridImport: []float32{0, 100, 0},
			GridExport: []float32{2000, 0, 2000},
		}
		plan := batteryModePlan(detail, req, batRes, res, []int{900, 900, 900}, firstBoundary, false)
		require.Len(t, plan, 3)
		assert.Equal(t, actionDischarge, plan[0].action)
		assert.Equal(t, api.BatteryHoldCharge.String(), plan[1].action)
		assert.Equal(t, actionDischarge, plan[2].action)
	})

	// [hold, normal]: a HOLD not followed by an export slot is left untouched, so
	// a genuine hold (preserve the pack, import the house) still holds.
	t.Run("hold not before export is preserved", func(t *testing.T) {
		batRes := optimizer.BatteryResult{
			ChargingPower:    []float32{0, 0},
			DischargingPower: []float32{0, 0},
			StateOfCharge:    []float32{6750, 6750},
		}
		res := optimizer.OptimizationResult{
			GridImport: []float32{100, 0},
			GridExport: []float32{0, 0},
		}
		plan := batteryModePlan(detail, req, batRes, res, []int{900, 900}, firstBoundary, false)
		require.Len(t, plan, 2)
		assert.Equal(t, api.BatteryHold.String(), plan[0].action)
		assert.Equal(t, api.BatteryNormal.String(), plan[1].action)
	})
}

// TestOptimizerBatteryTargetSoc resolves the current plan slot's planned SoC,
// including front-running the export transition to the next slot's SoC.
func TestOptimizerBatteryTargetSoc(t *testing.T) {
	now := time.Now()
	slot := tariff.SlotDuration

	site := &Site{batterySchedules: map[string][]batterySlot{
		batteryKey("home"): {
			{until: now.Add(10 * time.Minute), action: actionDischarge, soc: 55},
			{until: now.Add(10*time.Minute + slot), action: actionDischarge, soc: 40},
		},
	}}

	assert.Equal(t, 55.0, site.optimizerBatteryTargetSoc("home"), "current slot's planned soc")
	assert.Zero(t, site.optimizerBatteryTargetSoc("missing"), "no schedule => 0")

	// front-run: within the lead the next export slot is adopted, incl. its soc
	site.batterySchedules = map[string][]batterySlot{
		batteryKey("home"): {
			{until: now.Add(time.Minute), action: api.BatteryNormal.String(), soc: 80},
			{until: now.Add(time.Minute + slot), action: actionDischarge, soc: 45},
		},
	}
	assert.Equal(t, 45.0, site.optimizerBatteryTargetSoc("home"))
}

// TestSuggestionActionable ensures the actionable flag follows the current state
// instead of the state at optimizer run time
func TestSuggestionActionable(t *testing.T) {
	lp := NewLoadpoint(util.NewLogger("foo"), nil)

	site := &Site{
		batteryMode: api.BatteryNormal,
		loadpoints:  []*Loadpoint{lp},
	}
	site.setSuggestions(map[string]types.Suggestion{
		batteryKey("bat"): {Action: api.BatteryCharge.String()},
		loadpointKey(0):   {Action: actionCharge},
	})

	batterySuggestion := func(name string) *types.Suggestion {
		return site.suggestion(batteryKey(name), site.GetBatteryMode().String())
	}
	loadpointSuggestion := func(id int) *types.Suggestion {
		return site.suggestion(loadpointKey(id), loadpointCurrentAction(lp))
	}

	// battery mode differs from suggestion
	s := batterySuggestion("bat")
	require.NotNil(t, s)
	assert.True(t, s.Actionable)

	site.batteryMode = api.BatteryCharge
	assert.False(t, batterySuggestion("bat").Actionable)

	assert.Nil(t, batterySuggestion("unknown"))

	// loadpoint stopped, suggestion is to charge
	s = loadpointSuggestion(0)
	require.NotNil(t, s)
	assert.True(t, s.Actionable)

	// loadpoint charging matches the suggestion
	lp.enabled = true
	lp.status = api.StatusC
	assert.False(t, loadpointSuggestion(0).Actionable)

	assert.Nil(t, loadpointSuggestion(1))
}

func TestSuggestionEvent(t *testing.T) {
	id := 2

	// battery: no loadpoint id, carries name
	detail := batteryDetail{Type: batteryTypeBattery, Name: "home", Title: "Home"}
	assert.Equal(t, "battery:home", detail.key())

	ev := suggestionEvent(detail, types.Suggestion{Action: api.BatteryCharge.String()})
	assert.Nil(t, ev.Loadpoint)
	assert.Equal(t, evSuggestion, ev.Event)
	assert.Equal(t, api.BatteryCharge.String(), ev.Attributes["suggestionAction"])
	assert.Equal(t, "home", ev.Attributes["suggestionName"])
	assert.Equal(t, "Home", ev.Attributes["suggestionTitle"])

	// loadpoint: carries id, no name
	detail = batteryDetail{Type: batteryTypeVehicle, loadpoint: &id, Title: "Garage"}
	assert.Equal(t, "loadpoint:2", detail.key())

	ev = suggestionEvent(detail, types.Suggestion{Action: actionCharge})
	require.NotNil(t, ev.Loadpoint)
	assert.Equal(t, id, *ev.Loadpoint)
	assert.NotContains(t, ev.Attributes, "suggestionName")

	// vehicle without loadpoint can't act on a suggestion
	assert.Empty(t, batteryDetail{Type: batteryTypeVehicle}.key())
}

func TestDiffSuggestions(t *testing.T) {
	site := &Site{}

	pending := func(s types.Suggestion) map[string]pendingSuggestion {
		ev := suggestionEvent(batteryDetail{loadpoint: new(int)}, s)
		return map[string]pendingSuggestion{"loadpoint:0": {suggestion: s, event: ev}}
	}

	charge := types.Suggestion{Action: actionCharge, Actionable: true}
	stop := types.Suggestion{Action: actionStop, Actionable: true}
	notActionable := types.Suggestion{Action: actionCharge, Actionable: false}

	// first actionable suggestion fires
	assert.Len(t, site.diffSuggestions(pending(charge)), 1)

	// unchanged action does not fire again
	assert.Empty(t, site.diffSuggestions(pending(charge)))

	// changed action fires
	assert.Len(t, site.diffSuggestions(pending(stop)), 1)

	// non-actionable suggestion does not fire and clears tracking so the same
	// action re-notifies when it becomes actionable again
	assert.Empty(t, site.diffSuggestions(pending(notActionable)))
	assert.Len(t, site.diffSuggestions(pending(stop)), 1)

	// vanished device is pruned and re-notifies on return
	assert.Empty(t, site.diffSuggestions(map[string]pendingSuggestion{}))
	assert.Len(t, site.diffSuggestions(pending(stop)), 1)
}

func TestVehicleAvailabilitySlots(t *testing.T) {
	const c = float32(8000) // realCMax

	// far-slot connection profile in minutes/slot (0..15); slots 0,1 are always
	// overridden by the real connection state so their profile values don't matter.
	// threshold is >7.5 min (more than half of a 15min slot).
	profile := []float64{15, 15, 10, 5, 8, 7.5, 7.6, 0}

	tc := []struct {
		name      string
		connected bool
		profile   []float64
		minLen    int
		expected  []float32
	}{
		{"connected, no history → available everywhere", true, nil, 5, []float32{c, c, c, c, c}},
		{"disconnected, no history → near slots blocked, far available", false, nil, 5, []float32{0, 0, c, c, c}},
		{
			"connected, profile gates the far slots (>7.5min)", true, profile, 8,
			// slots 0,1 = connected; 2:10>7.5, 3:5, 4:8>7.5, 5:7.5(not >), 6:7.6>7.5, 7:0
			[]float32{c, c, c, 0, c, 0, c, 0},
		},
		{
			"disconnected, near slots forced 0, far slots follow profile", false, profile, 8,
			[]float32{0, 0, c, 0, c, 0, c, 0},
		},
	}

	for _, tc := range tc {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, vehicleAvailabilitySlots(tc.connected, tc.profile, c, tc.minLen))
		})
	}
}

func TestVehicleAvailability(t *testing.T) {
	// no collectors configured → connectedProfile returns nil, so the far slots fall
	// back to available. This also verifies the connection state is read from the
	// loadpoint status (B/C connected, else disconnected).
	site := &Site{}

	for _, tc := range []struct {
		status    api.ChargeStatus
		connected bool
	}{
		{api.StatusC, true},
		{api.StatusB, true},
		{api.StatusA, false},
		{api.StatusNone, false},
	} {
		ctrl := gomock.NewController(t)
		lp := loadpoint.NewMockAPI(ctrl)
		lp.EXPECT().GetStatus().Return(tc.status).AnyTimes()

		res := site.vehicleAvailability(0, lp, 8000, 5)
		if tc.connected {
			assert.Equal(t, []float32{8000, 8000, 8000, 8000, 8000}, res, "status %s", tc.status)
		} else {
			assert.Equal(t, []float32{0, 0, 8000, 8000, 8000}, res, "status %s", tc.status)
		}
		ctrl.Finish()
	}
}

// socMeterStub is a battery meter that can report a soc
type socMeterStub struct{ api.Meter }

func (socMeterStub) Soc() (float64, error) { return 50, nil }

// TestBatteriesReported covers the startup gate: the optimizer waits until every battery
// with a soc has reported one, instead of planning without it for a whole slot. Once a
// battery has reported, dropping out later no longer holds the optimizer.
func TestBatteriesReported(t *testing.T) {
	pack := config.NewStaticDevice(config.Named{Name: "pack"}, api.Meter(socMeterStub{}))
	noSoc := config.NewStaticDevice(config.Named{Name: "nosoc"}, api.Meter(&struct{ api.Meter }{}))
	site := &Site{log: util.NewLogger("foo"), batteryMeters: []config.Device[api.Meter]{pack, noSoc}}

	soc := 50.0
	reported := []types.Measurement{{Soc: &soc}, {}}
	pending := []types.Measurement{{}, {}}

	// the pack has not reported yet (no soc, or its measurement not in yet): not ready
	assert.ErrorIs(t, site.batteriesReported(pending), errOptimizerNotReady, "pack soc missing at startup")
	assert.ErrorIs(t, site.batteriesReported(nil), errOptimizerNotReady, "measurements lag the meters")

	// a meter that cannot report a soc is never waited for
	require.NoError(t, site.batteriesReported(reported), "pack reported, soc-less meter not awaited")

	// the pack drops out after having reported: planned without it rather than blocking
	assert.NoError(t, site.batteriesReported(pending), "a later outage does not block the optimizer")
}

// efficiencyState is the learner state of exact rows from model m for a pack of the
// given capacity (kWh): discharge and charge runs at two powers each, 60 points per side.
func efficiencyState(m soc.EfficiencyModel, capacity float64) soc.EfficiencyState {
	var st soc.EfficiencyState
	nominal := capacity * 10
	add := func(x [3]float64, pts float64) {
		for i := range 3 {
			for j := range 3 {
				st.XtX[3*i+j] += x[i] * x[j]
			}
			st.XtY[i] += x[i] * pts
		}
	}
	for _, p := range []float64{450, 5000} {
		h := 2.0
		add([3]float64{p * h, 0, h}, (p*h/m.EtaD+m.Standby*h)/nominal)
		add([3]float64{0, p * h, -h}, (p*h*m.EtaC-m.Standby*h)/nominal)
	}
	st.ChargePts, st.DischargePts = 60, 60
	return st
}

func TestBatteryRequestLearnedEfficiency(t *testing.T) {
	capacity, socV := 13.5, 50.0
	m := types.Measurement{Capacity: &capacity, Soc: &socV}
	dev := config.NewStaticDevice(config.Named{Name: "pw"}, api.Meter(&struct{ api.Meter }{}))
	want := soc.EfficiencyModel{EtaC: 0.98, EtaD: 0.95, Standby: 88}

	site := &Site{log: util.NewLogger("foo")}
	req, _ := site.batteryRequest(dev, m, nil, 8, 15*time.Minute)
	assert.Nil(t, req.EtaC, "nothing learned: global efficiency")
	assert.Nil(t, req.EtaD)

	site.batteryEfficiency = map[string]*soc.EfficiencyLearner{"pw": soc.NewEfficiencyLearner(efficiencyState(want, capacity))}
	req, _ = site.batteryRequest(dev, m, nil, 8, 15*time.Minute)
	require.NotNil(t, req.EtaC)
	require.NotNil(t, req.EtaD)
	assert.InDelta(t, 0.98, *req.EtaC, 1e-4)
	assert.InDelta(t, 0.95, *req.EtaD, 1e-4)

	got, ok := site.batteryEfficiencyModel("pw", capacity)
	require.True(t, ok)
	assert.InDelta(t, 88, got.Standby, 1e-3)
}

func TestAddStandby(t *testing.T) {
	ts := optimizer.TimeSeries{Dt: []int{600, 900}, Gt: []float32{100, 200}}
	addStandby(&ts, 88)
	assert.InDeltaSlice(t, []float32{100 + 88.0/6, 200 + 22}, ts.Gt, 1e-3)
}

// TestLoadpointRequestMinSoc covers a car below its min soc: the floor is sent as
// is, not clamped to the current soc, so the optimizer charges the car up to it.
func TestLoadpointRequestMinSoc(t *testing.T) {
	site := &Site{log: util.NewLogger("foo")}
	ctrl := gomock.NewController(t)

	v := api.NewMockVehicle(ctrl)
	v.EXPECT().Capacity().Return(50.0).AnyTimes()
	v.EXPECT().GetTitle().Return("").AnyTimes()

	lp := loadpoint.NewMockAPI(ctrl)
	lp.EXPECT().GetVehicle().Return(v).AnyTimes()
	lp.EXPECT().GetSoc().Return(20.0).AnyTimes()
	lp.EXPECT().EffectiveLimitSoc().Return(100).AnyTimes()
	lp.EXPECT().EffectiveMinSoc().Return(30).AnyTimes()
	lp.EXPECT().GetLimitEnergy().Return(0.0).AnyTimes()
	lp.EXPECT().GetChargedEnergy().Return(0.0).AnyTimes()
	lp.EXPECT().GetTitle().Return("lp").AnyTimes()
	lp.EXPECT().EffectiveMinPower().Return(1380.0).AnyTimes()
	lp.EXPECT().EffectiveMaxPower().Return(11000.0).AnyTimes()
	lp.EXPECT().GetMode().Return(api.ModePV).AnyTimes()
	lp.EXPECT().GetStatus().Return(api.StatusB).AnyTimes()
	lp.EXPECT().GetSmartCostLimit().Return(nil).AnyTimes()
	lp.EXPECT().EffectivePlanStrategy().Return(api.PlanStrategy{}).AnyTimes()
	lp.EXPECT().GetPlanGoals(gomock.Any()).Return(nil, false).AnyTimes()

	req, _ := site.loadpointRequest(lp, 8, 15*time.Minute, nil)

	assert.Equal(t, float32(10000), req.SInitial)
	assert.Equal(t, float32(15000), req.SMin, "floor above the current soc")
	assert.False(t, req.Charging, "connected but not charging")
}

func TestLoadpointRequestCharging(t *testing.T) {
	site := &Site{log: util.NewLogger("foo")}
	ctrl := gomock.NewController(t)

	v := api.NewMockVehicle(ctrl)
	v.EXPECT().Capacity().Return(50.0).AnyTimes()
	v.EXPECT().GetTitle().Return("").AnyTimes()

	lp := loadpoint.NewMockAPI(ctrl)
	lp.EXPECT().GetVehicle().Return(v).AnyTimes()
	lp.EXPECT().GetSoc().Return(40.0).AnyTimes()
	lp.EXPECT().EffectiveLimitSoc().Return(100).AnyTimes()
	lp.EXPECT().EffectiveMinSoc().Return(0).AnyTimes()
	lp.EXPECT().GetLimitEnergy().Return(0.0).AnyTimes()
	lp.EXPECT().GetChargedEnergy().Return(0.0).AnyTimes()
	lp.EXPECT().GetTitle().Return("lp").AnyTimes()
	lp.EXPECT().EffectiveMinPower().Return(1380.0).AnyTimes()
	lp.EXPECT().EffectiveMaxPower().Return(11000.0).AnyTimes()
	lp.EXPECT().GetMode().Return(api.ModePV).AnyTimes()
	lp.EXPECT().GetStatus().Return(api.StatusC).AnyTimes()
	lp.EXPECT().GetSmartCostLimit().Return(nil).AnyTimes()
	lp.EXPECT().EffectivePlanStrategy().Return(api.PlanStrategy{}).AnyTimes()
	lp.EXPECT().GetPlanGoals(gomock.Any()).Return(nil, false).AnyTimes()

	req, _ := site.loadpointRequest(lp, 8, 15*time.Minute, nil)

	assert.True(t, req.Charging, "a charging car continues its session")
}

func TestCurrentSuggestion(t *testing.T) {
	now := time.Now().Truncate(tariff.SlotDuration).Add(5 * time.Minute)
	boundary := now.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	plan := []suggestionSlot{
		{until: boundary, suggestion: types.Suggestion{Action: actionStop}},
		{until: boundary.Add(tariff.SlotDuration), suggestion: types.Suggestion{Action: actionCharge, Charge: 1840}},
		{until: boundary.Add(2 * tariff.SlotDuration)},
	}

	s, ok := currentSuggestion(plan, now)
	require.True(t, ok)
	assert.Equal(t, actionStop, s.Action)

	s, ok = currentSuggestion(plan, boundary)
	require.True(t, ok, "the next slot applies from its start, not the first slot's action")
	assert.Equal(t, actionCharge, s.Action)
	assert.Equal(t, 1840.0, s.Charge)

	_, ok = currentSuggestion(plan, boundary.Add(tariff.SlotDuration))
	assert.False(t, ok, "a slot without an action yields no suggestion")

	_, ok = currentSuggestion(plan, boundary.Add(time.Hour))
	assert.False(t, ok, "an exhausted plan yields no suggestion instead of a stale one")

	s, ok = currentSuggestion([]suggestionSlot{{suggestion: types.Suggestion{Action: actionStop}}}, now.Add(24*time.Hour))
	require.True(t, ok, "an open-ended slot never expires")
	assert.Equal(t, actionStop, s.Action)
}

func TestPlanFirstBoundary(t *testing.T) {
	slot := time.Now().Truncate(tariff.SlotDuration)

	for _, tc := range []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"mid slot", slot.Add(7 * time.Minute), slot.Add(tariff.SlotDuration)},
		{"seconds before a boundary", slot.Add(tariff.SlotDuration - 26*time.Second), slot.Add(tariff.SlotDuration)},
		{"on a boundary", slot, slot.Add(tariff.SlotDuration)},
		{"under a second before a boundary", slot.Add(tariff.SlotDuration - 500*time.Millisecond), slot.Add(2 * tariff.SlotDuration)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// the boundary must match the first slot timeSteps produced for the same instant
			assert.Equal(t, tc.want, planFirstBoundary(tc.at, timeSteps(4, tc.at)))
		})
	}
}

// TestSuggestionPlan replays a run that lands seconds before a slot boundary:
// its first slot's action must govern only those seconds, and each later slot
// carries its own action, power and charge source.
func TestSuggestionPlan(t *testing.T) {
	computedAt := time.Now().Truncate(tariff.SlotDuration).Add(tariff.SlotDuration - 26*time.Second)
	dt := timeSteps(3, computedAt)
	require.Equal(t, []int{26, 900, 900}, dt)

	lp := 0
	details := []batteryDetail{
		{Type: batteryTypeVehicle, loadpoint: &lp, controllable: true},
		{Type: batteryTypeBattery, Name: "pw", controllable: true},
	}
	req := optimizer.OptimizationInput{
		TimeSeries: optimizer.TimeSeries{Dt: dt, Ft: []float32{0, 0, 0}, Gt: []float32{5, 150, 150}},
		Batteries:  []optimizer.BatteryConfig{{CMin: 1840}, {SCapacity: 13500}},
	}
	res := optimizer.OptimizationResult{
		GridImport: []float32{0, 0, 2400},
		GridExport: []float32{0, 0, 0},
		Batteries: []optimizer.BatteryResult{
			// car: idle, then fed from the pack, then from the grid
			{ChargingPower: []float32{0, 460, 2400}, DischargingPower: []float32{0, 0, 0}},
			{ChargingPower: []float32{0, 0, 0}, DischargingPower: []float32{5, 610, 150}},
		},
	}

	boundary := planFirstBoundary(computedAt, dt)
	plan := suggestionPlan(0, req, res, details, boundary, false)
	require.Len(t, plan, 3)

	assert.Equal(t, boundary, plan[0].until)
	assert.Equal(t, actionStop, plan[0].suggestion.Action)

	assert.Equal(t, boundary.Add(tariff.SlotDuration), plan[1].until)
	assert.Equal(t, actionCharge, plan[1].suggestion.Action)
	assert.InDelta(t, 1840, plan[1].suggestion.Charge, 1)
	assert.Equal(t, sourceBattery, plan[1].suggestion.Source)

	assert.Equal(t, actionCharge, plan[2].suggestion.Action)
	assert.InDelta(t, 9600, plan[2].suggestion.Charge, 1)
	assert.Equal(t, sourceGrid, plan[2].suggestion.Source)

	// the control loop resolves the slot in effect, not the run's first slot
	s, ok := currentSuggestion(plan, boundary.Add(time.Minute))
	require.True(t, ok)
	assert.Equal(t, actionCharge, s.Action)
	assert.Equal(t, sourceBattery, s.Source)
}

// TestLoadpointOptimizerActionFollowsPlan applies the plan slot in effect rather
// than the slot that was current when the optimizer ran.
func TestLoadpointOptimizerActionFollowsPlan(t *testing.T) {
	site := &Site{loadpoints: []*Loadpoint{NewLoadpoint(util.NewLogger("lp0"), nil)}}

	now := time.Now()
	site.setSuggestionPlans(map[string][]suggestionSlot{
		loadpointKey(0): {
			{until: now.Add(-time.Second), suggestion: types.Suggestion{Action: actionCharge, Charge: 9600, Source: sourceGrid}},
			{until: now.Add(tariff.SlotDuration), suggestion: types.Suggestion{Action: actionStop}},
		},
	})

	action, _, _ := site.loadpointOptimizerAction(0)
	assert.Equal(t, actionStop, action, "the expired first slot no longer applies")

	site.setSuggestionPlans(map[string][]suggestionSlot{
		loadpointKey(0): {{until: now.Add(-time.Second), suggestion: types.Suggestion{Action: actionCharge}}},
	})
	action, _, _ = site.loadpointOptimizerAction(0)
	assert.Empty(t, action, "an exhausted plan falls back to no optimizer action")
}

// TestKeepCurrentSlot: a failed run leaves the last plan in charge until its slot
// ends, for the loadpoints and the battery schedules alike.
func TestKeepCurrentSlot(t *testing.T) {
	site := &Site{loadpoints: []*Loadpoint{NewLoadpoint(util.NewLogger("lp0"), nil)}}

	now := time.Now()
	slot := now.Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)
	site.setSuggestionPlans(map[string][]suggestionSlot{
		loadpointKey(0): {
			{until: slot.Add(-tariff.SlotDuration), suggestion: types.Suggestion{Action: actionStop}},
			{until: slot, suggestion: types.Suggestion{Action: actionCharge, Charge: 9600, Source: sourceGrid}},
			{until: slot.Add(tariff.SlotDuration), suggestion: types.Suggestion{Action: actionStop}},
		},
		"fixed": {{suggestion: types.Suggestion{Action: actionCharge}}},
	})
	site.setBatterySchedules(map[string][]batterySlot{
		batteryKey("pw"): {
			{until: slot, action: "hold", soc: 17},
			{until: slot.Add(tariff.SlotDuration), action: "normal"},
		},
	})

	site.keepCurrentSlot(now)

	action, _, _ := site.loadpointOptimizerAction(0)
	assert.Equal(t, actionCharge, action, "the current slot still applies")
	assert.Equal(t, "hold", currentBatteryAction(site.batterySchedules[batteryKey("pw")], now, 0))

	later := slot.Add(time.Second)
	_, ok := currentSuggestion(site.suggestions[loadpointKey(0)], later)
	assert.False(t, ok, "nothing beyond the current slot")
	assert.Empty(t, currentBatteryAction(site.batterySchedules[batteryKey("pw")], later, 0))

	s, ok := currentSuggestion(site.suggestions["fixed"], later)
	require.True(t, ok, "a fixed suggestion never expires")
	assert.Equal(t, actionCharge, s.Action)
}

func TestOptimizerDue(t *testing.T) {
	slot := time.Now().Truncate(tariff.SlotDuration)

	for _, tc := range []struct {
		name      string
		last, now time.Time
		minAge    time.Duration
		due       bool
	}{
		{"never ran", time.Time{}, slot.Add(time.Minute), tariff.SlotDuration, true},
		{"same slot", slot.Add(time.Minute), slot.Add(10 * time.Minute), tariff.SlotDuration, false},
		{"next slot starts", slot.Add(14 * time.Minute), slot.Add(tariff.SlotDuration + 30*time.Second), tariff.SlotDuration, true},
		{"forced", slot.Add(time.Minute), slot.Add(2 * time.Minute), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.due, optimizerDue(tc.last, tc.now, tc.minAge))
		})
	}
}

func TestSocOffPlan(t *testing.T) {
	discharge := batterySlot{startSoc: 16, soc: 14}
	charge := batterySlot{startSoc: 40, soc: 45}

	for _, tc := range []struct {
		name string
		slot batterySlot
		soc  float64
		off  bool
	}{
		{"on the planned drain", discharge, 15, false},
		{"within tolerance below target", discharge, 13.2, false},
		{"car drew the pack past the target", discharge, 12.7, true},
		{"pack did not discharge", discharge, 17.5, true},
		{"charging on plan", charge, 43, false},
		{"charging ahead of plan", charge, 46.5, true},
		{"unknown soc", discharge, 0, false},
		{"no planned start", batterySlot{soc: 14}, 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.off, socOffPlan(tc.slot, tc.soc))
		})
	}
}

func TestRequestReplanOncePerSlot(t *testing.T) {
	site := &Site{log: util.NewLogger("foo")}

	assert.True(t, site.requestReplan("battery boost reached reserve"))
	assert.False(t, site.requestReplan("battery boost reached reserve"), "latched for the slot")
	assert.True(t, site.requestReplan("loadpoint Home control failed"), "other reasons re-plan independently")

	// a deviation in a later slot re-plans again
	site.replanSlots["battery boost reached reserve"] = time.Now().Truncate(tariff.SlotDuration).Add(-tariff.SlotDuration)
	assert.True(t, site.requestReplan("battery boost reached reserve"))
}

func TestCheckBatteryPlan(t *testing.T) {
	now := time.Now()
	newSite := func(soc float64) *Site {
		s := &Site{
			log: util.NewLogger("foo"),
			batteryMeters: []config.Device[api.Meter]{
				config.NewStaticDevice(config.Named{Name: "home"}, api.Meter(&recordingBatteryController{})),
			},
			batterySchedules: map[string][]batterySlot{
				batteryKey("home"): {
					{until: now.Add(-time.Minute), action: "normal", startSoc: 20, soc: 16},
					{until: now.Add(time.Minute), action: "normal", startSoc: 16, soc: 14},
				},
			},
		}
		s.battery.Devices = []types.Measurement{{Name: "home", Soc: &soc}}
		return s
	}

	site := newSite(15)
	site.checkBatteryPlan(now)
	assert.Empty(t, site.replanSlots, "on plan: no re-plan")

	site = newSite(12.7)
	site.checkBatteryPlan(now)
	assert.Contains(t, site.replanSlots, "battery home off plan", "past the slot's target: re-plan")
}

func TestBatteryModePlanStartSoc(t *testing.T) {
	req := optimizer.BatteryConfig{SCapacity: 10000, SInitial: 2000}
	batRes := optimizer.BatteryResult{
		ChargingPower:    []float32{0, 0},
		DischargingPower: []float32{100, 200},
		StateOfCharge:    []float32{1900, 1700},
	}
	res := optimizer.OptimizationResult{GridImport: []float32{0, 0}, GridExport: []float32{0, 0}}
	boundary := time.Now().Truncate(tariff.SlotDuration).Add(tariff.SlotDuration)

	plan := batteryModePlan(batteryDetail{Type: batteryTypeBattery}, req, batRes, res, []int{600, 900}, boundary, false)
	require.Len(t, plan, 2)
	assert.InDelta(t, 20, plan[0].startSoc, 1e-6, "first slot starts at the request's soc")
	assert.InDelta(t, 19, plan[0].soc, 1e-6)
	assert.InDelta(t, 19, plan[1].startSoc, 1e-6, "later slots start where the previous one ended")
	assert.InDelta(t, 17, plan[1].soc, 1e-6)
}

// TestOptimizerExclusive keeps a forced request that arrives during a run and runs
// it once that run finishes, while an unforced one is dropped.
func TestOptimizerExclusive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		force bool
		want  []bool
	}{
		{"forced during a run", true, []bool{false, true}},
		{"unforced during a run", false, []bool{false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := new(Site)
			var runs []bool
			var run func(bool)
			run = func(force bool) {
				runs = append(runs, force)
				if len(runs) == 1 {
					site.optimizerExclusive(tc.force, run)
				}
			}

			site.optimizerExclusive(false, run)

			assert.Equal(t, tc.want, runs)
			assert.True(t, site.optimizerMu.TryLock(), "unlocked")
		})
	}
}
