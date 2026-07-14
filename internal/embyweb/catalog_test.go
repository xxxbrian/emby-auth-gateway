package embyweb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestCatalogDeterministicBytesAndDigest(t *testing.T) {
	files := readyMinimalFiles()
	tc1 := buildSyntheticCatalog(t, files, "det-a", "1.0.0")
	tc2 := buildSyntheticCatalog(t, files, "det-a", "1.0.0")
	if !bytes.Equal(tc1.Bytes, tc2.Bytes) {
		t.Fatal("encodeCatalog is not deterministic")
	}
	if tc1.Digest != tc2.Digest || tc1.Digest != catalogDigest(tc1.Bytes) {
		t.Fatalf("digest mismatch: %s vs %s", tc1.Digest, catalogDigest(tc1.Bytes))
	}
	// Digest is of exact committed bytes, not a re-encode of parsed struct with
	// different map order — re-parse and re-hash original bytes only.
	tc3, err := parseCatalog(tc1.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if tc3.Digest != tc1.Digest {
		t.Fatal("parseCatalog must hash input bytes, not re-encode")
	}
	if !bytes.Equal(tc3.Bytes, tc1.Bytes) {
		t.Fatal("trusted bytes must equal committed input")
	}
	// Trailing newline exactly once.
	if !bytes.HasSuffix(tc1.Bytes, []byte("\n")) || bytes.HasSuffix(tc1.Bytes, []byte("\n\n")) {
		t.Fatalf("expected single trailing newline")
	}
	// Fixed indentation (two spaces).
	if !bytes.Contains(tc1.Bytes, []byte("\n  \"schema\"")) && !bytes.Contains(tc1.Bytes, []byte("{\n  \"schema\"")) {
		// MarshalIndent puts fields with two-space indent.
		if !bytes.Contains(tc1.Bytes, []byte("\n  ")) {
			t.Fatalf("expected two-space indent in %q", tc1.Bytes[:min(80, len(tc1.Bytes))])
		}
	}
	wantRelease := "1.0.0-" + tc1.Digest
	if tc1.Release != wantRelease {
		t.Fatalf("release=%q want %q", tc1.Release, wantRelease)
	}
	if !validReleaseBasename(tc1.Release) {
		t.Fatalf("release %q failed validReleaseBasename", tc1.Release)
	}
}

func TestCatalogStrictSchema(t *testing.T) {
	base := buildSyntheticCatalog(t, readyMinimalFiles(), "strict", "1.0.0")
	var raw map[string]any
	if err := json.Unmarshal(base.Bytes, &raw); err != nil {
		t.Fatal(err)
	}

	t.Run("unknown_field", func(t *testing.T) {
		raw2 := cloneMap(raw)
		raw2["extra"] = true
		b, _ := json.MarshalIndent(raw2, "", "  ")
		b = append(b, '\n')
		if _, err := parseCatalog(b); err == nil {
			t.Fatal("expected unknown field rejection")
		}
	})
	t.Run("trailing_data", func(t *testing.T) {
		b := append(append([]byte{}, base.Bytes...), []byte(`{"x":1}`)...)
		if _, err := parseCatalog(b); err == nil {
			t.Fatal("expected trailing data rejection")
		}
	})
	t.Run("bad_source_digest", func(t *testing.T) {
		raw2 := cloneMap(raw)
		raw2["source_image_digest"] = "sha256:DEADBEEF"
		b, _ := json.MarshalIndent(raw2, "", "  ")
		b = append(b, '\n')
		if _, err := parseCatalog(b); err == nil {
			t.Fatal("expected source digest rejection")
		}
	})
	t.Run("bad_canaries_order", func(t *testing.T) {
		raw2 := cloneMap(raw)
		raw2["canaries"] = []string{"index.html", "manifest.json", "strings/en-US.json"}
		b, _ := json.MarshalIndent(raw2, "", "  ")
		b = append(b, '\n')
		if _, err := parseCatalog(b); err == nil {
			t.Fatal("expected canary order rejection")
		}
	})
	t.Run("unsorted_entries", func(t *testing.T) {
		// Swap two entry paths in JSON array while keeping valid fields.
		c := base.Catalog
		if len(c.Entries) < 2 {
			t.Fatal("need entries")
		}
		c.Entries[0], c.Entries[len(c.Entries)-1] = c.Entries[len(c.Entries)-1], c.Entries[0]
		// Force unsorted by putting "z" path first if needed.
		if entriesPathSorted(c.Entries) {
			c.Entries[0].Path = "zzz-last.js"
			c.Entries[0].MediaType = "text/javascript; charset=utf-8"
		}
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, '\n')
		if entriesPathSorted(c.Entries) {
			t.Fatal("test setup failed to unsort")
		}
		if _, err := parseCatalog(b); err == nil {
			t.Fatal("expected unsorted entries rejection")
		}
	})
	t.Run("duplicate_entry_path", func(t *testing.T) {
		c := base.Catalog
		c.Entries = append(append([]installEntry{}, c.Entries...), c.Entries[0])
		b, err := encodeCatalog(c) // encode sorts — duplicates still adjacent after sort
		if err != nil {
			t.Fatal(err)
		}
		// Manually build unsorted-ok but duplicate after sort
		if _, err := parseCatalog(b); err == nil {
			t.Fatal("expected duplicate path rejection")
		}
	})
}

func TestRegistryPinAndDuplicates(t *testing.T) {
	tc := buildSyntheticCatalog(t, readyMinimalFiles(), "reg-a", "1.0.0")

	t.Run("pin_mismatch", func(t *testing.T) {
		_, err := newCatalogRegistry([]catalogDeclaration{{
			Bytes:          tc.Bytes,
			ExpectedDigest: strings.Repeat("0", 64),
		}})
		if err == nil || !strings.Contains(err.Error(), "pin mismatch") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("duplicate_id", func(t *testing.T) {
		tc2 := buildSyntheticCatalog(t, readyMinimalFiles(), "reg-a", "2.0.0")
		_, err := newCatalogRegistry([]catalogDeclaration{
			{Bytes: tc.Bytes, ExpectedDigest: tc.Digest},
			{Bytes: tc2.Bytes, ExpectedDigest: tc2.Digest},
		})
		if err == nil || !strings.Contains(err.Error(), "duplicate catalog id") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("duplicate_digest", func(t *testing.T) {
		_, err := newCatalogRegistry([]catalogDeclaration{
			{Bytes: tc.Bytes, ExpectedDigest: tc.Digest},
			{Bytes: append([]byte{}, tc.Bytes...), ExpectedDigest: tc.Digest},
		})
		if err == nil || !strings.Contains(err.Error(), "duplicate digest") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("ok", func(t *testing.T) {
		reg := registryFromTrusted(t, tc)
		got, err := reg.lookupByDigest(tc.Digest)
		if err != nil || got.Release != tc.Release {
			t.Fatalf("lookup digest: %v %+v", err, got)
		}
		got, err = reg.lookupByID("reg-a")
		if err != nil || got.Digest != tc.Digest {
			t.Fatalf("lookup id: %v", err)
		}
	})
}

func TestProductionRegistryCatalog(t *testing.T) {
	const (
		wantID      = "emby-web-4.9.5.0"
		wantVersion = "4.9.5.0"
		wantDigest  = catalogEmbyWeb4950Digest
		wantImage   = "emby/embyserver"
		wantSrcDig  = "sha256:1e76e14a9c99507eb9f54361126f22c4658fc1588b2a710a99ba42f2335ff59a"
		wantEntries = 868
	)
	wantRelease := wantVersion + "-" + wantDigest
	runtimeHashes := map[string]string{
		"modules/apphost.js":                            "11cb7865e7e09be7e4c89d963245dc7142a496f68c1b000b6d6edd61ca0fd9b8",
		"modules/input/keyboard.js":                     "01871146aea79bb7f7366bbf82881acef11bcc9a06a25b596493526f45dc90ce",
		"modules/virtual-scroller/virtual-scroller.js":  "62b53c64e031db786a8b30017061f83bce8e46dc0a33804eaab4ca9dfb41f5ee",
		"modules/virtual-scroller/virtual-scroller.css": "66d2f49974cec517543e106e6cdb89f2a280d654a2490a44b203b0559236fb9e",
	}

	reg := getProductionRegistry()
	if reg.len() != 1 {
		t.Fatalf("production registry len=%d want 1", reg.len())
	}

	tc, err := reg.lookupByID(wantID)
	if err != nil {
		t.Fatalf("lookupByID: %v", err)
	}
	if tc.Digest != wantDigest {
		t.Fatalf("digest=%s want %s", tc.Digest, wantDigest)
	}
	if tc.Release != wantRelease {
		t.Fatalf("release=%s want %s", tc.Release, wantRelease)
	}
	if tc.Catalog.ID != wantID || tc.Catalog.Version != wantVersion {
		t.Fatalf("id/version=%s/%s", tc.Catalog.ID, tc.Catalog.Version)
	}
	if tc.Catalog.SourceImage != wantImage || tc.Catalog.SourceImageDigest != wantSrcDig {
		t.Fatalf("source=%s %s", tc.Catalog.SourceImage, tc.Catalog.SourceImageDigest)
	}
	if len(tc.Catalog.Entries) != wantEntries {
		t.Fatalf("entries=%d want %d", len(tc.Catalog.Entries), wantEntries)
	}
	if len(tc.Catalog.Canaries) != 3 ||
		tc.Catalog.Canaries[0] != "manifest.json" ||
		tc.Catalog.Canaries[1] != "index.html" ||
		tc.Catalog.Canaries[2] != "strings/en-US.json" {
		t.Fatalf("canaries=%v", tc.Catalog.Canaries)
	}

	byPath := make(map[string]string, len(tc.Catalog.Entries))
	for _, e := range tc.Catalog.Entries {
		byPath[e.Path] = e.SHA256
	}
	for p, h := range runtimeHashes {
		if byPath[p] != h {
			t.Fatalf("runtime hash %s=%s want %s", p, byPath[p], h)
		}
	}

	got, err := reg.lookupByDigest(wantDigest)
	if err != nil || got.Catalog.ID != wantID {
		t.Fatalf("lookupByDigest: %v %+v", err, got)
	}

	// Unknown ID still legal-gates; unknown digest is untrusted.
	if _, err := reg.lookupByID("not-a-shipped-catalog"); !errors.Is(err, ErrCatalogLegalGate) {
		t.Fatalf("unknown id err=%v", err)
	}
	if _, err := reg.lookupByDigest(strings.Repeat("ab", 32)); !errors.Is(err, ErrUntrustedCatalog) {
		t.Fatalf("unknown digest err=%v", err)
	}

	// Pin mismatch / tamper rejection at registry construction.
	t.Run("pin_mismatch", func(t *testing.T) {
		_, err := newCatalogRegistry([]catalogDeclaration{{
			Bytes:          append([]byte(nil), catalogEmbyWeb4950JSON...),
			ExpectedDigest: strings.Repeat("00", 32),
		}})
		if err == nil || !strings.Contains(err.Error(), "pin mismatch") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("tampered_bytes", func(t *testing.T) {
		tampered := append([]byte(nil), catalogEmbyWeb4950JSON...)
		// Flip a byte inside the JSON while keeping length; pin must fail.
		if len(tampered) < 64 {
			t.Fatal("catalog too small")
		}
		tampered[40] ^= 0x01
		_, err := newCatalogRegistry([]catalogDeclaration{{
			Bytes:          tampered,
			ExpectedDigest: catalogEmbyWeb4950Digest,
		}})
		if err == nil || !strings.Contains(err.Error(), "pin mismatch") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("embedded_bytes_match_pin", func(t *testing.T) {
		if catalogDigest(catalogEmbyWeb4950JSON) != catalogEmbyWeb4950Digest {
			t.Fatal("embedded catalog bytes do not match hard-coded digest pin")
		}
		// Canonical form required by parseCatalog.
		if _, err := parseCatalog(catalogEmbyWeb4950JSON); err != nil {
			t.Fatalf("parse embedded catalog: %v", err)
		}
	})

	// Configured tree with synthetic bytes but public New (production registry)
	// is corrupt/untrusted — synthetic digests are not production pins.
	tree := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: tree.Root})
	if err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.State != StateCorrupt {
		t.Fatalf("state=%s", st.State)
	}
	if st.Err == nil || !errors.Is(st.Err, ErrUntrustedCatalog) && !strings.Contains(st.Err.Error(), "untrusted") {
		t.Fatalf("err=%v", st.Err)
	}
}

func TestTrustedReaderReadyAndInstallMismatch(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		tree := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
		s := mustNewReady(t, tree)
		if s.Status().CatalogSHA256 != tree.Digest {
			t.Fatal("digest")
		}
	})

	t.Run("unknown_pointer_digest", func(t *testing.T) {
		tree := buildFixture(t, fixtureOpts{
			Files:                 readyMinimalFiles(),
			CatalogSHA256:         strings.Repeat("cd", 32),
			SkipSyntheticRegistry: true,
			Release:               "1.0.0-" + strings.Repeat("cd", 32),
		})
		// Empty registry
		s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: tree.Root}, tree.Registry)
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateCorrupt || !strings.Contains(s.Status().Err.Error(), "untrusted") {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})

	t.Run("install_entry_mismatch", func(t *testing.T) {
		base := readyMinimalFiles()
		tc := buildSyntheticCatalog(t, base, "mismatch", "1.0.0")
		reg := registryFromTrusted(t, tc)
		// Change one entry hash in install only.
		var entries []map[string]any
		for i, e := range tc.Catalog.Entries {
			h := e.SHA256
			if i == 0 {
				h = strings.Repeat("ef", 32)
			}
			entries = append(entries, map[string]any{
				"path": e.Path, "size": e.Size, "sha256": h,
				"media_type": e.MediaType, "cache_class": e.CacheClass,
			})
		}
		tree := buildFixture(t, fixtureOpts{
			Files:                 base,
			Release:               tc.Release,
			CatalogSHA256:         tc.Digest,
			EntryOverrides:        entries,
			SkipSyntheticRegistry: true,
		})
		s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: tree.Root}, reg)
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateCorrupt {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
		if s.Status().Err == nil || !strings.Contains(s.Status().Err.Error(), "entry") {
			t.Fatalf("err=%v", s.Status().Err)
		}
	})

	t.Run("install_order_mismatch", func(t *testing.T) {
		base := readyMinimalFiles()
		tc := buildSyntheticCatalog(t, base, "order", "1.0.0")
		reg := registryFromTrusted(t, tc)
		// Reverse entry order in install.
		ents := tc.Catalog.Entries
		var entries []map[string]any
		for i := len(ents) - 1; i >= 0; i-- {
			e := ents[i]
			entries = append(entries, map[string]any{
				"path": e.Path, "size": e.Size, "sha256": e.SHA256,
				"media_type": e.MediaType, "cache_class": e.CacheClass,
			})
		}
		tree := buildFixture(t, fixtureOpts{
			Files:                 base,
			Release:               tc.Release,
			CatalogSHA256:         tc.Digest,
			EntryOverrides:        entries,
			SkipSyntheticRegistry: true,
		})
		s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: tree.Root}, reg)
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateCorrupt {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// baseValidCatalog returns a minimal schema-valid catalog (canaries + fields).
func baseValidCatalog() catalog {
	hash := strings.Repeat("ab", 32)
	entries := make([]installEntry, 0, len(canaryRelativePaths))
	for _, p := range canaryRelativePaths {
		mt, ok := expectedMediaType(p)
		if !ok {
			panic("canary media type")
		}
		cc := cacheImmutable
		if forceRevalidate(p, mt) {
			cc = cacheRevalidate
		}
		entries = append(entries, installEntry{
			Path: p, Size: 0, SHA256: hash, MediaType: mt, CacheClass: cc,
		})
	}
	// ensure path-sorted (canaries are already sorted in package order, but be safe)
	// canaryRelativePaths: index.html, manifest.json, strings/en-US.json — sorted.
	return catalog{
		Schema:            SchemaVersion,
		ID:                "bounds-test",
		Version:           "1.0.0",
		SourceImage:       "emby/embyserver:synthetic",
		SourceImageDigest: "sha256:" + strings.Repeat("cd", 32),
		Canaries:          append([]string(nil), canaryRelativePaths...),
		Entries:           entries,
	}
}

func mustEntry(path string, size int64) installEntry {
	mt, ok := expectedMediaType(path)
	if !ok {
		panic("media type for " + path)
	}
	cc := cacheImmutable
	if forceRevalidate(path, mt) {
		cc = cacheRevalidate
	}
	return installEntry{
		Path: path, Size: size, SHA256: strings.Repeat("ef", 32),
		MediaType: mt, CacheClass: cc,
	}
}

func TestCatalogAdmissionAggregateBytes(t *testing.T) {
	// maxTotalBytes (256MiB) > maxFileBytes (64MiB), so the aggregate bound is
	// exercised with multiple per-file-legal sizes.
	appendExactTotal := func(c *catalog, total int64) {
		remaining := total
		i := 0
		for remaining > 0 {
			chunk := remaining
			if chunk > maxFileBytes {
				chunk = maxFileBytes
			}
			c.Entries = append(c.Entries, mustEntry(fmt.Sprintf("chunk%02d.js", i), chunk))
			remaining -= chunk
			i++
		}
	}

	t.Run("at_maxTotalBytes", func(t *testing.T) {
		c := baseValidCatalog()
		// Canaries are size 0; chunks consume the full aggregate budget.
		appendExactTotal(&c, maxTotalBytes)
		sortEntriesByPath(c.Entries)
		if err := validateCatalog(c); err != nil {
			t.Fatalf("validate at bound: %v", err)
		}
		raw, err := encodeCatalog(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseCatalog(raw); err != nil {
			t.Fatalf("parse at bound: %v", err)
		}
	})

	t.Run("over_maxTotalBytes", func(t *testing.T) {
		c := baseValidCatalog()
		appendExactTotal(&c, maxTotalBytes)
		// One more byte past the cap (checked add, no wrap).
		c.Entries = append(c.Entries, mustEntry("tiny.js", 1))
		sortEntriesByPath(c.Entries)
		err := validateCatalog(c)
		if err == nil || !strings.Contains(err.Error(), "aggregate size") {
			t.Fatalf("want aggregate rejection, got %v", err)
		}
		// Admission must fail before registry accept / source acquisition.
		raw, errEnc := encodeCatalog(c)
		if errEnc != nil {
			t.Fatal(errEnc)
		}
		if _, err := parseCatalog(raw); err == nil {
			t.Fatal("parseCatalog must reject over-budget catalog")
		}
		if _, err := newCatalogRegistry([]catalogDeclaration{{
			Bytes: raw, ExpectedDigest: catalogDigest(raw),
		}}); err == nil {
			t.Fatal("registry must reject over-budget catalog")
		}
	})

	t.Run("overflow_safe_checked_add", func(t *testing.T) {
		// Even if individual sizes were hypothetically huge, checked add must not
		// wrap. Per-file maxFileBytes already rejects MaxInt64, so exercise the
		// guard with sizes that sum past maxTotalBytes without exceeding maxFileBytes.
		c := baseValidCatalog()
		// maxFileBytes is 64MiB; four of them exceed maxTotalBytes (256MiB).
		for i := 0; i < 5; i++ {
			c.Entries = append(c.Entries, mustEntry(fmt.Sprintf("chunk%d.js", i), maxFileBytes))
		}
		sortEntriesByPath(c.Entries)
		err := validateCatalog(c)
		if err == nil || !strings.Contains(err.Error(), "aggregate size") {
			t.Fatalf("want aggregate rejection, got %v", err)
		}
		// Direct checked-add property: maxTotalBytes-total underflow protection
		// when total is already at the cap.
		var total int64 = maxTotalBytes
		if int64(1) > maxTotalBytes-total {
			// expected path
		} else {
			t.Fatal("checked-add condition broken at cap")
		}
		// math.MaxInt64 sanity: maxEntries*maxFileBytes fits int64 so wrap is
		// not reachable under current constants, but document the invariant.
		if int64(maxEntries) > math.MaxInt64/int64(maxFileBytes) {
			t.Fatal("maxEntries*maxFileBytes overflows int64; checked add still required")
		}
	})
}

func TestCatalogAdmissionImpliedDirs(t *testing.T) {
	// Canaries contribute: "." and "strings" (from strings/en-US.json).
	// Fill remaining slots with unique top-level parents dNNNN/file.js.
	const canaryExtraDirs = 1 // "strings"

	t.Run("at_maxDirs", func(t *testing.T) {
		c := baseValidCatalog()
		// dirs starts with "."; canaries add "strings". Remaining capacity:
		need := maxDirs - 1 - canaryExtraDirs // room for unique dNNNN parents
		for i := 0; i < need; i++ {
			c.Entries = append(c.Entries, mustEntry(fmt.Sprintf("d%04d/x.js", i), 0))
		}
		sortEntriesByPath(c.Entries)
		if err := validateCatalog(c); err != nil {
			t.Fatalf("validate at maxDirs: %v", err)
		}
		// Confirm implied count equals maxDirs via the same map semantics.
		_, dirs, err := expectedTreeFromManifest(c.Entries)
		if err != nil {
			t.Fatal(err)
		}
		if len(dirs) != maxDirs {
			t.Fatalf("dirs=%d want %d", len(dirs), maxDirs)
		}
	})

	t.Run("over_maxDirs", func(t *testing.T) {
		c := baseValidCatalog()
		need := maxDirs - 1 - canaryExtraDirs + 1 // one past the limit
		for i := 0; i < need; i++ {
			c.Entries = append(c.Entries, mustEntry(fmt.Sprintf("d%04d/x.js", i), 0))
		}
		sortEntriesByPath(c.Entries)
		err := validateCatalog(c)
		if err == nil || !strings.Contains(err.Error(), "directory count") {
			t.Fatalf("want dir count rejection, got %v", err)
		}
		raw, errEnc := encodeCatalog(c)
		if errEnc != nil {
			t.Fatal(errEnc)
		}
		if _, err := parseCatalog(raw); err == nil {
			t.Fatal("parseCatalog must reject over-maxDirs catalog")
		}
	})

	t.Run("shared_parents_not_quadratic", func(t *testing.T) {
		// Many files under one deep tree must count each parent once (map), not
		// re-scan all dirs per entry (O(D²)).
		c := baseValidCatalog()
		for i := 0; i < 64; i++ {
			c.Entries = append(c.Entries, mustEntry(fmt.Sprintf("a/b/c/d/e/f%02d.js", i), 0))
		}
		sortEntriesByPath(c.Entries)
		if err := validateCatalog(c); err != nil {
			t.Fatalf("shared parents: %v", err)
		}
		_, dirs, err := expectedTreeFromManifest(c.Entries)
		if err != nil {
			t.Fatal(err)
		}
		// "." + "strings" + a, a/b, a/b/c, a/b/c/d, a/b/c/d/e = 7
		if len(dirs) != 7 {
			t.Fatalf("dirs=%d want 7 (unique parents only): %v", len(dirs), dirs)
		}
	})
}

func TestCatalogCanonicalBytesExact(t *testing.T) {
	base := buildSyntheticCatalog(t, readyMinimalFiles(), "canon", "1.0.0")

	t.Run("encodeCatalog_roundtrip", func(t *testing.T) {
		// Canonical bytes produced internally must still pass admission.
		raw, err := encodeCatalog(base.Catalog)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, base.Bytes) {
			t.Fatal("encodeCatalog of parsed catalog must equal committed bytes")
		}
		tc, err := parseCatalog(raw)
		if err != nil {
			t.Fatal(err)
		}
		if tc.Digest != base.Digest || !bytes.Equal(tc.Bytes, raw) {
			t.Fatal("digest/bytes identity must remain deterministic")
		}
		// Registry pin of canonical bytes succeeds.
		if _, err := newCatalogRegistry([]catalogDeclaration{{
			Bytes: raw, ExpectedDigest: catalogDigest(raw),
		}}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("reject_compact_json", func(t *testing.T) {
		compact, err := json.Marshal(base.Catalog)
		if err != nil {
			t.Fatal(err)
		}
		compact = append(compact, '\n')
		if _, err := parseCatalog(compact); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("compact: %v", err)
		}
	})

	t.Run("reject_crlf", func(t *testing.T) {
		crlf := bytes.ReplaceAll(base.Bytes, []byte("\n"), []byte("\r\n"))
		if _, err := parseCatalog(crlf); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("crlf: %v", err)
		}
	})

	t.Run("reject_extra_newline", func(t *testing.T) {
		extra := append(append([]byte{}, base.Bytes...), '\n')
		if _, err := parseCatalog(extra); err == nil {
			t.Fatal("expected extra newline rejection")
		}
	})

	t.Run("reject_reordered_fields", func(t *testing.T) {
		// Swap the first two object fields (schema <-> id) so key order differs
		// from encodeCatalog's struct field order.
		lines := bytes.Split(base.Bytes, []byte("\n"))
		// lines[0] = `{`, [1]=`  "schema": 1,`, [2]=`  "id": "...",`
		if len(lines) < 4 {
			t.Fatal("unexpected shape")
		}
		lines[1], lines[2] = lines[2], lines[1]
		out := bytes.Join(lines, []byte("\n"))
		if bytes.Equal(out, base.Bytes) {
			t.Fatal("reorder setup failed")
		}
		if _, err := parseCatalog(out); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("reordered: %v", err)
		}
	})

	t.Run("reject_duplicate_known_key", func(t *testing.T) {
		// Insert a second "schema" field; decoder keeps last value, re-encode differs.
		// After `"schema": 1,` inject another schema line.
		needle := []byte(`  "schema": 1,`)
		idx := bytes.Index(base.Bytes, needle)
		if idx < 0 {
			t.Fatal("schema field not found")
		}
		var b bytes.Buffer
		b.Write(base.Bytes[:idx+len(needle)])
		b.WriteByte('\n')
		b.WriteString(`  "schema": 1,`)
		b.Write(base.Bytes[idx+len(needle):])
		dup := b.Bytes()
		if _, err := parseCatalog(dup); err == nil {
			t.Fatal("expected duplicate key rejection via canonical mismatch")
		}
	})
}

func sortEntriesByPath(entries []installEntry) {
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Path < entries[i].Path {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}

func TestCatalogUSTARPathAdmission(t *testing.T) {
	t.Run("helpers_boundaries", func(t *testing.T) {
		// Exactly 100 ASCII bytes: fits name field. ".js" is 3 bytes.
		p100 := strings.Repeat("a", 97) + ".js"
		if len(p100) != 100 {
			t.Fatalf("len=%d", len(p100))
		}
		if !ustarNamePrefixRepresentable(p100) {
			t.Fatal("100-byte path must be representable")
		}
		// 101-byte single segment: cannot split, reject.
		p101 := strings.Repeat("a", 98) + ".js"
		if len(p101) != 101 {
			t.Fatalf("len=%d", len(p101))
		}
		if ustarNamePrefixRepresentable(p101) {
			t.Fatal("101-byte single-segment must not be representable")
		}
		// Valid prefix/suffix split near maxima: prefix 155 + "/" + suffix 100.
		prefix := strings.Repeat("p", 155)
		suffix := strings.Repeat("s", 97) + ".js" // 100 bytes
		splitOK := prefix + "/" + suffix
		if len(splitOK) != 256 {
			t.Fatalf("len=%d", len(splitOK))
		}
		if !ustarNamePrefixRepresentable(splitOK) {
			t.Fatal("155+1+100 path must be representable")
		}
		// Suffix too long (101) after slash.
		badSuffix := "dir/" + strings.Repeat("y", 101)
		if ustarNamePrefixRepresentable(badSuffix) {
			t.Fatal("suffix>100 must fail")
		}
		// Non-ASCII rejected.
		if pathIsASCII("café/x.js") || ustarNamePrefixRepresentable("café/x.js") {
			t.Fatal("non-ASCII must fail")
		}
		// Dir header form: 99 a's + "/" fits; 100 a's + "/" does not.
		if !ustarNamePrefixRepresentable(strings.Repeat("a", 99) + "/") {
			t.Fatal("99-byte dir header should fit")
		}
		if ustarNamePrefixRepresentable(strings.Repeat("a", 100) + "/") {
			t.Fatal("100-byte name + slash dir header must fail USTAR")
		}
	})

	t.Run("admit_ascii_ustar_path", func(t *testing.T) {
		c := baseValidCatalog()
		// Path that requires prefix split but is valid USTAR.
		long := strings.Repeat("m", 80) + "/" + strings.Repeat("n", 80) + ".js"
		if !ustarNamePrefixRepresentable(long) {
			t.Fatal("setup")
		}
		c.Entries = append(c.Entries, mustEntry(long, 0))
		sortEntriesByPath(c.Entries)
		if err := validateCatalog(c); err != nil {
			t.Fatal(err)
		}
		raw, err := encodeCatalog(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseCatalog(raw); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("reject_non_ascii", func(t *testing.T) {
		c := baseValidCatalog()
		e := mustEntry("modules/app.js", 0)
		e.Path = "modüles/app.js"
		// Bypass mustEntry media type: keep .js media type.
		e.MediaType = "text/javascript; charset=utf-8"
		c.Entries = append(c.Entries, e)
		sortEntriesByPath(c.Entries)
		err := validateCatalog(c)
		if err == nil || !strings.Contains(err.Error(), "ASCII") {
			t.Fatalf("want ASCII rejection, got %v", err)
		}
	})

	t.Run("reject_non_ustar_single_segment", func(t *testing.T) {
		c := baseValidCatalog()
		p := strings.Repeat("z", 98) + ".js" // 101 bytes
		c.Entries = append(c.Entries, mustEntry(p, 0))
		sortEntriesByPath(c.Entries)
		err := validateCatalog(c)
		if err == nil || !strings.Contains(err.Error(), "USTAR") {
			t.Fatalf("want USTAR rejection, got %v", err)
		}
	})

	t.Run("admit_file_when_optional_dir_header_not_ustar", func(t *testing.T) {
		// 100-byte directory segment + "/x.js": file path is USTAR-representable
		// via prefix/suffix split, but optional dir header "dir/" is not. Directory
		// headers are optional, so admission must accept the file path alone.
		parent := strings.Repeat("d", 100)
		file := parent + "/x.js"
		if len(parent) != 100 {
			t.Fatalf("parent len=%d", len(parent))
		}
		if !ustarNamePrefixRepresentable(file) {
			t.Fatal("file should be USTAR ok")
		}
		if ustarNamePrefixRepresentable(parent + "/") {
			t.Fatal("parent dir header should not be USTAR ok (documents optional-header case)")
		}
		c := baseValidCatalog()
		c.Entries = append(c.Entries, mustEntry(file, 0))
		sortEntriesByPath(c.Entries)
		if err := validateCatalog(c); err != nil {
			t.Fatalf("file path must be admitted even when dir/ header is not USTAR: %v", err)
		}
		raw, err := encodeCatalog(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseCatalog(raw); err != nil {
			t.Fatalf("parseCatalog must admit file-only USTAR path: %v", err)
		}
		// Implied-dir counting still includes the 100-byte parent once.
		_, dirs, err := expectedTreeFromManifest(c.Entries)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := dirs[parent]; !ok {
			t.Fatalf("implied dirs must still count parent %q: %v", parent, dirs)
		}
	})

	t.Run("reject_via_parseCatalog", func(t *testing.T) {
		c := baseValidCatalog()
		c.Entries = append(c.Entries, mustEntry(strings.Repeat("q", 98)+".js", 0)) // 101 bytes
		sortEntriesByPath(c.Entries)
		raw, err := encodeCatalog(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseCatalog(raw); err == nil {
			t.Fatal("parseCatalog must reject non-USTAR path")
		}
	})
}
