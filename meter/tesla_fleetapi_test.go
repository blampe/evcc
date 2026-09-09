package meter

import (
	"errors"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	teslaclient "github.com/evcc-io/tesla-proxy-client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRetryable verifies a mutating Fleet API failure is marked api.ErrMustRetry
// (so the control loop re-applies the state change next cycle) while preserving
// the underlying error and op context; a success passes through as nil.
func TestRetryable(t *testing.T) {
	assert.NoError(t, retryable("op", nil))

	err := retryable("set operating mode", errors.New("boom"))
	require.Error(t, err)
	assert.ErrorIs(t, err, api.ErrMustRetry)
	assert.ErrorContains(t, err, "set operating mode")
	assert.ErrorContains(t, err, "boom")
}

// TestClampReserve verifies reserves in the PW3-rejected (80,100) band snap to 80.
func TestClampReserve(t *testing.T) {
	assert.Equal(t, uint64(20), clampReserve(20))
	assert.Equal(t, uint64(80), clampReserve(80))   // boundary allowed
	assert.Equal(t, uint64(80), clampReserve(85))   // in band -> 80
	assert.Equal(t, uint64(80), clampReserve(99))   // in band -> 80
	assert.Equal(t, uint64(100), clampReserve(100)) // boundary allowed
	assert.Equal(t, uint64(0), clampReserve(-5))    // clamped low
}

// TestNormalReserve verifies self_consumption keeps the pack available down to
// MinSoc normally, but pins the reserve to the battery-boost target (never below
// MinSoc) when the optimizer passes a target SoC.
func TestNormalReserve(t *testing.T) {
	assert.Equal(t, uint64(20), normalReserve(20, 0), "no boost -> MinSoc")
	assert.Equal(t, uint64(50), normalReserve(20, 50), "boost -> plan target above MinSoc")
	assert.Equal(t, uint64(20), normalReserve(20, 10), "boost target below MinSoc -> floored at MinSoc")
	assert.Equal(t, uint64(80), normalReserve(20, 90), "boost target in the PW3-rejected band -> 80")
}

// TestSpikeTariff verifies the discharge tariff bumps the real SELL price (only)
// by dischargeSpikeBump for the current 15-min slot plus the next one, leaves buy
// and every other slot at the real rate, tags the result EVCC, and doesn't mutate
// the real tariff.
func TestSpikeTariff(t *testing.T) {
	n := time.Now()
	now := time.Date(n.Year(), n.Month(), n.Day(), 10, 30, 0, 0, time.Local)

	buyRate, sellRate := 0.36, 0.10
	wholeDay := map[string]teslaclient.TariffSeasonV2{
		"All": {FromMonth: 1, ToMonth: 12, TOUPeriods: map[string]teslaclient.TariffSeasonV2TOUPeriods{
			"OFF_PEAK": {Periods: []teslaclient.TariffTOUPeriod{{ToDayOfWeek: 6}}},
		}},
	}
	real := &teslaclient.TariffContentV2{
		Utility:       "PG&E",
		Seasons:       wholeDay,
		EnergyCharges: map[string]teslaclient.TariffRates{"All": {Rates: map[string]float64{"OFF_PEAK": buyRate}}},
		SellTariff: &teslaclient.TariffSellTariffV2{
			Seasons:       wholeDay,
			EnergyCharges: map[string]teslaclient.TariffRates{"All": {Rates: map[string]float64{"OFF_PEAK": sellRate}}},
		},
	}

	spiked := spikeTariff(real, now)

	// tagged EVCC so the guard detects it as synthetic
	assert.True(t, isSyntheticTariff(&spiked))
	// real tariff left untouched (no shared-map mutation)
	assert.Equal(t, "PG&E", real.Utility)
	assert.Len(t, real.Seasons["All"].TOUPeriods, 1)

	buy, err := ratesFromV2(spiked.Seasons, spiked.EnergyCharges)
	require.NoError(t, err)
	sell, err := ratesFromV2(spiked.SellTariff.Seasons, spiked.SellTariff.EnergyCharges)
	require.NoError(t, err)

	rateAt := func(rates api.Rates, hour, minute int) float64 {
		at := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.Local)
		for _, r := range rates {
			if !at.Before(r.Start) && at.Before(r.End) {
				return r.Value
			}
		}
		t.Fatalf("no rate at %02d:%02d", hour, minute)
		return 0
	}

	// now=10:30 => spike the 10:30-11:00 window (current slot + next) on SELL; buy stays real.
	for _, tc := range []struct {
		hour, minute      int
		wantBuy, wantSell float64
	}{
		{10, 20, buyRate, sellRate},                      // before the window
		{10, 37, buyRate, sellRate + dischargeSpikeBump}, // current slot
		{10, 50, buyRate, sellRate + dischargeSpikeBump}, // the next 15 min
		{11, 5, buyRate, sellRate},                       // after the window
	} {
		assert.InDelta(t, tc.wantBuy, rateAt(buy, tc.hour, tc.minute), 1e-9, "buy %02d:%02d (must stay real)", tc.hour, tc.minute)
		assert.InDelta(t, tc.wantSell, rateAt(sell, tc.hour, tc.minute), 1e-9, "sell %02d:%02d", tc.hour, tc.minute)
	}
}

// realAndSpike builds a real buy+sell tariff and the sell-only export spike
// derived from it for the slot covering now, for round-trip tests.
func realAndSpike(now time.Time) (*teslaclient.TariffContentV2, teslaclient.TariffContentV2) {
	wholeDay := map[string]teslaclient.TariffSeasonV2{
		"All": {FromMonth: 1, ToMonth: 12, TOUPeriods: map[string]teslaclient.TariffSeasonV2TOUPeriods{
			"OFF_PEAK": {Periods: []teslaclient.TariffTOUPeriod{{ToDayOfWeek: 6}}},
		}},
	}
	real := &teslaclient.TariffContentV2{
		Utility:       "PG&E",
		Name:          "plan-PG&E",
		Seasons:       wholeDay,
		EnergyCharges: map[string]teslaclient.TariffRates{"All": {Rates: map[string]float64{"OFF_PEAK": 0.36}}},
		SellTariff: &teslaclient.TariffSellTariffV2{
			Seasons:       wholeDay,
			EnergyCharges: map[string]teslaclient.TariffRates{"All": {Rates: map[string]float64{"OFF_PEAK": 0.10}}},
		},
	}
	return real, spikeTariff(real, now)
}

// assertSameRates checks that want and got resolve to the same buy and sell rate
// at a spread of times across the day (before, inside, and after the spike window).
func assertSameRates(t *testing.T, want, got *teslaclient.TariffContentV2, day time.Time) {
	t.Helper()
	for _, m := range []int{0, 615, 637, 650, 665, 720, 1439} {
		at := day.Add(time.Duration(m-day.Hour()*60-day.Minute()) * time.Minute)

		wb, _ := rateAtV2(want.Seasons, want.EnergyCharges, at)
		gb, _ := rateAtV2(got.Seasons, got.EnergyCharges, at)
		assert.InDelta(t, wb, gb, 1e-9, "buy rate at minute %d", m)

		require.NotNil(t, got.SellTariff, "reconstructed sell tariff missing")
		ws, _ := rateAtV2(want.SellTariff.Seasons, want.SellTariff.EnergyCharges, at)
		gs, _ := rateAtV2(got.SellTariff.Seasons, got.SellTariff.EnergyCharges, at)
		assert.InDelta(t, ws, gs, 1e-9, "sell rate at minute %d", m)
	}
}

// TestUnspikeTariff verifies the spike is losslessly reversible: unspiking a
// spikeTariff restores the real rates (buy and sell), drops every _SPIKE period,
// and clears the synthetic marker — so effectiveTariff can recover the real
// tariff when no snapshot is held.
func TestUnspikeTariff(t *testing.T) {
	n := time.Now()
	now := time.Date(n.Year(), n.Month(), n.Day(), 10, 30, 0, 0, time.Local)
	real, spike := realAndSpike(now)
	require.True(t, isSyntheticTariff(&spike), "precondition: spike is synthetic")

	got := unspikeTariff(&spike)

	assert.False(t, isSyntheticTariff(got), "unspiked tariff must not be synthetic")
	assertSameRates(t, real, got, now)

	// no _SPIKE period survives on either side
	for _, seasons := range []map[string]teslaclient.TariffSeasonV2{got.Seasons, got.SellTariff.Seasons} {
		for _, s := range seasons {
			for name := range s.TOUPeriods {
				assert.NotContains(t, name, spikeSuffix, "leftover spike period %q", name)
			}
		}
	}

	// the spike input is not mutated
	assert.True(t, isSyntheticTariff(&spike), "unspikeTariff must not mutate its input")
}

// TestRateAtV2 verifies resolving the rate covering a given time.
func TestRateAtV2(t *testing.T) {
	seasons := map[string]teslaclient.TariffSeasonV2{
		"All": {FromMonth: 1, ToMonth: 12, TOUPeriods: map[string]teslaclient.TariffSeasonV2TOUPeriods{
			"OFF_PEAK": {Periods: []teslaclient.TariffTOUPeriod{{ToDayOfWeek: 6}}},
		}},
	}
	charges := map[string]teslaclient.TariffRates{"All": {Rates: map[string]float64{"OFF_PEAK": 0.42}}}

	v, ok := rateAtV2(seasons, charges, time.Now())
	assert.True(t, ok)
	assert.InDelta(t, 0.42, v, 1e-9)

	// empty tariff => not found
	_, ok = rateAtV2(nil, nil, time.Now())
	assert.False(t, ok)
}

// TestCarveWindow verifies a TOU period is split around a minute window.
func TestCarveWindow(t *testing.T) {
	hm := func(p teslaclient.TariffTOUPeriod) [4]int {
		return [4]int{p.FromHour, p.FromMinute, p.ToHour, p.ToMinute}
	}

	// whole-day period (0..0), 15-min window in the middle (10:30-10:45)
	before, mid, after, carved := carveWindow(teslaclient.TariffTOUPeriod{ToDayOfWeek: 6}, 630, 645)
	require.True(t, carved)
	require.Len(t, before, 1)
	assert.Equal(t, [4]int{0, 0, 10, 30}, hm(before[0]))
	require.Len(t, mid, 1)
	assert.Equal(t, [4]int{10, 30, 10, 45}, hm(mid[0]))
	require.Len(t, after, 1)
	assert.Equal(t, [4]int{10, 45, 0, 0}, hm(after[0])) // 00:00 = end of day

	// window at the period start (08:00-08:15): no before
	before, mid, after, carved = carveWindow(teslaclient.TariffTOUPeriod{FromHour: 8, ToHour: 16, ToDayOfWeek: 6}, 480, 495)
	require.True(t, carved)
	assert.Empty(t, before)
	assert.Equal(t, [4]int{8, 0, 8, 15}, hm(mid[0]))
	assert.Equal(t, [4]int{8, 15, 16, 0}, hm(after[0]))

	// period fully outside the window: no carve
	_, _, _, carved = carveWindow(teslaclient.TariffTOUPeriod{FromHour: 0, ToHour: 6, ToDayOfWeek: 6}, 630, 645)
	assert.False(t, carved)
}

func TestFleetAPIFeedinRates(t *testing.T) {
	site := &teslaclient.EnergySite{
		TariffContentV2: &teslaclient.TariffContentV2{
			Seasons: map[string]teslaclient.TariffSeasonV2{
				"Summer": {
					FromMonth: 6, ToMonth: 9,
					TOUPeriods: map[string]teslaclient.TariffSeasonV2TOUPeriods{
						"ON_PEAK":  {Periods: []teslaclient.TariffTOUPeriod{{ToDayOfWeek: 6, FromHour: 16, ToHour: 21}}},
						"OFF_PEAK": {Periods: []teslaclient.TariffTOUPeriod{{ToDayOfWeek: 6, FromHour: 0, ToHour: 16}}},
					},
				},
				"Winter": {
					FromMonth: 10, ToMonth: 5,
					TOUPeriods: map[string]teslaclient.TariffSeasonV2TOUPeriods{
						"OFF_PEAK": {Periods: []teslaclient.TariffTOUPeriod{{ToDayOfWeek: 6}}},
					},
				},
			},
			EnergyCharges: map[string]teslaclient.TariffRates{
				"Summer": {Rates: map[string]float64{"ON_PEAK": 0.579, "OFF_PEAK": 0.361}},
				"Winter": {Rates: map[string]float64{"OFF_PEAK": 0.312}},
			},
			SellTariff: &teslaclient.TariffSellTariffV2{
				Seasons: map[string]teslaclient.TariffSeasonV2{
					"All": {
						FromMonth: 1, ToMonth: 12,
						TOUPeriods: map[string]teslaclient.TariffSeasonV2TOUPeriods{
							"hour_0_all": {Periods: []teslaclient.TariffTOUPeriod{{ToDayOfWeek: 6, FromHour: 0, ToHour: 1}}},
							"hour_1_all": {Periods: []teslaclient.TariffTOUPeriod{{ToDayOfWeek: 6, FromHour: 1, ToHour: 2}}},
						},
					},
				},
				EnergyCharges: map[string]teslaclient.TariffRates{
					"All": {Rates: map[string]float64{"hour_0_all": 0.078, "hour_1_all": 0.077}},
				},
			},
		},
	}

	m := &FleetAPI{
		usage: "feedin",
		provider: &fleetAPIProvider{
			siteInfoG: func() (*teslaclient.EnergySite, error) { return site, nil },
		},
	}

	rates, err := m.Rates()
	require.NoError(t, err)
	require.NotEmpty(t, rates, "feedin rates should use sell_tariff.seasons, not main tariff seasons")

	for _, r := range rates {
		assert.Equal(t, time.Hour, r.End.Sub(r.Start))
	}
	assert.Len(t, rates, 14) // 7 days * 2 periods/day
}

func TestFleetAPIImportRates(t *testing.T) {
	site := &teslaclient.EnergySite{
		TariffContentV2: &teslaclient.TariffContentV2{
			Seasons: map[string]teslaclient.TariffSeasonV2{
				"All": {
					FromMonth: 1, ToMonth: 12,
					TOUPeriods: map[string]teslaclient.TariffSeasonV2TOUPeriods{
						"OFF_PEAK": {Periods: []teslaclient.TariffTOUPeriod{{ToDayOfWeek: 6}}},
					},
				},
			},
			EnergyCharges: map[string]teslaclient.TariffRates{
				"All": {Rates: map[string]float64{"OFF_PEAK": 0.312}},
			},
		},
	}

	m := &FleetAPI{
		usage: "grid",
		provider: &fleetAPIProvider{
			siteInfoG: func() (*teslaclient.EnergySite, error) { return site, nil },
		},
	}

	rates, err := m.Rates()
	require.NoError(t, err)
	require.NotEmpty(t, rates)
	assert.Len(t, rates, 7) // 7 days * 1 full-day period
}

func TestFleetAPICurrentPower(t *testing.T) {
	status := &teslaclient.EnergySiteLiveStatus{
		GridPower:    1500,
		SolarPower:   3200,
		BatteryPower: -800,
		LoadPower:    3900,
	}

	tests := []struct {
		usage string
		want  float64
	}{
		{"site", 1500},
		{"solar", 3200},
		{"battery", -800},
		{"load", 3900},
	}

	for _, tc := range tests {
		t.Run(tc.usage, func(t *testing.T) {
			m := &FleetAPI{
				usage: tc.usage,
				provider: &fleetAPIProvider{
					liveStatusG: func() (*teslaclient.EnergySiteLiveStatus, error) { return status, nil },
				},
			}
			power, err := m.CurrentPower()
			require.NoError(t, err)
			assert.Equal(t, tc.want, power)
		})
	}
}

func TestFleetAPIBatterySoc(t *testing.T) {
	m := &FleetAPI{
		usage: "battery",
		provider: &fleetAPIProvider{
			liveStatusG: func() (*teslaclient.EnergySiteLiveStatus, error) {
				return &teslaclient.EnergySiteLiveStatus{PercentageCharged: 72.5}, nil
			},
		},
	}

	soc, err := m.batterySoc()
	require.NoError(t, err)
	assert.Equal(t, 72.5, soc)
}

type mockEnergySite struct {
	mode         string
	reserve      uint64
	gridCharging bool
	modeCalls    int
	reserveCalls int
	gridChgCalls int
}

func (s *mockEnergySite) SetOperatingMode(mode string) error {
	s.mode = mode
	s.modeCalls++
	return nil
}

func (s *mockEnergySite) SetBatteryReserve(percent uint64) error {
	s.reserve = percent
	s.reserveCalls++
	return nil
}

func (s *mockEnergySite) SetGridCharging(enabled bool) error {
	s.gridCharging = enabled
	s.gridChgCalls++
	return nil
}

func TestFleetAPIBatteryController(t *testing.T) {
	tests := []struct {
		name            string
		mode            api.BatteryMode
		soc             float64
		allowGridImport bool
		wantMode        string
		wantReserve     uint64
		wantGridChg     bool
	}{
		{
			name:        "normal sets self_consumption and min reserve",
			mode:        api.BatteryNormal,
			wantMode:    "self_consumption",
			wantReserve: 20,
			wantGridChg: false,
		},
		{
			name:        "hold sets backup and current SOC reserve",
			mode:        api.BatteryHold,
			soc:         65,
			wantMode:    "backup",
			wantReserve: 65,
			wantGridChg: false,
		},
		{
			name:        "hold clamps to min SOC when below",
			mode:        api.BatteryHold,
			soc:         10,
			wantMode:    "backup",
			wantReserve: 20,
			wantGridChg: false,
		},
		{
			name:        "charge without allowGridImport keeps grid charging off",
			mode:        api.BatteryCharge,
			wantMode:    "self_consumption",
			wantReserve: 100,
			wantGridChg: false,
		},
		{
			name:            "charge with allowGridImport enables grid charging",
			mode:            api.BatteryCharge,
			allowGridImport: true,
			wantMode:        "self_consumption",
			wantReserve:     100,
			wantGridChg:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockEnergySite{}

			m := &FleetAPI{
				usage:           "battery",
				allowGridImport: tc.allowGridImport,
				provider: &fleetAPIProvider{
					site: &teslaclient.EnergySite{},
					liveStatusG: func() (*teslaclient.EnergySiteLiveStatus, error) {
						return &teslaclient.EnergySiteLiveStatus{PercentageCharged: tc.soc}, nil
					},
				},
			}

			limits := batterySocLimits{MinSoc: 20, MaxSoc: 100}
			// Build the controller but intercept the site commands via mock
			controller := func(mode api.BatteryMode) error {
				switch mode {
				case api.BatteryNormal:
					if err := mock.SetOperatingMode("self_consumption"); err != nil {
						return err
					}
					if err := mock.SetGridCharging(false); err != nil {
						return err
					}
					return mock.SetBatteryReserve(uint64(limits.MinSoc))

				case api.BatteryHold:
					soc, err := m.batterySoc()
					if err != nil {
						return err
					}
					reserve := min(100.0, max(soc, limits.MinSoc))
					if err := mock.SetOperatingMode("backup"); err != nil {
						return err
					}
					if err := mock.SetGridCharging(false); err != nil {
						return err
					}
					return mock.SetBatteryReserve(uint64(reserve))

				case api.BatteryCharge:
					if err := mock.SetOperatingMode("self_consumption"); err != nil {
						return err
					}
					// grid charging is opt-in via allowGridImport
					if err := mock.SetGridCharging(m.allowGridImport); err != nil {
						return err
					}
					return mock.SetBatteryReserve(uint64(limits.MaxSoc))

				default:
					return api.ErrNotAvailable
				}
			}

			err := controller(tc.mode)
			require.NoError(t, err)
			assert.Equal(t, tc.wantMode, mock.mode)
			assert.Equal(t, tc.wantReserve, mock.reserve)
			assert.Equal(t, tc.wantGridChg, mock.gridCharging)
		})
	}
}

func TestFleetAPIBatteryControllerUnsupported(t *testing.T) {
	m := &FleetAPI{
		usage:     "battery",
		socLimits: batterySocLimits{MinSoc: 20, MaxSoc: 100},
		provider: &fleetAPIProvider{
			site: &teslaclient.EnergySite{},
		},
	}

	controller := m.newBatteryController()
	err := controller(api.BatteryMode(99))
	assert.ErrorIs(t, err, api.ErrNotAvailable)
}

// TestDischargeReserve verifies the grid-discharge reserve tracks the optimizer's
// target SoC (never below min soc), falling back to min soc without a target.
func TestDischargeReserve(t *testing.T) {
	assert.Equal(t, 55.0, dischargeReserve(55, 20), "target above min soc is honored")
	assert.Equal(t, 20.0, dischargeReserve(10, 20), "target below min soc clamps to min soc")
	assert.Equal(t, 20.0, dischargeReserve(0, 20), "no target falls back to min soc")
	assert.Equal(t, 20.0, dischargeReserve(-5, 20), "negative target falls back to min soc")
}

// TestVPPReserve verifies the VPP event reserve tracks the optimizer's discharge
// floor: the target SoC floored at MinSoc (falling back to MinSoc with no target),
// snapped by clampReserve to a value the Powerwall accepts.
func TestVPPReserve(t *testing.T) {
	assert.Equal(t, uint64(20), vppReserve(0, 20), "no target → MinSoc floor")
	assert.Equal(t, uint64(20), vppReserve(10, 20), "target below MinSoc → MinSoc")
	assert.Equal(t, uint64(55), vppReserve(55, 20), "target above MinSoc is honored")
	assert.Equal(t, uint64(80), vppReserve(90, 20), "target in the PW3-rejected (80,100) band snaps to 80")
	assert.Equal(t, uint64(100), vppReserve(100, 20), "100 is allowed")
}

// TestHoldChargeReserve verifies hold-charge sits the reserve a small margin
// below the current SoC (so a PV dip is covered from the battery, not the grid),
// never below min soc. With clampReserve, an SoC in the PW3-rejected (80,100)
// band widens the margin to the 80 floor.
func TestHoldChargeReserve(t *testing.T) {
	assert.Equal(t, 55.0, holdChargeReserve(60, 20), "a margin below the current soc")
	assert.Equal(t, 20.0, holdChargeReserve(22, 20), "clamped to min soc when within the margin")
	assert.Equal(t, 20.0, holdChargeReserve(20, 20), "never below min soc")

	// combined with clampReserve: an soc in (80,100) snaps the reserve to 80
	assert.Equal(t, uint64(80), clampReserve(holdChargeReserve(90, 20)))
	assert.Equal(t, uint64(55), clampReserve(holdChargeReserve(60, 20)))
}

func TestFleetAPITariffType(t *testing.T) {
	tests := []struct {
		name     string
		site     *teslaclient.EnergySite
		wantType api.TariffType
	}{
		{
			name: "single TOU period is static",
			site: &teslaclient.EnergySite{
				TariffContentV2: &teslaclient.TariffContentV2{
					Seasons: map[string]teslaclient.TariffSeasonV2{
						"All": {TOUPeriods: map[string]teslaclient.TariffSeasonV2TOUPeriods{
							"OFF_PEAK": {},
						}},
					},
				},
			},
			wantType: api.TariffTypePriceStatic,
		},
		{
			name: "multiple TOU periods is dynamic",
			site: &teslaclient.EnergySite{
				TariffContentV2: &teslaclient.TariffContentV2{
					Seasons: map[string]teslaclient.TariffSeasonV2{
						"Summer": {TOUPeriods: map[string]teslaclient.TariffSeasonV2TOUPeriods{
							"ON_PEAK":  {},
							"OFF_PEAK": {},
						}},
					},
				},
			},
			wantType: api.TariffTypePriceDynamic,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &FleetAPI{
				provider: &fleetAPIProvider{
					siteInfoG: func() (*teslaclient.EnergySite, error) { return tc.site, nil },
				},
			}
			assert.Equal(t, tc.wantType, m.Type())
		})
	}
}

func tariffWith(utility string) *teslaclient.TariffContentV2 {
	return &teslaclient.TariffContentV2{Utility: utility, Name: "plan-" + utility}
}

// TestIsSyntheticTariff verifies detection of evcc-written (synthetic) tariffs
// vs. the user's real utility tariff — the basis of the reboot safeguard.
func TestIsSyntheticTariff(t *testing.T) {
	assert.True(t, isSyntheticTariff(tariffWith(syntheticTariffUtility)), "EVCC-tagged tariff is synthetic")
	assert.False(t, isSyntheticTariff(tariffWith("PG&E")), "a real utility tariff is not synthetic")
	assert.False(t, isSyntheticTariff(nil), "nil tariff is not synthetic")
}

// TestTariffGuard covers snapshot/restore and, critically, the safeguards that
// stop a synthetic tariff from ever being recorded or restored as the user's
// real one (which a restart mid-discharge could otherwise cause).
func TestTariffGuard(t *testing.T) {
	real := tariffWith("PG&E")
	synthetic := tariffWith(syntheticTariffUtility)

	t.Run("snapshot is readable and never dropped", func(t *testing.T) {
		g := newTariffGuard("") // empty key => no persistence
		g.remember(real)
		assert.Equal(t, real, g.peek(), "peek returns the snapshot without clearing it")
		assert.Equal(t, real, g.peek(), "peek is non-destructive; the snapshot is never dropped")
	})

	t.Run("never snapshot a synthetic tariff as real", func(t *testing.T) {
		g := newTariffGuard("")
		g.remember(synthetic)
		assert.Nil(t, g.peek(), "a synthetic tariff must not be recorded as the real one")
	})

	t.Run("keep the real tariff across a later synthetic read", func(t *testing.T) {
		// mirrors reboot mid-discharge: real tariff already held, then the live
		// read returns our synthetic tariff — the real one must survive.
		g := newTariffGuard("")
		g.remember(real)
		g.remember(synthetic)
		assert.Equal(t, real, g.peek(), "must keep the real tariff, not overwrite with synthetic")
	})

	t.Run("remember refreshes to the latest real tariff", func(t *testing.T) {
		// a plan switch / annual rate revision shows up as a new real tariff; the
		// snapshot must track it (not pin the first one forever).
		g := newTariffGuard("")
		g.remember(real)
		sce := tariffWith("SCE")
		g.remember(sce)
		assert.Equal(t, sce, g.peek(), "a real change must refresh the snapshot")
	})

	t.Run("nil current is ignored", func(t *testing.T) {
		g := newTariffGuard("")
		g.remember(nil)
		assert.Nil(t, g.peek())
	})
}

// TestEffectiveTariff verifies that while a synthetic export-forcing tariff is
// active, the meter reports the snapshotted REAL tariff — preventing our own
// synthetic prices from feeding back into evcc's tariff/optimizer inputs.
func TestEffectiveTariff(t *testing.T) {
	real := tariffWith("PG&E")
	synthetic := tariffWith(syntheticTariffUtility)

	// live tariff is synthetic + a real one is snapshotted -> report the real one
	guard := newTariffGuard("")
	guard.remember(real)
	m := &FleetAPI{provider: &fleetAPIProvider{guard: guard}}
	assert.Equal(t, real, m.effectiveTariff(&teslaclient.EnergySite{TariffContentV2: synthetic}),
		"synthetic live tariff must be reported as the snapshotted real tariff")

	// live tariff is real -> report it unchanged
	live := tariffWith("SCE")
	assert.Equal(t, live, m.effectiveTariff(&teslaclient.EnergySite{TariffContentV2: live}))

	// a real live read also refreshes the guard so we always hold the latest real
	// tariff to restore (a plan change is captured without extra API calls)
	freshGuard := newTariffGuard("")
	m3 := &FleetAPI{provider: &fleetAPIProvider{guard: freshGuard}}
	live2 := tariffWith("PG&E-2027")
	assert.Equal(t, live2, m3.effectiveTariff(&teslaclient.EnergySite{TariffContentV2: live2}))
	assert.Equal(t, live2, freshGuard.peek(), "a real live read must refresh the guard snapshot")

	// synthetic live tariff but nothing snapshotted -> reconstruct the real rates
	// from the spike rather than leaking the bumped prices into the optimizer/UI.
	n := time.Now()
	now := time.Date(n.Year(), n.Month(), n.Day(), 10, 30, 0, 0, time.Local)
	realFull, spike := realAndSpike(now)
	m2 := &FleetAPI{provider: &fleetAPIProvider{guard: newTariffGuard("")}}
	got := m2.effectiveTariff(&teslaclient.EnergySite{TariffContentV2: &spike})
	assert.False(t, isSyntheticTariff(got), "reconstructed tariff must not be reported as synthetic")
	assertSameRates(t, realFull, got, now)
}
