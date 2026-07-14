package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/embyweb"

	"github.com/spf13/cobra"
)

// newWebCommand returns the pure `gateway web` subtree (init|install|status).
// Commands never bootstrap PocketBase and never expose registry injection.
func newWebCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Manage Emby Web asset installation",
	}
	cmd.AddCommand(newWebInitCommand())
	cmd.AddCommand(newWebInstallCommand())
	cmd.AddCommand(newWebStatusCommand())
	return cmd
}

type webInstallFlags struct {
	AssetsDir       string
	CatalogID       string
	FromDir         string
	FromArchive     string
	FromURL         string
	AllowHTTPURL    bool
	AllowPrivateURL bool
}

// webInitFlags is the structured facade for `gateway web init`.
// Source kind is an explicit enum; values are never shell-parsed.
type webInitFlags struct {
	AssetsDir       string
	CatalogID       string
	SourceKind      string
	Source          string
	AllowHTTPURL    bool
	AllowPrivateURL bool
}

func newWebInitCommand() *cobra.Command {
	var flags webInitFlags
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize Emby Web assets from a structured source kind (Compose-friendly)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := flags.toInstallOptions()
			if err != nil {
				return err
			}
			res, err := embyweb.Install(cmd.Context(), opts)
			if err != nil {
				return err
			}
			return writeInstallResult(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&flags.AssetsDir, "assets-dir", "", "Assets root directory (default: GATEWAY_WEB_ASSETS_DIR)")
	cmd.Flags().StringVar(&flags.CatalogID, "catalog-id", "", "Built-in trusted catalog ID")
	cmd.Flags().StringVar(&flags.SourceKind, "source-kind", "", "Source kind: dir, archive, or url")
	cmd.Flags().StringVar(&flags.Source, "source", "", "Source value (directory path, archive path, or base URL)")
	cmd.Flags().BoolVar(&flags.AllowHTTPURL, "allow-http-url", false, "Development: allow http:// URL bases (url kind only)")
	cmd.Flags().BoolVar(&flags.AllowPrivateURL, "allow-private-url", false, "Development: allow private/loopback URL destinations (url kind only)")
	return cmd
}

func (f webInitFlags) toInstallOptions() (embyweb.InstallOptions, error) {
	assets := strings.TrimSpace(f.AssetsDir)
	if assets == "" {
		assets = webAssetsDirFromEnv()
	}
	if assets == "" {
		return embyweb.InstallOptions{}, errors.New("--assets-dir is required (or set GATEWAY_WEB_ASSETS_DIR)")
	}
	catalogID := strings.TrimSpace(f.CatalogID)
	if catalogID == "" {
		return embyweb.InstallOptions{}, errors.New("--catalog-id is required")
	}
	kind := strings.TrimSpace(f.SourceKind)
	if kind == "" {
		return embyweb.InstallOptions{}, errors.New("--source-kind is required (dir|archive|url)")
	}
	// Source is used as a literal path/URL; do not shell-expand or split.
	source := strings.TrimSpace(f.Source)
	if source == "" {
		return embyweb.InstallOptions{}, errors.New("--source is required")
	}

	opts := embyweb.InstallOptions{
		AssetsRoot: assets,
		CatalogID:  catalogID,
	}
	switch kind {
	case "dir":
		if f.AllowHTTPURL || f.AllowPrivateURL {
			return embyweb.InstallOptions{}, errors.New("--allow-http-url/--allow-private-url are only valid with --source-kind=url")
		}
		opts.FromDir = source
	case "archive":
		if f.AllowHTTPURL || f.AllowPrivateURL {
			return embyweb.InstallOptions{}, errors.New("--allow-http-url/--allow-private-url are only valid with --source-kind=url")
		}
		opts.FromArchive = source
	case "url":
		opts.FromURL = source
		opts.AllowHTTPURL = f.AllowHTTPURL
		opts.AllowPrivateURL = f.AllowPrivateURL
	default:
		return embyweb.InstallOptions{}, fmt.Errorf("--source-kind must be dir, archive, or url (got %q)", kind)
	}
	return opts, nil
}

func newWebInstallCommand() *cobra.Command {
	var flags webInstallFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install a trusted Emby Web catalog release into the assets root",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := flags.toInstallOptions()
			if err != nil {
				return err
			}
			res, err := embyweb.Install(cmd.Context(), opts)
			if err != nil {
				return err
			}
			return writeInstallResult(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&flags.AssetsDir, "assets-dir", "", "Assets root directory (default: GATEWAY_WEB_ASSETS_DIR)")
	cmd.Flags().StringVar(&flags.CatalogID, "catalog-id", "", "Built-in trusted catalog ID")
	cmd.Flags().StringVar(&flags.FromDir, "from-dir", "", "Prepared raw files tree directory")
	cmd.Flags().StringVar(&flags.FromArchive, "from-archive", "", "Prepared .tar.gz archive path")
	cmd.Flags().StringVar(&flags.FromURL, "from-url", "", "Prepared static-tree base URL (trailing slash required)")
	cmd.Flags().BoolVar(&flags.AllowHTTPURL, "allow-http-url", false, "Development: allow http:// FromURL bases (FromURL only)")
	cmd.Flags().BoolVar(&flags.AllowPrivateURL, "allow-private-url", false, "Development: allow private/loopback FromURL destinations (FromURL only)")
	return cmd
}

func (f webInstallFlags) toInstallOptions() (embyweb.InstallOptions, error) {
	assets := strings.TrimSpace(f.AssetsDir)
	if assets == "" {
		assets = webAssetsDirFromEnv()
	}
	if assets == "" {
		return embyweb.InstallOptions{}, errors.New("--assets-dir is required (or set GATEWAY_WEB_ASSETS_DIR)")
	}
	catalogID := strings.TrimSpace(f.CatalogID)
	if catalogID == "" {
		return embyweb.InstallOptions{}, errors.New("--catalog-id is required")
	}

	dir := strings.TrimSpace(f.FromDir)
	archive := strings.TrimSpace(f.FromArchive)
	url := strings.TrimSpace(f.FromURL)
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
	if n != 1 {
		return embyweb.InstallOptions{}, errors.New("exactly one of --from-dir, --from-archive, or --from-url is required")
	}
	if url == "" && (f.AllowHTTPURL || f.AllowPrivateURL) {
		return embyweb.InstallOptions{}, errors.New("--allow-http-url/--allow-private-url are only valid with --from-url")
	}

	return embyweb.InstallOptions{
		AssetsRoot:      assets,
		CatalogID:       catalogID,
		FromDir:         dir,
		FromArchive:     archive,
		FromURL:         url,
		AllowHTTPURL:    f.AllowHTTPURL,
		AllowPrivateURL: f.AllowPrivateURL,
	}, nil
}

func writeInstallResult(w io.Writer, res embyweb.InstallResult) error {
	_, err := fmt.Fprintf(w, "status: installed\nrelease: %s\ncatalog_sha256: %s\nreactivated: %t\n",
		res.Release, res.CatalogSHA256, res.Reactivated)
	return err
}

type webStatusFlags struct {
	AssetsDir string
	Verify    bool
}

func newWebStatusCommand() *cobra.Command {
	var flags webStatusFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report Emby Web asset installation status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			assets := strings.TrimSpace(flags.AssetsDir)
			if assets == "" {
				assets = webAssetsDirFromEnv()
			}
			st := embyweb.InspectInstallation(assets, flags.Verify)
			if err := writeInstallationStatus(cmd.OutOrStdout(), st); err != nil {
				return err
			}
			return installationStatusExitError(st)
		},
	}
	cmd.Flags().StringVar(&flags.AssetsDir, "assets-dir", "", "Assets root directory (default: GATEWAY_WEB_ASSETS_DIR)")
	cmd.Flags().BoolVar(&flags.Verify, "verify", false, "Run full shared verifier (ready) instead of plain trusted identity")
	return cmd
}

func writeInstallationStatus(w io.Writer, st embyweb.InstallationStatus) error {
	// Deterministic key order for scripting and tests.
	if _, err := fmt.Fprintf(w, "state: %s\n", st.State); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "verified: %t\n", st.Verified); err != nil {
		return err
	}
	if st.Release != "" {
		if _, err := fmt.Fprintf(w, "release: %s\n", st.Release); err != nil {
			return err
		}
	}
	if st.CatalogSHA256 != "" {
		if _, err := fmt.Fprintf(w, "catalog_sha256: %s\n", st.CatalogSHA256); err != nil {
			return err
		}
	}
	if st.Err != nil {
		if _, err := fmt.Fprintf(w, "error: %v\n", st.Err); err != nil {
			return err
		}
	}
	return nil
}

// installationStatusExitError returns a non-nil error for non-success states so
// cobra exits nonzero. Plain mode succeeds on installed; verify mode on ready.
func installationStatusExitError(st embyweb.InstallationStatus) error {
	switch st.State {
	case embyweb.InstallStateReady:
		return nil
	case embyweb.InstallStateInstalled:
		return nil
	default:
		if st.Err != nil {
			return fmt.Errorf("web assets %s: %w", st.State, st.Err)
		}
		return fmt.Errorf("web assets %s", st.State)
	}
}
