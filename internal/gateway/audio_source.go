package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

type audioSourceAccess struct {
	server        *Server
	session       Session
	request       *http.Request
	upstream      upstreamRequestSnapshot
	token, itemID string
	media         transcode.MediaSource
	mu            sync.Mutex
	reference     string
	headers       map[string]string
	revision      uint64
	etag          string
}

func (s *Server) audioSource(r *http.Request, session *Session, upstream upstreamRequestSnapshot, token, itemID string, media transcode.MediaSource) transcode.Source {
	access := &audioSourceAccess{server: s, session: *session, request: r.Clone(context.Background()), upstream: upstream, token: token, itemID: itemID, media: media, headers: media.RequiredHTTPHeaders}
	access.request.Body = nil
	access.reference = rewriteMediaReference(media.DirectStreamURL, session, upstream, token, s.gatewayBaseForRequest(r), s.cfg.GatewayServerID, false)
	return transcode.Source{Media: media, Open: access.open, Valid: access.valid}
}

func (a *audioSourceAccess) valid(ctx context.Context) bool {
	session, err := a.server.store.FindSessionByTokenHash(ctx, a.session.GatewayTokenHash)
	return err == nil && session.Active(time.Now().UTC())
}

func (a *audioSourceAccess) snapshot(ctx context.Context) (upstreamRequestSnapshot, error) {
	if !a.valid(ctx) {
		return upstreamRequestSnapshot{}, transcode.ErrSource
	}
	runtime, err := a.server.upstreamAuth.Ensure(ctx)
	if err != nil {
		return upstreamRequestSnapshot{}, transcode.ErrSource
	}
	upstream, err := upstreamRequestSnapshotFromRuntimeEndpoint(runtime, a.upstream.endpointKey)
	if err != nil || upstream.serverID != a.upstream.serverID || upstream.userID != a.upstream.userID || upstream.baseURL != a.upstream.baseURL {
		return upstreamRequestSnapshot{}, transcode.ErrSource
	}
	return upstream, nil
}

func (a *audioSourceAccess) refresh(ctx context.Context, upstream upstreamRequestSnapshot, revision uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if revision != a.revision {
		return nil
	}
	info, err := a.server.fetchDownloadPlaybackInfo(ctx, a.itemID, a.media.ID, upstream)
	if err != nil {
		return transcode.ErrSource
	}
	source, ok := selectDownloadMediaSource(info.MediaSources, a.media.ID)
	if !ok || source.Size != nil && *source.Size != a.media.Size {
		return transcode.ErrSource
	}
	a.reference = rewriteMediaReference(source.DirectStreamURL, &a.session, upstream, a.token, a.server.gatewayBaseForRequest(a.request), a.server.cfg.GatewayServerID, false)
	a.headers = source.RequiredHTTPHeaders
	a.revision++
	return nil
}

func (a *audioSourceAccess) open(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 || length > a.media.Size-offset {
		return nil, transcode.ErrSource
	}
	// Acquire once per source transfer, before validating its binding. A run
	// must not hold another read lock while making these requests: Go RWMutex
	// blocks new readers behind a waiting reconfiguration writer.
	a.server.beginMediaCopy()
	ownsGate := true
	defer func() {
		if ownsGate {
			a.server.endMediaCopy()
		}
	}()
	upstream, err := a.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		a.mu.Lock()
		reference, headers, revision := a.reference, a.headers, a.revision
		a.mu.Unlock()
		parsed, err := url.Parse(reference)
		if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(strings.ToLower(parsed.Path), "/videos/"+strings.ToLower(a.itemID)+"/") {
			return nil, transcode.ErrSource
		}
		if attempt == 0 {
			if raw := parsed.Query().Get("exp"); raw != "" {
				if expires, e := strconv.ParseInt(raw, 10, 64); e == nil && expires < time.Now().Add(30*time.Second).Unix() {
					if err = a.refresh(ctx, upstream, revision); err != nil {
						return nil, err
					}
					continue
				}
			}
		}
		decision, err := a.server.store.CheckPathPolicy(ctx, http.MethodGet, parsed.Path)
		if err != nil || !decision.Allowed {
			return nil, transcode.ErrSource
		}
		target, err := a.server.proxyURL(upstream, &a.session, parsed.Path, parsed.RawQuery, a.token)
		if err != nil {
			return nil, transcode.ErrSource
		}
		request, err := http.NewRequestWithContext(withRedirectCredentialTokens(ctx, a.token, upstream.token), http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, transcode.ErrSource
		}
		for name, value := range headers {
			if validDownloadRequiredHeader(name, value) {
				request.Header.Set(name, value)
			}
		}
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
		a.server.rewriteRequestHeaders(request.Header, upstream)
		request.Host = target.Host
		response, err := a.server.proxyClient.Do(request)
		if err != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			return nil, transcode.ErrSource
		}
		if attempt == 0 && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			status := response.StatusCode
			_ = response.Body.Close()
			if status == http.StatusUnauthorized {
				if refreshed, confirmed, e := a.server.refreshAfterUnauthorized(ctx, upstream); e == nil && confirmed {
					upstream = refreshed
				} else {
					return nil, transcode.ErrSource
				}
			}
			if err = a.refresh(ctx, upstream, revision); err != nil {
				return nil, err
			}
			continue
		}
		good := false
		if response.StatusCode == http.StatusPartialContent {
			var start, end, size int64
			if n, e := fmt.Sscanf(response.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &size); e == nil && n == 3 && start == offset && end >= offset+length-1 && size == a.media.Size {
				good = true
			}
		}
		if response.StatusCode == http.StatusOK && offset == 0 && response.ContentLength == a.media.Size {
			good = true
		}
		a.mu.Lock()
		etag := response.Header.Get("ETag")
		if a.etag != "" && etag != "" && a.etag != etag {
			good = false
		}
		if good && a.etag == "" {
			a.etag = etag
		}
		a.mu.Unlock()
		if !good {
			_ = response.Body.Close()
			return nil, transcode.ErrSource
		}
		ownsGate = false
		return &audioSourceBody{Reader: io.LimitReader(response.Body, length), body: response.Body, release: a.server.endMediaCopy, meter: a.server.meter}, nil
	}
	return nil, transcode.ErrSource
}

type audioSourceBody struct {
	io.Reader
	body    io.Closer
	release func()
	meter   TrafficMeter
	once    sync.Once
	err     error
}

func (b *audioSourceBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if b.meter != nil && n > 0 {
		b.meter.AddIngress(int64(n))
	}
	return n, err
}
func (b *audioSourceBody) Close() error {
	b.once.Do(func() { b.err = b.body.Close(); b.release() })
	return b.err
}
