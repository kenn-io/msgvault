package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

// messageExportMinAPISchemaVersion is the first daemon schema with exportCLIMessages.
const messageExportMinAPISchemaVersion = "5.19.0"

type exportMessagesDeps struct {
	openStore func(context.Context) (*store.Store, func(), error)
}

type exportMessagesOptions struct {
	PersonID     int64
	Start        string
	End          string
	Format       string
	MessageTypes []string
	Sources      []string
}

func defaultExportMessagesDeps() exportMessagesDeps {
	return exportMessagesDeps{openStore: func(ctx context.Context) (*store.Store, func(), error) {
		return openReadOnlyStoreForInvocation(ctx, invocationFromContext(ctx))
	}}
}

func newExportMessagesCmd(deps exportMessagesDeps) *cobra.Command {
	cmd := newExportMessagesLocalCmd(deps)
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if isDaemonCLISubprocess() {
			return runLocal(cmd, args)
		}
		request, err := exportMessagesRequestFromFlags(cmd)
		if err != nil {
			return err
		}
		return runExportMessagesHTTP(cmd, request)
	}
	return cmd
}

func newExportMessagesLocalCmd(deps exportMessagesDeps) *cobra.Command {
	opts := exportMessagesOptions{Format: "jsonl"}
	cmd := &cobra.Command{
		Use:   "export-messages",
		Short: "Export messages in a time window as provider-neutral JSONL",
		Long: `Write messages sent in [--start, --end) to stdout as JSON Lines. Records
have a record_type: manifest first, then source, conversation, and message
records, and a final complete record with counts.

--start and --end are required RFC3339 timestamps. Narrow the export with
--source <type>:<email> (type and email from 'msgvault list-accounts
--json', for example mbox:you@example.com), --message-type (exact values
such as email, sms, whatsapp, meeting_transcript; unknown values match
nothing), or --person-id from 'msgvault person list'.`,
		Example: `  msgvault export-messages --start 2026-09-01T00:00:00Z --end 2026-10-01T00:00:00Z > sept.jsonl
  msgvault export-messages --start 2026-09-01T00:00:00Z --end 2026-10-01T00:00:00Z --source mbox:you@example.com`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExportMessages(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Start, "start", "", "inclusive RFC3339 lower bound (required)")
	cmd.Flags().StringVar(&opts.End, "end", "", "exclusive RFC3339 upper bound (required)")
	cmd.Flags().StringArrayVar(
		&opts.MessageTypes, "message-type", nil, "exact message type to include (repeatable)",
	)
	cmd.Flags().StringArrayVar(
		&opts.Sources, "source", nil, "source selector type:email from list-accounts --json (repeatable)",
	)
	cmd.Flags().Int64Var(&opts.PersonID, "person-id", 0, "export messages for the durable person's bound participants")
	cmd.Flags().StringVar(&opts.Format, "format", "jsonl", "output format (jsonl)")
	_ = cmd.MarkFlagRequired("start")
	_ = cmd.MarkFlagRequired("end")
	return cmd
}

func runExportMessages(
	cmd *cobra.Command,
	deps exportMessagesDeps,
	_ exportMessagesOptions,
) error {
	request, err := exportMessagesRequestFromFlags(cmd)
	if err != nil {
		return err
	}
	if deps.openStore == nil {
		return errors.New("open message archive is not configured")
	}
	st, cleanup, err := deps.openStore(cmd.Context())
	if err != nil {
		return err
	}
	defer cleanup()
	messages, err := export.OpenMessages(cmd.Context(), st, request)
	if err != nil {
		return err
	}
	defer messages.Close()
	return messages.Write(cmd.OutOrStdout(), Version)
}

// exportMessagesRequestFromFlags validates the flags before any archive access.
func exportMessagesRequestFromFlags(cmd *cobra.Command) (export.MessagesRequest, error) {
	flags := cmd.Flags()
	format, _ := flags.GetString("format")
	start, _ := flags.GetString("start")
	end, _ := flags.GetString("end")
	messageTypes, _ := flags.GetStringArray("message-type")
	sources, _ := flags.GetStringArray("source")
	var personID *int64
	if flags.Changed("person-id") {
		id, _ := flags.GetInt64("person-id")
		personID = &id
	}
	request, err := export.ParseMessagesRequest(start, end, messageTypes, sources, personID)
	if err != nil {
		return export.MessagesRequest{}, err
	}
	if format != "jsonl" {
		return export.MessagesRequest{}, fmt.Errorf("unsupported --format %q (expected jsonl)", format)
	}
	return request, nil
}

// runExportMessagesHTTP streams the export from the local daemon or a remote.
func runExportMessagesHTTP(cmd *cobra.Command, request export.MessagesRequest) error {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	supported, err := client.SupportsAPISchemaVersion(cmd.Context(), messageExportMinAPISchemaVersion)
	if err != nil {
		return fmt.Errorf("check server for message export: %w", err)
	}
	if !supported {
		return fmt.Errorf("export-messages needs a server with API schema %s or newer; upgrade msgvault on the server", messageExportMinAPISchemaVersion)
	}
	body, err := client.OpenMessageExport(cmd.Context(), request)
	if err != nil {
		return err
	}
	out := &lastLineWriter{w: cmd.OutOrStdout()}
	_, copyErr := io.Copy(out, body)
	if err := errors.Join(copyErr, body.Close()); err != nil {
		return err
	}
	// The server reports a failure after the first byte only by ending the
	// stream without its completion record.
	var last struct {
		RecordType string `json:"record_type"`
	}
	if json.Unmarshal(out.last, &last) != nil || last.RecordType != "complete" {
		return errors.New("message export ended without its completion record; the output is incomplete")
	}
	return nil
}

// maxCompletionRecordBytes bounds what lastLineWriter keeps of a line. The
// completion record holds only counts, so it is far smaller.
const maxCompletionRecordBytes = 4096

// lastLineWriter forwards every byte and keeps the last complete line when it
// fits in maxCompletionRecordBytes.
type lastLineWriter struct {
	w        io.Writer
	line     []byte
	overflow bool
	last     []byte
}

func (l *lastLineWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	for rest := p[:n]; len(rest) > 0; {
		chunk, after, found := bytes.Cut(rest, []byte("\n"))
		if !l.overflow && len(l.line)+len(chunk) <= maxCompletionRecordBytes {
			l.line = append(l.line, chunk...)
		} else {
			l.overflow = true
		}
		if !found {
			break
		}
		l.last = nil
		if !l.overflow {
			l.last = append(l.last, l.line...)
		}
		l.line, l.overflow, rest = l.line[:0], false, after
	}
	return n, err
}

func init() {
	rootCmd.AddCommand(newExportMessagesCmd(defaultExportMessagesDeps()))
}
