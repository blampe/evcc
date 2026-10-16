package mercedes

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/oauth"
	"github.com/evcc-io/evcc/util/request"
	"github.com/evcc-io/evcc/util/transport"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

type Identity struct {
	*request.Helper
	oauth2.TokenSource
	mu        sync.Mutex
	log       *util.Logger
	account   string
	region    string
	Sessionid string
}

// OAuth2Config is the OAuth2 configuration for authenticating with the MercedesAPI.
var OAuth2Config = &oauth2.Config{
	//	RedirectURL: fmt.Sprintf("%s/void/RedirectURL", IdUri),
	Endpoint: oauth2.Endpoint{
		// AuthURL:   fmt.Sprintf("%s/void/AuthURL", IdUri),
		TokenURL:  fmt.Sprintf("%s/as/token.oauth2", IdUri),
		AuthStyle: oauth2.AuthStyleInParams,
	},
	Scopes: []string{"not_needed", "handled", "elsewhere"},
}

// NewIdentity creates Mercedes identity
func NewIdentity(log *util.Logger, token *oauth2.Token, account string, region string) (*Identity, error) {
	// serialise instance handling
	mu.Lock()
	defer mu.Unlock()

	v := &Identity{
		Helper:  request.NewHelper(log),
		log:     log,
		account: account,
		region:  region,
	}

	v.Sessionid = uuid.New().String()
	v.Helper.Transport = &transport.Decorator{
		Base:      v.Helper.Transport, //.NewTripper(log, transport.Insecure()),
		Decorator: transport.DecorateHeaders(v.authHeaders()),
	}

	// reuse identity instance
	if instance := getInstance(account); instance != nil {
		v.log.DEBUG.Println("identity.NewIdentity - token found in instance store")
		return instance, nil
	}

	// store config token for potential re-use
	var configToken = token

	// database token
	var tok oauth2.Token
	if err := settings.Json(v.settingsKey(), &tok); err == nil {
		v.log.DEBUG.Println("identity.NewIdentity - database token found")
		token = &tok
	}

	if !token.Valid() && token.RefreshToken != "" {
		v.log.DEBUG.Println("identity.NewIdentity - refreshToken started")
		if tok, err := v.refreshToken(token); err == nil {
			token = tok
		}
	}

	if !token.Valid() {
		v.log.DEBUG.Println("identity.NewIdentity - config refreshToken started")
		if tok, err := v.refreshToken(configToken); err == nil {
			token = tok
		}
	}

	if !token.Valid() {
		return nil, errors.New("token expired")
	}

	v.TokenSource = oauth.RefreshTokenSource(token, v.refreshToken)

	// add instance
	addInstance(account, v)

	return v, nil
}

func (v *Identity) settingsKey() string {
	return fmt.Sprintf("mercedes.%s-%s", v.account, v.region)
}

// authHeaders returns the auth server headers with a device id that is stable
// across restarts, so token refreshes don't present as a new device each time.
func (v *Identity) authHeaders() map[string]string {
	headers := mbheaders(true, v.region)
	headers["X-Device-Id"] = deviceID(v.account, v.region)
	return headers
}

// deviceID derives a stable device id from the account and region.
func deviceID(account, region string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("evcc-mercedes:"+account+"-"+region)).String()
}

func (v *Identity) refreshToken(token *oauth2.Token) (*oauth2.Token, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {token.RefreshToken},
	}

	uri := fmt.Sprintf("%s/as/token.oauth2", IdUri)
	req, _ := request.New(http.MethodPost, uri, strings.NewReader(data.Encode()), v.authHeaders())

	var res oauth2.Token
	if err := v.DoJSON(req, &res); err != nil {
		return nil, err
	}

	if res.RefreshToken == "" {
		res.RefreshToken = token.RefreshToken
	}

	tok := util.TokenWithExpiry(&res)
	v.TokenSource = oauth.RefreshTokenSource(tok, v.refreshToken)

	err := settings.SetJson(v.settingsKey(), tok)

	return tok, err
}
