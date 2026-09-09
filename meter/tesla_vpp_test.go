package meter

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVPPEvents(t *testing.T) {
	// shape captured live from owner-api /programs (Tesla PG&E DSGS)
	body := []byte(`{"response":{"programs":[{"program_name":"Tesla PG&E DSGS","event_dispatch_state":"not_active","events":[{"watch_event_id":"abc","opted_in":true,"start":{"seconds":1789001100},"end":{"seconds":1789008300}}]}]}}`)
	events, err := parseVPPEvents(body)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "abc", events[0].ID)
	assert.Equal(t, "Tesla PG&E DSGS", events[0].Program)
	assert.True(t, events[0].OptedIn)
	assert.Equal(t, int64(1789001100), events[0].Start.Unix())
	assert.Equal(t, int64(1789008300), events[0].End.Unix())
}

func TestParseVPPEventsEmpty(t *testing.T) {
	// no programs → response is the empty string, not an object
	events, err := parseVPPEvents([]byte(`{"response":""}`))
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestParseVPPEventsConcatenated(t *testing.T) {
	// the owner-api has been observed to stream more than one object back-to-back
	body := []byte(`{"response":""}{"response":{"programs":[{"program_name":"P","events":[{"watch_event_id":"x","opted_in":true,"start":{"seconds":100},"end":{"seconds":200}}]}]}}`)
	events, err := parseVPPEvents(body)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "x", events[0].ID)
}

func TestCurrentOrNextEvent(t *testing.T) {
	now := time.Unix(1000, 0)
	past := vppEvent{ID: "past", Start: time.Unix(100, 0), End: time.Unix(200, 0), OptedIn: true}
	active := vppEvent{ID: "active", Start: time.Unix(900, 0), End: time.Unix(1100, 0), OptedIn: true}
	future := vppEvent{ID: "future", Start: time.Unix(5000, 0), End: time.Unix(6000, 0), OptedIn: true}
	optedOut := vppEvent{ID: "out", Start: time.Unix(800, 0), End: time.Unix(9000, 0), OptedIn: false}

	t.Run("prefers the active event", func(t *testing.T) {
		ev, ok := currentOrNextEvent([]vppEvent{past, future, active}, now)
		require.True(t, ok)
		assert.Equal(t, "active", ev.ID)
	})
	t.Run("falls back to the soonest upcoming", func(t *testing.T) {
		ev, ok := currentOrNextEvent([]vppEvent{past, future}, now)
		require.True(t, ok)
		assert.Equal(t, "future", ev.ID)
	})
	t.Run("ignores opted-out and past events", func(t *testing.T) {
		_, ok := currentOrNextEvent([]vppEvent{past, optedOut}, now)
		assert.False(t, ok)
	})
}

func TestJWTSubject(t *testing.T) {
	// payload {"sub":"abc-123","scp":["openid"]} → base64url (no padding)
	tok := "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"abc-123","scp":["openid"]}`)) + ".sig"
	assert.Equal(t, "abc-123", jwtSubject(tok))
	assert.Empty(t, jwtSubject("not-a-jwt"))
	assert.Empty(t, jwtSubject(""))
}

func TestApplyVPPPremium(t *testing.T) {
	mkRate := func(startSec, endSec int64, v float64) api.Rate {
		return api.Rate{Start: time.Unix(startSec, 0), End: time.Unix(endSec, 0), Value: v}
	}
	rates := api.Rates{
		mkRate(0, 100, 0.03),   // before the event
		mkRate(100, 200, 0.03), // overlaps the event
		mkRate(200, 300, 3.00), // overlaps, already worth more than the premium
		mkRate(300, 400, 0.05), // after the event
	}
	events := []vppEvent{{Start: time.Unix(100, 0), End: time.Unix(300, 0), OptedIn: true}}

	out := applyVPPPremium(rates, events, 2.0)
	assert.Equal(t, 0.03, out[0].Value, "before the event is untouched")
	assert.Equal(t, 2.00, out[1].Value, "overlapping slot raised to the premium")
	assert.Equal(t, 3.00, out[2].Value, "a slot already worth more is not lowered")
	assert.Equal(t, 0.05, out[3].Value, "after the event is untouched")

	t.Run("opted-out events do not apply", func(t *testing.T) {
		optedOut := []vppEvent{{Start: time.Unix(100, 0), End: time.Unix(300, 0), OptedIn: false}}
		out := applyVPPPremium(api.Rates{mkRate(100, 200, 0.03)}, optedOut, 2.0)
		assert.Equal(t, 0.03, out[0].Value)
	})
}
