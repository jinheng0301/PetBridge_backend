package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/service"
)

const protoVersion = 1

// uuidRE matches a lower-case UUID; room ids that do not match cannot exist,
// so they are refused before any DB or Redis work.
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// TokenExpiry reads when a verified access token expires (service.Issuer).
type TokenExpiry interface {
	Expiry(token string) (time.Time, error)
}

// OnboardingChecker reports whether a user finished onboarding
// (repository.UserRepo satisfies it).
type OnboardingChecker interface {
	State(ctx context.Context, userID string) (status string, onboarded bool, err error)
}

// WsHandler serves GET /api/v1/ws (backend TRD 7.3, 9).
type WsHandler struct {
	hub      *service.Hub
	presence *service.PresenceService
	rt       *service.RealtimeService
	users    OnboardingChecker
	tokens   TokenExpiry
	cfg      service.RealtimeConfig
	authn    gin.HandlerFunc
	ipLimit  gin.HandlerFunc // optional, runs before authn
	usrLimit gin.HandlerFunc // optional, runs after authn
	log      *slog.Logger

	mu     sync.Mutex
	down   bool           // Shutdown started; new upgrades are refused
	active sync.WaitGroup // running connections; Add only under mu while !down
}

func NewWsHandler(hub *service.Hub, presence *service.PresenceService, rt *service.RealtimeService,
	users OnboardingChecker, tokens TokenExpiry, cfg service.RealtimeConfig, authn gin.HandlerFunc) *WsHandler {
	return &WsHandler{hub: hub, presence: presence, rt: rt, users: users, tokens: tokens, cfg: cfg,
		authn: authn, log: slog.Default()}
}

// WithUpgradeLimit rate-limits upgrades per IP (before auth) and per user (after it).
func (h *WsHandler) WithUpgradeLimit(perIP, perUser gin.HandlerFunc) *WsHandler {
	h.ipLimit, h.usrLimit = perIP, perUser
	return h
}

// WithLogger sets where server-side failures (internal errors, panics) go.
func (h *WsHandler) WithLogger(l *slog.Logger) *WsHandler {
	h.log = l
	return h
}

// Routes implements server.Module.
func (h *WsHandler) Routes(v1 *gin.RouterGroup) {
	var chain []gin.HandlerFunc
	if h.ipLimit != nil {
		chain = append(chain, h.ipLimit)
	}
	chain = append(chain, h.authn)
	if h.usrLimit != nil {
		chain = append(chain, h.usrLimit)
	}
	v1.GET("/ws", append(chain, h.serve)...)
}

// bearer returns the token Auth already verified.
func bearer(c *gin.Context) string {
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

// serve checks onboarding, upgrades, and runs the connection until it closes.
func (h *WsHandler) serve(c *gin.Context) {
	uid := UserID(c)
	_, onboarded, err := h.users.State(c.Request.Context(), uid)
	if err != nil {
		_ = c.Error(err)
		Fail(c, http.StatusInternalServerError, model.CodeInternal, "internal error")
		return
	}
	if !onboarded {
		Fail(c, http.StatusForbidden, model.CodeOnboardingRequired, "onboarding required")
		return
	}
	// Auth checks the token only here; the connection must not outlive it,
	// or a logged-out or stolen token would keep receiving room events.
	exp, err := h.tokens.Expiry(bearer(c))
	if err != nil {
		Fail(c, http.StatusUnauthorized, model.CodeUnauthorized, "unauthorized")
		return
	}
	h.mu.Lock()
	if h.down {
		h.mu.Unlock()
		Fail(c, http.StatusServiceUnavailable, model.CodeNotReady, "server restarting")
		return
	}
	h.active.Add(1)
	h.mu.Unlock()
	defer h.active.Done()

	// http.Server.ReadTimeout does not cut the socket: net/http clears the
	// connection's deadlines when Accept hijacks it
	// (TestWS_IdleConnSurvivesReadTimeout).
	ws, err := websocket.Accept(c.Writer, c.Request, nil)
	if err != nil {
		return // Accept has already written the HTTP error
	}
	ws.SetReadLimit(h.cfg.ReadLimit)

	// The request context ends when this handler returns; the connection
	// gets its own, cancelled once everything below has stopped.
	ctx, cancel := context.WithCancel(context.WithoutCancel(c.Request.Context()))
	defer cancel()
	conn := newWsConn(newConnID(), uid, ws, h.cfg)

	// hello is queued before the hub can route anything to this connection,
	// so it is always the first frame.
	conn.Send(model.Frame{T: model.FrameHello, D: map[string]any{
		"server_time":     time.Now().UTC().Format(time.RFC3339),
		"heartbeat_sec":   int(h.cfg.Heartbeat / time.Second),
		"proto":           protoVersion,
		"min_app_version": h.cfg.MinAppVersion,
	}})
	h.hub.Register(conn)
	h.presence.Connected(ctx, uid, conn.id)
	expiry := time.AfterFunc(time.Until(exp), func() { conn.Close(model.CloseTokenExpired, "token expired") })
	defer expiry.Stop()

	writerDone, pingDone := make(chan struct{}), make(chan struct{})
	pingCtx, stopPing := context.WithCancel(ctx)
	go func() { h.guard(conn, "write", func() { conn.writeLoop(ctx) }); close(writerDone) }()
	go func() { h.guard(conn, "ping", func() { h.pingLoop(pingCtx, conn) }); close(pingDone) }()
	go h.guard(conn, "read", func() { h.readLoop(ctx, conn) })

	<-conn.closing
	// The ping loop must be gone before presence is removed, or a late
	// Refresh would bring this connection back to life for 70 seconds.
	stopPing()
	<-pingDone
	h.hub.Unregister(conn)
	h.presence.Disconnected(ctx, uid, conn.id)
	<-writerDone
}

// guard keeps a panic in one connection's goroutine from taking the process
// down (these run outside gin's Recovery): it logs and closes the connection.
func (h *WsHandler) guard(conn *wsConn, loop string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("websocket goroutine panicked", "loop", loop, "panic", fmt.Sprint(r))
			conn.Close(int(websocket.StatusInternalError), "internal error")
			conn.ws.CloseNow()
		}
	}()
	f()
}

func newConnID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pingLoop pings every Heartbeat; a successful ping refreshes presence, a
// missed one closes the connection (the client is gone without saying so).
func (h *WsHandler) pingLoop(ctx context.Context, conn *wsConn) {
	t := time.NewTicker(h.cfg.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-conn.closing:
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, h.cfg.PingTimeout)
		err := conn.ws.Ping(pctx)
		cancel()
		if ctx.Err() != nil {
			return // stopped by serve
		}
		if err != nil {
			conn.Close(int(websocket.StatusGoingAway), "ping timeout")
			return
		}
		h.presence.Refresh(ctx, conn.user, conn.id)
	}
}

// readLoop parses client frames until the socket fails.
func (h *WsHandler) readLoop(ctx context.Context, conn *wsConn) {
	windowStart, frames := time.Now(), 0
	tapStart, taps := time.Now(), 0
	subStart, subs := time.Now(), 0
	for {
		typ, data, err := conn.ws.Read(ctx)
		if err != nil {
			conn.Close(int(websocket.StatusNormalClosure), "")
			return
		}
		if now := time.Now(); now.Sub(windowStart) >= time.Second {
			windowStart, frames = now, 0
		}
		if frames++; frames > h.cfg.FramesPerSec {
			conn.Close(model.ClosePolicy, "too many frames")
			return
		}
		var f model.ClientFrame
		if typ != websocket.MessageText || json.Unmarshal(data, &f) != nil {
			conn.sendError("", model.CodeValidationFailed)
			continue
		}
		switch f.T {
		case model.FrameRoomSub:
			if now := time.Now(); now.Sub(subStart) >= h.cfg.SubWindow {
				subStart, subs = now, 0
			}
			// Each room.sub costs DB and Redis work; a client resubscribing
			// in a loop is told to slow down.
			if subs++; subs > h.cfg.SubsPerWindow {
				conn.sendError(f.ID, model.CodeRateLimited)
				continue
			}
			h.roomSub(ctx, conn, f)
		case model.FrameRoomUnsub:
			var d struct {
				RoomID string `json:"room_id"`
			}
			if json.Unmarshal(f.D, &d) != nil || d.RoomID == "" {
				conn.sendError(f.ID, model.CodeValidationFailed)
				continue
			}
			h.hub.Unsub(conn, strings.ToLower(d.RoomID))
		case model.FramePetTap:
			if now := time.Now(); now.Sub(tapStart) >= time.Second {
				tapStart, taps = now, 0
			}
			taps++
			h.petTap(ctx, conn, f, taps > h.cfg.TapsPerSec)
		default:
			conn.sendError(f.ID, model.CodeValidationFailed)
		}
	}
}

func (h *WsHandler) roomSub(ctx context.Context, conn *wsConn, f model.ClientFrame) {
	var d struct {
		RoomID  string `json:"room_id"`
		LastSeq int64  `json:"last_seq"` // accepted, unused (TRD v1.4)
	}
	if json.Unmarshal(f.D, &d) != nil || d.RoomID == "" {
		conn.sendError(f.ID, model.CodeValidationFailed)
		return
	}
	room := strings.ToLower(d.RoomID)
	if !uuidRE.MatchString(room) {
		conn.sendError(f.ID, model.CodeRoomNotFound)
		return
	}
	if err := h.hub.BeginSub(conn, room); err != nil {
		conn.sendError(f.ID, model.CodeValidationFailed)
		return
	}
	octx, cancel := context.WithTimeout(ctx, h.cfg.OpTimeout)
	snap, err := h.rt.Snapshot(octx, conn.user, room)
	cancel()
	if err != nil {
		h.hub.CancelSub(conn, room)
		conn.sendError(f.ID, h.errCode("room.sub", err))
		return
	}
	h.hub.EndSub(conn, room, snap)
}

func (h *WsHandler) petTap(ctx context.Context, conn *wsConn, f model.ClientFrame, overLimit bool) {
	var d struct {
		RoomID string `json:"room_id"`
		Target string `json:"target_user_id"`
	}
	if json.Unmarshal(f.D, &d) != nil || d.RoomID == "" || d.Target == "" {
		conn.sendError(f.ID, model.CodeValidationFailed)
		return
	}
	room := strings.ToLower(d.RoomID)
	if !uuidRE.MatchString(room) || !h.hub.Subscribed(conn, room) {
		conn.sendError(f.ID, model.CodeRoomNotFound)
		return
	}
	if overLimit {
		return // ordinary rapid tapping: drop silently
	}
	octx, cancel := context.WithTimeout(ctx, h.cfg.OpTimeout)
	defer cancel()
	if err := h.rt.PetTap(octx, conn.user, room, d.Target); err != nil {
		conn.sendError(f.ID, h.errCode("pet.tap", err))
	}
}

// errCode maps a service error to an error-frame code, logging unexpected ones.
func (h *WsHandler) errCode(op string, err error) string {
	if errors.Is(err, service.ErrRoomNotFound) {
		return model.CodeRoomNotFound
	}
	h.log.Error("websocket request failed", "op", op, "error", err.Error())
	return model.CodeInternal
}

// wsConn implements service.Conn. Frames go through a bounded queue drained
// by writeLoop, which also owns closing the socket so that frames queued
// before Close (server.restart) still go out first.
type wsConn struct {
	id, user string
	ws       *websocket.Conn
	cfg      service.RealtimeConfig
	send     chan model.Frame

	once        sync.Once
	closing     chan struct{}
	closeCode   int
	closeReason string
}

func newWsConn(id, user string, ws *websocket.Conn, cfg service.RealtimeConfig) *wsConn {
	return &wsConn{id: id, user: user, ws: ws, cfg: cfg,
		send: make(chan model.Frame, cfg.SendQueue), closing: make(chan struct{})}
}

func (c *wsConn) ID() string     { return c.id }
func (c *wsConn) UserID() string { return c.user }

// Send queues f; false when the queue is full. After Close it drops f.
func (c *wsConn) Send(f model.Frame) bool {
	select {
	case <-c.closing:
		return true
	default:
	}
	select {
	case c.send <- f:
		return true
	default:
		return false
	}
}

// Close asks writeLoop to flush and close; only the first call counts.
func (c *wsConn) Close(code int, reason string) {
	c.once.Do(func() {
		c.closeCode, c.closeReason = code, reason
		close(c.closing)
	})
}

func (c *wsConn) sendError(ref, code string) {
	c.Send(model.Frame{T: model.FrameError, D: map[string]any{"ref": ref, "code": code}})
}

func (c *wsConn) write(ctx context.Context, f model.Frame) error {
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	return wsjson.Write(wctx, c.ws, f)
}

func (c *wsConn) writeLoop(ctx context.Context) {
	for {
		select {
		case f := <-c.send:
			if err := c.write(ctx, f); err != nil {
				c.Close(int(websocket.StatusGoingAway), "write failed")
				c.ws.CloseNow()
				return
			}
		case <-c.closing:
			for {
				select {
				case f := <-c.send:
					if c.write(ctx, f) != nil {
						c.ws.CloseNow()
						return
					}
					continue
				default:
				}
				break
			}
			_ = c.ws.Close(websocket.StatusCode(c.closeCode), c.closeReason)
			return
		}
	}
}

// Shutdown sends server.restart, closes every connection with 1001 and waits
// (up to timeout) until each has finished, presence cleanup included. Run it
// before http.Server.Shutdown, which does not wait for hijacked connections.
func (h *WsHandler) Shutdown(timeout time.Duration) {
	h.mu.Lock()
	h.down = true
	h.mu.Unlock()
	h.hub.Shutdown()
	done := make(chan struct{})
	go func() { h.active.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
