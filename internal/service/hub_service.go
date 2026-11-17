package service

import (
	"errors"
	"sync"

	"github.com/jinheng0301/petbridge/internal/model"
)

// ErrTooManySubs means the connection already holds MaxSubsPerConn rooms.
var ErrTooManySubs = errors.New("too many room subscriptions")

// Hub is this node's table of WebSocket connections and their room
// subscriptions. It decides who gets each event (backend TRD 7.3):
// room-scoped events go only to recipients subscribed to the room, personal
// events to every connection of each recipient. Sends happen under the lock,
// which keeps a connection's frames in order; Send never blocks, and Close is
// always called after the lock is released.
type Hub struct {
	cfg RealtimeConfig

	mu     sync.Mutex
	down   bool // Shutdown ran; late connections are turned away
	conns  map[Conn]*connState
	byUser map[string][]Conn // registration order, oldest first
}

type connState struct {
	subs map[string]*subState
}

// subState is pending between BeginSub and EndSub; its events are buffered
// until the snapshot has been sent.
type subState struct {
	pending bool
	buf     []model.Frame
}

// closeReq is a Close to run once the lock is released.
type closeReq struct {
	c      Conn
	code   int
	reason string
}

func NewHub(cfg RealtimeConfig) *Hub {
	return &Hub{cfg: cfg, conns: map[Conn]*connState{}, byUser: map[string][]Conn{}}
}

func runCloses(reqs []closeReq) {
	for _, r := range reqs {
		r.c.Close(r.code, r.reason)
	}
}

// Register adds c. When the user then holds more than MaxConnsPerUser
// connections here, the oldest is closed with 4001.
func (h *Hub) Register(c Conn) {
	h.mu.Lock()
	if h.down {
		h.mu.Unlock()
		c.Send(model.Frame{T: model.FrameServerRestart})
		c.Close(model.CloseGoingAway, "restart")
		return
	}
	h.conns[c] = &connState{subs: map[string]*subState{}}
	u := c.UserID()
	h.byUser[u] = append(h.byUser[u], c)
	var closes []closeReq
	for len(h.byUser[u]) > h.cfg.MaxConnsPerUser {
		oldest := h.byUser[u][0]
		h.removeLocked(oldest)
		closes = append(closes, closeReq{oldest, model.CloseReplaced, "replaced"})
	}
	h.mu.Unlock()
	runCloses(closes)
}

// Unregister removes c and its subscriptions; unknown connections are ignored.
func (h *Hub) Unregister(c Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(c)
}

func (h *Hub) removeLocked(c Conn) {
	if _, ok := h.conns[c]; !ok {
		return
	}
	delete(h.conns, c)
	u := c.UserID()
	list := h.byUser[u]
	for i, x := range list {
		if x == c {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(h.byUser, u)
	} else {
		h.byUser[u] = list
	}
}

// LocalCount is how many connections the user has on this node.
func (h *Hub) LocalCount(userID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.byUser[userID])
}

// BeginSub starts (or restarts) a subscription: events for the room are
// buffered until EndSub sends the snapshot.
func (h *Hub) BeginSub(c Conn, roomID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.conns[c]
	if !ok {
		return nil
	}
	if _, held := st.subs[roomID]; !held && len(st.subs) >= h.cfg.MaxSubsPerConn {
		return ErrTooManySubs
	}
	st.subs[roomID] = &subState{pending: true}
	return nil
}

// EndSub sends the snapshot, then the buffered events newer than it (events
// without a seq are always kept), and switches the room to direct delivery.
func (h *Hub) EndSub(c Conn, roomID string, snap model.RoomSnapshot) {
	h.mu.Lock()
	st, ok := h.conns[c]
	var sub *subState
	if ok {
		sub = st.subs[roomID]
	}
	if sub == nil || !sub.pending {
		h.mu.Unlock()
		return
	}
	frames := append([]model.Frame{{T: model.FrameRoomSnapshot, D: snap}}, sub.buf...)
	sub.pending, sub.buf = false, nil
	var closes []closeReq
	for _, f := range frames {
		if f.Seq != 0 && f.Seq <= snap.Seq {
			continue
		}
		if !c.Send(f) {
			h.removeLocked(c)
			closes = append(closes, closeReq{c, model.ClosePolicy, "slow consumer"})
			break
		}
	}
	h.mu.Unlock()
	runCloses(closes)
}

// CancelSub drops a subscription that failed before its snapshot.
func (h *Hub) CancelSub(c Conn, roomID string) { h.Unsub(c, roomID) }

// Unsub ends a subscription.
func (h *Hub) Unsub(c Conn, roomID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.conns[c]; ok {
		delete(st.subs, roomID)
	}
}

// Subscribed reports whether c holds a (possibly pending) subscription to the room.
func (h *Hub) Subscribed(c Conn, roomID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.conns[c]
	if !ok {
		return false
	}
	_, held := st.subs[roomID]
	return held
}

// Deliver hands e (with seq, 0 for none) to this node's connections.
func (h *Hub) Deliver(e Event, seq int64) {
	f := model.Frame{T: e.Type, Seq: seq, D: e.Data}
	h.mu.Lock()
	var closes []closeReq
	for _, u := range e.To {
		for _, c := range append([]Conn(nil), h.byUser[u]...) {
			st := h.conns[c]
			if e.Kind == KindPersonal {
				if !c.Send(f) {
					h.removeLocked(c)
					closes = append(closes, closeReq{c, model.ClosePolicy, "slow consumer"})
					continue
				}
				if e.Type == EventRoomKicked {
					delete(st.subs, e.RoomID)
				}
				continue
			}
			sub := st.subs[e.RoomID]
			switch {
			case sub == nil:
			case sub.pending && len(sub.buf) >= h.cfg.SendQueue:
				// The snapshot is stuck; holding more would grow without bound.
				h.removeLocked(c)
				closes = append(closes, closeReq{c, model.ClosePolicy, "subscription backlog"})
			case sub.pending:
				sub.buf = append(sub.buf, f)
			case !c.Send(f):
				h.removeLocked(c)
				closes = append(closes, closeReq{c, model.ClosePolicy, "slow consumer"})
			}
		}
	}
	h.mu.Unlock()
	runCloses(closes)
}

// Broadcast sends f to every connection on this node.
func (h *Hub) Broadcast(f model.Frame) {
	h.mu.Lock()
	var closes []closeReq
	for c := range h.conns {
		if !c.Send(f) {
			h.removeLocked(c)
			closes = append(closes, closeReq{c, model.ClosePolicy, "slow consumer"})
		}
	}
	h.mu.Unlock()
	runCloses(closes)
}

// Shutdown tells every connection the server is restarting and closes it with 1001.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	var all []Conn
	for c := range h.conns {
		all = append(all, c)
	}
	h.conns, h.byUser = map[Conn]*connState{}, map[string][]Conn{}
	h.down = true
	h.mu.Unlock()
	for _, c := range all {
		c.Send(model.Frame{T: model.FrameServerRestart})
		c.Close(model.CloseGoingAway, "restart")
	}
}
