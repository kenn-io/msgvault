package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

func newAddSynctechSMSDriveCmd() *cobra.Command {
	var opts struct {
		OwnerPhone      string
		FolderID        string
		GoogleAccount   string
		Schedule        string
		OAuthApp        string
		SkipAuthForTest bool
	}
	cmd := &cobra.Command{
		Use:   "add-synctech-sms-drive <name>",
		Short: "Configure a Google Drive SMS Backup & Restore source",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.OwnerPhone == "" {
				return errors.New("--owner-phone is required")
			}
			if opts.FolderID == "" {
				return errors.New("--folder-id is required")
			}
			if opts.GoogleAccount == "" {
				return errors.New("--google-account is required")
			}
			name := args[0]
			if cfg.GetSynctechSMSSource(name) != nil {
				return fmt.Errorf("synctech-sms source %q already exists", name)
			}
			// Complete OAuth before persisting the source so a failed or
			// cancelled browser flow does not leave a half-configured
			// source in the config that blocks a retry.
			if !opts.SkipAuthForTest {
				if err := ensureSynctechSMSDriveToken(cmd.Context(), opts.GoogleAccount, opts.OAuthApp); err != nil {
					return err
				}
			}
			cfg.SynctechSMS.Sources = append(cfg.SynctechSMS.Sources, config.SynctechSMSSource{
				Name:               name,
				Enabled:            true,
				Backend:            "drive",
				FolderID:           opts.FolderID,
				GoogleAccount:      opts.GoogleAccount,
				OwnerPhone:         opts.OwnerPhone,
				Schedule:           opts.Schedule,
				IncludeSMS:         true,
				IncludeMMS:         true,
				IncludeCalls:       true,
				IncludeAttachments: true,
				StableAfter:        "10m",
				OAuthApp:           opts.OAuthApp,
			})
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			if !opts.SkipAuthForTest {
				cmd.Println("Drive source configured.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.OwnerPhone, "owner-phone", "", "Owner phone number in E.164 format")
	cmd.Flags().StringVar(&opts.FolderID, "folder-id", "", "Google Drive folder ID")
	cmd.Flags().StringVar(&opts.GoogleAccount, "google-account", "", "Google account email used for Drive OAuth token lookup")
	cmd.Flags().StringVar(&opts.Schedule, "schedule", "30 4 * * *", "Cron schedule for Drive imports")
	cmd.Flags().StringVar(&opts.OAuthApp, "oauth-app", "", "Named OAuth app from config.toml")
	cmd.Flags().BoolVar(&opts.SkipAuthForTest, "skip-auth-for-test", false, "Skip OAuth setup in tests")
	_ = cmd.Flags().MarkHidden("skip-auth-for-test")
	return cmd
}

func newSyncSynctechSMSCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync-synctech-sms <name>",
		Short: "Run one configured synctech-sms source now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := cfg.GetSynctechSMSSource(args[0])
			if src == nil {
				return fmt.Errorf("synctech-sms source %q not found", args[0])
			}
			return runConfiguredSynctechSMSSource(cmd.Context(), *src)
		},
	}
}

func runConfiguredSynctechSMSSource(ctx context.Context, src config.SynctechSMSSource) error {
	st, err := openStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	opts := synctechImportOptions(src)
	switch src.Backend {
	case "", "local":
		if src.Path == "" {
			return fmt.Errorf("synctech-sms source %q path is required for local backend", src.Name)
		}
		_, err := synctechsms.NewImporter(st, opts).ImportPath(src.Path)
		return err
	case "drive":
		return runSynctechSMSDriveSource(ctx, st, src, opts)
	default:
		return fmt.Errorf("unsupported synctech-sms backend %q", src.Backend)
	}
}

func runSynctechSMSDriveSource(ctx context.Context, st *store.Store, src config.SynctechSMSSource, opts synctechsms.ImportOptions) error {
	if src.GoogleAccount == "" {
		return fmt.Errorf("synctech-sms source %q google_account is required", src.Name)
	}
	if src.FolderID == "" {
		return fmt.Errorf("synctech-sms source %q folder_id is required", src.Name)
	}
	client, err := newSynctechSMSDriveClient(ctx, src)
	if err != nil {
		return err
	}
	source, err := st.GetOrCreateSource(synctechsms.SourceType, src.OwnerPhone)
	if err != nil {
		return fmt.Errorf("get source: %w", err)
	}
	files, err := client.ListBackupFiles(ctx, src.FolderID)
	if err != nil {
		return err
	}
	imported, err := st.ListImportedSourceItemChecksums(source.ID, "drive")
	if err != nil {
		return err
	}
	stableAfter, err := time.ParseDuration(src.StableAfter)
	if err != nil {
		return fmt.Errorf("parse stable_after: %w", err)
	}
	selected := synctechsms.SelectStableDriveFiles(files, time.Now(), stableAfter, imported)
	stagingDir := filepath.Join(cfg.Data.DataDir, "imports", "synctech-sms", src.Name)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	imp := synctechsms.NewImporter(st, opts)
	for _, file := range selected {
		if err := importOneDriveBackup(ctx, st, imp, client, source.ID, file, stagingDir); err != nil {
			return err
		}
	}
	return nil
}

func importOneDriveBackup(ctx context.Context, st *store.Store, imp *synctechsms.Importer, client synctechsms.DriveClient, sourceID int64, file synctechsms.DriveFile, stagingDir string) error {
	staged := filepath.Join(stagingDir, file.ID+"-"+filepath.Base(file.Name))
	// Defer cleanup before the download starts so a partial file from a
	// failed DownloadToFile is removed too, not just successful imports.
	// Scoping this to a per-file helper keeps defers from piling up
	// across many files in a single scheduled run.
	defer func() { _ = os.Remove(staged) }()

	item := store.SourceImportItem{
		SourceID:   sourceID,
		Provider:   "drive",
		ProviderID: file.ID,
		Name:       file.Name,
		Checksum:   file.Checksum,
		Size:       file.Size,
		ModifiedAt: sql.NullTime{Time: file.ModifiedTime, Valid: !file.ModifiedTime.IsZero()},
		Status:     "pending",
	}
	if err := st.UpsertSourceImportItem(item); err != nil {
		return err
	}
	if err := client.DownloadToFile(ctx, file.ID, staged); err != nil {
		item.Status = "failed"
		item.ErrorMessage = sql.NullString{String: err.Error(), Valid: true}
		_ = st.UpsertSourceImportItem(item)
		return err
	}
	summary, err := imp.ImportPath(staged)
	if err != nil {
		item.Status = "failed"
		item.ErrorMessage = sql.NullString{String: err.Error(), Valid: true}
		_ = st.UpsertSourceImportItem(item)
		return err
	}
	item.Status = "imported"
	item.ImportedAt = sql.NullTime{Time: time.Now(), Valid: true}
	item.RecordsImported = summary.SMSImported + summary.MMSImported + summary.CallsImported
	item.ErrorMessage = sql.NullString{}
	return st.UpsertSourceImportItem(item)
}

func newSynctechSMSDriveClient(ctx context.Context, src config.SynctechSMSSource) (synctechsms.DriveClient, error) {
	clientSecrets, err := cfg.OAuth.ClientSecretsFor(src.OAuthApp)
	if err != nil {
		return nil, err
	}
	mgr, err := newSynctechSMSDriveOAuthManager(clientSecrets)
	if err != nil {
		return nil, err
	}
	if !mgr.HasToken(src.GoogleAccount) {
		return nil, fmt.Errorf("no Drive OAuth token for %s; run add-synctech-sms-drive on a machine with browser auth first", src.GoogleAccount)
	}
	ts, err := mgr.TokenSource(ctx, src.GoogleAccount)
	if err != nil {
		return nil, err
	}
	service, err := drive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("create Drive service: %w", err)
	}
	return synctechsms.NewGoogleDriveClient(service), nil
}

func ensureSynctechSMSDriveToken(ctx context.Context, googleAccount, oauthApp string) error {
	clientSecrets, err := cfg.OAuth.ClientSecretsFor(oauthApp)
	if err != nil {
		return err
	}
	mgr, err := newSynctechSMSDriveOAuthManager(clientSecrets)
	if err != nil {
		return err
	}
	if mgr.HasToken(googleAccount) {
		return nil
	}
	return mgr.Authorize(ctx, googleAccount)
}

func newSynctechSMSDriveOAuthManager(clientSecrets string) (*oauth.Manager, error) {
	// The current OAuth manager validates account identity through Gmail's
	// profile endpoint, so request a read-only Gmail scope alongside Drive.
	return oauth.NewManagerWithScopes(clientSecrets, cfg.TokensDir(), logger, []string{
		drive.DriveReadonlyScope,
		"https://www.googleapis.com/auth/gmail.readonly",
	})
}

func synctechImportOptions(src config.SynctechSMSSource) synctechsms.ImportOptions {
	return synctechsms.ImportOptions{
		OwnerPhone:         src.OwnerPhone,
		AttachmentsDir:     cfg.AttachmentsDir(),
		IncludeSMS:         src.IncludeSMS,
		IncludeMMS:         src.IncludeMMS,
		IncludeCalls:       src.IncludeCalls,
		IncludeAttachments: src.IncludeAttachments,
	}
}

func init() {
	rootCmd.AddCommand(newAddSynctechSMSDriveCmd())
	rootCmd.AddCommand(newSyncSynctechSMSCmd())
}
