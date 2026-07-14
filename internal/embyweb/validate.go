package embyweb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"unicode"
)

// mediaTypes maps file extensions (including the leading dot, lowercased) to
// the exact Content-Type the install manifest must declare.
var mediaTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".webmanifest": "application/manifest+json",
	".map":         "application/json",
	".txt":         "text/plain; charset=utf-8",
	".xml":         "application/xml",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".eot":         "application/vnd.ms-fontobject",
	".mp3":         "audio/mpeg",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
	".wasm":        "application/wasm",
	".md":          "text/markdown; charset=utf-8",
}

func expectedMediaType(assetPath string) (string, bool) {
	ext := strings.ToLower(path.Ext(assetPath))
	mt, ok := mediaTypes[ext]
	return mt, ok
}

func validReleaseBasename(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, `/\`+"\x00") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if r == 0 || unicode.IsControl(r) {
			return false
		}
	}
	// Single path segment only.
	if !fs.ValidPath(s) || path.Base(s) != s || path.Clean(s) != s {
		return false
	}
	return true
}

func validAssetPath(p string) bool {
	if p == "" {
		return false
	}
	if len(p) > maxPathBytes {
		return false
	}
	if strings.ContainsAny(p, `\`+"\x00") {
		return false
	}
	if !fs.ValidPath(p) {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	// Reject Windows drive-like and absolute forms that ValidPath might allow
	// as ordinary characters on non-Windows semantics.
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "./") {
		return false
	}
	segs := strings.Split(p, "/")
	if len(segs) > maxPathDepth {
		return false
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		if strings.ContainsAny(seg, `\`+"\x00") {
			return false
		}
	}
	return true
}

func validSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validCacheClass(s string) bool {
	return s == cacheRevalidate || s == cacheImmutable
}

func decodeStrictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON data")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}

func isCanaryPath(rel string) bool {
	for _, c := range canaryRelativePaths {
		if rel == c {
			return true
		}
	}
	return false
}

func forceRevalidate(rel, mediaType string) bool {
	if isCanaryPath(rel) {
		return true
	}
	// HTML always revalidates (index and any other HTML).
	return strings.HasPrefix(mediaType, "text/html")
}
