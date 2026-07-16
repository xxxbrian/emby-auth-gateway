package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/image/webp"
)

func TestAnonymousItemImageForwardsOnlyValidatedTokenlessRequest(t *testing.T) {
	const namespace = "namespace-1"
	var imageCalls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/System/Info/Public":
			writeTestJSON(w, map[string]any{"Id": namespace})
		case "/emby/Items/person/Images/Primary/0", "/emby/Items/person/Images/Primary":
			imageCalls++
			if r.Method == http.MethodGet && r.URL.RawQuery != "width=100&sig=a%2Bb" {
				t.Fatalf("anonymous GET query = %q", r.URL.RawQuery)
			}
			if r.Header.Get("Cookie") != "" || r.Header.Get("X-Emby-Token") != "" || r.Header.Get("Authorization") != "" || r.UserAgent() != "SenPlayer/6.1.3" {
				t.Fatalf("anonymous request query/cookie/token/auth/ua = %q/%q/%q/%q/%q", r.URL.RawQuery, r.Header.Get("Cookie"), r.Header.Get("X-Emby-Token"), r.Header.Get("Authorization"), r.UserAgent())
			}
			auth := r.Header.Get("X-Emby-Authorization")
			if auth == "" || strings.Contains(auth, "UserId=") || strings.Contains(auth, "Token=") || (r.Method == http.MethodGet && (r.Header.Get("Range") != "bytes=0-" || r.Header.Get("If-None-Match") != `"etag"`)) {
				t.Fatalf("anonymous identity/range = %q/%q/%q", auth, r.Header.Get("Range"), r.Header.Get("If-None-Match"))
			}
			w.Header().Set("Content-Type", "image/gif")
			w.Header().Set("ETag", `"etag"`)
			w.Header().Set("Set-Cookie", "backend=secret")
			w.Header().Set("Content-Range", "bytes 0-13/14")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(anonymousGIF())
		default:
			t.Fatalf("unexpected backend path %q", r.URL.Path)
		}
	}))
	defer backend.Close()
	server := anonymousImageGateway(t, backend.URL+"/emby", namespace)
	gw := httptest.NewServer(server)
	defer gw.Close()
	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/person/Images/Primary/0?width=100&sig=a%2Bb", nil)
	req.Header.Set("Cookie", "other=keep")
	req.Header.Set("Range", "bytes=0-")
	req.Header.Set("If-None-Match", `"etag"`)
	resp := do(t, req)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != string(anonymousGIF()) || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("ETag") != `"etag"` || imageCalls != 1 {
		t.Fatalf("anonymous image status/body/headers/calls = %d/%q/%#v/%d", resp.StatusCode, body, resp.Header, imageCalls)
	}

	head := do(t, mustRequest(t, http.MethodHead, gw.URL+"/emby/Items/person/Images/Primary", nil))
	headBody, _ := io.ReadAll(head.Body)
	_ = head.Body.Close()
	if head.StatusCode != http.StatusPartialContent || len(headBody) != 0 || head.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("HEAD status/body/cache = %d/%q/%q", head.StatusCode, headBody, head.Header.Get("Cache-Control"))
	}
}

func TestAnonymousItemImagePrecedenceAndAvailability(t *testing.T) {
	const namespace = "namespace-1"
	var calls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/System/Info/Public" {
			writeTestJSON(w, map[string]any{"Id": namespace})
			return
		}
		calls++
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(anonymousGIF())
	}))
	defer backend.Close()
	configured := anonymousImageGateway(t, backend.URL+"/emby", namespace)
	gw := httptest.NewServer(configured)
	defer gw.Close()
	for _, tc := range []struct {
		name  string
		apply func(*http.Request)
	}{
		{"invalid query", func(r *http.Request) { r.URL.RawQuery = "api_key=invalid" }},
		{"empty query", func(r *http.Request) { r.URL.RawQuery = "api_key=" }},
		{"empty generic query", func(r *http.Request) { r.URL.RawQuery = "token=" }},
		{"invalid header", func(r *http.Request) { r.Header.Set("X-Emby-Token", "invalid") }},
		{"empty header", func(r *http.Request) { r.Header.Set("X-Emby-Token", "") }},
		{"empty media header", func(r *http.Request) { r.Header.Set("X-MediaBrowser-Token", "") }},
		{"empty emby authorization", func(r *http.Request) { r.Header.Set("X-Emby-Authorization", "") }},
		{"empty authorization", func(r *http.Request) { r.Header.Set("Authorization", "") }},
		{"malformed authorization", func(r *http.Request) { r.Header.Set("Authorization", "not emby") }},
		{"empty reserved cookie", func(r *http.Request) { r.Header.Set("Cookie", resourceCookieName+"=") }},
		{"duplicate reserved cookie", func(r *http.Request) { r.Header.Set("Cookie", resourceCookieName+"=one; "+resourceCookieName+"=two") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil)
			tc.apply(req)
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "private, no-store" {
				t.Fatalf("status/cache = %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
			}
		})
	}
	if calls != 0 {
		t.Fatalf("credentialed requests reached anonymous upstream: %d", calls)
	}

	unavailable := NewServer(Config{}, anonymousImageTestStore(anonymousImageTestServer("one", backend.URL+"/emby", namespace)))
	unavailableGW := httptest.NewServer(unavailable)
	defer unavailableGW.Close()
	resp := do(t, mustRequest(t, http.MethodGet, unavailableGW.URL+"/emby/Items/item/Images/Primary", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" || calls != 0 {
		t.Fatalf("unavailable status/cache/calls = %d/%q/%d", resp.StatusCode, resp.Header.Get("Cache-Control"), calls)
	}

	absent := httptest.NewServer(NewServer(Config{}, NewMemoryStore()))
	defer absent.Close()
	resp = do(t, mustRequest(t, http.MethodGet, absent.URL+"/emby/Items/item/Images/Primary", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("absent singleton status/cache = %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

func TestAnonymousItemImageScopeAndFailures(t *testing.T) {
	const namespace = "namespace-1"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/System/Info/Public" {
			writeTestJSON(w, map[string]any{"Id": namespace})
			return
		}
		switch r.URL.Query().Get("mode") {
		case "notfound":
			w.WriteHeader(http.StatusNotFound)
		case "bad":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("ETag", `"backend-etag"`)
			_, _ = w.Write([]byte("backend secret"))
		default:
			w.Header().Set("Content-Type", "image/gif")
			_, _ = w.Write(anonymousGIF())
		}
	}))
	defer backend.Close()
	gw := httptest.NewServer(anonymousImageGateway(t, backend.URL+"/emby", namespace))
	defer gw.Close()
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/Items/item/Images/Primary?mode=notfound", http.StatusNotFound},
		{"/Items/item/Images/Primary?mode=bad", http.StatusBadGateway},
		{"/Items/item/Images/Primary?UserId=other", http.StatusBadRequest},
		{"/Users/user/Images/Primary", http.StatusUnauthorized},
		{"/Items/item/Images/Primary/abc", http.StatusUnauthorized},
	} {
		resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby"+tc.path, nil))
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		wantCache := "no-store"
		if strings.HasPrefix(tc.path, "/Users/") {
			wantCache = "private, no-store"
		}
		if strings.HasSuffix(tc.path, "/abc") {
			wantCache = ""
		}
		if resp.StatusCode != tc.want || resp.Header.Get("Cache-Control") != wantCache || strings.Contains(string(body), "backend secret") || (strings.Contains(tc.path, "mode=bad") && resp.Header.Get("ETag") != "") {
			t.Fatalf("%s status/cache/body = %d/%q/%q", tc.path, resp.StatusCode, resp.Header.Get("Cache-Control"), body)
		}
	}
}

func TestAnonymousItemImageOneBytePartialJPEG(t *testing.T) {
	const namespace = "namespace-1"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/System/Info/Public" {
			writeTestJSON(w, map[string]any{"Id": namespace})
			return
		}
		if r.Header.Get("Range") != "bytes=0-0" || r.Header.Get("If-Range") != `"etag"` {
			t.Fatalf("range validators = %q/%q", r.Header.Get("Range"), r.Header.Get("If-Range"))
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Range", "bytes 0-0/4")
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0xff})
	}))
	defer backend.Close()
	gw := httptest.NewServer(anonymousImageGateway(t, backend.URL+"/emby", namespace))
	defer gw.Close()
	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil)
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("If-Range", `"etag"`)
	resp := do(t, req)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(body) != 1 || body[0] != 0xff || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("one-byte partial = %d/%x/%#v", resp.StatusCode, body, resp.Header)
	}
}

func TestAnonymousFullImageFormatValidation(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82}...)
	webp := []byte{'R', 'I', 'F', 'F', 4, 0, 0, 0, 'W', 'E', 'B', 'P'}
	for _, tc := range []struct {
		contentType string
		body        []byte
	}{
		{"image/jpeg", []byte{0xff, 0xd8, 0xff, 0xd9}},
		{"image/png", png},
		{"image/webp", webp},
		{"image/gif", anonymousGIF()},
	} {
		if !validAnonymousFullImage(tc.body, tc.contentType) || validAnonymousFullImage(tc.body[:len(tc.body)-1], tc.contentType) || validAnonymousFullImage([]byte("not an image"), tc.contentType) {
			t.Fatalf("format validation failed for %s", tc.contentType)
		}
	}
	if !validAnonymousContentRange("bytes 0-0/4", 1) || validAnonymousContentRange("bytes 1-0/4", 1) || validAnonymousContentRange("bytes 0-1/4", 1) {
		t.Fatal("Content-Range validation mismatch")
	}
}

func TestAnonymousFullImageValidationAcceptsStdlibEncodedFixtures(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	var jpegBody, pngBody bytes.Buffer
	if err := jpeg.Encode(&jpegBody, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&pngBody, img); err != nil {
		t.Fatal(err)
	}
	if !validAnonymousFullImage(jpegBody.Bytes(), "image/jpeg") || !validAnonymousFullImage(pngBody.Bytes(), "image/png") {
		t.Fatal("stdlib image fixture rejected")
	}
}

func TestAnonymousPartialImageAbortsOnTruncationOrReadError(t *testing.T) {
	for _, body := range []io.Reader{strings.NewReader("x"), errorAfterReader{Reader: strings.NewReader("x"), err: errors.New("upstream failed")}} {
		server := NewServer(Config{}, NewMemoryStore())
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://gateway/emby/Items/item/Images/Primary", nil)
		resp := &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Type": []string{"image/jpeg"}, "Content-Range": []string{"bytes 0-1/4"}}, Body: io.NopCloser(body), ContentLength: -1}
		func() {
			defer func() {
				if recovered := recover(); recovered != http.ErrAbortHandler {
					t.Fatalf("recover = %#v, want ErrAbortHandler", recovered)
				}
			}()
			server.writeAnonymousPartialImage(recorder, req, resp)
		}()
	}
}

type errorAfterReader struct {
	io.Reader
	err error
}

func (r errorAfterReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF && n == 0 {
		return 0, r.err
	}
	return n, err
}

func TestAnonymousItemImage304AndValidationSemaphore(t *testing.T) {
	const namespace = "namespace-1"
	mode := "notmodified"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/System/Info/Public" {
			writeTestJSON(w, map[string]any{"Id": namespace})
			return
		}
		if mode == "notmodified" {
			w.Header().Set("ETag", `"etag"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(anonymousGIF())
	}))
	defer backend.Close()
	server := anonymousImageGateway(t, backend.URL+"/emby", namespace)
	gw := httptest.NewServer(server)
	defer gw.Close()
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified || len(body) != 0 || resp.Header.Get("ETag") != `"etag"` || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("304 response = %d/%q/%#v", resp.StatusCode, body, resp.Header)
	}
	mode = "full"
	for i := 0; i < cap(server.anonymousImageSlots); i++ {
		server.anonymousImageSlots <- struct{}{}
	}
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("semaphore response = %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	for i := 0; i < cap(server.anonymousImageSlots); i++ {
		<-server.anonymousImageSlots
	}
}

func TestAnonymousImageNonBufferedStatusesBypassValidationSlots(t *testing.T) {
	server := NewServer(Config{}, NewMemoryStore())
	for i := 0; i < cap(server.anonymousImageSlots); i++ {
		server.anonymousImageSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(server.anonymousImageSlots); i++ {
			<-server.anonymousImageSlots
		}
	}()
	cases := []struct {
		method string
		resp   *http.Response
		want   int
	}{
		{http.MethodHead, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/gif"}, "Content-Length": []string{"14"}}, Body: io.NopCloser(strings.NewReader(""))}, http.StatusOK},
		{http.MethodGet, &http.Response{StatusCode: http.StatusNotModified, Header: http.Header{"ETag": []string{`"etag"`}}, Body: io.NopCloser(strings.NewReader(""))}, http.StatusNotModified},
		{http.MethodGet, &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Type": []string{"image/jpeg"}, "Content-Range": []string{"bytes 0-0/4"}}, Body: io.NopCloser(bytes.NewReader([]byte{0xff})), ContentLength: 1}, http.StatusPartialContent},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, "http://gateway/emby/Items/item/Images/Primary", nil)
		server.writeAnonymousImageResponse(recorder, req, tc.resp)
		if recorder.Code != tc.want || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s status/cache = %d/%q", tc.method, recorder.Code, recorder.Header().Get("Cache-Control"))
		}
	}
}

func TestAnonymousItemImageRedirectIsContained(t *testing.T) {
	const namespace = "namespace-1"
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
	defer target.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/System/Info/Public" {
			writeTestJSON(w, map[string]any{"Id": namespace})
			return
		}
		w.Header().Set("Location", target.URL+"/next?api_key=backend-token")
		w.Header().Set("Set-Cookie", "backend=secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer backend.Close()
	gw := httptest.NewServer(anonymousImageGateway(t, backend.URL+"/emby", namespace))
	defer gw.Close()
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Location") != "" || resp.Header.Get("Set-Cookie") != "" || strings.Contains(string(body), backend.URL) || strings.Contains(string(body), "backend-token") || targetCalls != 0 {
		t.Fatalf("redirect response leaked or followed: %d/%#v/%q target=%d", resp.StatusCode, resp.Header, body, targetCalls)
	}
}

func TestAnonymousItemImageUsesOnlySelectedIngress(t *testing.T) {
	const namespace = "namespace-1"
	selectedMode := "notfound"
	var selectedCalls, otherCalls int
	selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/System/Info/Public" {
			writeTestJSON(w, map[string]any{"Id": namespace})
			return
		}
		selectedCalls++
		if selectedMode == "notfound" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/System/Info/Public" {
			writeTestJSON(w, map[string]any{"Id": namespace})
			return
		}
		otherCalls++
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(anonymousGIF())
	}))
	defer selected.Close()
	defer other.Close()
	store := anonymousImageTestStore(anonymousImageTestServer("one", selected.URL+"/emby", namespace), anonymousImageTestServer("two", other.URL+"/emby", namespace))
	server := NewServer(Config{}, store)
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(server)
	defer gw.Close()
	for _, want := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil))
		_ = resp.Body.Close()
		if resp.StatusCode != want || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("selected status/cache = %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
		selectedMode = "failure"
	}
	selected.Close()
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Cache-Control") != "no-store" || selectedCalls != 2 || otherCalls != 0 {
		t.Fatalf("selected/no-retry calls = %d/%d status=%d", selectedCalls, otherCalls, resp.StatusCode)
	}
}

func TestAnonymousItemImageHTTPNamespaceMutationAndRecovery(t *testing.T) {
	const namespace = "namespace-1"
	var imageCalls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/System/Info/Public" {
			writeTestJSON(w, map[string]any{"Id": namespace})
			return
		}
		imageCalls++
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(anonymousGIF())
	}))
	defer backend.Close()
	store := anonymousImageTestStore(anonymousImageTestServer("one", backend.URL+"/emby", namespace))
	server := NewServer(Config{}, store)
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(server)
	defer gw.Close()
	store.mu.Lock()
	endpoint := store.UpstreamEndpoints["endpoint"]
	endpoint.BaseURL = backend.URL + "/changed"
	store.UpstreamEndpoints["endpoint"] = endpoint
	store.mu.Unlock()
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" || imageCalls != 0 {
		t.Fatalf("mutated status/calls = %d/%d", resp.StatusCode, imageCalls)
	}
	store.mu.Lock()
	endpoint.BaseURL = backend.URL + "/emby"
	store.UpstreamEndpoints["endpoint"] = endpoint
	store.mu.Unlock()
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || imageCalls != 1 {
		t.Fatalf("recovery status/calls = %d/%d", resp.StatusCode, imageCalls)
	}
}

func TestAnonymousPartialImageAbortsOnExcessLength(t *testing.T) {
	server := NewServer(Config{}, NewMemoryStore())
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/emby/Items/item/Images/Primary", nil)
	resp := &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Type": []string{"image/jpeg"}, "Content-Range": []string{"bytes 0-0/4"}}, Body: io.NopCloser(strings.NewReader("xx")), ContentLength: -1}
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("recover = %#v", recovered)
		}
	}()
	server.writeAnonymousPartialImage(recorder, req, resp)
}

func TestAnonymousFullImageValidationAcceptsStdlibGIFAndMinimalWebP(t *testing.T) {
	img := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Black, color.White})
	img.SetColorIndex(0, 0, 1)
	var gifBody bytes.Buffer
	if err := gif.Encode(&gifBody, img, nil); err != nil {
		t.Fatal(err)
	}
	if !validAnonymousFullImage(gifBody.Bytes(), "image/gif") {
		t.Fatal("stdlib GIF rejected")
	}
	// Go's standard library does not encode/decode WebP. This compact known 1x1
	// WebP fixture avoids adding a decoder dependency just for this regression.
	webpBody, err := base64.StdEncoding.DecodeString("UklGRiQAAABXRUJQVlA4IBgAAAAwAQCdASoBAAEAAgA0JaQAA3AA/vuUAAA=")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := webp.Decode(bytes.NewReader(webpBody)); err != nil {
		t.Fatalf("known WebP fixture did not decode: %v", err)
	}
	if !validAnonymousFullImage(webpBody, "image/webp") {
		t.Fatal("minimal WebP framing rejected")
	}
}

func anonymousImageGateway(t *testing.T, baseURL, namespace string) *Server {
	t.Helper()
	store := anonymousImageTestStore(anonymousImageTestServer("one", baseURL, namespace))
	server := NewServer(Config{}, store)
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	return server
}

func anonymousGIF() []byte { return []byte("GIF89a\x00\x00\x00\x00\x00\x00\x00;") }
