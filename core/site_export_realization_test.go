package core

import (
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/cleanpowersf"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/stretchr/testify/assert"
)

func TestTrueUpStart(t *testing.T) {
	loc := time.UTC
	on := func(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, loc) }

	// delivery (February) anniversary
	assert.Equal(t, on(2026, time.February), trueUpStart(time.Date(2026, 9, 20, 18, 0, 0, 0, loc), time.February))
	assert.Equal(t, on(2025, time.February), trueUpStart(time.Date(2026, 1, 15, 0, 0, 0, 0, loc), time.February))
	// generation (May) anniversary
	assert.Equal(t, on(2026, time.May), trueUpStart(time.Date(2026, 9, 20, 0, 0, 0, 0, loc), time.May))
	assert.Equal(t, on(2025, time.May), trueUpStart(time.Date(2026, 3, 1, 0, 0, 0, 0, loc), time.May))
}

func TestRateAt(t *testing.T) {
	base := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	rates := api.Rates{
		{Start: base, End: base.Add(time.Hour), Value: 0.10},
		{Start: base.Add(time.Hour), End: base.Add(2 * time.Hour), Value: 0.35},
	}

	v, ok := rateAt(rates, base.Add(30*time.Minute))
	assert.True(t, ok)
	assert.Equal(t, 0.10, v)

	_, ok = rateAt(rates, base.Add(2*time.Hour)) // end exclusive
	assert.False(t, ok)
	_, ok = rateAt(rates, base.Add(-time.Minute)) // start inclusive
	assert.False(t, ok)
}

func TestBuyLookup(t *testing.T) {
	t0 := time.Date(2026, 8, 15, 12, 0, 0, 0, time.Local)
	prices := []metrics.GridPrice{
		{Start: t0, Value: 0.36},
		{Start: t0.Add(15 * time.Minute), Value: 0.42},
	}
	at := buyLookup(prices)
	assert.Equal(t, 0.0, at(t0.Add(-time.Minute))) // before the first sample
	assert.Equal(t, 0.36, at(t0.Add(5*time.Minute)))
	assert.Equal(t, 0.42, at(t0.Add(20*time.Minute))) // holds the most recent sample
}

func TestRealizationFactor(t *testing.T) {
	tc := []struct {
		name             string
		credits, charges float64
		want             float64
	}{
		{"no credits accrued", 0, 100, 1},
		{"credits below charges (headroom)", 30, 100, 1},
		{"credits equal charges (edge of saturation)", 100, 100, 1},
		{"half into the taper band", 112.5, 100, 0.5},
		{"fully saturated", 125, 100, 0},
		{"beyond saturation clamps to zero", 200, 100, 0},
		{"credits with no charges strand entirely", 50, 0, 0},
	}
	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			assert.InDelta(t, c.want, realizationFactor(c.credits, c.charges), 1e-9)
		})
	}
}

// TestRealizedExport: given a feed-in forecast carrying the CleanPowerSF generation
// rate (as the meter now provides), realizedExport blends in the PG&E delivery EEC
// (passed in) and the net-position/NSC realization. Crucially, generation resolves
// to CleanPowerSF RETAIL — never Tesla's ACC — even when fully realized.
func TestRealizedExport(t *testing.T) {
	at := time.Date(2026, 8, 15, 19, 0, 0, 0, time.UTC) // summer on-peak instant
	genRetail := cleanpowersf.GenerationRate(at)        // ~0.24327 (feed-in carries this)
	nsc := cleanpowersf.NetSurplusCompensation(at)      // ~0.0893
	deliveryVal := 0.05
	deliveryEEC := api.Rates{{Start: at.Add(-time.Hour), End: at.Add(2 * time.Hour), Value: deliveryVal}}
	feedIn := api.Rates{{Start: at, End: at.Add(time.Hour), Value: genRetail}}

	// net generation exporter (R_gen=0): delivery + generation floored at NSC
	out := realizedExport(feedIn, deliveryEEC, 1, 0)
	assert.InDelta(t, deliveryVal+nsc, out[0].Value, 1e-9)

	// net generation importer (R_gen=1): delivery + generation at RETAIL (not ACC)
	out = realizedExport(feedIn, deliveryEEC, 1, 1)
	assert.InDelta(t, deliveryVal+genRetail, out[0].Value, 1e-9)

	// delivery saturated (R_delivery=0) but net importer: generation-at-retail only
	out = realizedExport(feedIn, deliveryEEC, 0, 1)
	assert.InDelta(t, genRetail, out[0].Value, 1e-9)

	// a VPP premium the meter overlaid (feed-in > generation) passes straight through
	vppFeed := api.Rates{{Start: at, End: at.Add(time.Hour), Value: 2.0}}
	out = realizedExport(vppFeed, deliveryEEC, 1, 0)
	assert.InDelta(t, deliveryVal+nsc+(2.0-genRetail), out[0].Value, 1e-9)

	assert.Equal(t, genRetail, feedIn[0].Value) // original not mutated
}

// TestRealizedImport: the buy-side mirror discounts the retail generation premium a
// net exporter never pays, and climbs back to full retail as the position flips to
// net importer (rGen→1) - so the TOU arbitrage spread grows toward net-neutral.
func TestRealizedImport(t *testing.T) {
	at := time.Date(2026, 8, 15, 19, 0, 0, 0, time.UTC) // summer on-peak instant
	gen := cleanpowersf.GenerationRate(at)
	nsc := cleanpowersf.NetSurplusCompensation(at)
	const buy = 0.579
	grid := api.Rates{{Start: at, End: at.Add(time.Hour), Value: buy}}

	// net generation exporter (rGen=0): discount the full generation premium
	out := realizedImport(grid, 0)
	assert.InDelta(t, buy-(gen-nsc), out[0].Value, 1e-9)

	// net generation importer (rGen=1): full retail, no discount (arbitrage fully on)
	out = realizedImport(grid, 1)
	assert.InDelta(t, buy, out[0].Value, 1e-9)

	// approaching net-neutral (rGen=0.5): half the premium restored
	out = realizedImport(grid, 0.5)
	assert.InDelta(t, buy-(gen-nsc)*0.5, out[0].Value, 1e-9)

	assert.Equal(t, buy, grid[0].Value) // original not mutated
}
