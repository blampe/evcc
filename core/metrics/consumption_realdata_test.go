//go:build realdata

package metrics

import (
	"database/sql"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/tariff"
	"github.com/stretchr/testify/require"
)

// TestRealDataConsumption backtests two ways to predict the energy a vehicle uses while
// away - so the optimizer could start reconnect charging from a realistic SoC instead of
// assuming no drain - against today's behavior (assume flat SoC). Consumption is derived
// from odometer deltas between consecutive sessions; SoC is too sparse in real data. Run:
//
//	go test ./core/metrics -tags realdata -run TestRealDataConsumption -v
func TestRealDataConsumption(t *testing.T) {
	realDB(t)

	// EQB 350: ~66.5 kWh usable, ~191 Wh/km -> ~0.29 SoC points per km driven, used to
	// fold odometer distance into the same unit as a direct SoC drop.
	const socPerKm = 191.0 / 66500 * 100

	// away-episodes from CONSECUTIVE sessions (departure = previous disconnect, return =
	// next connect). Pairing over the full list - not an odometer-filtered one - never
	// merges away-periods across a session that lacks a reading. Consumption is in SoC
	// points: the direct SoC drop where both endpoints have it, else the odometer
	// distance, so neither signal is dropped.
	database, err := db.Instance.DB()
	require.NoError(t, err)
	rows, err := database.Query(`SELECT created, finished, soc_start, soc_end, odometer, charged_kwh
		FROM sessions WHERE created > '2020' AND finished > '2020' ORDER BY created ASC`)
	require.NoError(t, err)
	defer rows.Close()

	type sess struct {
		created, finished              time.Time
		socStart, socEnd, odo, charged sql.NullFloat64
	}
	var ss []sess
	for rows.Next() {
		var s sess
		require.NoError(t, rows.Scan(&s.created, &s.finished, &s.socStart, &s.socEnd, &s.odo, &s.charged))
		ss = append(ss, s)
	}

	// calibrate SoC gained per kWh charged from clean positive rises (excludes the stale-
	// reconnect glitches, where SocEnd reads below the stale SocStart)
	socPerKwh := 90.0 / 66.5 // nominal fallback (~90% charge efficiency over usable capacity)
	var ratios []float64
	for _, s := range ss {
		if s.socStart.Valid && s.socEnd.Valid && s.charged.Valid && s.charged.Float64 > 1 {
			if rise := s.socEnd.Float64 - s.socStart.Float64; rise > 2 {
				ratios = append(ratios, rise/s.charged.Float64)
			}
		}
	}
	if len(ratios) > 0 {
		slices.Sort(ratios)
		socPerKwh = ratios[len(ratios)/2] // median SoC%/kWh
	}

	var eps []AwayEpisode
	var fromSoc, fromOdo int
	for i := 1; i < len(ss); i++ {
		prev, cur := ss[i-1], ss[i]
		dep, ret := prev.finished, cur.created
		if away := ret.Sub(dep); away <= 0 || away > 36*time.Hour {
			continue // implausible single away-period (multi-day gap = trip / charged elsewhere)
		}
		// drain from signals immune to the stale reconnect SoC: odometer where it still
		// moves, else the energy balance - the real return SoC is min(SocStart, SocEnd -
		// charged), so a stale-high SocStart is discarded but a genuinely fresh low one wins.
		var consumed float64
		switch {
		case prev.odo.Valid && cur.odo.Valid && cur.odo.Float64 > prev.odo.Float64:
			consumed = (cur.odo.Float64 - prev.odo.Float64) * socPerKm // odometer distance
			fromOdo++
		case prev.socEnd.Valid && cur.socEnd.Valid && cur.charged.Valid:
			reconnect := cur.socEnd.Float64 - cur.charged.Float64*socPerKwh
			if cur.socStart.Valid {
				reconnect = min(reconnect, cur.socStart.Float64)
			}
			consumed = prev.socEnd.Float64 - reconnect
			fromSoc++
		}
		if consumed > 0 && consumed <= 50 { // drop no-drives and implausible drains (spurious)
			eps = append(eps, AwayEpisode{Departs: dep, Returns: ret, Consumed: consumed})
		}
	}
	require.NotEmpty(t, eps)
	t.Logf("episodes: %d (%d from SoC, %d from odometer); consumption in SoC points", len(eps), fromSoc, fromOdo)

	// hold out the most recent quarter of episodes as the test set
	split := len(eps) * 3 / 4
	train, test := eps[:split], eps[split:]
	require.NotEmpty(t, train)
	require.NotEmpty(t, test)

	// per-slot consumption series over the training period, then fit both models
	start := train[0].Departs.Truncate(tariff.SlotDuration)
	n := int(test[0].Departs.Sub(start)/tariff.SlotDuration) + 1
	series := consumptionSeries(train, start, n)

	seasonal, err := recencySeasonal(start, series, seasonalHalflife)
	require.NoError(t, err)
	hf, err := newHybridForecast(start, series)
	require.NoError(t, err)

	var globalMean float64
	for _, ep := range train {
		globalMean += ep.Consumed
	}
	globalMean /= float64(len(train))

	rmse := func(pred func(AwayEpisode) float64) float64 {
		var se float64
		for _, ep := range test {
			d := pred(ep) - ep.Consumed
			se += d * d
		}
		return math.Sqrt(se / float64(len(test)))
	}

	// explicit day-of-week × time-of-day seasonal, as a reference: if this beats the plain
	// time-of-day seasonal, the weekday/weekend signal is genuinely predictive
	dow := dowTodSeasonal(series, start, seasonalHalflife)

	isWeekday := func(ep AwayEpisode) bool {
		d := ep.Departs.Weekday()
		return d >= time.Monday && d <= time.Friday
	}
	rmseIf := func(pred func(AwayEpisode) float64, keep func(AwayEpisode) bool) float64 {
		var se float64
		var m int
		for _, ep := range test {
			if !keep(ep) {
				continue
			}
			d := pred(ep) - ep.Consumed
			se += d * d
			m++
		}
		if m == 0 {
			return 0
		}
		return math.Sqrt(se / float64(m))
	}

	cond := awayRateSeasonal(series, start, seasonalHalflife)

	// episode-level: recency-weighted average of the episode TOTAL drain, bucketed by
	// departure time-of-day. This sidesteps the per-slot-rate/duration artifact entirely
	// - it predicts the total directly instead of integrating a rate over the window.
	var depSum, depW [96]float64
	lastDep := train[len(train)-1].Departs
	hlDays := seasonalHalflife.Hours() / 24
	for _, ep := range train {
		k := todIndex(ep.Departs)
		w := math.Exp(-math.Ln2 * lastDep.Sub(ep.Departs).Hours() / 24 / hlDays)
		depSum[k] += w * ep.Consumed
		depW[k] += w
	}
	depPred := func(ep AwayEpisode) float64 {
		if k := todIndex(ep.Departs); depW[k] > 0 {
			return depSum[k] / depW[k]
		}
		return globalMean
	}

	// rate model: drop = SoC/hour × away-hours. Duration is the dominant predictor, so
	// forecast the intensity (rate) and multiply by the known duration.
	hours := func(ep AwayEpisode) float64 { return ep.Returns.Sub(ep.Departs).Hours() }
	weight := func(ep AwayEpisode) float64 {
		return math.Exp(-math.Ln2 * lastDep.Sub(ep.Departs).Hours() / 24 / hlDays)
	}

	var rSum, rW float64
	var rtSum, rtW [96]float64
	var rWd, rWe, wWd, wWe float64
	for _, ep := range train {
		w, r := weight(ep), weight(ep)*ep.Consumed/hours(ep)
		rSum += r
		rW += w
		k := todIndex(ep.Departs)
		rtSum[k] += r
		rtW[k] += w
		if isWeekday(ep) {
			rWd += r
			wWd += w
		} else {
			rWe += r
			wWe += w
		}
	}
	rateGlobal := rSum / rW
	rateGlobalPred := func(ep AwayEpisode) float64 { return rateGlobal * hours(ep) }
	rateTodPred := func(ep AwayEpisode) float64 {
		if k := todIndex(ep.Departs); rtW[k] > 0 {
			return rtSum[k] / rtW[k] * hours(ep)
		}
		return rateGlobal * hours(ep)
	}
	rateDowPred := func(ep AwayEpisode) float64 {
		if isWeekday(ep) {
			return rWd / wWd * hours(ep)
		}
		return rWe / wWe * hours(ep)
	}

	// clean 2-way weekday/weekend split (robust vs the 7-way dow×tod that overfit):
	// (a) average total drain by day type, (b) a time-of-day seasonal per day type.
	isWeekdayTime := func(t time.Time) bool { d := t.Weekday(); return d >= time.Monday && d <= time.Friday }
	var awd, awdN, awe, aweN float64
	var wdSum, wdW, weSum, weW [96]float64
	for _, ep := range train {
		w := weight(ep)
		if isWeekday(ep) {
			awd += w * ep.Consumed
			awdN += w
		} else {
			awe += w * ep.Consumed
			aweN += w
		}
	}
	for i, v := range series {
		t := start.Add(time.Duration(i) * tariff.SlotDuration)
		k := todIndex(t)
		w := math.Exp(-math.Ln2 * float64(len(series)-1-i) / 96 / hlDays)
		if isWeekdayTime(t) {
			wdSum[k] += w * v
			wdW[k] += w
		} else {
			weSum[k] += w * v
			weW[k] += w
		}
	}
	var wdSeas, weSeas [96]float64
	for k := range wdSeas {
		if wdW[k] > 0 {
			wdSeas[k] = wdSum[k] / wdW[k]
		}
		if weW[k] > 0 {
			weSeas[k] = weSum[k] / weW[k]
		}
	}
	avg2Pred := func(ep AwayEpisode) float64 {
		if isWeekday(ep) {
			return awd / awdN
		}
		return awe / aweN
	}
	daytypePred := func(ep AwayEpisode) float64 {
		return awaySum(ep, func(t time.Time) float64 {
			if isWeekdayTime(t) {
				return wdSeas[todIndex(t)]
			}
			return weSeas[todIndex(t)]
		})
	}

	seasonalPred := func(ep AwayEpisode) float64 { return seasonalConsumption(seasonal, ep) }
	condPred := func(ep AwayEpisode) float64 { return seasonalConsumption(cond, ep) }
	dowPred := func(ep AwayEpisode) float64 { return dowSeasonalConsumption(dow, ep) }
	hybridPred := func(ep AwayEpisode) float64 { return hybridConsumption(hf, ep) }

	t.Logf("consumption backtest: %d train + %d test episodes (last quarter), avg drain %.1f SoC points", len(train), len(test), globalMean)
	t.Logf("hybrid residual cycles kept: %v", hf.residual.Periods())
	report := func(name string, pred func(AwayEpisode) float64) {
		all, wd, we := rmse(pred), rmseIf(pred, isWeekday), rmseIf(pred, func(ep AwayEpisode) bool { return !isWeekday(ep) })
		t.Logf("  %-24s RMSE = %4.1f SoC   weekday %4.1f  weekend %4.1f", name, all, wd, we)
	}
	report("do-nothing (today)", func(AwayEpisode) float64 { return 0 })
	report("global mean", func(AwayEpisode) float64 { return globalMean })
	report("phase 1: tod seasonal (diluted)", seasonalPred)
	report("phase 1c: away-only (0s excluded)", condPred)
	report("wknd/wkdy: avg by type", avg2Pred)
	report("wknd/wkdy: x tod seasonal", daytypePred)
	report("phase 2: hybrid", hybridPred)
	_ = depPred
	_ = dowPred
	_ = rateGlobalPred
	_ = rateTodPred
	_ = rateDowPred

	// magnitude check: predicted drain for a standard weekday 08:00-18:00 commute window.
	// The diluted seasonal averages in the home/stale-0 days (under-predicts a known-away
	// window); the away-only seasonal excludes them and recovers the commute magnitude.
	loc := train[len(train)-1].Departs.Location()
	probeDay := time.Date(2026, 8, 31, 0, 0, 0, 0, loc)
	probe := AwayEpisode{Departs: probeDay.Add(8 * time.Hour), Returns: probeDay.Add(18 * time.Hour)}
	t.Logf("08:00-18:00 commute prediction: diluted=%.1f  away-only=%.1f SoC", seasonalPred(probe), condPred(probe))
}

// dowTodSeasonal is a recency-weighted average keyed by (day-of-week, time-of-day).
func dowTodSeasonal(series []float64, start time.Time, halflife time.Duration) [7][96]float64 {
	var wsum, wtot [7][96]float64
	n := len(series)
	hlDays := halflife.Hours() / 24
	for i, v := range series {
		t := start.Add(time.Duration(i) * tariff.SlotDuration)
		w := math.Exp(-math.Ln2 * float64(n-1-i) / 96 / hlDays)
		wsum[int(t.Weekday())][todIndex(t)] += w * v
		wtot[int(t.Weekday())][todIndex(t)] += w
	}
	var s [7][96]float64
	for d := range 7 {
		for k := range 96 {
			if wtot[d][k] > 0 {
				s[d][k] = wsum[d][k] / wtot[d][k]
			}
		}
	}
	return s
}

func dowSeasonalConsumption(s [7][96]float64, ep AwayEpisode) float64 {
	var sum float64
	for t := ep.Departs.Truncate(tariff.SlotDuration); t.Before(ep.Returns); t = t.Add(tariff.SlotDuration) {
		sum += s[int(t.Weekday())][todIndex(t)]
	}
	return sum
}
