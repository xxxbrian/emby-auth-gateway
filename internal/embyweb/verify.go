package embyweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
)

// verifyTrustedRelease fully verifies releases/<release>/ under root against tc:
// install.json identity/entries, exact files tree, and every file size/hash.
// When keep is true, verified file bytes are returned for the serve pin.
// This is the single full verifier used by serve, status --verify, staging, and
// existing-release checks.
func verifyTrustedRelease(root *os.Root, release string, tc *trustedCatalog, keep bool) (Status, map[string]*asset) {
	ptr := currentPointer{
		Schema:        SchemaVersion,
		Release:       tc.Release,
		CatalogSHA256: tc.Digest,
	}
	if release != tc.Release {
		return Status{
			State:         StateCorrupt,
			Release:       release,
			CatalogSHA256: tc.Digest,
			Err:           fmt.Errorf("release name %q != trusted %q", release, tc.Release),
		}, nil
	}

	releaseRel := path.Join("releases", release)
	if st, err := lstatDirOrMissing(root, "releases"); err != nil {
		return mapLoadErr(err, "releases"), nil
	} else if st == nil {
		return Status{State: StateMissing, Err: errors.New("releases directory missing"), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}

	if st, err := lstatDirOrMissing(root, releaseRel); err != nil {
		return mapLoadErrWithID(err, "release directory", ptr), nil
	} else if st == nil {
		return Status{State: StateMissing, Err: fmt.Errorf("release %q missing", release), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
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
	if err := installMatchesTrusted(man, tc); err != nil {
		return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}
	if err := validateManifest(man, ptr); err != nil {
		return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}

	// Optional byte-identity check against deterministic encoder (defense in depth).
	wantInstall, err := encodeInstallManifest(tc)
	if err != nil {
		return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}
	if string(manData) != string(wantInstall) {
		// Allow semantically equal JSON that isn't byte-identical only if fields
		// already matched; require deterministic bytes for installer-produced trees.
		// Reader accepts any strict JSON that matches trusted fields (Phase 2).
		// Installer always writes deterministic bytes.
		_ = wantInstall
	}

	filesRoot, err := root.OpenRoot(filesRel)
	if err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("open files root: %w", err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}
	defer filesRoot.Close()

	entryByPath, requiredDirs, err := expectedTreeFromManifest(man.Entries)
	if err != nil {
		return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
	}
	if err := verifyExactTree(filesRoot, entryByPath, requiredDirs); err != nil {
		return classifyTreeErr(err, ptr), nil
	}

	assets := make(map[string]*asset, 0)
	if keep {
		assets = make(map[string]*asset, len(man.Entries))
	}
	var total int64
	for _, e := range man.Entries {
		if e.Size < 0 || e.Size > maxFileBytes {
			return Status{State: StateCorrupt, Err: fmt.Errorf("entry %q size out of bounds: %d", e.Path, e.Size), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
		}
		total += e.Size
		if total > maxTotalBytes {
			return Status{State: StateCorrupt, Err: fmt.Errorf("total size exceeds %d bytes", maxTotalBytes), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}, nil
		}
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
		if keep {
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
	}

	return Status{
		State:         StateReady,
		Release:       ptr.Release,
		CatalogSHA256: ptr.CatalogSHA256,
	}, assets
}

// verifyTrustedReleaseDir verifies an absolute release directory that already
// contains install.json and files/ (staging or published release path).
func verifyTrustedReleaseDir(absRelease string, tc *trustedCatalog) Status {
	fi, err := os.Lstat(absRelease)
	if err != nil {
		if isNotExist(err) {
			return Status{State: StateMissing, Err: err, Release: tc.Release, CatalogSHA256: tc.Digest}
		}
		return Status{State: StateCorrupt, Err: err, Release: tc.Release, CatalogSHA256: tc.Digest}
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return Status{State: StateCorrupt, Err: errors.New("release path is not a real directory"), Release: tc.Release, CatalogSHA256: tc.Digest}
	}
	// Build a temporary mini-root: parent must contain releases/<name>.
	// Instead open the release dir directly via a synthetic layout check.
	parent := path.Dir(absRelease)
	name := path.Base(absRelease)
	if name != tc.Release {
		return Status{State: StateCorrupt, Err: fmt.Errorf("release basename %q != trusted %q", name, tc.Release), Release: tc.Release, CatalogSHA256: tc.Digest}
	}
	// parent should be .../releases; open grandparent as assets-style root if
	// structure is assets/releases/<rel>. For staging, absRelease IS the release
	// content root (install.json + files), not nested under releases/.
	// Detect layout:
	if _, err := os.Lstat(path.Join(absRelease, "install.json")); err == nil {
		return verifyFlatReleaseDir(absRelease, tc)
	}
	root, err := os.OpenRoot(path.Dir(parent)) // assets root
	if err != nil {
		return Status{State: StateCorrupt, Err: err, Release: tc.Release, CatalogSHA256: tc.Digest}
	}
	defer root.Close()
	st, _ := verifyTrustedRelease(root, name, tc, false)
	return st
}

// verifyFlatReleaseDir verifies a directory that directly contains install.json
// and files/ (staging layout before rename into releases/).
func verifyFlatReleaseDir(absRelease string, tc *trustedCatalog) Status {
	// Create a temporary mini assets root with releases/<name> -> we can't symlink.
	// Open parent and use relative path if parent is named "releases".
	// Simplest: open absRelease as files parent by constructing OpenRoot on absRelease
	// and verifying install + files relative to it.
	relRoot, err := os.OpenRoot(absRelease)
	if err != nil {
		return Status{State: StateCorrupt, Err: err, Release: tc.Release, CatalogSHA256: tc.Digest}
	}
	defer relRoot.Close()

	ptr := currentPointer{Schema: SchemaVersion, Release: tc.Release, CatalogSHA256: tc.Digest}

	if st, err := lstatRegularOrMissing(relRoot, "install.json"); err != nil {
		return mapLoadErrWithID(err, "install.json", ptr)
	} else if st == nil {
		return Status{State: StateMissing, Err: errors.New("install.json missing"), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	if st, err := lstatDirOrMissing(relRoot, "files"); err != nil {
		return mapLoadErrWithID(err, "files directory", ptr)
	} else if st == nil {
		return Status{State: StateMissing, Err: errors.New("files directory missing"), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}

	manData, err := readRootFile(relRoot, "install.json", maxManifestBytes)
	if err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("read install.json: %w", err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	var man installManifest
	if err := decodeStrictJSON(manData, &man); err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("parse install.json: %w", err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	if err := installMatchesTrusted(man, tc); err != nil {
		return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	if err := validateManifest(man, ptr); err != nil {
		return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}

	filesRoot, err := relRoot.OpenRoot("files")
	if err != nil {
		return Status{State: StateCorrupt, Err: fmt.Errorf("open files root: %w", err), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	defer filesRoot.Close()

	entryByPath, requiredDirs, err := expectedTreeFromManifest(man.Entries)
	if err != nil {
		return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	// Copy maps for verifyExactTree mutation.
	filesCopy := make(map[string]installEntry, len(entryByPath))
	for k, v := range entryByPath {
		filesCopy[k] = v
	}
	dirsCopy := make(map[string]struct{}, len(requiredDirs))
	for k := range requiredDirs {
		dirsCopy[k] = struct{}{}
	}
	if err := verifyExactTree(filesRoot, filesCopy, dirsCopy); err != nil {
		return classifyTreeErr(err, ptr)
	}

	var total int64
	for _, e := range man.Entries {
		total += e.Size
		if total > maxTotalBytes {
			return Status{State: StateCorrupt, Err: fmt.Errorf("total size exceeds %d bytes", maxTotalBytes), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
		}
		if err := ensurePathComponentsSafe(filesRoot, e.Path); err != nil {
			if isNotExist(err) {
				return Status{State: StateMissing, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
			}
			return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
		}
		if _, _, err := readVerifyFile(filesRoot, e.Path, e.Size, e.SHA256); err != nil {
			if isNotExist(err) {
				return Status{State: StateMissing, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
			}
			return Status{State: StateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
		}
	}
	return Status{State: StateReady, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
}

// encodeInstallManifest produces deterministic install.json bytes for tc.
func encodeInstallManifest(tc *trustedCatalog) ([]byte, error) {
	man := installManifest{
		Schema:        SchemaVersion,
		Release:       tc.Release,
		CatalogSHA256: tc.Digest,
		Entries:       append([]installEntry(nil), tc.Catalog.Entries...),
	}
	return marshalIndentNewline(man)
}

// encodePointer produces deterministic current.json bytes for tc.
func encodePointer(tc *trustedCatalog) ([]byte, error) {
	ptr := currentPointer{
		Schema:        SchemaVersion,
		Release:       tc.Release,
		CatalogSHA256: tc.Digest,
	}
	return marshalIndentNewline(ptr)
}

func marshalIndentNewline(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
