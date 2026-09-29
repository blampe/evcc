package meter

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/api/implement"
	"github.com/evcc-io/evcc/core/cleanpowersf"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/tariff/fixed"
	"github.com/evcc-io/evcc/util"
	utilcache "github.com/evcc-io/evcc/util/cache"
	"github.com/evcc-io/evcc/util/oauth"
	"github.com/evcc-io/evcc/util/request"
	evcctesla "github.com/evcc-io/evcc/vehicle/tesla"
	teslaclient "github.com/evcc-io/tesla-proxy-client"
	"golang.org/x/oauth2"
)

// fleetAPIProvider holds the shared HTTP client and cached API functions
// for a given credentials + energy site combination.
type fleetAPIProvider struct {
	log          *util.Logger
	client       *teslaclient.Client
	site         *teslaclient.EnergySite
	energySiteID int64
	liveStatusG  func() (*teslaclient.EnergySiteLiveStatus, error)
	siteInfoG    func() (*teslaclient.EnergySite, error)

	// tariffPlanName is the Tesla catalog lookup key for the user's utility rate
	// plan (e.g. "CPSF-E-ELEC-NEM3-2026"). Optional; when set it lets restoreTariff
	// recover the fully-labeled plan from the catalog if evcc never snapshotted the
	// real tariff (cold start onto a stranded spike). The pack does not expose this
	// key, so it must be configured.
	tariffPlanName string

	// guard snapshots the real tariff while a synthetic export-forcing tariff is
	// active. It lives on the shared provider so the battery controller (which
	// writes the synthetic tariff) and the Tariff reader (which must keep
	// reporting real prices) see the same state.
	guard *tariffGuard

	// export realization injected by core (which alone can read the metered
	// true-up-year history it's derived from): the PG&E delivery EEC schedule and
	// the two NEM3 realization factors. They let the meter replace the Powerwall's
	// inflated ACC sell tariff with evcc's realized export value (correctedTariff).
	// Guarded because the control loop injects while tariff writes read.
	exportMu             sync.Mutex
	exportEEC            api.Rates
	exportRDelivery      float64
	exportRGen           float64
	exportRealizationSet bool
	lastMaintain         time.Time // last proactive corrected-baseline write (rate-limits MaintainExportTariff)
}

// FleetAPI is a Tesla Fleet API meter for Powerwall 3.
// Multiple FleetAPI instances for the same site share a single provider.
type FleetAPI struct {
	implement.Caps
	usage           string
	provider        *fleetAPIProvider
	socLimits       batterySocLimits
	allowGridImport bool // permit charging the pack from the grid (default off)

	// VPP (grid-services) event awareness, only on a feed-in tariff meter: when a
	// scheduled event overlaps a feed-in slot, Rates() raises that slot to the
	// export premium so the optimizer pre-charges for and plans discharge into it.
	// nil unless an owner-api token is configured (auto-detection is opt-in).
	vppEventsG func() ([]vppEvent, error)
	vppPremium float64
}

var (
	_ api.BatteryModeController = (*FleetAPI)(nil)
	_ api.GridChargeController  = (*FleetAPI)(nil)
)

// AllowGridImport reports whether the Powerwall may charge from the grid.
// It implements api.GridChargeController so the optimizer can model the pack
// with charge_from_grid disabled when off.
func (m *FleetAPI) AllowGridImport() bool {
	return m.allowGridImport
}

func init() {
	registry.Add("teslafleetapi", NewFleetAPIFromConfig)
}

// NewFleetAPIFromConfig creates a Tesla Fleet API Meter from generic config.
func NewFleetAPIFromConfig(other map[string]any) (api.Meter, error) {
	cc := struct {
		Usage              string
		Credentials        oauth.ClientCredentials
		Tokens             oauth.Tokens
		EnergySiteID       int64  `mapstructure:"siteid"`
		Tariff             string `mapstructure:"tariff"` // Tesla catalog plan name for labeled restore-by-name (optional)
		Cache              time.Duration
		AllowGridImport    bool    // permit charging the pack from the grid (default off)
		OwnerAPIToken      string  `mapstructure:"ownerapitoken"` // legacy ownerapi REFRESH token, enables VPP event auto-detection on a feed-in tariff (optional)
		VPPPremium         float64 `mapstructure:"vpppremium"`    // export payout $/kWh applied to feed-in slots during a VPP event (default $2)
		batterySocLimits   `mapstructure:",squash"`
		batteryPowerLimits `mapstructure:",squash"`
	}{
		batterySocLimits: batterySocLimits{
			MinSoc: 20,
			MaxSoc: 100,
		},
		batteryPowerLimits: batteryPowerLimits{
			MaxChargePower:    5000,
			MaxDischargePower: 11500,
		},
		Cache: 30 * time.Second,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	if cc.Usage == "" {
		return nil, errors.New("missing usage")
	}

	if cc.Credentials.ID == "" {
		return nil, errors.New("missing client id, see https://docs.evcc.io/en/docs/devices/vehicles#tesla")
	}

	token, err := cc.Tokens.Token()
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(cc.Usage) {
	case "grid":
		cc.Usage = "site"
	case "pv":
		cc.Usage = "solar"
	}

	m, err := NewFleetAPI(cc.Usage, cc.Credentials.ID, cc.Credentials.Secret, token, cc.EnergySiteID, cc.Tariff, cc.Cache, cc.batterySocLimits, cc.batteryPowerLimits, cc.AllowGridImport)
	if err != nil {
		return nil, err
	}

	// opt-in VPP event auto-detection on a feed-in tariff: with an owner-api refresh
	// token configured, poll the grid-services schedule and reflect the export
	// premium in the feed-in rates during events.
	if fa, ok := m.(*FleetAPI); ok && cc.OwnerAPIToken != "" && (fa.usage == "feedin" || fa.usage == "solar") {
		fa.enableVPP(cc.OwnerAPIToken, cc.VPPPremium)
	}
	return m, nil
}

var fleetAPIProviders = utilcache.New[*fleetAPIProvider]()

func getOrCreateProvider(clientID, clientSecret string, token *oauth2.Token, energySiteID int64, tariffPlanName string, cacheDur time.Duration) (*fleetAPIProvider, error) {
	log := util.NewLogger("fleetapi").Redact(token.AccessToken, token.RefreshToken, clientID, clientSecret)

	tc, err := newFleetClient(log, evcctesla.OAuth2Config(clientID, clientSecret), token)
	if err != nil {
		return nil, err
	}

	if energySiteID == 0 {
		products, err := tc.Products()
		if err != nil {
			return nil, fmt.Errorf("failed to discover energy site ID: %w", err)
		}
		for _, product := range products {
			if product.EnergySiteId != 0 {
				energySiteID = product.EnergySiteId
				log.DEBUG.Printf("auto-discovered energy site ID: %d", energySiteID)
				break
			}
		}
		if energySiteID == 0 {
			return nil, errors.New("no energy site found in products")
		}
	}

	key := clientID + ":" + strconv.FormatInt(energySiteID, 10)
	return fleetAPIProviders.GetOrCreate(key, func() (*fleetAPIProvider, error) {
		log.Redact(strconv.FormatInt(energySiteID, 10))

		site, err := tc.EnergySite(energySiteID)
		if err != nil {
			return nil, fmt.Errorf("failed to get energy site: %w", err)
		}

		p := &fleetAPIProvider{
			log:            log,
			client:         tc,
			site:           site,
			energySiteID:   energySiteID,
			tariffPlanName: tariffPlanName,
			guard:          newTariffGuard(fmt.Sprintf("tesla.%d.tariff", energySiteID)),
		}

		p.liveStatusG = util.Cached(p.getLiveStatus, cacheDur)
		p.siteInfoG = util.Cached(func() (*teslaclient.EnergySite, error) {
			return p.client.EnergySite(p.energySiteID)
		}, siteInfoCacheTTL)

		if _, err := p.siteInfoG(); err != nil {
			return nil, fmt.Errorf("failed to connect to Tesla Fleet API: %w", err)
		}

		return p, nil
	})
}

// newFleetClient creates a region-configured *teslaclient.Client for the given OAuth2 credentials.
func newFleetClient(log *util.Logger, oc *oauth2.Config, token *oauth2.Token) (*teslaclient.Client, error) {
	identity, err := evcctesla.NewIdentity(log, oc, token)
	if err != nil {
		return nil, err
	}

	hc := request.NewClient(log)
	hc.Transport = &oauth2.Transport{
		Source: identity,
		Base:   hc.Transport,
	}

	tc, err := teslaclient.NewClient(context.Background(), teslaclient.WithClient(hc))
	if err != nil {
		return nil, err
	}

	region, err := tc.UserRegion()
	if err != nil {
		return nil, err
	}
	tc.SetBaseUrl(region.FleetApiBaseUrl)

	return tc, nil
}

// NewFleetAPI creates a Tesla Fleet API Meter for Powerwall 3.
func NewFleetAPI(usage, clientID, clientSecret string, token *oauth2.Token, energySiteID int64, tariffPlanName string, cacheDur time.Duration, socLimits batterySocLimits, powerLimits batteryPowerLimits, allowGridImport bool) (api.Meter, error) {
	provider, err := getOrCreateProvider(clientID, clientSecret, token, energySiteID, tariffPlanName, cacheDur)
	if err != nil {
		return nil, err
	}

	m := &FleetAPI{
		Caps:            implement.New(),
		usage:           strings.ToLower(usage),
		provider:        provider,
		socLimits:       socLimits,
		allowGridImport: allowGridImport,
	}

	if usage == "battery" {
		implement.Has(m, implement.Battery(m.batterySoc))
		implement.May(m, implement.BatterySocLimiter(socLimits.Decorator()))
		implement.May(m, implement.BatteryPowerLimiter(powerLimits.Decorator()))
		implement.Has(m, implement.BatteryController(m.newBatteryController()))

		// total_pack_energy is no longer returned by the API so use nominal value.
		// https://github.com/teslamotors/vehicle-command/issues/215
		implement.Has(m, implement.BatteryCapacity(func() float64 {
			return 13.5
		}))
	}

	return m, nil
}

func (p *fleetAPIProvider) getLiveStatus() (*teslaclient.EnergySiteLiveStatus, error) {
	res, err := p.site.EnergySiteLiveStatus()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("%w: %w", api.ErrMustRetry, err)
		}
		if se, ok := errors.AsType[*request.StatusError](err); ok && se.HasStatus(http.StatusInternalServerError, http.StatusGatewayTimeout, http.StatusFailedDependency) {
			return nil, fmt.Errorf("%w: %w", api.ErrMustRetry, err)
		}
	} else {
		p.log.DEBUG.Printf("live status: grid=%.0fW solar=%.0fW battery=%.0fW load=%.0fW soc=%.1f%%",
			res.GridPower, res.SolarPower, res.BatteryPower, res.LoadPower, res.PercentageCharged)
	}
	return res, err
}

var _ api.Meter = (*FleetAPI)(nil)

// CurrentPower implements the api.Meter interface.
func (m *FleetAPI) CurrentPower() (float64, error) {
	res, err := m.provider.liveStatusG()
	if err != nil {
		return 0, err
	}

	switch m.usage {
	case "site", "grid":
		return res.GridPower, nil
	case "solar", "pv":
		return res.SolarPower, nil
	case "battery":
		return res.BatteryPower, nil
	case "load":
		return res.LoadPower, nil
	}

	return 0, fmt.Errorf("invalid usage: %s", m.usage)
}

func (m *FleetAPI) batterySoc() (float64, error) {
	res, err := m.provider.liveStatusG()
	if err != nil {
		return 0, err
	}
	return res.PercentageCharged, nil
}

const (
	// syntheticTariffUtility marks the export-forcing tariff evcc writes (the
	// spike). It lets us tell our tariff apart from the user's real one so we never
	// snapshot or restore a synthetic tariff as if it were real.
	syntheticTariffUtility = "EVCC"

	// spikeSuffix names the extra TOU period the spike carves out of a base period
	// (see spikeSeasons). It also lets us reverse a spike back into the real tariff
	// (see unspikeSeasons) when no snapshot is available to restore from.
	spikeSuffix = "_SPIKE"

	// dischargeSpikeBump is added to the real SELL price only (not buy) within the
	// spike window to force export. Live probing showed bumping buy too is
	// counterproductive: an equal buy+sell bump preserves the real spread and
	// raises the value of stored energy (future import looks expensive), so the
	// Powerwall hoards and keeps charging. Sell-only makes sell >> buy, a strong
	// export signal. $10 is the empirically-confirmed level that flips a charging
	// pack to discharge within ~2 min (the pickup delay); $1 was too weak.
	dischargeSpikeBump = 10.0

	// spikeSlotDuration is the optimizer's slot granularity; the spike covers the
	// current slot plus the next one (aligned to this 15-min grid) and is re-written
	// as the slot advances (see updateBatteryMode) so it tracks the plan, never goes
	// stale, and always has runway ahead for the TOU algorithm to react.
	spikeSlotDuration = 15 * time.Minute
)

// isSyntheticTariff reports whether a tariff was written by evcc.
func isSyntheticTariff(t *teslaclient.TariffContentV2) bool {
	return t != nil && t.Utility == syntheticTariffUtility
}

// tariffGuard remembers the user's real Powerwall tariff so evcc can restore it
// after overwriting it with a synthetic export-forcing tariff. The snapshot is
// persisted (keyed by settingsKey) so it survives an evcc restart taken while a
// synthetic tariff is active.
//
// Two invariants make it robust:
//   - It never records a synthetic tariff as the real one — otherwise a restart
//     mid-discharge could make evcc "restore" the synthetic tariff permanently.
//   - It is never deleted, only refreshed. remember() updates the snapshot to the
//     latest real tariff whenever one is observed (so a utility plan change is
//     tracked automatically — the annual rate schedule itself never goes stale),
//     and persists immediately so a crash right after entering discharge can't
//     strand the pack with a snapshot that only lived in memory.
type tariffGuard struct {
	mu          sync.Mutex
	settingsKey string
	saved       *teslaclient.TariffContentV2
}

func newTariffGuard(settingsKey string) *tariffGuard {
	g := &tariffGuard{settingsKey: settingsKey}
	g.load()
	return g
}

// load restores a persisted real tariff, ignoring a synthetic one (the reboot
// safeguard: a synthetic tariff read from disk must not be treated as real).
func (g *tariffGuard) load() {
	if g.settingsKey == "" {
		return
	}
	var t teslaclient.TariffContentV2
	if err := settings.Json(g.settingsKey, &t); err == nil && !isSyntheticTariff(&t) {
		g.saved = &t
	}
}

// remember refreshes the saved real tariff to current, so the snapshot always
// tracks the latest real plan (a utility switch or annual rate revision is picked
// up the next time the pack shows a real tariff). It ignores a nil or synthetic
// current (never recording our own export-forcing tariff as real), and no-ops
// when current is unchanged so an unchanged read costs no persist. On a real
// change it persists immediately so a crash can't strand the snapshot in memory.
func (g *tariffGuard) remember(current *teslaclient.TariffContentV2) {
	if current == nil || isSyntheticTariff(current) {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.saved != nil && reflect.DeepEqual(g.saved, current) {
		return
	}
	g.saved = current
	if g.settingsKey != "" {
		if err := settings.SetJson(g.settingsKey, current); err == nil {
			_ = settings.Persist()
		}
	}
}

// peek returns the saved real tariff without clearing it, or nil. The snapshot is
// never dropped — restore rewrites it but keeps it, so evcc can restore again any
// time (the schedule stays valid indefinitely; remember refreshes it on change).
func (g *tariffGuard) peek() *teslaclient.TariffContentV2 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.saved
}

// snapshotTariff records the user's real tariff before a synthetic export-forcing
// tariff overwrites it. It reads FRESH from the Fleet API (bypassing the cache) —
// the snapshot is what we later restore, so a stale read must not be trusted, and
// re-reading is how we recover the real tariff after it's been restored upstream.
// No-op once a real tariff is saved; remember() ignores a synthetic live tariff.
func (m *FleetAPI) snapshotTariff() {
	if m.provider.guard == nil || m.provider.guard.peek() != nil {
		return
	}
	site, err := m.provider.client.EnergySite(m.provider.energySiteID)
	if err != nil {
		m.provider.log.WARN.Printf("snapshot tariff: %v", err)
		return
	}
	m.provider.guard.remember(site.TariffContentV2)
}

// restoreTariff writes the user's real tariff back after a synthetic one was
// applied. With a snapshot in hand it rewrites it and KEEPS it (never deleted, so
// evcc can always restore again — the annual schedule stays valid indefinitely).
//
// With no snapshot — evcc crashed before the snapshot persisted, or started up
// onto a pack already carrying a stranded spike — it reads the live tariff and
// self-heals: if the live tariff is synthetic it reconstructs the real one and
// writes that back, rather than masking the spike forever (effectiveTariff only
// hides it from our own reporting). Reconstruction prefers the fully-labeled plan
// from the catalog when a plan name is configured (tariffPlanName), and otherwise
// falls back to unspikeTariff (rates-exact, labels blank).
func (m *FleetAPI) restoreTariff() error {
	if m.provider.guard == nil {
		return nil
	}
	if t := m.provider.guard.peek(); t != nil {
		return m.writeBaselineTariff("restore real buy + realized export sell", t)
	}

	site, err := m.provider.client.EnergySite(m.provider.energySiteID)
	if err != nil {
		return retryable("restore tariff: read site", err)
	}
	if !isSyntheticTariff(site.TariffContentV2) {
		// already on the user's real tariff; capture it, then overlay our realized
		// export sell so the app shows it instead of the raw ACC (no-op sell change
		// until core injects the realization inputs).
		return m.ensureCorrectedBaseline("realized export sell overlay", site.TariffContentV2)
	}

	// no snapshot and stranded on a synthetic tariff: reconstruct the real one.
	// Prefer the fully-labeled plan from the catalog (looked up by configured name).
	// This cold-start self-heal restores the genuine (v1) plan uncorrected; the next
	// normal restore re-applies the corrected v2 baseline once a snapshot exists.
	if name := m.provider.tariffPlanName; name != "" {
		if plan, err := m.provider.client.RateTariff(name); err == nil && plan != nil {
			return m.apiWrite("tariff → restore by name (catalog)", func() error { return m.provider.site.SetTariffContent(*plan) })
		} else if err != nil {
			m.provider.log.WARN.Printf("restore tariff by name %q failed, reconstructing from spike: %v", name, err)
		}
	}

	return m.writeBaselineTariff("unspike stranded synthetic + realized export sell", unspikeTariff(site.TariffContentV2))
}

// writeBaselineTariff writes the non-spiked baseline tariff to the Powerwall: the
// real BUY side untouched and the SELL side replaced by evcc's realized export
// value (correctedTariff). Until core injects the realization inputs it writes the
// real tariff unchanged, so a cold start never strands the pack.
func (m *FleetAPI) writeBaselineTariff(what string, real *teslaclient.TariffContentV2) error {
	if real == nil {
		return nil
	}
	return m.apiWrite("raw tariff → "+what, func() error { return m.provider.site.SetRawTariff(*m.correctedTariff(real)) })
}

// ensureCorrectedBaseline captures the genuine tariff (so the guard can restore it)
// then overlays the corrected baseline (real buy + realized export sell). It's the
// shared "we're on a genuine tariff → snapshot and overlay" step used by both the
// mode-change restore and the proactive maintainer (MaintainExportTariff).
func (m *FleetAPI) ensureCorrectedBaseline(what string, genuine *teslaclient.TariffContentV2) error {
	if m.provider.guard != nil {
		m.provider.guard.remember(genuine) // capture the genuine before overlaying
	}
	return m.writeBaselineTariff(what, genuine)
}

// setDischargeTariff writes the sell-only export spike built from the user's real
// tariff. If no real tariff is saved it re-reads it fresh from the Fleet API
// (snapshotTariff). If none can be captured — e.g. the Powerwall is currently on
// a synthetic tariff — it leaves the tariff UNCHANGED. It never writes a flat
// synthetic tariff: that would clobber the real tariff with something we can't
// recover from (the guard skips synthetic, so we'd be stuck writing it forever).
func (m *FleetAPI) setDischargeTariff() error {
	if m.provider.guard == nil {
		return nil
	}
	real := m.provider.guard.peek()
	if real == nil {
		m.snapshotTariff() // re-read fresh from the Fleet API
		real = m.provider.guard.peek()
	}
	if real == nil {
		m.provider.log.WARN.Println("discharge tariff: no real tariff captured (Powerwall may be on a synthetic tariff); leaving it unchanged — restore the utility tariff upstream")
		return nil
	}

	now := time.Now()
	// spike the realized-export baseline (real buy + realized sell), not the raw ACC
	// tariff, so leaving the spike restores the corrected sell and the app never
	// falls back to ACC.
	base := m.correctedTariff(real)
	t := spikeTariff(base, now)

	// probe: confirm the +bump lands on sell (buy stays real) at the current hour
	buyBefore, _ := rateAtV2(base.Seasons, base.EnergyCharges, now)
	buyAfter, _ := rateAtV2(t.Seasons, t.EnergyCharges, now)
	var sellBefore, sellAfter float64
	if base.SellTariff != nil {
		sellBefore, _ = rateAtV2(base.SellTariff.Seasons, base.SellTariff.EnergyCharges, now)
	}
	if t.SellTariff != nil {
		sellAfter, _ = rateAtV2(t.SellTariff.Seasons, t.SellTariff.EnergyCharges, now)
	}
	m.provider.log.DEBUG.Printf("discharge tariff (spike +%.2g): utility=%q buy %.4g->%.4g sell %.4g->%.4g",
		dischargeSpikeBump, t.Utility, buyBefore, buyAfter, sellBefore, sellAfter)

	return m.apiWrite("raw tariff → export spike", func() error { return m.provider.site.SetRawTariff(t) })
}

// rateAtV2 resolves the tariff rate covering time t from a v2 season/charge map.
func rateAtV2(seasons map[string]teslaclient.TariffSeasonV2, charges map[string]teslaclient.TariffRates, t time.Time) (float64, bool) {
	rates, err := ratesFromV2(seasons, charges)
	if err != nil {
		return 0, false
	}
	for _, r := range rates {
		if !t.Before(r.Start) && t.Before(r.End) {
			return r.Value, true
		}
	}
	return 0, false
}

// effectiveTariff returns the tariff evcc should report to the rest of the
// system. While we force a synthetic export tariff on the Powerwall (to drive
// discharge), the live tariff no longer reflects real prices — reporting it
// would feed our own bumped prices back into the optimizer and UI. So whenever
// the live tariff is synthetic we report the real one instead: the exact
// snapshot if we still hold it, otherwise the real rates reconstructed from the
// spike (unspikeTariff). We must never leak the raw spike — that happens
// routinely in the window after we restore the real tariff but before the cached
// site-info refreshes, and for a leftover spike left on the Powerwall by a
// crashed session.
//
// When the live tariff is real this is also where we keep the guard snapshot
// fresh: every real read refreshes it (remember no-ops if unchanged), so a plan
// change is captured automatically and we always hold the latest real tariff to
// restore — without any extra Fleet API calls.
func (m *FleetAPI) effectiveTariff(site *teslaclient.EnergySite) *teslaclient.TariffContentV2 {
	t := site.TariffContentV2
	if !isSyntheticTariff(t) {
		if m.provider.guard != nil {
			m.provider.guard.remember(t)
		}
		return t
	}
	if m.provider.guard != nil {
		if real := m.provider.guard.peek(); real != nil {
			return real
		}
	}
	return unspikeTariff(t)
}

// correctedTariffName labels the baseline tariff evcc writes (real buy + realized
// export sell). Cosmetic; the EVCC utility marker is what the guard keys on.
const correctedTariffName = "Custom (evcc realized export)"

// cpsfLocation is CleanPowerSF's tariff timezone; representative TOU-period times
// are built here so the CleanPowerSF hour-of-day lands correctly across DST.
var cpsfLocation, _ = time.LoadLocation("America/Los_Angeles")

// monthByName maps Tesla's month-named tariff seasons (the sell tariff uses one
// season per month) to a month number, since those seasons carry no from/to month.
var monthByName = map[string]time.Month{
	"january": time.January, "february": time.February, "march": time.March,
	"april": time.April, "may": time.May, "june": time.June, "july": time.July,
	"august": time.August, "september": time.September, "october": time.October,
	"november": time.November, "december": time.December,
}

// siteInfoCacheTTL is how long site info (including the live tariff) is cached.
const siteInfoCacheTTL = 15 * time.Minute

// maintainExportInterval rate-limits the proactive baseline write to at most once
// per site-info cache period: siteInfoG is cached for siteInfoCacheTTL, so a
// just-written tariff still reads back stale for that long and we must not re-write
// it every control cycle. Derived from the cache TTL so the two can't diverge.
const maintainExportInterval = siteInfoCacheTTL

// MaintainExportTariff proactively ensures the Powerwall carries the corrected
// baseline (real buy + realized export sell) so the app shows evcc's export value
// without waiting for a battery-mode change to trigger restoreTariff. It no-ops
// while the pack already carries a synthetic tariff (our corrected baseline or an
// active discharge spike - both tagged EVCC), until core injects the realization
// inputs, and within maintainExportInterval of the last successful write.
func (m *FleetAPI) MaintainExportTariff() error {
	p := m.provider
	if p.siteInfoG == nil {
		return nil
	}
	p.exportMu.Lock()
	set := p.exportRealizationSet
	recent := !p.lastMaintain.IsZero() && time.Since(p.lastMaintain) < maintainExportInterval
	p.exportMu.Unlock()
	if !set || recent {
		return nil
	}
	site, err := p.siteInfoG()
	if err != nil {
		return nil // best-effort; a read failure must not disrupt control
	}
	if isSyntheticTariff(site.TariffContentV2) {
		return nil // already ours (corrected baseline or active spike) - leave it
	}
	if err := m.ensureCorrectedBaseline("maintain realized export sell", site.TariffContentV2); err != nil {
		return err
	}
	p.exportMu.Lock()
	p.lastMaintain = time.Now()
	p.exportMu.Unlock()
	return nil
}

// SetExportRealization injects the export-realization inputs computed by core
// (which alone can read the metered true-up-year history behind them): the PG&E
// delivery EEC schedule and the delivery/generation realization factors. They let
// the meter build the realized export sell tariff written back to the Powerwall
// (correctedTariff). Safe to call every control cycle.
func (m *FleetAPI) SetExportRealization(eec api.Rates, rDelivery, rGen float64) {
	p := m.provider
	p.exportMu.Lock()
	defer p.exportMu.Unlock()
	p.exportEEC = eec
	p.exportRDelivery = rDelivery
	p.exportRGen = rGen
	p.exportRealizationSet = true
}

// realizedSellRate returns the realized NEM3 export value at t - the PG&E delivery
// EEC scaled by R_delivery, plus CleanPowerSF generation floored at Net Surplus
// Compensation and climbing to the retail rate by R_gen. It hand-mirrors core's
// realizedExportAt (KEEP IN SYNC) - the formula can't be shared because meter can't
// import core (import cycle). ok is false until core injects the realization inputs,
// so the meter never writes a half-built sell tariff.
func (m *FleetAPI) realizedSellRate(t time.Time) (float64, bool) {
	p := m.provider
	p.exportMu.Lock()
	eec, rDel, rGen, set := p.exportEEC, p.exportRDelivery, p.exportRGen, p.exportRealizationSet
	p.exportMu.Unlock()
	if !set {
		return 0, false
	}
	genRetail := cleanpowersf.GenerationRate(t)
	nsc := cleanpowersf.NetSurplusCompensation(t)
	realizedGen := nsc + (genRetail-nsc)*rGen
	return rateAtSorted(eec, t)*rDel + realizedGen, true
}

// realizedBuyRate discounts a retail import price to its realized marginal cost at
// t: the CleanPowerSF generation premium a net exporter never pays (retail gen above
// NSC) is removed, scaled by how far the position is from net-generation exporter
// (rGen). Mirrors core's realizedImportAt (KEEP IN SYNC). ok is false until core
// injects the realization inputs.
func (m *FleetAPI) realizedBuyRate(t time.Time, retail float64) (float64, bool) {
	p := m.provider
	p.exportMu.Lock()
	rGen, set := p.exportRGen, p.exportRealizationSet
	p.exportMu.Unlock()
	if !set {
		return 0, false
	}
	genRetail := cleanpowersf.GenerationRate(t)
	nsc := cleanpowersf.NetSurplusCompensation(t)
	return retail - max(0, genRetail-nsc)*(1-rGen), true
}

// rateAtSorted returns the value of the start-sorted rate slot covering t, or 0.
func rateAtSorted(rates api.Rates, t time.Time) float64 {
	i := sort.Search(len(rates), func(i int) bool { return rates[i].End.After(t) })
	if i < len(rates) && !t.Before(rates[i].Start) && t.Before(rates[i].End) {
		return rates[i].Value
	}
	return 0
}

// correctedTariff returns the user's real tariff with both sides replaced by their
// realized NEM3 marginal values - the buy discounted to the marginal import cost
// (realizedBuyRate) and the sell to the realized export value (realizedSellRate),
// the same basis evcc optimizes on - so the Powerwall (its app and its own onboard
// optimizer) stops using Tesla's inflated ACC sell and the sticker retail buy, which
// for a net-metered customer are not the marginal prices that move the annual-netted
// bill. Tagged EVCC so the guard keeps snapshotting and restoring the user's genuine
// tariff and never mistakes this for real. Returns the input unchanged until core
// injects the realization inputs (never write a half-built tariff) or when there is
// no sell tariff to correct.
//
// It shares the EVCC marker with the (reversible) discharge spike even though the
// overwrite is lossy, which is safe: this builder always RECOMPUTES both sides from
// the injected realization (it never trusts an input's rates), so a stranded
// corrected tariff fed back through the self-heal path is re-derived correctly; and
// the guard refuses to snapshot any EVCC-tagged tariff as real, so realized rates
// can't be mistaken for the utility's genuine ones. The genuine tariff is captured
// (guard.remember) before it's ever overwritten. evcc's own grid/feed-in reads go
// through effectiveTariff, which returns the genuine snapshot while ours is live -
// so evcc never double-realizes.
func (m *FleetAPI) correctedTariff(genuine *teslaclient.TariffContentV2) *teslaclient.TariffContentV2 {
	if genuine == nil || genuine.SellTariff == nil {
		return genuine
	}
	p := m.provider
	p.exportMu.Lock()
	set := p.exportRealizationSet
	p.exportMu.Unlock()
	if !set {
		return genuine
	}

	out := *genuine
	out.Utility = syntheticTariffUtility
	out.Name = correctedTariffName
	out.Version = 2
	// buy = realized marginal import cost; sell = realized export value (same basis)
	out.Seasons, out.EnergyCharges = m.realizedSeasons(genuine.Seasons, genuine.EnergyCharges, m.realizedBuyRate)
	sell := *genuine.SellTariff
	sell.Seasons, sell.EnergyCharges = m.realizedSeasons(sell.Seasons, sell.EnergyCharges,
		func(t time.Time, _ float64) (float64, bool) { return m.realizedSellRate(t) })
	out.SellTariff = &sell
	return &out
}

// realizedSeasons returns copies of a tariff's seasons (structure kept as-is) and
// energy charges with each rate replaced by rate(representative-time, current-rate),
// sampled at a time representative of that season/period (season month, period hour
// and weekday). A season whose month can't be resolved, or a period whose rate()
// declines, keeps its original value - worst case leaves the genuine rate rather
// than a wrong one. Inputs are never mutated. Shared by the buy and sell corrections.
func (m *FleetAPI) realizedSeasons(seasons map[string]teslaclient.TariffSeasonV2, charges map[string]teslaclient.TariffRates, rate func(t time.Time, retail float64) (float64, bool)) (map[string]teslaclient.TariffSeasonV2, map[string]teslaclient.TariffRates) {
	outSeasons := make(map[string]teslaclient.TariffSeasonV2, len(seasons))
	maps.Copy(outSeasons, seasons)
	outCharges := make(map[string]teslaclient.TariffRates, len(charges))

	for name, season := range seasons {
		newRates := make(map[string]float64, len(charges[name].Rates))
		maps.Copy(newRates, charges[name].Rates) // default: keep original (month unresolved / rate declined)

		if month, ok := seasonMonth(name, season); ok {
			for periodName, tou := range season.TOUPeriods {
				retail, has := charges[name].Rates[periodName]
				if !has || len(tou.Periods) == 0 {
					continue
				}
				p0 := tou.Periods[0]
				if v, ok := rate(representativeTime(month, p0.FromDayOfWeek, p0.FromHour), retail); ok {
					newRates[periodName] = v
				}
			}
		}
		outCharges[name] = teslaclient.TariffRates{Rates: newRates}
	}
	return outSeasons, outCharges
}

// seasonMonth resolves a sell-tariff season to a representative month: its
// FromMonth when set, else its month name. Tesla's month-named sell seasons often
// arrive with FromMonth=0, so the name is the fallback, not the primary path.
func seasonMonth(name string, season teslaclient.TariffSeasonV2) (time.Month, bool) {
	if season.FromMonth >= 1 && season.FromMonth <= 12 {
		return time.Month(season.FromMonth), true
	}
	if mth, ok := monthByName[strings.ToLower(strings.TrimSpace(name))]; ok {
		return mth, true
	}
	return 0, false
}

// representativeTime returns a concrete instant for sampling a repeating TOU period:
// the given hour on a mid-month day whose weekday matches the Tesla day-of-week code
// (0=Mon..6=Sun), in the next occurrence of the month, in CleanPowerSF's timezone.
func representativeTime(month time.Month, teslaDow, hour int) time.Time {
	now := time.Now().In(cpsfLocation)
	year := now.Year()
	if month < now.Month() {
		year++ // month already passed this year -> use next year's occurrence
	}
	want := time.Weekday((teslaDow + 1) % 7) // Tesla 0=Mon..6=Sun -> Go 0=Sun..6=Sat
	day := time.Date(year, month, 15, hour, 0, 0, 0, cpsfLocation)
	return day.AddDate(0, 0, (int(want)-int(day.Weekday())+7)%7) // nearest matching weekday on/after the 15th
}

// retryable marks a mutating Fleet API failure as api.ErrMustRetry so the state
// change isn't silently dropped: updateBatteryMode only records the mode as
// applied on success, so on error it re-applies the whole (idempotent,
// snapshot-guarded) sequence on its next cycle. Letting the control loop own the
// retry keeps it non-blocking, unlike an in-call backoff.
func retryable(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w: %w", op, api.ErrMustRetry, err)
}

// apiWrite logs a Tesla Fleet API write (op should name the endpoint and its value,
// e.g. "operating mode → autonomous") at DEBUG before issuing it, then wraps any
// failure in api.ErrMustRetry so the control loop retries the whole mode sequence.
func (m *FleetAPI) apiWrite(op string, fn func() error) error {
	m.provider.log.DEBUG.Printf("tesla api: %s", op)
	return retryable(op, fn())
}

// clampReserve snaps a backup reserve to a value the Powerwall 3 accepts: it
// rejects reserves in the open interval (80,100), so anything in that band is
// lowered to 80 (100 itself stays 100). Also clamps to [0,100].
func clampReserve(pct float64) uint64 {
	pct = min(100, max(0, pct))
	if pct > 80 && pct < 100 {
		pct = 80
	}
	return uint64(pct)
}

// onOff renders a boolean as an on/off label for API operation logs.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// normalReserve is the self_consumption reserve: MinSoc normally, or the battery-
// boost target SoC (never below MinSoc) when the optimizer is draining the pack
// into a car and passes targetSoc > 0, so the discharge stops at the plan's
// target instead of at MinSoc.
func normalReserve(minSoc, targetSoc float64) uint64 {
	if targetSoc > 0 {
		return clampReserve(max(targetSoc, minSoc))
	}
	return clampReserve(minSoc)
}

// newBatteryController returns a BatteryController func that applies the mode
// with no optimizer SoC target (the static min/current-soc reserve defaults).
func (m *FleetAPI) newBatteryController() func(api.BatteryMode) error {
	return func(mode api.BatteryMode) error {
		return m.SetBatteryModeSoc(mode, 0)
	}
}

// dischargeReserve is the backup reserve (percent) for a grid discharge: the
// optimizer's target SoC, so the pack stops there instead of draining to min
// soc during the ~2min pickup lag, but never below min soc. A target <= 0 falls
// back to min soc (the previous, target-unaware behavior).
func dischargeReserve(targetSoc, minSoc float64) float64 {
	if targetSoc <= 0 {
		return minSoc
	}
	return max(targetSoc, minSoc)
}

// vppReserve is the reserve to configure as the VPP (grid-services) event floor:
// the optimizer's discharge floor (its target SoC, floored at MinSoc), so a
// Tesla-auto-dispatched event discharges only as deep as the optimizer intends.
// It reuses the grid-discharge reserve so the event floor tracks the plan exactly.
func vppReserve(targetSoc, minSoc float64) uint64 {
	return clampReserve(dischargeReserve(targetSoc, minSoc))
}

// syncVPPReserve best-effort writes the VPP event reserve to the optimizer's
// discharge floor. Failures are logged but not returned: a bad VPP-reserve write
// must not abort the actual battery-mode change (Tesla may reject the write while
// it owns the reserve during an active event).
func (m *FleetAPI) syncVPPReserve(targetSoc float64) {
	reserve := vppReserve(targetSoc, m.socLimits.MinSoc)
	m.provider.log.DEBUG.Printf("tesla api: vpp backup reserve → %d%% (optimizer floor, target %.0f%%)", reserve, targetSoc)
	if err := m.provider.site.SetVPPBackupReserve(reserve); err != nil {
		m.provider.log.WARN.Printf("vpp backup reserve → %d%% failed (non-fatal): %v", reserve, err)
	}
}

// holdChargeReserveMargin is how far (percent) below the current SoC the
// hold-charge reserve sits, so the pack can still cover the home through a brief
// PV dip (self-consumption) instead of forcing a grid import, while staying high
// enough that it neither grid-charges nor dumps to grid.
const holdChargeReserveMargin = 5

// holdChargeReserve is the backup reserve for hold-charge below 100%: a small
// margin under the current SoC so a PV dip is covered from the battery rather
// than the grid, never below min soc.
func holdChargeReserve(soc, minSoc float64) float64 {
	return max(soc-holdChargeReserveMargin, minSoc)
}

// holdOperatingMode is the Tesla operating mode BatteryHold applies.
//
// self_consumption, deliberately NOT backup. Backup-only is not an idle mode: it
// charges the pack for standby and takes priority over the house, so the Powerwall
// absorbs the entire PV output while the house and car draw their whole load from the
// grid. self_consumption serves the house from solar first and banks only the
// surplus. The reserve, not the operating mode, is what withholds discharge.
const holdOperatingMode = "self_consumption"

// holdReserve is the reserve BatteryHold pins: the current SoC, never below min soc.
// Sitting the reserve at where the pack already is blocks any further discharge,
// which is the whole of what hold means.
func holdReserve(soc, minSoc float64) uint64 {
	return clampReserve(max(soc, minSoc))
}

// SetBatteryModeSoc maps an evcc battery mode to Tesla Fleet API commands,
// pinning the reserve to the optimizer's target SoC where it matters (see
// dischargeReserve). It implements api.BatteryModeController; SetBatteryMode
// (via newBatteryController) calls it with targetSoc 0.
//
// Tesla Powerwall modes:
//   - self_consumption: battery charges from solar, discharges to home (default)
//   - autonomous (TOU): battery follows time-of-use schedule
//   - backup: battery holds charge for backup, only charges from solar
//
// Mapping to evcc BatteryMode:
//   - BatteryNormal:   self_consumption mode, reserve at MinSoc - or, during a
//     battery boost (targetSoc > 0), at the plan's target SoC so the pack stops
//     feeding the car there instead of draining to MinSoc
//   - BatteryHold:     backup mode, reserve at current SOC (prevents discharge)
//   - BatteryHoldCharge: prevent charging. SoC 100: nothing to prevent → plain
//     self_consumption + 80% backup (no spike/TOU). SoC < 100: autonomous + export
//     spike + pv_only, reserve a small margin below the current SoC so a PV dip is
//     still covered from the battery (never below MinSoc; the 80 floor applies in
//     the PW3-rejected (80,100) band).
//   - BatteryCharge:   self_consumption mode, reserve at MaxSoc. Grid charging is
//     enabled only when the allowGridImport config option is set; otherwise it is
//     forced off, so BatteryCharge charges from solar/surplus only.
//   - BatteryDischarge: autonomous (TOU) mode + battery_ok export + a synthetic
//     tariff that spikes the real buy/sell price for the current window, driving
//     the Powerwall to export stored energy to grid. The reserve is pinned at the
//     optimizer's target SoC so it stops there. The user's real tariff is
//     snapshotted first and restored when leaving.
func (m *FleetAPI) SetBatteryModeSoc(mode api.BatteryMode, targetSoc float64) error {
	limits := m.socLimits
	site := m.provider.site

	// Keep the VPP event reserve synced to the optimizer's discharge floor. A Tesla
	// VPP (grid-services) event is auto-dispatched by Tesla down to
	// vpp_backup_reserve_percent, so writing the optimizer's floor here makes an
	// event stop where the optimizer wants — not at the app's static VPP reserve.
	// Only for modes we actually apply (skip unsupported ones — no pack writes), and
	// best-effort so a VPP-reserve failure never blocks the real mode change.
	switch mode {
	case api.BatteryNormal, api.BatteryHold, api.BatteryHoldCharge, api.BatteryCharge, api.BatteryDischarge:
		m.syncVPPReserve(targetSoc)
	default:
		return api.ErrNotAvailable
	}

	switch mode {
	case api.BatteryNormal:
		if err := m.restoreTariff(); err != nil {
			return err
		}
		reserve := normalReserve(limits.MinSoc, targetSoc)
		m.provider.log.DEBUG.Printf("normal: → self_consumption, reserve %d%%", reserve)
		if err := m.apiWrite("operating mode → self_consumption", func() error { return site.SetOperatingMode("self_consumption") }); err != nil {
			return err
		}
		if err := m.apiWrite("grid charging → off", func() error { return site.SetGridCharging(false) }); err != nil {
			return err
		}
		return m.apiWrite(fmt.Sprintf("battery reserve → %d%%", reserve), func() error { return site.SetBatteryReserve(reserve) })

	case api.BatteryHold:
		// idle while importing: withhold discharge, and nothing more. The reserve at
		// the current SoC is what blocks discharge; the operating mode only decides
		// who gets the solar.
		//
		// This used to ask for backup mode, which is not "idle": Backup-only charges
		// the pack for standby and takes priority over the house, so the PW3 absorbed
		// the whole PV output while the house and car drew their entire load from the
		// grid (observed: pv 1720W == pack charge 1720W, grid == house + car). Holding
		// discharge cost ~1.7kW of import that way.
		//
		// self_consumption serves the house from solar first and only banks the
		// surplus, so the reserve still blocks discharge but PV is no longer diverted.
		// Grid charging stays off, so the pack can only take genuine surplus.
		if err := m.restoreTariff(); err != nil {
			return err
		}
		soc, err := m.batterySoc()
		if err != nil {
			return err
		}
		reserve := holdReserve(soc, limits.MinSoc)
		m.provider.log.DEBUG.Printf("hold: soc %.1f%% → %s, holding reserve at %d%%", soc, holdOperatingMode, reserve)
		if err := m.apiWrite("operating mode → "+holdOperatingMode, func() error { return site.SetOperatingMode(holdOperatingMode) }); err != nil {
			return err
		}
		if err := m.apiWrite("grid charging → off", func() error { return site.SetGridCharging(false) }); err != nil {
			return err
		}
		return m.apiWrite(fmt.Sprintf("battery reserve → %d%%", reserve), func() error { return site.SetBatteryReserve(reserve) })

	case api.BatteryHoldCharge:
		// idle while exporting: prevent the pack from charging further.
		soc, err := m.batterySoc()
		if err != nil {
			return err
		}

		// At 100% there's nothing to prevent — the pack can't charge, so surplus
		// exports naturally. Use plain self_consumption (self-powered, no spike or
		// TOU) with an 80% backup so it stays high and covers the home from the
		// battery if solar dips.
		if soc >= 100 {
			m.provider.log.DEBUG.Printf("holdcharge: soc %.0f%% (full) → self_consumption, reserve 80%% (nothing to prevent, surplus exports)", soc)
			if err := m.restoreTariff(); err != nil {
				return err
			}
			if err := m.apiWrite("operating mode → self_consumption", func() error { return site.SetOperatingMode("self_consumption") }); err != nil {
				return err
			}
			if err := m.apiWrite("grid charging → off", func() error { return site.SetGridCharging(false) }); err != nil {
				return err
			}
			return m.apiWrite("battery reserve → 80%", func() error { return site.SetBatteryReserve(clampReserve(80)) })
		}

		// Below 100%: hold the pack roughly in place (autonomous + the sell-only
		// spike so the Powerwall exports surplus instead of storing it; pv_only so
		// the battery itself isn't dumped to grid; buy stays real so it still covers
		// the home). Sit the reserve a small margin BELOW the current SoC so a brief
		// PV dip is covered from the battery instead of forcing a grid import — the
		// pack refusing to discharge below a reserve pinned at the SoC was the cause
		// of morning grid pulls. Never below MinSoc; the PW3 rejects a reserve in
		// (80,100), so above ~80 the margin widens to the 80 floor.
		reserve := clampReserve(holdChargeReserve(soc, limits.MinSoc))
		held := "holding in place"
		if float64(reserve) < soc {
			held = fmt.Sprintf("covering dips down to %d%%", reserve)
		}
		m.provider.log.DEBUG.Printf("holdcharge: soc %.0f%% → autonomous + sell spike + pv_only, reserve %d%% (%s)", soc, reserve, held)
		m.snapshotTariff()
		if err := m.apiWrite("operating mode → autonomous", func() error { return site.SetOperatingMode("autonomous") }); err != nil {
			return err
		}
		if err := m.apiWrite("grid export → pv_only", func() error { return site.SetGridExport("pv_only") }); err != nil {
			return err
		}
		if err := m.setDischargeTariff(); err != nil {
			return err
		}
		return m.apiWrite(fmt.Sprintf("battery reserve → %d%%", reserve), func() error { return site.SetBatteryReserve(reserve) })

	case api.BatteryCharge:
		if err := m.restoreTariff(); err != nil {
			return err
		}
		reserve := clampReserve(limits.MaxSoc)
		// grid charging is opt-in via the allowGridImport config option. When off,
		// BatteryCharge degrades to plain self_consumption (charges from solar/surplus
		// only), never pulling the pack up from the grid.
		gridCharging := m.allowGridImport
		label := onOff(gridCharging)
		m.provider.log.DEBUG.Printf("charge: → self_consumption, grid charging %s, reserve %d%%", label, reserve)
		if err := m.apiWrite("operating mode → self_consumption", func() error { return site.SetOperatingMode("self_consumption") }); err != nil {
			return err
		}
		if err := m.apiWrite("grid charging → "+label, func() error { return site.SetGridCharging(gridCharging) }); err != nil {
			return err
		}
		return m.apiWrite(fmt.Sprintf("battery reserve → %d%%", reserve), func() error { return site.SetBatteryReserve(reserve) })

	case api.BatteryDischarge:
		// pin the reserve at the optimizer's target SoC so the pack stops there
		// instead of draining to min soc through the ~2min pickup lag + slot.
		reserve := clampReserve(dischargeReserve(targetSoc, limits.MinSoc))
		m.provider.log.DEBUG.Printf("discharge: → autonomous + sell spike + battery_ok, reserve %d%% (target %.0f%%, dump to grid)", reserve, targetSoc)
		// snapshot the user's real tariff before we overwrite it
		m.snapshotTariff()
		if err := m.apiWrite("operating mode → autonomous", func() error { return site.SetOperatingMode("autonomous") }); err != nil {
			return err
		}
		if err := m.apiWrite("grid export → battery_ok", func() error { return site.SetGridExport("battery_ok") }); err != nil {
			return err
		}
		if err := m.setDischargeTariff(); err != nil {
			return err
		}
		return m.apiWrite(fmt.Sprintf("battery reserve → %d%%", reserve), func() error { return site.SetBatteryReserve(reserve) })

	default:
		return api.ErrNotAvailable
	}
}

// Tariff support

var _ api.Tariff = (*FleetAPI)(nil)

// NewFleetAPITariffFromConfig creates a Tesla Fleet API Tariff from generic config.
func NewFleetAPITariffFromConfig(other map[string]any) (api.Tariff, error) {
	if other == nil {
		other = make(map[string]any)
	}
	switch other["tariffDirection"] {
	case "export":
		other["usage"] = "feedin"
	default:
		other["usage"] = "grid"
	}
	delete(other, "tariffDirection")
	m, err := NewFleetAPIFromConfig(other)
	if err != nil {
		return nil, err
	}
	t, _ := m.(api.Tariff)
	return t, nil
}

// Rates implements the api.Tariff interface.
func (m *FleetAPI) Rates() (api.Rates, error) {
	isFeedIn := m.usage == "feedin" || m.usage == "solar"

	var rates api.Rates
	if isFeedIn {
		// Export credit uses the CleanPowerSF generation rate (retail net metering),
		// NOT the Tesla sell_tariff. sell_tariff reports the NBT/ACC total, which
		// overstates export for a CCA customer whose generation is retail-netted. The
		// PG&E delivery EEC and the net-position / NSC realization are layered on in
		// core (they need metered history the meter package can't reach). Independent
		// of the Tesla tariff, so no site-info call on the export path.
		rates = cleanPowerSFGenerationRates()
	} else {
		site, err := m.provider.siteInfoG()
		if err != nil {
			return nil, err
		}
		if tc := m.effectiveTariff(site); tc != nil {
			rates, err = ratesFromV2(tc.Seasons, tc.EnergyCharges)
		} else if site.TariffContent != nil {
			rates, err = ratesFromV1(site.TariffContent.Seasons, site.TariffContent.EnergyCharges)
		} else {
			return nil, api.ErrNotAvailable
		}
		if err != nil {
			return nil, err
		}
	}

	// overlay the VPP export premium on feed-in slots that fall inside a scheduled
	// grid-services event, so the optimizer values export during the event and
	// pre-charges for it. Only when auto-detection is configured (feed-in meter).
	if isFeedIn && m.vppEventsG != nil {
		if events, verr := m.vppEventsG(); verr == nil {
			rates = applyVPPPremium(rates, events, m.vppPremium)
		} else {
			m.provider.log.WARN.Printf("vpp events unavailable, feed-in premium not applied: %v", verr)
		}
	}
	return rates, nil
}

// cleanPowerSFGenerationRates builds a 7-day hourly forecast of the CleanPowerSF
// retail generation rate - the export credit basis for a CleanPowerSF NEM customer.
// core (realizedExport) layers on the PG&E delivery EEC and the net-position / NSC
// realization, which require metered history unavailable to the meter package.
func cleanPowerSFGenerationRates() api.Rates {
	start := time.Now().Truncate(time.Hour)
	rates := make(api.Rates, 0, 7*24)
	for i := range 7 * 24 {
		s := start.Add(time.Duration(i) * time.Hour)
		rates = append(rates, api.Rate{Start: s, End: s.Add(time.Hour), Value: cleanpowersf.GenerationRate(s)})
	}
	return rates
}

// enableVPP turns on VPP event auto-detection for this (feed-in) meter: it polls
// the owner-api programs schedule (authenticated by the ownerapi refresh token)
// and feeds the windows into Rates via applyVPPPremium.
func (m *FleetAPI) enableVPP(refreshToken string, premium float64) {
	if premium <= 0 {
		premium = vppPremiumDefault
	}
	log := m.provider.log
	client := newVPPClient(log, newOwnerAPITokenSource(log, refreshToken))
	siteID := m.provider.energySiteID
	m.vppPremium = premium
	m.vppEventsG = util.Cached(func() ([]vppEvent, error) {
		return client.events(siteID)
	}, vppPollInterval)
	log.DEBUG.Printf("VPP event auto-detection enabled (premium $%.2f/kWh)", premium)
}

// Type implements the api.Tariff interface.
func (m *FleetAPI) Type() api.TariffType {
	site, err := m.provider.siteInfoG()
	if err != nil {
		return api.TariffTypePriceStatic
	}
	if tc := m.effectiveTariff(site); tc != nil {
		for _, season := range tc.Seasons {
			if len(season.TOUPeriods) > 1 {
				return api.TariffTypePriceDynamic
			}
		}
	} else if site.TariffContent != nil {
		for _, season := range site.TariffContent.Seasons {
			if len(season.TOUPeriods) > 1 {
				return api.TariffTypePriceDynamic
			}
		}
	}
	return api.TariffTypePriceStatic
}

// tariff helpers

func tariffSeasonMonths(fromMonth, toMonth int) []fixed.Month {
	from := fixed.Month(fromMonth - 1)
	to := fixed.Month(toMonth - 1)
	if to < from {
		to += 12
	}
	months := make([]fixed.Month, 0, to-from+1)
	for m := from; m <= to; m++ {
		months = append(months, m%12)
	}
	return months
}

func tariffPeriodDays(fromDow, toDow int) []fixed.Day {
	from, to := fixed.Day(fromDow), fixed.Day(toDow)
	days := make([]fixed.Day, 0, to-from+1)
	for d := from; d <= to; d++ {
		days = append(days, d)
	}
	return days
}

func tariffPeriodRate(day time.Time, period teslaclient.TariffTOUPeriod, price float64) api.Rate {
	from := fixed.HourMin{Hour: period.FromHour, Min: period.FromMinute}
	to := fixed.HourMin{Hour: period.ToHour, Min: period.ToMinute}
	rateStart := time.Date(day.Year(), day.Month(), day.Day(), from.Hour, from.Min, 0, 0, day.Location())
	var rateEnd time.Time
	if to.IsNil() {
		rateEnd = day.AddDate(0, 0, 1)
	} else {
		rateEnd = time.Date(day.Year(), day.Month(), day.Day(), to.Hour, to.Min, 0, 0, day.Location())
	}
	return api.Rate{Start: rateStart, End: rateEnd, Value: price}
}

// spikeTariff builds a synthetic Powerwall tariff from the user's real tariff by
// adding dischargeSpikeBump to the SELL price only, for the current 15-min slot
// plus the next one, leaving every other slot (and the whole buy tariff) at the
// real rate. The Powerwall's onboard TOU optimizer then sees a short, localized
// export incentive (sell >> buy) for the slot the optimizer is acting on; it is
// re-written as the slot advances so it never goes stale, and the extra slot keeps
// runway ahead so a late-in-slot write still exports. Buy is left real — bumping it
// raises the value of stored energy and makes the pack hoard instead of export.
// The copy is tagged EVCC so the guard can tell it apart from a real tariff. NOTE:
// only takes effect in autonomous (TOU) mode; self_consumption ignores the tariff.
func spikeTariff(real *teslaclient.TariffContentV2, now time.Time) teslaclient.TariffContentV2 {
	return spikeTariffBumps(real, now, 0, dischargeSpikeBump)
}

// spikeTariffBumps is spikeTariff with independent buy/sell bumps, so the probe
// can test a sell-only spike (buyBump=0). A bump of 0 leaves that side untouched.
func spikeTariffBumps(real *teslaclient.TariffContentV2, now time.Time, buyBump, sellBump float64) teslaclient.TariffContentV2 {
	// window = the current 15-min slot PLUS the next 15 min (aligned to the grid),
	// in minutes since midnight. The extra slot gives the Powerwall's TOU algorithm
	// enough runway to react even when we (re)write near the end of a slot — with
	// only the current slot, a write in the last ~2 min lapses before it exports.
	slot := now.Truncate(spikeSlotDuration)
	winStart := slot.Hour()*60 + slot.Minute()
	winEnd := min(winStart+2*int(spikeSlotDuration/time.Minute), 24*60)
	month := int(now.Month())

	out := *real
	out.Utility = syntheticTariffUtility
	out.Name = "Custom"
	out.Version = 2
	out.Seasons, out.EnergyCharges = spikeSeasons(real.Seasons, real.EnergyCharges, month, winStart, winEnd, buyBump)
	if real.SellTariff != nil {
		sell := *real.SellTariff
		sell.Seasons, sell.EnergyCharges = spikeSeasons(real.SellTariff.Seasons, real.SellTariff.EnergyCharges, month, winStart, winEnd, sellBump)
		out.SellTariff = &sell
	}
	return out
}

// spikeSeasons returns copies of seasons/energyCharges in which, for the season
// active in the given month, each TOU period overlapping [winStart,winEnd) is
// carved so the overlapping sub-slot gets a separate period keyed "<name>_SPIKE"
// with its rate bumped by dischargeSpikeBump. Unaffected seasons are shared
// as-is (never mutated); the real tariff maps are left untouched.
func spikeSeasons(seasons map[string]teslaclient.TariffSeasonV2, charges map[string]teslaclient.TariffRates, month, winStart, winEnd int, bump float64) (map[string]teslaclient.TariffSeasonV2, map[string]teslaclient.TariffRates) {
	if bump <= 0 {
		return seasons, charges // this side of the tariff is left untouched
	}

	outSeasons := make(map[string]teslaclient.TariffSeasonV2, len(seasons))
	maps.Copy(outSeasons, seasons)
	outCharges := make(map[string]teslaclient.TariffRates, len(charges))
	maps.Copy(outCharges, charges)

	for name, season := range seasons {
		if !slices.Contains(tariffSeasonMonths(season.FromMonth, season.ToMonth), fixed.Month(month-1)) {
			continue
		}
		rates, ok := charges[name]
		if !ok {
			continue
		}

		newTOU := make(map[string]teslaclient.TariffSeasonV2TOUPeriods, len(season.TOUPeriods))
		newRates := make(map[string]float64, len(rates.Rates))
		maps.Copy(newRates, rates.Rates)

		for periodName, tou := range season.TOUPeriods {
			rate, hasRate := rates.Rates[periodName]
			base := make([]teslaclient.TariffTOUPeriod, 0, len(tou.Periods))
			var spike []teslaclient.TariffTOUPeriod
			for _, p := range tou.Periods {
				before, mid, after, carved := carveWindow(p, winStart, winEnd)
				if !hasRate || !carved {
					base = append(base, p)
					continue
				}
				base = append(base, before...)
				base = append(base, after...)
				spike = append(spike, mid...)
			}
			newTOU[periodName] = teslaclient.TariffSeasonV2TOUPeriods{Periods: base}
			if len(spike) > 0 {
				spikeName := periodName + spikeSuffix
				newTOU[spikeName] = teslaclient.TariffSeasonV2TOUPeriods{Periods: spike}
				newRates[spikeName] = rate + bump
			}
		}

		s := season
		s.TOUPeriods = newTOU
		outSeasons[name] = s
		outCharges[name] = teslaclient.TariffRates{Rates: newRates}
	}

	return outSeasons, outCharges
}

// unspikeTariff reconstructs the user's real tariff from a synthetic export
// spike, for use when no snapshot is available to restore from. The spike is
// structurally reversible: spikeSeasons only ADDS "<name>_SPIKE" TOU periods
// (carved out of a base period, at a bumped rate) and never alters the base
// rates — so merging each _SPIKE period back into its base period and dropping
// the bumped rate restores the original rate schedule. The EVCC marker is
// cleared so the result is no longer reported as synthetic. It does not mutate
// the input.
func unspikeTariff(t *teslaclient.TariffContentV2) *teslaclient.TariffContentV2 {
	if t == nil {
		return nil
	}
	out := *t
	out.Utility = ""
	out.Name = ""
	out.Seasons, out.EnergyCharges = unspikeSeasons(t.Seasons, t.EnergyCharges)
	if t.SellTariff != nil {
		sell := *t.SellTariff
		sell.Seasons, sell.EnergyCharges = unspikeSeasons(t.SellTariff.Seasons, t.SellTariff.EnergyCharges)
		out.SellTariff = &sell
	}
	return &out
}

// unspikeSeasons is the inverse of spikeSeasons: it returns copies of the
// seasons/charges maps with every "<name>_SPIKE" period merged back into its
// base "<name>" period and its bumped rate dropped. Maps with no _SPIKE period
// are returned as-is (shared, never mutated), so a non-spiked side (e.g. the
// untouched buy tariff of a sell-only spike) costs nothing.
func unspikeSeasons(seasons map[string]teslaclient.TariffSeasonV2, charges map[string]teslaclient.TariffRates) (map[string]teslaclient.TariffSeasonV2, map[string]teslaclient.TariffRates) {
	hasSpike := false
	for _, season := range seasons {
		for name := range season.TOUPeriods {
			if strings.HasSuffix(name, spikeSuffix) {
				hasSpike = true
			}
		}
	}
	if !hasSpike {
		return seasons, charges
	}

	outSeasons := make(map[string]teslaclient.TariffSeasonV2, len(seasons))
	maps.Copy(outSeasons, seasons)
	outCharges := make(map[string]teslaclient.TariffRates, len(charges))
	maps.Copy(outCharges, charges)

	for name, season := range seasons {
		var spiked []string
		for pname := range season.TOUPeriods {
			if strings.HasSuffix(pname, spikeSuffix) {
				spiked = append(spiked, pname)
			}
		}
		if len(spiked) == 0 {
			continue
		}

		newTOU := make(map[string]teslaclient.TariffSeasonV2TOUPeriods, len(season.TOUPeriods))
		maps.Copy(newTOU, season.TOUPeriods)
		newRates := make(map[string]float64)
		if r, ok := charges[name]; ok {
			newRates = make(map[string]float64, len(r.Rates))
			maps.Copy(newRates, r.Rates)
		}

		for _, sp := range spiked {
			if base, ok := newTOU[strings.TrimSuffix(sp, spikeSuffix)]; ok {
				merged := make([]teslaclient.TariffTOUPeriod, 0, len(base.Periods)+len(newTOU[sp].Periods))
				merged = append(merged, base.Periods...)
				merged = append(merged, newTOU[sp].Periods...)
				newTOU[strings.TrimSuffix(sp, spikeSuffix)] = teslaclient.TariffSeasonV2TOUPeriods{Periods: merged}
			}
			delete(newTOU, sp)
			delete(newRates, sp)
		}

		s := season
		s.TOUPeriods = newTOU
		outSeasons[name] = s
		outCharges[name] = teslaclient.TariffRates{Rates: newRates}
	}

	return outSeasons, outCharges
}

// carveWindow splits a TOU period around the minute window [winStart,winEnd)
// (minutes since midnight). It
// returns the portions before and after the window (to keep the original rate)
// and the overlapping portion (to be bumped), plus whether any overlap existed.
// A period ending at hour 0 is treated as running through the end of the day.
func carveWindow(p teslaclient.TariffTOUPeriod, winStart, winEnd int) (before, mid, after []teslaclient.TariffTOUPeriod, carved bool) {
	fromMin := p.FromHour*60 + p.FromMinute
	toMin := p.ToHour*60 + p.ToMinute
	if toMin == 0 {
		toMin = 24 * 60 // 00:00 end = through end of day
	}

	lo := max(fromMin, winStart)
	hi := min(toMin, winEnd)
	if lo >= hi {
		return nil, nil, nil, false
	}

	if lo > fromMin {
		b := p
		b.FromHour, b.FromMinute, b.ToHour, b.ToMinute = fromMin/60, fromMin%60, lo/60, lo%60
		before = []teslaclient.TariffTOUPeriod{b}
	}
	if hi < toMin {
		a := p
		a.FromHour, a.FromMinute, a.ToHour, a.ToMinute = hi/60, hi%60, (toMin/60)%24, toMin%60 // 24:00 -> 00:00
		after = []teslaclient.TariffTOUPeriod{a}
	}
	m := p
	m.FromHour, m.FromMinute, m.ToHour, m.ToMinute = lo/60, lo%60, (hi/60)%24, hi%60 // 24:00 -> 00:00
	mid = []teslaclient.TariffTOUPeriod{m}
	return before, mid, after, true
}

func ratesFromV2(seasons map[string]teslaclient.TariffSeasonV2, energyCharges map[string]teslaclient.TariffRates) (api.Rates, error) {
	var rates api.Rates
	now := time.Now().Local()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	for i := range 7 {
		day := start.AddDate(0, 0, i)
		month := fixed.Month(day.Month() - 1)
		dow := fixed.Day(day.Weekday())

		for seasonName, season := range seasons {
			if !slices.Contains(tariffSeasonMonths(season.FromMonth, season.ToMonth), month) {
				continue
			}
			charges, ok := energyCharges[seasonName]
			if !ok {
				continue
			}
			for periodName, touPeriods := range season.TOUPeriods {
				price, ok := charges.Rates[periodName]
				if !ok {
					continue
				}
				for _, period := range touPeriods.Periods {
					if !slices.Contains(tariffPeriodDays(period.FromDayOfWeek, period.ToDayOfWeek), dow) {
						continue
					}
					rates = append(rates, tariffPeriodRate(day, period, price))
				}
			}
		}
	}

	rates.Sort()
	return rates, nil
}

func ratesFromV1(seasons map[string]teslaclient.TariffSeason, energyCharges map[string]map[string]float64) (api.Rates, error) {
	var rates api.Rates
	now := time.Now().Local()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	for i := range 7 {
		day := start.AddDate(0, 0, i)
		month := fixed.Month(day.Month() - 1)
		dow := fixed.Day(day.Weekday())

		for seasonName, season := range seasons {
			if !slices.Contains(tariffSeasonMonths(season.FromMonth, season.ToMonth), month) {
				continue
			}
			seasonCharges, ok := energyCharges[seasonName]
			if !ok {
				continue
			}
			for periodName, touPeriods := range season.TOUPeriods {
				price, ok := seasonCharges[periodName]
				if !ok {
					continue
				}
				for _, period := range touPeriods {
					if !slices.Contains(tariffPeriodDays(period.FromDayOfWeek, period.ToDayOfWeek), dow) {
						continue
					}
					rates = append(rates, tariffPeriodRate(day, period, price))
				}
			}
		}
	}

	rates.Sort()
	return rates, nil
}
