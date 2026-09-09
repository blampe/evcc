package meter

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/util"
	evcctesla "github.com/evcc-io/evcc/vehicle/tesla"
	teslaclient "github.com/evcc-io/tesla-proxy-client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// TestProbeState is a read-only live snapshot (PW_PROBE=1) of the Powerwall:
// operating mode, backup reserve, export rule, the current tariff (real vs
// synthetic + current-hour buy/sell), and power flows over a few readings.
func TestProbeState(t *testing.T) {
	if os.Getenv("PW_PROBE") != "1" {
		t.Skip("set PW_PROBE=1 to run the live PW3 state probe")
	}
	clientID := os.Getenv("PW_CLIENTID")
	siteID, err := strconv.ParseInt(os.Getenv("PW_SITEID"), 10, 64)
	require.NoError(t, err)
	expiry, err := time.Parse(time.RFC3339, os.Getenv("PW_EXPIRY"))
	require.NoError(t, err)
	token := &oauth2.Token{
		AccessToken:  os.Getenv("PW_TOKEN"),
		RefreshToken: os.Getenv("PW_REFRESH"),
		TokenType:    "Bearer",
		Expiry:       expiry,
	}
	require.NotEmpty(t, token.AccessToken, "PW_TOKEN")

	tc, err := newFleetClient(util.NewLogger("probe"), evcctesla.OAuth2Config(clientID, ""), token)
	require.NoError(t, err)
	site, err := tc.EnergySite(siteID)
	require.NoError(t, err)

	t.Logf("mode=%q reserve=%d%% exportRule=%q netMeter=%q",
		site.DefaultRealMode, site.BackupReservePercent,
		site.Components.CustomerPreferredExportRule, site.Components.NetMeterMode)

	if tv := site.TariffContentV2; tv != nil {
		now := time.Now()
		buy, _ := rateAtV2(tv.Seasons, tv.EnergyCharges, now)
		var sell float64
		if tv.SellTariff != nil {
			sell, _ = rateAtV2(tv.SellTariff.Seasons, tv.SellTariff.EnergyCharges, now)
		}
		t.Logf("tariff: utility=%q name=%q synthetic=%v | current-hour buy=%.4g sell=%.4g",
			tv.Utility, tv.Name, isSyntheticTariff(tv), buy, sell)
	} else {
		t.Log("tariff: no v2 tariff")
	}

	t.Log("battery power sign: negative = charging, positive = discharging")
	for i := range 4 {
		st, err := site.EnergySiteLiveStatus()
		if err != nil {
			t.Logf("live status error: %v", err)
		} else {
			t.Logf("live: grid=%.0fW solar=%.0fW battery=%.0fW load=%.0fW soc=%.1f%%",
				st.GridPower, st.SolarPower, st.BatteryPower, st.LoadPower, st.PercentageCharged)
		}
		if i < 3 {
			time.Sleep(15 * time.Second)
		}
	}
}

// TestProbeSellSpike is a live, opt-in experiment (PW_PROBE=1): write a SELL-ONLY
// spike tariff (buy left at the real price) to the real Powerwall and watch live
// status to see whether it stops charging — vs the buy+sell spike that didn't.
//
// Run with evcc stopped so nothing else writes the tariff — the spike is written
// once and left in place so the PW3's TOU algorithm gets an uninterrupted window
// (up to ~5 min) to react.
//
// Env (from the DB): PW_CLIENTID, PW_SITEID, PW_TOKEN, PW_REFRESH, PW_EXPIRY
// (RFC3339). The real tariff is read live from the site (valid while evcc is
// stopped, having restored it).
func TestProbeSellSpike(t *testing.T) {
	if os.Getenv("PW_PROBE") != "1" {
		t.Skip("set PW_PROBE=1 to run the live PW3 sell-spike probe")
	}

	clientID := os.Getenv("PW_CLIENTID")
	siteID, err := strconv.ParseInt(os.Getenv("PW_SITEID"), 10, 64)
	require.NoError(t, err)
	expiry, err := time.Parse(time.RFC3339, os.Getenv("PW_EXPIRY"))
	require.NoError(t, err)
	token := &oauth2.Token{
		AccessToken:  os.Getenv("PW_TOKEN"),
		RefreshToken: os.Getenv("PW_REFRESH"),
		TokenType:    "Bearer",
		Expiry:       expiry,
	}
	require.NotEmpty(t, token.AccessToken, "PW_TOKEN")

	log := util.NewLogger("probe")
	tc, err := newFleetClient(log, evcctesla.OAuth2Config(clientID, ""), token)
	require.NoError(t, err)

	site, err := tc.EnergySite(siteID)
	require.NoError(t, err)

	// evcc is stopped and restored the real tariff, so the live one is real
	require.NotNil(t, site.TariffContentV2, "no v2 tariff on site")
	real := *site.TariffContentV2
	if isSyntheticTariff(&real) {
		t.Log("live tariff is a leftover synthetic spike; recovering the real tariff by merging _SPIKE periods back")
		real = recoverReal(real)
	}

	// bump magnitudes are env-driven so we can escalate without recompiling
	buyBump := envFloat("PW_BUY_BUMP", 0)
	sellBump := envFloat("PW_SELL_BUMP", dischargeSpikeBump)

	now := time.Now()
	buyReal, _ := rateAtV2(real.Seasons, real.EnergyCharges, now)
	var sellReal float64
	if real.SellTariff != nil {
		sellReal, _ = rateAtV2(real.SellTariff.Seasons, real.SellTariff.EnergyCharges, now)
	}
	spike := spikeTariffBumps(&real, now, buyBump, sellBump)
	buySpk, _ := rateAtV2(spike.Seasons, spike.EnergyCharges, now)
	var sellSpk float64
	if spike.SellTariff != nil {
		sellSpk, _ = rateAtV2(spike.SellTariff.Seasons, spike.SellTariff.EnergyCharges, now)
	}
	t.Logf("spike @ %s (buyBump=%g sellBump=%g): utility=%q buy %.4g->%.4g sell %.4g->%.4g",
		now.Format("15:04"), buyBump, sellBump, spike.Utility, buyReal, buySpk, sellReal, sellSpk)
	t.Log("battery power sign: negative = charging, positive = discharging")
	t.Logf("current operating mode=%q export rule=%q", site.DefaultRealMode, site.Components.CustomerPreferredExportRule)

	// current soc for the hold reserve
	st0, err := site.EnergySiteLiveStatus()
	require.NoError(t, err)
	reserve := clampReserve(st0.PercentageCharged)

	// autonomous (so the TOU algo acts on the tariff) + battery_ok export + reserve
	// at the current soc (holds it there — export surplus, no dump below current)
	require.NoError(t, site.SetOperatingMode("autonomous"))
	require.NoError(t, site.SetGridExport("battery_ok"))
	require.NoError(t, site.SetRawTariff(spike))
	require.NoError(t, site.SetBatteryReserve(reserve))
	t.Logf("set autonomous + battery_ok + spike + reserve=%d%% (soc=%.1f%%); polling ~11 min...",
		reserve, st0.PercentageCharged)

	start := time.Now()
	for i := 0; i <= 22; i++ {
		st, err := site.EnergySiteLiveStatus()
		el := time.Since(start).Round(time.Second)
		if err != nil {
			t.Logf("[+%s] live status error: %v", el, err)
		} else {
			t.Logf("[+%s] grid=%.0fW solar=%.0fW battery=%.0fW load=%.0fW soc=%.1f%%",
				el, st.GridPower, st.SolarPower, st.BatteryPower, st.LoadPower, st.PercentageCharged)
		}
		if i < 22 {
			time.Sleep(30 * time.Second)
		}
	}
	// restore the (recovered) real tariff so we stop leaving synthetic tariffs on
	// the PW3 and losing the real one
	if err := site.SetRawTariff(real); err != nil {
		t.Logf("restore real tariff failed: %v", err)
	} else {
		t.Logf("restored real tariff (utility=%q)", real.Utility)
	}
	t.Log("probe done; restart evcc to resume control")
}

// TestProbeRateTariffs is a read-only probe (PW_PROBE=1) of the catalog lookup
// GET /api/1/energy_sites/rate_tariffs?tariff=NAME. It tries the plan name(s) in
// PW_TARIFF_NAMES (comma-separated) and dumps the raw response so we can see the
// exact shape (and whether the pack's mangled Code works as a lookup key vs the
// real plan name).
//
// Env: PW_CLIENTID, PW_TOKEN, PW_REFRESH, PW_EXPIRY, and PW_TARIFF_NAMES.
func TestProbeRateTariffs(t *testing.T) {
	if os.Getenv("PW_PROBE") != "1" {
		t.Skip("set PW_PROBE=1 to run the live rate_tariffs catalog probe")
	}
	clientID := os.Getenv("PW_CLIENTID")
	expiry, err := time.Parse(time.RFC3339, os.Getenv("PW_EXPIRY"))
	require.NoError(t, err)
	token := &oauth2.Token{
		AccessToken:  os.Getenv("PW_TOKEN"),
		RefreshToken: os.Getenv("PW_REFRESH"),
		TokenType:    "Bearer",
		Expiry:       expiry,
	}
	require.NotEmpty(t, token.AccessToken, "PW_TOKEN")

	tc, err := newFleetClient(util.NewLogger("probe"), evcctesla.OAuth2Config(clientID, ""), token)
	require.NoError(t, err)

	// what does the SITE expose as an identifier? is TariffID the lookup key?
	products, err := tc.Products()
	require.NoError(t, err)
	var siteID int64
	for _, p := range products {
		if p.EnergySiteId != 0 {
			siteID = p.EnergySiteId
			break
		}
	}
	if siteID != 0 {
		if site, err := tc.EnergySite(siteID); err == nil {
			code := ""
			if site.TariffContentV2 != nil {
				code = site.TariffContentV2.Code
			}
			t.Logf("SITE: tariff_id=%q  tariff_content_v2.code=%q", site.TariffID, code)
		}
	}

	for name := range strings.SplitSeq(os.Getenv("PW_TARIFF_NAMES"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		body, err := tc.RateTariffs(name)
		if err != nil {
			t.Logf("rate_tariffs?tariff=%q -> ERROR: %v", name, err)
			continue
		}
		path := "/tmp/rate_tariffs_" + strings.ReplaceAll(name, "/", "_") + ".json"
		_ = os.WriteFile(path, body, 0o644)
		out := string(body)
		if len(out) > 700 {
			out = out[:700] + "...(truncated)"
		}
		t.Logf("rate_tariffs?tariff=%q -> %d bytes (full: %s):\n%s", name, len(body), path, out)
	}
}

// TestProbeRestoreByNameViaCatalog validates the ACTUAL restore-by-name path
// (PW_PROBE=1): look the plan up in Tesla's catalog by its lookup name
// (GET rate_tariffs?tariff=NAME), spike the pack, then restore by POSTing the
// catalog content back (tou_settings.tariff_content). Confirms we can recover the
// real, fully-labeled plan with only the configured plan name — no stored content.
//
// Env: PW_CLIENTID, PW_TOKEN, PW_REFRESH, PW_EXPIRY, PW_TARIFF_NAME (the catalog
// lookup key, e.g. CPSF-E-ELEC-NEM3-2026 — NOT the pack's stored code).
func TestProbeRestoreByNameViaCatalog(t *testing.T) {
	if os.Getenv("PW_PROBE") != "1" {
		t.Skip("set PW_PROBE=1 to run the live restore-by-name-via-catalog probe")
	}
	clientID := os.Getenv("PW_CLIENTID")
	name := os.Getenv("PW_TARIFF_NAME")
	require.NotEmpty(t, name, "PW_TARIFF_NAME (catalog lookup key)")
	expiry, err := time.Parse(time.RFC3339, os.Getenv("PW_EXPIRY"))
	require.NoError(t, err)
	token := &oauth2.Token{AccessToken: os.Getenv("PW_TOKEN"), RefreshToken: os.Getenv("PW_REFRESH"), TokenType: "Bearer", Expiry: expiry}
	require.NotEmpty(t, token.AccessToken, "PW_TOKEN")

	tc, err := newFleetClient(util.NewLogger("probe"), evcctesla.OAuth2Config(clientID, ""), token)
	require.NoError(t, err)
	products, err := tc.Products()
	require.NoError(t, err)
	var siteID int64
	for _, p := range products {
		if p.EnergySiteId != 0 {
			siteID = p.EnergySiteId
			break
		}
	}
	require.NotZero(t, siteID)
	site, err := tc.EnergySite(siteID)
	require.NoError(t, err)

	// look the plan up by name in the catalog
	plan, err := tc.RateTariff(name)
	require.NoError(t, err, "catalog lookup rate_tariffs?tariff=%q", name)
	t.Logf("catalog lookup %q -> code=%q name=%q utility=%q", name, plan.Code, plan.Name, plan.Utility)
	require.NotEmpty(t, plan.Code, "catalog returned no code")

	// current pack tariff: source for the spike + safety-net fallback
	require.NotNil(t, site.TariffContentV2)
	cur := *site.TariffContentV2
	if isSyntheticTariff(&cur) {
		cur = *unspikeTariff(&cur)
	}

	restored := false
	defer func() {
		if restored {
			return
		}
		t.Log("restore-by-name failed; falling back to raw restore")
		if err := site.SetRawTariff(cur); err != nil {
			t.Errorf("FALLBACK raw restore failed — pack may be left spiked: %v", err)
		}
	}()

	// spike the pack
	now := time.Now()
	require.NoError(t, site.SetRawTariff(spikeTariff(&cur, now)))
	after, err := tc.EnergySite(siteID)
	require.NoError(t, err)
	require.True(t, isSyntheticTariff(after.TariffContentV2), "spike did not take")
	t.Logf("spiked: utility=%q (pack still carries real code=%q)", after.TariffContentV2.Utility, after.TariffContentV2.Code)

	// THE TEST: restore using only the catalog plan (looked up by name)
	t.Logf("restoring by name: POST catalog tariff_content for %q...", name)
	require.NoError(t, site.SetTariffContent(*plan))

	back, err := tc.EnergySite(siteID)
	require.NoError(t, err)
	require.NotNil(t, back.TariffContentV2, "no tariff after restore")
	bc := back.TariffContentV2
	t.Logf("after restore-by-name: code=%q name=%q utility=%q synthetic=%v", bc.Code, bc.Name, bc.Utility, isSyntheticTariff(bc))
	require.False(t, isSyntheticTariff(bc), "still synthetic after catalog restore")
	require.Equal(t, plan.Code, bc.Code, "restored plan code differs from catalog")
	require.Equal(t, plan.Utility, bc.Utility, "restored utility label differs")

	buyBack, _ := rateAtV2(bc.Seasons, bc.EnergyCharges, now)
	var sellBack float64
	if bc.SellTariff != nil {
		sellBack, _ = rateAtV2(bc.SellTariff.Seasons, bc.SellTariff.EnergyCharges, now)
	}
	t.Logf("restored current-hour rates: buy=%.4g sell=%.4g", buyBack, sellBack)

	restored = true
	t.Log("restore-by-name via catalog CONFIRMED: plan name alone recovers the real, labeled tariff")
}

// TestProbeVPP is a READ-ONLY live probe (PW_PROBE=1) that hunts for how a Tesla
// Virtual Power Plant (grid-services) event surfaces in the Fleet API, so we can
// wire (a) the VPP export premium into evcc's tariffs and (b) the VPP-specific
// min-SoC into the optimizer. It dumps the full raw JSON of every plausible
// endpoint (typed structs hide unmodeled fields) and highlights any key whose
// name mentions grid/service/vpp/program/reserve/tariff/soc/price.
//
// Nothing is written to the pack. Safe to run while evcc is up.
//
// Env (from the DB): PW_CLIENTID, PW_SITEID (optional; auto-discovered), PW_TOKEN,
// PW_REFRESH, PW_EXPIRY (RFC3339).
func TestProbeVPP(t *testing.T) {
	if os.Getenv("PW_PROBE") != "1" {
		t.Skip("set PW_PROBE=1 to run the live VPP probe")
	}
	clientID := os.Getenv("PW_CLIENTID")
	expiry, err := time.Parse(time.RFC3339, os.Getenv("PW_EXPIRY"))
	require.NoError(t, err)
	token := &oauth2.Token{
		AccessToken:  os.Getenv("PW_TOKEN"),
		RefreshToken: os.Getenv("PW_REFRESH"),
		TokenType:    "Bearer",
		Expiry:       expiry,
	}
	require.NotEmpty(t, token.AccessToken, "PW_TOKEN")

	tc, err := newFleetClient(util.NewLogger("probe"), evcctesla.OAuth2Config(clientID, ""), token)
	require.NoError(t, err)

	siteID, _ := strconv.ParseInt(os.Getenv("PW_SITEID"), 10, 64)
	if siteID == 0 {
		products, err := tc.Products()
		require.NoError(t, err)
		for _, p := range products {
			if p.EnergySiteId != 0 {
				siteID = p.EnergySiteId
				break
			}
		}
	}
	require.NotZero(t, siteID, "no energy site")
	t.Logf("site %d", siteID)

	base := "/energy_sites/" + strconv.FormatInt(siteID, 10)
	// Every energy endpoint that might carry VPP / grid-services state. Some may
	// 404 for this account/region — we log that rather than fail, since the point
	// is to discover which ones exist and what they carry.
	endpoints := []struct{ label, path string }{
		{"live_status", base + "/live_status"},
		{"site_info", base + "/site_info"},
		{"site_status", base + "/site_status"},
		{"programs", base + "/programs"},
		{"grid_services_history", base + "/calendar_history?kind=grid_services&period=month"},
		{"energy_history", base + "/calendar_history?kind=energy&period=day"},
		{"backup_history", base + "/calendar_history?kind=backup&period=month"},
		{"tou_settings", base + "/tou_settings"},
	}

	// keys worth surfacing from any level of the JSON
	keywords := []string{"grid", "service", "vpp", "program", "reserve", "tariff", "soc", "price", "rate", "charge", "island", "storm", "enroll", "event", "opt"}

	for _, ep := range endpoints {
		body, err := tc.GetRaw(ep.path)
		if err != nil {
			// dump the (error) body too — Tesla puts the reason there
			snippet := string(body)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			t.Logf("\n=== %s -> ERROR: %v\n%s", ep.label, err, snippet)
			continue
		}
		path := "/tmp/vpp_" + ep.label + ".json"
		_ = os.WriteFile(path, body, 0o644)

		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Logf("\n=== %s (%d bytes, %s): not JSON: %v", ep.label, len(body), path, err)
			continue
		}
		hits := map[string]string{}
		collectMatchingKeys(doc, "", keywords, hits)
		keys := make([]string, 0, len(hits))
		for k := range hits {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		fmt.Fprintf(&b, "\n=== %s (%d bytes, full: %s) — %d matching keys:", ep.label, len(body), path, len(keys))
		for _, k := range keys {
			fmt.Fprintf(&b, "\n    %s = %s", k, hits[k])
		}
		t.Log(b.String())
	}

	t.Log("\nprobe done — full bodies in /tmp/vpp_*.json")
}

// collectMatchingKeys walks a decoded JSON value and records leaf paths whose key
// (at any depth) contains one of the keywords (case-insensitive), with a short
// rendering of the value. Nested objects/arrays under a matching key are summarized.
func collectMatchingKeys(v any, path string, keywords []string, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			child := k
			if path != "" {
				child = path + "." + k
			}
			if keyMatches(k, keywords) {
				out[child] = renderValue(val)
			}
			collectMatchingKeys(val, child, keywords, out)
		}
	case []any:
		for i, val := range t {
			collectMatchingKeys(val, fmt.Sprintf("%s[%d]", path, i), keywords, out)
		}
	}
}

func keyMatches(key string, keywords []string) bool {
	lk := strings.ToLower(key)
	for _, kw := range keywords {
		if strings.Contains(lk, kw) {
			return true
		}
	}
	return false
}

func renderValue(v any) string {
	switch t := v.(type) {
	case map[string]any:
		return fmt.Sprintf("{object, %d keys}", len(t))
	case []any:
		return fmt.Sprintf("[array, %d items]", len(t))
	default:
		b, _ := json.Marshal(v)
		s := string(b)
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		return s
	}
}

// TestProbeOwnerAPIPrograms validates the owner-api VPP schedule fetch/parse in
// Go (PW_PROBE=1): it hits owner-api /programs with the app UA header, gunzips,
// parses the event windows, and prints the current/next event. Read-only.
//
// Env: PW_OWNERAPI_TOKEN (a legacy `ownerapi` access token — NOT the Fleet API
// token), PW_SITEID (optional; defaults to the known site).
func TestProbeOwnerAPIPrograms(t *testing.T) {
	if os.Getenv("PW_PROBE") != "1" {
		t.Skip("set PW_PROBE=1 to run the owner-api programs probe")
	}
	tok := os.Getenv("PW_OWNERAPI_TOKEN")
	require.NotEmpty(t, tok, "PW_OWNERAPI_TOKEN (legacy ownerapi access token)")
	siteID, _ := strconv.ParseInt(os.Getenv("PW_SITEID"), 10, 64)
	if siteID == 0 {
		siteID = 2252480583537271
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, TokenType: "Bearer"})
	c := newVPPClient(util.NewLogger("vpp-probe"), ts)

	events, err := c.events(siteID)
	require.NoError(t, err)
	t.Logf("parsed %d event(s)", len(events))
	for _, e := range events {
		t.Logf("  program=%q id=%s %s → %s optedIn=%v (active now=%v)",
			e.Program, e.ID, e.Start.Format("2006-01-02 15:04"), e.End.Format("15:04"), e.OptedIn, e.active(time.Now()))
	}
	if ev, ok := currentOrNextEvent(events, time.Now()); ok {
		t.Logf("current/next opted-in event: %s → %s (in %s)", ev.Start.Format("15:04"), ev.End.Format("15:04"), time.Until(ev.Start).Round(time.Minute))
	} else {
		t.Log("no current/next opted-in event")
	}
}

// TestProbeOwnerAPIRefresh validates the DURABLE path in Go (PW_PROBE=1): refresh
// an ownerapi token over a TLS-1.2-capped transport (owner-api rejects tokens
// minted over TLS 1.3) and confirm the refreshed token can read /programs. This is
// the check that unattended auto-poll is viable from evcc's Go runtime.
//
// Env: PW_OWNERAPI_REFRESH (the ownerapi refresh token), PW_SITEID (optional).
func TestProbeOwnerAPIRefresh(t *testing.T) {
	if os.Getenv("PW_PROBE") != "1" {
		t.Skip("set PW_PROBE=1 to run the owner-api TLS-1.2 refresh probe")
	}
	refresh := os.Getenv("PW_OWNERAPI_REFRESH")
	require.NotEmpty(t, refresh, "PW_OWNERAPI_REFRESH (ownerapi refresh token)")
	siteID, _ := strconv.ParseInt(os.Getenv("PW_SITEID"), 10, 64)
	if siteID == 0 {
		siteID = 2252480583537271
	}

	tok, err := ownerAPIRefresh(&oauth2.Token{RefreshToken: refresh})
	require.NoError(t, err, "TLS-1.2 refresh")
	t.Logf("refreshed over TLS1.2: expires %s", tok.Expiry.Format("15:04:05"))

	c := newVPPClient(util.NewLogger("vpp-probe"), oauth2.StaticTokenSource(tok))
	events, err := c.events(siteID)
	require.NoError(t, err)
	require.NotEmpty(t, events, "a TLS-1.2-minted token must be accepted by /programs")
	for _, e := range events {
		t.Logf("  %s %s→%s optedIn=%v", e.Program, e.Start.Format("2006-01-02 15:04"), e.End.Format("15:04"), e.OptedIn)
	}
}

// TestProbeVPPRates is the end-to-end live check (PW_PROBE=1): build a feed-in
// FleetAPI tariff with VPP auto-detection enabled (Fleet token for the base sell
// rates + ownerapi refresh token for the schedule) and confirm Rates() overlays
// the export premium onto slots inside the scheduled event window.
//
// Env: PW_CLIENTID, PW_TOKEN, PW_REFRESH, PW_EXPIRY, PW_SITEID (optional),
// PW_OWNERAPI_REFRESH.
func TestProbeVPPRates(t *testing.T) {
	if os.Getenv("PW_PROBE") != "1" {
		t.Skip("set PW_PROBE=1 to run the end-to-end VPP feed-in rates probe")
	}
	clientID := os.Getenv("PW_CLIENTID")
	siteID, _ := strconv.ParseInt(os.Getenv("PW_SITEID"), 10, 64)
	if siteID == 0 {
		siteID = 2252480583537271
	}
	expiry, err := time.Parse(time.RFC3339, os.Getenv("PW_EXPIRY"))
	require.NoError(t, err)
	token := &oauth2.Token{AccessToken: os.Getenv("PW_TOKEN"), RefreshToken: os.Getenv("PW_REFRESH"), TokenType: "Bearer", Expiry: expiry}
	require.NotEmpty(t, token.AccessToken, "PW_TOKEN")
	ownerRefresh := os.Getenv("PW_OWNERAPI_REFRESH")
	require.NotEmpty(t, ownerRefresh, "PW_OWNERAPI_REFRESH")

	// in-memory settings DB so the token source can persist the rotated token
	// (production evcc initializes this at startup)
	require.NoError(t, db.NewInstance("sqlite", ":memory:"))

	meter, err := NewFleetAPI("feedin", clientID, "", token, siteID, "", 30*time.Second,
		batterySocLimits{MinSoc: 10, MaxSoc: 100}, batteryPowerLimits{}, false)
	require.NoError(t, err)
	fa := meter.(*FleetAPI)
	fa.enableVPP(ownerRefresh, 2.0)

	// what event(s) does the poller see?
	events, err := fa.vppEventsG()
	require.NoError(t, err)
	require.NotEmpty(t, events, "expected a scheduled VPP event")
	ev := events[0]
	t.Logf("event: %s %s→%s", ev.Program, ev.Start.Format("2006-01-02 15:04"), ev.End.Format("15:04"))

	rates, err := fa.Rates()
	require.NoError(t, err)

	// print the feed-in rates around the event window (±1h) so the boundary is
	// visible, and assert: in-event slots carry the premium, out-of-event slots do
	// NOT (the split must stop the premium exactly at the event edges).
	from, to := ev.Start.Add(-time.Hour), ev.End.Add(time.Hour)
	var inEvent, inEventPremium int
	for _, r := range rates {
		if !r.End.After(from) || !r.Start.Before(to) {
			continue
		}
		mid := r.Start.Add(r.End.Sub(r.Start) / 2)
		within := !mid.Before(ev.Start) && mid.Before(ev.End)
		t.Logf("  slot %s-%s value=%.3f  in-event=%v", r.Start.Format("15:04"), r.End.Format("15:04"), r.Value, within)
		if within {
			inEvent++
			if r.Value >= 2.0 {
				inEventPremium++
			}
		} else {
			assert.Less(t, r.Value, 2.0, "out-of-event slot %s-%s must not carry the premium", r.Start.Format("15:04"), r.End.Format("15:04"))
		}
	}
	require.Positive(t, inEvent, "no feed-in slots inside the event window")
	assert.Equal(t, inEvent, inEventPremium, "every in-event slot should carry the $2 premium")
}

// recoverReal reconstructs the real tariff from a leftover sell-only synthetic
// spike by delegating the structural merge to the production unspikeTariff (each
// _SPIKE period folded back into its base period at the real rate), then labels
// it with this site's real utility/plan name for the probe log.
func recoverReal(t teslaclient.TariffContentV2) teslaclient.TariffContentV2 {
	out := *unspikeTariff(&t)
	out.Utility = "Clean Power SF"
	out.Name = "Residential Time-Of-Use - Electric Home - NEM 3.0 (2026)"
	return out
}

func envFloat(key string, def float64) float64 {
	if s := os.Getenv(key); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return def
}
