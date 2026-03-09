// Package chargepoint implements authentication for the ChargePoint EV charging network.
//
// # Authentication overview
//
// ChargePoint uses a two-stage session model rather than standard OAuth2:
//
//  1. Login (one-time): POST credentials to the mobile app API endpoint with an
//     iOS device fingerprint. Returns a "sessionId" that encodes the user ID and
//     region directly in its structure: "<token>#D<userIDhex>#R<region>".
//
//  2. Session exchange: POST the sessionId to mobileapi/v5, which returns a
//     short-lived 32-character hex "coulomb_sess" cookie used for actual API calls.
//
// # Token storage
//
// The oauth2.Token fields map to ChargePoint's model as follows:
//   - AccessToken  = coulomb_sess (the active credential for API calls)
//   - RefreshToken = sessionId    (durable credential used to obtain new coulomb_sess tokens)
//
// # Refresh without re-login
//
// The mobileapi/v5 endpoint accepts either a sessionId or an existing coulomb_sess
// as the cp-session-token header and issues a fresh coulomb_sess in response.
// This means tokens can be refreshed indefinitely without ever calling the login
// endpoint again:
//
//	sessionId → mobileapi/v5 → coulomb_sess₁ → mobileapi/v5 → coulomb_sess₂ → …
//
// # CAPTCHA protection
//
// Both the SSO endpoint (sso.chargepoint.com) and the mobile app endpoint
// (account.chargepoint.com) are protected by DataDome bot detection. Repeated
// login attempts from the same IP will trigger a CAPTCHA challenge and return
// HTTP 403. The response body contains a captcha-delivery.com redirect URL.
//
// To avoid this: Login should be treated as a strictly one-time operation used
// only to bootstrap tokens via "evcc chargepoint-token". All subsequent credential
// management must go through Refresh, which only calls mobileapi/v5 and is not
// subject to the same bot protection.
package chargepoint

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
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
	// loginUA mimics the ChargePoint iOS app, which is required to avoid bot
	// detection on the login endpoint.
	loginUA = "com.coulomb.ChargePoint/" + appVersion + " CFNetwork/1329 Darwin/21.3.0"
	// sessionUA is used for post-login API calls including mobileapi/v5 refresh.
	sessionUA = "ChargePoint/236 (iPhone; iOS 15.3; Scale/3.00)"
)

// deviceUDID returns a stable UUID v5 derived from the machine hostname,
// mimicking a real iOS device that always presents the same UDID.
func deviceUDID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "evcc"
	}
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(host)).String()
}

func newDeviceData() deviceData {
	return deviceData{
		AppID:              "com.coulomb.ChargePoint",
		Manufacturer:       "Apple",
		Model:              "iPhone",
		NotificationID:     "",
		NotificationIDType: "",
		Type:               "IOS",
		UDID:               deviceUDID(),
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
//
// WARNING: The login endpoint is protected by DataDome bot detection. Calling
// Login repeatedly from the same IP will trigger CAPTCHA challenges. Run this
// once via "evcc chargepoint-token" and store the resulting tokens.
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

	// Discover the region-specific webservices endpoint. If discovery fails,
	// fall back to the default URL to avoid compounding a transient outage.
	client := &http.Client{
		Timeout:   request.Timeout,
		Transport: request.NewTripper(log, transport.Default()),
	}
	helper := &request.Helper{Client: client}

	cfg, err := discover(helper, newDeviceData(), SessionUserID(sessionID))
	if err != nil {
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
// fresh coulomb_sess by calling the mobileapi/v5 endpoint.
//
// The cp-region and user_id required by the endpoint are extracted from the
// metadata embedded in sessionId. The coulomb_sess is returned as a cookie in
// the response rather than in the JSON body.
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

	for _, c := range jar.Cookies(req.URL) {
		if c.Name == "coulomb_sess" {
			return c.Value, nil
		}
	}

	return "", fmt.Errorf("coulomb_sess cookie not found in mobileapi/v5 response")
}
