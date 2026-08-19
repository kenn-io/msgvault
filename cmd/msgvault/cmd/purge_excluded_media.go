package cmd

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

const (
	purgeExcludedMediaCommandName   = "purge-excluded-media"
	purgeExcludedMediaConfirmedFlag = "confirmed"
	purgeExcludedMediaYesFlag       = "--yes"
)

type purgeExcludedMediaDeps struct {
	openStore  func() (*store.Store, func(), error)
	config     func() *config.Config
	removeFile func(string) error
}

func defaultPurgeExcludedMediaDeps() purgeExcludedMediaDeps {
	return purgeExcludedMediaDeps{
		openStore:  openWritableStoreAndInitForIngest,
		config:     func() *config.Config { return cfg },
		removeFile: os.Remove,
	}
}

func newPurgeExcludedMediaCmd() *cobra.Command {
	cmd := newPurgeExcludedMediaLocalCmd(defaultPurgeExcludedMediaDeps())
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if isDaemonCLISubprocess() {
			return runLocal(cmd, args)
		}
		dryRun, err := cmd.Flags().GetBool("dry-run")
		if err != nil {
			return fmt.Errorf("read --dry-run flag: %w", err)
		}
		yes, err := cmd.Flags().GetBool("yes")
		if err != nil {
			return fmt.Errorf("read --yes flag: %w", err)
		}
		if !dryRun && !yes {
			confirmed, err := confirmExcludedMediaPurge(cmd.InOrStdin(), cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if !confirmed {
				return nil
			}
			if err := cmd.Flags().Set(purgeExcludedMediaConfirmedFlag, "true"); err != nil {
				return fmt.Errorf("record purge confirmation: %w", err)
			}
		}
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}
	return cmd
}

func newPurgeExcludedMediaLocalCmd(deps purgeExcludedMediaDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   purgeExcludedMediaCommandName,
		Short: "Reclaim locally stored media excluded by current policy",
		Long: `Replace locally stored provider attachment occurrences that the current
media policy excludes with typed skip markers. Shared blobs remain live while
any retained occurrence references them. Packed dead bytes are reclaimed by
the daemon's normal attachment repack maintenance.

Run with --dry-run to preview occurrence, blob, and logical byte counts.
Applying the purge requires an interactive confirmation or --yes. This command
never deletes media from a provider.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dryRun, err := cmd.Flags().GetBool("dry-run")
			if err != nil {
				return fmt.Errorf("read --dry-run flag: %w", err)
			}
			yes, err := cmd.Flags().GetBool("yes")
			if err != nil {
				return fmt.Errorf("read --yes flag: %w", err)
			}
			confirmed, err := cmd.Flags().GetBool(purgeExcludedMediaConfirmedFlag)
			if err != nil {
				return fmt.Errorf("read --%s flag: %w", purgeExcludedMediaConfirmedFlag, err)
			}
			return runPurgeExcludedMedia(cmd, deps, dryRun, yes || confirmed)
		},
	}
	cmd.Flags().Bool("dry-run", false, "preview exclusions without changing the archive")
	cmd.Flags().BoolP("yes", "y", false, "apply without an interactive confirmation")
	cmd.Flags().Bool(purgeExcludedMediaConfirmedFlag, false, "Internal: confirmation was accepted by the frontend CLI")
	if err := cmd.Flags().MarkHidden(purgeExcludedMediaConfirmedFlag); err != nil {
		panic(err)
	}
	return cmd
}

func confirmExcludedMediaPurge(in io.Reader, out io.Writer) (bool, error) {
	_, _ = fmt.Fprint(out, "Purge locally stored media excluded by the current config? [y/N] ")
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("read purge confirmation: %w", err)
		}
		return false, errors.New("no confirmation input (stdin closed); use --dry-run or --yes")
	}
	if !isYesAnswer(strings.TrimSpace(strings.ToLower(scanner.Text()))) {
		_, _ = fmt.Fprintln(out, "Aborted.")
		return false, nil
	}
	return true, nil
}

type excludedMediaPlan struct {
	Exclusions   []store.AttachmentExclusion
	Paths        map[string]string
	LogicalBytes int64
	BlobCount    int
	ByReason     map[attachmentpolicy.SkipReason]int
	// UnresolvedRosters counts occurrences left in place because a participant
	// limit is configured but no authoritative roster is archived for their
	// conversation, so the limit could not be evaluated.
	UnresolvedRosters int
}

func buildExcludedMediaPlan(candidates []store.AttachmentPolicyCandidate, cfg *config.Config) excludedMediaPlan {
	plan := excludedMediaPlan{
		Paths:    make(map[string]string),
		ByReason: make(map[attachmentpolicy.SkipReason]int),
	}
	seenBlobs := make(map[string]struct{})
	for _, candidate := range candidates {
		policy, ok := mediaPolicyForSource(cfg, candidate.SourceType, candidate.SourceIdentifier)
		if !ok {
			continue
		}
		unresolvedLimit := candidate.RosterUnresolved && policy.MaxParticipants > 0
		if unresolvedLimit {
			// No authoritative roster is archived, so the accumulated participant
			// count is not one to delete on. Excluding deletes blobs, so retain
			// rather than fail closed; scope, account, and size rules still apply.
			policy.MaxParticipants = 0
		}
		reason := policy.Evaluate(attachmentpolicy.Conversation{
			Type: candidate.ConversationType, ParticipantCount: candidate.ParticipantCount,
		}, candidate.Size)
		if reason == "" {
			if unresolvedLimit {
				plan.UnresolvedRosters++
			}
			continue
		}
		plan.Exclusions = append(plan.Exclusions, store.AttachmentExclusion{
			AttachmentID: candidate.AttachmentID, Reason: reason,
			SourceAttachmentID: candidate.SourceAttachmentID,
		})
		plan.LogicalBytes += candidate.Size
		plan.ByReason[reason]++
		addExcludedMediaBlob(&plan, seenBlobs, candidate.StoragePath, candidate.ContentHash)
		addExcludedMediaBlob(&plan, seenBlobs, candidate.ThumbnailPath, candidate.ThumbnailHash)
	}
	plan.BlobCount = len(seenBlobs)
	return plan
}

func addExcludedMediaBlob(plan *excludedMediaPlan, seen map[string]struct{}, storagePath, hash string) {
	if storagePath == "" || hash == "" {
		return
	}
	plan.Paths[storagePath] = hash
	seen[hash] = struct{}{}
}

func mediaPolicyForSource(cfg *config.Config, sourceType, identifier string) (attachmentpolicy.Policy, bool) {
	if cfg == nil {
		return attachmentpolicy.Policy{}, false
	}
	switch sourceType {
	case sourceTypeBeeper:
		return cfg.Beeper.MediaPolicy(identifier), true
	case sourceTypeSlack:
		teamID, _, ok := splitSlackIdentifier(identifier)
		if !ok {
			return attachmentpolicy.Policy{}, false
		}
		return cfg.Slack.MediaPolicy(teamID), true
	case sourceTypeDiscord:
		return cfg.Discord.MediaPolicy(identifier), true
	case sourceTypeTeams:
		return cfg.Teams.MediaPolicy(identifier), true
	default:
		return attachmentpolicy.Policy{}, false
	}
}

func runPurgeExcludedMedia(cmd *cobra.Command, deps purgeExcludedMediaDeps, dryRun, confirmed bool) error {
	if deps.openStore == nil || deps.config == nil {
		return errors.New("purge-excluded-media dependencies are unavailable")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	candidates, err := st.ListAttachmentPolicyCandidates(cmd.Context())
	if err != nil {
		return err
	}
	currentConfig := deps.config()
	plan := buildExcludedMediaPlan(candidates, currentConfig)
	if dryRun {
		writeExcludedMediaPlan(cmd.OutOrStdout(), plan, true)
		return nil
	}
	if !confirmed {
		accepted, err := confirmExcludedMediaPurge(cmd.InOrStdin(), cmd.OutOrStdout())
		if err != nil {
			return err
		}
		if !accepted {
			return nil
		}
	}
	if len(plan.Exclusions) > 0 {
		if err := st.ExcludeAttachmentOccurrences(cmd.Context(), plan.Exclusions); err != nil {
			return err
		}
	}
	writeExcludedMediaPlan(cmd.OutOrStdout(), plan, false)
	removeFile := deps.removeFile
	if removeFile == nil {
		removeFile = os.Remove
	}
	removed, cleanupErr := sweepUnreferencedLooseMedia(
		cmd.Context(), st, currentConfig.AttachmentsDir(), removeFile,
	)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Removed %d unreferenced loose blob file(s); packed dead bytes are eligible for attachment repack.\n", removed)
	return cleanupErr
}

func writeExcludedMediaPlan(out io.Writer, plan excludedMediaPlan, dryRun bool) {
	verb := "Excluded"
	if dryRun {
		verb = "Would exclude"
	}
	_, _ = fmt.Fprintf(out, "%s %d stored attachment occurrence(s), %s logical bytes across %d unique blob(s).\n",
		verb, len(plan.Exclusions), formatSize(plan.LogicalBytes), plan.BlobCount)
	reasons := make([]string, 0, len(plan.ByReason))
	for reason := range plan.ByReason {
		reasons = append(reasons, string(reason))
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		_, _ = fmt.Fprintf(out, "  %s: %d\n", reason, plan.ByReason[attachmentpolicy.SkipReason(reason)])
	}
	if plan.UnresolvedRosters > 0 {
		_, _ = fmt.Fprintf(out, "%d stored attachment occurrence(s) left in place: conversation membership is not archived, "+
			"so the participant limit could not be evaluated. Run a sync to record rosters, then purge again.\n",
			plan.UnresolvedRosters)
	}
	if dryRun && len(plan.Exclusions) > 0 {
		_, _ = fmt.Fprintln(out, "No changes made. Re-run with --yes to apply this local-only purge.")
	}
}

func sweepUnreferencedLooseMedia(
	ctx context.Context, st *store.Store, attachmentsDir string, removeFile func(string) error,
) (int, error) {
	topEntries, err := os.ReadDir(attachmentsDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("scan loose attachments: %w", err)
	}
	removed := 0
	var cleanupErrors []error
	for _, topEntry := range topEntries {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(cleanupErrors, err)...)
		}
		prefix := topEntry.Name()
		if !topEntry.IsDir() || !isLowerHex(prefix, 2) {
			continue
		}
		dirPath := filepath.Join(attachmentsDir, prefix)
		blobEntries, err := os.ReadDir(dirPath)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("scan attachment directory %s: %w", prefix, err))
			continue
		}
		for _, blobEntry := range blobEntries {
			if err := ctx.Err(); err != nil {
				return removed, errors.Join(append(cleanupErrors, err)...)
			}
			hash := blobEntry.Name()
			storagePath := prefix + "/" + hash
			if !blobEntry.Type().IsRegular() || !isCanonicalAttachmentPath(storagePath, hash) {
				continue
			}
			referenced, err := st.AttachmentBlobReferenced(ctx, hash, storagePath)
			if err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("check attachment %s: %w", storagePath, err))
				continue
			}
			if referenced {
				continue
			}
			err = removeFile(filepath.Join(dirPath, hash))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("remove unreferenced attachment %s: %w", storagePath, err))
				continue
			}
			if err == nil {
				removed++
			}
		}
	}
	return removed, errors.Join(cleanupErrors...)
}

func isCanonicalAttachmentPath(storagePath, hash string) bool {
	if !isLowerHex(hash, 64) || storagePath != hash[:2]+"/"+hash {
		return false
	}
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == 32
}

func isLowerHex(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func init() {
	rootCmd.AddCommand(newPurgeExcludedMediaCmd())
}
