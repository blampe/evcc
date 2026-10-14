package core

import (
	"errors"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/stretchr/testify/assert"
)

type outageMeter struct {
	outage bool
	err    error
}

func (m *outageMeter) CurrentPower() (float64, error) { return 0, nil }
func (m *outageMeter) GridOutage() (bool, error)      { return m.outage, m.err }

func TestUpdateGridOutage(t *testing.T) {
	grid := &outageMeter{}
	battery := &outageMeter{}
	site := &Site{
		log:           util.NewLogger("foo"),
		gridMeter:     config.NewStaticDevice(config.Named{Name: "grid"}, api.Meter(grid)),
		batteryMeters: []config.Device[api.Meter]{config.NewStaticDevice(config.Named{Name: "battery"}, api.Meter(battery))},
	}

	assert.False(t, site.updateGridOutage(), "on grid")

	battery.outage = true
	assert.True(t, site.updateGridOutage(), "any meter reporting an outage")

	grid.err, battery.err = errors.New("timeout"), errors.New("timeout")
	assert.True(t, site.updateGridOutage(), "read errors keep the outage")

	grid.err, battery.err = nil, nil
	battery.outage = false
	assert.False(t, site.updateGridOutage(), "grid restored")

	grid.err, battery.err = errors.New("timeout"), errors.New("timeout")
	assert.False(t, site.updateGridOutage(), "read errors don't start an outage")

	assert.False(t, (&Site{log: util.NewLogger("foo")}).updateGridOutage(), "no capable meter")
}
