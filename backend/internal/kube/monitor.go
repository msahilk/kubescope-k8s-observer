package kube

import (
	"context"
	"sync"
	"time"
)

// Health is the connectivity state tracked by Monitor.
type Health struct {
	Reachable     bool
	ServerVersion string
	LastSuccessAt *time.Time
	LastCheckedAt *time.Time
	Error         string
}

// Monitor periodically pings the API server (GET /version) so the UI can show
// "unreachable" instead of an empty, healthy-looking cluster.
type Monitor struct {
	c        *Client
	interval time.Duration
	onChange func(Health)

	mu sync.Mutex
	h  Health
}

// NewMonitor creates a monitor. onChange is called when reachability flips.
func NewMonitor(c *Client, interval time.Duration, onChange func(Health)) *Monitor {
	return &Monitor{c: c, interval: interval, onChange: onChange}
}

// Health returns the latest state.
func (m *Monitor) Health() Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.h
}

// MarkSuccess records a successful API interaction observed elsewhere (e.g. a snapshot).
func (m *Monitor) MarkSuccess() {
	m.update(true, m.Health().ServerVersion, "")
}

// MarkFailure records a failed API interaction observed elsewhere.
func (m *Monitor) MarkFailure(err error) {
	m.update(false, m.Health().ServerVersion, err.Error())
}

func (m *Monitor) update(ok bool, version, errMsg string) {
	now := time.Now().UTC()
	m.mu.Lock()
	changed := m.h.Reachable != ok || m.h.LastCheckedAt == nil
	m.h.Reachable = ok
	m.h.LastCheckedAt = &now
	m.h.Error = errMsg
	if ok {
		m.h.LastSuccessAt = &now
		if version != "" {
			m.h.ServerVersion = version
		}
	}
	h := m.h
	m.mu.Unlock()
	if changed && m.onChange != nil {
		m.onChange(h)
	}
}

// Check pings once.
func (m *Monitor) Check(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := m.c.Ping(cctx)
	if err != nil {
		m.update(false, "", err.Error())
		return
	}
	m.update(true, v, "")
}

// Run pings until ctx is done.
func (m *Monitor) Run(ctx context.Context) {
	m.Check(ctx)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Check(ctx)
		}
	}
}
