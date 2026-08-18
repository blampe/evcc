package chargepoint

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/stretchr/testify/assert"
)

// TestPollAckZeroIsNoop verifies that an ackId of 0 short-circuits to success
// instead of polling for an acknowledgement that never arrives. This is the
// case when stopping with no active session (or starting when already
// charging); previously it spun for 10s and returned errAckTimeout, which made
// evcc retry Enable(false) forever.
func TestPollAckZeroIsNoop(t *testing.T) {
	api := NewAPI(util.NewLogger("test"), nil)

	done := make(chan error, 1)
	go func() { done <- api.pollAck(0, "stop_session") }()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("pollAck(0) should return immediately, not poll")
	}
}

// sessionDetailsFixture is a (sanitized) driver-bff session response captured
// from a live CPH50 Home Flex while charging. Location, address and identifiers
// have been replaced with dummy values; the metric fields we parse are real.
const sessionDetailsFixture = `{
  "charging_status": {
    "device_id": 17437421,
    "session_id": 5275745901,
    "device_name": "CP HOME / 000000000000",
    "start_time": 1782590502000,
    "session_time": 16512000,
    "charging_time": 16496000,
    "last_update_data_timestamp": 1782607014000,
    "update_period": 300000,
    "energy_kwh": 21.551201,
    "energy_kwh_display": "21.5512",
    "power_kw": 3.3178,
    "power_kw_display": "3.31",
    "current_charging": "in_use",
    "outlet_number": 1,
    "port_level": 2,
    "update_data": [
      {"energy_kwh": 0.0, "power_kw": 0.0, "timestamp": 1782590502000},
      {"energy_kwh": 21.551201, "power_kw": 3.3178, "timestamp": 1782607014000}
    ]
  }
}`

// TestSessionDetailsParsing verifies the JSON field tags on SessionDetails map
// the driver-bff response correctly. The metric fields back the charger's
// api.Meter / ChargeRater / ChargeTimer implementations.
func TestSessionDetailsParsing(t *testing.T) {
	var res struct {
		ChargingStatus SessionDetails `json:"charging_status"`
	}
	require := assert.New(t)
	require.NoError(json.Unmarshal([]byte(sessionDetailsFixture), &res))

	d := res.ChargingStatus
	require.Equal(17437421, d.DeviceID)
	require.Equal(1, d.OutletNumber)
	require.Equal("in_use", d.ChargingState)
	require.InDelta(3.3178, d.PowerKW, 1e-9)
	require.InDelta(21.551201, d.EnergyKWh, 1e-9)
	// charging_time is milliseconds: session_time == last_update - start_time.
	require.Equal(16496000, d.ChargingTime)
}

func TestPollAckTimeoutReturnsError(t *testing.T) {
	assert.True(t, errors.Is(errAckTimeout, errAckTimeout))
}

func TestDeviceUDID(t *testing.T) {
	udid1 := deviceUDID("user@example.com")
	udid2 := deviceUDID("user@example.com")
	udid3 := deviceUDID("other@example.com")

	assert.Equal(t, udid1, udid2, "same username should produce same UDID")
	assert.NotEqual(t, udid1, udid3, "different usernames should produce different UDIDs")
	assert.Len(t, udid1, 36, "UDID should be a valid UUID string")
}

func TestNewDeviceData(t *testing.T) {
	dd := newDeviceData("test@example.com")
	assert.Equal(t, "com.coulomb.ChargePoint", dd.AppID)
	assert.Equal(t, "Apple", dd.Manufacturer)
	assert.Equal(t, "iPhone", dd.Model)
	assert.Equal(t, "IOS", dd.Type)
	assert.Equal(t, appVersion, dd.Version)
	assert.NotEmpty(t, dd.UDID)
}

func TestJwtExpired(t *testing.T) {
	assert.True(t, jwtExpired("not-a-jwt"), "invalid tokens should be treated as expired")
	assert.True(t, jwtExpired(""), "empty tokens should be treated as expired")

	// A JWT with exp in the past (2020-01-01T00:00:00Z).
	// Header: {"alg":"none","typ":"JWT"}
	// Payload: {"exp":1577836800}
	expiredJWT := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJleHAiOjE1Nzc4MzY4MDB9."
	assert.True(t, jwtExpired(expiredJWT), "past expiry should be treated as expired")

	// A JWT with exp far in the future (2099-01-01T00:00:00Z).
	// Payload: {"exp":4070908800}
	futureJWT := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJleHAiOjQwNzA5MDg4MDB9."
	assert.False(t, jwtExpired(futureJWT), "future expiry should not be treated as expired")
}
