package chargepoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
)

const wsUserAgent = "ChargePoint/664 (iPhone; iOS 26.3; Scale/3.00)"

// API is an HTTP client for the ChargePoint API.
type API struct {
	log      *util.Logger
	identity *Identity
}

// NewAPI creates a ChargePoint API client.
func NewAPI(log *util.Logger, identity *Identity) *API {
	return &API{
		log:      log,
		identity: identity,
	}
}

func (a *API) cpHeaders() map[string]string {
	return map[string]string{
		"User-Agent":       userAgent,
		"CP-Region":        a.identity.Region,
		"CP-Session-Token": a.identity.SessionID,
		"CP-Session-Type":  "CP_SESSION_TOKEN",
		"Cache-Control":    "no-store",
		"Accept-Language":  "en;q=1",
		"Cookie":           "coulomb_sess=" + a.identity.SessionID + "; auth-session=" + a.identity.SSOSessionID,
	}
}

func (a *API) cpWSHeaders() map[string]string {
	h := a.cpHeaders()
	h["User-Agent"] = wsUserAgent
	return h
}

func (a *API) cpInternalHeaders() map[string]string {
	h := a.cpHeaders()
	h["Authorization"] = "Bearer " + a.identity.SSOSessionID
	return h
}

// doJSON executes the request produced by makeReq. If the server returns 401,
// it re-authenticates and retries once.
func (a *API) doJSON(makeReq func() (*http.Request, error), res any) error {
	req, err := makeReq()
	if err != nil {
		return err
	}
	err = a.identity.DoJSON(req, res)
	if err == nil {
		return nil
	}
	se, ok := errors.AsType[*request.StatusError](err)
	if !ok || !se.HasStatus(http.StatusUnauthorized) {
		return err
	}
	a.log.DEBUG.Println("chargepoint session expired, re-authenticating")
	if loginErr := a.identity.Login(); loginErr != nil {
		return fmt.Errorf("re-authentication failed: %w (original: %v)", loginErr, err)
	}
	req, err = makeReq()
	if err != nil {
		return err
	}
	return a.identity.DoJSON(req, res)
}

// Account fetches the account and returns the user ID.
func (a *API) Account() (int32, error) {
	var res struct {
		User struct {
			UserID int32 `json:"userId"`
		} `json:"user"`
	}
	err := a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodGet, a.identity.cfg.EndPoints.Accounts.Value+"v1/driver/profile/user", nil,
			request.JSONEncoding, a.cpHeaders())
	}, &res)
	if err != nil {
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

	var res struct {
		GetPandas struct {
			DeviceIDs []int `json:"device_ids"`
		} `json:"get_pandas"`
	}
	err := a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodPost, a.identity.cfg.EndPoints.WebServices.Value+"mobileapi/v5",
			request.MarshalJSON(data), request.JSONEncoding, a.cpWSHeaders())
	}, &res)
	if err != nil {
		return nil, err
	}

	return res.GetPandas.DeviceIDs, nil
}

// HomeChargerStatus returns the current status of a home charger.
func (a *API) HomeChargerStatus(deviceID int) (HomeChargerStatus, error) {
	uri := fmt.Sprintf("%sapi/v1/configuration/users/%d/chargers/%d/status?", a.identity.cfg.EndPoints.Chargers.Value, a.identity.UserID, deviceID)

	var res HomeChargerStatus
	err := a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodGet, uri, nil,
			request.JSONEncoding, a.cpInternalHeaders())
	}, &res)
	return res, err
}

// StartCharging starts a charging session on the given device.
func (a *API) StartCharging(deviceID int) error {
	data := struct {
		DeviceData DeviceData `json:"deviceData"`
		DeviceID   int        `json:"deviceId"`
	}{
		DeviceData: a.identity.deviceData,
		DeviceID:   deviceID,
	}

	var res struct {
		AckID int `json:"ackId"`
	}
	if err := a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodPost, a.identity.cfg.EndPoints.Accounts.Value+"v1/driver/station/startsession",
			request.MarshalJSON(data), request.JSONEncoding, a.cpHeaders())
	}, &res); err != nil {
		se, ok := errors.AsType[*request.StatusError](err)
		if !ok || !se.HasStatus(http.StatusUnprocessableEntity) {
			return err
		}
	}

	return a.pollAck(res.AckID, "start_session")
}

// StopCharging stops the active charging session on the given device.
func (a *API) StopCharging(deviceID, sessionID, portNumber int) error {
	data := struct {
		DeviceData DeviceData `json:"deviceData"`
		DeviceID   int        `json:"deviceId"`
		PortNumber int        `json:"portNumber"`
		SessionID  int        `json:"sessionId"`
	}{
		DeviceData: a.identity.deviceData,
		DeviceID:   deviceID,
		PortNumber: portNumber,
		SessionID:  sessionID,
	}

	var res struct {
		AckID int `json:"ackId"`
	}
	if err := a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodPost, a.identity.cfg.EndPoints.Accounts.Value+"v1/driver/station/stopsession",
			request.MarshalJSON(data), request.JSONEncoding, a.cpHeaders())
	}, &res); err != nil {
		se, ok := errors.AsType[*request.StatusError](err)
		if !ok || !se.HasStatus(http.StatusUnprocessableEntity) {
			return err
		}
	}

	return a.pollAck(res.AckID, "stop_session")
}

var errAckTimeout = errors.New("charger did not acknowledge command within timeout")

func (a *API) pollAck(ackID int, action string) error {
	// An ackId of 0 means the charger did not queue a command: the device was
	// already in the requested state (e.g. stopping when no session is active,
	// or starting when already charging). There is nothing to acknowledge, so
	// polling would just time out after 10s. Treat it as success.
	if ackID == 0 {
		a.log.DEBUG.Printf("%s: no command queued (ackId=0), already in target state", action)
		return nil
	}

	ackData := struct {
		DeviceData DeviceData `json:"deviceData"`
		AckID      int        `json:"ackId"`
		Action     string     `json:"action"`
	}{
		DeviceData: a.identity.deviceData,
		AckID:      ackID,
		Action:     action,
	}

	for i := range 10 {
		if i > 0 {
			time.Sleep(time.Second)
		}

		req, err := request.New(http.MethodPost, a.identity.cfg.EndPoints.Accounts.Value+"v1/driver/station/session/ack",
			request.MarshalJSON(ackData), request.JSONEncoding, a.cpHeaders())
		if err != nil {
			return err
		}

		err = a.identity.DoJSON(req, nil)
		if err == nil {
			return nil
		}
		if se, ok := errors.AsType[*request.StatusError](err); ok && se.HasStatus(http.StatusUnprocessableEntity) {
			continue
		}
		a.log.DEBUG.Printf("pollAck %s attempt %d/10 (ackId=%d): %v", action, i+1, ackID, err)
	}

	return fmt.Errorf("%w: %s (ackId=%d)", errAckTimeout, action, ackID)
}

// UserChargingStatus returns the active session state from the mapcache API.
func (a *API) UserChargingStatus() (UserChargingStatus, error) {
	data := struct {
		UserStatus struct {
			MFHS struct{} `json:"mfhs"`
		} `json:"user_status"`
	}{}

	var res struct {
		UserStatus struct {
			Charging struct {
				SessionID int    `json:"sessionId"`
				State     string `json:"state"`
			} `json:"charging"`
		} `json:"user_status"`
	}
	err := a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodPost, a.identity.cfg.EndPoints.MapCache.Value+"v2",
			request.MarshalJSON(data), request.JSONEncoding, a.cpWSHeaders())
	}, &res)
	if err != nil {
		return UserChargingStatus{}, err
	}

	return UserChargingStatus{
		SessionID: res.UserStatus.Charging.SessionID,
		State:     res.UserStatus.Charging.State,
	}, nil
}

// SessionDetails returns live session metrics for the given session ID.
func (a *API) SessionDetails(sessionID int) (SessionDetails, error) {
	uri := fmt.Sprintf("%s/driver-bff/v1/sessions/%d", a.identity.cfg.EndPoints.InternalAPI.Value, sessionID)

	data := struct {
		ChargingStatus struct {
			SessionID int   `json:"session_id"`
			MFHS      []any `json:"mfhs"`
		} `json:"charging_status"`
	}{}
	data.ChargingStatus.SessionID = sessionID
	data.ChargingStatus.MFHS = []any{}

	var res struct {
		ChargingStatus SessionDetails `json:"charging_status"`
	}
	err := a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodPost, uri,
			request.MarshalJSON(data), request.JSONEncoding, a.cpInternalHeaders())
	}, &res)
	return res.ChargingStatus, err
}

// SessionDetailsRaw returns the raw JSON body of the SessionDetails response,
// for tooling that wants to inspect the full payload.
func (a *API) SessionDetailsRaw(sessionID int) (json.RawMessage, error) {
	uri := fmt.Sprintf("%s/driver-bff/v1/sessions/%d", a.identity.cfg.EndPoints.InternalAPI.Value, sessionID)

	data := struct {
		ChargingStatus struct {
			SessionID int   `json:"session_id"`
			MFHS      []any `json:"mfhs"`
		} `json:"charging_status"`
	}{}
	data.ChargingStatus.SessionID = sessionID
	data.ChargingStatus.MFHS = []any{}

	var res json.RawMessage
	err := a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodPost, uri,
			request.MarshalJSON(data), request.JSONEncoding, a.cpInternalHeaders())
	}, &res)
	return res, err
}

// RestartCharger reboots the given home charger. This is the only way to clear
// a hardware FAULT: the charger keeps a faulted session open and won't resume on
// a stop/start, so evcc reboots it (mirroring the mobile app's restart action).
func (a *API) RestartCharger(deviceID int) error {
	uri := fmt.Sprintf("%sapi/v1/configuration/users/%d/chargers/%d/restart", a.identity.cfg.EndPoints.Chargers.Value, a.identity.UserID, deviceID)

	return a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodPost, uri, nil,
			request.JSONEncoding, a.cpInternalHeaders())
	}, nil)
}

// SetAmperageLimit sets the charge amperage limit on the given device.
func (a *API) SetAmperageLimit(deviceID int, limit int64) error {
	uri := fmt.Sprintf("%sapi/v1/configuration/chargers/%d/charge-amperage-limit", a.identity.cfg.EndPoints.Chargers.Value, deviceID)

	data := struct {
		ChargeAmperageLimit int64 `json:"chargeAmperageLimit"`
	}{limit}

	return a.doJSON(func() (*http.Request, error) {
		return request.New(http.MethodPut, uri,
			request.MarshalJSON(data), request.JSONEncoding, a.cpInternalHeaders())
	}, nil)
}
