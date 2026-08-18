package charger

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	cpkg "github.com/evcc-io/evcc/charger/chargepoint"
	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testChargePoint(limit int64) *ChargePoint {
	var status cpkg.HomeChargerStatus
	status.ChargeAmperageSettings.ChargeLimit = limit

	c := &ChargePoint{log: util.NewLogger("test")}
	c.statusG = util.ResettableCached(func() (cpkg.HomeChargerStatus, error) {
		return status, nil
	}, time.Minute)
	return c
}

// TestChargePointGetMaxCurrent verifies the api.CurrentGetter read-back that
// lets evcc reconcile its commanded current against the charger's actual limit
// (SetAmperageLimit applies asynchronously, so evcc must not trust its command).
func TestChargePointGetMaxCurrent(t *testing.T) {
	// reports the charger's actual limit
	cur, err := testChargePoint(19).GetMaxCurrent()
	require.NoError(t, err)
	assert.Equal(t, 19.0, cur)

	// a transient 0 must not snap evcc's model to zero
	_, err = testChargePoint(0).GetMaxCurrent()
	assert.ErrorIs(t, err, api.ErrNotAvailable)
}

// TestChargePointStatus verifies that an active mapcache session forces StatusC
// even when the (unreliable) HomeChargerStatus reports AVAILABLE/unplugged —
// otherwise evcc goes blind to an externally-started session and can't stop it.
func TestChargePointStatus(t *testing.T) {
	status := func(chargingStatus string, pluggedIn bool) cpkg.HomeChargerStatus {
		var s cpkg.HomeChargerStatus
		s.ChargingStatus = chargingStatus
		s.IsPluggedIn = pluggedIn
		return s
	}

	tests := []struct {
		name          string
		res           cpkg.HomeChargerStatus
		activeSession bool
		want          api.ChargeStatus
	}{
		{"charging flag", status("CHARGING", true), false, api.StatusC},
		{"active session overrides AVAILABLE/unplugged", status("AVAILABLE", false), true, api.StatusC},
		{"plugged in, no session", status("AVAILABLE", true), false, api.StatusB},
		{"available, unplugged, no session", status("AVAILABLE", false), false, api.StatusA},
		// A faulted charger must not read as charging even while ChargePoint
		// keeps the session record open (state "fault") — that lingering session
		// no longer counts as active (see activeSession in Status).
		{"fault, plugged in, no active session", status("FAULT", true), false, api.StatusB},
		{"fault, unplugged", status("FAULT", false), false, api.StatusA},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chargePointStatus(tc.res, tc.activeSession))
		})
	}
}

// TestChargePointSessionActive verifies that a mapcache session the charger has
// faulted out of is not treated as an active charging session — ChargePoint
// keeps the session record open with a non-zero ID and state "fault", and
// counting it as active made evcc report StatusC (charging) at 0 kW.
func TestChargePointSessionActive(t *testing.T) {
	tests := []struct {
		name    string
		session cpkg.UserChargingStatus
		want    bool
	}{
		{"no session", cpkg.UserChargingStatus{}, false},
		{"charging session", cpkg.UserChargingStatus{SessionID: 5431758691, State: "in_use"}, true},
		{"faulted session", cpkg.UserChargingStatus{SessionID: 5431758691, State: "fault"}, false},
		{"faulted session, mixed case", cpkg.UserChargingStatus{SessionID: 5431758691, State: "FAULT"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sessionActive(tc.session))
		})
	}
}

// TestChargePointShouldRestartOnFault verifies the reboot gate: a FAULT triggers
// a restart, but only once per cooldown so a charger stuck in FAULT isn't
// hammered, and non-fault states never restart.
func TestChargePointShouldRestartOnFault(t *testing.T) {
	fault := func() cpkg.HomeChargerStatus {
		var s cpkg.HomeChargerStatus
		s.ChargingStatus = "FAULT"
		return s
	}
	ok := func() cpkg.HomeChargerStatus {
		var s cpkg.HomeChargerStatus
		s.ChargingStatus = "CHARGING"
		return s
	}
	now := time.Now()

	tests := []struct {
		name        string
		res         cpkg.HomeChargerStatus
		lastRestart time.Time
		want        bool
	}{
		{"fault, never restarted", fault(), time.Time{}, true},
		{"fault, within cooldown", fault(), now.Add(-faultRestartCooldown / 2), false},
		{"fault, cooldown elapsed", fault(), now.Add(-faultRestartCooldown - time.Second), true},
		{"not faulted", ok(), time.Time{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldRestartOnFault(tc.res, tc.lastRestart, now))
		})
	}
}

// TestChargePointFaultRestartReset verifies maybeRestartOnFault clears the
// cooldown once the charger leaves FAULT (so the next fault restarts at once)
// and does not re-restart while still within the cooldown. Neither path issues
// a live restart, so no API is required.
func TestChargePointFaultRestartReset(t *testing.T) {
	c := &ChargePoint{log: util.NewLogger("test")}

	// Leaving FAULT resets the cooldown timestamp.
	c.faultRestart = time.Now()
	c.maybeRestartOnFault(cpkg.HomeChargerStatus{ChargingStatus: "AVAILABLE"})
	assert.True(t, c.faultRestart.IsZero(), "non-fault status must reset the cooldown")

	// A fault within the cooldown must not touch the API (would panic on nil
	// API) or advance the timestamp.
	stamp := time.Now().Add(-faultRestartCooldown / 2)
	c.faultRestart = stamp
	c.maybeRestartOnFault(cpkg.HomeChargerStatus{ChargingStatus: "FAULT"})
	assert.Equal(t, stamp, c.faultRestart, "within cooldown must not re-restart")
}
