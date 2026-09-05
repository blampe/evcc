package meter

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/evcc-io/evcc/util"
	evcctesla "github.com/evcc-io/evcc/vehicle/tesla"
	teslaclient "github.com/evcc-io/tesla-proxy-client"
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
