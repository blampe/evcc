package chargepoint

type deviceData struct {
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
	WebServices endpointValue `json:"webservices_endpoint"`
}

type globalConfig struct {
	Region    string          `json:"region"`
	EndPoints configEndpoints `json:"endPoints"`
}

type loginResponse struct {
	SessionID string `json:"sessionId"`
	User      struct {
		UserID int `json:"userId"`
	} `json:"user"`
}


