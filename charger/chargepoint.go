package charger

import (
	"fmt"
	"time"

	"github.com/evcc-io/evcc/api"
	cpkg "github.com/evcc-io/evcc/charger/chargepoint"
	"github.com/evcc-io/evcc/util"
)

func init() {
	registry.Add("chargepoint", NewChargePointFromConfig)
}

// ChargePoint implements the api.Charger interface for ChargePoint Home Flex chargers.
type ChargePoint struct {
	*cpkg.API
	log        *util.Logger
	deviceID   int
	minCurrent int64
	maxCurrent int64
	enabled    bool
	statusG    util.Cacheable[cpkg.HomeChargerStatus]
	sessionG   util.Cacheable[cpkg.SessionData]
}

// NewChargePointFromConfig creates a ChargePoint charger from generic config.
func NewChargePointFromConfig(other map[string]interface{}) (api.Charger, error) {
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

	err = identity.Login()
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}

	api := cpkg.NewAPI(log, identity)

	if deviceID == 0 {
		ids, err := api.HomeChargerIDs()
		if err != nil {
			return nil, fmt.Errorf("discover chargers: %w", err)
		}
		switch len(ids) {
		case 0:
			return nil, fmt.Errorf("no home chargers found")
		case 1:
			deviceID = ids[0]
		default:
			return nil, fmt.Errorf("multiple home chargers found %v, specify deviceid", ids)
		}
	}

	cp := &ChargePoint{
		API:        api,
		log:        log,
		deviceID:   deviceID,
		minCurrent: minCurrent,
		maxCurrent: maxCurrent,
	}

	cp.statusG = util.ResettableCached(func() (cpkg.HomeChargerStatus, error) {
		return cp.API.HomeChargerStatus(cp.deviceID)
	}, cache)
	cp.sessionG = util.ResettableCached(func() (cpkg.SessionData, error) {
		return cp.API.SessionData()
	}, cache)

	return cp, nil
}

// Status implements the api.Charger interface.
func (c *ChargePoint) Status() (api.ChargeStatus, error) {
	res, err := c.statusG.Get()
	if err != nil {
		return api.StatusNone, err
	}

	switch {
	case !res.IsConnected && !res.IsPluggedIn:
		return api.StatusA, nil
	case res.ChargingStatus == "CHARGING":
		return api.StatusC, nil
	default:
		return api.StatusB, nil
	}
}

// Enabled implements the api.Charger interface.
func (c *ChargePoint) Enabled() (bool, error) {
	return verifyEnabled(c, c.enabled)
}

// Enable implements the api.Charger interface.
func (c *ChargePoint) Enable(enable bool) error {
	var err error
	if enable {
		err = c.API.StartSession(c.deviceID)
	} else {
		err = c.API.StopSession(c.deviceID)
	}
	if err != nil {
		return err
	}

	c.enabled = enable
	c.statusG.Reset()
	c.sessionG.Reset()
	return nil
}

// MaxCurrent implements the api.Charger interface.
func (c *ChargePoint) MaxCurrent(current int64) error {
	if current < c.minCurrent {
		current = c.minCurrent
	}
	if current > c.maxCurrent {
		current = c.maxCurrent
	}

	if err := c.API.SetAmperageLimit(c.deviceID, current); err != nil {
		return err
	}

	c.statusG.Reset()
	return nil
}

//var _ api.Meter = (*ChargePoint)(nil)

/*
// CurrentPower implements the api.Meter interface.
func (c *ChargePoint) CurrentPower() (float64, error) {
	res, err := c.sessionG.Get()
	return res.PowerKW * 1e3, err
}

var _ api.ChargeRater = (*ChargePoint)(nil)

// ChargedEnergy implements the api.ChargeRater interface.
func (c *ChargePoint) ChargedEnergy() (float64, error) {
	res, err := c.sessionG.Get()
	return res.EnergyKWh, err
}

var _ api.CurrentGetter = (*ChargePoint)(nil)

// GetMaxCurrent implements the api.CurrentGetter interface.
func (c *ChargePoint) GetMaxCurrent() (float64, error) {
	res, err := c.statusG.Get()
	return float64(res.AmpLimit), err
}
*/
