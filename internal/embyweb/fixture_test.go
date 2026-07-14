package embyweb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type fixtureFile struct {
	Path       string
	Data       []byte
	MediaType  string
	CacheClass string
}

type fixtureOpts struct {
	Release       string
	CatalogSHA256 string
	Files         []fixtureFile
	// Optional overrides for negative tests.
	SkipCurrent     bool
	SkipInstall     bool
	SkipFilesDir    bool
	SkipReleaseDir  bool
	CurrentRaw      []byte
	InstallRaw      []byte
	MutateAfter     func(root string)
	ExtraDiskFiles  map[string][]byte // relative to files/
	ExtraDiskDirs   []string          // relative to files/
	CurrentOverride map[string]any
	InstallOverride map[string]any
	EntryOverrides  []map[string]any // if set, replace entries entirely
	// SkipSyntheticRegistry disables auto registry for negative untrusted tests.
	SkipSyntheticRegistry bool
	// CatalogID overrides the synthetic catalog id (default "synthetic-test").
	CatalogID string
	// CatalogVersion overrides the synthetic catalog version (default "1.0.0").
	CatalogVersion string
}

type fixtureTree struct {
	Root     string
	Registry *catalogRegistry
	Digest   string
	Release  string
	Catalog  *trustedCatalog
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// buildSyntheticCatalog constructs a schema-1 catalog from fixture files, encodes
// deterministic bytes, and returns a one-entry registry plus trusted pin.
func buildSyntheticCatalog(t *testing.T, files []fixtureFile, id, version string) *trustedCatalog {
	t.Helper()
	if id == "" {
		id = "synthetic-test"
	}
	if version == "" {
		version = "1.0.0"
	}
	entries := make([]installEntry, 0, len(files))
	for _, f := range files {
		mt := f.MediaType
		if mt == "" {
			var ok bool
			mt, ok = expectedMediaType(f.Path)
			if !ok {
				t.Fatalf("no media type for %q", f.Path)
			}
		}
		cc := f.CacheClass
		if cc == "" {
			cc = cacheImmutable
			if forceRevalidate(f.Path, mt) {
				cc = cacheRevalidate
			}
		}
		entries = append(entries, installEntry{
			Path:       f.Path,
			Size:       int64(len(f.Data)),
			SHA256:     sha256Hex(f.Data),
			MediaType:  mt,
			CacheClass: cc,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	c := catalog{
		Schema:            SchemaVersion,
		ID:                id,
		Version:           version,
		SourceImage:       "emby/embyserver:synthetic",
		SourceImageDigest: "sha256:" + sha256Hex([]byte("synthetic-source-image")),
		Canaries:          append([]string(nil), canaryRelativePaths...),
		Entries:           entries,
	}
	raw, err := encodeCatalog(c)
	if err != nil {
		t.Fatalf("encode catalog: %v", err)
	}
	tc, err := parseCatalog(raw)
	if err != nil {
		t.Fatalf("parse catalog: %v", err)
	}
	return tc
}

func registryFromTrusted(t *testing.T, tc *trustedCatalog) *catalogRegistry {
	t.Helper()
	reg, err := newCatalogRegistry([]catalogDeclaration{{
		Bytes:          tc.Bytes,
		ExpectedDigest: tc.Digest,
	}})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

func buildFixture(t *testing.T, opts fixtureOpts) fixtureTree {
	t.Helper()
	root := t.TempDir()

	var tc *trustedCatalog
	var reg *catalogRegistry
	release := opts.Release
	catalogDigest := opts.CatalogSHA256

	// Build a synthetic trusted catalog from files when possible so Ready trees
	// can inject a package-private registry matching pointer/install digests.
	// Also used when Files is nil but we still need a trusted pin for missing-path
	// tests (canaries-only catalog); callers may set Files to readyMinimalFiles
	// and SkipFilesDir instead.
	if !opts.SkipSyntheticRegistry && opts.EntryOverrides == nil && opts.InstallRaw == nil {
		filesForCatalog := opts.Files
		if filesForCatalog == nil {
			// No auto-catalog for completely empty file lists unless canaries-only
			// is requested via CatalogID sentinel — leave untrusted.
		} else if fixtureHasAllCanaries(filesForCatalog) {
			tc = buildSyntheticCatalog(t, filesForCatalog, opts.CatalogID, opts.CatalogVersion)
			reg = registryFromTrusted(t, tc)
			if release == "" {
				release = tc.Release
			}
			if catalogDigest == "" {
				catalogDigest = tc.Digest
			}
		}
	}

	if release == "" {
		release = "1.0.0-deadbeef"
	}
	if catalogDigest == "" {
		catalogDigest = sha256Hex([]byte("untrusted-placeholder"))
	}
	if reg == nil {
		// Empty registry for untrusted/negative cases.
		var err error
		reg, err = newCatalogRegistry(nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	if !opts.SkipReleaseDir {
		relDir := filepath.Join(root, "releases", release)
		if err := os.MkdirAll(relDir, 0o755); err != nil {
			t.Fatalf("mkdir release: %v", err)
		}

		filesDir := filepath.Join(relDir, "files")
		if !opts.SkipFilesDir {
			if err := os.MkdirAll(filesDir, 0o755); err != nil {
				t.Fatalf("mkdir files: %v", err)
			}
		}

		entries := make([]map[string]any, 0, len(opts.Files))
		// Prefer trusted catalog entry order when available so install matches.
		if tc != nil && opts.EntryOverrides == nil {
			for _, e := range tc.Catalog.Entries {
				var data []byte
				for _, f := range opts.Files {
					if f.Path == e.Path {
						data = f.Data
						break
					}
				}
				if data == nil {
					t.Fatalf("missing fixture data for catalog path %q", e.Path)
				}
				if !opts.SkipFilesDir {
					full := filepath.Join(filesDir, filepath.FromSlash(e.Path))
					if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
						t.Fatalf("mkdir parent: %v", err)
					}
					if err := os.WriteFile(full, data, 0o644); err != nil {
						t.Fatalf("write file: %v", err)
					}
				}
				entries = append(entries, map[string]any{
					"path":        e.Path,
					"size":        e.Size,
					"sha256":      e.SHA256,
					"media_type":  e.MediaType,
					"cache_class": e.CacheClass,
				})
			}
		} else {
			for _, f := range opts.Files {
				if !opts.SkipFilesDir {
					full := filepath.Join(filesDir, filepath.FromSlash(f.Path))
					if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
						t.Fatalf("mkdir parent: %v", err)
					}
					if err := os.WriteFile(full, f.Data, 0o644); err != nil {
						t.Fatalf("write file: %v", err)
					}
				}
				mt := f.MediaType
				if mt == "" {
					var ok bool
					mt, ok = expectedMediaType(f.Path)
					if !ok {
						t.Fatalf("no media type for %q", f.Path)
					}
				}
				cc := f.CacheClass
				if cc == "" {
					cc = cacheImmutable
					if forceRevalidate(f.Path, mt) {
						cc = cacheRevalidate
					}
				}
				entries = append(entries, map[string]any{
					"path":        f.Path,
					"size":        len(f.Data),
					"sha256":      sha256Hex(f.Data),
					"media_type":  mt,
					"cache_class": cc,
				})
			}
		}

		if !opts.SkipFilesDir {
			for p, data := range opts.ExtraDiskFiles {
				full := filepath.Join(filesDir, filepath.FromSlash(p))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatalf("mkdir extra: %v", err)
				}
				if err := os.WriteFile(full, data, 0o644); err != nil {
					t.Fatalf("write extra: %v", err)
				}
			}
			for _, d := range opts.ExtraDiskDirs {
				if err := os.MkdirAll(filepath.Join(filesDir, filepath.FromSlash(d)), 0o755); err != nil {
					t.Fatalf("mkdir extra dir: %v", err)
				}
			}
		}

		if !opts.SkipInstall {
			if opts.InstallRaw != nil {
				if err := os.WriteFile(filepath.Join(relDir, "install.json"), opts.InstallRaw, 0o644); err != nil {
					t.Fatalf("write install raw: %v", err)
				}
			} else {
				install := map[string]any{
					"schema":         SchemaVersion,
					"release":        release,
					"catalog_sha256": catalogDigest,
					"entries":        entries,
				}
				if opts.EntryOverrides != nil {
					install["entries"] = opts.EntryOverrides
				}
				for k, v := range opts.InstallOverride {
					install[k] = v
				}
				writeJSON(t, filepath.Join(relDir, "install.json"), install)
			}
		}
	}

	if !opts.SkipCurrent {
		if opts.CurrentRaw != nil {
			if err := os.WriteFile(filepath.Join(root, "current.json"), opts.CurrentRaw, 0o644); err != nil {
				t.Fatalf("write current raw: %v", err)
			}
		} else {
			cur := map[string]any{
				"schema":         SchemaVersion,
				"release":        release,
				"catalog_sha256": catalogDigest,
			}
			for k, v := range opts.CurrentOverride {
				cur[k] = v
			}
			writeJSON(t, filepath.Join(root, "current.json"), cur)
		}
	}

	if opts.MutateAfter != nil {
		opts.MutateAfter(root)
	}
	return fixtureTree{
		Root:     root,
		Registry: reg,
		Digest:   catalogDigest,
		Release:  release,
		Catalog:  tc,
	}
}

func fixtureHasAllCanaries(files []fixtureFile) bool {
	have := map[string]bool{}
	for _, f := range files {
		have[f.Path] = true
	}
	for _, c := range canaryRelativePaths {
		if !have[c] {
			return false
		}
	}
	return true
}

func readyMinimalFiles() []fixtureFile {
	return []fixtureFile{
		{Path: "index.html", Data: []byte("<!doctype html><title>t</title>")},
		{Path: "manifest.json", Data: []byte(`{"name":"test"}`)},
		{Path: "strings/en-US.json", Data: []byte(`{"Hello":"Hello"}`)},
		{Path: "modules/app.js", Data: []byte("console.log(1)"), CacheClass: cacheImmutable},
		{Path: "css/site.css", Data: []byte("body{}"), CacheClass: cacheImmutable},
		{Path: "img/logo.png", Data: []byte("\x89PNG\r\n\x1a\n"), CacheClass: cacheImmutable},
	}
}

func mustNewReady(t *testing.T, tree fixtureTree) *Server {
	t.Helper()
	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: tree.Root}, tree.Registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st := s.Status()
	if st.State != StateReady {
		t.Fatalf("want ready, got %s err=%v", st.State, st.Err)
	}
	return s
}

func mustNewWithTree(t *testing.T, tree fixtureTree) *Server {
	t.Helper()
	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: tree.Root}, tree.Registry)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// releaseNameFromRoot returns the single release directory basename under releases/.
func releaseNameFromRoot(root string) string {
	entries, err := os.ReadDir(filepath.Join(root, "releases"))
	if err != nil || len(entries) == 0 {
		return "1.0.0-deadbeef"
	}
	for _, e := range entries {
		if e.IsDir() {
			return e.Name()
		}
	}
	return "1.0.0-deadbeef"
}
