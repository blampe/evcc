package chargepoint

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/transport"
)

// wsUserAgent is the User-Agent for webservices.chargepoint.com calls,
// matching the iOS app's WKWebView requests.
const wsUserAgent = "ChargePoint/664 (iPhone; iOS 26.3; Scale/3.00)"

// API is an HTTP client for the ChargePoint API.
type API struct {
	identity    *Identity
	wsURL       string
	accountsURL string
	internalURL string
	chargersURL string
	mapcacheURL string
	region      string
}

// NewAPI creates a ChargePoint API client.
func NewAPI(log *util.Logger, identity *Identity) *API {
	api := &API{
		identity:    identity,
		wsURL:       identity.cfg.EndPoints.WebServices.Value,
		accountsURL: identity.cfg.EndPoints.Accounts.Value,
		internalURL: identity.cfg.EndPoints.InternalAPI.Value,
		chargersURL: identity.cfg.EndPoints.Chargers.Value,
		mapcacheURL: identity.cfg.EndPoints.MapCache.Value,
		region:      identity.Region,
	}
	api.identity.Helper.Transport = transport.BrotliCompression(api.identity.Helper.Transport)
	return api
}

// cpHeaders returns the standard CP headers required by all API endpoints.
// Cookies are set explicitly because the cookie jar is empty after a settings
// restore and the app always sends them as static header values.
func (a *API) cpHeaders() map[string]string {
	return map[string]string{
		"User-Agent":       userAgent,
		"CP-Region":        a.region,
		"CP-Session-Token": a.identity.SessionID,
		"CP-Session-Type":  "CP_SESSION_TOKEN",
		"Cache-Control":    "no-store",
		"Accept-Language":  "en;q=1",
		"Accept-Encoding":  "gzip, deflate, br",
		"Cookie":           "coulomb_sess=" + a.identity.SessionID + "; auth-session=" + a.identity.SSOSessionID,
	}
}

// cpWSHeaders returns CP headers for webservices.chargepoint.com calls,
// which require a different User-Agent from the native app endpoints.
func (a *API) cpWSHeaders() map[string]string {
	h := a.cpHeaders()
	h["User-Agent"] = wsUserAgent
	return h
}

// cpInternalHeaders returns CP headers for internal-api calls, which
// additionally require an Authorization bearer token.
func (a *API) cpInternalHeaders() map[string]string {
	h := a.cpHeaders()
	h["Authorization"] = "Bearer " + a.identity.SSOSessionID
	return h
}

// Account fetches the account and returns the user ID.
func (a *API) Account() (int32, error) {
	req, err := request.New(http.MethodGet, a.accountsURL+"v1/driver/profile/user", nil,
		request.JSONEncoding, a.cpHeaders())
	if err != nil {
		return 0, err
	}

	var res struct {
		User struct {
			UserID int32 `json:"userId"`
		} `json:"user"`
	}
	if err := a.identity.DoJSON(req, &res); err != nil {
		return 0, err
	}
	return res.User.UserID, nil
}

// HomeChargerIDs returns the device IDs of all registered home chargers.
func (a *API) HomeChargerIDs() ([]int, error) {
	data := struct {
		UserID    int32 `json:"user_id"`
		GetPandas struct {
			MFHS struct{} `json:"mfhs"`
		} `json:"get_pandas"`
	}{UserID: a.identity.UserID}

	req, err := request.New(http.MethodPost, a.wsURL+"mobileapi/v5",
		request.MarshalJSON(data), request.JSONEncoding, a.cpWSHeaders())
	if err != nil {
		return nil, err
	}

	var res struct {
		GetPandas struct {
			DeviceIDs []int `json:"device_ids"`
		} `json:"get_pandas"`
	}
	if err := a.identity.DoJSON(req, &res); err != nil {
		return nil, err
	}

	return res.GetPandas.DeviceIDs, nil
}

// HomeChargerStatus returns the current status of a home charger via the
// internal REST API, which returns richer data than the legacy mobileapi.
func (a *API) HomeChargerStatus(deviceID int) (HomeChargerStatus, error) {
	uri := fmt.Sprintf("%sapi/v1/configuration/users/%d/chargers/%d/status?", a.chargersURL, a.identity.UserID, deviceID)

	req, err := request.New(http.MethodGet, uri, nil,
		request.JSONEncoding, a.cpInternalHeaders())
	if err != nil {
		return HomeChargerStatus{}, err
	}

	var res HomeChargerStatus
	err = a.identity.DoJSON(req, &res)
	return res, err
}

// StartSession starts a charging session on the given device.
func (a *API) StartSession(deviceID int) error {
	data := struct {
		DeviceData DeviceData `json:"deviceData"`
		DeviceID   int        `json:"deviceId"`
	}{
		DeviceData: a.identity.deviceData,
		DeviceID:   deviceID,
	}

	req, err := request.New(http.MethodPost, a.accountsURL+"v1/driver/station/startsession",
		request.MarshalJSON(data), request.JSONEncoding, a.cpHeaders())
	if err != nil {
		return err
	}

	var res struct {
		AckID int `json:"ackId"`
	}
	if err := a.identity.DoJSON(req, &res); err != nil {
		var se *request.StatusError
		if !errors.As(err, &se) || !se.HasStatus(http.StatusUnprocessableEntity) {
			return err
		}
	}

	return a.pollAck(res.AckID, "start_session")
}

// StopSession stops the active charging session on the given device.
func (a *API) StopSession(deviceID int) error {
	data := struct {
		DeviceData DeviceData `json:"deviceData"`
		DeviceID   int        `json:"deviceId"`
	}{
		DeviceData: a.identity.deviceData,
		DeviceID:   deviceID,
	}

	req, err := request.New(http.MethodPost, a.accountsURL+"v1/driver/station/stopsession",
		request.MarshalJSON(data), request.JSONEncoding, a.cpHeaders())
	if err != nil {
		return err
	}

	var res struct {
		AckID int `json:"ackId"`
	}
	if err := a.identity.DoJSON(req, &res); err != nil {
		var se *request.StatusError
		if !errors.As(err, &se) || !se.HasStatus(http.StatusUnprocessableEntity) {
			return err
		}
	}

	return a.pollAck(res.AckID, "stop_session")
}

func (a *API) pollAck(ackID int, action string) error {
	ackData := struct {
		DeviceData DeviceData `json:"deviceData"`
		AckID      int        `json:"ackId"`
		Action     string     `json:"action"`
	}{
		DeviceData: a.identity.deviceData,
		AckID:      ackID,
		Action:     action,
	}

	for i := 0; i < 5; i++ {
		if i > 0 {
			time.Sleep(time.Second)
		}

		req, err := request.New(http.MethodPost, a.accountsURL+"v1/driver/station/session/ack",
			request.MarshalJSON(ackData), request.JSONEncoding, a.cpHeaders())
		if err != nil {
			return err
		}

		if err := a.identity.DoJSON(req, nil); err == nil {
			return nil
		}
	}

	return nil
}

// SetAmperageLimit sets the charge amperage limit on the given device via the
// internal REST API using PUT, as required by that endpoint.
func (a *API) SetAmperageLimit(deviceID int, limit int64) error {
	uri := fmt.Sprintf("%sapi/v1/configuration/chargers/%d/charge-amperage-limit", a.chargersURL, deviceID)

	data := struct {
		ChargeAmperageLimit int64 `json:"chargeAmperageLimit"`
	}{limit}

	req, err := request.New(http.MethodPut, uri,
		request.MarshalJSON(data), request.JSONEncoding, a.cpInternalHeaders())
	if err != nil {
		return err
	}

	return a.identity.DoJSON(req, nil)
}
