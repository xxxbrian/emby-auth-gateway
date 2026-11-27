package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayJSONPreservesWireNumbers(t *testing.T) {
	const item = `{"Id":"0007","Type":"Movie","RunTimeTicks":9223372036854775807,"GenreItems":[{"Id":9007199254740993,"Name":"Fixture"}],"MediaSources":[{"Size":9007199254740995,"Id":"0017"}],"UserData":{"PlaybackPositionTicks":12,"Played":true},"NewMetadata":[-9223372036854775808,1.2300e-10,"9007199254740993",true,null]}`
	type itemDTO struct {
		ID           string `json:"Id"`
		RunTimeTicks int64  `json:"RunTimeTicks"`
		GenreItems   []struct {
			ID int64 `json:"Id"`
		}
		MediaSources []struct {
			ID   string `json:"Id"`
			Size int64  `json:"Size"`
		}
		UserData struct {
			PlaybackPositionTicks int64
			IsFavorite            bool
			Played                bool
		}
		NewMetadata json.RawMessage
	}
	for _, test := range []struct {
		name        string
		contentType string
		personal    bool
	}{
		{name: "explicit JSON", contentType: "application/json"},
		{name: "vendor JSON", contentType: "application/vnd.emby+json"},
		{name: "missing content type"},
		{name: "personal item resolution", contentType: "application/json", personal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.contentType == "" {
					w.Header()["Content-Type"] = nil
				} else {
					w.Header().Set("Content-Type", test.contentType)
				}
				if test.personal {
					_, _ = io.WriteString(w, `{"Items":[`+item+`],"TotalRecordCount":1}`)
					return
				}
				_, _ = io.WriteString(w, item)
			}))
			defer upstream.Close()
			store := NewMemoryStore()
			configureTestUpstream(store, upstream.URL+"/emby")
			store.Sessions[HashToken("gateway-token")] = testSession()
			if err := store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "0007", ItemType: "Movie", PlaybackPositionTicks: 9007199254740997, IsFavorite: true}); err != nil {
				t.Fatal(err)
			}
			server := NewServer(Config{GatewayBasePath: "/emby"}, store)
			path := "/emby/Users/gateway-user/Items/0007?api_key=gateway-token"
			if test.personal {
				path = "/emby/Users/gateway-user/Items?api_key=gateway-token&IsFavorite=true"
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected response %d, Content-Type %q: %s", response.Code, response.Header().Get("Content-Type"), response.Body)
			}
			var got itemDTO
			if test.personal {
				var page struct{ Items []itemDTO }
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
					t.Fatalf("invalid personal item page: %s, %v", response.Body, err)
				}
				got = page.Items[0]
			} else if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.ID != "0007" || got.RunTimeTicks != 9223372036854775807 || len(got.GenreItems) != 1 || got.GenreItems[0].ID != 9007199254740993 ||
				len(got.MediaSources) != 1 || got.MediaSources[0].ID != "0017" || got.MediaSources[0].Size != 9007199254740995 {
				t.Fatalf("Emby IDs or integer values changed: %+v", got)
			}
			if got.UserData.PlaybackPositionTicks != 9007199254740997 || !got.UserData.IsFavorite || got.UserData.Played {
				t.Fatalf("gateway UserData overlay changed type or value: %+v", got.UserData)
			}
			if string(got.NewMetadata) != `[-9223372036854775808,1.2300e-10,"9007199254740993",true,null]` {
				t.Fatalf("unrecognized JSON fields changed: %s", got.NewMetadata)
			}
		})
	}
}

func TestProxyJSONWithTrailingDataUsesRawFallback(t *testing.T) {
	const data = `{"Name":"test"} {"Id":9007199254740993}`
	for _, contentType := range []string{"", "application/json"} {
		server := NewServer(Config{}, NewMemoryStore())
		request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil)
		response := missingContentTypeResponse(request, strings.NewReader(data), int64(len(data)))
		response.Header.Set("Content-Type", contentType)
		writer := &deadlineRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
		server.writeProxyResponseWithSnapshot(writer, request, "/Items/item", response, &Session{}, upstreamRequestSnapshot{}, "", "")
		if writer.Code != http.StatusOK || writer.Body.String() != data {
			t.Fatalf("raw fallback for %q changed invalid JSON: %d %s", contentType, writer.Code, writer.Body)
		}
	}
}
