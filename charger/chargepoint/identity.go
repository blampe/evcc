package chargepoint

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/transport"
	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/oauth2"
)

const (
	discoveryAPI = "https://discovery.chargepoint.com/discovery/v3/globalconfig"
	appVersion   = "5.97.0"
	loginUA      = "com.coulomb.ChargePoint/" + appVersion + " CFNetwork/1329 Darwin/21.3.0"
	sessionUA    = "ChargePoint/236 (iPhone; iOS 15.3; Scale/3.00)"
)

func newDeviceData() deviceData {
	return deviceData{
		AppID:              "com.coulomb.ChargePoint",
		Manufacturer:       "Apple",
		Model:              "iPhone",
		NotificationID:     "",
		NotificationIDType: "",
		Type:               "IOS",
		UDID:               uuid.New().String(),
		Version:            appVersion,
	}
}

// Login performs the ChargePoint mobile app login flow and returns OAuth2 tokens.
//
// The AccessToken is a short-lived "coulomb_sess" session cookie obtained by
// exchanging the sessionId via mobileapi/v5. It can be refreshed without
// re-authenticating using the Refresh function.
//
// The RefreshToken is the "sessionId" returned directly by the login endpoint.
// It embeds the region and user ID and is used as the credential for refresh calls.
func Login(log *util.Logger, username, password string) (*oauth2.Token, error) {
	client := &http.Client{
		Timeout:   request.Timeout,
		Transport: request.NewTripper(log, transport.Default()),
	}
	helper := &request.Helper{Client: client}

	dev := newDeviceData()

	cfg, err := discover(helper, dev, username)
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}

	res, err := login(helper, cfg, dev, username, password)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}

	coulombSess, err := refreshSession(log, cfg.EndPoints.WebServices.Value, res.SessionID)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}

	return &oauth2.Token{
		AccessToken:  coulombSess,
		RefreshToken: res.SessionID,
	}, nil
}

// Refresh exchanges the stored sessionId (RefreshToken) for a new coulomb_sess
// (AccessToken) without re-authenticating. It never calls the login endpoint,
// avoiding CAPTCHA challenges.
func Refresh(log *util.Logger, token *oauth2.Token) (*oauth2.Token, error) {
	sessionID := token.RefreshToken

	// Discover webservices endpoint (region-specific)
	client := &http.Client{
		Timeout:   request.Timeout,
		Transport: request.NewTripper(log, transport.Default()),
	}
	helper := &request.Helper{Client: client}

	cfg, err := discover(helper, newDeviceData(), SessionUserID(sessionID))
	if err != nil {
		// Fall back to known-good webservices URL using the region from sessionId
		// (avoids failure if discovery itself is temporarily unavailable)
		_ = err
		cfg = &globalConfig{}
		cfg.EndPoints.WebServices.Value = "https://webservices.chargepoint.com/backend.php/"
	}

	coulombSess, err := refreshSession(log, cfg.EndPoints.WebServices.Value, sessionID)
	if err != nil {
		return nil, err
	}

	return &oauth2.Token{
		AccessToken:  coulombSess,
		RefreshToken: sessionID,
	}, nil
}

// SessionRegion extracts the cp-region value embedded in a ChargePoint session token.
// Session tokens have the form "<data>#R<region>", e.g. "abc...#RNA-US".
func SessionRegion(sessionID string) string {
	if parts := strings.SplitN(sessionID, "#R", 2); len(parts) == 2 {
		return parts[1]
	}
	return ""
}

// SessionUserID extracts the user ID embedded in a ChargePoint session token.
// Session tokens encode the user ID as a hex value between "#D" and "#R".
func SessionUserID(sessionID string) string {
	_, after, found := strings.Cut(sessionID, "#D")
	if !found {
		return ""
	}
	hexID, _, _ := strings.Cut(after, "#")
	if len(hexID)%2 != 0 {
		hexID = "0" + hexID
	}
	b, err := hex.DecodeString(hexID)
	if err != nil || len(b) == 0 {
		return ""
	}
	// Decode big-endian integer
	var n uint64
	for _, v := range b {
		n = n<<8 | uint64(v)
	}
	return strconv.FormatUint(n, 10)
}

func discover(c *request.Helper, dev deviceData, username string) (*globalConfig, error) {
	data := struct {
		DeviceData deviceData `json:"deviceData"`
		Username   string     `json:"username"`
	}{dev, username}

	req, err := request.New(http.MethodPost, discoveryAPI, request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return nil, err
	}

	var cfg globalConfig
	if err := c.DoJSON(req, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func login(c *request.Helper, cfg *globalConfig, dev deviceData, username, password string) (*loginResponse, error) {
	data := struct {
		DeviceData deviceData `json:"deviceData"`
		Username   string     `json:"username"`
		Password   string     `json:"password"`
	}{dev, username, password}

	uri := cfg.EndPoints.Accounts.Value + "v2/driver/profile/account/login"
	req, err := request.New(http.MethodPost, uri, request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", loginUA)

	var res loginResponse
	if err := c.DoJSON(req, &res); err != nil {
		return nil, err
	}

	if res.SessionID == "" {
		return nil, fmt.Errorf("no session ID in login response")
	}

	return &res, nil
}

// refreshSession exchanges a session token (sessionId or coulomb_sess) for a
// fresh coulomb_sess cookie by calling the mobileapi/v5 endpoint. The user ID
// and region are extracted from the sessionId embedded metadata.
func refreshSession(log *util.Logger, webservicesURL, sessionID string) (string, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return "", err
	}

	client := &http.Client{
		Timeout:   request.Timeout,
		Transport: request.NewTripper(log, transport.Default()),
		Jar:       jar,
	}
	helper := &request.Helper{Client: client}

	data := struct {
		UserID string `json:"user_id"`
	}{SessionUserID(sessionID)}

	uri := webservicesURL + "mobileapi/v5"
	req, err := request.New(http.MethodPost, uri, request.MarshalJSON(data), request.JSONEncoding)
	if err != nil {
		return "", err
	}
	req.Header.Set("cp-session-type", "CP_SESSION_TOKEN")
	req.Header.Set("cp-session-token", sessionID)
	req.Header.Set("cp-region", SessionRegion(sessionID))
	req.Header.Set("User-Agent", sessionUA)

	if _, err := helper.DoBody(req); err != nil {
		return "", fmt.Errorf("mobileapi/v5: %w", err)
	}

	// The response sets coulomb_sess as a cookie
	u := req.URL
	for _, c := range jar.Cookies(u) {
		if c.Name == "coulomb_sess" {
			return c.Value, nil
		}
	}

	return "", fmt.Errorf("coulomb_sess cookie not found in mobileapi/v5 response")
}
