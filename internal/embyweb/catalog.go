package embyweb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Catalog schema bounds. Catalog JSON is capped at maxCatalogBytes (same order
// as install manifests) so entry counts remain governed by maxEntries.
const (
	maxCatalogBytes        = 4 << 20 // 4 MiB; consistent with maxManifestBytes
	maxCatalogIDBytes      = 64
	maxCatalogVersionBytes = 64
	maxSourceImageBytes    = 256
)

// ErrUntrustedCatalog is returned when a pointer/install catalog_sha256 is not
// present in the active immutable registry (including the empty production
// registry). Readers map this to StateCorrupt.
var ErrUntrustedCatalog = errors.New("embyweb: untrusted catalog digest")

// ErrCatalogLegalGate is the sentinel for the empty production registry when a
// catalog ID is requested before legal/reproduction approval. Lane 1 exposes it
// for registry lookup-by-ID; the reader uses ErrUntrustedCatalog for digests.
var ErrCatalogLegalGate = errors.New("embyweb: catalog not available (legal/reproduction gate)")

// catalog is the package-private schema-1 trusted catalog document.
// JSON fields are exactly: schema, id, version, source_image, source_image_digest,
// canaries, entries.
type catalog struct {
	Schema            int            `json:"schema"`
	ID                string         `json:"id"`
	Version           string         `json:"version"`
	SourceImage       string         `json:"source_image"`
	SourceImageDigest string         `json:"source_image_digest"`
	Canaries          []string       `json:"canaries"`
	Entries           []installEntry `json:"entries"`
}

// trustedCatalog is a fully verified catalog pin: exact committed bytes, digest
// of those bytes, derived release basename, and parsed fields.
type trustedCatalog struct {
	Catalog catalog
	Bytes   []byte // exact committed catalog JSON
	Digest  string // lowercase 64-hex SHA-256 of Bytes
	Release string // <version>-<Digest>
}

// encodeCatalog produces deterministic committed catalog bytes: struct-based
// JSON with two-space indent and exactly one trailing newline. Entries are
// sorted by path on a defensive copy before encoding; the parser still requires
// the committed bytes to already present entries in sorted order.
func encodeCatalog(c catalog) ([]byte, error) {
	out := c
	if out.Entries != nil {
		out.Entries = append([]installEntry(nil), c.Entries...)
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
	data = append(data, '\n')
	if len(data) > maxCatalogBytes {
		return nil, fmt.Errorf("catalog encoding exceeds %d bytes", maxCatalogBytes)
	}
	return data, nil
}

// catalogDigest returns the lowercase 64-hex SHA-256 of exact committed bytes.
// Trust verification always hashes the stored bytes; it never parse+reencodes.
func catalogDigest(committed []byte) string {
	sum := sha256.Sum256(committed)
	return hex.EncodeToString(sum[:])
}

// releaseBasename derives <version>-<full 64-hex digest>.
func releaseBasename(version, digest string) string {
	return version + "-" + digest
}

// parseCatalog strictly decodes committed catalog bytes, validates schema rules,
// requires path-sorted unique entries, and returns a trustedCatalog whose Digest
// is the hash of the exact input bytes (not a re-encode).
//
// Committed bytes must exactly equal encodeCatalog output (two-space indent,
// struct field order, single trailing newline). Compact JSON, CRLF, reordered
// fields, duplicate keys, and other non-canonical encodings are rejected even
// when they decode to the same logical document.
func parseCatalog(committed []byte) (*trustedCatalog, error) {
	if len(committed) == 0 {
		return nil, errors.New("catalog is empty")
	}
	if len(committed) > maxCatalogBytes {
		return nil, fmt.Errorf("catalog size %d exceeds limit %d", len(committed), maxCatalogBytes)
	}
	var c catalog
	if err := decodeStrictJSON(committed, &c); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	if err := validateCatalog(c); err != nil {
		return nil, err
	}
	// Require committed entries already sorted (encoder sorts before commit).
	if !entriesPathSorted(c.Entries) {
		return nil, errors.New("catalog entries must be sorted by path")
	}
	// Exact canonical form: registry/catalog bytes identity is the digest of
	// encodeCatalog output, never a parse+semantic-equal substitute.
	canonical, err := encodeCatalog(c)
	if err != nil {
		return nil, fmt.Errorf("catalog canonical encode: %w", err)
	}
	if !bytes.Equal(committed, canonical) {
		return nil, errors.New("catalog bytes must exactly match canonical encodeCatalog form")
	}
	digest := catalogDigest(committed)
	release := releaseBasename(c.Version, digest)
	if !validReleaseBasename(release) {
		return nil, fmt.Errorf("derived release %q is not a valid release basename", release)
	}
	if len(release) > maxPathBytes {
		return nil, fmt.Errorf("derived release exceeds max path bound")
	}
	// Defensive copies so callers cannot mutate shared registry state via slices.
	c.Canaries = append([]string(nil), c.Canaries...)
	c.Entries = append([]installEntry(nil), c.Entries...)
	return &trustedCatalog{
		Catalog: c,
		Bytes:   append([]byte(nil), committed...),
		Digest:  digest,
		Release: release,
	}, nil
}

func validateCatalog(c catalog) error {
	if c.Schema != SchemaVersion {
		return fmt.Errorf("catalog schema %d unsupported (want %d)", c.Schema, SchemaVersion)
	}
	if err := validCatalogBasename(c.ID, "id", maxCatalogIDBytes); err != nil {
		return err
	}
	if err := validCatalogBasename(c.Version, "version", maxCatalogVersionBytes); err != nil {
		return err
	}
	if err := validSourceImage(c.SourceImage); err != nil {
		return err
	}
	if err := validSourceImageDigest(c.SourceImageDigest); err != nil {
		return err
	}
	if err := validateCatalogCanaries(c.Canaries); err != nil {
		return err
	}
	if c.Entries == nil {
		return errors.New("catalog entries must be present")
	}
	if len(c.Entries) > maxEntries {
		return fmt.Errorf("catalog has %d entries (max %d)", len(c.Entries), maxEntries)
	}
	// Admission-time resource bounds (before any source acquisition): aggregate
	// declared bytes and implied parent-directory count. Map-based dir tracking
	// is O(entries·depth), not O(D²). Size accumulation is overflow-safe.
	seen := make(map[string]struct{}, len(c.Entries))
	dirs := make(map[string]struct{}, len(c.Entries)+1)
	dirs["."] = struct{}{}
	var total int64
	for i, e := range c.Entries {
		if err := validTrustedCatalogPath(e.Path); err != nil {
			return fmt.Errorf("entry[%d]: %w", i, err)
		}
		if _, dup := seen[e.Path]; dup {
			return fmt.Errorf("duplicate entry path %q", e.Path)
		}
		seen[e.Path] = struct{}{}
		if e.Size < 0 || e.Size > maxFileBytes {
			return fmt.Errorf("entry %q size out of bounds: %d", e.Path, e.Size)
		}
		// Checked add: reject when e.Size would push total past maxTotalBytes
		// without relying on wrapping int64 arithmetic.
		if e.Size > maxTotalBytes-total {
			return fmt.Errorf("catalog aggregate size exceeds %d bytes", maxTotalBytes)
		}
		total += e.Size
		if !validSHA256Hex(e.SHA256) {
			return fmt.Errorf("entry %q sha256 is not lowercase 64-hex", e.Path)
		}
		if !validCacheClass(e.CacheClass) {
			return fmt.Errorf("entry %q cache_class %q invalid", e.Path, e.CacheClass)
		}
		want, ok := expectedMediaType(e.Path)
		if !ok {
			return fmt.Errorf("entry %q has unsupported extension", e.Path)
		}
		if e.MediaType != want {
			return fmt.Errorf("entry %q media_type %q != expected %q", e.Path, e.MediaType, want)
		}
		// Implied parent directories (plus root). Unique dirs via map only — directory
		// tar headers are optional, so USTAR/ASCII checks apply to file paths alone.
		dir := path.Dir(e.Path)
		for dir != "." && dir != "/" && dir != "" {
			if _, ok := dirs[dir]; !ok {
				if len(dirs) >= maxDirs {
					return fmt.Errorf("catalog implied directory count exceeds maxDirs (%d)", maxDirs)
				}
				dirs[dir] = struct{}{}
			}
			parent := path.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	for _, canary := range canaryRelativePaths {
		if _, ok := seen[canary]; !ok {
			return fmt.Errorf("catalog missing required canary entry %q", canary)
		}
	}
	return nil
}

func validateCatalogCanaries(canaries []string) error {
	if len(canaries) != len(canaryRelativePaths) {
		return fmt.Errorf("catalog canaries length %d != package canaries %d", len(canaries), len(canaryRelativePaths))
	}
	for i, want := range canaryRelativePaths {
		if canaries[i] != want {
			return fmt.Errorf("catalog canaries[%d]=%q want %q", i, canaries[i], want)
		}
	}
	return nil
}

func entriesPathSorted(entries []installEntry) bool {
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Path >= entries[i].Path {
			return false
		}
	}
	return true
}

func validCatalogBasename(s, field string, max int) error {
	if s == "" {
		return fmt.Errorf("catalog %s is empty", field)
	}
	if len(s) > max {
		return fmt.Errorf("catalog %s exceeds %d bytes", field, max)
	}
	if !validReleaseBasename(s) {
		return fmt.Errorf("catalog %s %q is not a strict basename", field, s)
	}
	return nil
}

func validSourceImage(s string) error {
	if s == "" {
		return errors.New("catalog source_image is empty")
	}
	if len(s) > maxSourceImageBytes {
		return fmt.Errorf("catalog source_image exceeds %d bytes", maxSourceImageBytes)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return errors.New("catalog source_image must be printable ASCII without control characters")
		}
	}
	// Product/image identifier: printable ASCII, no whitespace, NUL, backslash,
	// or credential markers. Forward slash and colon are allowed for forms like
	// emby/embyserver:4.9.5.0.
	if strings.ContainsAny(s, " \t\r\n@\\"+"\x00") {
		return errors.New("catalog source_image must not contain whitespace, credentials, or backslash")
	}
	if strings.Contains(s, "://") {
		return errors.New("catalog source_image must be a product identifier, not a URL")
	}
	return nil
}

func validSourceImageDigest(s string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(s, prefix) {
		return errors.New("catalog source_image_digest must start with sha256:")
	}
	hexPart := s[len(prefix):]
	if !validSHA256Hex(hexPart) {
		return errors.New("catalog source_image_digest must be sha256:<64 lowercase hex>")
	}
	return nil
}

// installMatchesTrusted requires install.json identity and every entry field,
// order, and count to equal the trusted catalog-derived install surface.
func installMatchesTrusted(man installManifest, tc *trustedCatalog) error {
	if man.Schema != SchemaVersion {
		return fmt.Errorf("install.json schema %d unsupported (want %d)", man.Schema, SchemaVersion)
	}
	if man.Release != tc.Release {
		return fmt.Errorf("install.json release %q != trusted release %q", man.Release, tc.Release)
	}
	if man.CatalogSHA256 != tc.Digest {
		return fmt.Errorf("install.json catalog_sha256 mismatch with trusted digest")
	}
	if man.Entries == nil {
		return errors.New("install.json entries must be present")
	}
	want := tc.Catalog.Entries
	if len(man.Entries) != len(want) {
		return fmt.Errorf("install.json entry count %d != trusted catalog %d", len(man.Entries), len(want))
	}
	for i := range man.Entries {
		if man.Entries[i] != want[i] {
			return fmt.Errorf("install.json entry[%d] does not match trusted catalog", i)
		}
	}
	return nil
}

// pointerMatchesTrusted checks pointer identity against a resolved trusted catalog.
func pointerMatchesTrusted(ptr currentPointer, tc *trustedCatalog) error {
	if ptr.Schema != SchemaVersion {
		return fmt.Errorf("current.json schema %d unsupported (want %d)", ptr.Schema, SchemaVersion)
	}
	if ptr.CatalogSHA256 != tc.Digest {
		return fmt.Errorf("current.json catalog_sha256 mismatch with trusted digest")
	}
	if ptr.Release != tc.Release {
		return fmt.Errorf("current.json release %q != trusted release %q", ptr.Release, tc.Release)
	}
	return nil
}
