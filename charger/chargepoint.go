package charger

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evcc-io/evcc/api"
	cpkg "github.com/evcc-io/evcc/charger/chargepoint"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"golang.org/x/oauth2"
)

func init() {
	registry.Add("chargepoint", NewChargePointFromConfig)
}

// ChargePoint implements the api.Charger interface for ChargePoint Home Flex chargers.
type ChargePoint struct {
	*request.Helper
	log         *util.Logger
	userID      string
	deviceID    int
	wsURL       string
	accountsURL string
	internalURL string
	mapcacheURL string
	region      string
	mu          sync.Mutex
	sessToken   string
	refreshTok  string
	settingsKey string
	minCurrent  int64
	maxCurrent  int64
	enabled     bool
	statusG     util.Cacheable[cpHomeStatus]
	sessionG    util.Cacheable[cpSessionData]
}

type cpHomeStatus struct {
	IsPluggedIn    bool
	IsConnected    bool
	ChargingStatus string
	AmpLimit       int
}

type cpSessionData struct {
	PowerKW   float64
	EnergyKWh float64
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

	settingsKey := "chargepoint." + user

	// Attempt to reuse a previously stored session token to avoid repeated logins,
	// which can trigger CAPTCHA protection on the ChargePoint login endpoint.
	var token oauth2.Token
	if err := settings.Json(settingsKey, &token); err == nil && token.RefreshToken != "" {
		tok, err := cpkg.Refresh(log, &token)
		if err == nil {
			token = *tok
			_ = settings.SetJson(settingsKey, tok)
		} else {
			log.WARN.Printf("token refresh failed, re-authenticating: %v", err)
			token = oauth2.Token{}
		}
	}

	if token.RefreshToken == "" {
		tok, err := cpkg.Login(log, user, password)
		if err != nil {
			return nil, fmt.Errorf("login: %w", err)
		}
		token = *tok
		_ = settings.SetJson(settingsKey, tok)
	}

	log.Redact(token.AccessToken, token.RefreshToken)

	endpoints, err := cpkg.Discover(log, token.RefreshToken)
	if err != nil {
		log.WARN.Printf("discovery failed, using US defaults: %v", err)
		endpoints = &cpkg.Endpoints{
			Region:      cpkg.SessionRegion(token.RefreshToken),
			WebServices: "https://webservices.chargepoint.com/backend.php/",
			Accounts:    "https://account.chargepoint.com/account/",
			InternalAPI: "https://internal-api-us.chargepoint.com",
			MapCache:    "https://mc.chargepoint.com/map-prod/",
		}
	}

	cp := &ChargePoint{
		log:         log,
		wsURL:       endpoints.WebServices,
		accountsURL: endpoints.Accounts,
		internalURL: endpoints.InternalAPI,
		mapcacheURL: endpoints.MapCache,
		region:      endpoints.Region,
		sessToken:   token.AccessToken,
		refreshTok:  token.RefreshToken,
		settingsKey: settingsKey,
		minCurrent:  minCurrent,
		maxCurrent:  maxCurrent,
	}

	cp.Helper = request.NewHelper(log)
	cp.Client.Transport = &cpTransport{
		base:   cp.Client.Transport,
		parent: cp,
	}

	// Fetch the user ID from the account API. The Python library does the same;
	// parsing it from the session token is unreliable.
	var acct struct {
		User struct {
			UserID int `json:"userId"`
		} `json:"user"`
	}
	if err := cp.GetJSON(cp.accountsURL+"v1/driver/profile/user", &acct); err != nil {
		return nil, fmt.Errorf("get account: %w", err)
	}
	cp.userID = strconv.Itoa(acct.User.UserID)

	if deviceID == 0 {
		ids, err := cp.homeChargerIDs()
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
	cp.deviceID = deviceID

	cp.statusG = util.ResettableCached(cp.getHomeChargerStatus, cache)
	cp.sessionG = util.ResettableCached(cp.getSessionData, cache)

	return cp, nil
}

// cpTransport injects the coulomb_sess cookie on every request, adds extra
// headers for the internal API gateway, and transparently refreshes the token
// on 401 responses.
type cpTransport struct {
	base   http.RoundTripper
	parent *ChargePoint
}

func (t *cpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.parent.mu.Lock()
	sess := t.parent.sessToken
	t.parent.mu.Unlock()

	r := t.decorate(req, sess)
	resp, err := t.base.RoundTrip(r)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	resp.Body.Close()

	if err := t.parent.refreshToken(); err != nil {
		return nil, fmt.Errorf("token refresh: %w", err)
	}

	t.parent.mu.Lock()
	sess = t.parent.sessToken
	t.parent.mu.Unlock()

	return t.base.RoundTrip(t.decorate(req, sess))
}

func (t *cpTransport) decorate(req *http.Request, sess string) *http.Request {
	r := req.Clone(req.Context())
	r.AddCookie(&http.Cookie{Name: "coulomb_sess", Value: sess})

	if strings.HasPrefix(req.URL.String(), t.parent.internalURL) {
		r.Header.Set("cp-session-type", "CP_SESSION_TOKEN")
		r.Header.Set("cp-session-token", sess)
		r.Header.Set("cp-region", t.parent.region)
	}

	return r
}

func (c *ChargePoint) refreshToken() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	tok, err := cpkg.Refresh(c.log, &oauth2.Token{
		AccessToken:  c.sessToken,
		RefreshToken: c.refreshTok,
	})
	if err != nil {
		return err
	}

	c.sessToken = tok.AccessToken
	_ = settings.SetJson(c.settingsKey, tok)
	return nil
}

func (c *ChargePoint) homeChargerIDs() ([]int, error) {
	data := struct {
		UserID    string `json:"user_id"`
		GetPandas struct {
			MFHS struct{} `json:"mfhs"`
		} `json:"get_pandas"`
	}{UserID: c.userID}

	req, err := request.New(http.MethodPost, c.wsURL+"mobileapi/v5", request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return nil, err
	}

	var res struct {
		GetPandas struct {
			DeviceIDs []int `json:"device_ids"`
		} `json:"get_pandas"`
	}
	if err := c.DoJSON(req, &res); err != nil {
		return nil, err
	}

	return res.GetPandas.DeviceIDs, nil
}

func (c *ChargePoint) getHomeChargerStatus() (cpHomeStatus, error) {
	data := struct {
		UserID         string `json:"user_id"`
		GetPandaStatus struct {
			DeviceID int      `json:"device_id"`
			MFHS     struct{} `json:"mfhs"`
		} `json:"get_panda_status"`
	}{UserID: c.userID}
	data.GetPandaStatus.DeviceID = c.deviceID

	req, err := request.New(http.MethodPost, c.wsURL+"mobileapi/v5", request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return cpHomeStatus{}, err
	}

	var res struct {
		GetPandaStatus struct {
			IsPluggedIn    bool   `json:"is_plugged_in"`
			IsConnected    bool   `json:"is_connected"`
			ChargingStatus string `json:"charging_status"`
			ChargeAmperageSetting struct {
				ChargeLimit int `json:"charge_limit"`
			} `json:"charge_amperage_setting"`
		} `json:"get_panda_status"`
	}
	if err := c.DoJSON(req, &res); err != nil {
		return cpHomeStatus{}, err
	}

	s := res.GetPandaStatus
	return cpHomeStatus{
		IsPluggedIn:    s.IsPluggedIn,
		IsConnected:    s.IsConnected,
		ChargingStatus: s.ChargingStatus,
		AmpLimit:       s.ChargeAmperageSetting.ChargeLimit,
	}, nil
}

func (c *ChargePoint) getSessionData() (cpSessionData, error) {
	data := struct {
		UserStatus struct {
			MFHS struct{} `json:"mfhs"`
		} `json:"user_status"`
	}{}

	req, err := request.New(http.MethodPost, c.mapcacheURL+"v2", request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return cpSessionData{}, err
	}

	var statusResp struct {
		UserStatus *struct {
			Charging *struct {
				SessionID int `json:"sessionId"`
			} `json:"charging"`
		} `json:"user_status"`
	}
	if err := c.DoJSON(req, &statusResp); err != nil {
		return cpSessionData{}, err
	}

	if statusResp.UserStatus == nil || statusResp.UserStatus.Charging == nil {
		return cpSessionData{}, nil
	}

	sessionID := statusResp.UserStatus.Charging.SessionID
	query := url.QueryEscape(fmt.Sprintf(
		`{"user_id":%s,"charging_status":{"mfhs":{},"session_id":%d}}`,
		c.userID, sessionID))

	var chargingResp struct {
		ChargingStatus *struct {
			PowerKW   float64 `json:"power_kw"`
			EnergyKWh float64 `json:"energy_kwh"`
		} `json:"charging_status"`
	}
	if err := c.GetJSON(c.mapcacheURL+"v2?"+query, &chargingResp); err != nil {
		return cpSessionData{}, err
	}

	if chargingResp.ChargingStatus == nil {
		return cpSessionData{}, nil
	}

	return cpSessionData{
		PowerKW:   chargingResp.ChargingStatus.PowerKW,
		EnergyKWh: chargingResp.ChargingStatus.EnergyKWh,
	}, nil
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
		err = c.startSession()
	} else {
		err = c.stopSession()
	}
	if err != nil {
		return err
	}

	c.enabled = enable
	c.statusG.Reset()
	c.sessionG.Reset()
	return nil
}

func (c *ChargePoint) startSession() error {
	data := struct {
		DeviceData cpkg.DeviceData `json:"deviceData"`
		DeviceID   int             `json:"deviceId"`
	}{
		DeviceData: cpkg.NewDeviceData(),
		DeviceID:   c.deviceID,
	}

	req, err := request.New(http.MethodPost, c.accountsURL+"v1/driver/station/startsession",
		request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return err
	}

	var res struct {
		AckID string `json:"ackId"`
	}
	if err := c.DoJSON(req, &res); err != nil {
		return err
	}

	return c.pollAck(res.AckID, "start_session")
}

func (c *ChargePoint) stopSession() error {
	data := struct {
		UserStatus struct {
			MFHS struct{} `json:"mfhs"`
		} `json:"user_status"`
	}{}

	req, err := request.New(http.MethodPost, c.mapcacheURL+"v2", request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return err
	}

	var statusResp struct {
		UserStatus *struct {
			Charging *struct {
				SessionID int `json:"sessionId"`
			} `json:"charging"`
		} `json:"user_status"`
	}
	if err := c.DoJSON(req, &statusResp); err != nil {
		return err
	}

	if statusResp.UserStatus == nil || statusResp.UserStatus.Charging == nil {
		return nil
	}

	stopData := struct {
		DeviceData cpkg.DeviceData `json:"deviceData"`
		DeviceID   int             `json:"deviceId"`
		PortNumber int             `json:"portNumber"`
		SessionID  int             `json:"sessionId"`
	}{
		DeviceData: cpkg.NewDeviceData(),
		DeviceID:   c.deviceID,
		PortNumber: 1,
		SessionID:  statusResp.UserStatus.Charging.SessionID,
	}

	req, err = request.New(http.MethodPost, c.accountsURL+"v1/driver/station/stopSession",
		request.MarshalJSON(stopData), request.JSONEncoding)
	if err != nil {
		return err
	}

	var res struct {
		AckID string `json:"ackId"`
	}
	if err := c.DoJSON(req, &res); err != nil {
		return err
	}

	return c.pollAck(res.AckID, "stop_session")
}

func (c *ChargePoint) pollAck(ackID, action string) error {
	ackData := struct {
		DeviceData cpkg.DeviceData `json:"deviceData"`
		AckID      string          `json:"ackId"`
		Action     string          `json:"action"`
	}{
		DeviceData: cpkg.NewDeviceData(),
		AckID:      ackID,
		Action:     action,
	}

	for i := 0; i < 5; i++ {
		if i > 0 {
			time.Sleep(time.Second)
		}

		req, err := request.New(http.MethodPost, c.accountsURL+"v1/driver/station/session/ack",
			request.MarshalJSON(ackData), request.JSONEncoding)
		if err != nil {
			return err
		}

		if err := c.DoJSON(req, nil); err == nil {
			return nil
		}
	}

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

	uri := fmt.Sprintf("%s/driver/charger/%d/config/v1/charge-amperage-limit",
		c.internalURL, c.deviceID)

	data := struct {
		ChargeAmperageLimit int64 `json:"chargeAmperageLimit"`
	}{current}

	req, err := request.New(http.MethodPost, uri, request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return err
	}

	var res struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := c.DoJSON(req, &res); err != nil {
		return err
	}

	if res.Status != "success" {
		return fmt.Errorf("set amperage: %s", res.Message)
	}

	c.statusG.Reset()
	return nil
}

var _ api.Meter = (*ChargePoint)(nil)

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
