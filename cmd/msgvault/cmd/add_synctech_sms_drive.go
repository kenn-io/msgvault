package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
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
			if !isDaemonCLISubprocess() {
				// Complete OAuth in this process — which owns the user's
				// browser — before proxying; the daemon subprocess's
				// idempotent token check then skips the browser flow.
				if !opts.SkipAuthForTest && !IsRemoteMode() {
					if err := ensureSynctechSMSDriveToken(cmd.Context(), opts.GoogleAccount, opts.OAuthApp); err != nil {
						return err
					}
				}
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
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
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			src := cfg.GetSynctechSMSSource(args[0])
			if src == nil {
				return fmt.Errorf("synctech-sms source %q not found", args[0])
			}
			return runConfiguredSynctechSMSSource(cmd.Context(), *src)
		},
	}
}

func runConfiguredSynctechSMSSource(ctx context.Context, src config.SynctechSMSSource) error {
	st, cleanup, err := openWritableStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer cleanup()

	return runConfiguredSynctechSMSSourceWithStore(ctx, st, src)
}

func runConfiguredSynctechSMSSourceWithStore(ctx context.Context, st *store.Store, src config.SynctechSMSSource) error {
	return runConfiguredSynctechSMSSourceWithStoreDriveClient(ctx, st, src, nil)
}

func runConfiguredSynctechSMSSourceWithStoreDriveClient(ctx context.Context, st *store.Store, src config.SynctechSMSSource, driveClient synctechsms.DriveClient) error {
	opts := synctechImportOptions(src)
	if opts.OwnerPhone == "" {
		return fmt.Errorf("synctech-sms source %q owner_phone is required", src.Name)
	}
	var err error
	switch src.Backend {
	case "", "local":
		if src.Path == "" {
			return fmt.Errorf("synctech-sms source %q path is required for local backend", src.Name)
		}
		if _, err := ensureConfiguredSynctechSMSSource(st, src, opts); err != nil {
			return err
		}
		_, err = synctechsms.NewImporter(st, opts).ImportPath(src.Path)
	case "drive":
		if driveClient != nil {
			_, err = runSynctechSMSDriveSourceWithClient(ctx, st, src, opts, driveClient)
		} else {
			_, err = runSynctechSMSDriveSource(ctx, st, src, opts)
		}
	default:
		return fmt.Errorf("unsupported synctech-sms backend %q", src.Backend)
	}
	if err != nil {
		return err
	}
	rebuildCacheAfterScheduledSync(ctx, "synctech-sms:"+src.Name)
	return nil
}

func ensureConfiguredSynctechSMSSource(st *store.Store, src config.SynctechSMSSource, opts synctechsms.ImportOptions) (*store.Source, error) {
	if opts.OwnerPhone == "" {
		return nil, fmt.Errorf("synctech-sms source %q owner_phone is required", src.Name)
	}
	source, err := st.GetOrCreateSource(synctechsms.SourceType, opts.OwnerPhone)
	if err != nil {
		return nil, fmt.Errorf("get source: %w", err)
	}
	confirmDefaultIdentity(io.Discard, st, source.ID, src.Name, opts.OwnerPhone, "account-identifier")
	if err := runPostSourceCreateMigrations(st); err != nil {
		return nil, fmt.Errorf("post-source-create migrations: %w", err)
	}
	return source, nil
}

// validateSynctechSMSDriveSource checks the required Drive source fields.
// It runs before Drive client construction so a misconfigured source
// surfaces a clear config error instead of an OAuth/token failure.
func validateSynctechSMSDriveSource(src config.SynctechSMSSource) error {
	if src.GoogleAccount == "" {
		return fmt.Errorf("synctech-sms source %q google_account is required", src.Name)
	}
	if src.FolderID == "" {
		return fmt.Errorf("synctech-sms source %q folder_id is required", src.Name)
	}
	return nil
}

func runSynctechSMSDriveSource(ctx context.Context, st *store.Store, src config.SynctechSMSSource, opts synctechsms.ImportOptions) (synctechsms.ImportSummary, error) {
	if err := validateSynctechSMSDriveSource(src); err != nil {
		return synctechsms.ImportSummary{}, err
	}
	client, err := newSynctechSMSDriveClient(ctx, src)
	if err != nil {
		return synctechsms.ImportSummary{}, err
	}
	return runSynctechSMSDriveSourceWithClient(ctx, st, src, opts, client)
}

func runSynctechSMSDriveSourceWithClient(ctx context.Context, st *store.Store, src config.SynctechSMSSource, opts synctechsms.ImportOptions, client synctechsms.DriveClient) (summary synctechsms.ImportSummary, retErr error) {
	if err := validateSynctechSMSDriveSource(src); err != nil {
		return summary, err
	}
	source, err := ensureConfiguredSynctechSMSSource(st, src, opts)
	if err != nil {
		return summary, err
	}
	syncID, err := st.StartSync(source.ID, synctechsms.AdapterName)
	if err != nil {
		return summary, fmt.Errorf("start sync: %w", err)
	}
	completed := false
	defer func() {
		if !completed && retErr != nil {
			if failErr := st.FailSync(syncID, retErr.Error()); failErr != nil {
				logger.Error("failed to mark synctech-sms Drive sync failed",
					"source", src.Name,
					"sync_id", syncID,
					"error", failErr,
				)
			}
		}
	}()
	files, err := client.ListBackupFiles(ctx, src.FolderID)
	if err != nil {
		return summary, fmt.Errorf("list Drive backup files: %w", err)
	}
	imported, err := st.ListImportedSourceItemChecksums(source.ID, "drive")
	if err != nil {
		return summary, fmt.Errorf("list imported Drive checksums: %w", err)
	}
	stableAfter, err := time.ParseDuration(src.StableAfter)
	if err != nil {
		return summary, fmt.Errorf("parse stable_after: %w", err)
	}
	selected := synctechsms.SelectStableDriveFiles(files, time.Now(), stableAfter, imported)
	stagingDir := filepath.Join(cfg.Data.DataDir, "imports", "synctech-sms", src.Name)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return summary, fmt.Errorf("create staging directory: %w", err)
	}
	imp := synctechsms.NewImporter(st, opts)
	for _, file := range selected {
		fileSummary, err := importOneDriveBackup(ctx, st, imp, client, source.ID, file, stagingDir)
		summary.FilesSeen += fileSummary.FilesSeen
		summary.FilesImported += fileSummary.FilesImported
		summary.SMSImported += fileSummary.SMSImported
		summary.MMSImported += fileSummary.MMSImported
		summary.CallsImported += fileSummary.CallsImported
		summary.AttachmentsImported += fileSummary.AttachmentsImported
		summary.MessageIDs = append(summary.MessageIDs, fileSummary.MessageIDs...)
		if err != nil {
			return summary, err
		}
	}
	if summary.FilesImported > 0 {
		if err := st.RecomputeConversationStats(source.ID); err != nil {
			return summary, fmt.Errorf("recompute conversation stats: %w", err)
		}
	}
	totalRecords := int64(summary.SMSImported + summary.MMSImported + summary.CallsImported)
	if err := st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		MessagesProcessed: totalRecords,
		MessagesAdded:     totalRecords,
	}); err != nil {
		return summary, fmt.Errorf("update sync checkpoint: %w", err)
	}
	if err := st.TouchSourceLastSyncAt(source.ID); err != nil {
		return summary, fmt.Errorf("touch source last sync: %w", err)
	}
	if err := st.CompleteSync(syncID, ""); err != nil {
		return summary, fmt.Errorf("complete sync: %w", err)
	}
	completed = true
	return summary, nil
}

func importOneDriveBackup(ctx context.Context, st *store.Store, imp *synctechsms.Importer, client synctechsms.DriveClient, sourceID int64, file synctechsms.DriveFile, stagingDir string) (synctechsms.ImportSummary, error) {
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
		return synctechsms.ImportSummary{}, err
	}
	if err := client.DownloadToFile(ctx, file.ID, staged); err != nil {
		item.Status = "failed"
		item.ErrorMessage = sql.NullString{String: err.Error(), Valid: true}
		_ = st.UpsertSourceImportItem(item)
		return synctechsms.ImportSummary{}, err
	}
	summary, err := imp.ImportPathIntoSource(sourceID, staged)
	if err != nil {
		item.Status = "failed"
		item.ErrorMessage = sql.NullString{String: err.Error(), Valid: true}
		_ = st.UpsertSourceImportItem(item)
		return summary, err
	}
	item.Status = "imported"
	item.ImportedAt = sql.NullTime{Time: time.Now(), Valid: true}
	item.RecordsImported = summary.SMSImported + summary.MMSImported + summary.CallsImported
	item.ErrorMessage = sql.NullString{}
	if err := st.UpsertSourceImportItem(item); err != nil {
		return summary, err
	}
	return summary, nil
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
