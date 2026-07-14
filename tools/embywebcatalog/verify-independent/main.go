// Command verify-independent re-inventories a prepared Emby Web asset tree and
// checks an existing schema-1 catalog file for exact field and byte identity.
//
// Expected catalog identity must be supplied on the CLI (--expect-*) and is
// compared directly to the catalog under verification. The tool never trusts
// identity fields copied from the catalog alone.
//
// Maintainer-only tool. Completely independent of tools/embywebcatalog/generate
// and of internal/embyweb: walk, hash, media/cache assignment, and canonical
// encoding are reimplemented here so a shared bug cannot pass both paths.
//
// Usage:
//
//	verify-independent \
//	  --tree /path/to/prepared \
//	  --catalog /path/to/catalog.json \
//	  --expect-id emby-web-4.9.5.0 \
//	  --expect-version 4.9.5.0 \
//	  --expect-source-image emby/embyserver \
//	  --expect-source-image-digest sha256:<64-hex> \
//	  --expect-digest <64-hex of catalog bytes>
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const catalogSchema = 1

// Fixed canary list (production order).
var requiredCanaries = [3]string{
	"manifest.json",
	"index.html",
	"strings/en-US.json",
}

// Content-Type by lowercased extension (production table, re-declared).
var contentTypesByExt = map[string]string{
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

type catalogDoc struct {
	Schema            int         `json:"schema"`
	ID                string      `json:"id"`
	Version           string      `json:"version"`
	SourceImage       string      `json:"source_image"`
	SourceImageDigest string      `json:"source_image_digest"`
	Canaries          []string    `json:"canaries"`
	Entries           []fileEntry `json:"entries"`
}

type fileEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	MediaType  string `json:"media_type"`
	CacheClass string `json:"cache_class"`
}

// diskFile is one regular file discovered under the tree.
type diskFile struct {
	rel        string
	size       int64
	sha256     string
	mediaType  string
	cacheClass string
}

// expectIdentity is the operator-supplied identity pin for the catalog under
// verification. All fields are required.
type expectIdentity struct {
	ID                string
	Version           string
	SourceImage       string
	SourceImageDigest string
	Digest            string // lowercase 64-hex of exact catalog file bytes
}

func main() {
	tree := flag.String("tree", "", "path to prepared asset tree root")
	catalogPath := flag.String("catalog", "", "path to catalog JSON to verify")
	expectID := flag.String("expect-id", "", "expected catalog id")
	expectVersion := flag.String("expect-version", "", "expected catalog version")
	expectSourceImage := flag.String("expect-source-image", "", "expected source_image")
	expectSourceImageDigest := flag.String("expect-source-image-digest", "", "expected source_image_digest (sha256:<64 hex>)")
	expectDigest := flag.String("expect-digest", "", "expected SHA-256 of exact catalog file bytes (lowercase 64-hex)")
	flag.Parse()

	exp := expectIdentity{
		ID:                *expectID,
		Version:           *expectVersion,
		SourceImage:       *expectSourceImage,
		SourceImageDigest: *expectSourceImageDigest,
		Digest:            *expectDigest,
	}
	if err := verify(*tree, *catalogPath, exp); err != nil {
		fmt.Fprintf(os.Stderr, "verify-independent: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "ok")
}

func verify(treeRoot, catalogPath string, exp expectIdentity) error {
	if treeRoot == "" {
		return fmt.Errorf("--tree is required")
	}
	if catalogPath == "" {
		return fmt.Errorf("--catalog is required")
	}
	if err := validateExpect(exp); err != nil {
		return err
	}

	committed, err := os.ReadFile(catalogPath)
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	if len(committed) == 0 {
		return fmt.Errorf("catalog is empty")
	}

	// Digest of exact file bytes must match the operator pin first.
	gotDigest := digestHex(committed)
	if gotDigest != exp.Digest {
		return fmt.Errorf("catalog digest mismatch: got %s want %s", gotDigest, exp.Digest)
	}

	var doc catalogDoc
	dec := json.NewDecoder(bytes.NewReader(committed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("parse catalog: %w", err)
	}
	// Reject trailing JSON.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("catalog has trailing JSON data")
	}

	if doc.Schema != catalogSchema {
		return fmt.Errorf("catalog schema %d want %d", doc.Schema, catalogSchema)
	}
	if err := checkCanaryField(doc.Canaries); err != nil {
		return err
	}

	// Identity fields must match operator expectations (not self-derived).
	if doc.ID != exp.ID {
		return fmt.Errorf("catalog id mismatch: got %q want %q", doc.ID, exp.ID)
	}
	if doc.Version != exp.Version {
		return fmt.Errorf("catalog version mismatch: got %q want %q", doc.Version, exp.Version)
	}
	if doc.SourceImage != exp.SourceImage {
		return fmt.Errorf("catalog source_image mismatch: got %q want %q", doc.SourceImage, exp.SourceImage)
	}
	if doc.SourceImageDigest != exp.SourceImageDigest {
		return fmt.Errorf("catalog source_image_digest mismatch: got %q want %q", doc.SourceImageDigest, exp.SourceImageDigest)
	}

	// Canonical form: re-encode and require byte identity with committed file.
	canonical, err := marshalCanonical(doc)
	if err != nil {
		return fmt.Errorf("canonical encode: %w", err)
	}
	if !bytes.Equal(committed, canonical) {
		return fmt.Errorf("catalog bytes are not canonical (indent/order/trailing newline mismatch)")
	}

	absRoot, err := filepath.Abs(treeRoot)
	if err != nil {
		return fmt.Errorf("resolve tree: %w", err)
	}
	st, err := os.Lstat(absRoot)
	if err != nil {
		return fmt.Errorf("stat tree: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("tree root must not be a symlink")
	}
	if !st.IsDir() {
		return fmt.Errorf("tree root is not a directory")
	}

	// Independent inventory: recursive ReadDir (not filepath.WalkDir).
	found, err := scanTree(absRoot)
	if err != nil {
		return err
	}

	// Sort disk inventory by path for ordered comparison.
	sort.Slice(found, func(i, j int) bool { return found[i].rel < found[j].rel })

	if len(found) != len(doc.Entries) {
		return fmt.Errorf("entry count mismatch: tree=%d catalog=%d", len(found), len(doc.Entries))
	}

	// Catalog entries must already be strictly path-sorted and unique.
	for i := 1; i < len(doc.Entries); i++ {
		if doc.Entries[i-1].Path >= doc.Entries[i].Path {
			return fmt.Errorf("catalog entries not strictly sorted by path at index %d", i)
		}
	}

	for i := range doc.Entries {
		want := doc.Entries[i]
		got := found[i]
		if got.rel != want.Path {
			return fmt.Errorf("path mismatch at index %d: tree=%q catalog=%q", i, got.rel, want.Path)
		}
		if got.size != want.Size {
			return fmt.Errorf("size mismatch for %q: tree=%d catalog=%d", want.Path, got.size, want.Size)
		}
		if got.sha256 != want.SHA256 {
			return fmt.Errorf("sha256 mismatch for %q: tree=%s catalog=%s", want.Path, got.sha256, want.SHA256)
		}
		if got.mediaType != want.MediaType {
			return fmt.Errorf("media_type mismatch for %q: tree=%q catalog=%q", want.Path, got.mediaType, want.MediaType)
		}
		if got.cacheClass != want.CacheClass {
			return fmt.Errorf("cache_class mismatch for %q: tree=%q catalog=%q", want.Path, got.cacheClass, want.CacheClass)
		}
	}

	// Re-derive expected media/cache for every catalog entry independently.
	for _, e := range doc.Entries {
		mt, ok := lookupMedia(e.Path)
		if !ok {
			return fmt.Errorf("catalog path %q has unknown extension", e.Path)
		}
		if e.MediaType != mt {
			return fmt.Errorf("catalog media_type for %q is %q want %q", e.Path, e.MediaType, mt)
		}
		cc := assignCache(e.Path, mt)
		if e.CacheClass != cc {
			return fmt.Errorf("catalog cache_class for %q is %q want %q", e.Path, e.CacheClass, cc)
		}
	}

	// Ensure all required canary paths exist among entries.
	have := make(map[string]struct{}, len(doc.Entries))
	for _, e := range doc.Entries {
		have[e.Path] = struct{}{}
	}
	for _, c := range requiredCanaries {
		if _, ok := have[c]; !ok {
			return fmt.Errorf("catalog missing canary entry %q", c)
		}
	}

	// Rebuild from tree + operator-expected identity (never copy identity from
	// the catalog under test) and require exact byte identity with committed file.
	rebuilt := catalogDoc{
		Schema:            catalogSchema,
		ID:                exp.ID,
		Version:           exp.Version,
		SourceImage:       exp.SourceImage,
		SourceImageDigest: exp.SourceImageDigest,
		Canaries:          append([]string(nil), requiredCanaries[:]...),
		Entries:           make([]fileEntry, len(found)),
	}
	for i, f := range found {
		rebuilt.Entries[i] = fileEntry{
			Path:       f.rel,
			Size:       f.size,
			SHA256:     f.sha256,
			MediaType:  f.mediaType,
			CacheClass: f.cacheClass,
		}
	}
	rebuiltBytes, err := marshalCanonical(rebuilt)
	if err != nil {
		return err
	}
	if !bytes.Equal(rebuiltBytes, committed) {
		return fmt.Errorf("tree-derived catalog bytes differ from committed catalog (digest tree=%s catalog=%s)",
			digestHex(rebuiltBytes), gotDigest)
	}
	if digestHex(rebuiltBytes) != exp.Digest {
		return fmt.Errorf("tree-derived catalog digest mismatch: got %s want %s", digestHex(rebuiltBytes), exp.Digest)
	}
	return nil
}

func validateExpect(exp expectIdentity) error {
	if exp.ID == "" {
		return fmt.Errorf("--expect-id is required")
	}
	if exp.Version == "" {
		return fmt.Errorf("--expect-version is required")
	}
	if exp.SourceImage == "" {
		return fmt.Errorf("--expect-source-image is required")
	}
	if exp.SourceImageDigest == "" {
		return fmt.Errorf("--expect-source-image-digest is required")
	}
	if exp.Digest == "" {
		return fmt.Errorf("--expect-digest is required")
	}
	if !validSHA256Hex(exp.Digest) {
		return fmt.Errorf("--expect-digest must be lowercase 64-hex")
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

func checkCanaryField(got []string) error {
	if len(got) != len(requiredCanaries) {
		return fmt.Errorf("canaries length %d want %d", len(got), len(requiredCanaries))
	}
	for i, want := range requiredCanaries {
		if got[i] != want {
			return fmt.Errorf("canaries[%d]=%q want %q", i, got[i], want)
		}
	}
	return nil
}

// scanTree walks with Open/ReadDir recursion (independent of generate's WalkDir).
func scanTree(root string) ([]diskFile, error) {
	var out []diskFile
	var walk func(absDir, relDir string) error
	walk = func(absDir, relDir string) error {
		f, err := os.Open(absDir)
		if err != nil {
			return err
		}
		defer f.Close()

		for {
			batch, err := f.ReadDir(64)
			if err != nil && err != io.EOF {
				return fmt.Errorf("readdir %s: %w", relDir, err)
			}
			for _, de := range batch {
				name := de.Name()
				childAbs := filepath.Join(absDir, name)
				var childRel string
				if relDir == "" {
					childRel = name
				} else {
					childRel = relDir + "/" + name
				}
				// Normalize to slash form (name has no separators).
				childRel = path.Clean(childRel)

				// Lstat via Info on DirEntry uses Lstat semantics for the entry.
				info, err := de.Info()
				if err != nil {
					return fmt.Errorf("stat %s: %w", childRel, err)
				}
				mode := info.Mode()
				if mode&os.ModeSymlink != 0 {
					return fmt.Errorf("symlink rejected: %s", childRel)
				}
				if info.IsDir() {
					if err := walk(childAbs, childRel); err != nil {
						return err
					}
					continue
				}
				if !mode.IsRegular() {
					return fmt.Errorf("non-regular file rejected: %s (mode %v)", childRel, mode)
				}

				mt, ok := lookupMedia(childRel)
				if !ok {
					return fmt.Errorf("unknown extension for path %q", childRel)
				}
				sum, n, err := fileSHA256(childAbs)
				if err != nil {
					return fmt.Errorf("hash %s: %w", childRel, err)
				}
				if n != info.Size() {
					return fmt.Errorf("size race on %s: lstat=%d read=%d", childRel, info.Size(), n)
				}
				out = append(out, diskFile{
					rel:        childRel,
					size:       n,
					sha256:     sum,
					mediaType:  mt,
					cacheClass: assignCache(childRel, mt),
				})
			}
			if err == io.EOF {
				return nil
			}
		}
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	return out, nil
}

func lookupMedia(assetPath string) (string, bool) {
	ext := strings.ToLower(path.Ext(assetPath))
	mt, ok := contentTypesByExt[ext]
	return mt, ok
}

func assignCache(rel, mediaType string) string {
	switch rel {
	case requiredCanaries[0], requiredCanaries[1], requiredCanaries[2]:
		return "revalidate"
	}
	if strings.HasPrefix(mediaType, "text/html") {
		return "revalidate"
	}
	return "immutable"
}

func fileSHA256(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	written, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), written, nil
}

func marshalCanonical(doc catalogDoc) ([]byte, error) {
	// Defensive copies + path sort so encoding matches production encodeCatalog.
	cp := doc
	if doc.Canaries != nil {
		cp.Canaries = append([]string(nil), doc.Canaries...)
	}
	if doc.Entries != nil {
		cp.Entries = append([]fileEntry(nil), doc.Entries...)
		sort.SliceStable(cp.Entries, func(i, j int) bool {
			return cp.Entries[i].Path < cp.Entries[j].Path
		})
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func digestHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
