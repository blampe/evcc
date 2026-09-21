package core

import (
	"sort"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/cleanpowersf"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
)

// NEM 3.0 export valuation for a CleanPowerSF (CCA) customer.
//
// An exported kWh pays into two independent accounts that each only offset like
// charges, so its realized worth is the sum of two bucket terms:
//
//		export(h) = delivery_EEC(h)·R_delivery  +  [NSC + (gen_retail(h) − NSC)·R_gen]
//
//	  - DELIVERY (PG&E, NBT): credited at the hourly ACC Energy Export Credit (the
//	    feed-in tariff). A credit is spendable only while unmet delivery CHARGES
//	    remain this true-up year; excess strands. R_delivery is that headroom.
//	    PG&E's true-up follows the customer's anniversary (deliveryTrueUpMonth).
//
//	  - GENERATION (CleanPowerSF, NEM): credited at the flat retail generation rate
//	    (see the cleanpowersf package), net-metered and banked. A banked credit is
//	    worth retail only if drawn down as a net-generation IMPORTER; a net
//	    exporter's surplus cashes out at Net Surplus Compensation. R_gen is that net
//	    position, floored at NSC. CleanPowerSF's true-up is May (cleanpowersf.TrueUpMonth).
//
// Both factors are computed from evcc's own metered grid history over each
// bucket's true-up year, priced against the rates actually in effect - the
// persisted import price, the delivery EEC schedule, and the cleanpowersf rate
// table. Nothing is hard-coded here.
const (
	// deliveryEECArchiveURI is the full-vintage PG&E NBT delivery export-credit
	// schedule (github.com/danielraffel/pge-feed-in-rates), used to value metered
	// export history. Mirrors the feed-in tariff source but spans all years.
	deliveryEECArchiveURI = "https://raw.githubusercontent.com/danielraffel/pge-feed-in-rates/refs/heads/main/archives/NBT26-delivery-feed-in-rates.json"

	// deliveryTrueUpMonth is PG&E's NBT (delivery) annual true-up anniversary.
	deliveryTrueUpMonth = time.February

	// realizationBand is the saturation taper width as a fraction of charges: a
	// bucket's factor falls linearly 1→0 as its credits exceed its charges by up
	// to this much, avoiding a hard flip at the crossover.
	realizationBand = 0.25

	// minRealizationEnergy is the metered throughput (kWh) below which history is
	// too thin to judge net position; export is then valued delivery-only.
	minRealizationEnergy = 10
)

// deliveryEECSchedule fetches and caches the delivery EEC schedule. It is a
// static vintage-locked dataset, so a daily refresh only picks up corrections.
var deliveryEECSchedule = util.Cached(func() (api.Rates, error) {
	var res []struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
		Price float64   `json:"price"`
	}
	if err := request.NewHelper(util.NewLogger("nem3")).GetJSON(deliveryEECArchiveURI, &res); err != nil {
		return nil, err
	}
	rates := make(api.Rates, len(res))
	for i, r := range res {
		rates[i] = api.Rate{Start: r.Start, End: r.End, Value: r.Price}
	}
	rates.Sort()
	return rates, nil
}, 24*time.Hour)

// trueUpStart returns the start of the current annual true-up period for the given
// anniversary month: the most recent such boundary at or before now.
func trueUpStart(now time.Time, month time.Month) time.Time {
	start := time.Date(now.Year(), month, 1, 0, 0, 0, 0, now.Location())
	if start.After(now) {
		start = start.AddDate(-1, 0, 0)
	}
	return start
}

// rateAt returns the value of the rate slot covering t, if any. rates must be
// sorted by start.
func rateAt(rates api.Rates, t time.Time) (float64, bool) {
	i := sort.Search(len(rates), func(i int) bool { return rates[i].End.After(t) })
	if i < len(rates) && !t.Before(rates[i].Start) && t.Before(rates[i].End) {
		return rates[i].Value, true
	}
	return 0, false
}

// buyLookup turns persisted import-price samples into a step function: the price
// in effect at t is the most recent sample at or before it. Zero before the first
// sample.
func buyLookup(prices []metrics.GridPrice) func(time.Time) float64 {
	return func(t time.Time) float64 {
		i := sort.Search(len(prices), func(i int) bool { return prices[i].Start.After(t) })
		if i == 0 {
			return 0
		}
		return prices[i-1].Value
	}
}

// realizationFactor maps a bucket's true-up-to-date credits vs charges to a [0,1]
// multiplier: 1 while credits stay at or below charges (every credit lands on a
// real charge), tapering to 0 once credits saturate charges by realizationBand.
func realizationFactor(credits, charges float64) float64 {
	excess := credits - charges
	if excess <= 0 {
		return 1
	}
	band := realizationBand * charges
	if band <= 0 {
		return 0
	}
	return max(0, 1-excess/band)
}

// bucketPosition sums a bucket's credits (metered export × creditRate) and charges
// (metered import × chargeRate) over its true-up year, pricing each metered hour
// against the rate in effect then. throughput is total metered energy.
func (site *Site) bucketPosition(now time.Time, month time.Month,
	creditRate, chargeRate func(time.Time) float64,
) (credits, charges, throughput float64, ok bool) {
	series, err := metrics.QueryEnergy(trueUpStart(now, month), now, "hour", true)
	if err != nil {
		site.log.WARN.Printf("export realization: metered history unavailable: %v", err)
		return 0, 0, 0, false
	}
	for _, s := range series {
		if s.Group != metrics.Grid {
			continue
		}
		for _, slot := range s.Data {
			credits += slot.ReturnEnergy * creditRate(slot.Start)
			charges += slot.Energy * chargeRate(slot.Start)
			throughput += slot.Energy + slot.ReturnEnergy
		}
	}
	return credits, charges, throughput, true
}

// realizationFactors is the cached result of exportRealization.
type realizationFactors struct {
	rDelivery, rGen float64
	ok              bool
}

// realizationTTL is how long the realization factors are cached. They aggregate a
// full true-up year of metered history, so one more slot of energy barely moves
// them - a day-stale value is inconsequential, and it lets realizedFeedInRates run
// every cycle for free.
const realizationTTL = 24 * time.Hour

// realization returns the export realization factors, computing them at most once
// per realizationTTL (realizationG is wired up in NewSite). Safe to call on every
// update loop.
func (site *Site) realization() (rDelivery, rGen float64, ok bool) {
	f, _ := site.realizationG()
	return f.rDelivery, f.rGen, f.ok
}

// exportRealization computes the delivery and generation realization factors from
// metered grid history over each bucket's own true-up year. ok is false (caller
// leaves the export price unadjusted) when history or the schedule is unavailable.
func (site *Site) exportRealization(now time.Time) (rDelivery, rGen float64, ok bool) {
	eec, err := deliveryEECSchedule()
	if err != nil {
		site.log.WARN.Printf("export realization: delivery EEC schedule unavailable: %v", err)
		return 1, 0, false
	}
	// combined import price actually in effect, over the wider (delivery) window
	prices, err := metrics.QueryGridPrices(trueUpStart(now, deliveryTrueUpMonth), now)
	if err != nil {
		site.log.WARN.Printf("export realization: import price history unavailable: %v", err)
		return 1, 0, false
	}
	buy := buyLookup(prices)

	deliveryCredit := func(t time.Time) float64 { v, _ := rateAt(eec, t); return v }
	// PG&E delivery charge = live combined import rate minus the CleanPowerSF
	// generation component (NBC/riders fold into delivery, a slight overstatement
	// that only makes R_delivery more conservative).
	deliveryCharge := func(t time.Time) float64 { return max(0, buy(t)-cleanpowersf.GenerationRate(t)) }
	genRate := cleanpowersf.GenerationRate // generation nets export and import at the same retail rate

	dCr, dCh, dThru, ok1 := site.bucketPosition(now, deliveryTrueUpMonth, deliveryCredit, deliveryCharge)
	gCr, gCh, _, ok2 := site.bucketPosition(now, cleanpowersf.TrueUpMonth, genRate, genRate)
	if !ok1 || !ok2 {
		return 1, 0, false
	}
	if dThru < minRealizationEnergy {
		return 1, 0, true // too little history to judge net position -> delivery-only
	}

	rDelivery = realizationFactor(dCr, dCh)
	rGen = realizationFactor(gCr, gCh)
	site.log.DEBUG.Printf("export realization: R_delivery=%.2f (cr=$%.2f ch=$%.2f) R_gen=%.2f (cr=$%.2f ch=$%.2f)",
		rDelivery, dCr, dCh, rGen, gCr, gCh)
	return rDelivery, rGen, true
}

// realizedExport turns the feed-in forecast into the realized export value. The
// feed-in carries the CleanPowerSF generation rate (from the meter) plus any VPP
// premium the meter overlaid. This adds the PG&E delivery EEC (deliveryEEC) scaled
// by its saturation factor, replaces generation with its realized value (floored at
// NSC, climbing to retail as the net-generation position flips exporter→importer),
// and passes any VPP premium through untouched.
func realizedExport(feedIn, deliveryEEC api.Rates, rDelivery, rGen float64) api.Rates {
	out := make(api.Rates, len(feedIn))
	for i, slot := range feedIn {
		delivery, _ := rateAt(deliveryEEC, slot.Start)
		slot.Value = realizedExportAt(slot.Start, slot.Value, delivery, rDelivery, rGen)
		out[i] = slot
	}
	return out
}

// realizedExportAt is the per-slot export valuation shared by the forecast
// (realizedExport) and point (realizedFeedInNow) paths: delivery EEC scaled by
// R_delivery, plus CleanPowerSF generation floored at NSC and climbing to retail by
// R_gen, plus any VPP premium the meter overlaid on feedIn above the generation rate.
func realizedExportAt(t time.Time, feedIn, delivery, rDelivery, rGen float64) float64 {
	genRetail := cleanpowersf.GenerationRate(t)
	nsc := cleanpowersf.NetSurplusCompensation(t)
	premium := max(0, feedIn-genRetail)
	return delivery*rDelivery + nsc + (genRetail-nsc)*rGen + premium
}

// realizedImportAt is the per-slot import valuation shared by the forecast
// (realizedImport) and point (realizedGridNow) paths: full retail buy less the
// generation premium a net exporter never pays (see realizedImport).
func realizedImportAt(t time.Time, buy, rGen float64) float64 {
	genRetail := cleanpowersf.GenerationRate(t)
	nsc := cleanpowersf.NetSurplusCompensation(t)
	return buy - max(0, genRetail-nsc)*(1-rGen)
}

// realizedFeedInRates values the feed-in forecast at its realized export worth. It
// is the single export rate surfaced everywhere export value appears: the
// optimizer, the published/persisted feed-in shown in the UI, and the sell tariff
// written back to the Powerwall. It layers the two NEM3 buckets onto the raw
// feed-in (the CleanPowerSF generation rate the meter reports, plus any VPP
// premium): the PG&E delivery EEC scaled by whether delivery credits still have
// charges left to offset this true-up year (R_delivery), and CleanPowerSF
// generation floored at Net Surplus Compensation and climbing to the retail rate
// as the net-generation position flips exporter→importer (R_gen) - see
// realizedExport / exportRealization. For a net exporter this is ~delivery + NSC;
// it climbs to delivery + retail generation (never Tesla's ACC-based total) as
// winter charging draws the NEM bank down. Falls back to the raw feed-in when
// metered history or the delivery EEC schedule is unavailable.
func (site *Site) realizedFeedInRates() api.Rates {
	feedIn := currentRates(site.GetTariff(api.TariffUsageFeedIn))
	if len(feedIn) == 0 {
		return feedIn
	}
	rDelivery, rGen, ok := site.realization()
	if !ok {
		return feedIn
	}
	eec, err := deliveryEECSchedule()
	if err != nil {
		site.log.WARN.Printf("export realization: delivery EEC schedule unavailable: %v", err)
		return feedIn
	}
	return realizedExport(feedIn, eec, rDelivery, rGen)
}

// realizedImport values the grid import forecast at its realized marginal cost, the
// buy-side mirror of realizedExport. Under CleanPowerSF NEM the generation half of
// an imported kWh nets against the same retail-priced bank as an exported one, so
// for a net-generation exporter (rGen→0) it really costs Net Surplus Compensation,
// not the retail generation rate: realized buy = buy − (gen_retail − NSC)·(1 − rGen).
// Otherwise the optimizer overstates import cost, and inflates the TOU arbitrage
// spread by a generation premium a net exporter never pays. It climbs back to full
// retail as winter charging flips the position to net importer (rGen→1), which is
// exactly when battery TOU arbitrage becomes genuinely profitable again.
//
// Only the CleanPowerSF generation premium is discounted; the PG&E delivery charge
// stays at full retail. Discounting delivery by banked export credits would be the
// exact mirror, but that credit pool is finite and can't be expressed as a per-slot
// price without risking runaway import once it's exhausted (dormant for a customer
// with delivery-charge headroom, i.e. R_delivery=1, where the two are identical).
func realizedImport(grid api.Rates, rGen float64) api.Rates {
	out := make(api.Rates, len(grid))
	for i, slot := range grid {
		slot.Value = realizedImportAt(slot.Start, slot.Value, rGen)
		out[i] = slot
	}
	return out
}

// realizedImportRates applies realizedImport using the cached net-position factor,
// falling back to the raw grid tariff when metered history is unavailable.
func (site *Site) realizedImportRates(grid api.Rates) api.Rates {
	if len(grid) == 0 {
		return grid
	}
	_, rGen, ok := site.realization()
	if !ok {
		return grid
	}
	return realizedImport(grid, rGen)
}

// realizedGridNow returns the realized import price at the current instant (see
// realizedImport), or the raw grid price when unavailable. ok mirrors whether a
// grid tariff exists.
func (site *Site) realizedGridNow() (float64, bool) {
	raw, err := tariff.Now(site.GetTariff(api.TariffUsageGrid))
	if err != nil {
		return 0, false
	}
	_, rGen, ok := site.realization()
	if !ok {
		return raw, true
	}
	return realizedImportAt(time.Now(), raw, rGen), true
}

// realizedFeedInNow returns the realized export value at the current instant (see
// realizedExport), or the raw feed-in when unavailable.
func (site *Site) realizedFeedInNow() float64 {
	raw, err := tariff.Now(site.GetTariff(api.TariffUsageFeedIn))
	if err != nil {
		return 0
	}
	rDelivery, rGen, ok := site.realization()
	if !ok {
		return raw
	}
	eec, err := deliveryEECSchedule()
	if err != nil {
		return raw
	}
	now := time.Now()
	delivery, _ := rateAt(eec, now)
	return realizedExportAt(now, raw, delivery, rDelivery, rGen)
}
