package core

import (
	"database/sql"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/metrics"
	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/tariff"
	optimizer "github.com/evcc-io/optimizer/client"
	"github.com/jinzhu/now"
)

const (
	// consumptionDays bounds the driving history the forecast is fit from; the seasonal
	// recency-weights recent weeks anyway, this just caps the query.
	consumptionDays = 365

	// maxAwayWindow is the longest gap still treated as a single commute-style away-period;
	// beyond it a gap is a multi-day trip or charged-elsewhere and is out of model.
	maxAwayWindow = 36 * time.Hour

	// maxRealDrain caps a plausible single away-period drain in SoC points; above it is a
	// road trip or a glitch.
	maxRealDrain = 50

	// defaultWhPerKm folds odometer distance into SoC when no per-vehicle value is known.
	defaultWhPerKm = 180.0

	// chargeEfficiency is the share of grid kWh that reaches the battery, used to convert
	// charged energy into a SoC gain when calibration data is thin.
	chargeEfficiency = 0.9

	// minCalibration is the fewest samples needed to trust a measured conversion factor.
	minCalibration = 5
)

// consumptionEntry caches a loadpoint's driving forecast, refit at most once per day.
type consumptionEntry struct {
	forecast *metrics.ConsumptionForecast
	day      time.Time
}

// sessionRow is the subset of a charging session the driving forecast reads. SocStart is
// read only to calibrate charging efficiency, never to measure the away-drain: the value
// captured at reconnect is often the stale pre-drive SoC (see sessionStartSoc), so the
// drain is derived from odometer distance or the SocEnd/charged energy balance instead.
type sessionRow struct {
	Created  time.Time
	Finished time.Time
	SocStart sql.NullFloat64
	SocEnd   sql.NullFloat64
	Odometer sql.NullFloat64
	Charged  sql.NullFloat64 `gorm:"column:charged_kwh"`
}

// vehicleSessions loads a loadpoint's completed charging sessions since the cutoff,
// oldest first, or nil when there is no session store.
func vehicleSessions(title string, since time.Time) []sessionRow {
	if db.Instance == nil {
		return nil
	}
	var rows []sessionRow
	db.Instance.Table("sessions").
		Where("loadpoint = ? AND created > ? AND finished > ?", title, since, since).
		Order("created ASC").
		Scan(&rows)
	return rows
}

// awayDrain returns the SoC a vehicle consumed over an away-period using signals immune
// to the stale reconnect reading. Odometer distance is used where present (older data);
// otherwise the energy balance: the true return SoC is the session-end SoC minus what was
// then charged, so drain = prev.SocEnd - (cur.SocEnd - cur.charged). SocStart is never
// used. Returns false when no reliable signal is available.
func awayDrain(prev, cur sessionRow, socPerKm, socPerKwh float64) (float64, bool) {
	switch {
	case socPerKm > 0 && prev.Odometer.Valid && cur.Odometer.Valid && cur.Odometer.Float64 > prev.Odometer.Float64:
		return (cur.Odometer.Float64 - prev.Odometer.Float64) * socPerKm, true
	case prev.SocEnd.Valid && cur.SocEnd.Valid && cur.Charged.Valid:
		// real return SoC = the low point of the return session: the energy-balance estimate
		// (session end minus what was then charged), or a fresh SocStart if it read lower. A
		// stale SocStart reads high and is discarded by the min, so this is not relied on.
		reconnect := cur.SocEnd.Float64 - cur.Charged.Float64*socPerKwh
		if cur.SocStart.Valid {
			reconnect = min(reconnect, cur.SocStart.Float64)
		}
		return prev.SocEnd.Float64 - reconnect, true
	}
	return 0, false
}

// awayEpisodes turns consecutive sessions into away-episodes (departure = previous
// disconnect, return = next connect) with the SoC consumed while gone. Zero, negative and
// implausible drains are dropped as spurious (no drive, charged elsewhere, or a glitch).
func awayEpisodes(rows []sessionRow, socPerKm, socPerKwh float64) []metrics.AwayEpisode {
	var eps []metrics.AwayEpisode
	for i := 1; i < len(rows); i++ {
		dep, ret := rows[i-1].Finished, rows[i].Created
		if away := ret.Sub(dep); away <= 0 || away > maxAwayWindow {
			continue // implausible single away-period (multi-day gap = trip / charged elsewhere)
		}
		if consumed, ok := awayDrain(rows[i-1], rows[i], socPerKm, socPerKwh); ok && consumed > 0 && consumed <= maxRealDrain {
			eps = append(eps, metrics.AwayEpisode{Departs: dep, Returns: ret, Consumed: consumed})
		}
	}
	return eps
}

// calibrateSocPerKwh estimates the SoC percent gained per kWh charged, from sessions with
// a clean positive SoC rise - which excludes the stale-reconnect glitches, where SocEnd
// reads below the stale SocStart. Falls back to a nominal efficiency over the capacity.
func calibrateSocPerKwh(rows []sessionRow, capacityKWh float64) float64 {
	var ratios []float64
	for _, r := range rows {
		if r.SocStart.Valid && r.SocEnd.Valid && r.Charged.Valid && r.Charged.Float64 > 1 {
			if rise := r.SocEnd.Float64 - r.SocStart.Float64; rise > 2 {
				ratios = append(ratios, rise/r.Charged.Float64)
			}
		}
	}
	if m, ok := percentileOf(ratios, 0.5, minCalibration); ok {
		return m
	}
	if capacityKWh > 0 {
		return chargeEfficiency * 100 / capacityKWh // nominal SoC%/kWh
	}
	return 0
}

// cachedConsumption returns the driving forecast for a loadpoint's vehicle, refit at
// most once per calendar day (like cachedForecast - the fit is the only real cost).
// nil when there is too little history. Runs under optimizerMu, so the cache needs no
// further locking.
func (site *Site) cachedConsumption(lp loadpoint.API) *metrics.ConsumptionForecast {
	title := lp.GetTitle()
	day := now.BeginningOfDay()
	if e, ok := site.consumption[title]; ok && e.day.Equal(day) {
		return e.forecast
	}

	rows := vehicleSessions(title, day.AddDate(0, 0, -consumptionDays))
	capacity := lp.GetVehicle().Capacity()
	var socPerKm float64
	if capacity > 0 {
		socPerKm = defaultWhPerKm / (capacity * 1000) * 100 // nominal; odometer is legacy/low-weight
	}
	eps := awayEpisodes(rows, socPerKm, calibrateSocPerKwh(rows, capacity))
	fc := metrics.NewConsumptionForecast(eps)
	site.consumption[title] = consumptionEntry{forecast: fc, day: day}
	return fc
}

// applyConsumption models a disconnected vehicle draining its own charge while driven
// away. It sets SInitial to the best estimate of the SoC now - the disconnect SoC minus
// what has been driven since departure - and emits a per-slot exogenous drain (SDrainT)
// over the rest of the away-window, so the plan's SoC declines to the predicted reconnect
// level and then charges back up, instead of being pinned flat (the optimizer cannot
// otherwise lower a vehicle's SoC: it has no discharge path). Connected cars, and those
// without enough driving history, are left unchanged.
func (site *Site) applyConsumption(cfg *optimizer.BatteryConfig, lp loadpoint.API) {
	if s := lp.GetStatus(); s == api.StatusB || s == api.StatusC {
		return // connected: the current SoC is real, nothing is draining
	}

	fc := site.cachedConsumption(lp)
	if fc == nil {
		return
	}

	departs, ok := lastDisconnect(lp.GetTitle())
	if !ok {
		return
	}

	capacity := lp.GetVehicle().Capacity()
	if capacity <= 0 {
		return
	}

	// reconnect = the first slot the availability model expects the car plugged in again
	base := time.Now().Truncate(tariff.SlotDuration)
	reconnect := len(cfg.CMaxT)
	for i, c := range cfg.CMaxT {
		if c > 0 {
			reconnect = i
			break
		}
	}

	// only the current commute-style away-period; a multi-day gap is out of model
	if d := base.Add(time.Duration(reconnect) * tariff.SlotDuration).Sub(departs); d <= 0 || d > maxAwayWindow {
		return
	}

	// SInitial := the SoC now - the disconnect SoC (which evcc still holds, since SocStart at
	// reconnect is stale) minus what has already been driven since departure.
	already := fc.Drain(departs, base) // SoC percent driven departure -> now
	cfg.SInitial = max(cfg.SMin, cfg.SInitial-float32(already*capacity*10))

	// per-slot drain across every away-period in the horizon, capped so the optimizer can
	// always recharge enough to keep its SoC equality feasible (see drainProfile).
	availNow := (float64(cfg.SInitial) - float64(cfg.SMin)) / (capacity * 10) // current away: SoC now
	availMax := (float64(cfg.SMax) - float64(cfg.SMin)) / (capacity * 10)     // later: pack headroom
	cfg.SDrainT = drainProfile(fc, base, cfg.CMaxT, capacity, availNow, availMax)
}

// drainProfile builds the per-slot exogenous drain power (W) across every away-period in
// the horizon (the CMaxT==0 stretches), from the forecast's away-conditional per-slot
// rate. The cumulative drop within an away-period is capped - at availNow for the current
// one (bounded by the SoC now) and at availMax for later ones (bounded by the pack headroom
// the optimizer can recharge into between trips) - so the optimizer's SoC equality stays
// feasible against its s >= 0 floor. A car that would run dry just arrives empty.
func drainProfile(fc *metrics.ConsumptionForecast, base time.Time, cMaxT []float32, capacityKWh, availNow, availMax float64) []float32 {
	slotHours := tariff.SlotDuration.Hours()
	drainT := make([]float32, len(cMaxT))
	avail, cum, away := availNow, 0.0, false
	for i, c := range cMaxT {
		if c > 0 { // connected: reset the cap for the next away-period
			if away {
				avail, cum, away = availMax, 0, false
			}
			continue
		}
		away = true
		rate := fc.RateAt(base.Add(time.Duration(i) * tariff.SlotDuration)) // SoC percent this slot
		if rate <= 0 {
			continue
		}
		if cum+rate > avail {
			rate = avail - cum
		}
		if rate <= 0 {
			continue // this away-period is drained to the floor; keep scanning for the reconnect
		}
		cum += rate
		drainT[i] = float32(rate / 100 * capacityKWh * 1000 / slotHours) // SoC% -> Wh -> W over the slot
	}
	return drainT
}

// lastDisconnect returns the most recent completed session's disconnect time for a
// loadpoint - the start of the current away-period while the car is unplugged.
func lastDisconnect(title string) (time.Time, bool) {
	if db.Instance == nil {
		return time.Time{}, false
	}
	var row sessionRow
	if err := db.Instance.Table("sessions").
		Where("loadpoint = ?", title).
		Order("created DESC").Limit(1).Scan(&row).Error; err != nil || row.Finished.IsZero() {
		return time.Time{}, false
	}
	return row.Finished, true
}
