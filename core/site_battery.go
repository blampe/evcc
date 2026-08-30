package core

import (
	"errors"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/hems/hems"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util/config"
)

func batteryModeModified(mode api.BatteryMode) bool {
	return mode != api.BatteryUnknown && mode != api.BatteryNormal
}

func (site *Site) batteryConfigured() bool {
	return len(site.batteryMeters) > 0
}

func (site *Site) hasBatteryControl() bool {
	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		if api.HasCap[api.BatteryController](meter) {
			return true
		}
	}

	return false
}

// setBatteryMode sets the battery mode
func (site *Site) setBatteryMode(batMode api.BatteryMode) {
	site.batteryMode = batMode
	site.publish(keys.BatteryMode, batMode)
}

// SetBatteryMode sets the battery mode
func (site *Site) SetBatteryMode(batMode api.BatteryMode) {
	site.Lock()
	defer site.Unlock()

	site.log.DEBUG.Println("set battery mode:", batMode)

	if site.batteryMode != batMode {
		site.setBatteryMode(batMode)
	}

	if site.batteryModeExternal == api.BatteryUnknown {
		site.batteryModeExternalTimer = time.Time{}
	}
}

func (site *Site) updateBatteryMode(batteryGridChargeActive bool, rate api.Rate) {
	batteryMode := site.requiredBatteryMode(batteryGridChargeActive, rate)

	// put battery into hold mode when charging is active and HEMS dimmed
	fromToCharge := batteryMode == api.BatteryCharge || batteryMode == api.BatteryUnknown && site.batteryMode == api.BatteryCharge
	if dimmed := hems.Dimmed(site.hems); fromToCharge && dimmed != nil && *dimmed {
		site.log.DEBUG.Println("battery mode: HEMS dimmed")
		batteryMode = api.BatteryHold
	}

	// refresh the export spike as the 15-min slot advances: it targets only the
	// current slot, so re-apply the (unchanged) spike mode when we cross into a new
	// slot, otherwise the spike written on mode-entry goes stale and the pack
	// resumes charging while the old synthetic tariff still lingers on the pack.
	slot := time.Now().Truncate(tariff.SlotDuration)
	if batteryMode == api.BatteryUnknown && !slot.Equal(site.batterySpikeSlot) &&
		(site.batteryMode == api.BatteryDischarge || site.batteryMode == api.BatteryHoldCharge) {
		batteryMode = site.batteryMode
	}

	// battery boost: while a car charges from the home battery, normal mode pins
	// the pack's reserve to the plan's per-slot target SoC (see applyBatteryMode)
	// so it stops there instead of draining to MinSoc. Re-apply the (unchanged)
	// normal mode when the boost starts or the slot advances so the pin tracks the
	// plan, and once when it ends to restore the MinSoc reserve.
	boostActive := site.optimizerBatteryBoostActive()
	site.RLock()
	boostSlot := site.batteryBoostSlot
	site.RUnlock()
	if batteryMode == api.BatteryUnknown && site.batteryMode == api.BatteryNormal &&
		((boostActive && !slot.Equal(boostSlot)) || (!boostActive && !boostSlot.IsZero())) {
		batteryMode = site.batteryMode
	}

	// NOTE: applyBatteryMode is always called when charge mode is active to validate max soc
	if modeChanged := batteryMode != api.BatteryUnknown; modeChanged || site.batteryMode == api.BatteryCharge {
		if err := site.applyBatteryMode(batteryMode, boostActive); err == nil {
			if batteryMode == api.BatteryDischarge || batteryMode == api.BatteryHoldCharge {
				site.batterySpikeSlot = slot
			}
			// track the slot the boost reserve was pinned for (zero when not boosting)
			newBoostSlot := time.Time{}
			if boostActive {
				newBoostSlot = slot
			}
			site.Lock()
			site.batteryBoostSlot = newBoostSlot
			site.Unlock()
			if modeChanged {
				site.SetBatteryMode(batteryMode)
			}
		} else {
			site.log.ERROR.Println("battery mode:", err)
		}
	}
}

// optimizerBatteryBoostActive reports whether the optimizer is boosting a car
// from the home battery this cycle (any loadpoint charging from source
// "battery"). During a boost the pack's discharge reserve is pinned to the plan's
// target SoC (in normal mode) so it stops there instead of draining to MinSoc.
func (site *Site) optimizerBatteryBoostActive() bool {
	for _, lp := range site.loadpoints {
		if lp != nil && lp.optimizerChargeSource == sourceBattery {
			return true
		}
	}
	return false
}

// batteryBoostActive reports whether any loadpoint is boosting, i.e. draining
// the home battery into the vehicle
func (site *Site) batteryBoostActive() bool {
	for _, lp := range site.loadpoints {
		if lp.GetBatteryBoost() != boostDisabled {
			return true
		}
	}
	return false
}

// requiredBatteryMode determines required battery mode based on grid charge and rate
func (site *Site) requiredBatteryMode(batteryGridChargeActive bool, rate api.Rate) api.BatteryMode {
	var res api.BatteryMode
	batMode := site.GetBatteryMode()
	extMode := site.GetBatteryModeExternal()

	var extModeReset bool
	if extMode == api.BatteryUnknown {
		site.Lock()
		extModeReset = !site.batteryModeExternalTimer.IsZero()
		site.Unlock()
	}

	keepUnlessModified := func(s api.BatteryMode) api.BatteryMode {
		return map[bool]api.BatteryMode{false: s, true: api.BatteryUnknown}[batMode == s]
	}

	// optimizer suggestion (always-on): drives battery control whenever present,
	// e.g. discharge-to-grid during export-price peaks
	optMode := site.optimizerBatteryMode()

	switch {
	case !site.batteryConfigured():
		res = api.BatteryUnknown
	case extModeReset:
		// require normal mode to leave external control
		res = api.BatteryNormal
	case extMode != api.BatteryUnknown:
		// require external mode only once
		if extMode != batMode {
			res = extMode
		}
	case site.batteryBoostActive():
		// battery boost drains the home battery into the vehicle, so the battery
		// must be allowed to discharge — override the optimizer's hold/holdcharge
		// (otherwise the boost pulls from the grid instead)
		res = keepUnlessModified(api.BatteryNormal)
	case optMode != api.BatteryUnknown:
		// optimizer takes precedence over the static grid-charge / discharge
		// heuristics; it reverts to normal (via batteryModeModified below) once
		// it stops suggesting an action
		res = keepUnlessModified(optMode)
	case batteryGridChargeActive:
		res = keepUnlessModified(api.BatteryCharge)
	case site.dischargeControlActive(rate):
		res = keepUnlessModified(api.BatteryHold)
	case batteryModeModified(batMode):
		res = api.BatteryNormal
	}

	return res
}

// batteryMaxSocReached checks is battery has exceed max soc limit
func (site *Site) batteryMaxSocReached(dev config.Device[api.Meter]) (bool, error) {
	meter := dev.Instance()

	batLimiter, ok := api.Cap[api.BatterySocLimiter](meter)
	if !ok {
		return false, nil
	}

	batSoc, ok := api.Cap[api.Battery](meter)
	if !ok {
		return false, errors.New("battery with soc limits must have soc")
	}

	soc, err := batSoc.Soc()
	if err != nil {
		return false, err
	}

	if _, max := batLimiter.GetSocLimits(); max > 0 && max < 100 && soc >= max {
		site.log.DEBUG.Printf("battery %s: limit soc reached (%.0f > %.0f)", deviceTitleOrName(dev), soc, max)
		return true, nil
	}

	return false, nil
}

// applyBatteryMode applies the mode to each battery. boost indicates the
// optimizer is draining the pack into a car this cycle, which pins normal mode's
// reserve to the plan's target SoC.
//
// api.BatteryCharge:
//
//	The current soc is validated against max soc.
//	In case max soc is reached, hold mode is applied.
func (site *Site) applyBatteryMode(mode api.BatteryMode, boost bool) error {
	fromToCharge := mode == api.BatteryCharge || mode == api.BatteryUnknown && site.batteryMode == api.BatteryCharge

	for _, dev := range site.batteryMeters {
		meter := dev.Instance()

		batCtrl, ok := api.Cap[api.BatteryController](meter)
		if !ok {
			continue
		}

		// validate max soc
		if fromToCharge && mode != api.BatteryHold {
			ok, err := site.batteryMaxSocReached(dev)
			if err != nil && !errors.Is(err, api.ErrNotAvailable) {
				return err
			}

			// put battery into hold mode when soc limit reached
			if ok {
				// TODO do this only once
				mode = api.BatteryHold
			}
		}

		if mode != api.BatteryUnknown {
			var err error
			if ctrl, ok := api.Cap[api.BatteryModeController](meter); ok {
				// target-aware controllers pin the reserve to the optimizer's
				// planned SoC (e.g. stop a grid discharge at the target instead
				// of draining to min soc). Normal mode only pins during a boost;
				// plain self-consumption keeps the pack available down to MinSoc.
				target := site.optimizerBatteryTargetSoc(dev.Config().Name)
				if mode == api.BatteryNormal && !boost {
					target = 0
				}
				err = ctrl.SetBatteryModeSoc(mode, target)
			} else {
				err = batCtrl.SetBatteryMode(mode)
			}
			if err == nil {
				site.log.DEBUG.Printf("set battery %s mode: %s", deviceTitleOrName(dev), mode)
			} else if !errors.Is(err, api.ErrNotAvailable) {
				return err
			}
		}
	}

	return nil
}

func (site *Site) tariffRates(usage api.TariffUsage) (api.Rates, error) {
	tariff := site.GetTariff(usage)
	if tariff == nil || tariff.Type() == api.TariffTypePriceStatic {
		return nil, nil
	}

	return tariff.Rates()
}

func (site *Site) smartCostActive(lp loadpoint.API, rate api.Rate) bool {
	limit := lp.GetSmartCostLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

func (site *Site) batteryGridChargeActive(rate api.Rate) bool {
	limit := site.GetBatteryGridChargeLimit()
	return limit != nil && !rate.IsZero() && rate.Value <= *limit
}

func (site *Site) dischargeControlActive(rate api.Rate) bool {
	if !site.GetBatteryDischargeControl() {
		return false
	}

	for _, lp := range site.activeLoadpoints() {
		smartCostActive := site.smartCostActive(lp, rate)
		if lp.GetStatus() == api.StatusC && (smartCostActive || lp.IsFastChargingActive()) {
			return true
		}
	}

	return false
}
