package embyweb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Install / publication errors.
var (
	// ErrInstallBusy indicates another install holds the exclusive flock.
	ErrInstallBusy = errors.New("embyweb: install lock busy")
	// ErrActivationUncertain indicates the pointer may have been activated but a
	// later durability step failed; callers must not restore old pointer state.
	ErrActivationUncertain = errors.New("embyweb: activation durability uncertain")
	// ErrReleaseConflict indicates an existing release directory does not match
	// the trusted catalog.
	ErrReleaseConflict = errors.New("embyweb: existing release conflicts with catalog")
	// errReleaseExists is internal: no-replace rename found a target.
	errReleaseExists = errors.New("embyweb: release path exists")
	// ErrInstallUnsupported is returned on platforms without flock/no-replace.
	ErrInstallUnsupported = errors.New("embyweb: install unsupported on this platform")
)

const (
	installLockName = "install.lock"
	stagingDirName  = "staging"
	releasesDirName = "releases"
	dirMode         = 0o755
	fileMode        = 0o644
	lockMode        = 0o600
)

// installResult is the outcome of a successful installTrusted call.
type installResult struct {
	Release       string
	CatalogSHA256 string
	Reactivated   bool
}

// installHooks are package-private fault-injection points for tests.
// Production deps leave all hooks nil.
type installHooks struct {
	BeforeReleaseRename func() error
	AfterReleaseRename  func() error
	AfterReleasesSync   func() error
	BeforePointerRename func() error
	AfterPointerRename  func() error
	AfterRootSync       func() error
}

// installDeps holds package-private OS primitives. Nil fields use production defaults.
type installDeps struct {
	FlockExclusive  func(f *os.File) error
	FlockUnlock     func(f *os.File) error
	RenameNoReplace func(oldpath, newpath string) error
	SyncFile        func(f *os.File) error
	SyncDir         func(path string) error
	Hooks           installHooks
	// RandID optionally overrides staging id generation (tests).
	RandID func() (string, error)
}

func (d installDeps) withDefaults() installDeps {
	if d.FlockExclusive == nil {
		d.FlockExclusive = flockExclusiveNonblock
	}
	if d.FlockUnlock == nil {
		d.FlockUnlock = flockUnlock
	}
	if d.RenameNoReplace == nil {
		d.RenameNoReplace = renameNoReplace
	}
	if d.SyncFile == nil {
		d.SyncFile = defaultSyncFile
	}
	if d.SyncDir == nil {
		d.SyncDir = defaultSyncDir
	}
	if d.RandID == nil {
		d.RandID = randomStagingID
	}
	return d
}

func defaultSyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func randomStagingID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// installTrusted stages, verifies, and publishes a trusted catalog release from src.
// assetsRoot must be a non-blank absolute-cleanable path. Production callers pass
// zero installDeps; tests inject hooks/fakes via package-private deps only.
func installTrusted(ctx context.Context, assetsRoot string, tc *trustedCatalog, src acquisitionSource, deps installDeps) (installResult, error) {
	var zero installResult
	if tc == nil {
		return zero, errors.New("installTrusted: nil catalog")
	}
	if src == nil {
		return zero, errors.New("installTrusted: nil source")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	deps = deps.withDefaults()

	absRoot, err := prepareAssetsRoot(assetsRoot)
	if err != nil {
		return zero, err
	}

	lockFile, err := acquireInstallLock(absRoot, deps)
	if err != nil {
		return zero, err
	}
	defer func() { _ = deps.FlockUnlock(lockFile); _ = lockFile.Close() }()

	// Idempotent path: existing exact release => sync releases/ then reactivate.
	// Syncing before pointer activation ensures a prior post-rename SyncDir
	// failure cannot leave a durable pointer at a non-durable release.
	finalRelease := filepath.Join(absRoot, releasesDirName, tc.Release)
	if fi, err := os.Lstat(finalRelease); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return zero, fmt.Errorf("%w: %s is not a directory", ErrReleaseConflict, tc.Release)
		}
		st := verifyTrustedReleaseDir(finalRelease, tc)
		if st.State == StateReady {
			// Sync releases/ before pointer so a prior post-rename SyncDir failure
			// cannot leave a durable pointer at a non-durable release.
			if err := syncReleasesDir(absRoot, deps); err != nil {
				return zero, err
			}
			if err := activatePointer(absRoot, tc, deps); err != nil {
				return zero, err
			}
			return installResult{Release: tc.Release, CatalogSHA256: tc.Digest, Reactivated: true}, nil
		}
		return zero, fmt.Errorf("%w: %v", ErrReleaseConflict, st.Err)
	} else if !isNotExist(err) {
		return zero, err
	}

	// Stage under assetsRoot/staging/<id>/{install.json,files/...}.
	stageID, err := deps.RandID()
	if err != nil {
		return zero, err
	}
	stagePath := filepath.Join(absRoot, stagingDirName, stageID)
	if err := os.Mkdir(stagePath, dirMode); err != nil {
		return zero, fmt.Errorf("mkdir staging: %w", err)
	}
	stageOwned := true
	defer func() {
		if stageOwned {
			_ = os.RemoveAll(stagePath)
		}
	}()

	filesPath := filepath.Join(stagePath, "files")
	if err := os.Mkdir(filesPath, dirMode); err != nil {
		return zero, err
	}
	filesRoot, err := os.OpenRoot(filesPath)
	if err != nil {
		return zero, err
	}
	defer filesRoot.Close()

	writer, err := newStagingWriter(filesRoot, tc, deps.SyncFile)
	if err != nil {
		return zero, err
	}
	if err := src.acquire(ctx, tc, writer); err != nil {
		return zero, fmt.Errorf("acquire (%s): %w", src.kind(), err)
	}
	if err := writer.complete(); err != nil {
		return zero, err
	}

	installBytes, err := encodeInstallManifest(tc)
	if err != nil {
		return zero, err
	}
	if err := writeExclusiveFile(filepath.Join(stagePath, "install.json"), installBytes, deps.SyncFile); err != nil {
		return zero, err
	}

	// Sync staged files tree directories deepest-first, then stage root.
	if err := syncTreeDirs(stagePath, tc, deps.SyncDir); err != nil {
		return zero, err
	}

	if st := verifyFlatReleaseDir(stagePath, tc); st.State != StateReady {
		return zero, fmt.Errorf("staging verification failed: %w", st.Err)
	}

	// Publish release (irreversible boundary starts at successful rename).
	if deps.Hooks.BeforeReleaseRename != nil {
		if err := deps.Hooks.BeforeReleaseRename(); err != nil {
			return zero, err
		}
	}
	if err := deps.RenameNoReplace(stagePath, finalRelease); err != nil {
		if errors.Is(err, errReleaseExists) {
			// Concurrent publisher: verify, sync releases/, then maybe reactivate.
			st := verifyTrustedReleaseDir(finalRelease, tc)
			if st.State == StateReady {
				stageOwned = true // our staging still present; remove it
				if err := syncReleasesDir(absRoot, deps); err != nil {
					return zero, err
				}
				if err := activatePointer(absRoot, tc, deps); err != nil {
					return zero, err
				}
				return installResult{Release: tc.Release, CatalogSHA256: tc.Digest, Reactivated: true}, nil
			}
			return zero, fmt.Errorf("%w: %v", ErrReleaseConflict, st.Err)
		}
		return zero, fmt.Errorf("publish release: %w", err)
	}
	stageOwned = false // now owned as published release; never auto-remove
	if deps.Hooks.AfterReleaseRename != nil {
		if err := deps.Hooks.AfterReleaseRename(); err != nil {
			// Release published; do not remove it. Fail without pointer change.
			return zero, err
		}
	}

	// Sync releases/ before pointer activation so retries that take the
	// existing-release path also cannot activate without a successful sync.
	if err := syncReleasesDir(absRoot, deps); err != nil {
		return zero, err
	}

	if st := verifyTrustedReleaseDir(finalRelease, tc); st.State != StateReady {
		return zero, fmt.Errorf("published release verification failed: %w", st.Err)
	}

	if err := activatePointer(absRoot, tc, deps); err != nil {
		return zero, err
	}
	return installResult{Release: tc.Release, CatalogSHA256: tc.Digest, Reactivated: false}, nil
}

// syncReleasesDir durably syncs assetsRoot/releases and runs AfterReleasesSync.
// Callers must invoke this before every activatePointer that publishes or
// reactivates a release, including idempotent retries.
func syncReleasesDir(absRoot string, deps installDeps) error {
	releasesPath := filepath.Join(absRoot, releasesDirName)
	if err := deps.SyncDir(releasesPath); err != nil {
		return err
	}
	if deps.Hooks.AfterReleasesSync != nil {
		if err := deps.Hooks.AfterReleasesSync(); err != nil {
			return err
		}
	}
	return nil
}

// activatePointer writes deterministic current.json via temp+rename and syncs root.
// After the pointer rename succeeds, errors wrap ErrActivationUncertain.
func activatePointer(absRoot string, tc *trustedCatalog, deps installDeps) error {
	ptrBytes, err := encodePointer(tc)
	if err != nil {
		return err
	}
	tmpName := fmt.Sprintf("current.json.tmp-%s", tc.Digest[:16])
	tmpPath := filepath.Join(absRoot, tmpName)
	finalPath := filepath.Join(absRoot, "current.json")

	// Clean stale owned temp if present.
	_ = os.Remove(tmpPath)

	if deps.Hooks.BeforePointerRename != nil {
		if err := deps.Hooks.BeforePointerRename(); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
	}

	if err := writeExclusiveFile(tmpPath, ptrBytes, deps.SyncFile); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	// Ordinary same-directory rename over current.json (POSIX atomic replace).
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("activate pointer: %w", err)
	}
	// From here, never restore old pointer.
	if deps.Hooks.AfterPointerRename != nil {
		if err := deps.Hooks.AfterPointerRename(); err != nil {
			return fmt.Errorf("%w: %v", ErrActivationUncertain, err)
		}
	}
	if err := deps.SyncDir(absRoot); err != nil {
		return fmt.Errorf("%w: sync root: %v", ErrActivationUncertain, err)
	}
	if deps.Hooks.AfterRootSync != nil {
		if err := deps.Hooks.AfterRootSync(); err != nil {
			return fmt.Errorf("%w: %v", ErrActivationUncertain, err)
		}
	}
	return nil
}

func writeExclusiveFile(path string, data []byte, syncFile func(*os.File) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := syncFile(f); err != nil {
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

func syncTreeDirs(stagePath string, tc *trustedCatalog, syncDir func(string) error) error {
	// Collect dirs under files/ from catalog, deepest first.
	dirs := map[string]struct{}{filepath.Join(stagePath, "files"): {}, stagePath: {}}
	for _, e := range tc.Catalog.Entries {
		dir := path.Dir(e.Path)
		for dir != "." && dir != "/" && dir != "" {
			dirs[filepath.Join(stagePath, "files", filepath.FromSlash(dir))] = struct{}{}
			parent := path.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		ordered = append(ordered, d)
	}
	// Deepest first by path length.
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if len(ordered[j]) > len(ordered[i]) {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	for _, d := range ordered {
		if err := syncDir(d); err != nil {
			return fmt.Errorf("sync dir %s: %w", d, err)
		}
	}
	return nil
}

func prepareAssetsRoot(assetsRoot string) (string, error) {
	assetsRoot = strings.TrimSpace(assetsRoot)
	if assetsRoot == "" {
		return "", errors.New("assets root is empty")
	}
	abs, err := filepath.Abs(assetsRoot)
	if err != nil {
		return "", fmt.Errorf("resolve assets root: %w", err)
	}
	abs = filepath.Clean(abs)

	fi, err := os.Lstat(abs)
	if err != nil {
		if !isNotExist(err) {
			return "", err
		}
		// Create root as a real directory.
		if err := os.MkdirAll(abs, dirMode); err != nil {
			return "", fmt.Errorf("mkdir assets root: %w", err)
		}
		fi, err = os.Lstat(abs)
		if err != nil {
			return "", err
		}
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("assets root is a symlink")
	}
	if !fi.IsDir() {
		return "", errors.New("assets root is not a directory")
	}

	for _, sub := range []string{releasesDirName, stagingDirName} {
		p := filepath.Join(abs, sub)
		sfi, err := os.Lstat(p)
		if err != nil {
			if isNotExist(err) {
				if err := os.Mkdir(p, dirMode); err != nil {
					return "", fmt.Errorf("mkdir %s: %w", sub, err)
				}
				continue
			}
			return "", err
		}
		if sfi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symlink", sub)
		}
		if !sfi.IsDir() {
			return "", fmt.Errorf("%s is not a directory", sub)
		}
	}
	return abs, nil
}

func acquireInstallLock(absRoot string, deps installDeps) (*os.File, error) {
	lockPath := filepath.Join(absRoot, installLockName)
	f, err := openInstallLockFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("open install lock: %w", err)
	}
	if err := deps.FlockExclusive(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	// Never delete the lock file.
	return f, nil
}
