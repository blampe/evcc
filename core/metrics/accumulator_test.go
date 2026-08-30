package metrics

import (
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/jinzhu/now"
	"github.com/stretchr/testify/assert"
)

func TestMeterEnergyMeterTotal(t *testing.T) {
	clock := clock.NewMock()
	clock.Set(now.BeginningOfDay())

	me := &Accumulator{clock: clock}

	me.SetEnergyMeterTotal(10)
	assert.Equal(t, 0.0, me.Energy)
	me.SetEnergyMeterTotal(11)
	assert.Equal(t, 1.0, me.Energy)
	me.SetEnergyMeterTotal(11)
	assert.Equal(t, 1.0, me.Energy)
}

func TestMeterEnergyAddPower(t *testing.T) {
	clock := clock.NewMock()
	clock.Set(now.BeginningOfDay())

	me := &Accumulator{clock: clock}

	clock.Add(60 * time.Minute)
	me.AddPower(1e3)
	assert.Equal(t, 0.0, me.Energy)

	clock.Add(60 * time.Minute)
	me.AddPower(1e3)
	assert.Equal(t, 1.0, me.Energy)

	clock.Add(30 * time.Minute)
	me.AddPower(1e3)
	assert.Equal(t, 1.5, me.Energy)
}

func TestMeterAddConnected(t *testing.T) {
	clock := clock.NewMock()
	clock.Set(now.BeginningOfDay())

	me := &Accumulator{clock: clock}

	// first sample after boot has no elapsed baseline
	me.AddConnected(true)
	assert.Equal(t, 0.0, me.Energy)

	// fully connected for 5 minutes -> 5 connected-minutes
	clock.Add(5 * time.Minute)
	me.AddConnected(true)
	assert.Equal(t, 5.0, me.Energy)

	// disconnected interval adds nothing but advances the clock
	clock.Add(5 * time.Minute)
	me.AddConnected(false)
	assert.Equal(t, 5.0, me.Energy)

	// reconnected: only the new elapsed interval counts, the disconnected gap
	// is not back-filled
	clock.Add(5 * time.Minute)
	me.AddConnected(true)
	assert.Equal(t, 10.0, me.Energy)
}
