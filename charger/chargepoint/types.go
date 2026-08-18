package chargepoint

// HomeChargerStatus holds the current status of a home charger.
type HomeChargerStatus struct {
	ChargingStatus         string `json:"chargingStatus"` // AVAILABLE, CHARGING
	IsPluggedIn            bool   `json:"isPluggedIn"`
	IsConnected            bool   `json:"isConnected"`
	ChargeAmperageSettings struct {
		ChargeLimit         int64   `json:"chargeLimit"`
		PossibleChargeLimit []int64 `json:"possibleChargeLimit"`
	} `json:"chargeAmperageSettings"`
}

// UserChargingStatus holds the active session state from the mapcache API.
type UserChargingStatus struct {
	SessionID int    `json:"sessionId"`
	State     string `json:"state"`
}

// SessionDetails holds live session metrics from the driver-bff API.
type SessionDetails struct {
	ChargingState string  `json:"current_charging"`
	ChargingTime  int     `json:"charging_time"`
	EnergyKWh     float64 `json:"energy_kwh"`
	PowerKW       float64 `json:"power_kw"`
	DeviceID      int     `json:"device_id"`
	OutletNumber  int     `json:"outlet_number"`
}

// DeviceData is the iOS device fingerprint included in ChargePoint API requests.
type DeviceData struct {
	AppID              string `json:"appId"`
	Manufacturer       string `json:"manufacturer"`
	Model              string `json:"model"`
	NotificationID     string `json:"notificationId"`
	NotificationIDType string `json:"notificationIdType"`
	Type               string `json:"type"`
	UDID               string `json:"udid"`
	Version            string `json:"version"`
}

type endpointValue struct {
	Value string `json:"value"`
}

type configEndpoints struct {
	Accounts    endpointValue `json:"accounts_endpoint"`
	Chargers    endpointValue `json:"hcpo_hcm_endpoint"`
	InternalAPI endpointValue `json:"internal_api_gateway_endpoint"`
	MapCache    endpointValue `json:"mapcache_endpoint"`
	SSO         endpointValue `json:"sso_endpoint"`
	WebServices endpointValue `json:"webservices_endpoint"`
}

type globalConfig struct {
	Region    string          `json:"region"`
	EndPoints configEndpoints `json:"endPoints"`
}

type accountLoginResponse struct {
	SessionID    string `json:"sessionId"`
	SSOSessionID string `json:"ssoSessionId"`
	User         struct {
		UserID int32 `json:"userId"`
	} `json:"user"`
}
