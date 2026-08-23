package charger

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/evcc-io/evcc/api"
	cpkg "github.com/evcc-io/evcc/charger/chargepoint"
	"github.com/evcc-io/evcc/util"
)

const (
	// HomeChargerStatus.ChargingStatus values.
	chargingStatusCharging = "CHARGING"
	chargingStatusFault    = "FAULT"

	// faultRestartCooldown bounds how often we reboot a charger (whether stuck in
	// FAULT or wedged in a phantom session), so one that won't recover isn't
	// hammered with restart requests while it reboots.
	faultRestartCooldown = 5 * time.Minute
)

var _ api.Charger = (*ChargePoint)(nil)
var _ api.Meter = (*ChargePoint)(nil)
var _ api.ChargeRater = (*ChargePoint)(nil)
var _ api.ChargeTimer = (*ChargePoint)(nil)
var _ api.CurrentGetter = (*ChargePoint)(nil)

func init() {
	registry.Add("chargepoint", NewChargePointFromConfig)
}

// ChargePoint implements the api.Charger interface for ChargePoint Home Flex chargers.
type ChargePoint struct {
	log *util.Logger
	*cpkg.API
	deviceID     int
	enabled      bool
	sessionID    int       // 0 = no active session
	faultRestart time.Time // last time we rebooted the charger to clear a FAULT
	wedgeRestart time.Time // last time we rebooted the charger to clear a wedged session
	statusG      util.Cacheable[cpkg.HomeChargerStatus]
	sessionG     util.Cacheable[cpkg.SessionDetails]
}

// NewChargePointFromConfig creates a ChargePoint charger from generic config.
func NewChargePointFromConfig(other map[string]any) (api.Charger, error) {
	cc := struct {
		DeviceID   int
		User       string
		Password   string
		MinCurrent int64
		MaxCurrent int64
		Cache      time.Duration
	}{
		MinCurrent: 8,
		MaxCurrent: 48,
		Cache:      30 * time.Second,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.User == "" || cc.Password == "" {
		return nil, api.ErrMissingCredentials
	}

	return NewChargePoint(cc.DeviceID, cc.User, cc.Password, cc.MinCurrent, cc.MaxCurrent, cc.Cache)
}

// NewChargePoint creates a ChargePoint Home Flex charger.
func NewChargePoint(deviceID int, user, password string, minCurrent, maxCurrent int64, cache time.Duration) (api.Charger, error) {
	log := util.NewLogger("chargepoint").Redact(user, password)

	identity, err := cpkg.NewIdentity(log, user, password)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}

	cpAPI := cpkg.NewAPI(log, identity)

	id := ""
	if deviceID != 0 {
		id = strconv.Itoa(deviceID)
	}
	foundID, err := ensureChargerEx(id, cpAPI.HomeChargerIDs, func(v int) (string, error) {
		return strconv.Itoa(v), nil
	})
	if err != nil {
		return nil, fmt.Errorf("charger: %w", err)
	}
	deviceID = foundID

	cp := &ChargePoint{
		log:      log,
		API:      cpAPI,
		deviceID: deviceID,
	}

	cp.statusG = util.ResettableCached(func() (cpkg.HomeChargerStatus, error) {
		return cp.API.HomeChargerStatus(cp.deviceID)
	}, cache)

	cp.recoverSession()

	return cp, nil
}

// recoverSession attempts to discover an active charging session.
// This handles the case where evcc restarts while a session is in progress.
func (c *ChargePoint) recoverSession() {
	status, err := c.API.UserChargingStatus()
	if err != nil || status.SessionID == 0 {
		return
	}
	c.trackSession(status)

	// Only measured power tells us the session is really charging (ChargePoint's
	// label fields are unreliable — see chargePointStatus).
	if c.sessionPowerKW() > 0 {
		c.enabled = true
	}
}

// Status implements the api.Charger interface.
//
// ChargePoint's status labels are unreliable in BOTH directions: it reports
// chargingStatus=NOT_CHARGING / AVAILABLE, isPluggedIn=false and session
// state="fully_charged" while the charger is actually delivering several kW, and
// it also leaves a "fully_charged" session open at 0 kW on a car that never
// finished. The one field that tracks reality is the session's measured power,
// so we look at the current session's power alongside the status and treat
// power > 0 as authoritative for "charging".
func (c *ChargePoint) Status() (api.ChargeStatus, error) {
	res, err := c.statusG.Get()
	if err != nil {
		return api.StatusNone, err
	}

	// A hardware FAULT won't clear on its own and a stop/start session can't
	// recover it; reboot the charger (rate-limited) to get it back.
	c.maybeRestartOnFault(res)

	// On a mapcache error we fall back to HomeChargerStatus (no worse than before).
	var session cpkg.UserChargingStatus
	if st, serr := c.API.UserChargingStatus(); serr == nil {
		session = st
	}

	// Read the live session power; it is the only trustworthy "is charging"
	// signal (see the method comment).
	var power float64
	if session.SessionID != 0 {
		c.trackSession(session)
		power = c.sessionPowerKW()
	}

	switch chargePointStatus(res, session, power) {
	case api.StatusC:
		c.enabled = true
		return api.StatusC, nil

	case api.StatusB:
		// Connected but not charging (0 kW). If we still believe we're enabled,
		// the session stopped (car finished / paused) or the charger wedged in a
		// phantom terminal session it won't leave — reflect disabled so evcc
		// re-issues StartCharging (which reboots a wedged charger, see Enable)
		// rather than waiting forever on a session delivering nothing.
		if c.enabled {
			c.log.DEBUG.Println("connected but not charging (0 kW); marking disabled")
			c.clearSession()
		}
		return api.StatusB, nil

	default:
		c.clearSession()
		return api.StatusA, nil
	}
}

// chargePointStatus derives an evcc charge status from the charger status, the
// mapcache session, and the session's measured power. ChargePoint's label
// fields lie in both directions (see Status), so measured power is authoritative
// for whether the car is charging. A charger-reported FAULT (which never carries
// power) reports plugged-in-not-charging so evcc re-issues StartCharging; a
// lingering 0 kW session still means the car is connected.
func chargePointStatus(res cpkg.HomeChargerStatus, session cpkg.UserChargingStatus, power float64) api.ChargeStatus {
	switch {
	case power > 0 || res.ChargingStatus == chargingStatusCharging:
		return api.StatusC
	case res.ChargingStatus == chargingStatusFault:
		if res.IsPluggedIn {
			return api.StatusB
		}
		return api.StatusA
	case session.SessionID != 0 || res.IsPluggedIn:
		return api.StatusB
	default:
		return api.StatusA
	}
}

// trackSession points sessionG at the given mapcache session so the session
// power/energy metrics reflect it.
func (c *ChargePoint) trackSession(session cpkg.UserChargingStatus) {
	if session.SessionID == c.sessionID && c.sessionG != nil {
		return
	}
	c.sessionID = session.SessionID
	c.sessionG = util.ResettableCached(func() (cpkg.SessionDetails, error) {
		return c.API.SessionDetails(c.sessionID)
	}, 30*time.Second)
}

// sessionPowerKW returns the tracked session's measured charge power in kW, or 0
// when there is no session or the read fails.
func (c *ChargePoint) sessionPowerKW() float64 {
	if c.sessionG == nil {
		return 0
	}
	res, err := c.sessionG.Get()
	if err != nil {
		return 0
	}
	return res.PowerKW
}

// maybeRestartOnFault reboots the charger when it reports a FAULT, at most once
// per faultRestartCooldown. A hardware fault leaves a session open at 0 kW that
// stop/start can't recover, so the only fix is a restart (the same action the
// mobile app issues). The cooldown is reset once the charger leaves FAULT so the
// next fault triggers an immediate restart.
func (c *ChargePoint) maybeRestartOnFault(res cpkg.HomeChargerStatus) {
	if res.ChargingStatus != chargingStatusFault {
		// Not faulted: reset so the next fault episode restarts immediately.
		c.faultRestart = time.Time{}
		return
	}

	if !shouldRestartOnFault(res, c.faultRestart, time.Now()) {
		return
	}

	// Stamp the attempt time first so a failing restart still honors the
	// cooldown instead of retrying on every status poll.
	c.faultRestart = time.Now()
	c.log.WARN.Println("charger reported FAULT; issuing restart")
	if err := c.API.RestartCharger(c.deviceID); err != nil {
		c.log.ERROR.Printf("charger restart failed: %v", err)
	}
}

// shouldRestartOnFault reports whether a faulted charger is due for a restart,
// given the last restart attempt. now is a parameter for testability. Only a
// FAULT status is restartable, and then at most once per faultRestartCooldown.
func shouldRestartOnFault(res cpkg.HomeChargerStatus, lastRestart, now time.Time) bool {
	if res.ChargingStatus != chargingStatusFault {
		return false
	}
	return rebootDue(lastRestart, now)
}

// rebootDue reports whether a charger is due for another reboot given the last
// reboot attempt: never rebooted, or at least faultRestartCooldown ago.
func rebootDue(lastRestart, now time.Time) bool {
	return lastRestart.IsZero() || now.Sub(lastRestart) >= faultRestartCooldown
}

// maybeRestartWedged reboots a charger that is wedged in a phantom session it
// refuses to leave — StartCharging is not acknowledged and the charger never
// energizes, exactly like a FAULT (see Enable). Rate-limited to at most once per
// faultRestartCooldown via its own timestamp. Returns true when a restart was
// issued (so the caller can ask evcc to retry once the charger is back).
func (c *ChargePoint) maybeRestartWedged() bool {
	if !rebootDue(c.wedgeRestart, time.Now()) {
		return false
	}

	// Stamp first so a failing restart still honors the cooldown.
	c.wedgeRestart = time.Now()
	c.log.WARN.Println("charger did not acknowledge start (wedged); issuing restart")
	if err := c.API.RestartCharger(c.deviceID); err != nil {
		c.log.ERROR.Printf("charger restart failed: %v", err)
		return false
	}
	return true
}

// clearSession resets local charging-session state.
func (c *ChargePoint) clearSession() {
	c.sessionID = 0
	c.sessionG = nil
	c.enabled = false
}

// Enabled implements the api.Charger interface.
func (c *ChargePoint) Enabled() (bool, error) {
	return verifyEnabled(c, c.enabled)
}

// Enable implements the api.Charger interface.
func (c *ChargePoint) Enable(enable bool) error {
	if enable {
		if err := c.API.StartCharging(c.deviceID); err != nil {
			// A charger wedged in a phantom terminal session (e.g. "fully_charged"
			// at 0 kW on a car that never finished) does not acknowledge the start
			// command — it times out and the charger never energizes. Like a
			// FAULT, only a reboot recovers it, so reboot (rate-limited) and let
			// the control loop retry once the charger is back (~1–2 min).
			if errors.Is(err, cpkg.ErrAckTimeout) && c.maybeRestartWedged() {
				return api.ErrMustRetry
			}
			return err
		}
		for range 5 {
			status, err := c.API.UserChargingStatus()
			if err != nil {
				return err
			}
			if status.SessionID != 0 {
				c.trackSession(status)
				break
			}
			time.Sleep(time.Second)
		}
	} else {
		if err := c.API.StopCharging(c.deviceID, c.sessionID, 1); err != nil {
			return err
		}
		c.sessionID = 0
		c.sessionG = nil
	}

	c.enabled = enable
	c.statusG.Reset()
	return nil
}

// MaxCurrent implements the api.Charger interface.
func (c *ChargePoint) MaxCurrent(current int64) error {
	if err := c.API.SetAmperageLimit(c.deviceID, current); err != nil {
		return err
	}

	c.statusG.Reset()
	return nil
}

// GetMaxCurrent implements the api.CurrentGetter interface.
//
// This is essential for correct PV control: SetAmperageLimit is applied
// asynchronously (the charger returns status "APPLYING" and can take a minute
// or more to take effect), so evcc must not assume its commanded current is in
// effect. Without a read-back, evcc's model races ahead of the charger and the
// control loop diverges (e.g. evcc thinks it is at 8A while the charger is
// still delivering 19A). Reporting the charger's actual limit lets syncCharger
// reconcile evcc's offeredCurrent to reality each cycle.
func (c *ChargePoint) GetMaxCurrent() (float64, error) {
	res, err := c.statusG.Get()
	if err != nil {
		return 0, err
	}

	// The charger transiently reports 0 (e.g. mid-renegotiation); don't let
	// evcc snap its model to a bogus 0. The minimum real limit is 8A.
	if res.ChargeAmperageSettings.ChargeLimit == 0 {
		return 0, api.ErrNotAvailable
	}

	return float64(res.ChargeAmperageSettings.ChargeLimit), nil
}

// CurrentPower implements the api.Meter interface. Reporting real measured
// charge power (rather than letting evcc estimate it from the commanded
// current) keeps the site energy accounting balanced: the EV draw is part of
// the Powerwall's measured load, so an inaccurate estimate would surface as
// unaccounted power in the UI.
func (c *ChargePoint) CurrentPower() (float64, error) {
	if c.sessionG == nil {
		return 0, nil
	}
	res, err := c.sessionG.Get()
	return res.PowerKW * 1e3, err
}

// ChargedEnergy implements the api.ChargeRater interface.
func (c *ChargePoint) ChargedEnergy() (float64, error) {
	if c.sessionG == nil {
		return 0, nil
	}
	res, err := c.sessionG.Get()
	return res.EnergyKWh, err
}

// ChargeDuration implements the api.ChargeTimer interface.
// The API reports charging_time in milliseconds.
func (c *ChargePoint) ChargeDuration() (time.Duration, error) {
	if c.sessionG == nil {
		return 0, nil
	}
	res, err := c.sessionG.Get()
	return time.Duration(res.ChargingTime) * time.Millisecond, err
}

