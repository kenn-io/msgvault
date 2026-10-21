package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const callsMinAPIVersion = "5.2.0"
const slackCallsMinAPIVersion = "5.3.0"

type callCommandClient interface {
	ImportSlackHuddle(ctx context.Context, messageID int64) (*generated.CallImportResponse, error)
	APISchemaVersion(ctx context.Context) (string, error)
	ImportCallObservation(ctx context.Context, data []byte) (*generated.CallImportResponse, error)
	ListCallObservations(ctx context.Context, q store.CallQuery) (*generated.CallEvidenceResponse, error)
	SummarizeCalls(ctx context.Context, q store.CallQuery) (*generated.CallSummaryResponse, error)
}
type callCommandDeps struct {
	open func(context.Context) (callCommandClient, func(), error)
}

func newCallsCommand(deps callCommandDeps) *cobra.Command {
	root := &cobra.Command{
		Use: "calls", Short: "Import and inspect call history",
		Long: "Read call evidence through the daemon. Invitations never count as calls. All output is JSON.",
	}
	open := func(ctx context.Context, minimum string) (callCommandClient, func(), error) {
		client, cleanup, err := deps.open(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		version, err := client.APISchemaVersion(ctx)
		if err != nil || !daemonclient.APISchemaVersionAtLeast(version, minimum) {
			cleanup()
			if err != nil {
				return nil, func() {}, err
			}
			return nil, func() {}, fmt.Errorf("calls require daemon API %s or newer; daemon reports %s", minimum, version)
		}
		return client, cleanup, nil
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
			var envelope struct {
				Format string `json:"format"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				return err
			}
			minimum := callsMinAPIVersion
			if envelope.Format == "slack_huddle" {
				minimum = slackCallsMinAPIVersion
			}
			client, cleanup, err := open(cmd.Context(), minimum)
			if err != nil {
				return err
			}
			defer cleanup()
			result, err := client.ImportCallObservation(cmd.Context(), data)
			if err != nil {
				return err
			}
			return writeJSONOutput(cmd, result)
		},
	}
	root.AddCommand(importCommand)
	root.AddCommand(&cobra.Command{
		Use: "import-slack <message-id>", Short: "Derive call evidence from one archived Slack huddle", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return errors.New("message-id must be a positive archive ID")
			}
			client, cleanup, err := open(cmd.Context(), slackCallsMinAPIVersion)
			if err != nil {
				return err
			}
			defer cleanup()
			result, err := client.ImportSlackHuddle(cmd.Context(), id)
			if err != nil {
				return err
			}
			return writeJSONOutput(cmd, result)
		},
	})
	for _, summary := range []bool{false, true} {
		name, description, limitHelp := "list", "List imported calls and who joined", "Maximum calls to return (1–100)"
		if summary {
			name, description, limitHelp = "summary", "Rank people by confirmed answered calls", "Maximum people to return (1–100)"
		}
		var q store.CallQuery
		var after, before string
		command := &cobra.Command{Use: name, Short: description, Args: cobra.NoArgs}
		command.Flags().Int64Var(&q.PersonID, "person-id", 0, "Filter by durable person ID")
		command.Flags().IntVar(&q.Limit, "limit", 50, limitHelp)
		command.Flags().StringVar(&after, "after", "", "Inclusive RFC3339 time")
		command.Flags().StringVar(&before, "before", "", "Exclusive RFC3339 time")
		if summary {
			command.Flags().IntVar(&q.Offset, "offset", 0, "Skip ranked people")
		} else {
			command.Flags().Int64Var(&q.AfterID, "after-id", 0, "Continue after this call's message ID")
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
			client, cleanup, err := open(cmd.Context(), callsMinAPIVersion)
			if err != nil {
				return err
			}
			defer cleanup()
			if summary {
				result, err := client.SummarizeCalls(cmd.Context(), q)
				if err != nil {
					return err
				}
				return writeJSONOutput(cmd, result)
			}
			result, err := client.ListCallObservations(cmd.Context(), q)
			if err != nil {
				return err
			}
			return writeJSONOutput(cmd, result)
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
