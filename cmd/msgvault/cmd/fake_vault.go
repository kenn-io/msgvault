package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/fakevault"
)

// fakeVaultCmd is a hidden developer tool: benchmark harnesses and tests
// need multi-gigabyte msgvault archives that cannot be checked into any
// repository, so they generate them on demand instead.
var fakeVaultCmd = &cobra.Command{
	Use:   "fake-vault",
	Short: "Generate a synthetic vault for benchmarking and testing",
	Long: `Generate a synthetic msgvault archive: a schema-valid msgvault.db
plus a content-addressed attachments tree, deterministic for a given seed.

The output directory receives msgvault.db and attachments/. Generation is
sized by --messages (database bulk: bodies, raw MIME, metadata rows) and
--attachment-bytes (content tree bulk). With --append, an existing generated
vault is extended in place — the incremental-backup benchmarking mode.

The generated data is synthetic and worthless; the tool exists to measure
how msgvault (backup, search, sync machinery) behaves at scale.`,
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runFakeVault,
}

var (
	fakeVaultOutput     string
	fakeVaultMessages   int64
	fakeVaultAttachSize string
	fakeVaultSeed       uint64
	fakeVaultAppend     bool
	fakeVaultQuiet      bool
)

func init() {
	fakeVaultCmd.Flags().StringVarP(&fakeVaultOutput, "output", "o", "",
		"vault directory to create (msgvault.db and attachments/ inside)")
	fakeVaultCmd.Flags().Int64Var(&fakeVaultMessages, "messages", 10_000,
		"number of messages to generate")
	fakeVaultCmd.Flags().StringVar(&fakeVaultAttachSize, "attachment-bytes", "50MB",
		"target total size of attachment content (e.g. 500MB, 5GB)")
	fakeVaultCmd.Flags().Uint64Var(&fakeVaultSeed, "seed", 1,
		"deterministic generation seed")
	fakeVaultCmd.Flags().BoolVar(&fakeVaultAppend, "append", false,
		"extend an existing generated vault instead of creating one")
	fakeVaultCmd.Flags().BoolVar(&fakeVaultQuiet, "quiet", false,
		"suppress progress output")
	_ = fakeVaultCmd.MarkFlagRequired("output")
	rootCmd.AddCommand(fakeVaultCmd)
}

func runFakeVault(cmd *cobra.Command, args []string) error {
	attachBytes, err := parseByteSize(fakeVaultAttachSize)
	if err != nil {
		return usageErr(cmd, fmt.Errorf("--attachment-bytes: %w", err))
	}
	progress := func(done, total int64) {
		if !fakeVaultQuiet {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\rGenerating messages: %d/%d", done, total)
		}
	}
	sum, err := fakevault.Generate(cmd.Context(), fakevault.Options{
		Dir:             fakeVaultOutput,
		Messages:        fakeVaultMessages,
		AttachmentBytes: attachBytes,
		Seed:            fakeVaultSeed,
		Append:          fakeVaultAppend,
		Progress:        progress,
	})
	if err != nil {
		return err
	}
	if !fakeVaultQuiet {
		_, _ = fmt.Fprintln(cmd.OutOrStdout())
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Vault: %s\n", fakeVaultOutput)
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
