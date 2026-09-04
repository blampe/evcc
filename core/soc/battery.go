package soc

import (
	"time"

	"github.com/benbjohnson/clock"
	"github.com/evcc-io/evcc/util"
)

// BatteryEstimator interpolates a home battery soc reading that updates
// infrequently. Some batteries only refresh their soc every few minutes (e.g.
// the Tesla Powerwall Fleet API reports a value that can lag reality by
// minutes), which makes the pack look like it charges or discharges in coarse
// steps. Between updates the estimator integrates the measured battery power to
// fill in the intermediate soc.
//
// evcc battery power sign convention is negative = charging, positive =
// discharging; charging raises soc and discharging lowers it. Whenever the raw
// reading advances the estimate snaps back to it and restarts integration, so
// the estimate stays drift-free and is never worse than the raw reading, only
// fresher.
type BatteryEstimator struct {
	log  *util.Logger
	clck clock.Clock

	initialized  bool
	lastRawSoc   float64   // raw soc at the last observed change, in %
	lastTime     time.Time // timestamp of the previous sample
	estimatedSoc float64   // interpolated soc in %
	accumWh      float64   // energy charged since lastRawSoc, in Wh (negative while discharging)
}

// NewBatteryEstimator creates a battery soc estimator with a realtime clock.
func NewBatteryEstimator(log *util.Logger) *BatteryEstimator {
	return &BatteryEstimator{
		log:  log,
		clck: clock.New(),
	}
}

// Soc returns an estimated soc that reflects the power integrated since the raw
// reading last changed. rawSoc is the meter's reported soc (%), power the signed
// battery power (W, negative = charging) and capacity the pack capacity (kWh).
func (s *BatteryEstimator) Soc(rawSoc, power, capacity float64) float64 {
	now := s.clck.Now()

	// first sample: adopt the raw value as baseline
	if !s.initialized {
		s.initialized = true
		s.lastRawSoc = rawSoc
		s.lastTime = now
		s.estimatedSoc = rawSoc
		return rawSoc
	}

	// integrate energy since the previous sample (charging raises soc)
	dt := now.Sub(s.lastTime)
	s.lastTime = now
	s.accumWh += -power * dt.Hours()

	// the raw reading caught up: snap to it and restart integration
	if rawSoc != s.lastRawSoc {
		s.lastRawSoc = rawSoc
		s.accumWh = 0
		s.estimatedSoc = rawSoc
		return rawSoc
	}

	// without a capacity we cannot convert energy to soc
	if capacity <= 0 {
		s.estimatedSoc = rawSoc
		return rawSoc
	}

	// Wh per soc percent
	energyPerPercent := capacity * 1e3 / 100
	s.estimatedSoc = min(max(rawSoc+s.accumWh/energyPerPercent, 0), 100)

	return s.estimatedSoc
}
