package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

const callsMinAPIVersion = "3.24.0"

type callCommandClient interface {
	APISchemaVersion(ctx context.Context) (string, error)
	ImportCallObservation(ctx context.Context, data []byte) (daemonclient.CallImportResult, error)
	ListCallObservations(ctx context.Context, q store.CallQuery) (daemonclient.CallEvidencePage, error)
	SummarizeCalls(ctx context.Context, q store.CallQuery) (daemonclient.CallSummaryPage, error)
}
type callCommandDeps struct {
	open func(context.Context) (callCommandClient, func(), error)
}

func newCallsCommand(deps callCommandDeps) *cobra.Command {
	root := &cobra.Command{
		Use: "calls", Short: "Import and inspect call history",
		Long: "Read call evidence through the daemon. Scheduled invitations never count as answered calls. All output is JSON.",
	}
	open := func(ctx context.Context) (callCommandClient, func(), error) {
		client, cleanup, err := deps.open(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		version, err := client.APISchemaVersion(ctx)
		if err != nil || !daemonclient.APISchemaVersionAtLeast(version, callsMinAPIVersion) {
			cleanup()
			if err != nil {
				return nil, func() {}, err
			}
			return nil, func() {}, fmt.Errorf("calls require daemon API %s or newer; daemon reports %s", callsMinAPIVersion, version)
		}
		return client, cleanup, nil
	}
	write := func(cmd *cobra.Command, value any) error {
		if err := json.MarshalWrite(cmd.OutOrStdout(), value); err != nil {
			return err
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout())
		if err != nil {
			return fmt.Errorf("write call output: %w", err)
		}
		return nil
	}
	importCommand := &cobra.Command{
		Use: "import [file|-]", Short: "Import one supplied JSON observation (default: stdin)",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := cmd.InOrStdin()
			if len(args) == 1 && args[0] != "-" {
				file, err := os.Open(args[0])
				if err != nil {
					return fmt.Errorf("open call import: %w", err)
				}
				defer func() { _ = file.Close() }()
				input = file
			}
			data, err := io.ReadAll(io.LimitReader(input, callhistory.MaxRequestBytes+1))
			if err != nil {
				return err
			}
			if len(data) > callhistory.MaxRequestBytes {
				return callhistory.ErrImportTooLarge
			}
			if _, err := callhistory.DecodeImport(bytes.NewReader(data)); err != nil {
				return err
			}
			client, cleanup, err := open(cmd.Context())
			if err != nil {
				return err
			}
			defer cleanup()
			result, err := client.ImportCallObservation(cmd.Context(), data)
			if err != nil {
				return err
			}
			return write(cmd, result)
		},
	}
	root.AddCommand(importCommand)
	for _, summary := range []bool{false, true} {
		name, description := "list", "Inspect source observations and participant attendance"
		if summary {
			name, description = "summary", "Rank people by confirmed answered calls"
		}
		var q store.CallQuery
		var after, before string
		command := &cobra.Command{Use: name, Short: description, Args: cobra.NoArgs}
		command.Flags().Int64Var(&q.PersonID, "person-id", 0, "Filter by durable person ID")
		command.Flags().IntVar(&q.Limit, "limit", 50, "Maximum rows (1–100)")
		command.Flags().StringVar(&after, "after", "", "Inclusive RFC3339 time")
		command.Flags().StringVar(&before, "before", "", "Exclusive RFC3339 time")
		if summary {
			command.Flags().IntVar(&q.Offset, "offset", 0, "Skip ranked people")
		} else {
			command.Flags().Int64Var(&q.AfterID, "after-id", 0, "Continue after this observation message ID")
		}
		command.RunE = func(cmd *cobra.Command, _ []string) error {
			for _, bound := range []struct {
				text   string
				target **time.Time
			}{{after, &q.After}, {before, &q.Before}} {
				if bound.text == "" {
					continue
				}
				at, err := time.Parse(time.RFC3339Nano, bound.text)
				if err != nil {
					return fmt.Errorf("parse call time bound: %w", err)
				}
				*bound.target = &at
			}
			client, cleanup, err := open(cmd.Context())
			if err != nil {
				return err
			}
			defer cleanup()
			if summary {
				result, err := client.SummarizeCalls(cmd.Context(), q)
				if err != nil {
					return err
				}
				return write(cmd, result)
			}
			result, err := client.ListCallObservations(cmd.Context(), q)
			if err != nil {
				return err
			}
			return write(cmd, result)
		}
		root.AddCommand(command)
	}
	return root
}

func init() {
	rootCmd.AddCommand(newCallsCommand(callCommandDeps{open: func(ctx context.Context) (callCommandClient, func(), error) {
		client, _, err := OpenHTTPStore(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		return client, func() { _ = client.Close() }, nil
	}}))
}
