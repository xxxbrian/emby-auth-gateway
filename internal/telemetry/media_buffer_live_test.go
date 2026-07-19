package telemetry

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
)

type testMediaBufferLiveState struct {
	id       uint64
	snapshot MediaBufferLiveSnapshot
}

type blockingTerminalMediaBufferState struct {
	id       uint64
	terminal bool
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (s *blockingTerminalMediaBufferState) MediaBufferRawStreamID() uint64 { return s.id }
func (s *blockingTerminalMediaBufferState) MediaBufferTerminal() bool {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.terminal
}
func (s *blockingTerminalMediaBufferState) MediaBufferLiveSnapshot() MediaBufferLiveSnapshot {
	panic("full snapshot used for terminal membership")
}

func (s *testMediaBufferLiveState) MediaBufferRawStreamID() uint64 { return s.id }
func (s *testMediaBufferLiveState) MediaBufferTerminal() bool      { return s.snapshot.Terminal }
func (s *testMediaBufferLiveState) MediaBufferLiveSnapshot() MediaBufferLiveSnapshot {
	snapshot := s.snapshot
	snapshot.StreamID = s.id
	return snapshot
}

func TestRegistryBootIDAndLiveRegistryShareIdentity(t *testing.T) {
	r := New(nil)
	if r.BootID() == "" || r.MediaBufferLive().BootID() != r.BootID() {
		t.Fatalf("registry boot=%q live boot=%q", r.BootID(), r.MediaBufferLive().BootID())
	}
}

func TestMediaBufferLiveRegistryFixedCapacityGuardAndBacking(t *testing.T) {
	r := newMediaBufferLiveRegistry("boot")
	if len(r.slots) != 0 || cap(r.slots) != MediaBufferLiveCapacity {
		t.Fatalf("initial len/cap=%d/%d", len(r.slots), cap(r.slots))
	}
	backing := unsafe.SliceData(r.slots[:cap(r.slots)])
	for id := uint64(1); id <= MediaBufferLiveCapacity; id++ {
		if !r.Register(&testMediaBufferLiveState{id: id}) {
			t.Fatalf("register id=%d dropped early", id)
		}
	}
	if len(r.slots) != MediaBufferLiveCapacity || cap(r.slots) != MediaBufferLiveCapacity || unsafe.SliceData(r.slots) != backing {
		t.Fatalf("full len/cap/backing=%d/%d/%p want backing=%p", len(r.slots), cap(r.slots), unsafe.SliceData(r.slots), backing)
	}
	if r.Register(&testMediaBufferLiveState{id: MediaBufferLiveCapacity + 1}) {
		t.Fatal("registration appended beyond fixed capacity")
	}
	if len(r.slots) != MediaBufferLiveCapacity || r.RegistrationDrops() != 1 || unsafe.SliceData(r.slots) != backing {
		t.Fatalf("post-drop len/drops/backing=%d/%d/%p", len(r.slots), r.RegistrationDrops(), unsafe.SliceData(r.slots))
	}
}

func TestMediaBufferLiveRegistryPageDetailAndTerminalProgress(t *testing.T) {
	r := newMediaBufferLiveRegistry("boot")
	states := make([]*testMediaBufferLiveState, 4)
	for i := range states {
		states[i] = &testMediaBufferLiveState{id: uint64(i + 1)}
		if !r.Register(states[i]) {
			t.Fatal("register")
		}
	}
	states[0].snapshot.Terminal = true
	states[1].snapshot.Terminal = true
	page := r.Page(0, 2)
	if len(page.Items) != 0 || page.NextCursor != 2 || !page.HasMore {
		t.Fatalf("terminal page=%+v", page)
	}
	page = r.Page(page.NextCursor, 2)
	if len(page.Items) != 2 || page.NextCursor != 4 || page.HasMore {
		t.Fatalf("active page=%+v", page)
	}
	if _, ok := r.Detail(1); ok {
		t.Fatal("terminal detail remained visible")
	}
	if got, ok := r.Detail(3); !ok || got.MediaBufferRawStreamID() != 3 {
		t.Fatalf("detail=%v ok=%v", got, ok)
	}
}

func TestMediaBufferLiveRegistryStableCompactionReusesBacking(t *testing.T) {
	r := newMediaBufferLiveRegistry("boot")
	states := make([]*testMediaBufferLiveState, MediaBufferLiveCapacity)
	for i := range states {
		states[i] = &testMediaBufferLiveState{id: uint64(i + 1)}
		if !r.Register(states[i]) {
			t.Fatalf("register %d", i)
		}
		if i%2 == 0 {
			states[i].snapshot.Terminal = true
		}
	}
	backing := unsafe.SliceData(r.slots)
	if removed := r.CompactTerminal(); removed != MediaBufferLiveCapacity/2 {
		t.Fatalf("removed=%d", removed)
	}
	if cap(r.slots) != MediaBufferLiveCapacity || unsafe.SliceData(r.slots) != backing {
		t.Fatalf("compaction cap/backing=%d/%p want %p", cap(r.slots), unsafe.SliceData(r.slots), backing)
	}
	for i, state := range r.slots {
		want := uint64(2 * (i + 1))
		if state.MediaBufferRawStreamID() != want {
			t.Fatalf("slot %d id=%d want=%d", i, state.MediaBufferRawStreamID(), want)
		}
	}
	if !r.Register(&testMediaBufferLiveState{id: MediaBufferLiveCapacity + 1}) {
		t.Fatal("registration did not reuse compacted backing")
	}
}

func TestMediaBufferRegistryTerminalChecksAvoidFullSnapshots(t *testing.T) {
	r := newMediaBufferLiveRegistry("boot")
	state := &blockingTerminalMediaBufferState{id: 1, entered: make(chan struct{}), release: make(chan struct{})}
	close(state.release)
	if !r.Register(state) {
		t.Fatal("register")
	}
	if page := r.Page(0, 1); len(page.Items) != 1 {
		t.Fatalf("page=%+v", page)
	}
	if _, ok := r.Detail(1); !ok {
		t.Fatal("detail missing")
	}
	if removed := r.CompactTerminal(); removed != 0 {
		t.Fatalf("removed=%d", removed)
	}
}

func TestMediaBufferRegistryRegistrationWaitsForOverlappingCompaction(t *testing.T) {
	for _, n := range []int{512, MediaBufferLiveCapacity} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			r, blocker := blockingCompactionRegistry(n)
			compacted := make(chan int, 1)
			go func() { compacted <- r.CompactTerminal() }()
			<-blocker.entered
			started := make(chan struct{})
			registered := make(chan bool, 1)
			go func() {
				close(started)
				registered <- r.Register(&testMediaBufferLiveState{id: uint64(n + 1)})
			}()
			<-started
			select {
			case result := <-registered:
				t.Fatalf("registration completed while compaction held WLock: %v", result)
			case <-time.After(10 * time.Millisecond):
			}
			close(blocker.release)
			if removed := <-compacted; removed != 1 {
				t.Fatalf("removed=%d", removed)
			}
			if !<-registered || r.RegistrationDrops() != 0 {
				t.Fatalf("registration failed drops=%d", r.RegistrationDrops())
			}
		})
	}
}

func TestMediaBufferRegistryMixedOperationsWaitForCompaction(t *testing.T) {
	for _, n := range []int{512, MediaBufferLiveCapacity} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			r, blocker := blockingCompactionRegistry(n)
			compacted := make(chan int, 1)
			go func() { compacted <- r.CompactTerminal() }()
			<-blocker.entered
			started := make(chan struct{}, 4)
			done := make(chan string, 4)
			go func() { started <- struct{}{}; _ = r.Page(0, 50); done <- "page" }()
			go func() { started <- struct{}{}; _, _ = r.Detail(uint64(n)); done <- "detail" }()
			go func() { started <- struct{}{}; _ = r.Aggregate(uint32(n)); done <- "aggregate" }()
			go func() {
				started <- struct{}{}
				if !r.Register(&testMediaBufferLiveState{id: uint64(n + 1)}) {
					done <- "registration-failed"
					return
				}
				done <- "registration"
			}()
			for i := 0; i < 4; i++ {
				<-started
			}
			select {
			case operation := <-done:
				t.Fatalf("%s completed while maintenance held WLock", operation)
			case <-time.After(10 * time.Millisecond):
			}
			close(blocker.release)
			if removed := <-compacted; removed != 1 {
				t.Fatalf("removed=%d", removed)
			}
			seen := make(map[string]bool, 4)
			for i := 0; i < 4; i++ {
				seen[<-done] = true
			}
			for _, operation := range []string{"page", "detail", "aggregate", "registration"} {
				if !seen[operation] {
					t.Fatalf("missing completed operation %q: %v", operation, seen)
				}
			}
		})
	}
}

func TestMediaBufferLiveRegistryNAnchoredAggregateBarriers(t *testing.T) {
	r := newMediaBufferLiveRegistry("boot")
	first := &testMediaBufferLiveState{id: 1, snapshot: MediaBufferLiveSnapshot{QueuedBytes: 10, WritingBytes: 2}}
	second := &testMediaBufferLiveState{id: 2, snapshot: MediaBufferLiveSnapshot{QueuedBytes: 20, WritingBytes: 4}}
	if !r.Register(first) {
		t.Fatal("register first")
	}

	// N=0 anchors before a newer registration; the row remains detail-visible.
	zero := r.Aggregate(0)
	if zero.ObservedActiveRequests != 0 || zero.UnobservedActiveRequests != 0 || zero.QueuedBytes != 0 {
		t.Fatalf("N=0 aggregate=%+v", zero)
	}
	if _, ok := r.Detail(1); !ok {
		t.Fatal("N=0 extra row not detail-visible")
	}

	if !r.Register(second) {
		t.Fatal("register second")
	}
	one := r.Aggregate(1)
	if one.ObservedActiveRequests != 1 || one.UnobservedActiveRequests != 0 || one.QueuedBytes != 10 {
		t.Fatalf("first-N aggregate=%+v", one)
	}
	if _, ok := r.Detail(2); !ok {
		t.Fatal("row beyond N not detail-visible")
	}
	two := r.Aggregate(2)
	if two.ObservedActiveRequests != 2 || two.UnobservedActiveRequests != 0 || two.QueuedBytes != 30 {
		t.Fatalf("next-cycle convergence=%+v", two)
	}

	// Completion between controller and live scans leaves an exact non-negative gap.
	first.snapshot.Terminal = true
	second.snapshot.Terminal = true
	missing := r.Aggregate(2)
	if missing.ObservedActiveRequests != 0 || missing.UnobservedActiveRequests != 2 || missing.Completeness != MediaBufferObservationLimited {
		t.Fatalf("N>0 zero observed=%+v", missing)
	}
}

func TestMediaBufferLiveRegistryCompletionDropDoesNotLeakTerminal(t *testing.T) {
	r := newMediaBufferLiveRegistry("boot")
	state := &testMediaBufferLiveState{id: 1}
	if !r.Register(state) {
		t.Fatal("register")
	}
	for i := 0; i < mediaBufferCompletionCapacity; i++ {
		if !r.OfferCompletion(MediaBufferCompletion{Terminal: MediaBufferLiveSnapshot{StreamID: uint64(i + 1)}}) {
			t.Fatalf("offer %d", i)
		}
	}
	if r.OfferCompletion(MediaBufferCompletion{Terminal: MediaBufferLiveSnapshot{StreamID: 999}}) || r.CompletionDrops() != 1 {
		t.Fatalf("completion drops=%d", r.CompletionDrops())
	}
	state.snapshot.Terminal = true
	if removed := r.CompactTerminal(); removed != 1 || len(r.slots) != 0 {
		t.Fatalf("terminal cleanup removed=%d slots=%d", removed, len(r.slots))
	}
}

func BenchmarkMediaBufferLiveRegistry(b *testing.B) {
	for _, n := range []int{1, 64, 512, MediaBufferLiveCapacity} {
		b.Run(fmt.Sprintf("N=%d/Registration", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				r := benchmarkMediaBufferRegistry(n - 1)
				state := &testMediaBufferLiveState{id: uint64(n)}
				b.StartTimer()
				if !r.Register(state) {
					b.Fatal("registration dropped")
				}
			}
		})
		b.Run(fmt.Sprintf("N=%d/Aggregate", n), func(b *testing.B) {
			r := benchmarkMediaBufferRegistry(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = r.Aggregate(uint32(n))
			}
		})
		b.Run(fmt.Sprintf("N=%d/PageDetail", n), func(b *testing.B) {
			r := benchmarkMediaBufferRegistry(n)
			id := uint64(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = r.Page(0, 50)
				_, _ = r.Detail(id)
			}
		})
	}
	b.Run("N=4096/FullCompaction", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			r := benchmarkMediaBufferRegistry(MediaBufferLiveCapacity)
			for _, state := range r.slots {
				state.(*testMediaBufferLiveState).snapshot.Terminal = true
			}
			b.StartTimer()
			_ = r.CompactTerminal()
		}
	})
	for _, n := range []int{512, MediaBufferLiveCapacity} {
		b.Run(fmt.Sprintf("N=%d/RegistrationDuringCompaction", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				r, blocker := blockingCompactionRegistry(n)
				compacted := make(chan int, 1)
				go func() { compacted <- r.CompactTerminal() }()
				<-blocker.entered
				started := make(chan struct{})
				registered := make(chan bool, 1)
				b.StartTimer()
				go func() {
					close(started)
					registered <- r.Register(&testMediaBufferLiveState{id: uint64(n + 1)})
				}()
				<-started
				runtime.Gosched()
				close(blocker.release)
				if removed := <-compacted; removed != 1 || !<-registered {
					b.Fatalf("removed=%d registration failed", removed)
				}
			}
		})
		b.Run(fmt.Sprintf("N=%d/MixedOperationsDuringCompaction", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				r, blocker := blockingCompactionRegistry(n)
				compacted := make(chan int, 1)
				go func() { compacted <- r.CompactTerminal() }()
				<-blocker.entered
				started := make(chan struct{}, 4)
				done := make(chan struct{}, 4)
				b.StartTimer()
				go func() { started <- struct{}{}; _ = r.Page(0, 50); done <- struct{}{} }()
				go func() { started <- struct{}{}; _, _ = r.Detail(uint64(n)); done <- struct{}{} }()
				go func() { started <- struct{}{}; _ = r.Aggregate(uint32(n)); done <- struct{}{} }()
				go func() {
					started <- struct{}{}
					_ = r.Register(&testMediaBufferLiveState{id: uint64(n + 1)})
					done <- struct{}{}
				}()
				for operation := 0; operation < 4; operation++ {
					<-started
				}
				runtime.Gosched()
				select {
				case <-done:
					b.Fatal("operation completed while maintenance held WLock")
				default:
				}
				close(blocker.release)
				if removed := <-compacted; removed != 1 {
					b.Fatalf("removed=%d", removed)
				}
				for operation := 0; operation < 4; operation++ {
					<-done
				}
			}
		})
	}
}

func blockingCompactionRegistry(n int) (*MediaBufferLiveRegistry, *blockingTerminalMediaBufferState) {
	r := newMediaBufferLiveRegistry("bench")
	blocker := &blockingTerminalMediaBufferState{id: 1, terminal: true, entered: make(chan struct{}), release: make(chan struct{})}
	r.Register(blocker)
	for id := 2; id <= n; id++ {
		r.Register(&testMediaBufferLiveState{id: uint64(id)})
	}
	return r, blocker
}

func benchmarkMediaBufferRegistry(n int) *MediaBufferLiveRegistry {
	r := newMediaBufferLiveRegistry("bench")
	for i := 0; i < n; i++ {
		r.Register(&testMediaBufferLiveState{id: uint64(i + 1), snapshot: MediaBufferLiveSnapshot{QueuedBytes: 1}})
	}
	return r
}
