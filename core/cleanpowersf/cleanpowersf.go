// Package cleanpowersf holds CleanPowerSF's (San Francisco CCA) published
// generation rates and NEM parameters as a single, effective-dated source of
// truth. CleanPowerSF supplies the generation half of a customer's electricity
// (PG&E handles delivery); it publishes rates only as HTML, so the values below
// are hand-transcribed from its rate pages and verified against a customer bill.
//
// Under CleanPowerSF's Net Energy Metering, exported generation is credited at
// these same retail time-of-use rates (import and export net at the same rate),
// with any annual net surplus cashed out at the Net Surplus Compensation rate.
// This is why a CleanPowerSF customer's generation export is worth retail (not
// PG&E's ACC), and its floor is NSC (not zero).
//
// Updating for a rate change: add a newer entry (with its effective date) to the
// relevant table and rebuild. Lookups pick the latest set effective at or before
// the queried instant.
package cleanpowersf

import "time"

// TrueUpMonth is CleanPowerSF's NEM annual true-up anniversary: the twelve-month
// period commences in May (per cleanpowersf.org/nem-tariff, Definitions). This is
// distinct from PG&E's delivery (NBT) true-up, which follows the customer's PG&E
// anniversary.
const TrueUpMonth = time.May

// summerMonths is CleanPowerSF's summer season: June 1 - September 30.
func isSummer(m time.Month) bool { return m >= time.June && m <= time.September }

// touRates are the time-of-use generation rates in $/kWh. EELEC periods (local):
// peak 16:00-21:00, part-peak 15:00-16:00 & 21:00-24:00, off-peak otherwise.
type touRates struct{ peak, partPeak, offPeak float64 }

func (r touRates) at(hour int) float64 {
	switch {
	case hour >= 16 && hour < 21:
		return r.peak
	case hour == 15 || hour >= 21:
		return r.partPeak
	default:
		return r.offPeak
	}
}

type genRateSet struct {
	effective      time.Time
	summer, winter touRates
	source, note   string
}

// eelecGeneration: E-ELEC (Electric Home) generation rates, newest last.
// Source: https://cleanpowersf.org/residential-rate-tables
var eelecGeneration = []genRateSet{
	{
		effective: date(2026, time.March, 1),
		summer:    touRates{peak: 0.24327, partPeak: 0.15767, offPeak: 0.11871},
		winter:    touRates{peak: 0.10323, partPeak: 0.08598, offPeak: 0.07445},
		source:    "https://cleanpowersf.org/residential-rate-tables",
		note:      "SFPUC-approved ~20-25% generation cut effective 2026-03-01; summer peak verified to the cent against a Jul-Aug 2026 bill.",
	},
}

type nscRate struct {
	effective    time.Time
	rate         float64
	fiscalYear   string
	source, note string
}

// netSurplus: Net Surplus Compensation, fiscal-year-dated, newest last.
// Source: https://cleanpowersf.org/nem-tariff
var netSurplus = []nscRate{
	{
		effective:  date(2024, time.May, 1),
		rate:       0.0893,
		fiscalYear: "FY24-25",
		source:     "https://cleanpowersf.org/nem-tariff",
		note:       "Published tariff value. The Jun-2026 SFPUC Rate Fairness Board deck cites $0.0983 for current NEM; verify the current fiscal year. The May-2027 Solar Billing Plan proposal would eliminate NSC.",
	},
}

// laLocation anchors the TOU/season classification to CleanPowerSF's service
// territory so a queried instant maps to the correct local period across DST.
var laLocation = mustLoad("America/Los_Angeles")

// GenerationRate returns the E-ELEC retail generation rate ($/kWh) in effect at t.
func GenerationRate(t time.Time) float64 {
	lt := t.In(laLocation)
	set := latestGen(lt)
	r := set.winter
	if isSummer(lt.Month()) {
		r = set.summer
	}
	return r.at(lt.Hour())
}

// NetSurplusCompensation returns the NSC floor ($/kWh) in effect at t: the value
// an exported kWh's generation half is worth once a net exporter's credits have
// saturated their charges for the true-up year.
func NetSurplusCompensation(t time.Time) float64 {
	lt := t.In(laLocation)
	best := netSurplus[0]
	for _, n := range netSurplus {
		if !n.effective.After(lt) && n.effective.After(best.effective) {
			best = n
		}
	}
	return best.rate
}

func latestGen(t time.Time) genRateSet {
	best := eelecGeneration[0]
	for _, s := range eelecGeneration {
		if !s.effective.After(t) && s.effective.After(best.effective) {
			best = s
		}
	}
	return best
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, laLocation)
}

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}
