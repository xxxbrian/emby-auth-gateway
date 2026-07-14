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

const (
	testID          = "syn-verify"
	testVersion     = "9.9.9"
	testSourceImage = "emby/embyserver:syn"
)

func testSourceImageDigest() string {
	return "sha256:" + strings.Repeat("aa", 32)
}

func writeFiles(t *testing.T, root string, files map[string][]byte) {
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

func baseTree() map[string][]byte {
	return map[string][]byte{
		"manifest.json":      []byte(`{"n":1}`),
		"index.html":         []byte("<html>i</html>"),
		"strings/en-US.json": []byte(`{"a":1}`),
		"modules/app.js":     []byte("var x=1"),
		"css/site.css":       []byte("a{}"),
	}
}

// buildCatalogFromTree invents a correct catalog for the tree using this
// package's own scan/encode path (not generate).
func buildCatalogFromTree(t *testing.T, root string) []byte {
	t.Helper()
	found, err := scanTree(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	doc := catalogDoc{
		Schema:            catalogSchema,
		ID:                testID,
		Version:           testVersion,
		SourceImage:       testSourceImage,
		SourceImageDigest: testSourceImageDigest(),
		Canaries:          append([]string(nil), requiredCanaries[:]...),
		Entries:           make([]fileEntry, len(found)),
	}
	for i, f := range found {
		doc.Entries[i] = fileEntry{
			Path:       f.rel,
			Size:       f.size,
			SHA256:     f.sha256,
			MediaType:  f.mediaType,
			CacheClass: f.cacheClass,
		}
	}
	raw, err := marshalCanonical(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func expectFor(raw []byte) expectIdentity {
	return expectIdentity{
		ID:                testID,
		Version:           testVersion,
		SourceImage:       testSourceImage,
		SourceImageDigest: testSourceImageDigest(),
		Digest:            digestHex(raw),
	}
}

func writeCatalog(t *testing.T, raw []byte) (treeRoot, catPath string, exp expectIdentity) {
	t.Helper()
	treeRoot = t.TempDir()
	writeFiles(t, treeRoot, baseTree())
	// Rebuild catalog from the actual tree so entries match.
	raw = buildCatalogFromTree(t, treeRoot)
	catPath = filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return treeRoot, catPath, expectFor(raw)
}

func TestVerifyHappyPath(t *testing.T) {
	root, catPath, exp := writeCatalog(t, nil)
	if err := verify(root, catPath, exp); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Digest of exact bytes is stable.
	raw, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != exp.Digest {
		t.Fatalf("digest drift")
	}
}

func TestVerifyRejectsMissingExpectFlags(t *testing.T) {
	root, catPath, exp := writeCatalog(t, nil)
	cases := []struct {
		name string
		mut  func(*expectIdentity)
		want string
	}{
		{"id", func(e *expectIdentity) { e.ID = "" }, "--expect-id"},
		{"version", func(e *expectIdentity) { e.Version = "" }, "--expect-version"},
		{"source_image", func(e *expectIdentity) { e.SourceImage = "" }, "--expect-source-image"},
		{"source_image_digest", func(e *expectIdentity) { e.SourceImageDigest = "" }, "--expect-source-image-digest"},
		{"digest", func(e *expectIdentity) { e.Digest = "" }, "--expect-digest"},
		{"digest_format", func(e *expectIdentity) { e.Digest = "not-hex" }, "--expect-digest must be lowercase 64-hex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := exp
			tc.mut(&e)
			err := verify(root, catPath, e)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want contain %q", err, tc.want)
			}
		})
	}
}

func TestVerifyIdentityFieldMismatch(t *testing.T) {
	root, catPath, exp := writeCatalog(t, nil)
	// Mutate each expected identity field while keeping digest pin of the real
	// file: catalog fields must fail against the wrong expectation.
	cases := []struct {
		name string
		mut  func(*expectIdentity)
		want string
	}{
		{"id", func(e *expectIdentity) { e.ID = "wrong-id" }, "catalog id mismatch"},
		{"version", func(e *expectIdentity) { e.Version = "0.0.0" }, "catalog version mismatch"},
		{"source_image", func(e *expectIdentity) { e.SourceImage = "other/image" }, "catalog source_image mismatch"},
		{"source_image_digest", func(e *expectIdentity) {
			e.SourceImageDigest = "sha256:" + strings.Repeat("bb", 32)
		}, "catalog source_image_digest mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := exp
			tc.mut(&e)
			err := verify(root, catPath, e)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want contain %q", err, tc.want)
			}
		})
	}
}

func TestVerifyDigestMismatch(t *testing.T) {
	root, catPath, exp := writeCatalog(t, nil)
	exp.Digest = strings.Repeat("ff", 32)
	err := verify(root, catPath, exp)
	if err == nil || !strings.Contains(err.Error(), "catalog digest mismatch") {
		t.Fatalf("err=%v", err)
	}
}

func TestVerifyCatalogIdentityMutationFailsAgainstExpect(t *testing.T) {
	// Catalog with wrong identity but matching tree entries must fail when
	// operator expects the correct identity (P1: do not trust catalog self-ID).
	root := t.TempDir()
	writeFiles(t, root, baseTree())
	found, err := scanTree(root)
	if err != nil {
		t.Fatal(err)
	}
	doc := catalogDoc{
		Schema:            catalogSchema,
		ID:                "attacker-id",
		Version:           "0.0.1",
		SourceImage:       "evil/image",
		SourceImageDigest: "sha256:" + strings.Repeat("cc", 32),
		Canaries:          append([]string(nil), requiredCanaries[:]...),
		Entries:           make([]fileEntry, len(found)),
	}
	for i, f := range found {
		doc.Entries[i] = fileEntry{
			Path: f.rel, Size: f.size, SHA256: f.sha256,
			MediaType: f.mediaType, CacheClass: f.cacheClass,
		}
	}
	raw, err := marshalCanonical(doc)
	if err != nil {
		t.Fatal(err)
	}
	catPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// Operator pins the legitimate identity + the attacker's file digest would
	// still fail field checks; pin legitimate digest of a correct catalog so
	// digest check fails first, and also pin attacker's digest with legitimate
	// identity so field checks fire.
	t.Run("wrong_digest_pin", func(t *testing.T) {
		// Build correct catalog digest for comparison pin.
		good := buildCatalogFromTree(t, root)
		err := verify(root, catPath, expectIdentity{
			ID:                testID,
			Version:           testVersion,
			SourceImage:       testSourceImage,
			SourceImageDigest: testSourceImageDigest(),
			Digest:            digestHex(good),
		})
		if err == nil || !strings.Contains(err.Error(), "catalog digest mismatch") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("attacker_digest_legitimate_identity", func(t *testing.T) {
		err := verify(root, catPath, expectIdentity{
			ID:                testID,
			Version:           testVersion,
			SourceImage:       testSourceImage,
			SourceImageDigest: testSourceImageDigest(),
			Digest:            digestHex(raw), // matches attacker file bytes
		})
		if err == nil {
			t.Fatal("expected identity field mismatch")
		}
		if !strings.Contains(err.Error(), "catalog id mismatch") &&
			!strings.Contains(err.Error(), "version mismatch") &&
			!strings.Contains(err.Error(), "source_image") {
			t.Fatalf("err=%v want identity mismatch", err)
		}
	})
}

func TestVerifyRejectsSymlink(t *testing.T) {
	root, catPath, exp := writeCatalog(t, nil)
	if err := os.Symlink(filepath.Join(root, "modules", "app.js"), filepath.Join(root, "modules", "x.js")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	err := verify(root, catPath, exp)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestVerifyExtraFile(t *testing.T) {
	root, catPath, exp := writeCatalog(t, nil)
	if err := os.WriteFile(filepath.Join(root, "extra.js"), []byte("extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verify(root, catPath, exp)
	if err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("expected count mismatch, got %v", err)
	}
}

func TestVerifyMissingFile(t *testing.T) {
	root, catPath, exp := writeCatalog(t, nil)
	if err := os.Remove(filepath.Join(root, "modules", "app.js")); err != nil {
		t.Fatal(err)
	}
	err := verify(root, catPath, exp)
	if err == nil {
		t.Fatal("expected missing-file failure")
	}
	if !strings.Contains(err.Error(), "count") && !strings.Contains(err.Error(), "path") && !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyHashMismatch(t *testing.T) {
	root, catPath, exp := writeCatalog(t, nil)
	if err := os.WriteFile(filepath.Join(root, "modules", "app.js"), []byte("var x=2"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verify(root, catPath, exp)
	if err == nil || (!strings.Contains(err.Error(), "sha256") && !strings.Contains(err.Error(), "differ") && !strings.Contains(err.Error(), "size")) {
		t.Fatalf("expected hash/size mismatch, got %v", err)
	}
}

func TestVerifyNonCanonicalCatalog(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, baseTree())
	raw := buildCatalogFromTree(t, root)
	var doc catalogDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(compact, raw) {
		t.Fatal("compact unexpectedly equal to canonical")
	}
	catPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catPath, compact, 0o644); err != nil {
		t.Fatal(err)
	}
	// Pin digest of the non-canonical file so we reach the canonical check.
	err = verify(root, catPath, expectIdentity{
		ID:                testID,
		Version:           testVersion,
		SourceImage:       testSourceImage,
		SourceImageDigest: testSourceImageDigest(),
		Digest:            digestHex(compact),
	})
	if err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("expected non-canonical rejection, got %v", err)
	}
}

func TestVerifyUnknownExtension(t *testing.T) {
	root := t.TempDir()
	files := baseTree()
	files["weird.xyz"] = []byte("nope")
	writeFiles(t, root, files)
	_, err := scanTree(root)
	if err == nil || !strings.Contains(err.Error(), "unknown extension") {
		t.Fatalf("expected unknown extension on scan, got %v", err)
	}
	clean := t.TempDir()
	writeFiles(t, clean, baseTree())
	raw := buildCatalogFromTree(t, clean)
	catPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	err = verify(root, catPath, expectFor(raw))
	if err == nil || !strings.Contains(err.Error(), "unknown extension") {
		t.Fatalf("expected unknown extension rejection, got %v", err)
	}
}

func TestVerifyRejectsWrongCanaryOrder(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, baseTree())
	raw := buildCatalogFromTree(t, root)
	var doc catalogDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc.Canaries = []string{"index.html", "manifest.json", "strings/en-US.json"}
	bad, err := marshalCanonical(doc)
	if err != nil {
		t.Fatal(err)
	}
	catPath := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	err = verify(root, catPath, expectIdentity{
		ID:                testID,
		Version:           testVersion,
		SourceImage:       testSourceImage,
		SourceImageDigest: testSourceImageDigest(),
		Digest:            digestHex(bad),
	})
	if err == nil || !strings.Contains(err.Error(), "canaries") {
		t.Fatalf("expected canary order rejection, got %v", err)
	}
}
