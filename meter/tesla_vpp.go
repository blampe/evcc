package meter

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
	"golang.org/x/oauth2"
)

// The Tesla Fleet API does not expose the Virtual Power Plant (grid-services)
// event schedule — its /programs endpoint returns an empty response. The Tesla
// app reads the schedule from the legacy owner-api, gated behind the app's
// X-Tesla-User-Agent header (without it the endpoint returns an empty string too).
// We mimic that request to learn when a VPP event is scheduled so the optimizer
// can reflect the export premium in the feed-in tariff for that window.
const (
	ownerAPIBaseURL = "https://owner-api.teslamotors.com/api/1"

	// teslaAppUserAgent is the gate: owner-api /programs returns `{"response":""}`
	// without it. Tracks a recent Tesla iOS app build; may need refreshing if Tesla
	// tightens the check.
	teslaAppUserAgent = "com.teslamotors.TeslaApp/4.59.5-4504/4eb68a6d/iOS/26.6.1"

	// ownerAPIClientID is the legacy first-party OAuth client that mints owner-api
	// tokens (no client secret). Distinct from the Fleet API partner client.
	ownerAPIClientID = "ownerapi"
	ownerAPITokenURL = "https://auth.tesla.com/oauth2/v3/token"

	// vppPollInterval is how often the grid-services schedule is fetched. Events are
	// announced ~a day ahead and change rarely, so a slow poll is plenty (and keeps
	// the owner-api token refreshes infrequent).
	vppPollInterval = 15 * time.Minute
)

// ownerAPIOAuthConfig is the OAuth2 config for the legacy ownerapi client.
func ownerAPIOAuthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID: ownerAPIClientID,
		Endpoint: oauth2.Endpoint{
			TokenURL:  ownerAPITokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

// tls12Transport returns a transport capped at TLS 1.2. owner-api fingerprints the
// TLS handshake that MINTS a token and rejects (403 "forbidden") tokens minted
// over TLS 1.3 — the minted token is claim-identical to a good one but for its
// encrypted `x-enc`. So the token refresh must happen over TLS 1.2. (The API calls
// themselves are fine over any TLS version — only the mint is fingerprinted.)
func tls12Transport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = new(tls.Config)
	} else {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	}
	tr.TLSClientConfig.MaxVersion = tls.VersionTLS12
	return tr
}

// ownerAPIRefresh exchanges an ownerapi refresh token for a fresh access token
// over TLS 1.2 (see tls12Transport). It forces a refresh regardless of the passed
// token's expiry.
func ownerAPIRefresh(token *oauth2.Token) (*oauth2.Token, error) {
	forced := *token
	forced.Expiry = time.Unix(1, 0) // long past → force the refresh grant
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: tls12Transport()})
	return ownerAPIOAuthConfig().TokenSource(ctx, &forced).Token()
}

// jwtSubject extracts the `sub` claim from a JWT without verifying it — used only
// to derive a per-user settings key for token persistence.
func jwtSubject(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(raw, &claims)
	return claims.Sub
}

// ownerAPITokenSource is an oauth2.TokenSource for the ownerapi client that
// refreshes over TLS 1.2 and persists the (rotating) token to settings. Tesla
// invalidates a refresh token once it has been used, so the rotated token must be
// stored or a restart would fall back to a dead one.
type ownerAPITokenSource struct {
	mu  sync.Mutex
	tok *oauth2.Token
	key string
	log *util.Logger
}

// newOwnerAPITokenSource seeds from a persisted token if present (it carries the
// latest rotated refresh token), else from the configured refresh token.
func newOwnerAPITokenSource(log *util.Logger, refreshToken string) *ownerAPITokenSource {
	var key string
	if sub := jwtSubject(refreshToken); sub != "" {
		key = "tesla-ownerapi." + sub
	}
	tok := &oauth2.Token{RefreshToken: refreshToken, Expiry: time.Unix(1, 0)}
	if key != "" {
		var stored oauth2.Token
		if err := settings.Json(key, &stored); err == nil && stored.RefreshToken != "" {
			tok = &stored
		}
	}
	return &ownerAPITokenSource{tok: tok, key: key, log: log}
}

func (s *ownerAPITokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tok.Valid() {
		return s.tok, nil
	}
	nt, err := ownerAPIRefresh(s.tok)
	if err != nil {
		return nil, err
	}
	s.tok = nt
	if s.key != "" {
		if err := settings.SetJson(s.key, nt); err != nil {
			s.log.WARN.Printf("persist owner-api token: %v", err)
		} else {
			_ = settings.Persist()
		}
	}
	return nt, nil
}

// vppEvent is a scheduled grid-services (VPP) event window.
type vppEvent struct {
	ID      string
	Program string
	Start   time.Time
	End     time.Time
	OptedIn bool
}

// active reports whether the event covers t.
func (e vppEvent) active(t time.Time) bool {
	return !t.Before(e.Start) && t.Before(e.End)
}

// vppClient fetches VPP event schedules from the Tesla owner-api. It needs an
// owner-api (legacy `ownerapi` client) OAuth token — distinct from the Fleet API
// token the rest of the meter uses.
type vppClient struct {
	hc  *http.Client
	log *util.Logger
}

// newVPPClient builds an owner-api client authenticated by the given token source.
func newVPPClient(log *util.Logger, ts oauth2.TokenSource) *vppClient {
	hc := request.NewClient(log)
	hc.Transport = &oauth2.Transport{Source: ts, Base: hc.Transport}
	return &vppClient{hc: hc, log: log}
}

// events fetches the site's grid-services programs and returns their event
// windows. A site with no scheduled events yields an empty slice (not an error).
func (c *vppClient) events(siteID int64) ([]vppEvent, error) {
	url := fmt.Sprintf("%s/energy_sites/%d/programs?language=en", ownerAPIBaseURL, siteID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// the UA header is the gate; Accept mirrors the app. Encoding is left to the
	// transport (handled below regardless).
	req.Header.Set("X-Tesla-User-Agent", teslaAppUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// the owner-api responds gzip (and the transport may or may not have already
	// decompressed it depending on the Accept-Encoding handling) — gunzip by magic
	// bytes so we are correct either way.
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("gunzip programs: %w", err)
		}
		if body, err = io.ReadAll(zr); err != nil {
			return nil, fmt.Errorf("gunzip programs: %w", err)
		}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("owner-api programs: %s", resp.Status)
	}
	return parseVPPEvents(body)
}

// programsPayload models the parts of the owner-api /programs response we use.
// `response` is polymorphic: an empty string when the site has no programs, or an
// object when it does — so it is decoded as RawMessage and branched on.
type programsPayload struct {
	Response json.RawMessage `json:"response"`
}

type programsData struct {
	Programs []struct {
		ProgramName string `json:"program_name"`
		Events      []struct {
			WatchEventID string `json:"watch_event_id"`
			OptedIn      bool   `json:"opted_in"`
			Start        struct {
				Seconds int64 `json:"seconds"`
			} `json:"start"`
			End struct {
				Seconds int64 `json:"seconds"`
			} `json:"end"`
		} `json:"events"`
	} `json:"programs"`
}

// parseVPPEvents decodes the (possibly stream-concatenated) programs response into
// event windows. The owner-api has been observed to return more than one JSON
// object back-to-back, so we decode until EOF and merge.
func parseVPPEvents(body []byte) ([]vppEvent, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var events []vppEvent
	for {
		var outer programsPayload
		if err := dec.Decode(&outer); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		// empty ("") response, or anything that isn't an object → no programs here
		trimmed := bytes.TrimSpace(outer.Response)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var data programsData
		if err := json.Unmarshal(trimmed, &data); err != nil {
			return nil, err
		}
		for _, p := range data.Programs {
			for _, e := range p.Events {
				if e.Start.Seconds == 0 || e.End.Seconds == 0 {
					continue
				}
				events = append(events, vppEvent{
					ID:      e.WatchEventID,
					Program: p.ProgramName,
					Start:   time.Unix(e.Start.Seconds, 0),
					End:     time.Unix(e.End.Seconds, 0),
					OptedIn: e.OptedIn,
				})
			}
		}
	}
	return events, nil
}

// currentOrNextEvent returns the opted-in event that is active at now, or the
// soonest upcoming one; ok is false when there is none.
func currentOrNextEvent(events []vppEvent, now time.Time) (vppEvent, bool) {
	var best vppEvent
	var ok bool
	for _, e := range events {
		if !e.OptedIn || !now.Before(e.End) {
			continue // not opted in, or already over
		}
		if !ok || e.Start.Before(best.Start) {
			best, ok = e, true
		}
	}
	return best, ok
}

// vppPremiumDefault is the export payout ($/kWh) the optimizer plans against
// during a VPP event. Tesla does not expose the program's actual rate anywhere in
// its APIs, so it is a hard-coded default (PG&E DSGS ≈ $2/kWh) until configurable.
const vppPremiumDefault = 2.0

// applyVPPPremium overlays the VPP export premium onto feed-in rates for slots
// overlapping an opted-in event window, so the optimizer values export during the
// event at the program payout — driving it to pre-charge for and discharge into
// the event. It only raises a slot's value (never lowers one already worth more).
func applyVPPPremium(rates api.Rates, events []vppEvent, premium float64) api.Rates {
	for i := range rates {
		for _, e := range events {
			if !e.OptedIn {
				continue
			}
			// slot [Start,End) overlaps event [Start,End)
			if rates[i].Start.Before(e.End) && rates[i].End.After(e.Start) {
				rates[i].Value = max(rates[i].Value, premium)
				break
			}
		}
	}
	return rates
}
