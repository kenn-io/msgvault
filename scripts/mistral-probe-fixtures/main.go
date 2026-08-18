// Command mistral-probe-fixtures builds the private synthetic corpus consumed
// by `msgvault documents probe-mistral`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"go.kenn.io/docbank/document/mistral"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("mistral-probe-fixtures", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var outputDirectory string
	var seedDirectory string
	flags.StringVar(&outputDirectory, "output", "", "private output directory")
	flags.StringVar(&seedDirectory, "seed-dir", "", "private directory containing native-format seeds")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse fixture builder flags: %w", err)
	}
	if flags.NArg() != 0 || outputDirectory == "" {
		return errors.New("usage: mistral-probe-fixtures --output <private-dir> [--seed-dir <private-dir>]")
	}
	if err := mistral.WriteProbeFixtures(ctx, outputDirectory, mistral.FixtureOptions{
		SeedDirectory: seedDirectory,
	}); err != nil {
		return fmt.Errorf("write Mistral probe fixtures: %w", err)
	}
	_, _ = fmt.Fprintf(stdout,
		"Wrote %d private Mistral probe fixtures; no provider requests were made.\n",
		len(mistral.CandidateFormats()))
	return nil
}
