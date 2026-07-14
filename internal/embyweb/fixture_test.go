package embyweb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func defaultCatalogSHA() string {
	return sha256Hex([]byte("synthetic-catalog-v1"))
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

func buildFixture(t *testing.T, opts fixtureOpts) string {
	t.Helper()
	root := t.TempDir()

	release := opts.Release
	if release == "" {
		release = "1.0.0-deadbeef"
	}
	catalog := opts.CatalogSHA256
	if catalog == "" {
		catalog = defaultCatalogSHA()
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
		for _, f := range opts.Files {
			full := filepath.Join(filesDir, filepath.FromSlash(f.Path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("mkdir parent: %v", err)
			}
			if err := os.WriteFile(full, f.Data, 0o644); err != nil {
				t.Fatalf("write file: %v", err)
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

		if !opts.SkipInstall {
			if opts.InstallRaw != nil {
				if err := os.WriteFile(filepath.Join(relDir, "install.json"), opts.InstallRaw, 0o644); err != nil {
					t.Fatalf("write install raw: %v", err)
				}
			} else {
				install := map[string]any{
					"schema":         SchemaVersion,
					"release":        release,
					"catalog_sha256": catalog,
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
				"catalog_sha256": catalog,
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
	return root
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

func mustNewReady(t *testing.T, root string) *Server {
	t.Helper()
	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st := s.Status()
	if st.State != StateReady {
		t.Fatalf("want ready, got %s err=%v", st.State, st.Err)
	}
	return s
}
