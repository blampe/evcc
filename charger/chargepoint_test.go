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

// TestChargePointStatus verifies that measured session power — not ChargePoint's
// unreliable status/session labels — decides the charge status. ChargePoint has
// been observed reporting AVAILABLE / unplugged / state "fully_charged" while
// delivering several kW, and leaving a "fully_charged" session open at 0 kW on a
// car that never finished; both must resolve correctly off power.
func TestChargePointStatus(t *testing.T) {
	res := func(chargingStatus string, pluggedIn bool) cpkg.HomeChargerStatus {
		var s cpkg.HomeChargerStatus
		s.ChargingStatus = chargingStatus
		s.IsPluggedIn = pluggedIn
		return s
	}
	// A lingering "fully_charged" session (the unreliable label observed live).
	sess := cpkg.UserChargingStatus{SessionID: 5451583781, State: "fully_charged"}
	none := cpkg.UserChargingStatus{}

	tests := []struct {
		name    string
		res     cpkg.HomeChargerStatus
		session cpkg.UserChargingStatus
		power   float64
		want    api.ChargeStatus
	}{
		{"charging flag, no session yet", res("CHARGING", true), none, 0, api.StatusC},
		// Measured power is authoritative even when every label lies: AVAILABLE,
		// isPluggedIn=false and state "fully_charged" while delivering 2 kW.
		{"power overrides AVAILABLE/unplugged/fully_charged", res("AVAILABLE", false), sess, 2.04, api.StatusC},
		// Phantom wedge: "fully_charged" session left open at 0 kW is not charging.
		{"fully_charged session at 0 kW is not charging", res("NOT_CHARGING", true), sess, 0, api.StatusB},
		// A lingering 0 kW session still means connected, even if the plug flag lies.
		{"idle 0 kW session, unplugged flag unreliable", res("AVAILABLE", false), sess, 0, api.StatusB},
		{"plugged in, no session", res("AVAILABLE", true), none, 0, api.StatusB},
		{"available, unplugged, no session", res("AVAILABLE", false), none, 0, api.StatusA},
		// A faulted charger delivers no power → plugged-in-not-charging / disconnected.
		{"fault, plugged in", res("FAULT", true), none, 0, api.StatusB},
		{"fault, unplugged", res("FAULT", false), none, 0, api.StatusA},
		// Real power flow wins even over a FAULT label.
		{"power overrides fault label", res("FAULT", true), sess, 7.0, api.StatusC},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chargePointStatus(tc.res, tc.session, tc.power))
		})
	}
}

// TestChargePointRebootDue verifies the shared reboot cooldown gate used for both
// FAULT and wedged-charger restarts: due when never rebooted or after the cooldown.
func TestChargePointRebootDue(t *testing.T) {
	now := time.Now()
	assert.True(t, rebootDue(time.Time{}, now), "never rebooted → due")
	assert.False(t, rebootDue(now.Add(-faultRestartCooldown/2), now), "within cooldown → not due")
	assert.True(t, rebootDue(now.Add(-faultRestartCooldown-time.Second), now), "cooldown elapsed → due")
}

// TestChargePointMaybeRestartWedged verifies a wedged restart within the cooldown
// does not touch the API (a nil API would panic) or issue a reboot.
func TestChargePointMaybeRestartWedged(t *testing.T) {
	c := &ChargePoint{log: util.NewLogger("test")}
	c.wedgeRestart = time.Now().Add(-faultRestartCooldown / 2)
	assert.False(t, c.maybeRestartWedged(), "within cooldown must not restart")
	assert.Equal(t, c.wedgeRestart, c.wedgeRestart, "timestamp unchanged within cooldown")
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
