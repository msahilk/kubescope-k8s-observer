// Package stream is a small in-memory pub/sub hub for Server-Sent Events.
//
// Every published event gets a monotonically increasing sequence id. A bounded
// ring buffer allows clients that reconnect with Last-Event-ID to replay what
// they missed; if the gap is larger than the buffer, the client is told to
// resync (refetch /snapshot) instead of silently missing events.
package stream

import (
	"sync"
	"time"

	"github.com/sahilkamil/kubescope/backend/internal/models"
)

// Hub fans out events to subscribers.
type Hub struct {
	mu      sync.Mutex
	seq     uint64
	buf     []models.StreamEvent
	size    int
	subs    map[*Sub]struct{}
	subSize int
	now     func() time.Time
}

// Sub is one subscriber. C is closed when the subscriber falls too far behind
// (the client must reconnect and will get a resync) or is unsubscribed.
type Sub struct {
	C      chan models.StreamEvent
	closed bool
}

// NewHub creates a hub with a replay buffer of `size` events.
func NewHub(size int) *Hub {
	if size < 16 {
		size = 16
	}
	return &Hub{size: size, subs: map[*Sub]struct{}{}, subSize: 256, now: func() time.Time { return time.Now().UTC() }}
}

// Seq returns the latest assigned sequence id.
func (h *Hub) Seq() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// Publish assigns a sequence id and delivers the event.
func (h *Hub) Publish(typ string, data any) models.StreamEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev := models.StreamEvent{Seq: h.seq, Type: typ, At: h.now(), Data: data}
	h.buf = append(h.buf, ev)
	if len(h.buf) > h.size {
		h.buf = h.buf[len(h.buf)-h.size:]
	}
	for s := range h.subs {
		select {
		case s.C <- ev:
		default:
			// Slow consumer: drop it. The client reconnects with Last-Event-ID
			// and either replays from the buffer or is told to resync.
			h.closeLocked(s)
		}
	}
	return ev
}

// Subscribe registers a subscriber. If lastSeen > 0, buffered events after it
// are returned for replay; resync is true when events were lost (the gap is
// older than the buffer, or lastSeen is from a previous server process).
func (h *Hub) Subscribe(lastSeen uint64) (s *Sub, replay []models.StreamEvent, resync bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s = &Sub{C: make(chan models.StreamEvent, h.subSize)}
	h.subs[s] = struct{}{}
	if lastSeen == 0 {
		return s, nil, false
	}
	if lastSeen > h.seq {
		return s, nil, true // id from an earlier server run
	}
	if len(h.buf) == 0 || h.buf[0].Seq > lastSeen+1 {
		return s, nil, lastSeen < h.seq
	}
	for _, ev := range h.buf {
		if ev.Seq > lastSeen {
			replay = append(replay, ev)
		}
	}
	return s, replay, false
}

// Unsubscribe removes a subscriber.
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeLocked(s)
}

func (h *Hub) closeLocked(s *Sub) {
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
	}
	if !s.closed {
		s.closed = true
		close(s.C)
	}
}

// Subscribers returns the number of active subscribers.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
