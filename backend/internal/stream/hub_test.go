package stream

import "testing"

func TestSeqReplayResync(t *testing.T) {
	h := NewHub(16)
	for i := 0; i < 10; i++ {
		h.Publish("x", i)
	}
	if h.Seq() != 10 {
		t.Fatalf("seq=%d", h.Seq())
	}
	_, replay, resync := h.Subscribe(7)
	if resync || len(replay) != 3 || replay[0].Seq != 8 || replay[2].Seq != 10 {
		t.Fatalf("replay=%v resync=%v", replay, resync)
	}
	// Overflow the buffer: events 1..24 dropped from a 16-slot buffer.
	for i := 0; i < 30; i++ {
		h.Publish("x", i)
	}
	if _, _, resync := h.Subscribe(3); !resync {
		t.Fatal("gap older than buffer must require resync")
	}
	if _, _, resync := h.Subscribe(9999); !resync {
		t.Fatal("id from a previous server run must require resync")
	}
	if _, r, resync := h.Subscribe(h.Seq()); resync || len(r) != 0 {
		t.Fatal("up-to-date client needs nothing")
	}
}

func TestMonotonicAndSlowConsumer(t *testing.T) {
	h := NewHub(16)
	h.subSize = 2
	s, _, _ := h.Subscribe(0)
	var last uint64
	for i := 0; i < 5; i++ {
		ev := h.Publish("x", i)
		if ev.Seq <= last {
			t.Fatal("sequence ids must increase")
		}
		last = ev.Seq
	}
	n := 0
	for range s.C { // channel must be closed after overflow
		n++
	}
	if n != 2 || h.Subscribers() != 0 {
		t.Fatalf("got %d buffered, %d subscribers", n, h.Subscribers())
	}
}
