package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseWidthsSortsAndDeduplicates(t *testing.T) {
	widths, err := parseWidths("800,400,400,1200")
	if err != nil {
		t.Fatalf("parse widths: %v", err)
	}
	want := []int{400, 800, 1200}
	if len(widths) != len(want) {
		t.Fatalf("width count = %d, want %d", len(widths), len(want))
	}
	for i := range want {
		if widths[i] != want[i] {
			t.Fatalf("widths = %v, want %v", widths, want)
		}
	}
}

func TestParseConfigAcceptsLimitNames(t *testing.T) {
	cfg, err := parseConfig([]string{
		"--gateway-url", "https://gateway.example/emby",
		"--cdn-url", "https://cdn.example/emby",
		"--username", "user",
		"--password", "pass",
		"--limit-names", "3",
	})
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.LimitNames != 3 {
		t.Fatalf("LimitNames = %d, want 3", cfg.LimitNames)
	}
}

func TestStateSelectionResumesPastLimitedBatch(t *testing.T) {
	ctx := context.Background()
	db, err := openStateDB(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer db.Close()

	urls := planWarmURLs([]imageSource{{ItemID: "item-1", ImageType: "Primary", ImageIndex: -1, Tag: "tag-1"}}, []int{400, 800}, 90)
	if err := storePlan(ctx, db, urls); err != nil {
		t.Fatalf("store plan: %v", err)
	}
	first, err := selectWarmURLs(ctx, db, false, 2)
	if err != nil {
		t.Fatalf("select first batch: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first batch size = %d, want 2", len(first))
	}
	for _, u := range first {
		if err := updateWarmResult(ctx, db, warmResult{URLPath: u.Path, HTTPStatus: 200, ContentType: "image/jpeg"}); err != nil {
			t.Fatalf("mark warm result: %v", err)
		}
	}

	next, err := selectWarmURLs(ctx, db, false, 2)
	if err != nil {
		t.Fatalf("select next batch: %v", err)
	}
	if len(next) != 1 {
		t.Fatalf("next batch size = %d, want 1", len(next))
	}
	for _, u := range first {
		if next[0].Path == u.Path {
			t.Fatalf("next batch repeated completed URL %q", u.Path)
		}
	}
}

func TestGetItemListAcceptsEmptyObjectResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Items":[],"TotalRecordCount":0}`))
	}))
	defer server.Close()

	list, err := getItemList(context.Background(), server.Client(), server.URL, "token")
	if err != nil {
		t.Fatalf("get item list: %v", err)
	}
	if len(list.Items) != 0 || list.TotalRecordCount != 0 {
		t.Fatalf("list = %#v, want empty", list)
	}
}

func TestDiscoverSourcesAdaptsPageSizeAndPreservesOffsets(t *testing.T) {
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("StartIndex")
		limit := r.URL.Query().Get("Limit")
		requests = append(requests, start+"/"+limit)
		if r.URL.Query().Get("Limit") == "4" {
			http.Error(w, "temporary upstream failure", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if start == "0" {
			_, _ = w.Write([]byte(`{"Items":[{"Id":"a"},{"Id":"b"}],"TotalRecordCount":4}`))
			return
		}
		_, _ = w.Write([]byte(`{"Items":[{"Id":"c"},{"Id":"d"}],"TotalRecordCount":4}`))
	}))
	defer server.Close()

	var discovered []string
	err := discoverSources(context.Background(), server.Client(), config{GatewayURL: server.URL, PageSize: 4, MinPageSize: 2, MetadataRetries: 0, RetryDelay: time.Millisecond, IncludeNames: false}, authResult{User: struct {
		ID string `json:"Id"`
	}{ID: "user-1"}}, func(_ context.Context, sources []imageSource) error {
		for _, source := range sources {
			discovered = append(discovered, source.ItemID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover sources: %v", err)
	}
	wantRequests := []string{"0/4", "0/2", "2/2"}
	if strings.Join(requests, ",") != strings.Join(wantRequests, ",") {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
	if strings.Join(discovered, ",") != "" {
		t.Fatalf("discovered sources = %v, want none without image tags", discovered)
	}
}

func TestDiscoverSourcesPaginatesArrayResponses(t *testing.T) {
	var starts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("StartIndex")
		starts = append(starts, start)
		w.Header().Set("Content-Type", "application/json")
		if start == "0" {
			_, _ = w.Write([]byte(`[{"Id":"a","ImageTags":{"Primary":"tag-a"}},{"Id":"b","ImageTags":{"Primary":"tag-b"}}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"Id":"c","ImageTags":{"Primary":"tag-c"}}]`))
	}))
	defer server.Close()

	var discovered []string
	err := discoverSources(context.Background(), server.Client(), config{GatewayURL: server.URL, PageSize: 2, MinPageSize: 1, MetadataRetries: 0, RetryDelay: time.Millisecond}, authResult{User: struct {
		ID string `json:"Id"`
	}{ID: "user-1"}}, func(_ context.Context, sources []imageSource) error {
		for _, source := range sources {
			discovered = append(discovered, source.ItemID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover sources: %v", err)
	}
	if strings.Join(starts, ",") != "0,2" {
		t.Fatalf("StartIndex values = %v, want [0 2]", starts)
	}
	if strings.Join(discovered, ",") != "a,b,c" {
		t.Fatalf("discovered = %v, want [a b c]", discovered)
	}
}

func TestMetadataFailureRequestCountIsBounded(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	_, usedLimit, err := getItemListPage(context.Background(), server.Client(), config{MinPageSize: 25, MetadataRetries: 2, RetryDelay: time.Nanosecond}, server.URL, nil, 0, 500, "token")
	if err == nil {
		t.Fatal("expected metadata error")
	}
	if usedLimit != 25 {
		t.Fatalf("used limit = %d, want 25", usedLimit)
	}
	if requests != 18 {
		t.Fatalf("requests = %d, want 18", requests)
	}
}

func TestMetadata429RetriesWithoutReducingPage(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "0")
		http.Error(w, "limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, usedLimit, err := getItemListPage(context.Background(), server.Client(), config{MinPageSize: 25, MetadataRetries: 2, RetryDelay: time.Nanosecond}, server.URL, nil, 0, 500, "token")
	if err == nil {
		t.Fatal("expected metadata error")
	}
	if usedLimit != 500 {
		t.Fatalf("used limit = %d, want 500", usedLimit)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
}

func TestMetadataRetriesTransportFailure(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"Items":[],"TotalRecordCount":0}`)), Request: req}, nil
	})}

	page, err := getItemListWithRetry(context.Background(), client, "http://metadata.test/items", "token", 1, time.Nanosecond)
	if err != nil {
		t.Fatalf("get item list: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if !page.TotalKnown || page.TotalRecordCount != 0 {
		t.Fatalf("page = %#v, want known empty page", page)
	}
}

func TestRunRefreshDoneSchedulesPersistedURL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := openStateDB(dbPath)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	u := warmURL{Source: imageSource{ItemID: "item-1", ImageType: "Primary", ImageIndex: -1, Tag: "tag-1"}, Path: "/Items/item-1/Images/Primary?tag=tag-1", Variant: "tag_only"}
	if err := storePlan(context.Background(), db, []warmURL{u}); err != nil {
		t.Fatalf("store plan: %v", err)
	}
	if err := updateWarmResult(context.Background(), db, warmResult{URLPath: u.Path, HTTPStatus: http.StatusOK, ContentType: "image/jpeg", Bytes: 4}); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close state db: %v", err)
	}

	imageRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/emby/Users/AuthenticateByName":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"AccessToken":"token","User":{"Id":"user-1"}}`))
		case r.URL.Path == "/emby/Users/user-1/Items":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[],"TotalRecordCount":0}`))
		case r.URL.Path == "/emby/Items/item-1/Images/Primary":
			imageRequests++
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
		case r.URL.Path == "/emby/Sessions/Logout":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err = run(context.Background(), []string{"--gateway-url", server.URL + "/emby", "--cdn-url", server.URL + "/emby", "--username", "user", "--password", "pass", "--include-names=false", "--refresh-done", "--db", dbPath, "--report", ""})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if imageRequests != 1 {
		t.Fatalf("image requests = %d, want 1", imageRequests)
	}

	db, err = openStateDB(dbPath)
	if err != nil {
		t.Fatalf("reopen state db: %v", err)
	}
	defer db.Close()
	var attempts int
	if err := db.QueryRow(`select attempts from warm_urls where url_path = ?`, u.Path).Scan(&attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestRunWritesReportAfterCancellation(t *testing.T) {
	metadataStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/AuthenticateByName":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"AccessToken":"token","User":{"Id":"user-1"}}`))
		case "/emby/Users/user-1/Items":
			select {
			case <-metadataStarted:
			default:
				close(metadataStarted)
			}
			<-r.Context().Done()
		case "/emby/Sessions/Logout":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	reportPath := filepath.Join(t.TempDir(), "report.json")
	statePath := filepath.Join(t.TempDir(), "state.sqlite")
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--gateway-url", server.URL + "/emby", "--cdn-url", server.URL + "/emby", "--username", "user", "--password", "pass", "--include-names=false", "--db", statePath, "--report", reportPath})
	}()
	select {
	case <-metadataStarted:
	case <-time.After(time.Second):
		t.Fatal("metadata request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run returned nil after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop after cancellation")
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var rep report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if rep.FinishedAt.IsZero() {
		t.Fatal("report missing finished_at")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRunWarmsImagesBeforeDiscoveryCompletes(t *testing.T) {
	imageSeen := make(chan struct{})
	var closeImageSeen sync.Once
	imageBeforeSecondPage := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/emby/Users/AuthenticateByName":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"AccessToken":"token","User":{"Id":"user-1"}}`))
		case r.URL.Path == "/emby/Users/user-1/Items":
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Query().Get("StartIndex") {
			case "0":
				_, _ = w.Write([]byte(`{"Items":[{"Id":"item-1","Type":"Movie","ImageTags":{"Primary":"tag-1"}}],"TotalRecordCount":2}`))
			case "1":
				select {
				case <-imageSeen:
					imageBeforeSecondPage = true
				case <-time.After(time.Second):
				}
				_, _ = w.Write([]byte(`{"Items":[{"Id":"item-2","Type":"Movie","ImageTags":{"Primary":"tag-2"}}],"TotalRecordCount":2}`))
			default:
				_, _ = w.Write([]byte(`{"Items":[],"TotalRecordCount":2}`))
			}
		case strings.HasPrefix(r.URL.Path, "/emby/Items/"):
			closeImageSeen.Do(func() { close(imageSeen) })
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("jpeg"))
		case r.URL.Path == "/emby/Sessions/Logout":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := run(context.Background(), []string{
		"--gateway-url", server.URL + "/emby",
		"--cdn-url", server.URL + "/emby",
		"--username", "user",
		"--password", "pass",
		"--include-names=false",
		"--page-size", "1",
		"--widths", "400",
		"--concurrency", "1",
		"--db", filepath.Join(t.TempDir(), "state.sqlite"),
		"--report", "",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !imageBeforeSecondPage {
		t.Fatal("image was not warmed before metadata discovery completed")
	}
}

func TestWarmOneRejectsEmptyImageResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := warmOne(context.Background(), server.Client(), config{CDNURL: server.URL}, authResult{AccessToken: "token"}, warmURL{Path: "/image", Variant: "tag_only", Source: imageSource{ImageType: "Primary"}})
	if result.Error != "response image body is empty" {
		t.Fatalf("error = %q, want empty image error", result.Error)
	}
}

func TestWarmOneRejectsIncompleteJPEG(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xff, 0xd8, 1, 2, 3})
	}))
	defer server.Close()

	result := warmOne(context.Background(), server.Client(), config{CDNURL: server.URL}, authResult{AccessToken: "token"}, warmURL{Path: "/image", Variant: "tag_only", Source: imageSource{ImageType: "Primary"}})
	if result.Error != "response JPEG image is incomplete" {
		t.Fatalf("error = %q, want incomplete JPEG error", result.Error)
	}
}

func TestSourcesFromItemsIncludesImagesBackdropsAndPeople(t *testing.T) {
	items := []itemDTO{{
		ID:        "item-1",
		Name:      "Movie",
		Type:      "Movie",
		ImageTags: map[string]string{"Primary": "primary-tag", "Logo": "logo-tag"},
		BackdropImageTags: []string{
			"backdrop-0",
			"backdrop-1",
		},
		People: []personDTO{{ID: "person-1", Name: "Actor", PrimaryImageTag: "person-tag"}},
	}}
	sources := sourcesFromItems(items, "item")
	keys := map[string]bool{}
	for _, source := range sources {
		keys[sourceKey(source)] = true
	}
	for _, want := range []string{
		"item-1|Primary|-1|primary-tag",
		"item-1|Logo|-1|logo-tag",
		"item-1|Backdrop|0|backdrop-0",
		"item-1|Backdrop|1|backdrop-1",
		"person-1|Primary|-1|person-tag",
	} {
		if !keys[want] {
			t.Fatalf("missing source %s in %#v", want, sources)
		}
	}
}

func TestPlanWarmURLsGeneratesCanonicalVariants(t *testing.T) {
	sources := []imageSource{{ItemID: "item 1", ImageType: "Primary", ImageIndex: -1, Tag: "tag+1"}, {ItemID: "item-2", ImageType: "Backdrop", ImageIndex: 1, Tag: "tag-2"}}
	urls := planWarmURLs(sources, []int{400, 800}, 90)
	joined := []string{}
	for _, u := range urls {
		joined = append(joined, u.Path)
	}
	text := strings.Join(joined, "\n")
	for _, want := range []string{
		"/Items/item%201/Images/Primary?tag=tag%2B1",
		"/Items/item%201/Images/Primary?maxWidth=400&tag=tag%2B1&quality=90",
		"/Items/item%201/Images/Primary?maxWidth=800&tag=tag%2B1&quality=90",
		"/Items/item-2/Images/Backdrop/1?tag=tag-2",
		"/Items/item-2/Images/Backdrop/1?maxWidth=400&tag=tag-2&quality=90",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing planned URL %q in:\n%s", want, text)
		}
	}
	if len(urls) != 6 {
		t.Fatalf("planned URL count = %d, want 6", len(urls))
	}
}
