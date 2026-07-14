package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func minimalFiles() map[string][]byte {
	return map[string][]byte{
		"manifest.json":      []byte(`{"name":"t"}`),
		"index.html":         []byte("<html></html>"),
		"strings/en-US.json": []byte(`{"ok":true}`),
		"modules/app.js":     []byte("console.log(1)"),
		"css/site.css":       []byte("body{}"),
		"docs/readme.md":     []byte("# hi"),
		"pages/other.html":   []byte("<html>x</html>"),
	}
}

func TestGenerateHappyPath(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, minimalFiles())
	out := filepath.Join(t.TempDir(), "catalog.json")

	if err := run(root, "synthetic-id", "1.0.0", "emby/embyserver:synthetic",
		"sha256:"+strings.Repeat("ab", 32), out); err != nil {
		t.Fatalf("run: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("catalog must end with single trailing newline")
	}
	// Canonical: re-encode must match exactly.
	var cat catalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	if cat.Schema != 1 {
		t.Fatalf("schema=%d", cat.Schema)
	}
	if len(cat.Canaries) != 3 || cat.Canaries[0] != "manifest.json" || cat.Canaries[1] != "index.html" || cat.Canaries[2] != "strings/en-US.json" {
		t.Fatalf("canaries=%v", cat.Canaries)
	}
	// Entries path-sorted unique.
	for i := 1; i < len(cat.Entries); i++ {
		if cat.Entries[i-1].Path >= cat.Entries[i].Path {
			t.Fatalf("entries not sorted: %q >= %q", cat.Entries[i-1].Path, cat.Entries[i].Path)
		}
	}
	// Cache classes: canaries + html revalidate; others immutable.
	byPath := map[string]entry{}
	for _, e := range cat.Entries {
		byPath[e.Path] = e
	}
	for _, c := range canaries {
		if byPath[c].CacheClass != "revalidate" {
			t.Fatalf("canary %s cache=%s", c, byPath[c].CacheClass)
		}
	}
	if byPath["pages/other.html"].CacheClass != "revalidate" {
		t.Fatalf("html cache=%s", byPath["pages/other.html"].CacheClass)
	}
	if byPath["modules/app.js"].CacheClass != "immutable" {
		t.Fatalf("js cache=%s", byPath["modules/app.js"].CacheClass)
	}
	if byPath["modules/app.js"].MediaType != "text/javascript; charset=utf-8" {
		t.Fatalf("js media=%s", byPath["modules/app.js"].MediaType)
	}
	if byPath["docs/readme.md"].MediaType != "text/markdown; charset=utf-8" {
		t.Fatalf("md media=%s", byPath["docs/readme.md"].MediaType)
	}
	// Size/hash match file bytes.
	js := []byte("console.log(1)")
	sum := sha256.Sum256(js)
	if byPath["modules/app.js"].Size != int64(len(js)) || byPath["modules/app.js"].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("js size/hash mismatch: %+v", byPath["modules/app.js"])
	}
	// Exact canonical form.
	canonical, err := encodeCatalog(cat)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, canonical) {
		t.Fatalf("output is not canonical encodeCatalog form")
	}
}

func TestGenerateRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, minimalFiles())
	target := filepath.Join(root, "modules", "app.js")
	link := filepath.Join(root, "modules", "link.js")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	out := filepath.Join(t.TempDir(), "c.json")
	err := run(root, "id", "1.0.0", "img", "sha256:"+strings.Repeat("cd", 32), out)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestGenerateRejectsUnknownExtension(t *testing.T) {
	root := t.TempDir()
	files := minimalFiles()
	files["bin/tool.exe"] = []byte("MZ")
	writeTree(t, root, files)
	out := filepath.Join(t.TempDir(), "c.json")
	err := run(root, "id", "1.0.0", "img", "sha256:"+strings.Repeat("ef", 32), out)
	if err == nil || !strings.Contains(err.Error(), "unknown extension") {
		t.Fatalf("expected unknown extension rejection, got %v", err)
	}
}

func TestGenerateRequiresCanaries(t *testing.T) {
	root := t.TempDir()
	files := minimalFiles()
	delete(files, "index.html")
	writeTree(t, root, files)
	out := filepath.Join(t.TempDir(), "c.json")
	err := run(root, "id", "1.0.0", "img", "sha256:"+strings.Repeat("11", 32), out)
	if err == nil || !strings.Contains(err.Error(), "canary") {
		t.Fatalf("expected canary rejection, got %v", err)
	}
}
