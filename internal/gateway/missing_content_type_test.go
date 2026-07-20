package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestMissingContentTypeSniffClassification(t *testing.T) {
	for _, test := range []struct {
		body string
		json bool
	}{
		{body: " \t\r\n{\"Name\":\"value\"}", json: true},
		{body: "raw"},
		{body: "true"},
		{body: strings.Repeat(" ", missingContentTypeSniffSize)},
	} {
		reader, candidate, err := sniffMissingContentType(strings.NewReader(test.body))
		if err != nil || candidate != test.json {
			t.Fatalf("body %q candidate=%v error=%v", test.body, candidate, err)
		}
		if replay, err := io.ReadAll(reader); err != nil || string(replay) != test.body {
			t.Fatalf("body replay=%q error=%v", replay, err)
		}
	}
}

func TestMissingContentTypeOpaqueSafeExact(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	body := " {\n \"Unknown\":1, \"Unknown\":2\n}\n"
	headers := make(http.Header)
	headers.Set("ETag", `"opaque"`)
	headers.Set("Last-Modified", "yesterday")
	response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item/Images", http.StatusOK, "", body, headers)
	if response.Code != http.StatusOK || response.Body.String() != body {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("ETag") != `"opaque"` || response.Header().Get("Last-Modified") != "yesterday" || response.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("headers=%v", response.Header())
	}
}

func TestMissingContentTypeOpaqueUnsafeAndOversizeFailClosed(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	for _, body := range []string{
		`{"Unknown":"backend-token"}`,
		`{"Unknown":"backend\u002dtoken"}`,
		strings.Repeat("x", proxyJSONLimit+1),
	} {
		headers := make(http.Header)
		headers.Set("ETag", `"stale"`)
		response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item/Images", http.StatusOK, "", body, headers)
		if response.Code != http.StatusBadGateway || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("ETag") != "" || strings.Contains(response.Body.String(), "backend-token") {
			t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
		}
	}
}

func TestMissingContentTypeProjectedJSONAndMalformed(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	response := writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item", http.StatusOK, "", `{"Id":"item","ServerId":"backend-server"}`, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ServerId":"gateway-server"`) || response.Header().Get("ETag") != "" {
		t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	response = writeProjectedTestResponse(server, session, http.MethodGet, "/Items/item", http.StatusOK, "", `{"Id":`, nil)
	if response.Code != http.StatusBadGateway || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

func TestMissingContentTypeMedia206DispatchUnchanged(t *testing.T) {
	server, session := responseProjectionTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "https://gateway.test/emby/Videos/item/stream", nil)
	request.Header.Set("Range", "bytes=0-3")
	responseRequest := request.Clone(request.Context())
	responseRequest.URL.Path = "/Videos/item/stream"
	response := &http.Response{
		StatusCode:    http.StatusPartialContent,
		Header:        http.Header{"Content-Range": {"bytes 0-3/4"}, "Accept-Ranges": {"bytes"}},
		Body:          io.NopCloser(bytes.NewReader([]byte("data"))),
		ContentLength: 4,
		Request:       responseRequest,
	}
	writer := httptest.NewRecorder()
	server.writeProxyResponseWithSnapshot(writer, request, "/Videos/item/stream", response, session, upstreamRequestSnapshot{}, "", "")
	if writer.Code != http.StatusPartialContent || writer.Body.String() != "data" || writer.Header().Get("Content-Range") != "bytes 0-3/4" {
		t.Fatalf("status=%d headers=%v body=%q", writer.Code, writer.Header(), writer.Body.String())
	}
}
