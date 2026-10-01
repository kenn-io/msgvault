package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type draftForwardIntent struct {
	draftComposeIntent

	MessageID int64
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

// parseDraftForwardArgs takes one message ID plus draft-compose's flags,
// except --subject, which the forward derives from the parent.
func parseDraftForwardArgs(args []string) (draftForwardIntent, error) {
	invalid := func(format string, args ...any) (draftForwardIntent, error) {
		return draftForwardIntent{}, draftReplyError("invalid_args", fmt.Errorf(format, args...))
	}
	if !api.IsCLIRunDraftForward(args) {
		return invalid("expected %s as the first argument", api.CLIRunDraftForwardCommand)
	}
	composeArgs := []string{api.CLIRunDraftComposeCommand}
	var positional []string
	for i := 1; i < len(args); i++ {
		nameValue, isFlag := strings.CutPrefix(args[i], "--")
		name, _, hasValue := strings.Cut(nameValue, "=")
		switch {
		case !isFlag:
			positional = append(positional, args[i])
		case name == "subject":
			return invalid("unknown flag --subject")
		default:
			composeArgs = append(composeArgs, args[i])
			switch name {
			case draftFromFlag, "account", "source-id", "body", "to", "cc", "bcc", "log-level", "log-sql-slow-ms":
				if !hasValue && i+1 < len(args) {
					i++
					composeArgs = append(composeArgs, args[i])
				}
			}
		}
	}
	if len(positional) != 1 {
		return invalid("expected exactly one message ID")
	}
	messageID, err := strconv.ParseInt(strings.TrimSpace(positional[0]), 10, 64)
	if err != nil || messageID <= 0 {
		return invalid("message ID must be a positive integer")
	}
	compose, err := parseDraftComposeArgs(composeArgs)
	if err != nil {
		return draftForwardIntent{}, err
	}
	return draftForwardIntent{draftComposeIntent: compose, MessageID: messageID}, nil
}

func (a *storeAPIAdapter) runCLIForwardDraft(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	if req.Grant != nil {
		return draftReplyError("not_permitted", errors.New("draft-forward requires owner access"))
	}
	if len(req.Env) != 0 || req.Cwd != "" {
		return draftReplyError("invalid_args", errors.New("draft-forward accepts no environment or working directory"))
	}
	intent, err := parseDraftForwardArgs(req.Args)
	if err != nil {
		return err
	}
	target, from, _, err := a.resolveDraftTarget(
		ctx, &intent.MessageID, intent.Account, intent.SourceID, intent.SourceIDSet,
		intent.From, nil,
	)
	if err != nil {
		return err
	}
	if target.source.SourceType != "imap" {
		return draftReplyError("draft_disabled", errors.New("draft-forward requires an IMAP source"))
	}
	parent, err := msgmime.Parse(target.raw)
	if err != nil {
		return draftReplyError("invalid_parent", fmt.Errorf("parse parent MIME: %w", err))
	}
	// Sync stores UTF-8-normalized filenames; match against the same form.
	for i := range parent.Attachments {
		parent.Attachments[i].Filename = textutil.EnsureUTF8(parent.Attachments[i].Filename)
	}
	maintenance := a.attachmentMaintenance
	if maintenance != nil && maintenance.blob == nil {
		maintenance = nil
	}
	return runWithAttachmentMutation(ctx, maintenance, func(ctx context.Context) error {
		refs, err := a.store.MessageMIMEAttachmentsContext(ctx, target.parent.ID)
		if err != nil {
			return draftReplyError("attachment_preflight_failed", err)
		}
		if maintenance == nil && (len(refs) > 0 || len(parent.Attachments) > 0) {
			return a.emitDraftForwardPreflight(emit, intent.JSON, []draftForwardProblem{{Reason: "attachment_reader_unavailable"}})
		}
		attachments, problems := a.readForwardAttachments(ctx, parent, refs)
		if len(problems) != 0 {
			return a.emitDraftForwardPreflight(emit, intent.JSON, problems)
		}
		draft, err := imaplib.BuildForward(imaplib.ForwardOptions{
			From: from, To: intent.To, Cc: intent.Cc, Bcc: intent.Bcc,
			Subject: parent.Subject, Body: intent.Body, QuotedHeader: forwardHeaderSummary(parent),
			QuotedText:  textutil.EnsureUTF8(parent.GetBodyText()),
			Attachments: attachments,
		}, time.Now(), "")
		if err != nil {
			return draftReplyError("invalid_forward_metadata", err)
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
	boundHashes := make(map[string]bool, len(refs))
	for _, part := range msgmime.DistinctAttachments(parsed.Attachments) {
		if part.Size == 0 {
			// Sync stores no file or row for an empty part; forward it as is.
			attachments = append(attachments, imaplib.ForwardAttachment{
				Filename: part.Filename, ContentType: part.ContentType, ContentID: part.ContentID,
				Disposition: part.Disposition, IsInline: part.IsInline,
			})
			continue
		}
		index := matchForwardAttachmentRef(part, refs, used)
		if index < 0 {
			problems = append(problems, draftForwardProblem{Filename: part.Filename, PartKey: part.PartKey, Reason: "missing_catalog_reference"})
			continue
		}
		used[index] = true
		ref := refs[index]
		boundHashes[strings.ToLower(ref.ContentHash)] = true
		if (ref.State != "" && ref.State != attachmentpolicy.StateStored) ||
			(ref.State == "" && ref.SkipReason != "") {
			state := string(ref.State)
			if state == "" {
				state = string(attachmentpolicy.StateSkipped)
			}
			problem := draftForwardProblem{Filename: ref.Filename, PartKey: part.PartKey, Reason: "attachment_" + state}
			if ref.SkipReason != "" {
				problem.Detail = string(ref.SkipReason)
			}
			problems = append(problems, problem)
			continue
		}
		reader, _, openErr := a.attachmentMaintenance.blob.OpenStream(ctx, ref.ContentHash)
		if openErr != nil {
			problems = append(problems, draftForwardProblem{Filename: ref.Filename, PartKey: part.PartKey, Reason: "unreadable_file", Detail: openErr.Error()})
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
			problems = append(problems, draftForwardProblem{Filename: ref.Filename, PartKey: part.PartKey, Reason: "unreadable_file", Detail: detail})
			continue
		}
		if len(content) != part.Size {
			problems = append(problems, draftForwardProblem{Filename: ref.Filename, PartKey: part.PartKey, Reason: "catalog_size_mismatch", Detail: fmt.Sprintf("parent part has %d bytes, read %d", part.Size, len(content))})
			continue
		}
		attachments = append(attachments, imaplib.ForwardAttachment{
			Filename: part.Filename, ContentType: part.ContentType, ContentID: part.ContentID,
			Disposition: part.Disposition, IsInline: part.IsInline, Content: content,
		})
	}
	for i, ref := range refs {
		// A keyless legacy row shadowed by a bound copy of its bytes is not a separate occurrence.
		if used[i] || (ref.SourcePartKey == "" && boundHashes[strings.ToLower(ref.ContentHash)]) {
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

func forwardHeaderSummary(parent *msgmime.Message) string {
	lines := make([]string, 0, 5)
	add := func(name, value string) {
		if value = strings.TrimSpace(textutil.EnsureUTF8(value)); value != "" {
			lines = append(lines, name+": "+value)
		}
	}
	add("From", forwardAddressList(parent.From))
	add("Date", parent.RawDateHeader)
	add("Subject", parent.Subject)
	add("To", forwardAddressList(parent.To))
	add("Cc", forwardAddressList(parent.Cc))
	return strings.Join(lines, "\r\n")
}

func forwardAddressList(addresses []msgmime.Address) string {
	values := make([]string, len(addresses))
	for i, address := range addresses {
		// Summary is body text, so write names unencoded.
		values[i] = address.Email
		if address.Name != "" {
			values[i] = address.Name + " <" + address.Email + ">"
		}
	}
	return strings.Join(values, ", ")
}
