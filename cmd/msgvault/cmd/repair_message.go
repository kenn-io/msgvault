package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	syncer "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type repairMessageCommandDeps struct {
	isDaemonSubprocess func() bool
	openHTTPStore      func(context.Context) (*daemonclient.Client, HTTPStoreInfo, error)
	preflightReauth    func(context.Context, *daemonclient.Client, HTTPStoreInfo, int64) error
	openWritableStore  func() (*store.Store, func(), error)
	openReadOnlyStore  func() (*store.Store, func(), error)
	newGmailClient     func(context.Context, *store.Source) (gmail.API, error)
	refreshCache       func() error
	attachmentsDir     string
}

func defaultRepairMessageCommandDeps() repairMessageCommandDeps {
	return repairMessageCommandDeps{
		isDaemonSubprocess: isDaemonCLISubprocess,
		openHTTPStore:      OpenHTTPStore,
		preflightReauth: func(
			ctx context.Context, client *daemonclient.Client, info HTTPStoreInfo, sourceID int64,
		) error {
			return preflightReauth(ctx, buildSyncPreflight(client, info), "", sourceID)
		},
		openWritableStore: openWritableStoreAndInit,
		openReadOnlyStore: func() (*store.Store, func(), error) {
			st, err := store.OpenReadOnly(cfg.DatabaseDSN())
			if err != nil {
				return nil, nil, fmt.Errorf("open database read-only: %w", err)
			}
			return st, func() { _ = st.Close() }, nil
		},
		newGmailClient: func(ctx context.Context, source *store.Source) (gmail.API, error) {
			return buildAPIClient(ctx, source, oauthManagerCache(), nil)
		},
		refreshCache: func() error {
			return rebuildCacheAfterWrite(cfg.DatabaseDSN())
		},
	}
}

func init() {
	rootCmd.AddCommand(newRepairMessageCmd(defaultRepairMessageCommandDeps()))
}

func newRepairMessageCmd(deps repairMessageCommandDeps) *cobra.Command {
	var (
		audit    bool
		jsonOut  bool
		sourceID int64
	)
	command := &cobra.Command{
		Use:   "repair-message <internal-id-or-gmail-id>",
		Short: "Repair one Gmail message snapshot or audit stored Gmail MIME",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if audit {
				if len(args) != 0 {
					return usageErr(cmd, errors.New("--audit does not accept a message reference"))
				}
			} else if len(args) != 1 {
				return usageErr(cmd, errors.New("repair-message requires exactly one message reference unless --audit is set"))
			}
			if cmd.Flags().Changed("source-id") && sourceID <= 0 {
				return usageErr(cmd, errors.New("--source-id must be a positive integer"))
			}
			if jsonOut && !audit {
				return usageErr(cmd, errors.New("--json requires --audit"))
			}
			if deps.isDaemonSubprocess != nil && !deps.isDaemonSubprocess() {
				return runRepairMessageHTTP(cmd, deps, args, sourceID, audit, jsonOut)
			}
			if audit {
				return runRepairMessageAudit(cmd, deps, sourceID, jsonOut)
			}
			return runRepairMessageLocal(cmd, deps, args[0], sourceID)
		},
	}
	command.Flags().BoolVar(&audit, "audit", false, "audit stored Gmail MIME without changing the archive")
	command.Flags().Int64Var(&sourceID, "source-id", 0, "scope the operation to one positive source ID")
	command.Flags().BoolVar(&jsonOut, "json", false, "emit newline-delimited JSON audit results")
	return command
}

func runRepairMessageHTTP(
	cmd *cobra.Command,
	deps repairMessageCommandDeps,
	args []string,
	sourceID int64,
	audit bool,
	jsonOut bool,
) error {
	if deps.openHTTPStore == nil {
		return errors.New("repair message HTTP store is unavailable")
	}
	client, info, err := deps.openHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	req := generated.CLIRepairMessageRequest{}
	if audit {
		req.Audit = new(true)
	} else {
		reference := args[0]
		req.Reference = &reference
	}
	if sourceID > 0 {
		req.SourceID = &sourceID
	}
	if jsonOut {
		req.JSON = new(true)
	}

	var preflight func(context.Context) error
	if !audit && info.Kind == HTTPStoreLocalDaemon {
		resolvedSourceID := sourceID
		if resolvedSourceID == 0 {
			if deps.openReadOnlyStore == nil {
				return errors.New("repair message read-only store is unavailable")
			}
			st, cleanup, err := deps.openReadOnlyStore()
			if err != nil {
				return err
			}
			source, resolveErr := resolveRepairMessageSource(cmd.Context(), st, args[0], 0)
			cleanup()
			if resolveErr != nil {
				return resolveErr
			}
			resolvedSourceID = source.ID
			req.SourceID = &resolvedSourceID
		}
		if deps.preflightReauth != nil {
			preflight = func(ctx context.Context) error {
				return deps.preflightReauth(ctx, client, info, resolvedSourceID)
			}
		}
	}

	output := func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			if _, err := fmt.Fprint(cmd.OutOrStdout(), data); err != nil {
				return fmt.Errorf("write repair-message stdout: %w", err)
			}
		case cliStreamStderr:
			if _, err := fmt.Fprint(cmd.ErrOrStderr(), data); err != nil {
				return fmt.Errorf("write repair-message stderr: %w", err)
			}
		}
		return nil
	}
	if preflight != nil {
		return client.RunCLIRepairMessageWithPreflight(cmd.Context(), req, preflight, output)
	}
	return client.RunCLIRepairMessage(cmd.Context(), req, output)
}

func runRepairMessageLocal(
	cmd *cobra.Command, deps repairMessageCommandDeps, reference string, sourceID int64,
) error {
	if deps.openWritableStore == nil {
		return errors.New("repair message writable store is unavailable")
	}
	st, cleanup, err := deps.openWritableStore()
	if err != nil {
		return err
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	source, err := resolveRepairMessageSource(cmd.Context(), st, reference, sourceID)
	if err != nil {
		return err
	}
	if deps.newGmailClient == nil {
		return errors.New("repair message Gmail client is unavailable")
	}
	client, err := deps.newGmailClient(cmd.Context(), source)
	if err != nil {
		return fmt.Errorf("open Gmail client for source %d: %w", source.ID, err)
	}
	if closer, ok := client.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	attachmentsDir := deps.attachmentsDir
	if attachmentsDir == "" && cfg != nil {
		attachmentsDir = cfg.AttachmentsDir()
	}
	service := syncer.New(client, st, &syncer.Options{AttachmentsDir: attachmentsDir}).WithLogger(logger)
	result, err := service.RepairMessage(cmd.Context(), syncer.RepairRequest{
		Reference: reference,
		SourceID:  sourceID,
	})
	if err != nil {
		return err
	}
	cleanup()
	cleanup = nil
	if deps.refreshCache != nil {
		if err := deps.refreshCache(); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Repaired message %d (source %d, Gmail %s): %s\n",
		result.InternalID, result.SourceID, result.SourceMessageID, result.Subject)
	if err != nil {
		return fmt.Errorf("write repair-message result: %w", err)
	}
	return nil
}

func resolveRepairMessageSource(
	ctx context.Context, st *store.Store, reference string, sourceID int64,
) (*store.Source, error) {
	targets, err := st.FindRepairMessageTargetsContext(ctx, strings.TrimSpace(reference), sourceID)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("repair message %q not found", reference)
	}
	if len(targets) != 1 {
		return nil, fmt.Errorf("repair message reference %q is ambiguous across %d archive rows", reference, len(targets))
	}
	if targets[0].SourceType != sourceTypeGmail {
		return nil, fmt.Errorf("repair message source %d is %s, not gmail", targets[0].SourceID, targets[0].SourceType)
	}
	return st.GetSourceByIDContext(ctx, targets[0].SourceID)
}

func runRepairMessageAudit(
	cmd *cobra.Command, deps repairMessageCommandDeps, sourceID int64, jsonOut bool,
) error {
	if deps.openReadOnlyStore == nil {
		return errors.New("repair message read-only store is unavailable")
	}
	st, cleanup, err := deps.openReadOnlyStore()
	if err != nil {
		return err
	}
	defer cleanup()
	service := syncer.New(nil, st, &syncer.Options{})
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetEscapeHTML(false)
	return service.AuditGmailMessages(cmd.Context(), sourceID, func(result syncer.RepairAuditResult) error {
		if jsonOut {
			return encoder.Encode(result)
		}
		return writeRepairAuditHuman(cmd.OutOrStdout(), result)
	})
}

func writeRepairAuditHuman(w io.Writer, result syncer.RepairAuditResult) error {
	fields := make([]string, 0, len(result.Fields))
	for name := range result.Fields {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	parts := make([]string, 0, len(fields)+1)
	for _, name := range fields {
		parts = append(parts, name+"="+string(result.Fields[name]))
	}
	if result.Error != "" {
		parts = append(parts, "error="+strconv.Quote(result.Error))
	}
	_, err := fmt.Fprintf(w, "%s message %d (source %d, Gmail %s): %s\n",
		result.Status, result.InternalID, result.SourceID, result.SourceMessageID, strings.Join(parts, ", "))
	if err != nil {
		return fmt.Errorf("write repair-message audit result: %w", err)
	}
	return nil
}
