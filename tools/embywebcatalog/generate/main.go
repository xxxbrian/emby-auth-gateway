// Command generate inventories a prepared Emby Web asset tree and emits a
// schema-1 catalog JSON document in exact canonical form.
//
// Maintainer-only tool. Does not import internal/embyweb; media types, canaries,
// cache rules, and encoding are re-declared here for independent generation.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const schemaVersion = 1

// Package canaries in fixed production order.
var canaries = []string{
	"manifest.json",
	"index.html",
	"strings/en-US.json",
}

// Extension → Content-Type map matching production validate.go (re-declared).
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

type catalog struct {
	Schema            int      `json:"schema"`
	ID                string   `json:"id"`
	Version           string   `json:"version"`
	SourceImage       string   `json:"source_image"`
	SourceImageDigest string   `json:"source_image_digest"`
	Canaries          []string `json:"canaries"`
	Entries           []entry  `json:"entries"`
}

type entry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	MediaType  string `json:"media_type"`
	CacheClass string `json:"cache_class"`
}

func main() {
	tree := flag.String("tree", "", "path to prepared asset tree root")
	id := flag.String("id", "", "catalog id")
	version := flag.String("version", "", "catalog version")
	sourceImage := flag.String("source-image", "", "source image product identifier")
	sourceImageDigest := flag.String("source-image-digest", "", "source image digest (sha256:<64 hex>)")
	out := flag.String("out", "", "write catalog JSON to this path (default: stdout)")
	flag.Parse()

	if err := run(*tree, *id, *version, *sourceImage, *sourceImageDigest, *out); err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		os.Exit(1)
	}
}

func run(tree, id, version, sourceImage, sourceImageDigest, outPath string) error {
	if tree == "" {
		return fmt.Errorf("--tree is required")
	}
	if id == "" {
		return fmt.Errorf("--id is required")
	}
	if version == "" {
		return fmt.Errorf("--version is required")
	}
	if sourceImage == "" {
		return fmt.Errorf("--source-image is required")
	}
	if sourceImageDigest == "" {
		return fmt.Errorf("--source-image-digest is required")
	}

	absTree, err := filepath.Abs(tree)
	if err != nil {
		return fmt.Errorf("resolve tree: %w", err)
	}
	fi, err := os.Lstat(absTree)
	if err != nil {
		return fmt.Errorf("stat tree: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("tree root must not be a symlink: %s", absTree)
	}
	if !fi.IsDir() {
		return fmt.Errorf("tree root is not a directory: %s", absTree)
	}

	entries, err := inventoryTree(absTree)
	if err != nil {
		return err
	}
	if err := requireCanaries(entries); err != nil {
		return err
	}

	cat := catalog{
		Schema:            schemaVersion,
		ID:                id,
		Version:           version,
		SourceImage:       sourceImage,
		SourceImageDigest: sourceImageDigest,
		Canaries:          append([]string(nil), canaries...),
		Entries:           entries,
	}
	raw, err := encodeCatalog(cat)
	if err != nil {
		return err
	}
	digest := sha256Hex(raw)

	if outPath == "" {
		if _, err := os.Stdout.Write(raw); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
	} else {
		if err := os.WriteFile(outPath, raw, 0o644); err != nil {
			return fmt.Errorf("write --out: %w", err)
		}
	}
	// Digest of exact catalog bytes always goes to stderr for piping safety.
	fmt.Fprintln(os.Stderr, digest)
	return nil
}

func inventoryTree(root string) ([]entry, error) {
	var entries []entry
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Always use slash-separated catalog paths.
		relSlash := filepath.ToSlash(rel)

		mode := d.Type()
		if mode&fs.ModeSymlink != 0 {
			return fmt.Errorf("symlink rejected: %s", relSlash)
		}
		if d.IsDir() {
			return nil
		}
		if !mode.IsRegular() {
			// Re-check with Lstat for platforms where Type() is incomplete.
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat %s: %w", relSlash, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("non-regular file rejected: %s (mode %s)", relSlash, info.Mode())
			}
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", relSlash, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file rejected: %s (mode %s)", relSlash, info.Mode())
		}

		mt, ok := mediaTypeFor(relSlash)
		if !ok {
			return fmt.Errorf("unknown extension for path %q", relSlash)
		}
		sum, size, err := hashFile(full)
		if err != nil {
			return fmt.Errorf("hash %s: %w", relSlash, err)
		}
		if size != info.Size() {
			return fmt.Errorf("size race on %s: lstat=%d read=%d", relSlash, info.Size(), size)
		}
		entries = append(entries, entry{
			Path:       relSlash,
			Size:       size,
			SHA256:     sum,
			MediaType:  mt,
			CacheClass: cacheClassFor(relSlash, mt),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Path == entries[i].Path {
			return nil, fmt.Errorf("duplicate path %q", entries[i].Path)
		}
	}
	return entries, nil
}

func mediaTypeFor(assetPath string) (string, bool) {
	ext := strings.ToLower(path.Ext(assetPath))
	mt, ok := mediaTypes[ext]
	return mt, ok
}

func cacheClassFor(rel, mediaType string) string {
	for _, c := range canaries {
		if rel == c {
			return "revalidate"
		}
	}
	if strings.HasPrefix(mediaType, "text/html") {
		return "revalidate"
	}
	return "immutable"
}

func requireCanaries(entries []entry) error {
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		seen[e.Path] = struct{}{}
	}
	for _, c := range canaries {
		if _, ok := seen[c]; !ok {
			return fmt.Errorf("tree missing required canary %q", c)
		}
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func encodeCatalog(c catalog) ([]byte, error) {
	out := c
	if out.Entries != nil {
		out.Entries = append([]entry(nil), c.Entries...)
		sort.SliceStable(out.Entries, func(i, j int) bool {
			return out.Entries[i].Path < out.Entries[j].Path
		})
	}
	if out.Canaries != nil {
		out.Canaries = append([]string(nil), c.Canaries...)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
