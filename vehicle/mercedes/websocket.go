package mercedes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/coder/websocket"
	"github.com/evcc-io/evcc/cmd/shutdown"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/util"
	pb "github.com/evcc-io/evcc/vehicle/mercedes/pb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	// wsMinPollInterval floors the time between sessions. Mercedes blocks a token
	// set that opens the websocket too often, so the vehicle cache interval is
	// never allowed to drive the handshake rate higher than this.
	wsMinPollInterval = 5 * time.Minute

	// wsMinReconnectDelay is the initial retry delay after a failed session.
	wsMinReconnectDelay = 30 * time.Second

	// wsRateLimitBackoff is the initial cooldown after an HTTP 429 handshake
	// rejection that carries no Retry-After.
	wsRateLimitBackoff = 5 * time.Minute

	// wsMaxRateLimitBackoff caps the escalating 429 cooldown. The block only
	// clears after a sustained quiet period, so consecutive rejections back off
	// toward this cap rather than resetting a rolling-window block.
	wsMaxRateLimitBackoff = 30 * time.Minute

	// wsMaxMessageBytes bounds a single websocket frame. VSU full updates carry
	// ~250 attributes per vehicle; 8 MiB is comfortably above that.
	wsMaxMessageBytes = 8 << 20
)

// Session timing, variables so tests can shorten them.
var (
	// wsLinger is how long a session stays open after the last message. The full
	// update arrives within a second of connecting; lingering briefly picks up
	// any trailing partial updates before closing.
	wsLinger = 30 * time.Second

	// wsMaxSession caps a single session. A charging car keeps pushing partial
	// updates, which would otherwise keep the idle timer from ever firing.
	// Mercedes blocks accounts whose total websocket open time exceeds roughly
	// 16-20h per day, summed over all clients, so sessions must stay short.
	wsMaxSession = 2 * time.Minute
)

// wsState is one vehicle's cached VSU together with the time it was last
// updated, used by the provider's freshness guard.
type wsState struct {
	vsu     *pb.VehicleStatusUpdate
	updated time.Time
}

// Websocket polls the VSU push endpoint per Mercedes account and caches the
// latest VehicleStatusUpdate per VIN. Instead of holding the socket open, it
// opens a short session every poll interval: the backend sends a full update on
// connect, so a brief session yields a complete snapshot while keeping the
// account's daily websocket open time far below Mercedes' throttling limit.
type Websocket struct {
	mu        sync.RWMutex
	log       *util.Logger
	identity  *Identity
	region    string
	sessionID string
	states    map[string]*wsState                 // keyed by VIN
	commands  map[string]*pb.AppTwinCommandStatus // latest command result, keyed by VIN
	cancel    context.CancelFunc
	uri       string
	interval  time.Duration

	// rateLimit tracks consecutive 429 handshake rejections. It is persisted so
	// a restart honors the pending cooldown instead of dialing straight away.
	// Only touched from the run goroutine.
	rateLimit rateLimitState

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
// starting it on first use. Subsequent vehicles on the same account reuse it,
// and the first vehicle's interval sets the poll interval.
func NewWebsocket(log *util.Logger, identity *Identity, interval time.Duration) *Websocket {
	wsMu.Lock()
	defer wsMu.Unlock()

	key := identity.account + "-" + identity.region
	if ws, ok := websockets[key]; ok {
		return ws
	}

	ctx, cancel := context.WithCancel(context.Background())

	ws := &Websocket{
		log:       log,
		identity:  identity,
		region:    identity.region,
		sessionID: uuid.New().String(),
		states:    make(map[string]*wsState),
		commands:  make(map[string]*pb.AppTwinCommandStatus),
		cancel:    cancel,
		uri:       getWebsocketUri(identity.region),
		interval:  max(interval, wsMinPollInterval),
	}

	// Process-lifetime goroutine. api.Vehicle has no Close(); the account
	// registry guarantees at most one manager per account.
	go ws.run(ctx)
	shutdown.Register(ws.shutdown)

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
// asynchronous result via commandStatus. It does not open a session: between
// sessions it returns errNotConnected.
func (ws *Websocket) sendCommand(ctx context.Context, req *pb.CommandRequest) (string, error) {
	if req.GetRequestId() == "" {
		req.RequestId = uuid.New().String()
	}
	return req.GetRequestId(), ws.writeMessage(ctx, &pb.ClientMessage{
		Msg: &pb.ClientMessage_CommandRequest{CommandRequest: req},
	})
}

// run opens a session every poll interval. A failed session is retried with
// exponential backoff up to the poll interval. A handshake rejected with HTTP
// 429 escalates its own cooldown (see rateLimitCooldown) so a sustained block
// gets an increasingly quiet window to clear.
func (ws *Websocket) run(ctx context.Context) {
	bo := backoff.NewExponentialBackOff(
		backoff.WithMaxElapsedTime(0),
		backoff.WithInitialInterval(wsMinReconnectDelay),
		backoff.WithMaxInterval(ws.interval),
	)

	ws.rateLimit = loadRateLimit(ws.rateLimitKey())
	if delay := time.Until(ws.rateLimit.Until); delay > 0 {
		ws.log.DEBUG.Printf("websocket: resuming 429 cooldown x%d, waiting %s", ws.rateLimit.Count, delay.Round(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}

	for ctx.Err() == nil {
		started := time.Now()
		err := ws.session(ctx)

		var delay time.Duration
		var rl *rateLimitError
		switch {
		case err == nil:
			bo.Reset()
			delay = ws.interval - time.Since(started)

		case errors.As(err, &rl):
			ws.rateLimit.Count++
			delay = rateLimitCooldown(ws.rateLimit.Count, rl.retryAfter)
			ws.rateLimit.Until = time.Now().Add(delay)
			ws.saveRateLimit()
			ws.log.DEBUG.Printf("websocket: handshake rate limited (429) x%d, backing off %s", ws.rateLimit.Count, delay.Round(time.Second))

		default:
			if !errors.Is(err, context.Canceled) {
				ws.log.DEBUG.Printf("websocket: %v", err)
			}
			delay = bo.NextBackOff()
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

// session dials the websocket, reads until it has been quiet for wsLinger or
// wsMaxSession has elapsed, and then closes it with a normal close frame. It
// returns nil only if at least one vehicle status update was received.
func (ws *Websocket) session(ctx context.Context) error {
	tok, err := ws.identity.Token()
	if err != nil {
		return err
	}

	headers := wsheaders(tok.AccessToken, ws.sessionID, ws.region)
	opts := &websocket.DialOptions{HTTPHeader: mapToHeader(headers)}

	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, ws.uri, opts)
	cancel()
	if err != nil {
		return dialError(err, resp)
	}
	conn.SetReadLimit(wsMaxMessageBytes)

	if ws.rateLimit.Count > 0 {
		ws.rateLimit = rateLimitState{}
		ws.saveRateLimit()
	}

	ws.connMu.Lock()
	ws.conn = conn
	ws.connMu.Unlock()

	defer func() {
		ws.connMu.Lock()
		ws.conn = nil
		ws.connMu.Unlock()
		_ = conn.CloseNow()
	}()

	ws.log.TRACE.Println("websocket: connected")

	// Closing from a timer rather than via a read deadline sends a close frame;
	// an expired read context would drop the connection without one.
	var closing atomic.Bool
	closeNormal := func() {
		closing.Store(true)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	idle := time.AfterFunc(wsLinger, closeNormal)
	defer idle.Stop()
	limit := time.AfterFunc(wsMaxSession, closeNormal)
	defer limit.Stop()

	var updated bool
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			switch {
			case !closing.Load():
				return err
			case !updated:
				return errors.New("no vehicle status update received")
			default:
				return nil
			}
		}
		idle.Reset(wsLinger)

		if typ != websocket.MessageBinary {
			continue
		}

		vsu, err := ws.handleMessage(ctx, data)
		if err != nil {
			ws.log.DEBUG.Printf("websocket: handle: %v", err)
		}
		updated = updated || vsu
	}
}

// shutdown closes the connection with a normal close frame so the backend
// releases the session right away instead of waiting for it to time out, then
// stops the run loop. Close must come first: cancelling the read context would
// tear the connection down without a close handshake.
func (ws *Websocket) shutdown() {
	ws.connMu.Lock()
	conn := ws.conn
	ws.connMu.Unlock()

	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	ws.cancel()
}

// handleMessage decodes a PushMessage and acknowledges it. Every
// sequence-numbered push type must be acknowledged or the backend redelivers it
// and eventually drops the connection, so both vehicle_status_updates (the data
// we cache) and apptwin_command_status_updates (results of commands we sent) are
// handled and acked. Other push types are ignored. The bool reports whether the
// message carried vehicle status updates.
func (ws *Websocket) handleMessage(ctx context.Context, data []byte) (bool, error) {
	var msg pb.PushMessage
	if err := proto.Unmarshal(data, &msg); err != nil {
		return false, err
	}

	var vsu bool
	var ack *pb.ClientMessage
	switch {
	case msg.GetVehicleStatusUpdates() != nil:
		vsus := msg.GetVehicleStatusUpdates()
		ws.applyVSU(vsus)
		vsu = true
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
		return false, nil
	}

	return vsu, ws.writeMessage(ctx, ack)
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

// rateLimitState is the persisted 429 backoff: consecutive rejections and the
// earliest time the next handshake may be attempted.
type rateLimitState struct {
	Count int       `json:"count"`
	Until time.Time `json:"until"`
}

func (ws *Websocket) rateLimitKey() string {
	return fmt.Sprintf("mercedes.%s-%s.ratelimit", ws.identity.account, ws.region)
}

// loadRateLimit returns the persisted 429 backoff, or the zero state if none.
func loadRateLimit(key string) rateLimitState {
	var res rateLimitState
	if err := settings.Json(key, &res); err != nil {
		return rateLimitState{}
	}
	return res
}

func (ws *Websocket) saveRateLimit() {
	if err := settings.SetJson(ws.rateLimitKey(), ws.rateLimit); err != nil {
		ws.log.DEBUG.Printf("websocket: persist rate limit: %v", err)
	}
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
