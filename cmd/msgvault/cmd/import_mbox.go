package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/importer"
	"github.com/wesm/msgvault/internal/importer/mboxzip"
	"github.com/wesm/msgvault/internal/store"
)

var (
	importMboxSourceType         string
	importMboxLabels             []string
	importMboxNoResume           bool
	importMboxCheckpointInterval int
	importMboxNoAttachments      bool
	noDefaultIdentityImportMbox  bool
)

type mboxCheckpoint struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
	Seq    int64  `json:"seq,omitempty"`
}

var importMboxCmd = &cobra.Command{
	Use:   "import-mbox <identifier> <export-file>",
	Short: "Import an MBOX export into msgvault",
	Long: `Import an MBOX export into msgvault.

The export file may be a plain mbox file (any extension) or a .zip containing
one or more .mbox files.

This is useful for email providers that offer an export but no IMAP/POP access.
The importer stores raw MIME, bodies, recipients, and optional attachments.

Examples:
  msgvault init-db
  msgvault import-mbox you@example.com /path/to/export.mbox
  msgvault import-mbox you@example.com /path/to/export.zip

  # HEY.com export (still MBOX)
  msgvault import-mbox you@hey.com hey-export.zip --source-type hey --label hey
`,
	Args:         cobra.ExactArgs(2),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]
		exportPath := args[1]

		// Handle Ctrl+C gracefully (save checkpoint and exit cleanly).
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		sigChan := make(chan os.Signal, 2)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		done := make(chan struct{})
		defer func() {
			close(done)
			signal.Stop(sigChan)
			for {
				select {
				case <-sigChan:
					// Drain queued signals to avoid late os.Exit(130) during teardown.
				default:
					return
				}
			}
		}()
		go func() {
			signals := 0
			for {
				select {
				case <-done:
					return
				case <-sigChan:
					select {
					case <-done:
						return
					default:
					}
					signals++
					if signals == 1 {
						_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "\nInterrupted. Saving checkpoint...")
						cancel()
						continue
					}
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Interrupted again. Exiting immediately.")
					os.Exit(130)
				}
			}
		}()

		dbPath := cfg.DatabaseDSN()
		st, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = st.Close() }()

		if err := st.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
		if err := runStartupMigrationsForIngest(st); err != nil {
			return fmt.Errorf("startup migrations: %w", err)
		}

		attachmentsDir := cfg.AttachmentsDir()
		if importMboxNoAttachments {
			attachmentsDir = ""
		}

		mboxFiles, err := mboxzip.ResolveMboxExport(exportPath, cfg.Data.DataDir, logger)
		if err != nil {
			return err
		}

		// If we're resuming, start from the active file in a multi-file zip export.
		// Source creation here is for resume detection only; the post-import
		// runPostSourceCreateMigrations call below covers both resume and
		// --no-resume paths.
		if !importMboxNoResume {
			src, err := st.GetOrCreateSource(importMboxSourceType, identifier)
			if err != nil {
				return fmt.Errorf("get/create source: %w", err)
			}
			active, err := st.GetActiveSync(src.ID)
			if err != nil {
				return fmt.Errorf("check active sync: %w", err)
			}
			if active != nil && active.CursorBefore.Valid && active.CursorBefore.String != "" {
				var cp mboxCheckpoint
				if err := json.Unmarshal([]byte(active.CursorBefore.String), &cp); err == nil && cp.File != "" {
					cpFile := cp.File
					if abs, err := filepath.Abs(cpFile); err == nil {
						cpFile = abs
					}
					cpFile = filepath.Clean(cpFile)
					if resolved, err := filepath.EvalSymlinks(cpFile); err == nil {
						cpFile = resolved
					}
					cpInfo, cpInfoErr := os.Stat(cpFile)

					startIdx := -1
					for i, p := range mboxFiles {
						pp := p
						if abs, err := filepath.Abs(pp); err == nil {
							pp = abs
						}
						pp = filepath.Clean(pp)

						if pp == cpFile {
							startIdx = i
							break
						}
						if cpInfoErr == nil {
							if info, err := os.Stat(pp); err == nil && os.SameFile(cpInfo, info) {
								startIdx = i
								break
							}
						}
					}
					if startIdx == -1 {
						return fmt.Errorf("active mbox import is for a different file (%q); rerun with --no-resume to start fresh", cp.File)
					}
					mboxFiles = mboxFiles[startIdx:]
				}
			} else if len(mboxFiles) > 1 {
				// If we don't have an active sync, fall back to the last successful
				// sync run. This avoids rescanning already-finished files when a
				// multi-file import is interrupted between files.
				last, err := st.GetLastSuccessfulSync(src.ID)
				if err != nil {
					return fmt.Errorf("check last successful sync: %w", err)
				}
				if last != nil && last.CursorBefore.Valid && last.CursorBefore.String != "" {
					var cp mboxCheckpoint
					if err := json.Unmarshal([]byte(last.CursorBefore.String), &cp); err == nil && cp.File != "" {
						cpFile := cp.File
						if abs, err := filepath.Abs(cpFile); err == nil {
							cpFile = abs
						}
						cpFile = filepath.Clean(cpFile)
						if resolved, err := filepath.EvalSymlinks(cpFile); err == nil {
							cpFile = resolved
						}
						cpInfo, cpInfoErr := os.Stat(cpFile)

						startIdx := -1
						var matchedPath string
						for i, p := range mboxFiles {
							pp := p
							if abs, err := filepath.Abs(pp); err == nil {
								pp = abs
							}
							pp = filepath.Clean(pp)

							if pp == cpFile {
								startIdx = i
								matchedPath = p
								break
							}
							if cpInfoErr == nil {
								if info, err := os.Stat(pp); err == nil && os.SameFile(cpInfo, info) {
									startIdx = i
									matchedPath = p
									break
								}
							}
						}
						if startIdx != -1 {
							cursorOK := true
							if st, err := os.Stat(matchedPath); err == nil {
								if cp.Offset == st.Size() {
									startIdx++
								} else if cp.Offset > st.Size() {
									// Ignore an invalid cursor (beyond EOF). Falling back to scanning
									// all files is safer than skipping the matched file.
									cursorOK = false
								}
							}
							if cursorOK {
								if startIdx >= len(mboxFiles) {
									mboxFiles = nil
								} else {
									mboxFiles = mboxFiles[startIdx:]
								}
							}
						}
					}
				}
			}
		}

		var (
			totalProcessed     int64
			totalAdded         int64
			totalUpdated       int64
			totalSkipped       int64
			totalLabelsUpdated int64
			totalErrors        int64
			totalBytes         int64
			hadHardErrors      bool
			sourceID           int64
		)
		type processedFile struct {
			Path    string
			Partial bool
		}
		processedFiles := make([]processedFile, 0, len(mboxFiles))

		for _, mboxPath := range mboxFiles {
			summary, err := importer.ImportMbox(ctx, st, mboxPath, importer.MboxImportOptions{
				SourceType:         importMboxSourceType,
				Identifier:         identifier,
				Labels:             importMboxLabels,
				NoResume:           importMboxNoResume,
				CheckpointInterval: importMboxCheckpointInterval,
				AttachmentsDir:     attachmentsDir,
				Logger:             logger,
			})
			if err != nil {
				return err
			}

			totalProcessed += summary.MessagesProcessed
			totalAdded += summary.MessagesAdded
			totalUpdated += summary.MessagesUpdated
			totalSkipped += summary.MessagesSkipped
			totalLabelsUpdated += summary.LabelsUpdated
			totalErrors += summary.Errors
			totalBytes += summary.BytesProcessed
			if sourceID == 0 && summary.SourceID != 0 {
				sourceID = summary.SourceID
			}
			if summary.HardErrors {
				hadHardErrors = true
			}

			var partial bool
			if fi, err := os.Stat(mboxPath); err == nil && summary.FinalOffset < fi.Size() {
				partial = true
			}
			processedFiles = append(processedFiles, processedFile{Path: mboxPath, Partial: partial})

			if ctx.Err() != nil {
				break
			}

			// Stop processing subsequent files when the current file had
			// hard errors. Otherwise resume would advance past this file
			// based on a later successful file's checkpoint, permanently
			// skipping the failed file's unprocessed messages.
			if summary.HardErrors {
				break
			}
		}

		// Auto-default-identity must run BEFORE the legacy migration
		// retry — see comment in account_identity.go. Earlier shape
		// ran the migration first and confirmDefaultIdentity later,
		// which suppressed the source's own account identifier
		// whenever the legacy [identity] block had populated rows.
		if ctx.Err() == nil && !hadHardErrors && !noDefaultIdentityImportMbox {
			if sourceID != 0 {
				confirmDefaultIdentity(cmd.OutOrStdout(), st, sourceID, identifier, identifier, "account-identifier")
			}
		}

		// Re-run startup migrations after the importer has had a chance
		// to create the first source. Required when the deferred legacy
		// identity migration parked at startup because no source existed.
		// Cheap no-op once the migration sentinel is set.
		//
		// Migration error returns before the summary print on purpose:
		// minimal in-scope shape for #304's deferred legacy [identity]
		// migration retry. The alternative (capture, print summary,
		// return after) would restructure pre-existing summary code
		// this PR otherwise leaves alone. Migration is idempotent —
		// next invocation retries and prints summary then. UX polish
		// tracked in
		// private/drafts/2026-05-02-issue-import-migration-error-ux.md.
		if sourceID != 0 {
			if err := runPostSourceCreateMigrations(st); err != nil {
				return fmt.Errorf("post-source-create migrations: %w", err)
			}
		}

		out := cmd.OutOrStdout()
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(out, "Import interrupted. Run again to resume.")
		} else if totalErrors > 0 {
			_, _ = fmt.Fprintln(out, "Import complete (with errors).")
		} else {
			_, _ = fmt.Fprintln(out, "Import complete.")
		}
		for _, p := range processedFiles {
			if p.Partial {
				_, _ = fmt.Fprintf(out, "  Imported (partial): %s\n", p.Path)
			} else {
				_, _ = fmt.Fprintf(out, "  Imported:           %s\n", p.Path)
			}
		}
		_, _ = fmt.Fprintf(out, "  Processed:      %d messages\n", totalProcessed)
		_, _ = fmt.Fprintf(out, "  Added:          %d messages\n", totalAdded)
		_, _ = fmt.Fprintf(out, "  Updated:        %d messages\n", totalUpdated)
		_, _ = fmt.Fprintf(out, "  Skipped:        %d messages\n", totalSkipped)
		_, _ = fmt.Fprintf(out, "  Labels updated: %d messages\n", totalLabelsUpdated)
		_, _ = fmt.Fprintf(out, "  Errors:         %d\n", totalErrors)
		_, _ = fmt.Fprintf(out, "  Bytes:          %.2f MB\n", float64(totalBytes)/(1024*1024))

		rebuildCacheAfterWrite(dbPath)

		if ctx.Err() == nil && hadHardErrors {
			return fmt.Errorf("import completed with %d errors", totalErrors)
		}
		if ctx.Err() != nil {
			return context.Canceled
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(importMboxCmd)

	importMboxCmd.Flags().StringVar(&importMboxSourceType, "source-type", "mbox", "Source type to record in the database (e.g. mbox, hey)")
	importMboxCmd.Flags().StringSliceVar(&importMboxLabels, "label", nil, "Label(s) to apply to imported messages (repeatable, or comma-separated)")
	importMboxCmd.Flags().BoolVar(&importMboxNoResume, "no-resume", false, "Do not resume from an interrupted import")
	importMboxCmd.Flags().IntVar(&importMboxCheckpointInterval, "checkpoint-interval", 200, "Save progress every N messages")
	importMboxCmd.Flags().BoolVar(&importMboxNoAttachments, "no-attachments", false, "Do not store attachments (disk or database). Messages will still be marked as having attachments. Note: rerunning later without --no-attachments will not backfill attachments for already-imported messages.")
	importMboxCmd.Flags().BoolVar(&noDefaultIdentityImportMbox, "no-default-identity", false, noDefaultIdentityHelp)
}
