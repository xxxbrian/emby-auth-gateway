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

// USTAR header field sizes (archive/tar nameSize / prefixSize). Trusted catalog
// paths must be strictly representable so built-in catalogs support dir, archive
// (FormatUSTAR), and URL acquisition modes without PAX/GNU long-name extensions.
const (
	ustarNameMax   = 100
	ustarPrefixMax = 155
)

// pathIsASCII reports whether s is a NUL-free ASCII string (bytes < 0x80),
// matching archive/tar isASCII used by the USTAR writer/reader.
func pathIsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 || s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// ustarNamePrefixRepresentable reports whether name can be stored in a strict
// USTAR header using only the 100-byte name field and optional 155-byte prefix,
// mirroring archive/tar.splitUSTARPath / allowedFormats USTAR rules.
//
// A path is representable when it is ASCII and either fits in name (≤100 bytes)
// or splits on a '/' into non-empty prefix (≤155) and suffix (≤100).
func ustarNamePrefixRepresentable(name string) bool {
	if !pathIsASCII(name) {
		return false
	}
	if len(name) <= ustarNameMax {
		return true
	}
	// Mirror archive/tar.splitUSTARPath exactly.
	length := len(name)
	if length > ustarPrefixMax+1 {
		length = ustarPrefixMax + 1
	} else if name[length-1] == '/' {
		length--
	}
	i := strings.LastIndex(name[:length], "/")
	nlen := len(name) - i - 1 // suffix length
	plen := i                 // prefix length
	if i <= 0 || nlen > ustarNameMax || nlen == 0 || plen > ustarPrefixMax {
		return false
	}
	return true
}

// validTrustedCatalogPath enforces admission path rules for trusted catalogs:
// canonical relative asset path, ASCII-only, and strict USTAR name/prefix form.
func validTrustedCatalogPath(p string) error {
	if !validAssetPath(p) {
		return fmt.Errorf("path %q is not a canonical relative path", p)
	}
	if !pathIsASCII(p) {
		return fmt.Errorf("path %q is not ASCII", p)
	}
	if !ustarNamePrefixRepresentable(p) {
		return fmt.Errorf("path %q is not USTAR name/prefix representable", p)
	}
	return nil
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
