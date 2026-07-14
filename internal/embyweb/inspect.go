package embyweb

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// InstallationState is the inspect/status outcome for an assets root.
// Values are distinct from serve State names used in InstallationStatus.State strings.
const (
	InstallStateDisabled  = "disabled"
	InstallStateMissing   = "missing"
	InstallStateCorrupt   = "corrupt"
	InstallStateInstalled = "installed" // trusted identity only; not fully verified
	InstallStateReady     = "ready"     // full verifier passed
)

// InstallationStatus is the exported status facade for later CLI wiring.
// It does not conflict with the serve Status type.
type InstallationStatus struct {
	State         string
	Verified      bool
	Release       string
	CatalogSHA256 string
	Err           error
}

// InspectInstallation reports installation status for assetsRoot using the
// immutable production registry. blank root => disabled. verify selects plain
// trusted identity vs full verifier (ready).
func InspectInstallation(assetsRoot string, verify bool) InstallationStatus {
	return inspectInstallation(assetsRoot, verify, getProductionRegistry())
}

// inspectInstallation is the package-private registry-aware variant for tests.
func inspectInstallation(assetsRoot string, verify bool, reg *catalogRegistry) InstallationStatus {
	assetsRoot = strings.TrimSpace(assetsRoot)
	if assetsRoot == "" {
		return InstallationStatus{State: InstallStateDisabled}
	}
	if reg == nil {
		reg = getProductionRegistry()
	}

	abs, err := filepath.Abs(assetsRoot)
	if err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: err}
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		if isNotExist(err) {
			return InstallationStatus{State: InstallStateMissing, Err: err}
		}
		return InstallationStatus{State: InstallStateCorrupt, Err: err}
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return InstallationStatus{State: InstallStateCorrupt, Err: errors.New("assets root is a symlink")}
	}
	if !fi.IsDir() {
		return InstallationStatus{State: InstallStateCorrupt, Err: errors.New("assets root is not a directory")}
	}

	root, err := os.OpenRoot(abs)
	if err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: err}
	}
	defer root.Close()

	// Lightweight identity: pointer + install identity against registry.
	if st, err := lstatRegularOrMissing(root, "current.json"); err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: err}
	} else if st == nil {
		return InstallationStatus{State: InstallStateMissing, Err: errors.New("current.json missing")}
	}

	ptrData, err := readRootFile(root, "current.json", maxPointerBytes)
	if err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: fmt.Errorf("read current.json: %w", err)}
	}
	var ptr currentPointer
	if err := decodeStrictJSON(ptrData, &ptr); err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: fmt.Errorf("parse current.json: %w", err)}
	}
	if err := validatePointer(ptr); err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}

	tc, err := reg.lookupByDigest(ptr.CatalogSHA256)
	if err != nil {
		return InstallationStatus{
			State:         InstallStateCorrupt,
			Release:       ptr.Release,
			CatalogSHA256: ptr.CatalogSHA256,
			Err:           fmt.Errorf("%w: %s", ErrUntrustedCatalog, ptr.CatalogSHA256),
		}
	}
	if err := pointerMatchesTrusted(ptr, tc); err != nil {
		return InstallationStatus{
			State:         InstallStateCorrupt,
			Release:       ptr.Release,
			CatalogSHA256: ptr.CatalogSHA256,
			Err:           err,
		}
	}

	// Require install.json identity match (bounded, no full tree walk).
	installRel := path.Join("releases", ptr.Release, "install.json")
	if st, err := lstatRegularOrMissing(root, installRel); err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	} else if st == nil {
		return InstallationStatus{State: InstallStateMissing, Err: errors.New("install.json missing"), Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	manData, err := readRootFile(root, installRel, maxManifestBytes)
	if err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	var man installManifest
	if err := decodeStrictJSON(manData, &man); err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}
	if err := installMatchesTrusted(man, tc); err != nil {
		return InstallationStatus{State: InstallStateCorrupt, Err: err, Release: ptr.Release, CatalogSHA256: ptr.CatalogSHA256}
	}

	if !verify {
		return InstallationStatus{
			State:         InstallStateInstalled,
			Verified:      false,
			Release:       ptr.Release,
			CatalogSHA256: ptr.CatalogSHA256,
		}
	}

	// Full shared verifier (same as serve).
	full, _ := verifyTrustedRelease(root, ptr.Release, tc, false)
	switch full.State {
	case StateReady:
		return InstallationStatus{
			State:         InstallStateReady,
			Verified:      true,
			Release:       full.Release,
			CatalogSHA256: full.CatalogSHA256,
		}
	case StateMissing:
		return InstallationStatus{
			State:         InstallStateMissing,
			Verified:      false,
			Release:       full.Release,
			CatalogSHA256: full.CatalogSHA256,
			Err:           full.Err,
		}
	default:
		return InstallationStatus{
			State:         InstallStateCorrupt,
			Verified:      false,
			Release:       full.Release,
			CatalogSHA256: full.CatalogSHA256,
			Err:           full.Err,
		}
	}
}
