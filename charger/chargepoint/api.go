package chargepoint

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
)

// API is an HTTP client for the ChargePoint API. The shared cookie jar from
// Identity carries the coulomb_sess cookie to all endpoints automatically,
// mirroring how python-chargepoint uses requests.Session. Extra headers are
// only added for the internal API endpoint that requires them.
type API struct {
	identity    *Identity
	wsURL       string
	accountsURL string
	internalURL string
	mapcacheURL string
	region      string
}

// NewAPI creates a ChargePoint API client. It attaches the identity's cookie
// jar to the underlying http.Client so that Set-Cookie responses are captured
// and cookies are sent automatically.
func NewAPI(log *util.Logger, identity *Identity) *API {
	v := &API{
		identity:    identity,
		wsURL:       identity.cfg.EndPoints.WebServices.Value,
		accountsURL: identity.cfg.EndPoints.Accounts.Value,
		internalURL: identity.cfg.EndPoints.InternalAPI.Value,
		mapcacheURL: identity.cfg.EndPoints.MapCache.Value,
		region:      identity.Region,
	}

	return v
}

// Account fetches the account and returns the user ID.
func (a *API) Account() (int32, error) {
	var res struct {
		User struct {
			UserID int32 `json:"userId"`
		} `json:"user"`
	}
	if err := a.identity.GetJSON(a.accountsURL+"v1/driver/profile/user", &res); err != nil {
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

	req, err := request.New(http.MethodPost, a.wsURL+"mobileapi/v5", request.MarshalJSON(data), request.JSONEncoding)
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

// HomeChargerStatus returns the current status of a home charger.
func (a *API) HomeChargerStatus(deviceID int) (HomeChargerStatus, error) {
	data := struct {
		UserID         int32 `json:"user_id"`
		GetPandaStatus struct {
			DeviceID int      `json:"device_id"`
			MFHS     struct{} `json:"mfhs"`
		} `json:"get_panda_status"`
	}{UserID: a.identity.UserID}
	data.GetPandaStatus.DeviceID = deviceID

	req, err := request.New(http.MethodPost, a.wsURL+"mobileapi/v5", request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return HomeChargerStatus{}, err
	}

	var res struct {
		GetPandaStatus struct {
			IsPluggedIn           bool   `json:"is_plugged_in"`
			IsConnected           bool   `json:"is_connected"`
			ChargingStatus        string `json:"charging_status"`
			ChargeAmperageSetting struct {
				ChargeLimit int `json:"charge_limit"`
			} `json:"charge_amperage_setting"`
		} `json:"get_panda_status"`
	}
	if err := a.identity.DoJSON(req, &res); err != nil {
		return HomeChargerStatus{}, err
	}

	s := res.GetPandaStatus
	return HomeChargerStatus{
		IsPluggedIn:    s.IsPluggedIn,
		IsConnected:    s.IsConnected,
		ChargingStatus: s.ChargingStatus,
		AmpLimit:       s.ChargeAmperageSetting.ChargeLimit,
	}, nil
}

// SessionData returns the current charging session metrics.
func (a *API) SessionData() (SessionData, error) {
	data := struct {
		UserStatus struct {
			MFHS struct{} `json:"mfhs"`
		} `json:"user_status"`
	}{}

	req, err := request.New(http.MethodPost, a.mapcacheURL+"v2", request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return SessionData{}, err
	}

	var statusResp struct {
		UserStatus *struct {
			Charging *struct {
				SessionID int `json:"sessionId"`
			} `json:"charging"`
		} `json:"user_status"`
	}
	if err := a.identity.DoJSON(req, &statusResp); err != nil {
		return SessionData{}, err
	}

	if statusResp.UserStatus == nil || statusResp.UserStatus.Charging == nil {
		return SessionData{}, nil
	}

	sessionID := statusResp.UserStatus.Charging.SessionID
	query := url.QueryEscape(fmt.Sprintf(
		`{"user_id":%s,"charging_status":{"mfhs":{},"session_id":%d}}`,
		a.identity.UserID, sessionID))

	var chargingResp struct {
		ChargingStatus *struct {
			PowerKW   float64 `json:"power_kw"`
			EnergyKWh float64 `json:"energy_kwh"`
		} `json:"charging_status"`
	}
	if err := a.identity.GetJSON(a.mapcacheURL+"v2?"+query, &chargingResp); err != nil {
		return SessionData{}, err
	}

	if chargingResp.ChargingStatus == nil {
		return SessionData{}, nil
	}

	return SessionData{
		PowerKW:   chargingResp.ChargingStatus.PowerKW,
		EnergyKWh: chargingResp.ChargingStatus.EnergyKWh,
	}, nil
}

// StartSession starts a charging session on the given device and waits for acknowledgement.
func (a *API) StartSession(deviceID int) error {
	data := struct {
		DeviceData DeviceData `json:"deviceData"`
		DeviceID   int        `json:"deviceId"`
	}{
		DeviceData: a.identity.deviceData,
		DeviceID:   deviceID,
	}

	req, err := request.New(http.MethodPost, a.accountsURL+"v1/driver/station/startsession",
		request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return err
	}

	var res struct {
		AckID string `json:"ackId"`
	}
	if err := a.identity.DoJSON(req, &res); err != nil {
		return err
	}

	return a.pollAck(res.AckID, "start_session")
}

// StopSession stops any active charging session on the given device.
func (a *API) StopSession(deviceID int) error {
	data := struct {
		UserStatus struct {
			MFHS struct{} `json:"mfhs"`
		} `json:"user_status"`
	}{}

	req, err := request.New(http.MethodPost, a.mapcacheURL+"v2", request.MarshalJSON(data), request.JSONEncoding)
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
	if err := a.identity.DoJSON(req, &statusResp); err != nil {
		return err
	}

	if statusResp.UserStatus == nil || statusResp.UserStatus.Charging == nil {
		return nil
	}

	stopData := struct {
		DeviceData DeviceData `json:"deviceData"`
		DeviceID   int        `json:"deviceId"`
		PortNumber int        `json:"portNumber"`
		SessionID  int        `json:"sessionId"`
	}{
		DeviceData: a.identity.deviceData,
		DeviceID:   deviceID,
		PortNumber: 1,
		SessionID:  statusResp.UserStatus.Charging.SessionID,
	}

	req, err = request.New(http.MethodPost, a.accountsURL+"v1/driver/station/stopSession",
		request.MarshalJSON(stopData), request.JSONEncoding)
	if err != nil {
		return err
	}

	var res struct {
		AckID string `json:"ackId"`
	}
	if err := a.identity.DoJSON(req, &res); err != nil {
		return err
	}

	return a.pollAck(res.AckID, "stop_session")
}

func (a *API) pollAck(ackID, action string) error {
	ackData := struct {
		DeviceData DeviceData `json:"deviceData"`
		AckID      string     `json:"ackId"`
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
			request.MarshalJSON(ackData), request.JSONEncoding)
		if err != nil {
			return err
		}

		if err := a.identity.DoJSON(req, nil); err == nil {
			return nil
		}
	}

	return nil
}

// SetAmperageLimit sets the charge amperage limit on the given device.
// Per the ChargePoint API, this endpoint requires cp-session-type, cp-session-token,
// and cp-region headers in addition to the coulomb_sess cookie; these are added
// by the transport decorator for the internalURL prefix.
func (a *API) SetAmperageLimit(deviceID int, limit int64) error {
	uri := fmt.Sprintf("%s/driver/charger/%d/config/v1/charge-amperage-limit", a.internalURL, deviceID)

	data := struct {
		ChargeAmperageLimit int64 `json:"chargeAmperageLimit"`
	}{limit}

	req, err := request.New(http.MethodPost, uri, request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return err
	}

	var res struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := a.identity.DoJSON(req, &res); err != nil {
		return err
	}

	if res.Status != "success" {
		return fmt.Errorf("set amperage: %s", res.Message)
	}

	return nil
}
