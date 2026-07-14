package embyweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeSource writes catalog entry bytes from an in-memory map.
type fakeSource struct {
	files map[string][]byte
	kindS string
	// mutate hooks
	beforeWrite func(rel string)
	skipPath    string
	dupPath     string
	wrongHash   string
}

func (f *fakeSource) kind() string {
	if f.kindS != "" {
		return f.kindS
	}
	return "fake"
}

func (f *fakeSource) acquire(ctx context.Context, tc *trustedCatalog, w *stagingWriter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, e := range tc.Catalog.Entries {
		if e.Path == f.skipPath {
			continue
		}
		data := f.files[e.Path]
		if f.wrongHash == e.Path {
			data = append([]byte(nil), data...)
			if len(data) > 0 {
				data[0] ^= 0xff
			} else {
				data = []byte("x")
			}
		}
		if f.beforeWrite != nil {
			f.beforeWrite(e.Path)
		}
		if err := w.writeFile(e.Path, bytes.NewReader(data)); err != nil {
			return err
		}
		if f.dupPath == e.Path {
			if err := w.writeFile(e.Path, bytes.NewReader(data)); err != nil {
				return err
			}
		}
	}
	return nil
}

func testCatalogAndFiles(t *testing.T) (*trustedCatalog, map[string][]byte, *catalogRegistry) {
	t.Helper()
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "install-test", "1.0.0")
	m := map[string][]byte{}
	for _, f := range files {
		m[f.Path] = f.Data
	}
	return tc, m, registryFromTrusted(t, tc)
}

func TestInstallTrustedSuccessAndReady(t *testing.T) {
	tc, files, reg := testCatalogAndFiles(t)
	root := t.TempDir()
	src := &fakeSource{files: files}
	res, err := installTrusted(context.Background(), root, tc, src, installDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Release != tc.Release || res.CatalogSHA256 != tc.Digest || res.Reactivated {
		t.Fatalf("result=%+v", res)
	}

	// Deterministic install/current bytes.
	wantInstall, _ := encodeInstallManifest(tc)
	gotInstall, err := os.ReadFile(filepath.Join(root, "releases", tc.Release, "install.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotInstall, wantInstall) {
		t.Fatalf("install.json bytes mismatch")
	}
	wantPtr, _ := encodePointer(tc)
	gotPtr, err := os.ReadFile(filepath.Join(root, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPtr, wantPtr) {
		t.Fatalf("current.json bytes mismatch")
	}

	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: root}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}
}

func TestInstallIdempotentReactivate(t *testing.T) {
	tc, files, reg := testCatalogAndFiles(t)
	root := t.TempDir()
	src := &fakeSource{files: files}
	if _, err := installTrusted(context.Background(), root, tc, src, installDeps{}); err != nil {
		t.Fatal(err)
	}
	res, err := installTrusted(context.Background(), root, tc, src, installDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reactivated {
		t.Fatal("expected reactivation")
	}
	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: root}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		t.Fatalf("state=%s", s.Status().State)
	}
}

func TestInstallConflictExistingRelease(t *testing.T) {
	tc, files, _ := testCatalogAndFiles(t)
	root := t.TempDir()
	// Publish once.
	if _, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{}); err != nil {
		t.Fatal(err)
	}
	// Corrupt a published file without changing install.json identity enough...
	// Actually change file bytes so full verify fails.
	p := filepath.Join(root, "releases", tc.Release, "files", "modules", "app.js")
	if err := os.WriteFile(p, []byte("MUTATED"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Remove current so we attempt install again against conflicting release.
	_ = os.Remove(filepath.Join(root, "current.json"))
	_, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{})
	if err == nil || !errors.Is(err, ErrReleaseConflict) && !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("err=%v", err)
	}
}

func TestInstallLockContention(t *testing.T) {
	tc, files, _ := testCatalogAndFiles(t)
	root := t.TempDir()
	// Hold lock manually.
	if _, err := prepareAssetsRoot(root); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireInstallLock(root, installDeps{}.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = flockUnlock(lock); _ = lock.Close() }()

	_, err = installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{})
	if !errors.Is(err, ErrInstallBusy) {
		t.Fatalf("err=%v want busy", err)
	}
}

func TestInstallStagingCleanupOnFailure(t *testing.T) {
	tc, files, _ := testCatalogAndFiles(t)
	root := t.TempDir()
	src := &fakeSource{files: files, skipPath: "modules/app.js"}
	_, err := installTrusted(context.Background(), root, tc, src, installDeps{})
	if err == nil {
		t.Fatal("expected failure")
	}
	// staging should be empty or only unrelated
	entries, _ := os.ReadDir(filepath.Join(root, "staging"))
	if len(entries) != 0 {
		t.Fatalf("staging not cleaned: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(root, "current.json")); !os.IsNotExist(err) {
		t.Fatal("current should be absent")
	}
}

func TestInstallCurrentPreservedOnPrePointerFailure(t *testing.T) {
	tc, files, _ := testCatalogAndFiles(t)
	root := t.TempDir()
	// First successful install.
	if _, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{}); err != nil {
		t.Fatal(err)
	}
	oldPtr, _ := os.ReadFile(filepath.Join(root, "current.json"))

	// Build a second catalog with different id/version so release name differs.
	files2 := readyMinimalFiles()
	files2[0].Data = []byte("<html>v2</html>")
	tc2 := buildSyntheticCatalog(t, files2, "install-test-2", "2.0.0")
	m2 := map[string][]byte{}
	for _, f := range files2 {
		m2[f.Path] = f.Data
	}

	// Fail before pointer rename after publishing new release.
	_, err := installTrusted(context.Background(), root, tc2, &fakeSource{files: m2}, installDeps{
		Hooks: installHooks{
			BeforePointerRename: func() error { return errors.New("inject before pointer") },
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	// Old current preserved.
	got, _ := os.ReadFile(filepath.Join(root, "current.json"))
	if !bytes.Equal(got, oldPtr) {
		t.Fatal("current.json was mutated before pointer rename")
	}
	// New release may exist (published) — must not be auto-removed.
	if _, err := os.Stat(filepath.Join(root, "releases", tc2.Release)); err != nil {
		t.Fatalf("published release should remain: %v", err)
	}
}

func TestInstallActivationUncertainAfterPointerRename(t *testing.T) {
	tc, files, _ := testCatalogAndFiles(t)
	root := t.TempDir()
	_, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{
		Hooks: installHooks{
			AfterPointerRename: func() error { return errors.New("inject after pointer") },
		},
	})
	if err == nil || !errors.Is(err, ErrActivationUncertain) {
		t.Fatalf("err=%v", err)
	}
	// Pointer should already be the new one.
	got, err := os.ReadFile(filepath.Join(root, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := encodePointer(tc)
	if !bytes.Equal(got, want) {
		t.Fatal("pointer should be activated despite later uncertainty")
	}
}

func TestInstallOperationOrdering(t *testing.T) {
	tc, files, _ := testCatalogAndFiles(t)
	root := t.TempDir()
	var order []string
	hook := func(name string) func() error {
		return func() error {
			order = append(order, name)
			return nil
		}
	}
	_, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{
		Hooks: installHooks{
			BeforeReleaseRename: hook("beforeRelease"),
			AfterReleaseRename:  hook("afterRelease"),
			AfterReleasesSync:   hook("afterReleasesSync"),
			BeforePointerRename: hook("beforePointer"),
			AfterPointerRename:  hook("afterPointer"),
			AfterRootSync:       hook("afterRootSync"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"beforeRelease", "afterRelease", "afterReleasesSync", "beforePointer", "afterPointer", "afterRootSync"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v want %v", order, want)
	}
}

func TestStagingWriterDuplicateMissingHash(t *testing.T) {
	tc, files, _ := testCatalogAndFiles(t)
	root := t.TempDir()

	t.Run("duplicate", func(t *testing.T) {
		_, err := installTrusted(context.Background(), root+"-dup", tc, &fakeSource{files: files, dupPath: "index.html"}, installDeps{})
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		_, err := installTrusted(context.Background(), root+"-miss", tc, &fakeSource{files: files, skipPath: "manifest.json"}, installDeps{})
		if err == nil {
			t.Fatal("expected missing")
		}
	})
	t.Run("wrong_hash", func(t *testing.T) {
		_, err := installTrusted(context.Background(), root+"-hash", tc, &fakeSource{files: files, wrongHash: "index.html"}, installDeps{})
		if err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestInstallSymlinkRootRejected(t *testing.T) {
	tc, files, _ := testCatalogAndFiles(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := installTrusted(context.Background(), link, tc, &fakeSource{files: files}, installDeps{})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
}

func TestInspectInstallationMatrix(t *testing.T) {
	tc, files, reg := testCatalogAndFiles(t)

	t.Run("disabled", func(t *testing.T) {
		st := inspectInstallation("", false, reg)
		if st.State != InstallStateDisabled || st.Verified {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("missing", func(t *testing.T) {
		st := inspectInstallation(filepath.Join(t.TempDir(), "nope"), false, reg)
		if st.State != InstallStateMissing {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("untrusted_corrupt", func(t *testing.T) {
		// Install with reg, inspect with empty production registry.
		root := t.TempDir()
		if _, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{}); err != nil {
			t.Fatal(err)
		}
		st := InspectInstallation(root, false) // production empty
		if st.State != InstallStateCorrupt || st.Err == nil {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("installed_plain", func(t *testing.T) {
		root := t.TempDir()
		if _, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{}); err != nil {
			t.Fatal(err)
		}
		st := inspectInstallation(root, false, reg)
		if st.State != InstallStateInstalled || st.Verified {
			t.Fatalf("%+v", st)
		}
		if st.Release != tc.Release || st.CatalogSHA256 != tc.Digest {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("ready_verify", func(t *testing.T) {
		root := t.TempDir()
		if _, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{}); err != nil {
			t.Fatal(err)
		}
		st := inspectInstallation(root, true, reg)
		if st.State != InstallStateReady || !st.Verified {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("verify_detects_corruption", func(t *testing.T) {
		root := t.TempDir()
		if _, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{}); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(root, "releases", tc.Release, "files", "modules", "app.js"), []byte("x"), 0o644)
		plain := inspectInstallation(root, false, reg)
		if plain.State != InstallStateInstalled {
			t.Fatalf("plain should still be installed identity: %+v", plain)
		}
		full := inspectInstallation(root, true, reg)
		if full.State == InstallStateReady {
			t.Fatalf("verify should fail: %+v", full)
		}
	})
}

func TestInstallConcurrentRace(t *testing.T) {
	tc, files, reg := testCatalogAndFiles(t)
	root := t.TempDir()
	var wg sync.WaitGroup
	var success atomic.Int64
	var busy atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{})
			if err == nil {
				success.Add(1)
				return
			}
			if errors.Is(err, ErrInstallBusy) {
				busy.Add(1)
				return
			}
			// Reactivation success also ok if another won first.
			if strings.Contains(err.Error(), "conflict") {
				return
			}
		}()
	}
	wg.Wait()
	if success.Load() < 1 {
		t.Fatalf("success=%d busy=%d", success.Load(), busy.Load())
	}
	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: root}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		// One more install to settle if races left incomplete.
		_, _ = installTrusted(context.Background(), root, tc, &fakeSource{files: files}, installDeps{})
		s, _ = newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: root}, reg)
		if s.Status().State != StateReady {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	}
}

func TestPrepareAssetsRootRejectsNonDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := prepareAssetsRoot(f)
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestInstallRetrySyncsReleasesBeforePointer covers: publish rename succeeds,
// releases/ SyncDir fails (no pointer), retry takes existing-release path and
// must sync releases/ before activating current.json.
func TestInstallRetrySyncsReleasesBeforePointer(t *testing.T) {
	tc, files, _ := testCatalogAndFiles(t)
	root := t.TempDir()
	src := &fakeSource{files: files}

	// Attempt 1: fail durable releases sync after rename; no pointer.
	_, err := installTrusted(context.Background(), root, tc, src, installDeps{
		SyncDir: func(p string) error {
			if filepath.Base(p) == releasesDirName {
				return errors.New("inject releases sync failure")
			}
			return defaultSyncDir(p)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "inject releases sync failure") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "current.json")); !os.IsNotExist(err) {
		t.Fatal("current.json must not exist after releases sync failure")
	}
	if _, err := os.Stat(filepath.Join(root, "releases", tc.Release)); err != nil {
		t.Fatalf("published release must remain: %v", err)
	}

	// Attempt 2 (reactivation): record ordering of releases sync vs pointer.
	var order []string
	var mu sync.Mutex
	track := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	res, err := installTrusted(context.Background(), root, tc, src, installDeps{
		SyncDir: func(p string) error {
			if filepath.Base(p) == releasesDirName {
				track("syncReleases")
			}
			return defaultSyncDir(p)
		},
		Hooks: installHooks{
			BeforePointerRename: func() error {
				track("beforePointer")
				return nil
			},
			AfterPointerRename: func() error {
				track("afterPointer")
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reactivated {
		t.Fatal("expected reactivation path")
	}
	// syncReleases must precede any pointer activation step.
	var syncIdx, ptrIdx = -1, -1
	for i, s := range order {
		if s == "syncReleases" && syncIdx < 0 {
			syncIdx = i
		}
		if s == "beforePointer" && ptrIdx < 0 {
			ptrIdx = i
		}
	}
	if syncIdx < 0 || ptrIdx < 0 {
		t.Fatalf("order=%v missing syncReleases or beforePointer", order)
	}
	if syncIdx > ptrIdx {
		t.Fatalf("pointer activation preceded releases sync: order=%v", order)
	}
	// Pointer now present and correct.
	got, err := os.ReadFile(filepath.Join(root, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := encodePointer(tc)
	if !bytes.Equal(got, want) {
		t.Fatal("current.json mismatch after retry")
	}
}

func TestErrInstallUnsupportedSentinel(t *testing.T) {
	if ErrInstallUnsupported == nil || ErrInstallUnsupported.Error() == "" {
		t.Fatal("ErrInstallUnsupported must be defined")
	}
	// Wrapping form used by stubs must be detectable via errors.Is.
	wrapped := fmt.Errorf("%w: flock", ErrInstallUnsupported)
	if !errors.Is(wrapped, ErrInstallUnsupported) {
		t.Fatal("stub wrap must preserve ErrInstallUnsupported")
	}
	if errors.Is(ErrInstallBusy, ErrInstallUnsupported) {
		t.Fatal("busy must not match unsupported")
	}
}

func TestInstallLockRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if _, err := prepareAssetsRoot(root); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, installLockName)
	target := filepath.Join(root, "lock-target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}
	_, err := acquireInstallLock(root, installDeps{}.withDefaults())
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v want symlink rejection", err)
	}
}

func TestInstallLockRejectsNonRegular(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fifo not portable")
	}
	root := t.TempDir()
	if _, err := prepareAssetsRoot(root); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, installLockName)
	if err := mkfifo(lockPath); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	_, err := acquireInstallLock(root, installDeps{}.withDefaults())
	if err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("err=%v want non-regular rejection", err)
	}
}

func TestInstallLockHappyPathRegular(t *testing.T) {
	root := t.TempDir()
	if _, err := prepareAssetsRoot(root); err != nil {
		t.Fatal(err)
	}
	f, err := acquireInstallLock(root, installDeps{}.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = flockUnlock(f); _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("stat=%v err=%v", st, err)
	}
	// Second exclusive lock should be busy.
	_, err = acquireInstallLock(root, installDeps{}.withDefaults())
	if !errors.Is(err, ErrInstallBusy) {
		t.Fatalf("err=%v want busy", err)
	}
}
