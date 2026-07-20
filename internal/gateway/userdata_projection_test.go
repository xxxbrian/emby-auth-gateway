package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUserDataWritePresenceFalseZeroNullAndLikes(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"Played":false,"PlaybackPositionTicks":0,"PlayedPercentage":0,"PlayCount":0,"IsFavorite":false,"Likes":false}`))
	body, err := readUserDataWriteBody(req, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	likes := true
	state := &PlaybackState{Played: true, PlaybackPositionTicks: 99, PlayedPercentage: floatPtr(50), PlayCount: 3, IsFavorite: true, Likes: &likes}
	applyUserDataWriteToState(body, state, time.Now())
	if state.Played || state.PlaybackPositionTicks != 0 || state.PlayedPercentage == nil || *state.PlayedPercentage != 0 || state.PlayCount != 0 || state.IsFavorite || state.Likes == nil || *state.Likes {
		t.Fatalf("state = %#v", state)
	}

	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"Played":null,"PlaybackPositionTicks":null,"Likes":null}`))
	body, err = readUserDataWriteBody(req, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	applyUserDataWriteToState(body, state, time.Now())
	if state.Played || state.PlaybackPositionTicks != 0 || state.Likes == nil || *state.Likes {
		t.Fatalf("null fields changed state = %#v", state)
	}

	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"PlaybackPositionTicks":"12","PlayedPercentage":"25.5","PlayCount":"2"}`))
	body, err = readUserDataWriteBody(req, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	applyUserDataWriteToState(body, state, time.Now())
	if state.PlaybackPositionTicks != 12 || state.PlayedPercentage == nil || *state.PlayedPercentage != 25.5 || state.PlayCount != 2 {
		t.Fatalf("numeric string fields = %#v", state)
	}
}

func TestUserDataWireDTOFalseZeroAndLikes(t *testing.T) {
	likes := false
	percentage := 0.0
	got, err := json.Marshal(userDataWireDTO(&PlaybackState{ItemID: "item-1", PlayedPercentage: &percentage, Likes: &likes}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"PlaybackPositionTicks":0,"PlayCount":0,"IsFavorite":false,"Played":false,"PlayedPercentage":0,"Likes":false,"Key":"item-1","ItemId":"item-1"}`
	if string(got) != want {
		t.Fatalf("user data = %s, want %s", got, want)
	}
}

func TestUserDataWireDTODerivesPercentageFromRuntime(t *testing.T) {
	stored := 3.0
	got := userDataWireDTO(&PlaybackState{ItemID: "item-1", PlaybackPositionTicks: 25, RunTimeTicks: 100, PlayedPercentage: &stored})
	if got.PlayedPercentage == nil || *got.PlayedPercentage != 25 {
		t.Fatalf("PlayedPercentage = %v, want 25", got.PlayedPercentage)
	}
}

func TestDisplayPreferenceRoundTripPreservesUnknownFields(t *testing.T) {
	store := NewMemoryStore()
	session := testSession()
	store.Sessions[HashToken("gateway-token")] = session
	server := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer server.Close()

	payload := `{"CustomPrefs":{"plugin":{"counter":9007199254740993,"enabled":false}},"Unknown":null,"SortBy":"DateCreated"}`
	request := mustRequest(t, http.MethodPost, server.URL+"/emby/DisplayPreferences/home?api_key=gateway-token&Client=web", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := do(t, request)
	posted, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Equal(posted, []byte(payload)) {
		t.Fatalf("post status/body = %d/%s", response.StatusCode, posted)
	}

	response = do(t, mustRequest(t, http.MethodGet, server.URL+"/emby/DisplayPreferences/home?api_key=gateway-token&Client=web", nil))
	got, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Equal(got, []byte(payload)) {
		t.Fatalf("get status/body = %d/%s", response.StatusCode, got)
	}
}
