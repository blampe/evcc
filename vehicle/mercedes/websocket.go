package mercedes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/coder/websocket"
	"github.com/evcc-io/evcc/util"
	pb "github.com/evcc-io/evcc/vehicle/mercedes/pb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	// wsPingInterval keeps the connection alive and proves liveness between VSU
	// pushes. A parked vehicle sends no data for hours; the successful ping (not
	// incoming data) is what tells us the cached snapshot is still current.
	wsPingInterval = 30 * time.Second

	// wsMinReconnectDelay is the floor between handshake attempts. The VSU socket
	// is long-lived — kept open by pings and only re-dialed when the backend
	// actually drops us — so this simply bounds how fast we re-handshake after a
	// drop. Re-dialing too eagerly is what earns HTTP 429 account blocks, so the
	// connection is never torn down on idle; it is held open instead.
	wsMinReconnectDelay = 30 * time.Second

	// wsMaxReconnectDelay caps the exponential backoff after repeated dial
	// failures.
	wsMaxReconnectDelay = 5 * time.Minute

	// wsRateLimitBackoff is the initial cooldown after an HTTP 429 handshake
	// rejection that carries no Retry-After. A 429 means we dialed too often for
	// the account, so we wait far longer than a normal reconnect before retrying.
	wsRateLimitBackoff = 5 * time.Minute

	// wsMaxRateLimitBackoff caps the escalating 429 cooldown. The backend's
	// account block outlasts a single cooldown and only clears after a sustained
	// quiet period, so consecutive rejections back off toward this cap rather than
	// re-dialing every wsRateLimitBackoff forever (which can keep resetting a
	// rolling-window block so it never clears).
	wsMaxRateLimitBackoff = 30 * time.Minute

	// wsHealthyThreshold is how long a connection must stay up to count as a
	// healthy session that resets the reconnect backoff. A connection that drops
	// sooner leaves the backoff climbing, so repeated fast drops back off instead
	// of hammering the handshake.
	wsHealthyThreshold = 60 * time.Second

	// wsMaxMessageBytes bounds a single websocket frame. VSU full updates carry
	// ~250 attributes per vehicle; 8 MiB is comfortably above that.
	wsMaxMessageBytes = 8 << 20
)

// wsState is one vehicle's cached VSU together with the time it was last
// updated, used by the provider's freshness guard.
type wsState struct {
	vsu     *pb.VehicleStatusUpdate
	updated time.Time
}

// Websocket maintains a single VSU push connection per Mercedes account and
// caches the latest VehicleStatusUpdate per VIN. It is long-lived: there is one
// instance per account (see the registry in helper.go), started once and kept
// running for the process lifetime, mirroring evcc's websocket chargers.
type Websocket struct {
	mu        sync.RWMutex
	log       *util.Logger
	identity  *Identity
	region    string
	sessionID string
	states    map[string]*wsState                 // keyed by VIN
	commands  map[string]*pb.AppTwinCommandStatus // latest command result, keyed by VIN

	// lastAlive is the last time the connection proved itself alive — a
	// successful ping or an incoming message — guarded by mu. The freshness guard
	// keys off this rather than the age of the last data push, since a healthy
	// connection to a parked car legitimately delivers nothing for long stretches.
	lastAlive time.Time

	// conn is the current connection, guarded by mu; used to send acks from the
	// read loop.
	connMu sync.Mutex
	conn   *websocket.Conn
}

var (
	wsMu       sync.Mutex
	websockets = make(map[string]*Websocket)

	errNotConnected = errors.New("websocket not connected")
)

// NewWebsocket returns the shared Websocket for the given account, creating and
// starting it on first use. Subsequent vehicles on the same account reuse it.
func NewWebsocket(log *util.Logger, identity *Identity) *Websocket {
	wsMu.Lock()
	defer wsMu.Unlock()

	key := identity.account + "-" + identity.region
	if ws, ok := websockets[key]; ok {
		return ws
	}

	ws := &Websocket{
		log:       log,
		identity:  identity,
		region:    identity.region,
		sessionID: uuid.New().String(),
		states:    make(map[string]*wsState),
		commands:  make(map[string]*pb.AppTwinCommandStatus),
	}

	// Process-lifetime goroutine. api.Vehicle has no Close(); the account
	// registry guarantees at most one manager per account.
	go ws.run(context.Background())

	websockets[key] = ws
	return ws
}

// Status returns the latest cached VSU for the given VIN and the time it was
// received. ok is false until the first update for that VIN arrives.
func (ws *Websocket) Status(vin string) (*pb.VehicleStatusUpdate, time.Time, bool) {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	s, ok := ws.states[vin]
	if !ok || s.vsu == nil {
		return nil, time.Time{}, false
	}
	return s.vsu, s.updated, true
}

// commandStatus returns the latest command result whose request id matches
// reqID (the id returned by sendCommand). ok is false until that command's
// status push arrives. Results are cached latest-per-VIN, so a newer command to
// the same vehicle replaces an older one.
func (ws *Websocket) commandStatus(reqID string) (*pb.AppTwinCommandStatus, bool) {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	for _, st := range ws.commands {
		if st.GetRequestId() == reqID {
			return st, true
		}
	}
	return nil, false
}

// sendCommand issues a vehicle CommandRequest over the websocket. It stamps a
// request id when one is not set and returns it so the caller can correlate the
// asynchronous result via commandStatus. It does not block waiting for a
// reconnect: if the connection is momentarily down it returns errNotConnected so
// the caller can retry.
func (ws *Websocket) sendCommand(ctx context.Context, req *pb.CommandRequest) (string, error) {
	if req.GetRequestId() == "" {
		req.RequestId = uuid.New().String()
	}
	return req.GetRequestId(), ws.writeMessage(ctx, &pb.ClientMessage{
		Msg: &pb.ClientMessage_CommandRequest{CommandRequest: req},
	})
}

// run maintains a single long-lived VSU connection. The socket is kept open
// (pings prove liveness); we only re-dial when the backend drops us, which keeps
// the handshake rate low enough to stay clear of the 429 account limit.
//
// Normal reconnects use exponential backoff floored at wsMinReconnectDelay, and
// a healthy session (one that stayed up past wsHealthyThreshold) resets it. A
// handshake rejected with HTTP 429 is handled separately: the cooldown escalates
// across consecutive rejections (see rateLimitCooldown) so a sustained account
// block gets an increasingly quiet window to clear rather than being reset by
// eager re-dials.
func (ws *Websocket) run(ctx context.Context) {
	bo := backoff.NewExponentialBackOff(
		backoff.WithMaxElapsedTime(0),
		backoff.WithInitialInterval(wsMinReconnectDelay),
		backoff.WithMaxInterval(wsMaxReconnectDelay),
	)

	var rateLimits int // consecutive 429 handshake rejections
	for ctx.Err() == nil {
		healthy, err := ws.connect(ctx)

		var delay time.Duration
		var rl *rateLimitError
		switch {
		case errors.As(err, &rl):
			rateLimits++
			delay = rateLimitCooldown(rateLimits, rl.retryAfter)
			ws.log.DEBUG.Printf("websocket: handshake rate limited (429) x%d, backing off %s", rateLimits, delay.Round(time.Second))

		default:
			if err != nil && !errors.Is(err, context.Canceled) {
				ws.log.DEBUG.Printf("websocket: %v", err)
			}
			rateLimits = 0
			if healthy {
				bo.Reset()
			}
			delay = bo.NextBackOff()
			if delay < wsMinReconnectDelay {
				delay = wsMinReconnectDelay
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// rateLimitCooldown returns how long to wait after `consecutive` back-to-back
// 429 handshake rejections. The wait starts at wsRateLimitBackoff and doubles
// each time up to wsMaxRateLimitBackoff, so a sustained account block gets an
// increasingly quiet window to clear. A server Retry-After hint wins if longer.
func rateLimitCooldown(consecutive int, retryAfter time.Duration) time.Duration {
	d := wsRateLimitBackoff
	for i := 1; i < consecutive && d < wsMaxRateLimitBackoff; i++ {
		d *= 2
	}
	if d > wsMaxRateLimitBackoff {
		d = wsMaxRateLimitBackoff
	}
	return max(d, retryAfter)
}

// connect dials the websocket and runs the read loop until the connection drops,
// returning the terminating error. The bool reports whether the session was
// healthy (stayed up past wsHealthyThreshold) so the caller can reset its
// reconnect backoff.
//
// There is deliberately no read deadline: a parked vehicle receives its full
// update on connect and then goes quiet indefinitely, so a read timeout would
// tear down a perfectly good connection and drive the very reconnect storm that
// triggers 429s. Liveness is instead proven by the pinger, which closes the
// connection on a failed ping and thereby unblocks Read with an error.
func (ws *Websocket) connect(ctx context.Context) (bool, error) {
	tok, err := ws.identity.Token()
	if err != nil {
		return false, err
	}

	headers := wsheaders(tok.AccessToken, ws.sessionID, ws.region)
	opts := &websocket.DialOptions{HTTPHeader: mapToHeader(headers)}

	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, getWebsocketUri(ws.region), opts)
	cancel()
	if err != nil {
		return false, dialError(err, resp)
	}
	conn.SetReadLimit(wsMaxMessageBytes)

	ws.connMu.Lock()
	ws.conn = conn
	ws.connMu.Unlock()

	defer func() {
		ws.connMu.Lock()
		ws.conn = nil
		ws.connMu.Unlock()
		_ = conn.CloseNow()
	}()

	connectedAt := time.Now()
	ws.markAlive()
	ws.log.DEBUG.Println("websocket: connected")

	// Ping keep-alive. Liveness is proven by pings, not by data: the pinger
	// tears the connection down when a ping fails, so a stalled connection is
	// surfaced as a read error rather than blocking Read forever.
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go ws.pinger(pingCtx, conn)

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// Any read return means the connection dropped: the pinger closed a
			// dead link, the backend closed it, or the network failed. Report
			// whether the session lived long enough to count as healthy so run()
			// can decide between resetting and climbing its backoff.
			return time.Since(connectedAt) >= wsHealthyThreshold, err
		}
		ws.markAlive()
		if typ != websocket.MessageBinary {
			continue
		}

		if err := ws.handleMessage(ctx, data); err != nil {
			ws.log.DEBUG.Printf("websocket: handle: %v", err)
		}
	}
}

func (ws *Websocket) pinger(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				// A failed ping means the connection is dead. Close it so the
				// read loop unblocks immediately with an error and reconnects.
				// Skip the close on parent-context teardown, where connect()
				// already closes the connection.
				if ctx.Err() == nil {
					_ = conn.CloseNow()
				}
				return
			}
			// A successful ping proves the link is alive even while a parked car
			// sends no data, keeping the freshness guard satisfied.
			ws.markAlive()
		}
	}
}

// handleMessage decodes a PushMessage and acknowledges it. Every
// sequence-numbered push type must be acknowledged or the backend redelivers it
// and eventually drops the connection, so both vehicle_status_updates (the data
// we cache) and apptwin_command_status_updates (results of commands we sent) are
// handled and acked. Other push types are ignored.
func (ws *Websocket) handleMessage(ctx context.Context, data []byte) error {
	var msg pb.PushMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		return err
	}

	var ack *pb.ClientMessage
	switch {
	case msg.GetVehicleStatusUpdates() != nil:
		vsus := msg.GetVehicleStatusUpdates()
		ws.applyVSU(vsus)
		ack = &pb.ClientMessage{Msg: &pb.ClientMessage_AcknowledgeVehicleStatusUpdates{
			AcknowledgeVehicleStatusUpdates: &pb.AcknowledgeVehicleStatusUpdates{SequenceNumber: vsus.GetSequenceNumber()},
		}}

	case msg.GetApptwinCommandStatusUpdatesByVin() != nil:
		// Result of a command we (or another session) sent. Cache it for
		// SendCommand callers and ack it: the push is sequence-numbered, and an
		// unacknowledged push is redelivered and can eventually make the backend
		// drop the connection.
		cmd := msg.GetApptwinCommandStatusUpdatesByVin()
		ws.applyCommandStatus(cmd)
		ack = &pb.ClientMessage{Msg: &pb.ClientMessage_AcknowledgeApptwinCommandStatusUpdateByVin{
			AcknowledgeApptwinCommandStatusUpdateByVin: &pb.AcknowledgeAppTwinCommandStatusUpdatesByVIN{SequenceNumber: cmd.GetSequenceNumber()},
		}}

	default:
		return nil
	}

	return ws.writeMessage(ctx, ack)
}

// writeMessage marshals a ClientMessage and writes it on the current connection.
// connMu only guards fetching the connection pointer; the blocking write happens
// outside the lock (coder/websocket serialises concurrent writes internally).
func (ws *Websocket) writeMessage(ctx context.Context, msg *pb.ClientMessage) error {
	out, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	ws.connMu.Lock()
	conn := ws.conn
	ws.connMu.Unlock()
	if conn == nil {
		return errNotConnected
	}

	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageBinary, out)
}

// applyVSU merges each vehicle's update into the cache. full_update replaces the
// entry; a partial update is merged field-wise (see mergeVSU).
func (ws *Websocket) applyVSU(vsus *pb.VehicleStatusUpdates) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	for vin, upd := range vsus.GetVehicleStatusUpdates() {
		if upd == nil {
			continue
		}

		s, ok := ws.states[vin]
		if !ok {
			s = &wsState{}
			ws.states[vin] = s
		}

		if upd.GetFullUpdate() || s.vsu == nil {
			// Clone so the cache owns its copy and later merges don't mutate the
			// received message.
			s.vsu = proto.Clone(upd).(*pb.VehicleStatusUpdate)
		} else {
			// Merge into a fresh clone and swap the pointer atomically. A reader
			// that obtained the previous *VehicleStatusUpdate from Status() keeps
			// an immutable snapshot, so no lock is needed on the read side beyond
			// fetching the pointer.
			merged := proto.Clone(s.vsu).(*pb.VehicleStatusUpdate)
			mergeVSU(merged, upd)
			s.vsu = merged
		}
		s.updated = time.Now()

		ws.log.TRACE.Printf("websocket: VSU %s full=%v", vin, upd.GetFullUpdate())
	}
}

// applyCommandStatus caches the latest command result per VIN and logs its
// state/errors so a command's outcome (ENQUEUED → PROCESSING → FINISHED/FAILED)
// is observable to SendCommand callers.
func (ws *Websocket) applyCommandStatus(upd *pb.AppTwinCommandStatusUpdatesByVIN) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	for vin, byPid := range upd.GetUpdatesByVin() {
		for pid, st := range byPid.GetUpdatesByPid() {
			if st == nil {
				continue
			}
			ws.commands[vin] = st

			ws.log.TRACE.Printf("websocket: command %s vin=%s pid=%d type=%s state=%s", st.GetRequestId(), vin, pid, st.GetType(), st.GetState())
			for _, e := range st.GetErrors() {
				ws.log.TRACE.Printf("websocket: command %s error code=%q message=%q", st.GetRequestId(), e.GetCode(), e.GetMessage())
			}
		}
	}
}

// markAlive records that the connection just proved itself alive (a successful
// ping or an incoming message).
func (ws *Websocket) markAlive() {
	ws.mu.Lock()
	ws.lastAlive = time.Now()
	ws.mu.Unlock()
}

// alive returns the last time the connection proved itself alive. The provider's
// freshness guard treats the cached snapshot as current while this is recent.
func (ws *Websocket) alive() time.Time {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.lastAlive
}

// rateLimitError marks a handshake rejected with HTTP 429. retryAfter carries
// the server's Retry-After hint when present (zero otherwise).
type rateLimitError struct {
	retryAfter time.Duration
}

func (e *rateLimitError) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("handshake rate limited (429), retry after %s", e.retryAfter)
	}
	return "handshake rate limited (429)"
}

// dialError classifies a websocket.Dial failure. A 429 handshake response is
// wrapped as a rateLimitError so run() applies the longer rate-limit cooldown;
// every other failure is returned unchanged.
func dialError(err error, resp *http.Response) error {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		return &rateLimitError{retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	return err
}

// parseRetryAfter parses a Retry-After header (delta-seconds or HTTP-date) into
// a positive duration, returning 0 when it is absent or unparseable.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// mapToHeader converts a header map to http.Header for the dialer.
func mapToHeader(m map[string]string) map[string][]string {
	h := make(map[string][]string, len(m))
	for k, v := range m {
		h[k] = []string{v}
	}
	return h
}
