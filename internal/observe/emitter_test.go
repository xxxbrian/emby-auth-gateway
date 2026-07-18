package observe

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewEmitterDefaultBuffer(t *testing.T) {
	e := NewEmitter(0)
	if cap(e.ch) != defaultBuffer {
		t.Fatalf("default buffer: got %d want %d", cap(e.ch), defaultBuffer)
	}
	e2 := NewEmitter(-5)
	if cap(e2.ch) != defaultBuffer {
		t.Fatalf("negative buffer: got %d want %d", cap(e2.ch), defaultBuffer)
	}
}

func TestTryEmitAndReceive(t *testing.T) {
	e := NewEmitter(4)
	defer e.Close()

	ev := Event{Kind: KindRequest, RouteClass: RouteMetadata, Outcome: OutcomeOK}
	if !e.TryEmit(ev) {
		t.Fatal("expected emit success")
	}
	select {
	case got := <-e.Events():
		if got.Kind != KindRequest || got.RouteClass != RouteMetadata {
			t.Fatalf("unexpected event: %+v", got)
		}
		if got.At.IsZero() {
			t.Fatal("expected At to be filled")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestTryEmitDropsWhenFull(t *testing.T) {
	e := NewEmitter(2)
	defer e.Close()

	if !e.TryEmit(Event{Kind: KindRequest}) {
		t.Fatal("first emit")
	}
	if !e.TryEmit(Event{Kind: KindRequest}) {
		t.Fatal("second emit")
	}
	if e.TryEmit(Event{Kind: KindRequest}) {
		t.Fatal("expected drop when full")
	}
	if e.Drops() != 1 {
		t.Fatalf("drops: got %d want 1", e.Drops())
	}
	// still non-blocking after drop
	if e.TryEmit(Event{Kind: KindRequest}) {
		t.Fatal("expected continued drop while full")
	}
	if e.Drops() != 2 {
		t.Fatalf("drops: got %d want 2", e.Drops())
	}
}

func TestCloseIdempotentAndBlocksEmit(t *testing.T) {
	e := NewEmitter(4)
	e.Close()
	e.Close() // idempotent

	if e.TryEmit(Event{Kind: KindRequest}) {
		t.Fatal("emit after close should fail")
	}
	// channel should be closed
	_, ok := <-e.Events()
	if ok {
		t.Fatal("expected closed channel")
	}
}

func TestNilEmitterSafe(t *testing.T) {
	var e *Emitter
	if e.TryEmit(Event{Kind: KindRequest}) {
		t.Fatal("nil emit")
	}
	if e.Events() != nil {
		t.Fatal("nil events")
	}
	if e.Drops() != 0 {
		t.Fatal("nil drops")
	}
	e.Close()
}

func TestTryEmitCloseRace(t *testing.T) {
	e := NewEmitter(64)
	var emits, drops atomic.Uint64
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				if e.TryEmit(Event{Kind: KindRequest, Outcome: OutcomeOK}) {
					emits.Add(1)
				} else {
					drops.Add(1)
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Millisecond)
		e.Close()
	}()

	// drain until closed
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range e.Events() {
		}
	}()

	wg.Wait()
	if e.TryEmit(Event{Kind: KindRequest}) {
		t.Fatal("emit after close")
	}
	// no panic is success; counters should be consistent
	_ = emits.Load()
	_ = drops.Load()
	_ = e.Drops()
}

func TestClassifyRoute(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"POST", "/Users/AuthenticateByName", RouteAuth},
		{"POST", "/Sessions/Logout", RouteAuth},
		{"GET", "/Items/abc/PlaybackInfo", RoutePlayback},
		{"POST", "/Sessions/Playing/Progress", RoutePlayback},
		{"GET", "/Videos/item1/stream", RouteMedia},
		{"GET", "/Audio/item1/stream.mp3", RouteMedia},
		{"GET", "/Items/x/Download", RouteMedia},
		{"POST", "/Users/u/PlayedItems/i", RouteUserdata},
		{"POST", "/Users/u/FavoriteItems/i", RouteUserdata},
		{"GET", "/Users/u/Items", RouteMetadata},
		{"GET", "/System/Info/Public", RouteMetadata},
		{"GET", "/unknown/path", RouteOther},
		{"GET", "Items/x?api_key=secret", RouteMetadata},
	}
	for _, tc := range cases {
		if got := ClassifyRoute(tc.method, tc.path); got != tc.want {
			t.Fatalf("ClassifyRoute(%q,%q)=%q want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestStatusClassOf(t *testing.T) {
	cases := map[int]string{
		0:   Status0,
		-1:  Status0,
		200: Status2xx,
		204: Status2xx,
		301: Status3xx,
		404: Status4xx,
		502: Status5xx,
	}
	for status, want := range cases {
		if got := StatusClassOf(status); got != want {
			t.Fatalf("StatusClassOf(%d)=%q want %q", status, got, want)
		}
	}
}
