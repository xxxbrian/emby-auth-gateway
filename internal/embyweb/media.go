package embyweb

import (
	"io/fs"
	"path"
	"strings"
)

// mediaTypes maps file extensions (including the leading dot, lowercased) to
// Content-Type values used when serving from disk.
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

func contentTypeFor(assetPath string) string {
	ext := strings.ToLower(path.Ext(assetPath))
	if mt, ok := mediaTypes[ext]; ok {
		return mt
	}
	return "application/octet-stream"
}

func cacheClassFor(assetPath string) string {
	if needsHostInject(assetPath) {
		return cacheRevalidate
	}
	ext := strings.ToLower(path.Ext(assetPath))
	switch ext {
	case ".html", ".json", ".webmanifest", ".txt", ".xml", ".md":
		return cacheRevalidate
	default:
		return cacheImmutable
	}
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

func isCanaryPath(assetPath string) bool {
	for _, c := range canaryRelativePaths {
		if assetPath == c {
			return true
		}
	}
	return false
}
