package core

import (
	"testing"

	evbus "github.com/asaskevich/EventBus"
	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestSyncCharger(t *testing.T) {
	tc := []struct {
		status                      api.ChargeStatus
		expected, actual, corrected bool
	}{
		{api.StatusA, false, false, false},
		{api.StatusC, false, false, true}, // disabled but charging
		{api.StatusA, false, true, true},
		{api.StatusA, true, false, false},
		{api.StatusA, true, true, true},
	}

	ctrl := gomock.NewController(t)

	for _, tc := range tc {
		t.Logf("%+v", tc)

		charger := api.NewMockCharger(ctrl)
		charger.EXPECT().Enabled().Return(tc.actual, nil).AnyTimes()

		if tc.status == api.StatusC {
			charger.EXPECT().Enable(tc.corrected).Times(1)
		}

		lp := &Loadpoint{
			log:     util.NewLogger("foo"),
			clock:   clock.New(),
			charger: charger,
			status:  tc.status,
			enabled: tc.expected,
		}

		require.NoError(t, lp.syncCharger())
		assert.Equal(t, tc.corrected, lp.enabled)
	}
}

func TestSyncChargerCurrentsByGetter(t *testing.T) {
	tc := []struct {
		lpCurrent, actualCurrent, outCurrent float64
	}{
		{6, 5, 5}, // force
		{6, 6.1, 6},
		{6, 6.5, 6.5},
		{6, 7, 7},
	}

	for _, tc := range tc {
		ctrl := gomock.NewController(t)
		t.Logf("%+v", tc)

		ch := api.NewMockCharger(ctrl)
		cg := api.NewMockCurrentGetter(ctrl)

		charger := struct {
			api.Charger
			api.CurrentGetter
		}{
			ch, cg,
		}

		ch.EXPECT().Enabled().Return(true, nil)
		cg.EXPECT().GetMaxCurrent().Return(tc.actualCurrent, nil).MaxTimes(1)

		lp := &Loadpoint{
			log:            util.NewLogger("foo"),
			bus:            evbus.New(),
			clock:          clock.New(),
			charger:        charger,
			status:         api.StatusC,
			enabled:        true,
			phases:         3,
			offeredCurrent: tc.lpCurrent,
		}

		require.NoError(t, lp.syncCharger())
		assert.Equal(t, tc.outCurrent, lp.offeredCurrent)
	}
}

func TestSyncChargerCurrentsByMeasurement(t *testing.T) {
	tc := []struct {
		lpCurrent     float64
		actualCurrent float64
		outCurrent    float64
	}{
		{6, 5, 6}, // ignore
		{6, 6.1, 6},
		{6, 6.5, 6},
		{6, 7, 6}, // ignore
		{6, 7.1, 7.1},
	}

	for _, tc := range tc {
		ctrl := gomock.NewController(t)
		t.Logf("%+v", tc)

		charger := api.NewMockCharger(ctrl)
		charger.EXPECT().Enabled().Return(true, nil)

		lp := &Loadpoint{
			log:            util.NewLogger("foo"),
			bus:            evbus.New(),
			clock:          clock.New(),
			charger:        charger,
			status:         api.StatusC,
			enabled:        true,
			phases:         3,
			offeredCurrent: tc.lpCurrent,
			chargeCurrents: []float64{tc.actualCurrent, 0, 0},
		}

		require.NoError(t, lp.syncCharger())
		assert.Equal(t, tc.outCurrent, lp.offeredCurrent)
	}
}

func TestSyncChargerPhasesByGetter(t *testing.T) {
	tc := []struct {
		lpPhases, actualPhases, outPhases int
	}{
		{0, 0, 0},
		{1, 0, 1},
		{1, 1, 1},
		{1, 3, 3},
		{3, 0, 3},
		{3, 1, 1}, // force
		{3, 3, 3},
	}

	for _, tc := range tc {
		ctrl := gomock.NewController(t)
		t.Logf("%+v", tc)

		ch := api.NewMockCharger(ctrl)
		ps := api.NewMockPhaseSwitcher(ctrl)
		pg := api.NewMockPhaseGetter(ctrl)

		charger := struct {
			api.Charger
			api.PhaseSwitcher
			api.PhaseGetter
		}{
			ch, ps, pg,
		}

		ch.EXPECT().Enabled().Return(true, nil)
		pg.EXPECT().GetPhases().Return(tc.actualPhases, nil).MaxTimes(1)

		lp := &Loadpoint{
			log:            util.NewLogger("foo"),
			bus:            evbus.New(),
			clock:          clock.New(),
			charger:        charger,
			status:         api.StatusC,
			enabled:        true,
			phases:         tc.lpPhases,
			measuredPhases: tc.actualPhases,
		}

		require.NoError(t, lp.syncCharger())
		assert.Equal(t, tc.outPhases, lp.phases)
	}
}

func TestSyncChargerPhasesByMeasurement(t *testing.T) {
	tc := []struct {
		lpPhases, actualPhases, outPhases int
	}{
		{0, 0, 0},
		{1, 0, 1},
		{1, 1, 1},
		{1, 3, 3},
		{3, 0, 3},
		{3, 1, 3}, // ignore
		{3, 3, 3},
	}

	ctrl := gomock.NewController(t)

	for _, tc := range tc {
		t.Logf("%+v", tc)

		ch := api.NewMockCharger(ctrl)
		ps := api.NewMockPhaseSwitcher(ctrl)

		charger := struct {
			api.Charger
			api.PhaseSwitcher
		}{
			ch, ps,
		}

		ch.EXPECT().Enabled().Return(true, nil)

		lp := &Loadpoint{
			log:            util.NewLogger("foo"),
			bus:            evbus.New(),
			clock:          clock.New(),
			charger:        charger,
			status:         api.StatusC,
			enabled:        true,
			phases:         tc.lpPhases,
			measuredPhases: tc.actualPhases,
		}

		require.NoError(t, lp.syncCharger())
		assert.Equal(t, tc.outPhases, lp.phases)
	}
}

// TestSyncChargerAdoptsCurrent covers a charger that starts the session itself
// when the car is plugged in: evcc adopts the enabled state and must adopt the
// charger's current with it. Disabling resets offeredCurrent to 0, which would
// otherwise make effectiveCurrent report the running car as drawing nothing.
func TestSyncChargerAdoptsCurrent(t *testing.T) {
	tc := []struct {
		name                      string
		status                    api.ChargeStatus
		chargerEnabled, lpEnabled bool
		actual, offered, expected float64
	}{
		// charger energized the car on plug-in, evcc still thinks it is disabled
		{"plug and charge", api.StatusC, true, false, 32, 0, 32},
		// nothing to adopt in the other direction: the charger offers no current
		{"charger disabled", api.StatusB, false, true, 0, 16, 16},
	}

	for _, tc := range tc {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)

			ch := api.NewMockCharger(ctrl)
			cg := api.NewMockCurrentGetter(ctrl)

			charger := struct {
				api.Charger
				api.CurrentGetter
			}{
				ch, cg,
			}

			ch.EXPECT().Enabled().Return(tc.chargerEnabled, nil)
			if tc.chargerEnabled {
				cg.EXPECT().GetMaxCurrent().Return(tc.actual, nil)
			}

			lp := &Loadpoint{
				log:            util.NewLogger("foo"),
				bus:            evbus.New(),
				clock:          clock.New(),
				charger:        charger,
				status:         tc.status,
				enabled:        tc.lpEnabled,
				phases:         1,
				offeredCurrent: tc.offered,
			}

			require.NoError(t, lp.syncCharger())
			assert.Equal(t, tc.chargerEnabled, lp.enabled, "enabled state adopted")
			assert.Equal(t, tc.expected, lp.offeredCurrent)
		})
	}
}

// TestPlugAndChargeKeepsCharging is the regression for a car that the charger
// energizes on plug-in while pv mode is active. Connecting elapses the pv timer,
// so the very first evaluation decides immediately - and it must decide on the
// car's actual draw. Before the current was adopted the loadpoint saw the car's
// own consumption as a site deficit with nothing charging, disabled the charger
// and only re-enabled it an enable delay later.
func TestPlugAndChargeKeepsCharging(t *testing.T) {
	Voltage = 100

	ctrl := gomock.NewController(t)

	ch := api.NewMockCharger(ctrl)
	cg := api.NewMockCurrentGetter(ctrl)

	charger := struct {
		api.Charger
		api.CurrentGetter
	}{
		ch, cg,
	}

	// the charger started the session on its own at 16A
	ch.EXPECT().Enabled().Return(true, nil)
	cg.EXPECT().GetMaxCurrent().Return(maxA, nil)

	lp := &Loadpoint{
		log:            util.NewLogger("foo"),
		bus:            evbus.New(),
		clock:          clock.New(),
		charger:        charger,
		status:         api.StatusC,
		enabled:        false, // evcc did not enable the charger
		offeredCurrent: 0,     // reset when the charger was last disabled
		minCurrent:     minA,
		maxCurrent:     maxA,
		phases:         1,
		measuredPhases: 1,
	}

	// connecting the car allows pv mode to act without waiting out the timer
	lp.elapsePVTimer()

	require.NoError(t, lp.syncCharger())

	// car draws 16A (1600W) against a 1000W surplus, so 600W is imported -
	// pv mode ramps the charge down to the surplus instead of stopping it
	sitePower := maxA*Voltage - 1000
	assert.Equal(t, 10.0, lp.pvMaxCurrent(api.ModePV, sitePower, 0, false, false))
}
