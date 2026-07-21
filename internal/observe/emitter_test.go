package observe

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
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
		{"GET", "/Items/abc/PlaybackInfo", RouteMedia},
		{"POST", "/Sessions/Playing/Progress", RoutePlayback},
		{"GET", "/Videos/item1/stream", RouteMedia},
		{"GET", "/Audio/item1/stream.mp3", RouteMedia},
		{"GET", "/Items/x/Download", RouteMedia},
		{"POST", "/Users/u/PlayedItems/i", RouteUserdata},
		{"POST", "/Users/u/FavoriteItems/i", RouteUserdata},
		{"GET", "/Users/u/Items", RouteMetadata},
		{"GET", "/System/Info/Public", RouteMetadata},
		{"GET", "/unknown/path", RouteOther},
		// Compatibility: ClassifyRoute sanitizes query/fragment/whitespace before routeclass.
		// Bare /Items/{id} is not a curated template (Unclassified → RouteOther).
		{"GET", "Items/x?api_key=secret", RouteOther},
		{"POST", "/Sessions/Playing/Progress?api_key=secret#frag", RoutePlayback},
		{"POST", "  /Sessions/Logout  ", RouteAuth},
		{"GET", "/System/Info/Public?x=1", RouteMetadata},
		{"GET", "/Sessions/PlayQueue", RoutePlayback},
		{"POST", "/Sessions/sid/Playing/Pause", RoutePlayback},
		{"GET", "/Sessions", RoutePlayback},
		{"GET", "/Users/Me", RouteMetadata},
		{"GET", "/DisplayPreferences/home", RouteUserdata},
		{"GET", "/SessionsX", RouteOther},
	}
	for _, tc := range cases {
		if got := ClassifyRoute(tc.method, tc.path); got != tc.want {
			t.Fatalf("ClassifyRoute(%q,%q)=%q want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestClassifyRouteSanitizesBeforeRouteclass(t *testing.T) {
	// routeclass treats ?/#/spaces as path data; the compat wrapper strips them.
	if got := routeclass.Classify("POST", "/Sessions/Playing?x"); got.Ownership != routeclass.DeniedSession {
		t.Fatalf("routeclass path-data: got %+v want DeniedSession", got)
	}
	if got := ClassifyRoute("POST", "/Sessions/Playing?x"); got != RoutePlayback {
		t.Fatalf("compat wrapper: ClassifyRoute=/Sessions/Playing?x got %q want %q", got, RoutePlayback)
	}
	if got := ClassifyRoute("POST", "/Sessions/Playing#x"); got != RoutePlayback {
		t.Fatalf("compat wrapper: ClassifyRoute=/Sessions/Playing#x got %q want %q", got, RoutePlayback)
	}
	if got := ClassifyRoute("POST", "  /Sessions/Playing  "); got != RoutePlayback {
		t.Fatalf("compat wrapper: whitespace path got %q want %q", got, RoutePlayback)
	}
}

func TestRouteClassOf(t *testing.T) {
	cases := []struct {
		name string
		dec  routeclass.Decision
		want string
	}{
		{"auth", routeclass.Decision{Ownership: routeclass.LocalPublic, Operation: routeclass.OperationAuthenticate}, RouteAuth},
		{"logout", routeclass.Decision{Ownership: routeclass.LocalSession, Operation: routeclass.OperationLogout}, RouteAuth},
		{"playback report", routeclass.Decision{Ownership: routeclass.LocalSession, Operation: routeclass.OperationPlaybackReport}, RoutePlayback},
		{"session list", routeclass.Decision{Ownership: routeclass.LocalSession, Operation: routeclass.OperationSessionList}, RoutePlayback},
		{"denied session", routeclass.Decision{Ownership: routeclass.DeniedSession, Operation: routeclass.OperationDeniedSession}, RoutePlayback},
		{"current user", routeclass.Decision{Ownership: routeclass.LocalPersonal, Operation: routeclass.OperationCurrentUser}, RouteMetadata},
		{"personal", routeclass.Decision{Ownership: routeclass.LocalPersonal, Operation: routeclass.OperationPersonal}, RouteUserdata},
		{"metadata", routeclass.Decision{Ownership: routeclass.MetadataProxy}, RouteMetadata},
		{"media", routeclass.Decision{Ownership: routeclass.MediaProxy}, RouteMedia},
		{"public system", routeclass.Decision{Ownership: routeclass.LocalPublic, Operation: routeclass.OperationPublicSystemInfo}, RouteMetadata},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RouteClassOf(tc.dec); got != tc.want {
				t.Fatalf("RouteClassOf(%+v)=%q want %q", tc.dec, got, tc.want)
			}
		})
	}
}

func TestClassifyRouteDelegatesToRouteclass(t *testing.T) {
	// Compatibility: ClassifyRoute is RouteClassOf(routeclass.Classify(sanitize(path))).
	paths := []struct{ method, path string }{
		{"POST", "/Users/AuthenticateByName"},
		{"GET", "/Sessions/PlayQueue"},
		{"POST", "/Sessions/Playing/Pause"},
		{"GET", "/Videos/x/stream"},
		{"GET", "/unknown"},
		{"POST", "/Sessions/Playing?x"},
		{"GET", "  /System/Info/Public  "},
	}
	for _, tc := range paths {
		want := RouteClassOf(routeclass.Classify(tc.method, sanitizeCompatPath(tc.path)))
		if got := ClassifyRoute(tc.method, tc.path); got != want {
			t.Fatalf("ClassifyRoute(%q,%q)=%q want %q", tc.method, tc.path, got, want)
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
