package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	stdmime "mime"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

type draftForwardIntent struct {
	MessageID   int64
	From        string
	To          []string
	Cc          []string
	Bcc         []string
	Body        string
	JSON        bool
	Account     string
	SourceID    int64
	SourceIDSet bool
}

type draftForwardProblem struct {
	Filename string `json:"filename,omitempty"`
	PartKey  string `json:"part_key,omitempty"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
}

type draftForwardPreflightOutput struct {
	Status   string                `json:"status"`
	Problems []draftForwardProblem `json:"problems"`
}

func invalidDraftForwardArgs(format string, args ...any) (draftForwardIntent, error) {
	return draftForwardIntent{}, draftReplyError("invalid_args", fmt.Errorf(format, args...))
}

func parseDraftForwardArgs(args []string) (draftForwardIntent, error) {
	if !api.IsCLIRunDraftForward(args) {
		return invalidDraftForwardArgs("expected %s as the first argument", api.CLIRunDraftForwardCommand)
	}
	var intent draftForwardIntent
	var positional string
	var fromSet, bodySet, jsonSet, accountSet, sourceIDSet bool
	rest := args[1:]
	for len(rest) > 0 {
		arg := rest[0]
		rest = rest[1:]
		nameValue, ok := strings.CutPrefix(arg, "--")
		if !ok {
			if positional != "" {
				return invalidDraftForwardArgs("expected exactly one message ID")
			}
			positional = arg
			continue
		}
		name, value, hasValue := strings.Cut(nameValue, "=")
		switch name {
		case draftFromFlag, "account", "source-id", "body", "to", "cc", "bcc":
			if !hasValue {
				if len(rest) == 0 {
					return invalidDraftForwardArgs("--%s requires a value", name)
				}
				value, rest = rest[0], rest[1:]
			}
			switch name {
			case draftFromFlag:
				if fromSet || strings.TrimSpace(value) == "" {
					return invalidDraftForwardArgs("--from must be given once with a value")
				}
				intent.From, fromSet = value, true
			case "account":
				if accountSet || strings.TrimSpace(value) == "" {
					return invalidDraftForwardArgs("--account must be given once with a value")
				}
				intent.Account, accountSet = strings.TrimSpace(value), true
			case "source-id":
				if sourceIDSet {
					return invalidDraftForwardArgs("--source-id given more than once")
				}
				id, parseErr := parsePositiveDraftID(value)
				if parseErr != nil {
					return invalidDraftForwardArgs("source ID must be a positive integer")
				}
				intent.SourceID, intent.SourceIDSet, sourceIDSet = id, true, true
			case "body":
				if bodySet {
					return invalidDraftForwardArgs("--body given more than once")
				}
				intent.Body, bodySet = value, true
			case "to":
				if strings.TrimSpace(value) == "" {
					return invalidDraftForwardArgs("--to must not be empty")
				}
				intent.To = append(intent.To, value)
			case "cc":
				if strings.TrimSpace(value) == "" {
					return invalidDraftForwardArgs("--cc must not be empty")
				}
				intent.Cc = append(intent.Cc, value)
			case "bcc":
				if strings.TrimSpace(value) == "" {
					return invalidDraftForwardArgs("--bcc must not be empty")
				}
				intent.Bcc = append(intent.Bcc, value)
			}
		case "json":
			if jsonSet || (hasValue && value != "true") {
				return invalidDraftForwardArgs("--json accepts one flag without a value")
			}
			intent.JSON, jsonSet = true, true
		case "log-level", "verbose", "log-sql", "log-sql-slow-ms":
			if !hasValue && name != "verbose" && name != "log-sql" && len(rest) > 0 {
				rest = rest[1:]
			}
		default:
			return invalidDraftForwardArgs("unknown flag --%s", name)
		}
	}
	if accountSet == sourceIDSet {
		return invalidDraftForwardArgs("exactly one of --account or --source-id is required")
	}
	if len(intent.To)+len(intent.Cc)+len(intent.Bcc) == 0 {
		return invalidDraftForwardArgs("at least one of --to, --cc, or --bcc is required")
	}
	messageID, err := parsePositiveDraftID(positional)
	if err != nil {
		return invalidDraftForwardArgs("message ID must be a positive integer")
	}
	intent.MessageID = messageID
	return intent, nil
}

func parsePositiveDraftID(value string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("positive integer required")
	}
	return id, nil
}

func (a *storeAPIAdapter) runCLIForwardDraft(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	if len(req.Env) != 0 || req.Cwd != "" {
		return draftReplyError("invalid_args", errors.New("draft-forward accepts no environment or working directory"))
	}
	intent, err := parseDraftForwardArgs(req.Args)
	if err != nil {
		return err
	}
	target, from, _, err := a.resolveDraftTarget(
		ctx, &intent.MessageID, intent.Account, intent.SourceID, intent.SourceIDSet,
		intent.From, req.Grant, draftOperationForward,
	)
	if err != nil {
		return err
	}
	parent, err := msgmime.Parse(target.raw)
	if err != nil {
		return draftReplyError("invalid_parent", fmt.Errorf("parse parent MIME: %w", err))
	}
	readerAvailable := a.attachmentMaintenance != nil && a.attachmentMaintenance.blob != nil
	maintenance := a.attachmentMaintenance
	if !readerAvailable {
		maintenance = nil
	}
	return runWithAttachmentMutation(ctx, maintenance, func(ctx context.Context) error {
		refs, err := a.store.MessageAttachmentRefsContext(ctx, target.parent.ID)
		if err != nil {
			return draftReplyError("attachment_preflight_failed", err)
		}
		if len(refs) > 0 || len(parent.Attachments) > 0 {
			if !readerAvailable {
				return a.emitDraftForwardPreflight(emit, intent.JSON, []draftForwardProblem{{Reason: "attachment_reader_unavailable"}})
			}
		}
		attachments, problems := a.readForwardAttachments(ctx, parent, refs)
		if len(problems) != 0 {
			return a.emitDraftForwardPreflight(emit, intent.JSON, problems)
		}
		headerSummary, err := forwardHeaderSummary(target.raw)
		if err != nil {
			return draftReplyError("invalid_parent", err)
		}
		subject := parent.Subject
		draft, err := imaplib.BuildForward(imaplib.ForwardOptions{
			From: from, To: intent.To, Cc: intent.Cc, Bcc: intent.Bcc,
			Subject: subject, Body: intent.Body, QuotedHeader: headerSummary,
			QuotedText: parent.BodyText, QuotedHTML: parent.BodyHTML,
			Attachments: attachments,
		}, time.Now(), "")
		if err != nil {
			return a.emitDraftForwardPreflight(emit, intent.JSON, []draftForwardProblem{{
				Reason: "unsupported_metadata", Detail: err.Error(),
			}})
		}
		writes, err := prepareIMAPDraftAttachmentWrites(ctx, draft.Parsed, refs)
		if err != nil {
			return a.emitDraftForwardPreflight(emit, intent.JSON, []draftForwardProblem{{
				Reason: "unrepresentable_attachment", Detail: err.Error(),
			}})
		}
		target.attachmentWrites = &writes
		return a.createDraft(ctx, target, draft, intent.JSON, emit)
	})
}

func (a *storeAPIAdapter) readForwardAttachments(
	ctx context.Context,
	parsed *msgmime.Message,
	refs []store.AttachmentRef,
) ([]imaplib.ForwardAttachment, []draftForwardProblem) {
	attachments := make([]imaplib.ForwardAttachment, 0, len(parsed.Attachments))
	problems := make([]draftForwardProblem, 0)
	used := make([]bool, len(refs))
	for _, part := range parsed.Attachments {
		index := matchForwardAttachmentRef(part, refs, used)
		if index < 0 {
			problems = append(problems, draftForwardProblem{Filename: part.Filename, PartKey: part.PartKey, Reason: "missing_catalog_reference"})
			continue
		}
		used[index] = true
		ref := refs[index]
		if (ref.State != "" && ref.State != attachmentpolicy.StateStored) ||
			(ref.State == "" && ref.SkipReason != "") {
			state := string(ref.State)
			if state == "" {
				state = string(attachmentpolicy.StateSkipped)
			}
			problem := draftForwardProblem{Filename: ref.Filename, PartKey: ref.SourcePartKey, Reason: "attachment_" + state}
			if ref.SkipReason != "" {
				problem.Detail = string(ref.SkipReason)
			}
			problems = append(problems, problem)
			continue
		}
		if ref.ContentHash == "" {
			problems = append(problems, draftForwardProblem{Filename: ref.Filename, PartKey: ref.SourcePartKey, Reason: "missing_stored_file"})
			continue
		}
		reader, _, openErr := a.attachmentMaintenance.blob.OpenStream(ctx, ref.ContentHash)
		if openErr != nil {
			problems = append(problems, draftForwardProblem{Filename: ref.Filename, PartKey: ref.SourcePartKey, Reason: "unreadable_file", Detail: openErr.Error()})
			continue
		}
		content, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			detail := "read attachment failed"
			if readErr != nil {
				detail = readErr.Error()
			} else if closeErr != nil {
				detail = closeErr.Error()
			}
			problems = append(problems, draftForwardProblem{Filename: ref.Filename, PartKey: ref.SourcePartKey, Reason: "unreadable_file", Detail: detail})
			continue
		}
		if (ref.Size > 0 || len(content) == 0) && int64(ref.Size) != int64(len(content)) {
			problems = append(problems, draftForwardProblem{Filename: ref.Filename, PartKey: ref.SourcePartKey, Reason: "catalog_size_mismatch", Detail: fmt.Sprintf("catalog reports %d bytes, read %d", ref.Size, len(content))})
			continue
		}
		attachments = append(attachments, imaplib.ForwardAttachment{
			Filename: part.Filename, ContentType: part.ContentType, ContentID: part.ContentID,
			Disposition: part.Disposition, IsInline: part.IsInline, Content: content,
		})
	}
	for i, ref := range refs {
		if used[i] {
			continue
		}
		problems = append(problems, draftForwardProblem{Filename: ref.Filename, PartKey: ref.SourcePartKey, Reason: "unrepresented_attachment"})
	}
	return attachments, problems
}

func (a *storeAPIAdapter) emitDraftForwardPreflight(
	emit func(api.CLIRunEvent) error,
	asJSON bool,
	problems []draftForwardProblem,
) error {
	output := draftForwardPreflightOutput{Status: "attachment_preflight_failed", Problems: problems}
	if emit != nil {
		if asJSON {
			data, err := json.Marshal(output, json.Deterministic(true))
			if err != nil {
				return draftReplyError("output_failed", err)
			}
			if err := emit(api.CLIRunEvent{Type: cliStreamStderr, Data: string(data) + "\n"}); err != nil {
				return draftReplyError("output_failed", err)
			}
		} else {
			var text strings.Builder
			text.WriteString("draft-forward refused before APPEND:\n")
			for _, problem := range problems {
				fmt.Fprintf(&text, "attachment %q (%s): %s", problem.Filename, problem.PartKey, problem.Reason)
				if problem.Detail != "" {
					fmt.Fprintf(&text, " (%s)", problem.Detail)
				}
				text.WriteByte('\n')
			}
			if err := emit(api.CLIRunEvent{Type: cliStreamStderr, Data: text.String()}); err != nil {
				return draftReplyError("output_failed", err)
			}
		}
	}
	return draftReplyError("attachment_preflight_failed", errors.New("one or more archived attachments are unavailable"))
}

func forwardHeaderSummary(raw []byte) (string, error) {
	message, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return "", fmt.Errorf("read parent headers: %w", err)
	}
	lines := make([]string, 0, 5)
	for _, name := range []string{"From", "Date", "Subject", "To", "Cc"} {
		value := strings.TrimSpace(message.Header.Get(name))
		if value == "" {
			continue
		}
		if name == "Subject" {
			value = decodeForwardHeader(value)
		}
		lines = append(lines, name+": "+value)
	}
	return strings.Join(lines, "\r\n"), nil
}

func decodeForwardHeader(value string) string {
	decoded, err := new(stdmime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}
