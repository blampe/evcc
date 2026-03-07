package meter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andig/go-powerwall"
	"github.com/bogosj/tesla"
	teslaclient "github.com/evcc-io/tesla-proxy-client"
	"golang.org/x/oauth2"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/vehicle"
	evcctesla "github.com/evcc-io/evcc/vehicle/tesla"
)

// PowerWall is the tesla powerwall meter
type PowerWall struct {
	usage      string
	client     *powerwall.Client
	meterG     func() (map[string]powerwall.MeterAggregatesData, error)
	energySite *tesla.EnergySite
}

// FleetAPI is a Tesla Fleet API meter for Powerwall 3
type FleetAPI struct {
	usage        string
	log          *util.Logger
	client       *teslaclient.Client
	site         *teslaclient.EnergySite
	energySiteID int64

	// Cached APIs
	liveStatusG        func() (*teslaclient.EnergySiteLiveStatus, error)
	siteInfoG          func() (*teslaclient.EnergySite, error)
	batterySocLimits   batterySocLimits
	batteryPowerLimits batteryPowerLimits
}

func init() {
	registry.Add("tesla", NewPowerWallFromConfig)
	registry.Add("powerwall", NewPowerWallFromConfig)
	registry.Add("teslafleetapi", NewFleetAPIFromConfig)
}

//go:generate go tool decorate -f decoratePowerWall -b *PowerWall -r api.Meter -t api.MeterEnergy,api.Battery,api.BatteryCapacity,api.BatterySocLimiter,api.BatteryPowerLimiter,api.BatteryController

// NewPowerWallFromConfig creates a PowerWall Powerwall Meter from generic config
func NewPowerWallFromConfig(other map[string]any) (api.Meter, error) {
	cc := struct {
		URI, Usage, User, Password string
		Cache                      time.Duration
		RefreshToken               string
		SiteId                     int64
		batterySocLimits           `mapstructure:",squash"`
		batteryPowerLimits         `mapstructure:",squash"`
	}{
		batterySocLimits: batterySocLimits{
			MinSoc: 20,
			MaxSoc: 95,
		},
		batteryPowerLimits: batteryPowerLimits{
			MaxChargePower:    4600,
			MaxDischargePower: 4600,
		},
		Cache: time.Second,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.Usage == "" {
		return nil, errors.New("missing usage")
	}

	if cc.Password == "" {
		return nil, errors.New("missing password")
	}

	// support default meter names
	switch strings.ToLower(cc.Usage) {
	case "grid":
		cc.Usage = "site"
	case "pv":
		cc.Usage = "solar"
	}

	return NewPowerWall(cc.URI, cc.Usage, cc.User, cc.Password, cc.Cache, cc.RefreshToken, cc.SiteId, cc.batterySocLimits, cc.batteryPowerLimits)
}

// NewPowerWall creates a Tesla PowerWall Meter
func NewPowerWall(uri, usage, user, password string, cache time.Duration, refreshToken string, siteId int64, batterySocLimits batterySocLimits, batteryPowerLimits batteryPowerLimits) (api.Meter, error) {
	log := util.NewLogger("powerwall").Redact(user, password, refreshToken)

	httpClient := &http.Client{
		Transport: request.NewTripper(log, powerwall.DefaultTransport()),
		Timeout:   time.Second * 2, // Timeout after 2 seconds
	}

	client := powerwall.NewClient(uri, user, password, powerwall.WithHttpClient(httpClient))
	if _, err := client.GetStatus(); err != nil {
		return nil, err
	}

	m := &PowerWall{
		client: client,
		usage:  strings.ToLower(usage),
		meterG: util.Cached(client.GetMetersAggregates, cache),
	}

	var batteryControl bool
	if refreshToken != "" || siteId != 0 {
		if refreshToken == "" {
			return nil, errors.New("missing refresh token")
		}
		batteryControl = true
	}

	if batteryControl {
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, request.NewClient(log))

		options := []tesla.ClientOption{tesla.WithToken(&oauth2.Token{
			RefreshToken: refreshToken,
			Expiry:       time.Now(),
		})}

		cloudClient, err := tesla.NewClient(ctx, options...)
		if err != nil {
			return nil, err
		}

		if siteId == 0 {
			// auto detect energy site ID, picking first
			products, err := cloudClient.Products()
			if err != nil {
				return nil, err
			}

			for _, p := range products {
				if p.EnergySiteId != 0 {
					siteId = p.EnergySiteId
					break
				}
			}
		}

		log.Redact(strconv.FormatInt(siteId, 10))
		energySite, err := cloudClient.EnergySite(siteId)
		if err != nil {
			return nil, err
		}
		m.energySite = energySite
	}

	// decorate api.MeterEnergy
	var totalEnergy func() (float64, error)
	if m.usage == "load" || m.usage == "solar" {
		totalEnergy = m.totalEnergy
	}

	// decorate battery
	var batteryCapacity func() float64
	var batterySoc func() (float64, error)
	var batterySocLimiter func() (float64, float64)
	var batteryPowerLimiter func() (float64, float64)

	if usage == "battery" {
		batterySoc = m.batterySoc
		batterySocLimiter = batterySocLimits.Decorator()
		batteryPowerLimiter = batteryPowerLimits.Decorator()

		res, err := m.client.GetSystemStatus()
		if err != nil {
			return nil, err
		}

		batteryCapacity = func() float64 {
			return res.NominalFullPackEnergy / 1e3
		}
	}

	// decorate api.BatteryController
	var batModeS func(api.BatteryMode) error
	if batteryControl {
		batModeS = batterySocLimits.LimitController(m.socG, func(limit float64) error {
			// Handle Tesla firmware 25.18.4 restrictions:
			// Values between 81-99% are not allowed, only ≤80% or exactly 100%
			limitUint := uint64(limit)
			if limitUint > 80 && limitUint < 100 {
				// Adjust to maximum allowed (80%)
				limitUint = 80
			}
			return m.energySite.SetBatteryReserve(limitUint)
		})
	}

	return decoratePowerWall(m, totalEnergy, batterySoc, batteryCapacity, batterySocLimiter, batteryPowerLimiter, batModeS), nil
}

var _ api.Meter = (*PowerWall)(nil)

// CurrentPower implements the api.Meter interface
func (m *PowerWall) CurrentPower() (float64, error) {
	res, err := m.meterG()
	if err != nil {
		return 0, err
	}

	if o, ok := res[m.usage]; ok {
		return float64(o.InstantPower), nil
	}

	return 0, fmt.Errorf("invalid usage: %s", m.usage)
}

// totalEnergy implements the api.MeterEnergy interface
func (m *PowerWall) totalEnergy() (float64, error) {
	res, err := m.meterG()
	if err != nil {
		return 0, err
	}

	if o, ok := res[m.usage]; ok {
		switch m.usage {
		case "load":
			return float64(o.EnergyImported) / 1e3, nil
		case "solar":
			return float64(o.EnergyExported) / 1e3, nil
		}
	}

	return 0, fmt.Errorf("invalid usage: %s", m.usage)
}

// batterySoc implements the api.Battery interface
func (m *PowerWall) batterySoc() (float64, error) {
	res, err := m.client.GetSOE()
	if err != nil {
		return 0, err
	}

	return res.Percentage, err
}

// decorate soc
func (m *PowerWall) socG() (float64, error) {
	ess, err := m.energySite.EnergySiteStatus()
	if err != nil {
		return 0, err
	}
	// Fix for Tesla firmware 25.18.4: Remove the problematic +0.5 rounding logic
	// that was interfering with exact 100% reserve settings. Simply return the
	// actual current SOC rounded to nearest integer.
	return math.Round(ess.PercentageCharged), nil
}

// NewFleetAPIFromConfig creates a Tesla Fleet API Meter from generic config
func NewFleetAPIFromConfig(other map[string]any) (api.Meter, error) {
	cc := struct {
		Usage              string
		Credentials        vehicle.ClientCredentials
		Tokens             vehicle.Tokens
		EnergySiteID       int64 `mapstructure:"siteid"`
		Cache              time.Duration
		batterySocLimits   `mapstructure:",squash"`
		batteryPowerLimits `mapstructure:",squash"`
	}{
		batterySocLimits: batterySocLimits{
			MinSoc: 20,
			MaxSoc: 100,
		},
		batteryPowerLimits: batteryPowerLimits{
			MaxChargePower:    5000, // Typical PW3 power
			MaxDischargePower: 11500,
		},
		Cache: 5 * time.Second, // More frequent updates for live status
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.Usage == "" {
		return nil, errors.New("missing usage")
	}

	if cc.Credentials.ID == "" {
		return nil, errors.New("missing client id, see https://docs.evcc.io/en/docs/devices/vehicles#tesla")
	}

	token, err := cc.Tokens.Token()
	if err != nil {
		return nil, err
	}

	// Support default meter names
	switch strings.ToLower(cc.Usage) {
	case "grid":
		cc.Usage = "site"
	case "pv":
		cc.Usage = "solar"
	}

	return NewFleetAPI(cc.Usage, cc.Credentials.ID, cc.Credentials.Secret, token, cc.EnergySiteID, cc.Cache, cc.batterySocLimits, cc.batteryPowerLimits)
}

//go:generate go tool decorate -f decoratePowerWall3 -b *FleetAPI --out powerwall3_decorators.go -r api.Meter -t api.MeterEnergy,api.Battery,api.BatteryCapacity,api.BatterySocLimiter,api.BatteryPowerLimiter,api.BatteryController

// NewFleetAPI creates a Tesla Fleet API Meter for Powerwall 3
func NewFleetAPI(usage, clientID, clientSecret string, token *oauth2.Token, energySiteID int64, cache time.Duration, batterySocLimits batterySocLimits, batteryPowerLimits batteryPowerLimits) (api.Meter, error) {
	log := util.NewLogger("fleetapi").Redact(token.AccessToken, token.RefreshToken, clientID, clientSecret)

	identity, err := evcctesla.NewIdentity(log, evcctesla.OAuth2Config(clientID, clientSecret), token)
	if err != nil {
		return nil, err
	}

	hc := request.NewClient(log)
	hc.Transport = &oauth2.Transport{
		Source: identity,
		Base:   hc.Transport,
	}

	tc, err := teslaclient.NewClient(context.Background(), teslaclient.WithClient(hc))
	if err != nil {
		return nil, err
	}

	// validate base url
	region, err := tc.UserRegion()
	if err != nil {
		return nil, err
	}
	tc.SetBaseUrl(region.FleetApiBaseUrl)

	m := &FleetAPI{
		usage:              strings.ToLower(usage),
		log:                log,
		client:             tc,
		energySiteID:       energySiteID,
		batterySocLimits:   batterySocLimits,
		batteryPowerLimits: batteryPowerLimits,
	}

	// Auto-discovery of energy site ID if not provided
	if energySiteID == 0 {
		if err := m.discoverEnergySiteID(); err != nil {
			return nil, fmt.Errorf("failed to discover energy site ID: %w", err)
		}
	}

	// Get energy site for caching
	site, err := tc.EnergySite(m.energySiteID)
	if err != nil {
		return nil, fmt.Errorf("failed to get energy site: %w", err)
	}
	m.site = site

	// Setup cached functions
	m.liveStatusG = util.Cached(m.getLiveStatus, cache)
	m.siteInfoG = util.Cached(func() (*teslaclient.EnergySite, error) { return m.site, nil }, cache)

	// Validate connection by getting site info
	if _, err := m.siteInfoG(); err != nil {
		return nil, fmt.Errorf("failed to connect to Tesla Fleet API: %w", err)
	}

	// Setup decorators based on usage
	var totalEnergy func() (float64, error)
	if usage == "load" || usage == "solar" {
		totalEnergy = m.totalEnergy
	}

	var batteryCapacity func() float64
	var batterySoc func() (float64, error)
	var batterySocLimiter func() (float64, float64)
	var batteryPowerLimiter func() (float64, float64)
	var batModeS func(api.BatteryMode) error

	if usage == "battery" {
		batterySoc = m.batterySoc
		batterySocLimiter = batterySocLimits.Decorator()
		batteryPowerLimiter = batteryPowerLimits.Decorator()
		batModeS = batterySocLimits.LimitController(m.socG, m.setBatteryReserve)

		batteryCapacity = func() float64 {
			// total_pack_energy is no longer returned so assume 13.5kWh for now.
			// https://github.com/teslamotors/vehicle-command/issues/215
			return 13.5
		}
	}

	return decoratePowerWall3(m, totalEnergy, batterySoc, batteryCapacity, batterySocLimiter, batteryPowerLimiter, batModeS), nil
}

// discoverEnergySiteID auto-discovers the energy site ID from the user's products
func (m *FleetAPI) discoverEnergySiteID() error {
	products, err := m.client.Products()
	if err != nil {
		return err
	}

	// Find the first energy site
	for _, product := range products {
		if product.EnergySiteId != 0 {
			m.energySiteID = product.EnergySiteId
			m.log.DEBUG.Printf("Auto-discovered energy site ID: %d", m.energySiteID)
			return nil
		}
	}

	return errors.New("no energy site found in products")
}

// getLiveStatus returns the current status of the energy site
func (m *FleetAPI) getLiveStatus() (*teslaclient.EnergySiteLiveStatus, error) {
	return m.site.EnergySiteLiveStatus()
}

var _ api.Meter = (*FleetAPI)(nil)

// CurrentPower implements the api.Meter interface
func (m *FleetAPI) CurrentPower() (float64, error) {
	res, err := m.liveStatusG()
	if err != nil {
		return 0, err
	}

	switch m.usage {
	case "site", "grid":
		return res.GridPower, nil
	case "solar", "pv":
		return res.SolarPower, nil
	case "battery":
		return res.BatteryPower, nil
	case "load":
		return res.LoadPower, nil
	}

	return 0, fmt.Errorf("invalid usage: %s", m.usage)
}

// totalEnergy implements the api.MeterEnergy interface
func (m *FleetAPI) totalEnergy() (float64, error) {
	return 13.5, nil
}

// batterySoc implements the api.Battery interface
func (m *FleetAPI) batterySoc() (float64, error) {
	res, err := m.liveStatusG()
	if err != nil {
		return 0, err
	}

	return res.PercentageCharged, nil
}

// socG returns current SOC for battery control
func (m *FleetAPI) socG() (float64, error) {
	return m.batterySoc()
}

// setBatteryReserve sets the battery backup reserve percentage
func (m *FleetAPI) setBatteryReserve(reservePercent float64) error {
	return m.site.SetBatteryReserve(uint64(reservePercent))
}
