package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fakevault"
	"go.kenn.io/msgvault/internal/fakevault/agentdemo"
)

// fakeVaultCmd is a hidden developer tool: benchmark harnesses and tests
// need multi-gigabyte msgvault archives that cannot be checked into any
// repository, so they generate them on demand instead.
var fakeVaultCmd = newFakeVaultCommand()

type fakeVaultCommandOptions struct {
	scenario         string
	asOf             string
	output           string
	messages         int64
	participants     int64
	participantEdges int64
	groupChatMembers int64
	attachmentSize   string
	seed             uint64
	appendMode       bool
	quiet            bool
}

func newFakeVaultCommand() *cobra.Command {
	var opts fakeVaultCommandOptions
	command := &cobra.Command{
		Use:   "fake-vault",
		Short: "Generate a synthetic vault for benchmarking and testing",
		Long: `Generate a synthetic msgvault archive: a schema-valid msgvault.db
plus a content-addressed attachments tree, deterministic for a given seed.

The output directory receives msgvault.db and attachments/. Generation is
sized by --messages (database bulk: bodies, raw MIME, metadata rows) and
--attachment-bytes (content tree bulk). With --append, an existing generated
vault is extended in place — the incremental-backup benchmarking mode.

Setting --participants and --participant-edges selects the set-wise
relationship benchmark generator. Participant edges count the union of one
sender edge per message and all generated recipient rows.

Use --scenario agent-demo for a small named synthetic archive for feature tests
and safe exploration.

The generated data is synthetic and worthless; the tool exists to measure
how msgvault (backup, search, sync machinery) behaves at scale.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFakeVault(cmd, opts)
		},
	}
	command.Flags().StringVar(&opts.scenario, "scenario", "scale", "scenario: scale or agent-demo")
	command.Flags().StringVar(&opts.asOf, "as-of", agentdemo.DefaultAsOf.Format(time.RFC3339), "agent demo clock (RFC 3339 or now)")
	command.Flags().StringVarP(&opts.output, "output", "o", "",
		"vault directory to create (msgvault.db and attachments/ inside)")
	command.Flags().Int64Var(&opts.messages, "messages", 10_000,
		"number of messages to generate")
	command.Flags().Int64Var(&opts.participants, "participants", 0,
		"exact participant count (zero keeps the adaptive default)")
	command.Flags().Int64Var(&opts.participantEdges, "participant-edges", 0,
		"exact sender plus recipient edge count (zero keeps the default generator)")
	command.Flags().Int64Var(&opts.groupChatMembers, "group-chat-members", 2,
		"conversation members in set-wise group chats")
	command.Flags().StringVar(&opts.attachmentSize, "attachment-bytes", "50MB",
		"target total size of attachment content (e.g. 500MB, 5GB)")
	command.Flags().Uint64Var(&opts.seed, "seed", 1,
		"deterministic generation seed")
	command.Flags().BoolVar(&opts.appendMode, "append", false,
		"extend an existing generated vault instead of creating one")
	command.Flags().BoolVar(&opts.quiet, "quiet", false,
		"suppress progress output")
	_ = command.MarkFlagRequired("output")
	return command
}

func init() {
	rootCmd.AddCommand(fakeVaultCmd)
}

func runFakeVault(cmd *cobra.Command, opts fakeVaultCommandOptions) error {
	switch opts.scenario {
	case agentdemo.Scenario:
		return runAgentDemo(cmd, opts)
	case "scale":
	default:
		return usageErr(cmd, fmt.Errorf("--scenario: choose scale, agent-demo"))
	}
	attachBytes, err := parseByteSize(opts.attachmentSize)
	if err != nil {
		return usageErr(cmd, fmt.Errorf("--attachment-bytes: %w", err))
	}
	progress := func(done, total int64) {
		if !opts.quiet {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\rGenerating messages: %d/%d", done, total)
		}
	}
	sum, err := fakevault.Generate(cmd.Context(), fakevault.Options{
		Dir:              opts.output,
		Messages:         opts.messages,
		Participants:     opts.participants,
		ParticipantEdges: opts.participantEdges,
		GroupChatMembers: opts.groupChatMembers,
		AttachmentBytes:  attachBytes,
		Seed:             opts.seed,
		Append:           opts.appendMode,
		Progress:         progress,
	})
	if err != nil {
		return err
	}
	if !opts.quiet {
		_, _ = fmt.Fprintln(cmd.OutOrStdout())
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Vault: %s\n", opts.output)
	_, _ = fmt.Fprintf(out, "Messages: %d (in %d conversations)\n", sum.Messages, sum.Conversations)
	_, _ = fmt.Fprintf(out, "Attachment rows: %d\n", sum.AttachmentRows)
	_, _ = fmt.Fprintf(out, "Attachment content written: %d files, %s\n",
		sum.AttachmentBlobs, formatSize(sum.AttachmentBytes))
	return nil
}

// parseByteSize parses a size like "500MB", "5GB", or a plain byte count.
// Suffixes are binary multiples (KB = 1024).
func parseByteSize(s string) (int64, error) {
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1},
	}
	trimmed := strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for _, m := range suffixes {
		if rest, ok := strings.CutSuffix(trimmed, m.suffix); ok {
			trimmed, mult = rest, m.mult
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(trimmed), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q (use e.g. 500MB, 5GB, or bytes)", s)
	}
	if n < 0 {
		return 0, errors.New("size must not be negative")
	}
	return n * mult, nil
}

func runAgentDemo(cmd *cobra.Command, opts fakeVaultCommandOptions) error {
	for _, flag := range []string{"messages", "participants", "participant-edges", "group-chat-members", "attachment-bytes", "append"} {
		if cmd.Flags().Changed(flag) {
			return usageErr(cmd, fmt.Errorf("--%s cannot be used with --scenario agent-demo", flag))
		}
	}
	var asOf time.Time
	if opts.asOf == "now" {
		asOf = time.Now().UTC().Truncate(time.Hour)
	} else {
		var err error
		asOf, err = time.Parse(time.RFC3339, opts.asOf)
		if err != nil {
			return usageErr(cmd, fmt.Errorf("--as-of: expected RFC 3339 or now: %w", err))
		}
		if asOf.IsZero() {
			return usageErr(cmd, errors.New("--as-of must not be the zero time"))
		}
	}
	cfg := config.NewDefaultConfig()
	if inv := invocationFromCommand(cmd); inv != nil && inv.cfg != nil {
		cfg = inv.cfg
	}
	output, err := canonicalPathThroughExistingAncestor(opts.output)
	if err != nil {
		return err
	}
	live, err := canonicalPathThroughExistingAncestor(cfg.Data.DataDir)
	if err != nil {
		return err
	}
	refuse := func() error {
		return fmt.Errorf("fake-vault: refusing to write a demo archive into an existing archive directory")
	}
	if output == live {
		return refuse()
	}
	m, err := agentdemo.GenerateDir(cmd.Context(), agentdemo.Options{Dir: opts.output, Seed: opts.seed, AsOf: asOf, Progress: func(chapter string) {
		if !opts.quiet {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Generating %s\n", chapter)
		}
	}})
	if err != nil {
		if errors.Is(err, fakevault.ErrTargetExists) {
			return refuse()
		}
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Agent demo archive: %s (scenario %s v%d, seed %d, as of %s)\n", opts.output, m.Scenario, m.ScenarioVersion, m.Seed, m.AsOf.Format(time.RFC3339))
	_, _ = fmt.Fprintf(out, "Messages: %d in %d conversations across %d sources\n", len(m.Messages), len(m.Conversations), len(m.Sources))
	_, _ = fmt.Fprintf(out, "Persons: %d; unsaved participants: %d\n", len(m.Persons), countUnsavedParticipants(m))
	_, _ = fmt.Fprintf(out, "Manifest: %s\n", filepath.Join(opts.output, agentdemo.ManifestFile))
	_, _ = fmt.Fprintf(out, "Try: MSGVAULT_HOME=%s msgvault serve\n", opts.output)
	return nil
}
func countUnsavedParticipants(m *agentdemo.Manifest) int {
	n := 0
	for _, p := range m.Persons {
		n += len(p.ParticipantIDs)
	}
	for h := range m.Participants {
		if h == "owner" || strings.HasPrefix(h, "owner_") {
			n++
		}
	}
	return len(m.Participants) - n
}
