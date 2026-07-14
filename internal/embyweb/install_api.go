package embyweb

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// InstallOptions configures a production install from a built-in catalog ID and
// exactly one prepared source. There is no catalog-file, digest, or registry
// override field.
type InstallOptions struct {
	AssetsRoot string
	CatalogID  string

	// Exactly one of FromDir, FromArchive, or FromURL must be non-blank.
	FromDir     string
	FromArchive string
	FromURL     string

	// AllowHTTPURL and AllowPrivateURL are development flags for FromURL only.
	// They are rejected when any other source mode is selected.
	AllowHTTPURL    bool
	AllowPrivateURL bool
}

// InstallResult is the public outcome of a successful Install.
type InstallResult struct {
	Release       string
	CatalogSHA256 string
	Reactivated   bool
}

// Install installs a trusted catalog release into AssetsRoot using the empty
// production registry. Unknown CatalogID returns/wraps ErrCatalogLegalGate
// before any root creation, lock acquisition, source construction, or network.
func Install(ctx context.Context, opts InstallOptions) (InstallResult, error) {
	return installWithRegistry(ctx, opts, getProductionRegistry(), installDeps{}, urlSourceDeps{})
}

// installWithRegistry is the package-private test entry point. It resolves the
// catalog through reg before any filesystem or network side effects, constructs
// exactly one acquisition source, and publishes via installTrusted.
func installWithRegistry(
	ctx context.Context,
	opts InstallOptions,
	reg *catalogRegistry,
	ideps installDeps,
	udeps urlSourceDeps,
) (InstallResult, error) {
	var zero InstallResult

	if err := validateInstallOptions(opts); err != nil {
		return zero, err
	}
	if reg == nil {
		reg = getProductionRegistry()
	}

	// Catalog lookup before any side effects (root, lock, source, DNS/network).
	tc, err := reg.lookupByID(strings.TrimSpace(opts.CatalogID))
	if err != nil {
		// Preserve ErrCatalogLegalGate for errors.Is; wrap with ID for diagnostics.
		if errors.Is(err, ErrCatalogLegalGate) {
			return zero, fmt.Errorf("%w: catalog id %q", ErrCatalogLegalGate, strings.TrimSpace(opts.CatalogID))
		}
		return zero, err
	}

	if err := ctx.Err(); err != nil {
		return zero, err
	}

	src, err := newInstallSource(opts, udeps)
	if err != nil {
		return zero, err
	}

	res, err := installTrusted(ctx, strings.TrimSpace(opts.AssetsRoot), tc, src, ideps)
	if err != nil {
		return zero, err
	}
	return InstallResult{
		Release:       res.Release,
		CatalogSHA256: res.CatalogSHA256,
		Reactivated:   res.Reactivated,
	}, nil
}

// validateInstallOptions performs pure option validation with no I/O.
func validateInstallOptions(opts InstallOptions) error {
	if strings.TrimSpace(opts.AssetsRoot) == "" {
		return errors.New("install: assets root is required")
	}
	if strings.TrimSpace(opts.CatalogID) == "" {
		return errors.New("install: catalog id is required")
	}

	dir := strings.TrimSpace(opts.FromDir)
	archive := strings.TrimSpace(opts.FromArchive)
	url := strings.TrimSpace(opts.FromURL)
	n := 0
	if dir != "" {
		n++
	}
	if archive != "" {
		n++
	}
	if url != "" {
		n++
	}
	if n == 0 {
		return errors.New("install: exactly one of FromDir, FromArchive, or FromURL is required")
	}
	if n > 1 {
		return errors.New("install: exactly one of FromDir, FromArchive, or FromURL is required")
	}

	if url == "" {
		if opts.AllowHTTPURL || opts.AllowPrivateURL {
			return errors.New("install: AllowHTTPURL/AllowPrivateURL are only valid with FromURL")
		}
	}
	return nil
}

// newInstallSource constructs the single selected acquisition source.
// Call only after trusted catalog lookup.
func newInstallSource(opts InstallOptions, udeps urlSourceDeps) (acquisitionSource, error) {
	switch {
	case strings.TrimSpace(opts.FromDir) != "":
		return newDirectorySource(strings.TrimSpace(opts.FromDir))
	case strings.TrimSpace(opts.FromArchive) != "":
		return newArchiveSource(strings.TrimSpace(opts.FromArchive))
	case strings.TrimSpace(opts.FromURL) != "":
		return newURLSource(urlSourceSpec{
			BaseURL:      strings.TrimSpace(opts.FromURL),
			AllowHTTP:    opts.AllowHTTPURL,
			AllowPrivate: opts.AllowPrivateURL,
		}, udeps)
	default:
		// validateInstallOptions should have rejected this.
		return nil, errors.New("install: no source selected")
	}
}
