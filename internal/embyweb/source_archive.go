package embyweb

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Archive stream bounds. Compressed and expanded streams are capped at 272 MiB;
// catalog file payload uses the existing maxTotalBytes (256 MiB).
const (
	maxArchiveCompressedBytes = 272 << 20 // 272 MiB
	maxArchiveExpandedBytes   = 272 << 20 // 272 MiB
	archiveSuffix             = ".tar.gz"
)

// archiveSource materializes catalog files from a single local .tar.gz archive.
// It is package-private; construction validates the path suffix only.
type archiveSource struct {
	path string

	// Optional package-private test overrides; zero means production default.
	testMaxCompressed int64
	testMaxExpanded   int64
	testMaxPayload    int64
	testMaxHeaders    int
	testMaxNodes      int
}

// newArchiveSource returns an acquisitionSource for a case-sensitive .tar.gz path.
// The file is not opened until acquire; only the path form is checked here.
func newArchiveSource(archivePath string) (*archiveSource, error) {
	if archivePath == "" {
		return nil, errors.New("archive source: empty path")
	}
	if !strings.HasSuffix(archivePath, archiveSuffix) {
		return nil, fmt.Errorf("archive source: path must end with %q", archiveSuffix)
	}
	return &archiveSource{path: archivePath}, nil
}

func (s *archiveSource) kind() string { return "archive" }

func (s *archiveSource) limits() (compressed, expanded, payload int64, maxHeaders, maxNodes int) {
	compressed = maxArchiveCompressedBytes
	expanded = maxArchiveExpandedBytes
	payload = int64(maxTotalBytes)
	maxHeaders = maxEntries + maxDirs
	maxNodes = maxEntries + maxDirs
	if s.testMaxCompressed > 0 {
		compressed = s.testMaxCompressed
	}
	if s.testMaxExpanded > 0 {
		expanded = s.testMaxExpanded
	}
	if s.testMaxPayload > 0 {
		payload = s.testMaxPayload
	}
	if s.testMaxHeaders > 0 {
		maxHeaders = s.testMaxHeaders
	}
	if s.testMaxNodes > 0 {
		maxNodes = s.testMaxNodes
	}
	return compressed, expanded, payload, maxHeaders, maxNodes
}

func (s *archiveSource) acquire(ctx context.Context, tc *trustedCatalog, w *stagingWriter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tc == nil {
		return errors.New("archive source: nil catalog")
	}
	if w == nil {
		return errors.New("archive source: nil staging writer")
	}
	if !strings.HasSuffix(s.path, archiveSuffix) {
		return fmt.Errorf("archive source: path must end with %q", archiveSuffix)
	}

	maxCompressed, maxExpanded, maxPayload, maxHeaders, maxNodes := s.limits()

	// Atomic final-component no-follow open (O_NOFOLLOW|O_NONBLOCK) so a
	// symlink/FIFO swap cannot hang or redirect the open. Size is taken from
	// the opened fd, not a prior Lstat.
	f, err := openRegularFileNoFollow(s.path)
	if err != nil {
		return fmt.Errorf("archive source: open: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("archive source: stat: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return errors.New("archive source: opened handle is not a regular file")
	}
	if fi.Size() < 0 || fi.Size() > maxCompressed {
		return fmt.Errorf("archive source: compressed size %d exceeds limit %d", fi.Size(), maxCompressed)
	}

	// Bound compressed reads; +1 detects growth past the declared cap.
	compressed := &countCapReader{r: f, cap: maxCompressed}
	br := bufio.NewReader(compressed)

	gz, err := gzip.NewReader(br)
	if err != nil {
		return fmt.Errorf("archive source: gzip: %w", err)
	}
	gz.Multistream(false)
	gzClosed := false
	defer func() {
		if !gzClosed {
			_ = gz.Close()
		}
	}()

	expanded := &countCapReader{r: gz, cap: maxExpanded}
	tr := tar.NewReader(expanded)

	requiredDirs := catalogRequiredDirs(tc)
	expectedFiles := make(map[string]installEntry, len(tc.Catalog.Entries))
	for _, e := range tc.Catalog.Entries {
		expectedFiles[e.Path] = e
	}

	seenFiles := make(map[string]struct{}, len(expectedFiles))
	seenDirs := make(map[string]struct{}, len(requiredDirs))
	var payload int64
	headers := 0
	nodes := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("archive source: tar: %w", err)
		}
		headers++
		if headers > maxHeaders {
			return fmt.Errorf("archive source: header count exceeds limit %d", maxHeaders)
		}

		if err := validateArchiveHeader(hdr); err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			name, err := normalizeArchiveFileName(hdr.Name)
			if err != nil {
				return err
			}
			if _, dup := seenFiles[name]; dup {
				return fmt.Errorf("archive source: duplicate file %q", name)
			}
			if _, asDir := seenDirs[name]; asDir {
				return fmt.Errorf("archive source: %q declared as both file and directory", name)
			}
			entry, ok := expectedFiles[name]
			if !ok {
				return fmt.Errorf("archive source: undeclared file %q", name)
			}
			if hdr.Size != entry.Size {
				return fmt.Errorf("archive source: %q header size %d != catalog %d", name, hdr.Size, entry.Size)
			}
			if hdr.Size < 0 {
				return fmt.Errorf("archive source: %q negative size", name)
			}
			// Safe payload accumulation against overflow and catalog total cap.
			if payload > maxPayload || hdr.Size > maxPayload-payload {
				return fmt.Errorf("archive source: payload exceeds limit %d", maxPayload)
			}

			nodes++
			if nodes > maxNodes {
				return fmt.Errorf("archive source: node count exceeds limit %d", maxNodes)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := w.writeFile(name, tr); err != nil {
				return fmt.Errorf("archive source: write %q: %w", name, err)
			}
			payload += hdr.Size
			seenFiles[name] = struct{}{}

		case tar.TypeDir:
			name, err := normalizeArchiveDirName(hdr.Name)
			if err != nil {
				return err
			}
			if hdr.Size != 0 {
				return fmt.Errorf("archive source: directory %q has non-zero size %d", name, hdr.Size)
			}
			if _, ok := requiredDirs[name]; !ok {
				return fmt.Errorf("archive source: undeclared directory %q", name)
			}
			if _, dup := seenDirs[name]; dup {
				return fmt.Errorf("archive source: duplicate directory %q", name)
			}
			if _, asFile := seenFiles[name]; asFile {
				return fmt.Errorf("archive source: %q declared as both file and directory", name)
			}
			nodes++
			if nodes > maxNodes {
				return fmt.Errorf("archive source: node count exceeds limit %d", maxNodes)
			}
			seenDirs[name] = struct{}{}

		default:
			return fmt.Errorf("archive source: forbidden typeflag %q", typeflagName(hdr.Typeflag))
		}
	}

	// Reject any extra decompressed bytes after tar EOF (beyond end-of-archive).
	var extra [1]byte
	if n, err := expanded.Read(extra[:]); n > 0 {
		return errors.New("archive source: extra decompressed bytes after tar EOF")
	} else if err != nil && err != io.EOF {
		return fmt.Errorf("archive source: post-tar read: %w", err)
	}

	// Close gzip to validate CRC/size trailer for the single member.
	if err := gz.Close(); err != nil {
		return fmt.Errorf("archive source: gzip close: %w", err)
	}
	gzClosed = true

	// Multistream(false): any remaining compressed input is a second member or trailing garbage.
	if _, err := br.ReadByte(); err != io.EOF {
		if err == nil {
			return errors.New("archive source: trailing compressed bytes after gzip member")
		}
		return fmt.Errorf("archive source: trailing compressed check: %w", err)
	}
	if compressed.read > maxCompressed {
		return fmt.Errorf("archive source: compressed size exceeds limit %d", maxCompressed)
	}

	if len(seenFiles) != len(expectedFiles) {
		for p := range expectedFiles {
			if _, ok := seenFiles[p]; !ok {
				return fmt.Errorf("archive source: missing catalog file %q", p)
			}
		}
		return fmt.Errorf("archive source: file count %d != catalog %d", len(seenFiles), len(expectedFiles))
	}
	return nil
}

func validateArchiveHeader(hdr *tar.Header) error {
	if hdr == nil {
		return errors.New("archive source: nil tar header")
	}
	// Strict USTAR only: reject PAX, GNU, and legacy/unknown formats.
	switch hdr.Format {
	case tar.FormatUSTAR:
		// ok
	case tar.FormatPAX:
		return errors.New("archive source: PAX format rejected")
	case tar.FormatGNU:
		return errors.New("archive source: GNU format rejected")
	case tar.FormatUnknown:
		return errors.New("archive source: unknown/legacy tar format rejected")
	default:
		return fmt.Errorf("archive source: unsupported tar format %v", hdr.Format)
	}
	if len(hdr.PAXRecords) > 0 {
		return errors.New("archive source: PAX records rejected")
	}
	if len(hdr.Xattrs) > 0 {
		return errors.New("archive source: xattrs rejected")
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
		if hdr.Linkname != "" {
			return errors.New("archive source: linkname set on non-link entry")
		}
	case tar.TypeXHeader, tar.TypeXGlobalHeader:
		return errors.New("archive source: PAX extended header rejected")
	case tar.TypeGNUSparse, tar.TypeGNULongName, tar.TypeGNULongLink:
		return errors.New("archive source: GNU extension rejected")
	case tar.TypeLink, tar.TypeSymlink:
		return fmt.Errorf("archive source: link type %q rejected", typeflagName(hdr.Typeflag))
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo, tar.TypeCont:
		return fmt.Errorf("archive source: special type %q rejected", typeflagName(hdr.Typeflag))
	default:
		return fmt.Errorf("archive source: forbidden typeflag %q", typeflagName(hdr.Typeflag))
	}
	return nil
}

func normalizeArchiveFileName(name string) (string, error) {
	if name == "" {
		return "", errors.New("archive source: empty file name")
	}
	if strings.ContainsAny(name, `\`+"\x00") {
		return "", fmt.Errorf("archive source: file name %q has backslash or NUL", name)
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive source: absolute path %q", name)
	}
	if strings.HasSuffix(name, "/") {
		return "", fmt.Errorf("archive source: file name %q has trailing slash", name)
	}
	// Reject wrapper-style or non-canonical forms before ValidPath.
	if name == "." || name == ".." || strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("archive source: non-canonical file path %q", name)
	}
	if strings.Contains(name, "/./") || strings.Contains(name, "/../") || strings.HasSuffix(name, "/.") || strings.HasSuffix(name, "/..") {
		return "", fmt.Errorf("archive source: non-canonical file path %q", name)
	}
	if path.Clean(name) != name {
		return "", fmt.Errorf("archive source: non-canonical file path %q", name)
	}
	if !validAssetPath(name) {
		return "", fmt.Errorf("archive source: invalid file path %q", name)
	}
	return name, nil
}

func normalizeArchiveDirName(name string) (string, error) {
	if name == "" {
		return "", errors.New("archive source: empty directory name")
	}
	if strings.ContainsAny(name, `\`+"\x00") {
		return "", fmt.Errorf("archive source: directory name %q has backslash or NUL", name)
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive source: absolute directory path %q", name)
	}
	// At most one conventional trailing slash.
	trimmed := name
	if strings.HasSuffix(name, "/") {
		if strings.HasSuffix(name, "//") {
			return "", fmt.Errorf("archive source: directory name %q has multiple trailing slashes", name)
		}
		trimmed = strings.TrimSuffix(name, "/")
	}
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return "", fmt.Errorf("archive source: invalid directory path %q", name)
	}
	if strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../") {
		return "", fmt.Errorf("archive source: non-canonical directory path %q", name)
	}
	if strings.Contains(trimmed, "/./") || strings.Contains(trimmed, "/../") || strings.HasSuffix(trimmed, "/.") || strings.HasSuffix(trimmed, "/..") {
		return "", fmt.Errorf("archive source: non-canonical directory path %q", name)
	}
	if path.Clean(trimmed) != trimmed {
		return "", fmt.Errorf("archive source: non-canonical directory path %q", name)
	}
	// Directory paths use the same bounds as asset paths (depth/bytes/segments).
	if !validAssetPath(trimmed) {
		return "", fmt.Errorf("archive source: invalid directory path %q", name)
	}
	return trimmed, nil
}

func catalogRequiredDirs(tc *trustedCatalog) map[string]struct{} {
	dirs := make(map[string]struct{})
	if tc == nil {
		return dirs
	}
	for _, e := range tc.Catalog.Entries {
		dir := path.Dir(e.Path)
		for dir != "." && dir != "/" && dir != "" {
			dirs[dir] = struct{}{}
			parent := path.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return dirs
}

func typeflagName(t byte) string {
	switch t {
	case tar.TypeReg:
		return "reg"
	case tar.TypeRegA:
		return "regA"
	case tar.TypeLink:
		return "hardlink"
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeChar:
		return "char"
	case tar.TypeBlock:
		return "block"
	case tar.TypeDir:
		return "dir"
	case tar.TypeFifo:
		return "fifo"
	case tar.TypeCont:
		return "cont"
	case tar.TypeXHeader:
		return "pax"
	case tar.TypeXGlobalHeader:
		return "pax-global"
	case tar.TypeGNUSparse:
		return "gnu-sparse"
	case tar.TypeGNULongName:
		return "gnu-longname"
	case tar.TypeGNULongLink:
		return "gnu-longlink"
	default:
		if t >= 32 && t < 127 {
			return string(t)
		}
		return fmt.Sprintf("0x%02x", t)
	}
}

// countCapReader counts bytes read and fails when the cap is exceeded.
// Reading is allowed up to cap inclusive; any byte past cap returns an error.
type countCapReader struct {
	r    io.Reader
	read int64
	cap  int64
}

func (c *countCapReader) Read(p []byte) (int, error) {
	if c.read > c.cap {
		return 0, fmt.Errorf("stream exceeds limit %d", c.cap)
	}
	// Permit reading at most (cap - read + 1) so we can detect overflow by one byte.
	remain := c.cap - c.read + 1
	if remain <= 0 {
		return 0, fmt.Errorf("stream exceeds limit %d", c.cap)
	}
	if int64(len(p)) > remain {
		p = p[:remain]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.cap {
		return n, fmt.Errorf("stream exceeds limit %d", c.cap)
	}
	return n, err
}
