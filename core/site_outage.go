package core

import (
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/util/config"
)

// updateGridOutage polls the grid and battery meters that can report an outage
// and publishes whether the site is islanded. A read error keeps the last known
// state, so a flaky cloud API neither starts nor ends an outage on its own.
func (site *Site) updateGridOutage() bool {
	var outage, known bool

	meters := site.batteryMeters
	if site.gridMeter != nil {
		meters = append([]config.Device[api.Meter]{site.gridMeter}, meters...)
	}

	for _, dev := range meters {
		m, ok := api.Cap[api.GridOutage](dev.Instance())
		if !ok {
			continue
		}

		res, err := m.GridOutage()
		if err != nil {
			site.log.DEBUG.Printf("grid outage: %v", err)
			continue
		}

		known = true
		outage = outage || res
	}

	if !known {
		return site.gridOutage
	}

	if outage != site.gridOutage {
		if outage {
			site.log.WARN.Println("grid outage: site is islanded, charging from solar surplus only")
		} else {
			site.log.INFO.Println("grid outage: grid restored")
		}
		site.gridOutage = outage
	}

	site.publish(keys.GridOutage, outage)

	return outage
}
