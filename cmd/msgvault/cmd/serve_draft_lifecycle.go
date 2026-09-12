package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/api"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

const draftLifecycleDiscarded = "discarded"

// draftClient contains the read-only IMAP operations used by draft-get.
type draftClient interface {
	InspectDraft(context.Context, imaplib.DraftTarget) (imaplib.DraftInspectResult, error)
	Close() error
}

// draftLifecycleIntent holds the parsed arguments for draft-get.
type draftLifecycleIntent struct {
	Command string
	DraftID int64
	JSON    bool
}

// draftLifecycleOutput is the JSON output for draft-get.
type draftLifecycleOutput struct {
	Status          string `json:"status"`
	DraftID         int64  `json:"draft_id,omitempty"`
	Lifecycle       string `json:"lifecycle,omitempty"`
	Revision        int64  `json:"revision,omitempty"`
	RFC822MessageID string `json:"rfc822_message_id,omitempty"`
	ProviderStatus  string `json:"provider_status,omitempty"`
	SourceID        int64  `json:"source_id,omitempty"`
	Mailbox         string `json:"mailbox,omitempty"`
	UID             uint32 `json:"uid,omitempty"`
	UIDValidity     uint32 `json:"uidvalidity,omitempty"`
	Subject         string `json:"subject,omitempty"`
	FromAddress     string `json:"from_address,omitempty"`
	BodyText        string `json:"body_text,omitempty"`
}

func parseDraftLifecycleArgs(args []string) (draftLifecycleIntent, error) {
	if len(args) == 0 {
		return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("no command provided"))
	}
	if !api.IsCLIRunDraftLifecycle(args) {
		return draftLifecycleIntent{}, draftReplyError("invalid_args", fmt.Errorf("unexpected command %q", args[0]))
	}

	intent := draftLifecycleIntent{Command: args[0]}
	var positional string
	var jsonSet bool
	for rest := args[1:]; len(rest) > 0; {
		arg := rest[0]
		rest = rest[1:]
		nameValue, ok := strings.CutPrefix(arg, "--")
		if !ok {
			if positional != "" {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("expected exactly one draft ID"))
			}
			positional = arg
			continue
		}

		name, value, hasValue := strings.Cut(nameValue, "=")
		switch name {
		case "json":
			if jsonSet || (hasValue && value != "true") {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--json accepts no value and may appear once"))
			}
			intent.JSON = true
			jsonSet = true
		case "log-level", "log-sql-slow-ms":
			if hasValue {
				continue
			}
			if len(rest) == 0 {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", fmt.Errorf("--%s requires a value", name))
			}
			rest = rest[1:]
		case "verbose", "log-sql":
			if hasValue && value != "true" && value != "false" {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", fmt.Errorf("--%s accepts true or false", name))
			}
		default:
			return draftLifecycleIntent{}, draftReplyError("invalid_args", fmt.Errorf("unknown flag --%s", name))
		}
	}

	id, err := strconv.ParseInt(positional, 10, 64)
	if err != nil || id <= 0 {
		return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("draft ID must be a positive integer"))
	}
	intent.DraftID = id
	return intent, nil
}

func marshalDraftLifecycleOutput(output draftLifecycleOutput) []byte {
	data, err := json.Marshal(output)
	if err != nil {
		return []byte(`{"status":"output_encoding_failed"}`)
	}
	return data
}

func emitDraftLifecycleOutput(
	emit func(api.CLIRunEvent) error,
	stream string,
	asJSON bool,
	output draftLifecycleOutput,
) error {
	if emit == nil {
		return nil
	}
	var data string
	if asJSON {
		data = string(marshalDraftLifecycleOutput(output)) + "\n"
	} else if output.Status == "active" {
		data = fmt.Sprintf(
			"draft %d: lifecycle=%s revision=%d provider=%s\n"+
				"subject: %s\nfrom: %s\nmessage_id: %s\nsource_id: %d\n"+
				"mailbox: %s\nuidvalidity: %d\nuid: %d\nbody:\n%s\n",
			output.DraftID, output.Lifecycle, output.Revision, output.ProviderStatus,
			output.Subject, output.FromAddress, output.RFC822MessageID, output.SourceID,
			output.Mailbox, output.UIDValidity, output.UID, output.BodyText)
	} else {
		data = fmt.Sprintf("draft %d: lifecycle=%s revision=%d\n",
			output.DraftID, output.Lifecycle, output.Revision)
	}
	return emit(api.CLIRunEvent{Type: stream, Data: data})
}

func draftInspectionClient(ctx context.Context, adapter *storeAPIAdapter, source *store.Source) (draftClient, error) {
	if adapter.draftClientFactory != nil {
		return adapter.draftClientFactory(ctx, source)
	}
	return defaultDraftClientFactory(ctx, source)
}

func validateDraftSource(source *store.Source) error {
	if source.SourceType != "imap" {
		return fmt.Errorf("source %d is type %q, not imap", source.ID, source.SourceType)
	}
	if !source.SyncConfig.Valid || strings.TrimSpace(source.SyncConfig.String) == "" {
		return errors.New("source has no IMAP sync config")
	}
	config, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
	if err != nil {
		return fmt.Errorf("decode source IMAP config: %w", err)
	}
	if config.Identifier() != source.Identifier {
		return errors.New("source IMAP config identifier does not match the source")
	}
	return nil
}

// runCLIDraftLifecycle dispatches the read-only draft-get route.
func (a *storeAPIAdapter) runCLIDraftLifecycle(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	if len(req.Env) != 0 || req.Cwd != "" {
		return draftReplyError("invalid_args", errors.New("draft lifecycle commands accept no environment or working directory"))
	}
	intent, err := parseDraftLifecycleArgs(req.Args)
	if err != nil {
		return err
	}
	return a.runCLIDraftGet(ctx, intent, emit)
}

// runCLIDraftGet reads the owned message and, when authorized, inspects its
// current provider copy. It does not acquire a sync lock or write local state.
func (a *storeAPIAdapter) runCLIDraftGet(
	ctx context.Context,
	intent draftLifecycleIntent,
	emit func(api.CLIRunEvent) error,
) error {
	draft, err := a.store.GetIMAPDraftContext(ctx, intent.DraftID)
	if err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", fmt.Errorf("draft %d: %w", intent.DraftID, err))
		}
		return draftReplyError("internal", fmt.Errorf("load draft %d: %w", intent.DraftID, err))
	}

	output := draftLifecycleOutput{
		Status:          draft.Lifecycle,
		DraftID:         draft.DraftID,
		Lifecycle:       draft.Lifecycle,
		Revision:        draft.Revision,
		RFC822MessageID: draft.RFC822MessageID,
		SourceID:        draft.SourceID,
		Mailbox:         draft.Mailbox,
		UID:             draft.UID,
		UIDValidity:     draft.UIDValidity,
		Subject:         draft.Subject,
		FromAddress:     draft.FromAddress,
	}
	if draft.Lifecycle == draftLifecycleDiscarded {
		return emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
	}
	bodyText, err := a.store.GetMessageBodyText(draft.CurrentMessageID)
	if err != nil {
		return draftReplyError("internal", fmt.Errorf("load body for draft %d: %w", draft.DraftID, err))
	}
	output.BodyText = bodyText

	source, err := a.store.GetSourceByIDContext(ctx, draft.SourceID)
	if err != nil {
		return draftReplyError("invalid_source", fmt.Errorf("load source %d: %w", draft.SourceID, err))
	}
	grantedMailbox, grantErr := authorizeIMAPDraft(a.draftPolicy, source.ID, source.SourceType)
	if grantErr != nil || grantedMailbox != draft.Mailbox {
		output.ProviderStatus = "not_checked"
		return emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
	}
	if err := validateDraftSource(source); err != nil {
		return draftReplyError("invalid_source", fmt.Errorf("validate source %d: %w", source.ID, err))
	}

	raw, err := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return draftReplyError("internal", fmt.Errorf("load raw MIME for draft %d: %w", draft.DraftID, err))
	}
	digest := sha256.Sum256(raw)
	client, err := draftInspectionClient(ctx, a, source)
	if err != nil {
		output.ProviderStatus = "unknown"
		return emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
	}
	defer func() { _ = client.Close() }()
	result, err := client.InspectDraft(ctx, imaplib.DraftTarget{
		Mailbox:     draft.Mailbox,
		UIDValidity: draft.UIDValidity,
		UID:         draft.UID,
		RawSHA256:   digest,
	})
	if err != nil {
		output.ProviderStatus = "unknown"
	} else {
		output.ProviderStatus = result.State
	}
	return emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
}
