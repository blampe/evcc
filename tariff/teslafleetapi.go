package tariff

import (
	"github.com/evcc-io/evcc/meter"
)

func init() {
	registry.Add("teslafleetapi", meter.NewFleetAPITariffFromConfig)
}
