package embyweb

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

type currentPointer struct {
	Schema        int    `json:"schema"`
	Release       string `json:"release"`
	CatalogSHA256 string `json:"catalog_sha256"`
}

type installManifest struct {
	Schema        int            `json:"schema"`
	Release       string         `json:"release"`
	CatalogSHA256 string         `json:"catalog_sha256"`
	Entries       []installEntry `json:"entries"`
}

type installEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	MediaType  string `json:"media_type"`
	CacheClass string `json:"cache_class"`
}

// loadAssets loads and verifies the asset tree at absRoot.
// Missing required paths yield StateMissing; all other failures yield StateCorrupt.
func loadAssets(absRoot string) (Status, map[string]*asset) {
	// Assets root must exist as a real directory (not a symlink).
	fi, err := os.Lstat(absRoot)
	if err != nil {
		if isNotExist(err) {
			return Status{State: StateMissing, Err: fmt.Errorf("assets root missing: %w", err)}, nil
		}
		return Status{State: StateCorrupt, Err: fmt.Errorf("stat assets root: %w", err)}, nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return Status{State: StateCorrupt, Err: errors.New("assets root is a symlink")}, nil
	}
	if !fi.IsDir() {
		return Status{State: StateCorrupt, Err: errors.New("assets root is not a directory")}, nil
	}

	root, err := os.OpenRoot(absRoot)
	if err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("open assets root: %w", err)}, nil
	}
	defer root.Close()

	// current.json
	if st, err := lstatRegularOrMissing(root, "current.json"); err != nil {
		return mapLoadErr(err, "current.json"), nil
	} else if st == nil {
		return Status{State: StateMissing, Err: errors.New("current.json missing")}, nil
	}

	ptrData, err := readRootFile(root, "current.json", maxPointerBytes)
	if err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("read current.json: %w", err)}, nil
	}

	var ptr currentPointer
	if err := decodeStrictJSON(ptrData, &ptr); err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("parse current.json: %w", err)}, nil
	}
	if err := validatePointer(ptr); err != nil {
		return Status{State: StateCorrupt, Err: err}, nil
	}

	releaseRel := path.Join("releases", ptr.Release)
	if st, err := lstatDirOrMissing(root, "releases"); err != nil {
		return mapLoadErr(err, "releases"), nil
	} else if st == nil {
		return Status{State: StateMissing, Err: errors.New("releases directory missing"), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}

	if st, err := lstatDirOrMissing(root, releaseRel); err != nil {
		return mapLoadErrWithID(err, "release directory", ptr), nil
	} else if st == nil {
		return Status{State: StateMissing, Err: fmt.Errorf("release %q missing", ptr.Release), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}

	installRel := path.Join(releaseRel, "install.json")
	if st, err := lstatRegularOrMissing(root, installRel); err != nil {
		return mapLoadErrWithID(err, "install.json", ptr), nil
	} else if st == nil {
		return Status{State: StateMissing, Err: errors.New("install.json missing"), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}

	filesRel := path.Join(releaseRel, "files")
	if st, err := lstatDirOrMissing(root, filesRel); err != nil {
		return mapLoadErrWithID(err, "files directory", ptr), nil
	} else if st == nil {
		return Status{State: StateMissing, Err: errors.New("files directory missing"), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}

	manData, err := readRootFile(root, installRel, maxManifestBytes)
	if err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("read install.json: %w", err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}

	var man installManifest
	if err := decodeStrictJSON(manData, &man); err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("parse install.json: %w", err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}
	if err := validateManifest(man, ptr); err != nil {
		return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}

	// Open nested roots for release and files; reject symlinks via Lstat already done.
	filesRoot, err := root.OpenRoot(filesRel)
	if err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("open files root: %w", err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}
	defer filesRoot.Close()

	// Manifest-derived expected sets only (bounded by maxEntries / maxDirs).
	entryByPath, requiredDirs, err := expectedTreeFromManifest(man.Entries)
	if err != nil {
		return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}

	// Bounded walk: only fixed ReadDir batches + deletions from expected sets.
	// Unexpected/excess nodes => Corrupt; remaining expected after walk => Missing.
	if err := verifyExactTree(filesRoot, entryByPath, requiredDirs); err != nil {
		st := classifyTreeErr(err, ptr)
		return st, nil
	}

	assets := make(map[string]*asset, len(man.Entries))
	var total int64
	for _, e := range man.Entries {
		if e.Size < 0 || e.Size > maxFileBytes {
			return Status{State: StateCorrupt, Err: fmt.Errorf("entry %q size out of bounds: %d", e.Path, e.Size), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
		}
		total += e.Size
		if total > maxTotalBytes {
			return Status{State: StateCorrupt, Err: fmt.Errorf("total size exceeds %d bytes", maxTotalBytes), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
		}

		// Re-check path components are not symlinks while reading.
		if err := ensurePathComponentsSafe(filesRoot, e.Path); err != nil {
			if isNotExist(err) {
				return Status{State: StateMissing, Err: fmt.Errorf("path %q missing: %w", e.Path, err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
			}
			return Status{State: StateCorrupt, Err: fmt.Errorf("path %q: %w", e.Path, err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
		}

		data, sum, err := readVerifyFile(filesRoot, e.Path, e.Size, e.SHA256)
		if err != nil {
			if isNotExist(err) {
				return Status{State: StateMissing, Err: fmt.Errorf("file %q missing: %w", e.Path, err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
			}
			return Status{State: StateCorrupt, Err: fmt.Errorf("file %q: %w", e.Path, err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
		}

		cacheClass := e.CacheClass
		if forceRevalidate(e.Path, e.MediaType) {
			cacheClass = cacheRevalidate
		}

		assets[e.Path] = &asset{
			path:       e.Path,
			data:       data,
			sha256:     sum,
			mediaType:  e.MediaType,
			cacheClass: cacheClass,
			etag:       `"` + sum + `"`,
		}
	}

	return Status{
		State:         StateReady,
		Release:       ptr.Release,
		CatalogSHA256: ptr.CatalogSHA256,
	}, assets
}

// expectedTreeFromManifest builds bounded expected file and required-directory
// sets from the install entries. Allocation is O(entries), never O(disk).
func expectedTreeFromManifest(entries []installEntry) (files map[string]installEntry, dirs map[string]struct{}, err error) {
	files = make(map[string]installEntry, len(entries))
	dirs = make(map[string]struct{}, len(entries)+1)
	dirs["."] = struct{}{}
	for _, e := range entries {
		if _, dup := files[e.Path]; dup {
			return nil, nil, fmt.Errorf("duplicate entry path %q", e.Path)
		}
		files[e.Path] = e
		dir := path.Dir(e.Path)
		for dir != "." && dir != "/" && dir != "" {
			if _, ok := dirs[dir]; !ok {
				if len(dirs) >= maxDirs {
					return nil, nil, fmt.Errorf("required directory count exceeds maxDirs (%d)", maxDirs)
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
	return files, dirs, nil
}

// treeError distinguishes Missing (declared path absent) from Corrupt.
type treeError struct {
	missing bool
	msg     string
}

func (e *treeError) Error() string { return e.msg }

func errTreeMissing(format string, args ...any) error {
	return &treeError{missing: true, msg: fmt.Sprintf(format, args...)}
}

func errTreeCorrupt(format string, args ...any) error {
	return &treeError{missing: false, msg: fmt.Sprintf(format, args...)}
}

func classifyTreeErr(err error, ptr currentPointer) Status {
	var te *treeError
	if errors.As(err, &te) && te.missing {
		return Status{State: StateMissing, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	if isNotExist(err) {
		return Status{State: StateMissing, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
}

// verifyExactTree walks files/ using only fixed-size ReadDir batches and the
// manifest-derived expected sets. It mutates remainingFiles/remainingDirs by
// deleting observed expected nodes. Unexpected or excess nodes are Corrupt;
// leftover expected nodes after a complete walk are Missing.
func verifyExactTree(filesRoot *os.Root, remainingFiles map[string]installEntry, remainingDirs map[string]struct{}) error {
	// remainingDirs includes "."; track observed required dirs separately so we
	// can detect missing parent directories after the walk.
	expectedDirCount := len(remainingDirs)
	expectedFileCount := len(remainingFiles)
	// Hard cap on nodes visited: expected files + expected dirs + 1 (stop early
	// on excess without scanning an attacker tree).
	maxNodes := expectedFileCount + expectedDirCount
	var nodesSeen int

	var walk func(rel string) error
	walk = func(rel string) error {
		fi, err := filesRoot.Lstat(rel)
		if err != nil {
			if isNotExist(err) {
				return errTreeMissing("directory %q missing", rel)
			}
			return errTreeCorrupt("stat %q: %v", rel, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errTreeCorrupt("%q is a symlink", rel)
		}
		if !fi.IsDir() {
			return errTreeCorrupt("%q is not a directory", rel)
		}
		if _, ok := remainingDirs[rel]; !ok && rel != "." {
			return errTreeCorrupt("undeclared directory %q", rel)
		}
		delete(remainingDirs, rel)
		nodesSeen++
		if nodesSeen > maxNodes {
			return errTreeCorrupt("files tree exceeds expected node count %d", maxNodes)
		}

		f, err := filesRoot.Open(rel)
		if err != nil {
			if isNotExist(err) {
				return errTreeMissing("directory %q missing", rel)
			}
			return errTreeCorrupt("open %q: %v", rel, err)
		}
		defer f.Close()

		for {
			batch, err := f.ReadDir(readDirBatch)
			if err != nil && err != io.EOF {
				return errTreeCorrupt("readdir %q: %v", rel, err)
			}
			if len(batch) == 0 {
				break
			}
			for _, ent := range batch {
				child := ent.Name()
				if child == "" || strings.ContainsAny(child, `/\`+"\x00") {
					return errTreeCorrupt("unsafe directory entry name %q under %q", child, rel)
				}
				childRel := child
				if rel != "." {
					childRel = path.Join(rel, child)
				}
				if !fs.ValidPath(childRel) || strings.ContainsAny(childRel, `\`+"\x00") || path.Clean(childRel) != childRel {
					return errTreeCorrupt("non-canonical path %q", childRel)
				}
				if len(childRel) > maxPathBytes {
					return errTreeCorrupt("path %q exceeds maxPathBytes", childRel)
				}

				cfi, err := filesRoot.Lstat(childRel)
				if err != nil {
					return errTreeCorrupt("stat %q: %v", childRel, err)
				}
				mode := cfi.Mode()
				if mode&os.ModeSymlink != 0 {
					return errTreeCorrupt("%q is a symlink", childRel)
				}
				switch {
				case mode.IsRegular():
					if !validAssetPath(childRel) {
						return errTreeCorrupt("non-canonical file path %q", childRel)
					}
					if _, ok := remainingFiles[childRel]; !ok {
						return errTreeCorrupt("undeclared file %q", childRel)
					}
					delete(remainingFiles, childRel)
					nodesSeen++
					if nodesSeen > maxNodes {
						return errTreeCorrupt("files tree exceeds expected node count %d", maxNodes)
					}
				case cfi.IsDir():
					// Directory segments must not be . or ..
					for _, seg := range strings.Split(childRel, "/") {
						if seg == "" || seg == "." || seg == ".." {
							return errTreeCorrupt("invalid directory segment in %q", childRel)
						}
					}
					if _, ok := remainingDirs[childRel]; !ok {
						// Not required by manifest: undeclared (including empty extras).
						return errTreeCorrupt("undeclared directory %q", childRel)
					}
					if err := walk(childRel); err != nil {
						return err
					}
				default:
					return errTreeCorrupt("%q is not a regular file or directory", childRel)
				}
			}
			if err == io.EOF {
				break
			}
		}
		return nil
	}

	if err := walk("."); err != nil {
		return err
	}
	// Any remaining expected files or dirs were not observed on disk.
	if len(remainingFiles) > 0 {
		for p := range remainingFiles {
			return errTreeMissing("declared file missing on disk: %q", p)
		}
	}
	if len(remainingDirs) > 0 {
		for d := range remainingDirs {
			return errTreeMissing("declared directory missing on disk: %q", d)
		}
	}
	return nil
}

func validatePointer(ptr currentPointer) error {
	if ptr.Schema != SchemaVersion {
		return fmt.Errorf("current.json schema %d unsupported (want %d)", ptr.Schema, SchemaVersion)
	}
	if !validReleaseBasename(ptr.Release) {
		return fmt.Errorf("current.json release %q is not a strict basename", ptr.Release)
	}
	if !validSHA256Hex(ptr.CatalogSHA256) {
		return fmt.Errorf("current.json catalog_sha256 is not lowercase 64-hex")
	}
	return nil
}

func validateManifest(man installManifest, ptr currentPointer) error {
	if man.Schema != SchemaVersion {
		return fmt.Errorf("install.json schema %d unsupported (want %d)", man.Schema, SchemaVersion)
	}
	if man.Release != ptr.Release {
		return fmt.Errorf("install.json release %q != current.json release %q", man.Release, ptr.Release)
	}
	if man.CatalogSHA256 != ptr.CatalogSHA256 {
		return fmt.Errorf("install.json catalog_sha256 mismatch with current.json")
	}
	if !validReleaseBasename(man.Release) {
		return fmt.Errorf("install.json release %q is not a strict basename", man.Release)
	}
	if !validSHA256Hex(man.CatalogSHA256) {
		return fmt.Errorf("install.json catalog_sha256 is not lowercase 64-hex")
	}
	if man.Entries == nil {
		return errors.New("install.json entries must be present")
	}
	if len(man.Entries) > maxEntries {
		return fmt.Errorf("install.json has %d entries (max %d)", len(man.Entries), maxEntries)
	}
	seen := make(map[string]struct{}, len(man.Entries))
	for i, e := range man.Entries {
		if !validAssetPath(e.Path) {
			return fmt.Errorf("entry[%d] path %q is not a canonical relative path", i, e.Path)
		}
		if _, dup := seen[e.Path]; dup {
			return fmt.Errorf("duplicate entry path %q", e.Path)
		}
		seen[e.Path] = struct{}{}
		if e.Size < 0 || e.Size > maxFileBytes {
			return fmt.Errorf("entry %q size out of bounds: %d", e.Path, e.Size)
		}
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
	}
	// Ready requires all three app.emby.media canaries to be declared (and later
	// verified on disk). A configured install missing any canary is corrupt, not
	// missing: install.json is present but internally invalid.
	for _, c := range canaryRelativePaths {
		if _, ok := seen[c]; !ok {
			return fmt.Errorf("install.json missing required canary %q", c)
		}
	}
	return nil
}

func mapLoadErr(err error, what string) Status {
	if isNotExist(err) {
		return Status{State: StateMissing, Err: fmt.Errorf("%s missing: %w", what, err)}
	}
	return Status{State: StateCorrupt, Err: fmt.Errorf("%s: %w", what, err)}
}

func mapLoadErrWithID(err error, what string, ptr currentPointer) Status {
	st := mapLoadErr(err, what)
	st.Release = ptr.Release
	st.CatalogSHA256 = ptr.CatalogSHA256
	return st
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist)
}

// lstatRegularOrMissing returns (nil, nil) when missing, (info, nil) when a
// regular file, or an error for symlink/non-regular/other failures.
func lstatRegularOrMissing(root *os.Root, name string) (fs.FileInfo, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("symlink not allowed")
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return fi, nil
}

func lstatDirOrMissing(root *os.Root, name string) (fs.FileInfo, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("symlink not allowed")
	}
	if !fi.IsDir() {
		return nil, errors.New("not a directory")
	}
	return fi, nil
}

func readRootFile(root *os.Root, name string, limit int64) ([]byte, error) {
	// Lstat again immediately before open.
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("symlink not allowed")
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("size %d exceeds limit %d", fi.Size(), limit)
	}

	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Confirm still regular after open (best-effort).
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file after open")
	}
	if st.Size() > limit {
		return nil, fmt.Errorf("size %d exceeds limit %d", st.Size(), limit)
	}

	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("size exceeds limit %d", limit)
	}
	return data, nil
}

func ensurePathComponentsSafe(filesRoot *os.Root, assetPath string) error {
	// Check each directory component and the final file with Lstat.
	parts := strings.Split(assetPath, "/")
	cur := ""
	for i, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur = path.Join(cur, p)
		}
		fi, err := filesRoot.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink at %q", cur)
		}
		if i < len(parts)-1 {
			if !fi.IsDir() {
				return fmt.Errorf("%q is not a directory", cur)
			}
		} else {
			if !fi.Mode().IsRegular() {
				return fmt.Errorf("%q is not a regular file", cur)
			}
		}
	}
	return nil
}

func readVerifyFile(filesRoot *os.Root, assetPath string, wantSize int64, wantSHA string) ([]byte, string, error) {
	fi, err := filesRoot.Lstat(assetPath)
	if err != nil {
		return nil, "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, "", errors.New("symlink not allowed")
	}
	if !fi.Mode().IsRegular() {
		return nil, "", errors.New("not a regular file")
	}
	if fi.Size() != wantSize {
		return nil, "", fmt.Errorf("size %d != declared %d", fi.Size(), wantSize)
	}
	if wantSize > maxFileBytes {
		return nil, "", fmt.Errorf("size %d exceeds per-file limit", wantSize)
	}

	f, err := filesRoot.Open(assetPath)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !st.Mode().IsRegular() {
		return nil, "", errors.New("not a regular file after open")
	}
	if st.Size() != wantSize {
		return nil, "", fmt.Errorf("size %d != declared %d after open", st.Size(), wantSize)
	}

	h := sha256.New()
	// Read exactly wantSize bytes; any extra is an error.
	limited := io.LimitReader(f, wantSize+1)
	data, err := io.ReadAll(io.TeeReader(limited, h))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) != wantSize {
		return nil, "", fmt.Errorf("read %d bytes, declared %d", len(data), wantSize)
	}
	// Ensure no trailing bytes remain.
	var extra [1]byte
	n, err := f.Read(extra[:])
	if n > 0 {
		return nil, "", errors.New("file longer than declared size")
	}
	if err != nil && err != io.EOF {
		return nil, "", err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	if sum != wantSHA {
		return nil, "", fmt.Errorf("sha256 mismatch")
	}
	// Pin a fresh immutable copy (data is already ours).
	return data, sum, nil
}
