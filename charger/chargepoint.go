package charger

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	cpkg "github.com/evcc-io/evcc/charger/chargepoint"
	"github.com/evcc-io/evcc/util"
)

const (
	// HomeChargerStatus.ChargingStatus values.
	chargingStatusCharging = "CHARGING"
	chargingStatusFault    = "FAULT"

	// UserChargingStatus.State value for a session the charger has faulted out
	// of. ChargePoint keeps the session record open (non-zero ID) after a fault.
	sessionStateFault = "fault"

	// faultRestartCooldown bounds how often we reboot a faulted charger, so one
	// stuck in FAULT isn't hammered with restart requests while it reboots.
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
	sessionID    int // 0 = no active session
	faultRestart time.Time // last time we rebooted the charger to clear a FAULT
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
	c.sessionID = status.SessionID
	c.sessionG = util.ResettableCached(func() (cpkg.SessionDetails, error) {
		return c.API.SessionDetails(c.sessionID)
	}, 30*time.Second)

	res, err := c.statusG.Get()
	if err == nil && res.ChargingStatus == chargingStatusCharging {
		c.enabled = true
	}
}

// Status implements the api.Charger interface.
//
// The ChargePoint API reports isPluggedIn and chargingStatus separately, and
// there is a delay between when the vehicle plugs in and when a charging
// session starts. We use session awareness to distinguish "plugged in but no
// session started yet" (StatusB) from "plugged in and charging" (StatusC).
func (c *ChargePoint) Status() (api.ChargeStatus, error) {
	res, err := c.statusG.Get()
	if err != nil {
		return api.StatusNone, err
	}

	// A hardware FAULT won't clear on its own and a stop/start session can't
	// recover it; reboot the charger (rate-limited) to get it back.
	c.maybeRestartOnFault(res)

	// The mapcache session is authoritative for an active charging session.
	// HomeChargerStatus is unreliable: during ChargePoint cloud incidents it can
	// report AVAILABLE / isPluggedIn=false while the charger is actively
	// delivering power. Trusting it alone makes evcc believe the car is
	// disconnected and leaves an externally-started session running with no way
	// to stop it for PV, so let a live session override it. On a mapcache error
	// we fall back to HomeChargerStatus (no worse than before).
	var session cpkg.UserChargingStatus
	if st, serr := c.API.UserChargingStatus(); serr == nil {
		session = st
	}

	activeSession := sessionActive(session)

	switch chargePointStatus(res, activeSession) {
	case api.StatusC:
		if session.SessionID != 0 && c.sessionID != session.SessionID {
			c.sessionID = session.SessionID
			c.sessionG = util.ResettableCached(func() (cpkg.SessionDetails, error) {
				return c.API.SessionDetails(c.sessionID)
			}, 30*time.Second)
		} else if c.sessionG == nil {
			c.recoverSession()
		}
		c.enabled = true
		return api.StatusC, nil

	case api.StatusB:
		// Plugged in but no active session. If we still believe we're enabled,
		// the session ended externally (car finished / stopped) or the charger
		// faulted — reflect disabled so evcc re-issues StartCharging when it
		// wants to charge, rather than waiting forever for the vehicle.
		if c.enabled && !activeSession {
			c.log.DEBUG.Println("charging session ended or faulted; marking disabled")
			c.clearSession()
		}
		return api.StatusB, nil

	default:
		c.clearSession()
		return api.StatusA, nil
	}
}

// sessionActive reports whether a mapcache session represents a genuinely
// active charging session. After the charger faults, ChargePoint keeps the
// session record open with a non-zero ID and state "fault" while delivering
// 0 kW; that lingering session must not be treated as active, otherwise evcc
// believes the car is charging with no way to recover.
func sessionActive(session cpkg.UserChargingStatus) bool {
	return session.SessionID != 0 && !strings.EqualFold(session.State, sessionStateFault)
}

// chargePointStatus maps the charger's reported status plus mapcache session
// state to an evcc charge status. An active (non-faulted) mapcache session
// forces StatusC even when HomeChargerStatus wrongly reports AVAILABLE/unplugged.
// A charger-reported FAULT always wins over a lingering session: the charger is
// not delivering power, so report plugged-in-not-charging (or disconnected) and
// let evcc re-issue StartCharging.
func chargePointStatus(res cpkg.HomeChargerStatus, activeSession bool) api.ChargeStatus {
	switch {
	case res.ChargingStatus == chargingStatusFault:
		if res.IsPluggedIn {
			return api.StatusB
		}
		return api.StatusA
	case res.ChargingStatus == chargingStatusCharging || activeSession:
		return api.StatusC
	case res.IsPluggedIn:
		return api.StatusB
	default:
		return api.StatusA
	}
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
	return lastRestart.IsZero() || now.Sub(lastRestart) >= faultRestartCooldown
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
			return err
		}
		for range 5 {
			status, err := c.API.UserChargingStatus()
			if err != nil {
				return err
			}
			if status.SessionID != 0 {
				c.sessionID = status.SessionID
				c.sessionG = util.ResettableCached(func() (cpkg.SessionDetails, error) {
					return c.API.SessionDetails(c.sessionID)
				}, 30*time.Second)
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

