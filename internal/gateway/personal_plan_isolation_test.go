package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type phase6MetadataFake struct {
	mu        sync.Mutex
	responses []string
	requests  []*http.Request
}

func (f *phase6MetadataFake) RoundTripMetadata(in metadataUpstreamRequest) (*http.Response, error) {
	f.mu.Lock()
	index := len(f.requests)
	f.requests = append(f.requests, in.Request)
	body := f.responses[index]
	f.mu.Unlock()

	if in.SnapshotRef != nil {
		*in.SnapshotRef = personalPlanSourceUpstreamSnapshot()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    in.Request,
	}, nil
}

func phase6PlannerSource(t *testing.T, store Store, fake *phase6MetadataFake, session *Session) (*Server, *personalPlanSource) {
	t.Helper()
	server := NewServer(Config{GatewayServerID: "gateway-server", PublicBaseURL: "https://gateway.test/emby"}, store)
	server.managedAuthUpstream = &phase5AuthSpy{runtime: managedRuntime("old-token")}
	server.metadataUpstream = fake
	request := httptest.NewRequest(http.MethodGet, "https://gateway.test/emby/Items", nil)
	source, err := newPersonalPlanSource(server, request, session, "gateway-token")
	if err != nil {
		t.Fatal(err)
	}
	return server, source
}

func TestPersonalPlannerListIsolatesTwoUsersSharingBackendItems(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	liked, disliked := true, false
	userA := &Session{GatewayUserID: "user-a", SyntheticUserID: "synthetic-a"}
	userB := &Session{GatewayUserID: "user-b", SyntheticUserID: "synthetic-b"}
	states := []PlaybackState{
		{GatewayUserID: userA.GatewayUserID, SyntheticUserID: userA.SyntheticUserID, ItemID: "shared-1", Played: true, PlaybackPositionTicks: 111, IsFavorite: true, Likes: &liked},
		{GatewayUserID: userA.GatewayUserID, SyntheticUserID: userA.SyntheticUserID, ItemID: "shared-2", Played: false, PlaybackPositionTicks: 112, IsFavorite: false, Likes: &disliked},
		{GatewayUserID: userB.GatewayUserID, SyntheticUserID: userB.SyntheticUserID, ItemID: "shared-1", Played: false, PlaybackPositionTicks: 221, IsFavorite: false, Likes: &disliked},
		{GatewayUserID: userB.GatewayUserID, SyntheticUserID: userB.SyntheticUserID, ItemID: "shared-2", Played: true, PlaybackPositionTicks: 222, IsFavorite: true, Likes: &liked},
	}
	for _, state := range states {
		if err := store.SavePlaybackState(ctx, state); err != nil {
			t.Fatal(err)
		}
	}

	plan := executorPlan(personalPlanPositive)
	plan.Predicates.Favorite = personalTruthTrue

	tests := []struct {
		name     string
		session  *Session
		wantID   string
		played   bool
		position int64
		favorite bool
		likes    bool
	}{
		{"user A", userA, "shared-1", true, 111, true, true},
		{"user B", userB, "shared-2", true, 222, true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backendItem := `{"Items":[{"Id":"` + test.wantID + `","Name":"Shared Item","Type":"Movie","UserData":{"Played":false,"PlaybackPositionTicks":999,"IsFavorite":false,"Likes":false}}]}`
			fake := &phase6MetadataFake{responses: []string{backendItem}}
			server, source := phase6PlannerSource(t, store, fake, test.session)
			result, err := executePositivePersonalPlan(ctx, source, plan)
			if err != nil {
				t.Fatal(err)
			}
			if result.Total == nil || *result.Total != 1 || len(result.Items) != 1 || result.Items[0].state.ItemID != test.wantID {
				t.Fatalf("result = %#v, want only %q", result, test.wantID)
			}
			projected, err := server.projectPlannedPersonalItems(result.Items, test.session)
			if err != nil {
				t.Fatal(err)
			}
			userData, ok := mapField(projected[0], "UserData")
			if !ok {
				t.Fatalf("projected item has no UserData: %#v", projected[0])
			}
			if userData["Played"] != test.played || userData["PlaybackPositionTicks"] != test.position || userData["IsFavorite"] != test.favorite || userData["Likes"] != test.likes {
				t.Fatalf("UserData = %#v", userData)
			}
		})
	}

	stateA, err := store.FindPlaybackState(ctx, userA.GatewayUserID, "shared-1")
	if err != nil {
		t.Fatal(err)
	}
	stateB, err := store.FindPlaybackState(ctx, userB.GatewayUserID, "shared-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stateA.Played || stateA.PlaybackPositionTicks != 111 || !stateA.IsFavorite || stateA.Likes == nil || !*stateA.Likes || stateA.LastSeenAt == nil {
		t.Fatalf("user A repaired state = %#v", stateA)
	}
	if stateB.Played || stateB.PlaybackPositionTicks != 221 || stateB.IsFavorite || stateB.Likes == nil || *stateB.Likes || stateB.LastSeenAt != nil {
		t.Fatalf("user A resolution repair altered user B state: %#v", stateB)
	}
}

type phase6ResolutionBarrierStore struct {
	*MemoryStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *phase6ResolutionBarrierStore) SavePlaybackResolution(ctx context.Context, state PlaybackState) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.MemoryStore.SavePlaybackResolution(ctx, state)
}

func TestPersonalPlannerResolutionDoesNotClobberConcurrentPersonalWrite(t *testing.T) {
	ctx := context.Background()
	orphaned := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	base := NewMemoryStore()
	store := &phase6ResolutionBarrierStore{MemoryStore: base, entered: make(chan struct{}), release: make(chan struct{})}
	session := &Session{GatewayUserID: "user", SyntheticUserID: "synthetic-user"}
	oldLike := false
	initial := PlaybackState{
		GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, ItemID: "shared-item",
		PlaybackPositionTicks: 10, IsFavorite: true, Likes: &oldLike, OrphanedAt: &orphaned,
	}
	if err := store.SavePlaybackState(ctx, initial); err != nil {
		t.Fatal(err)
	}

	fake := &phase6MetadataFake{responses: []string{`{"Items":[{"Id":"shared-item","Name":"Resolved Name","Type":"Movie","RunTimeTicks":9000}]}`}}
	_, source := phase6PlannerSource(t, store, fake, session)
	plan := executorPlan(personalPlanPositive)
	plan.Predicates.Favorite = personalTruthTrue
	done := make(chan error, 1)
	go func() {
		_, err := executePositivePersonalPlan(ctx, source, plan)
		done <- err
	}()

	<-store.entered
	newLike := true
	personalWrite := initial
	personalWrite.Played = true
	personalWrite.PlaybackPositionTicks = 777
	personalWrite.IsFavorite = false
	personalWrite.Likes = &newLike
	if err := store.SavePlaybackState(ctx, personalWrite); err != nil {
		t.Fatal(err)
	}
	close(store.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	final, err := store.FindPlaybackState(ctx, session.GatewayUserID, initial.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Played || final.PlaybackPositionTicks != 777 || final.IsFavorite || final.Likes == nil || !*final.Likes {
		t.Fatalf("personal write was clobbered: %#v", final)
	}
	if final.OrphanedAt != nil || final.LastSeenAt == nil || final.ItemName != "Resolved Name" || final.ItemType != "Movie" || final.RunTimeTicks != 9000 || final.Fingerprint == "" {
		t.Fatalf("resolution metadata was not repaired: %#v", final)
	}
}
