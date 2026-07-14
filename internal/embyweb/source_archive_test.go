package embyweb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type craftEntry struct {
	Name     string
	Body     []byte
	Typeflag byte
	Format   tar.Format
	Linkname string
	// SizeOverride, when non-nil, sets Header.Size independently of Body.
	SizeOverride *int64
	Mode         int64
}

func int64ptr(v int64) *int64 { return &v }

func writeUSTARTarGz(t *testing.T, path string, entries []craftEntry) {
	t.Helper()
	raw := craftUSTARBytes(t, entries)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// craftUSTARBytes builds a tar stream with FormatUSTAR forced on every header
// and verifies the first readable header round-trips as FormatUSTAR.
func craftUSTARBytes(t *testing.T, entries []craftEntry) []byte {
	t.Helper()
	forced := make([]craftEntry, len(entries))
	for i, e := range entries {
		forced[i] = e
		forced[i].Format = tar.FormatUSTAR
	}
	raw := craftTarBytes(t, forced)
	assertTarHeadersFormat(t, raw, tar.FormatUSTAR)
	return raw
}

func craftTarBytes(t *testing.T, entries []craftEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		tf := e.Typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		format := e.Format
		if format == 0 {
			format = tar.FormatUSTAR
		}
		size := int64(len(e.Body))
		if e.SizeOverride != nil {
			size = *e.SizeOverride
		}
		mode := e.Mode
		if mode == 0 {
			if tf == tar.TypeDir {
				mode = 0o755
			} else {
				mode = 0o644
			}
		}
		hdr := &tar.Header{
			Typeflag: tf,
			Name:     e.Name,
			Size:     size,
			Mode:     mode,
			ModTime:  time.Unix(0, 0).UTC(),
			Format:   format,
			Linkname: e.Linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader %q: %v", e.Name, err)
		}
		if len(e.Body) > 0 && (tf == tar.TypeReg || tf == tar.TypeRegA) {
			if _, err := tw.Write(e.Body); err != nil {
				t.Fatalf("Write body %q: %v", e.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func assertTarHeadersFormat(t *testing.T, raw []byte, want tar.Format) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if h.Format != want {
			t.Fatalf("tar header %q format=%v want %v", h.Name, h.Format, want)
		}
		if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA {
			if _, err := io.Copy(io.Discard, tr); err != nil {
				t.Fatalf("drain %q: %v", h.Name, err)
			}
		}
	}
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func catalogFilesEntries(files []fixtureFile) []craftEntry {
	out := make([]craftEntry, 0, len(files))
	for _, f := range files {
		out = append(out, craftEntry{Name: f.Path, Body: f.Data, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
	}
	return out
}

func catalogFilesWithDirs(files []fixtureFile) []craftEntry {
	dirs := map[string]struct{}{}
	for _, f := range files {
		dir := filepath.ToSlash(filepath.Dir(f.Path))
		for dir != "." && dir != "/" && dir != "" {
			dirs[dir] = struct{}{}
			parent := filepath.ToSlash(filepath.Dir(dir))
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	var out []craftEntry
	// Stable-ish order: shorter dirs first.
	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		ordered = append(ordered, d)
	}
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if strings.Count(ordered[j], "/") < strings.Count(ordered[i], "/") ||
				(strings.Count(ordered[j], "/") == strings.Count(ordered[i], "/") && ordered[j] < ordered[i]) {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	for _, d := range ordered {
		out = append(out, craftEntry{Name: d + "/", Typeflag: tar.TypeDir, Format: tar.FormatUSTAR})
	}
	out = append(out, catalogFilesEntries(files)...)
	return out
}

func newArchiveTestEnv(t *testing.T, files []fixtureFile) (*trustedCatalog, *stagingWriter, string) {
	t.Helper()
	tc := buildSyntheticCatalog(t, files, "archive-test", "1.0.0")
	dir := t.TempDir()
	filesDir := filepath.Join(dir, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	w, err := newStagingWriter(root, tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tc, w, dir
}

func runArchiveAcquire(t *testing.T, src *archiveSource, tc *trustedCatalog, w *stagingWriter) error {
	t.Helper()
	return src.acquire(context.Background(), tc, w)
}

func TestNewArchiveSource(t *testing.T) {
	if _, err := newArchiveSource(""); err == nil {
		t.Fatal("expected empty path error")
	}
	if _, err := newArchiveSource("/tmp/x.tgz"); err == nil {
		t.Fatal("expected non-.tar.gz error")
	}
	if _, err := newArchiveSource("/tmp/x.TAR.GZ"); err == nil {
		t.Fatal("expected case-sensitive suffix error")
	}
	if _, err := newArchiveSource("/tmp/x.tar.GZ"); err == nil {
		t.Fatal("expected case-sensitive suffix error")
	}
	src, err := newArchiveSource("/tmp/x.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if src.kind() != "archive" {
		t.Fatalf("kind=%q", src.kind())
	}
	var _ acquisitionSource = src
}

func TestArchiveSourceSuccessNoDirHeaders(t *testing.T) {
	files := readyMinimalFiles()
	tc, w, dir := newArchiveTestEnv(t, files)
	path := filepath.Join(dir, "ok.tar.gz")
	writeUSTARTarGz(t, path, catalogFilesEntries(files))
	src, err := newArchiveSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runArchiveAcquire(t, src, tc, w); err != nil {
		t.Fatal(err)
	}
	if err := w.complete(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveSourceSuccessWithDirHeaders(t *testing.T) {
	files := readyMinimalFiles()
	tc, w, dir := newArchiveTestEnv(t, files)
	path := filepath.Join(dir, "ok-dirs.tar.gz")
	writeUSTARTarGz(t, path, catalogFilesWithDirs(files))
	src, err := newArchiveSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runArchiveAcquire(t, src, tc, w); err != nil {
		t.Fatal(err)
	}
	if err := w.complete(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveSourceRejectsWrongExtension(t *testing.T) {
	files := readyMinimalFiles()
	tc, w, dir := newArchiveTestEnv(t, files)
	// Valid archive content under a non-matching name.
	path := filepath.Join(dir, "bad.tgz")
	writeUSTARTarGz(t, path, catalogFilesEntries(files))
	if _, err := newArchiveSource(path); err == nil {
		t.Fatal("expected constructor rejection")
	}
	// Force acquire path check via crafted source.
	src := &archiveSource{path: path}
	err := runArchiveAcquire(t, src, tc, w)
	if err == nil || !strings.Contains(err.Error(), ".tar.gz") {
		t.Fatalf("err=%v", err)
	}
}

func TestArchiveSourceRejectsSymlinkAndNonRegular(t *testing.T) {
	files := readyMinimalFiles()
	tc, w, dir := newArchiveTestEnv(t, files)

	realPath := filepath.Join(dir, "real.tar.gz")
	writeUSTARTarGz(t, realPath, catalogFilesEntries(files))

	linkPath := filepath.Join(dir, "link.tar.gz")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	src, err := newArchiveSource(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	err = runArchiveAcquire(t, src, tc, w)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink err=%v", err)
	}

	dirPath := filepath.Join(dir, "dir.tar.gz")
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	src2, err := newArchiveSource(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	// Need a fresh writer; previous may be partially used (none written though).
	_, w2, _ := newArchiveTestEnv(t, files)
	err = runArchiveAcquire(t, src2, tc, w2)
	if err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("dir err=%v", err)
	}
}

func TestArchiveSourceRejectsPathTraversalAndWrapper(t *testing.T) {
	files := readyMinimalFiles()
	cases := []struct {
		name    string
		entries []craftEntry
		substr  string
	}{
		{
			name: "dotdot",
			entries: []craftEntry{
				{Name: "../index.html", Body: files[0].Data, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR},
			},
			substr: "canonical",
		},
		{
			name: "absolute",
			entries: []craftEntry{
				{Name: "/index.html", Body: files[0].Data, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR},
			},
			substr: "absolute",
		},
		{
			name: "backslash",
			entries: []craftEntry{
				{Name: `modules\app.js`, Body: []byte("x"), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR},
			},
			substr: "backslash",
		},
		{
			name: "wrapper",
			entries: func() []craftEntry {
				var e []craftEntry
				for _, f := range files {
					e = append(e, craftEntry{Name: "web/" + f.Path, Body: f.Data, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
				}
				return e
			}(),
			substr: "undeclared",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat, w, dir := newArchiveTestEnv(t, files)
			path := filepath.Join(dir, tc.name+".tar.gz")
			writeUSTARTarGz(t, path, tc.entries)
			src, err := newArchiveSource(path)
			if err != nil {
				t.Fatal(err)
			}
			err = runArchiveAcquire(t, src, cat, w)
			if err == nil || !strings.Contains(err.Error(), tc.substr) {
				t.Fatalf("err=%v want substr %q", err, tc.substr)
			}
		})
	}
}

func TestArchiveSourceRejectsDuplicateAndUndeclaredMissing(t *testing.T) {
	files := readyMinimalFiles()
	t.Run("duplicate", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		entries := catalogFilesEntries(files)
		entries = append(entries, craftEntry{Name: files[0].Path, Body: files[0].Data, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
		path := filepath.Join(dir, "dup.tar.gz")
		writeUSTARTarGz(t, path, entries)
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("undeclared", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		entries := catalogFilesEntries(files)
		entries = append(entries, craftEntry{Name: "extra.txt", Body: []byte("nope"), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
		path := filepath.Join(dir, "extra.tar.gz")
		writeUSTARTarGz(t, path, entries)
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "undeclared") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		// Omit modules/app.js
		var entries []craftEntry
		for _, f := range files {
			if f.Path == "modules/app.js" {
				continue
			}
			entries = append(entries, craftEntry{Name: f.Path, Body: f.Data, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
		}
		path := filepath.Join(dir, "miss.tar.gz")
		writeUSTARTarGz(t, path, entries)
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("duplicate-dir", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		entries := []craftEntry{
			{Name: "modules/", Typeflag: tar.TypeDir, Format: tar.FormatUSTAR},
			{Name: "modules/", Typeflag: tar.TypeDir, Format: tar.FormatUSTAR},
		}
		entries = append(entries, catalogFilesEntries(files)...)
		path := filepath.Join(dir, "dupdir.tar.gz")
		writeUSTARTarGz(t, path, entries)
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "duplicate directory") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("undeclared-dir", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		entries := []craftEntry{
			{Name: "not-in-catalog/", Typeflag: tar.TypeDir, Format: tar.FormatUSTAR},
		}
		entries = append(entries, catalogFilesEntries(files)...)
		path := filepath.Join(dir, "baddir.tar.gz")
		writeUSTARTarGz(t, path, entries)
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "undeclared directory") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestArchiveSourceRejectsWrongSizeAndHash(t *testing.T) {
	files := readyMinimalFiles()
	t.Run("wrong-size", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		var entries []craftEntry
		for _, f := range files {
			body := f.Data
			if f.Path == "index.html" {
				body = append(append([]byte{}, f.Data...), '!')
			}
			entries = append(entries, craftEntry{Name: f.Path, Body: body, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
		}
		path := filepath.Join(dir, "size.tar.gz")
		writeUSTARTarGz(t, path, entries)
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "size") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("wrong-hash", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		var entries []craftEntry
		for _, f := range files {
			body := append([]byte{}, f.Data...)
			if f.Path == "index.html" && len(body) > 0 {
				body[0] ^= 0xff
			}
			entries = append(entries, craftEntry{Name: f.Path, Body: body, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
		}
		path := filepath.Join(dir, "hash.tar.gz")
		writeUSTARTarGz(t, path, entries)
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestArchiveSourceRejectsForbiddenTypeflags(t *testing.T) {
	files := readyMinimalFiles()
	// Minimal single-file catalog for isolated typeflag tests would still need canaries.
	// Use full catalog but put forbidden type first so we fail before missing files.
	types := []struct {
		name     string
		typeflag byte
		link     string
		substr   string
	}{
		{"symlink", tar.TypeSymlink, "target", "link"},
		{"hardlink", tar.TypeLink, "index.html", "link"},
		{"char", tar.TypeChar, "", "special"},
		{"block", tar.TypeBlock, "", "special"},
		{"fifo", tar.TypeFifo, "", "special"},
		{"cont", tar.TypeCont, "", "special"},
		{"unknown", 'Z', "", "forbidden"},
	}
	for _, tc := range types {
		t.Run(tc.name, func(t *testing.T) {
			cat, w, dir := newArchiveTestEnv(t, files)
			entries := []craftEntry{{
				Name:     "evil",
				Typeflag: tc.typeflag,
				Format:   tar.FormatUSTAR,
				Linkname: tc.link,
				Body:     nil,
			}}
			// For types that require size 0 body.
			path := filepath.Join(dir, tc.name+".tar.gz")
			writeUSTARTarGz(t, path, entries)
			src, _ := newArchiveSource(path)
			err := runArchiveAcquire(t, src, cat, w)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.substr) &&
				!strings.Contains(err.Error(), typeflagName(tc.typeflag)) &&
				!strings.Contains(err.Error(), "rejected") &&
				!strings.Contains(err.Error(), "forbidden") {
				t.Fatalf("err=%v want substr related to %q", err, tc.substr)
			}
		})
	}
}

func writeForcedPAXTarGz(t *testing.T, path string, files []fixtureFile) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, f := range files {
		hdr := &tar.Header{
			Typeflag:   tar.TypeReg,
			Name:       f.Path,
			Size:       int64(len(f.Data)),
			Mode:       0o644,
			ModTime:    time.Unix(1, 123456789), // sub-second forces PAX records
			AccessTime: time.Unix(2, 0),
			Format:     tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeForcedGNUTarGz(t *testing.T, path string, files []fixtureFile) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, f := range files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     f.Path,
			Size:     int64(len(f.Data)),
			Mode:     0o644,
			ModTime:  time.Unix(1, 0),
			Format:   tar.FormatGNU,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveSourceRejectsPAXAndGNU(t *testing.T) {
	files := readyMinimalFiles()
	t.Run("pax-format", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		path := filepath.Join(dir, "pax.tar.gz")
		writeForcedPAXTarGz(t, path, files)
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "PAX") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("gnu-format", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		path := filepath.Join(dir, "gnu.tar.gz")
		writeForcedGNUTarGz(t, path, files)
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "GNU") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("gnu-longname-attempt", func(t *testing.T) {
		// Long path requires GNU long-name extension; must not use USTAR fixture writer.
		cat, w, dir := newArchiveTestEnv(t, files)
		long := strings.Repeat("a", 120) + ".js"
		entries := []craftEntry{{
			Name: long, Body: []byte("x"), Typeflag: tar.TypeReg, Format: tar.FormatGNU,
		}}
		path := filepath.Join(dir, "longname.tar.gz")
		if err := os.WriteFile(path, gzipBytes(t, craftTarBytes(t, entries)), 0o644); err != nil {
			t.Fatal(err)
		}
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil {
			t.Fatal("expected rejection")
		}
		// GNU format, undeclared path, or invalid path are all acceptable rejections.
		if !strings.Contains(err.Error(), "GNU") &&
			!strings.Contains(err.Error(), "undeclared") &&
			!strings.Contains(err.Error(), "invalid") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("sparse-typeflag", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		path := filepath.Join(dir, "sparse.tar.gz")
		raw := makeSparseLikeTar(t)
		if err := os.WriteFile(path, gzipBytes(t, raw), 0o644); err != nil {
			t.Fatal(err)
		}
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil {
			t.Fatal("expected sparse rejection")
		}
	})
}

// makeSparseLikeTar builds a tar by taking a valid USTAR header and flipping
// the typeflag to GNU sparse 'S', recomputing the checksum. The archive/tar
// reader may reject the incomplete sparse map; either library rejection or our
// policy rejection satisfies the sparse-attempt coverage.
func makeSparseLikeTar(t *testing.T) []byte {
	t.Helper()
	base := craftTarBytes(t, []craftEntry{{
		Name: "sparse.bin", Body: []byte("data"), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR,
	}})
	if len(base) < 512 {
		t.Fatalf("tar too short: %d", len(base))
	}
	hdr := append([]byte(nil), base[:512]...)
	hdr[156] = tar.TypeGNUSparse
	for i := 148; i < 156; i++ {
		hdr[i] = ' '
	}
	var sum int64
	for _, b := range hdr {
		sum += int64(b)
	}
	// Standard ustar checksum: 6 octal digits, NUL, space.
	cs := []byte{0, 0, 0, 0, 0, 0, 0, ' '}
	v := sum
	for i := 5; i >= 0; i-- {
		cs[i] = byte('0' + (v & 7))
		v >>= 3
	}
	cs[6] = 0
	copy(hdr[148:], cs)
	out := append(hdr, base[512:]...)
	return out
}

func TestArchiveSourceRejectsConcatenatedGzipAndTrailing(t *testing.T) {
	files := readyMinimalFiles()
	t.Run("concatenated-gzip", func(t *testing.T) {
		// Second gzip member after a complete valid member must be rejected as
		// trailing compressed input (Multistream(false) + post-member peek).
		cat, w, dir := newArchiveTestEnv(t, files)
		raw := craftUSTARBytes(t, catalogFilesEntries(files))
		first := gzipBytes(t, raw)
		second := gzipBytes(t, []byte("second-member"))
		path := filepath.Join(dir, "concat.tar.gz")
		if err := os.WriteFile(path, append(append([]byte{}, first...), second...), 0o644); err != nil {
			t.Fatal(err)
		}
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "trailing compressed bytes after gzip member") {
			t.Fatalf("err=%v want trailing compressed bytes after gzip member", err)
		}
	})
	t.Run("trailing-bytes", func(t *testing.T) {
		// Non-gzip garbage after a single complete member is the same class of error.
		cat, w, dir := newArchiveTestEnv(t, files)
		raw := craftUSTARBytes(t, catalogFilesEntries(files))
		gz := gzipBytes(t, raw)
		gz = append(gz, []byte("GARBAGE")...)
		path := filepath.Join(dir, "trail.tar.gz")
		if err := os.WriteFile(path, gz, 0o644); err != nil {
			t.Fatal(err)
		}
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "trailing compressed bytes after gzip member") {
			t.Fatalf("err=%v want trailing compressed bytes after gzip member", err)
		}
	})
	t.Run("extra-decompressed", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		raw := craftUSTARBytes(t, catalogFilesEntries(files))
		// Append non-zero junk after tar end markers inside the same gzip member.
		raw = append(raw, []byte("NOT-TAR-PADDING!!")...)
		path := filepath.Join(dir, "extradec.tar.gz")
		if err := os.WriteFile(path, gzipBytes(t, raw), 0o644); err != nil {
			t.Fatal(err)
		}
		src, _ := newArchiveSource(path)
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "extra decompressed bytes after tar EOF") {
			t.Fatalf("err=%v want extra decompressed bytes after tar EOF", err)
		}
	})
}

func TestArchiveSourceRejectsGzipChecksumCorruption(t *testing.T) {
	files := readyMinimalFiles()
	cat, w, dir := newArchiveTestEnv(t, files)
	raw := craftUSTARBytes(t, catalogFilesEntries(files))
	gz := gzipBytes(t, raw)
	if len(gz) < 8 {
		t.Fatal("gzip too short")
	}
	// Flip a bit in the CRC32 trailer (last 8 bytes: CRC32 + ISIZE).
	// archive/tar drains the member fully; the next post-tar gzip Read surfaces
	// gzip.ErrChecksum ("gzip: invalid checksum") before Close.
	gz[len(gz)-8] ^= 0xff
	path := filepath.Join(dir, "badcrc.tar.gz")
	if err := os.WriteFile(path, gz, 0o644); err != nil {
		t.Fatal(err)
	}
	src, _ := newArchiveSource(path)
	err := runArchiveAcquire(t, src, cat, w)
	if err == nil {
		t.Fatal("expected checksum error")
	}
	if !errors.Is(err, gzip.ErrChecksum) && !strings.Contains(err.Error(), "invalid checksum") {
		t.Fatalf("err=%v want gzip invalid checksum", err)
	}
}

func TestArchiveSourceCancellation(t *testing.T) {
	files := readyMinimalFiles()
	tc, w, dir := newArchiveTestEnv(t, files)
	path := filepath.Join(dir, "ok.tar.gz")
	writeUSTARTarGz(t, path, catalogFilesEntries(files))
	src, err := newArchiveSource(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = src.acquire(ctx, tc, w)
	if err == nil || !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "cancelled") && err != context.Canceled {
		// context.Canceled string is "context canceled"
		if err != context.Canceled && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestArchiveSourceLimitsViaTestOverrides(t *testing.T) {
	files := readyMinimalFiles()

	t.Run("compressed-limit", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		path := filepath.Join(dir, "c.tar.gz")
		writeUSTARTarGz(t, path, catalogFilesEntries(files))
		src, _ := newArchiveSource(path)
		src.testMaxCompressed = 32 // tiny
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "compressed") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("expanded-limit", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		path := filepath.Join(dir, "e.tar.gz")
		writeUSTARTarGz(t, path, catalogFilesEntries(files))
		src, _ := newArchiveSource(path)
		src.testMaxExpanded = 64 // smaller than tar stream
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("payload-limit", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		path := filepath.Join(dir, "p.tar.gz")
		writeUSTARTarGz(t, path, catalogFilesEntries(files))
		src, _ := newArchiveSource(path)
		// Sum of readyMinimalFiles is small; set payload below first file size.
		src.testMaxPayload = 1
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "payload") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("header-limit", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		path := filepath.Join(dir, "h.tar.gz")
		writeUSTARTarGz(t, path, catalogFilesEntries(files))
		src, _ := newArchiveSource(path)
		src.testMaxHeaders = 2
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "header") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("node-limit", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		path := filepath.Join(dir, "n.tar.gz")
		writeUSTARTarGz(t, path, catalogFilesWithDirs(files))
		src, _ := newArchiveSource(path)
		src.testMaxNodes = 2
		err := runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "node") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestArchiveSourceTypeRegA(t *testing.T) {
	files := readyMinimalFiles()
	cat, w, dir := newArchiveTestEnv(t, files)
	var entries []craftEntry
	for _, f := range files {
		entries = append(entries, craftEntry{
			Name: f.Path, Body: f.Data, Typeflag: tar.TypeRegA, Format: tar.FormatUSTAR,
		})
	}
	path := filepath.Join(dir, "rega.tar.gz")
	writeUSTARTarGz(t, path, entries)
	src, _ := newArchiveSource(path)
	if err := runArchiveAcquire(t, src, cat, w); err != nil {
		t.Fatal(err)
	}
	if err := w.complete(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveSourceRejectsFormatUnknownLegacy(t *testing.T) {
	// V7 tar without ustar magic is reported as FormatUnknown and must be rejected.
	files := readyMinimalFiles()
	t.Run("validateArchiveHeader", func(t *testing.T) {
		err := validateArchiveHeader(&tar.Header{
			Format:   tar.FormatUnknown,
			Typeflag: tar.TypeReg,
			Name:     "index.html",
			Size:     1,
		})
		if err == nil || !strings.Contains(err.Error(), "unknown/legacy") {
			t.Fatalf("err=%v", err)
		}
		// Positive control: USTAR still accepted.
		if err := validateArchiveHeader(&tar.Header{
			Format:   tar.FormatUSTAR,
			Typeflag: tar.TypeReg,
			Name:     "index.html",
			Size:     1,
		}); err != nil {
			t.Fatalf("USTAR rejected: %v", err)
		}
	})
	t.Run("acquire-v7-legacy", func(t *testing.T) {
		cat, w, dir := newArchiveTestEnv(t, files)
		// Minimal V7 header for the first catalog file; FormatUnknown on read.
		f0 := files[0]
		raw := craftV7LegacyTar(t, f0.Path, f0.Data)
		// Confirm reader classifies as unknown before acquire.
		tr := tar.NewReader(bytes.NewReader(raw))
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Format != tar.FormatUnknown {
			t.Fatalf("fixture format=%v want FormatUnknown", h.Format)
		}
		path := filepath.Join(dir, "v7.tar.gz")
		if err := os.WriteFile(path, gzipBytes(t, raw), 0o644); err != nil {
			t.Fatal(err)
		}
		src, _ := newArchiveSource(path)
		err = runArchiveAcquire(t, src, cat, w)
		if err == nil || !strings.Contains(err.Error(), "unknown/legacy") {
			t.Fatalf("err=%v want unknown/legacy rejection", err)
		}
	})
}

// craftV7LegacyTar builds a pre-ustar (V7) tar with a single regular file and
// two end-of-archive zero blocks. archive/tar reports FormatUnknown for these.
func craftV7LegacyTar(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	if name == "" || strings.ContainsAny(name, `\`+"\x00") || len(name) >= 100 {
		t.Fatalf("unsupported v7 name %q", name)
	}
	hdr := make([]byte, 512)
	copy(hdr[0:], name)
	copy(hdr[100:], []byte("0000644\x00"))
	copy(hdr[108:], []byte("0000000\x00"))
	copy(hdr[116:], []byte("0000000\x00"))
	sz := []byte("00000000000")
	v := int64(len(body))
	for i := 10; i >= 0; i-- {
		sz[i] = byte('0' + (v & 7))
		v >>= 3
	}
	copy(hdr[124:], sz)
	hdr[135] = 0
	copy(hdr[136:], []byte("00000000000\x00"))
	for i := 148; i < 156; i++ {
		hdr[i] = ' '
	}
	hdr[156] = tar.TypeReg // '0'
	// Intentionally omit ustar magic at 257 so the reader classifies as unknown.
	var sum int64
	for _, b := range hdr {
		sum += int64(b)
	}
	cs := make([]byte, 8)
	cv := sum
	for i := 5; i >= 0; i-- {
		cs[i] = byte('0' + (cv & 7))
		cv >>= 3
	}
	cs[6] = 0
	cs[7] = ' '
	copy(hdr[148:], cs)

	pad := (512 - (len(body) % 512)) % 512
	out := append(hdr, body...)
	out = append(out, make([]byte, pad)...)
	out = append(out, make([]byte, 1024)...) // two zero blocks
	return out
}

func TestNormalizeArchiveNames(t *testing.T) {
	if _, err := normalizeArchiveFileName("a/../b.js"); err == nil {
		t.Fatal("expected reject")
	}
	if _, err := normalizeArchiveFileName("ok/path.js"); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeArchiveDirName("modules/"); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeArchiveDirName("modules//"); err == nil {
		t.Fatal("expected multi-slash reject")
	}
	if _, err := normalizeArchiveDirName("/abs/"); err == nil {
		t.Fatal("expected abs reject")
	}
}
