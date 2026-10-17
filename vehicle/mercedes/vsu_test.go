package mercedes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	pb "github.com/evcc-io/evcc/vehicle/mercedes/pb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/protobuf/proto"
)

func TestMapVSUOdometer(t *testing.T) {
	vsu := &pb.VehicleStatusUpdate{
		FinOrVin: "WDB1234567890",
		Odo: &pb.Int64DistanceAttribute{
			Value: 42195,
			Unit:  pb.VehicleAttributeStatus_KILOMETERS,
		},
		Soc: &pb.Int64RatioAttribute{
			Value: 73,
		},
		Rangeelectric: &pb.Int64DistanceAttribute{
			Value: 210,
			Unit:  pb.VehicleAttributeStatus_KILOMETERS,
		},
		Chargingstatus: &pb.ChargingstatusEnumAttribute{
			Value: pb.Chargingstatus_CHARGINGSTATUS_AC_CHARGING_ACTIVE,
		},
	}

	res := mapVSU(vsu)

	assert.Equal(t, 42195, res.VehicleInfo.Odometer.Value, "odometer value")
	assert.Equal(t, "KILOMETERS", res.VehicleInfo.Odometer.Unit, "odometer unit")
	assert.Equal(t, 73.0, res.EvInfo.Battery.StateOfCharge, "soc")
	assert.Equal(t, 210, res.EvInfo.Battery.DistanceToEmpty.Value, "range")
	assert.Equal(t, pb.Chargingstatus_CHARGINGSTATUS_AC_CHARGING_ACTIVE, res.EvInfo.Battery.ChargingStatus)

	// AC charging active must map to StatusC (charging).
	assert.Equal(t, api.StatusC, MapChargeStatus(res.EvInfo.Battery.ChargingStatus))
}

// TestMapVSUOdometerWireRoundtrip proves the odo field (147) survives a
// marshal/unmarshal cycle – i.e. the regenerated proto decodes what the backend
// serialises.
func TestMapVSUOdometerWireRoundtrip(t *testing.T) {
	orig := &pb.VehicleStatusUpdate{
		Odo: &pb.Int64DistanceAttribute{
			Value: 123456,
			Unit:  pb.VehicleAttributeStatus_MILES,
		},
	}

	data, err := proto.Marshal(orig)
	require.NoError(t, err)

	var decoded pb.VehicleStatusUpdate
	require.NoError(t, proto.Unmarshal(data, &decoded))

	res := mapVSU(&decoded)
	assert.Equal(t, 123456, res.VehicleInfo.Odometer.Value)
	assert.Equal(t, "MILES", res.VehicleInfo.Odometer.Unit)
}

func TestMapVSUChargeCableUnplugged(t *testing.T) {
	// Missing chargingstatus falls back to UNPLUGGED (disconnected).
	res := mapVSU(&pb.VehicleStatusUpdate{})
	assert.Equal(t, pb.Chargingstatus_CHARGINGSTATUS_CHARGE_CABLE_UNPLUGGED, res.EvInfo.Battery.ChargingStatus)
	assert.Equal(t, api.StatusA, MapChargeStatus(res.EvInfo.Battery.ChargingStatus))
}

func TestMapVSUSocLimitFromChargeProgram(t *testing.T) {
	// No explicit max_soc: the SoC limit comes from the selected charge program.
	vsu := &pb.VehicleStatusUpdate{
		SelectedChargeProgram: &pb.SelectedChargeProgramEnumAttribute{
			Value: pb.SelectedChargeProgram_SELECTED_CHARGE_PROGRAM_HOME, // index 2
		},
		ChargePrograms: &pb.ChargeProgramsArrayAttribute{
			Value: []*pb.ChargeProgramParameters{
				{MaxSoc: 80},
				{MaxSoc: 90},
				{MaxSoc: 70}, // selected index 2
			},
		},
	}

	res := mapVSU(vsu)
	assert.Equal(t, 2, res.EvInfo.Battery.SelectedChargeProgram)
	assert.Equal(t, 70, res.EvInfo.Battery.SocLimit)
}

func TestMapVSUSocLimitExplicit(t *testing.T) {
	// Explicit max_soc wins over the charge-program fallback.
	vsu := &pb.VehicleStatusUpdate{
		MaxSoc: &pb.Int64RatioAttribute{Value: 85},
		ChargePrograms: &pb.ChargeProgramsArrayAttribute{
			Value: []*pb.ChargeProgramParameters{{MaxSoc: 70}},
		},
	}

	res := mapVSU(vsu)
	assert.Equal(t, 85, res.EvInfo.Battery.SocLimit)
}

// TestMapVSUSocLimitPresentButZero mirrors the live case: the VSU always carries
// max_soc, and a value of 0 means "no global override". A present-but-zero
// max_soc must fall through to the selected charge program instead of reporting
// a 0 % limit. (The legacy REST widget omitted the attribute entirely when
// unset, so this only surfaces over the VSU.)
func TestMapVSUSocLimitPresentButZero(t *testing.T) {
	vsu := &pb.VehicleStatusUpdate{
		MaxSoc: &pb.Int64RatioAttribute{Value: 0}, // present, unset
		SelectedChargeProgram: &pb.SelectedChargeProgramEnumAttribute{
			Value: pb.SelectedChargeProgram_SELECTED_CHARGE_PROGRAM_HOME, // index 2
		},
		ChargePrograms: &pb.ChargeProgramsArrayAttribute{
			Value: []*pb.ChargeProgramParameters{
				{MaxSoc: 100},
				{MaxSoc: 100},
				{MaxSoc: 100}, // selected index 2
			},
		},
	}

	res := mapVSU(vsu)
	assert.Equal(t, 100, res.EvInfo.Battery.SocLimit, "must fall back to charge program, not report 0%")
}

// TestDialErrorRateLimit covers the handshake-failure classification: a 429
// response becomes a rateLimitError carrying its Retry-After hint, while any
// other response (or none) passes the original dial error through unchanged.
func TestDialErrorRateLimit(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"120"}},
	}
	err := dialError(errors.New("expected handshake response status code 101 but got 429"), resp)
	var rl *rateLimitError
	require.True(t, errors.As(err, &rl), "429 must classify as rateLimitError")
	assert.Equal(t, 120*time.Second, rl.retryAfter)

	// Non-429 and nil responses pass the original error through untouched.
	orig := errors.New("dial tcp: connection refused")
	assert.Equal(t, orig, dialError(orig, nil))
	assert.Equal(t, orig, dialError(orig, &http.Response{StatusCode: http.StatusInternalServerError}))
}

// TestParseRetryAfter covers Retry-After parsing: valid delta-seconds map to a
// duration; empty, non-positive, or unparseable values yield zero (fall back to
// the default rate-limit cooldown).
func TestParseRetryAfter(t *testing.T) {
	assert.Equal(t, 90*time.Second, parseRetryAfter("90"))
	assert.Equal(t, time.Duration(0), parseRetryAfter(""))
	assert.Equal(t, time.Duration(0), parseRetryAfter("garbage"))
	assert.Equal(t, time.Duration(0), parseRetryAfter("0"))
	assert.Equal(t, time.Duration(0), parseRetryAfter("-5"))
}

// TestRateLimitCooldown covers the escalating 429 backoff: it starts at
// wsRateLimitBackoff, doubles per consecutive rejection, caps at
// wsMaxRateLimitBackoff, and yields to a longer Retry-After hint.
func TestRateLimitCooldown(t *testing.T) {
	assert.Equal(t, wsRateLimitBackoff, rateLimitCooldown(1, 0))
	assert.Equal(t, 2*wsRateLimitBackoff, rateLimitCooldown(2, 0))
	assert.Equal(t, 4*wsRateLimitBackoff, rateLimitCooldown(3, 0))
	assert.Equal(t, wsMaxRateLimitBackoff, rateLimitCooldown(99, 0), "escalation is capped")

	// A longer Retry-After wins; a shorter one never shrinks the escalated wait.
	assert.Equal(t, time.Hour, rateLimitCooldown(1, time.Hour))
	assert.Equal(t, 2*wsRateLimitBackoff, rateLimitCooldown(2, time.Second))
}

// TestVSUStatusFreshness covers the liveness-based freshness guard: before any
// update the provider signals retry; a cached snapshot stays fresh as long as
// the connection proved itself alive recently (even when the data itself is
// old, as for a parked car); and it only goes stale once the connection has
// been dark past the staleness window.
func TestVSUStatusFreshness(t *testing.T) {
	const vin = "WDB1234567890"
	ws := &Websocket{states: map[string]*wsState{}, interval: 15 * time.Minute}

	// No full update yet → retry semantics.
	_, err := vsuStatus(ws, vin)
	assert.ErrorIs(t, err, api.ErrMustRetry)

	// Snapshot from the last session, within the staleness window → fresh.
	ws.states[vin] = &wsState{
		vsu: &pb.VehicleStatusUpdate{
			Odo: &pb.Int64DistanceAttribute{Value: 4242, Unit: pb.VehicleAttributeStatus_KILOMETERS},
		},
		updated: time.Now().Add(-20 * time.Minute),
	}
	res, err := vsuStatus(ws, vin)
	require.NoError(t, err)
	assert.Equal(t, 4242, res.VehicleInfo.Odometer.Value)

	// Several missed sessions → stale.
	ws.states[vin].updated = time.Now().Add(-time.Hour)
	_, err = vsuStatus(ws, vin)
	assert.Error(t, err, "missed sessions eventually go stale")
}

// TestMergeVSUReplacesFields verifies that a partial update replaces individual
// fields on the cached state while leaving untouched fields intact.
func TestMergeVSUReplacesFields(t *testing.T) {
	cached := &pb.VehicleStatusUpdate{
		Odo: &pb.Int64DistanceAttribute{Value: 1000, Unit: pb.VehicleAttributeStatus_KILOMETERS},
		Soc: &pb.Int64RatioAttribute{Value: 50},
	}

	// Partial update carrying only a new SoC.
	partial := &pb.VehicleStatusUpdate{
		Soc: &pb.Int64RatioAttribute{Value: 55},
	}

	mergeVSU(cached, partial)

	// Odo unchanged, Soc replaced.
	assert.Equal(t, int64(1000), cached.GetOdo().GetValue(), "odometer preserved")
	assert.Equal(t, int64(55), cached.GetSoc().GetValue(), "soc replaced")
}

// TestMergeVSUReplacesRepeated verifies repeated/array attributes are REPLACED,
// not concatenated (the reason we use protoreflect.Range instead of proto.Merge).
func TestMergeVSUReplacesRepeated(t *testing.T) {
	cached := &pb.VehicleStatusUpdate{
		ChargePrograms: &pb.ChargeProgramsArrayAttribute{
			Value: []*pb.ChargeProgramParameters{
				{MaxSoc: 80},
				{MaxSoc: 90},
			},
		},
	}

	partial := &pb.VehicleStatusUpdate{
		ChargePrograms: &pb.ChargeProgramsArrayAttribute{
			Value: []*pb.ChargeProgramParameters{
				{MaxSoc: 100},
			},
		},
	}

	mergeVSU(cached, partial)

	// The array must be replaced (1 entry), not appended (would be 3).
	require.Len(t, cached.GetChargePrograms().GetValue(), 1)
	assert.Equal(t, int32(100), cached.GetChargePrograms().GetValue()[0].GetMaxSoc())
}

func TestDeviceIDStable(t *testing.T) {
	id := deviceID("user@example.com", "NORAM")
	assert.Equal(t, id, deviceID("user@example.com", "NORAM"), "same account must yield the same device id across restarts")
	assert.NotEqual(t, id, deviceID("other@example.com", "NORAM"))
	assert.NotEqual(t, id, deviceID("user@example.com", "EMEA"))
	_, err := uuid.Parse(id)
	assert.NoError(t, err)
}

func TestRateLimitPersisted(t *testing.T) {
	ws := &Websocket{
		identity: &Identity{account: "ratelimit@example.com"},
		region:   "NORAM",
	}
	assert.Zero(t, loadRateLimit(ws.rateLimitKey()), "no state before the first 429")

	until := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	ws.rateLimit = rateLimitState{Count: 3, Until: until}
	ws.saveRateLimit()

	res := loadRateLimit(ws.rateLimitKey())
	assert.Equal(t, 3, res.Count)
	assert.True(t, until.Equal(res.Until), "a restart must resume the pending cooldown")
}

func TestShutdownSendsNormalClose(t *testing.T) {
	closed := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			closed <- err
			return
		}
		_, _, err = c.Read(context.Background())
		closed <- err
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	ws := &Websocket{conn: conn, cancel: cancel}

	// Mirror connect(): the read loop must observe the close and return.
	readDone := make(chan struct{})
	go func() {
		_, _, _ = conn.Read(ctx)
		close(readDone)
	}()

	ws.shutdown()

	select {
	case err := <-closed:
		assert.Equal(t, websocket.StatusNormalClosure, websocket.CloseStatus(err), "backend must see a normal close frame")
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the close")
	}
	<-readDone
	assert.Error(t, ctx.Err(), "shutdown must stop the run loop")
}

// sessionServer accepts one websocket, sends the given pushes, then reads until
// the client closes and reports the close status.
func sessionServer(t *testing.T, pushes [][]byte, every time.Duration) (string, <-chan error) {
	t.Helper()
	closed := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			closed <- err
			return
		}
		go func() {
			for _, p := range pushes {
				if c.Write(context.Background(), websocket.MessageBinary, p) != nil {
					return
				}
				time.Sleep(every)
			}
		}()
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				closed <- err
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), closed
}

func vsuPush(t *testing.T, vin string, seq int64, odo int64) []byte {
	t.Helper()
	b, err := proto.Marshal(&pb.PushMessage{Msg: &pb.PushMessage_VehicleStatusUpdates{
		VehicleStatusUpdates: &pb.VehicleStatusUpdates{
			SequenceNumber: seq,
			VehicleStatusUpdates: map[string]*pb.VehicleStatusUpdate{
				vin: {FullUpdate: true, Odo: &pb.Int64DistanceAttribute{Value: odo}},
			},
		},
	}})
	require.NoError(t, err)
	return b
}

func testWebsocket(uri string) *Websocket {
	return &Websocket{
		log:      util.NewLogger("test"),
		identity: &Identity{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "token"})},
		uri:      uri,
		states:   make(map[string]*wsState),
		commands: make(map[string]*pb.AppTwinCommandStatus),
	}
}

func shortSession(t *testing.T, linger, maxSession time.Duration) {
	t.Helper()
	l, m := wsLinger, wsMaxSession
	wsLinger, wsMaxSession = linger, maxSession
	t.Cleanup(func() { wsLinger, wsMaxSession = l, m })
}

func TestSessionClosesWhenIdle(t *testing.T) {
	shortSession(t, 100*time.Millisecond, 5*time.Second)
	const vin = "WDB1234567890"
	uri, closed := sessionServer(t, [][]byte{vsuPush(t, vin, 1, 4242)}, 0)

	ws := testWebsocket(uri)
	start := time.Now()
	require.NoError(t, ws.session(t.Context()))
	assert.Less(t, time.Since(start), time.Second, "session must close once idle, not stay open")

	assert.Equal(t, websocket.StatusNormalClosure, websocket.CloseStatus(<-closed), "backend must see a normal close frame")
	vsu, _, ok := ws.Status(vin)
	require.True(t, ok)
	assert.Equal(t, int64(4242), vsu.GetOdo().GetValue())
}

func TestSessionWithoutUpdateKeepsStateCurrent(t *testing.T) {
	shortSession(t, 100*time.Millisecond, 5*time.Second)
	const vin = "WDB1234567890"
	uri, _ := sessionServer(t, nil, 0)

	ws := testWebsocket(uri)
	stale := time.Now().Add(-time.Hour)
	ws.states[vin] = &wsState{vsu: &pb.VehicleStatusUpdate{Odo: &pb.Int64DistanceAttribute{Value: 4242}}, updated: stale}

	require.NoError(t, ws.session(t.Context()), "a quiet session is not a failure to retry")

	vsu, updated, ok := ws.Status(vin)
	require.True(t, ok)
	assert.Equal(t, int64(4242), vsu.GetOdo().GetValue(), "the cached snapshot stays")
	assert.True(t, updated.After(stale), "a quiet session means nothing changed, so the snapshot is current")
}

func TestSessionMaxDuration(t *testing.T) {
	shortSession(t, time.Second, 300*time.Millisecond)
	const vin = "WDB1234567890"
	pushes := make([][]byte, 100)
	for i := range pushes {
		pushes[i] = vsuPush(t, vin, int64(i), int64(i))
	}
	uri, closed := sessionServer(t, pushes, 50*time.Millisecond)

	start := time.Now()
	require.NoError(t, testWebsocket(uri).session(t.Context()))
	assert.Less(t, time.Since(start), time.Second, "continuous updates must not extend the session past its cap")
	assert.Equal(t, websocket.StatusNormalClosure, websocket.CloseStatus(<-closed))
}
