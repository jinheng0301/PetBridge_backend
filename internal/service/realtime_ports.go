package service

import (
	"context"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// Conn is one WebSocket connection as the hub sees it. The handler owns the
// socket; Send must not block (it queues, and reports false when the queue is
// full) and Close must not call back into the hub synchronously.
type Conn interface {
	ID() string
	UserID() string
	Send(f model.Frame) bool
	Close(code int, reason string)
}

// Bus numbers room events and carries them between api nodes
// (realtime.Bus in production).
type Bus interface {
	NextSeq(ctx context.Context, roomID string) (int64, error)
	CurrentSeq(ctx context.Context, roomID string) (int64, error)
	Publish(ctx context.Context, payload []byte) error
	Run(ctx context.Context, onMessage func([]byte), onReconnect func())
}

// PresenceStore tracks live connections per user (realtime.PresenceStore in
// production). Add and Remove return the user's live count right after their
// own change, atomically, so callers decide first/last from that count.
type PresenceStore interface {
	Add(ctx context.Context, userID, connID string, now time.Time) (int, error)
	Remove(ctx context.Context, userID, connID string, now time.Time) (int, error)
	Count(ctx context.Context, userID string, now time.Time) (int, error)
}

// PresenceSweeper is the optional part of a PresenceStore that finds users
// whose connections all expired without being removed (their node died).
// Each such user is returned to exactly one caller.
type PresenceSweeper interface {
	Sweep(ctx context.Context, now time.Time) ([]string, error)
}

// RealtimeConfig holds the WebSocket protocol parameters (backend TRD 7.3, 9).
// They are constants in production; tests shorten the timings.
type RealtimeConfig struct {
	Heartbeat    time.Duration // server ping interval, also hello.heartbeat_sec
	PingTimeout  time.Duration
	PresenceTTL  time.Duration
	WriteTimeout time.Duration

	MaxConnsPerUser int
	MaxSubsPerConn  int
	ReadLimit       int64
	FramesPerSec    int
	SendQueue       int
	TapsPerSec      int
	SubsPerWindow   int           // room.sub frames allowed per SubWindow per connection
	SubWindow       time.Duration //
	OpTimeout       time.Duration // bound on the DB/Redis work behind one client frame

	MinAppVersion string
}

// DefaultRealtimeConfig returns the protocol values of backend TRD v1.4.
func DefaultRealtimeConfig(minAppVersion string) RealtimeConfig {
	return RealtimeConfig{
		Heartbeat:       25 * time.Second,
		PingTimeout:     10 * time.Second,
		PresenceTTL:     70 * time.Second,
		WriteTimeout:    5 * time.Second,
		MaxConnsPerUser: 3,
		MaxSubsPerConn:  2,
		ReadLimit:       4096,
		FramesPerSec:    10,
		SendQueue:       64,
		TapsPerSec:      2,
		SubsPerWindow:   10,
		SubWindow:       10 * time.Second,
		OpTimeout:       5 * time.Second,
		MinAppVersion:   minAppVersion,
	}
}
