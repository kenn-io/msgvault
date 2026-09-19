package cmd

import (
	"context"
	"database/sql"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

const (
	gmailDraftStatusCreated     = "created"
	gmailDraftStatusLocalFailed = "remote_accepted_local_failed"
)

// authorizeGmailDraft applies the daemon-start policy snapshot. Reads from
// the archive and owner-only send-as listing do not use this write grant.
func authorizeGmailDraft(policy []config.GmailDraftSource, sourceID int64, sourceType string) error {
	if sourceType != "gmail" {
		return draftReplyError("draft_disabled", fmt.Errorf("source %d is a %q source, not gmail", sourceID, sourceType))
	}
	for _, grant := range policy {
		if grant.SourceID == sourceID && grant.Enabled {
			return nil
		}
	}
	return draftReplyError("draft_disabled", fmt.Errorf("source %d has no enabled [[gmail.drafts]] grant", sourceID))
}

func gmailDraftScopeGate(source *store.Source, accepted []string) error {
	if source == nil {
		return draftReplyError("invalid_source", errors.New("missing Gmail source"))
	}
	if cfg != nil && cfg.OAuth.ServiceAccountKeyFor(sourceOAuthApp(source)) != "" {
		return nil
	}
	manager, err := oauthManagerCache()(sourceOAuthApp(source))
	if err != nil {
		return draftReplyError("invalid_source", err)
	}
	if !manager.HasScopeMetadata(source.Identifier) {
		return nil
	}
	if oauth.GrantCoversAnyScope(manager.GrantedScopes(source.Identifier), accepted) {
		return nil
	}
	return draftReplyError("insufficient_scope", fmt.Errorf(
		"saved Gmail grant for %s does not cover this operation; re-authorize with add-account %s --force",
		source.Identifier, source.Identifier,
	))
}

func defaultGmailDraftClientFactory(ctx context.Context, source *store.Source) (gmail.DraftAPI, error) {
	client, err := buildAPIClient(ctx, source, oauthManagerCache(), nil)
	if err != nil {
		return nil, err
	}
	draftClient, ok := client.(gmail.DraftAPI)
	if !ok {
		_ = client.Close()
		return nil, fmt.Errorf("source %d did not produce a Gmail draft client", source.ID)
	}
	return draftClient, nil
}

func validateGmailSendAs(entries []gmail.SendAs, from string) error {
	for _, entry := range entries {
		if store.EqualIdentifier(entry.Email, from) &&
			(entry.Primary || strings.EqualFold(entry.VerificationStatus, "accepted")) {
			return nil
		}
	}
	return draftReplyError("invalid_from", errors.New("--from is not a primary or accepted Gmail send-as identity"))
}

type gmailDraftReplyOutput struct {
	Status          string `json:"status"`
	DraftID         string `json:"draft_id,omitempty"`
	Revision        int64  `json:"revision,omitzero"`
	MessageID       int64  `json:"message_id,omitzero"`
	OperationRef    string `json:"operation_ref"`
	RFC822MessageID string `json:"rfc822_message_id"`
	SourceID        int64  `json:"source_id"`
	GmailDraftID    string `json:"gmail_draft_id,omitempty"`
	GmailMessageID  string `json:"gmail_message_id,omitempty"`
	ThreadID        string `json:"thread_id,omitempty"`
}

func (a *storeAPIAdapter) runGmailReplyDraft(
	ctx context.Context,
	intent draftReplyIntent,
	target draftReplyTarget,
	reply imaplib.ReplyDraft,
	messageIDValue string,
	emit func(api.CLIRunEvent) error,
) error {
	if err := gmailDraftScopeGate(target.source, oauth.ScopesGmailDraftWrite); err != nil {
		return err
	}
	if err := gmailDraftScopeGate(target.source, oauth.ScopesGmailSendAsList); err != nil {
		return err
	}
	execution, err := a.store.AcquireSyncExecutionContext(ctx, target.source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", fmt.Errorf("source %d: %w", target.source.ID, err))
		}
		return draftReplyError("sync_lock_failed", fmt.Errorf("source %d: %w", target.source.ID, err))
	}
	released := false
	defer func() {
		if !released {
			_ = execution.Release()
		}
	}()
	finish := func(refresh bool) {
		if released {
			return
		}
		released = true
		if err := execution.Release(); err != nil {
			logger.Error("release source after Gmail draft write", "source_id", target.source.ID, "error", err)
		}
		if refresh {
			evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
			defer cancelEvidence()
			a.refreshDraftCache(evidenceCtx, target.source)
		}
	}

	clientFactory := a.gmailDraftClientFactory
	if clientFactory == nil {
		clientFactory = defaultGmailDraftClientFactory
	}
	client, err := clientFactory(ctx, target.source)
	if err != nil {
		return draftReplyError("invalid_source", fmt.Errorf("build Gmail client for source %d: %w", target.source.ID, err))
	}
	defer func() { _ = client.Close() }()
	sendAs, err := client.ListSendAs(ctx)
	if err != nil {
		return draftReplyError(gmailReadErrorCode(err), err)
	}
	if err := validateGmailSendAs(sendAs, intent.From); err != nil {
		return err
	}
	draft, err := client.CreateDraft(ctx, reply.Raw, target.parent.SourceConversationID)
	if err != nil {
		return emitGmailDraftReplyFailure(emit, intent.JSON, target, messageIDValue, err)
	}
	if draft == nil {
		return emitGmailDraftReplyFailure(emit, intent.JSON, target, messageIDValue,
			&gmail.DraftWriteError{State: gmail.DraftStateRemoteUnknown, Code: "remote_unknown", Err: errors.New("Gmail create returned no draft")})
	}
	receipt := store.GmailDraftReceipt{
		SourceID: target.source.ID, GmailDraftID: draft.ID,
		GmailMessageID: draft.Message.ID, ThreadID: draft.Message.ThreadID,
	}
	result := gmailDraftReplyOutput{
		Status:          gmailDraftStatusCreated,
		OperationRef:    gmailDraftOperationRef(receipt),
		RFC822MessageID: messageIDValue, SourceID: target.source.ID,
		GmailDraftID: receipt.GmailDraftID, GmailMessageID: receipt.GmailMessageID,
		ThreadID: receipt.ThreadID,
	}
	evidenceCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	localDraft, err := a.store.PersistGmailDraftContext(
		evidenceCtx, receipt, gmailDraftParticipants(reply.Parsed),
		gmailDraftReplyPersistData(target, reply, receipt, messageIDValue),
	)
	if err != nil {
		result.Status = gmailDraftStatusLocalFailed
		_ = emitGmailDraftReplyOutput(emit, cliStreamStderr, intent.JSON, result)
		finish(true)
		return draftReplyError(gmailDraftStatusLocalFailed, err)
	}
	finish(true)
	result.DraftID = localDraft.DraftID
	result.Revision = localDraft.Revision
	result.MessageID = localDraft.CurrentMessageID
	if err := emitGmailDraftReplyOutput(emit, cliStreamStdout, intent.JSON, result); err != nil {
		return draftReplyError("output_failed", err)
	}
	return nil
}

func emitGmailDraftReplyFailure(
	emit func(api.CLIRunEvent) error,
	asJSON bool,
	target draftReplyTarget,
	rfc822 string,
	err error,
) error {
	code := gmailWriteErrorCode(err)
	result := gmailDraftReplyOutput{
		Status: code, OperationRef: fmt.Sprintf("%d:gmail:unknown", target.source.ID),
		RFC822MessageID: rfc822, SourceID: target.source.ID,
	}
	_ = emitGmailDraftReplyOutput(emit, cliStreamStderr, asJSON, result)
	return draftReplyError(code, gmailWriteCause(err))
}

func emitGmailDraftReplyOutput(
	emit func(api.CLIRunEvent) error,
	stream string,
	asJSON bool,
	output gmailDraftReplyOutput,
) error {
	if emit == nil {
		return nil
	}
	if asJSON {
		data, err := jsonv2.Marshal(output)
		if err != nil {
			return err
		}
		return emit(api.CLIRunEvent{Type: stream, Data: string(data) + "\n"})
	}
	if output.Status == gmailDraftStatusCreated {
		return emit(api.CLIRunEvent{Type: stream, Data: fmt.Sprintf(
			"created Gmail draft message %d (%s), operation %s, draft %s revision %d\n",
			output.MessageID, textutil.SanitizeTerminal(output.GmailMessageID),
			textutil.SanitizeTerminal(output.OperationRef), textutil.SanitizeTerminal(output.DraftID), output.Revision,
		)})
	}
	if output.RFC822MessageID != "" {
		if err := emit(api.CLIRunEvent{Type: stream, Data: fmt.Sprintf(
			"Gmail draft operation %s, RFC822 Message-ID: %s, inspect operation %s\n",
			textutil.SanitizeTerminal(output.Status),
			textutil.SanitizeTerminal(output.RFC822MessageID),
			textutil.SanitizeTerminal(output.OperationRef),
		)}); err != nil {
			return err
		}
		return nil
	}
	return emit(api.CLIRunEvent{Type: stream, Data: fmt.Sprintf(
		"Gmail draft operation %s, inspect operation %s\n",
		textutil.SanitizeTerminal(output.Status), textutil.SanitizeTerminal(output.OperationRef),
	)})
}

func gmailWriteErrorCode(err error) string {
	var writeErr *gmail.DraftWriteError
	if errors.As(err, &writeErr) && writeErr.Code != "" {
		return writeErr.Code
	}
	return "provider_rejected"
}

func gmailWriteCause(err error) error {
	var writeErr *gmail.DraftWriteError
	if errors.As(err, &writeErr) && writeErr.Err != nil {
		return writeErr.Err
	}
	return err
}

func gmailReadErrorCode(err error) string {
	if _, ok := errors.AsType[*gmail.NotFoundError](err); ok {
		return "provider_absent"
	}
	if gmail.IsInsufficientScopeError(err.Error()) {
		return "insufficient_scope"
	}
	return "provider_refused"
}

func gmailDraftOperationRef(receipt store.GmailDraftReceipt) string {
	return fmt.Sprintf("%d:gmail:%s", receipt.SourceID, receipt.GmailDraftID)
}

func gmailDraftParticipants(parsed *msgmime.Message) []store.ParticipantPersistData {
	addresses := append([]msgmime.Address(nil), parsed.From...)
	addresses = append(addresses, parsed.To...)
	addresses = append(addresses, parsed.Cc...)
	addresses = append(addresses, parsed.Bcc...)
	participants := make([]store.ParticipantPersistData, len(addresses))
	for i, address := range addresses {
		participants[i] = store.ParticipantPersistData{
			EmailAddress: address.Email, DisplayName: address.Name, Domain: address.Domain,
		}
	}
	return participants
}

func gmailDraftReplyPersistData(
	target draftReplyTarget,
	reply imaplib.ReplyDraft,
	receipt store.GmailDraftReceipt,
	messageIDValue string,
) func([]int64) *store.MessagePersistData {
	return gmailDraftMessagePersistData(target.source.ID, target.parent.ID, reply.Parsed, reply.Raw, receipt, messageIDValue)
}

func gmailDraftMessagePersistData(
	sourceID int64,
	replyToMessageID int64,
	parsed *msgmime.Message,
	raw []byte,
	receipt store.GmailDraftReceipt,
	rfc822 string,
) func([]int64) *store.MessagePersistData {
	return func(ids []int64) *store.MessagePersistData {
		fromCount := len(parsed.From)
		if fromCount == 0 {
			return nil
		}
		at := 0
		fromIDs := ids[at : at+fromCount]
		at += fromCount
		toIDs := ids[at : at+len(parsed.To)]
		at += len(parsed.To)
		ccIDs := ids[at : at+len(parsed.Cc)]
		at += len(parsed.Cc)
		bccIDs := ids[at : at+len(parsed.Bcc)]
		toAddresses := gmailAddressStrings(parsed.To)
		ccAddresses := gmailAddressStrings(parsed.Cc)
		bccAddresses := gmailAddressStrings(parsed.Bcc)
		fromAddresses := gmailAddressStrings(parsed.From)
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: sourceID, SourceMessageID: receipt.GmailMessageID,
				RFC822MessageID: sql.NullString{String: rfc822, Valid: rfc822 != ""},
				MessageType:     store.MessageTypeEmail, IsFromMe: true, IdentityDerivedIsFromMe: true,
				SenderID:         sql.NullInt64{Int64: fromIDs[0], Valid: true},
				ReplyToMessageID: sql.NullInt64{Int64: replyToMessageID, Valid: replyToMessageID > 0},
				Subject:          sql.NullString{String: parsed.Subject, Valid: parsed.Subject != ""},
				Snippet:          sql.NullString{String: strings.TrimSpace(parsed.BodyText), Valid: parsed.BodyText != ""},
				SentAt:           sql.NullTime{Time: parsed.Date, Valid: !parsed.Date.IsZero()},
				InternalDate:     sql.NullTime{Time: parsed.Date, Valid: !parsed.Date.IsZero()},
				SizeEstimate:     int64(len(raw)), ArchivedAt: time.Now(),
			},
			Conversation: &store.ConversationPersistData{
				SourceConversationID: receipt.ThreadID,
				ConversationType:     "email_thread", Title: parsed.Subject,
			},
			BodyText: sql.NullString{String: parsed.BodyText, Valid: true},
			BodyHTML: sql.NullString{String: parsed.BodyHTML, Valid: parsed.BodyHTML != ""},
			RawMIME:  raw, RawFormat: "mime",
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: fromIDs, EmailAddresses: fromAddresses},
				{Type: "to", ParticipantIDs: toIDs, EmailAddresses: toAddresses},
				{Type: "cc", ParticipantIDs: ccIDs, EmailAddresses: ccAddresses},
				{Type: "bcc", ParticipantIDs: bccIDs, EmailAddresses: bccAddresses},
			},
			LabelRefs: []store.MessageLabelRef{{SourceLabelID: "DRAFT", Info: store.LabelInfo{Name: "DRAFT", Type: "system"}}},
			FTS:       &store.FTSDoc{Subject: parsed.Subject, Body: parsed.BodyText, FromAddr: firstGmailAddress(parsed.From), ToAddrs: strings.Join(toAddresses, " ")},
		}
	}
}

func gmailAddressStrings(addresses []msgmime.Address) []string {
	result := make([]string, len(addresses))
	for i, address := range addresses {
		result[i] = address.Email
	}
	return result
}

func firstGmailAddress(addresses []msgmime.Address) string {
	if len(addresses) == 0 {
		return ""
	}
	return addresses[0].Email
}

type gmailDraftLifecycleReceipt struct {
	GmailDraftID   string `json:"gmail_draft_id"`
	GmailMessageID string `json:"gmail_message_id"`
	ThreadID       string `json:"thread_id"`
}

type gmailDraftLifecycleObservation struct {
	State          string `json:"state"`
	Code           string `json:"code,omitempty"`
	GmailDraftID   string `json:"gmail_draft_id,omitempty"`
	GmailMessageID string `json:"gmail_message_id,omitempty"`
	ThreadID       string `json:"thread_id,omitempty"`
	Present        bool   `json:"present"`
}

type gmailDraftLifecycleOutput struct {
	Status                           string                          `json:"status"`
	Provider                         string                          `json:"provider"`
	DraftID                          string                          `json:"draft_id"`
	Revision                         int64                           `json:"revision"`
	Lifecycle                        string                          `json:"lifecycle"`
	MessageID                        int64                           `json:"message_id"`
	SourceID                         int64                           `json:"source_id"`
	Receipt                          gmailDraftLifecycleReceipt      `json:"receipt"`
	Content                          string                          `json:"content,omitempty"`
	RawMIME                          string                          `json:"raw_mime,omitempty"`
	CandidateContent                 string                          `json:"candidate_content,omitempty"`
	PendingOperation                 string                          `json:"pending_operation,omitempty"`
	PendingCode                      string                          `json:"pending_code,omitempty"`
	PendingReplacementGmailMessageID string                          `json:"pending_replacement_gmail_message_id,omitempty"`
	ProviderObservation              *gmailDraftLifecycleObservation `json:"provider_observation,omitempty"`
	Observation                      *gmailDraftLifecycleObservation `json:"observation,omitempty"`
	ManualReconciliation             bool                            `json:"manual_reconciliation,omitempty"`
}

func (a *storeAPIAdapter) gmailDraftLifecycleOutput(
	ctx context.Context,
	draft store.GmailDraft,
	status string,
	providerObservation *gmailDraftLifecycleObservation,
	observation *gmailDraftLifecycleObservation,
) (gmailDraftLifecycleOutput, error) {
	message, err := a.store.GetMessageContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return gmailDraftLifecycleOutput{}, fmt.Errorf("load managed Gmail draft message: %w", err)
	}
	raw, err := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return gmailDraftLifecycleOutput{}, fmt.Errorf("load managed Gmail draft MIME: %w", err)
	}
	output := gmailDraftLifecycleOutputWithoutMessage(
		draft, status, providerObservation, observation,
	)
	output.Content = message.BodyText
	output.RawMIME = string(raw)
	return output, nil
}

func gmailDraftLifecycleOutputWithoutMessage(
	draft store.GmailDraft,
	status string,
	providerObservation *gmailDraftLifecycleObservation,
	observation *gmailDraftLifecycleObservation,
) gmailDraftLifecycleOutput {
	lifecycle := "active"
	if draft.DiscardedAt != nil {
		lifecycle = "discarded"
	}
	output := gmailDraftLifecycleOutput{
		Status: status, Provider: "gmail", DraftID: draft.DraftID,
		Revision: draft.Revision, Lifecycle: lifecycle, MessageID: draft.CurrentMessageID,
		SourceID: draft.SourceID,
		Receipt: gmailDraftLifecycleReceipt{
			GmailDraftID:   draft.CurrentReceipt.GmailDraftID,
			GmailMessageID: draft.CurrentReceipt.GmailMessageID,
			ThreadID:       draft.CurrentReceipt.ThreadID,
		},
		ProviderObservation: providerObservation, Observation: observation,
	}
	if draft.Pending != nil {
		output.PendingOperation = draft.Pending.Operation
		output.PendingCode = draft.Pending.Code
		output.CandidateContent = string(draft.Pending.Raw)
		output.PendingReplacementGmailMessageID = draft.Pending.ReplacementGmailMessageID
	}
	return output
}

func (a *storeAPIAdapter) reportGmailDraftAcceptedLocalFailure(
	emit func(api.CLIRunEvent) error,
	asJSON bool,
	claimed store.GmailDraft,
	replacement store.GmailDraftReceipt,
	replacementRaw []byte,
	cause error,
) error {
	output := gmailDraftLifecycleOutputWithoutMessage(
		claimed,
		"accepted_local_failed",
		&gmailDraftLifecycleObservation{
			State:          "present",
			Code:           "accepted_local_failed",
			GmailDraftID:   replacement.GmailDraftID,
			GmailMessageID: replacement.GmailMessageID,
			ThreadID:       replacement.ThreadID,
			Present:        true,
		},
		nil,
	)
	output.PendingOperation = store.GmailDraftOperationEdit
	output.PendingCode = "accepted_local_failed"
	output.CandidateContent = string(replacementRaw)
	output.PendingReplacementGmailMessageID = replacement.GmailMessageID
	output.ManualReconciliation = true
	if claimed.Pending != nil {
		output.PendingOperation = claimed.Pending.Operation
		if len(claimed.Pending.Raw) != 0 {
			output.CandidateContent = string(claimed.Pending.Raw)
		}
	}
	if err := emitGmailDraftLifecycleOutput(emit, cliStreamStderr, asJSON, output); err != nil {
		return draftReplyError("accepted_local_failed", errors.Join(cause, err))
	}
	return draftReplyError("accepted_local_failed", cause)
}

func emitGmailDraftLifecycleOutput(
	emit func(api.CLIRunEvent) error,
	stream string,
	asJSON bool,
	output gmailDraftLifecycleOutput,
) error {
	if emit == nil {
		return nil
	}
	if asJSON {
		data, err := jsonv2.Marshal(output)
		if err != nil {
			return err
		}
		return emit(api.CLIRunEvent{Type: stream, Data: string(data) + "\n"})
	}
	var data strings.Builder
	fmt.Fprintf(&data, "draft %s revision %d %s\n",
		textutil.SanitizeTerminal(output.DraftID), output.Revision,
		textutil.SanitizeTerminal(output.Lifecycle))
	fmt.Fprintf(&data, "status: %s\n", textutil.SanitizeTerminal(output.Status))
	fmt.Fprintf(&data, "receipt (revision %d): %s\n", output.Revision,
		textutil.SanitizeTerminal(formatGmailDraftLifecycleReceipt(output.Receipt)))
	fmt.Fprintf(&data, "content:\n%s\n",
		strings.TrimRight(textutil.SanitizeTerminalMultiline(output.Content), "\n"))
	if output.PendingOperation != "" {
		fmt.Fprintf(&data, "pending operation: %s\n", textutil.SanitizeTerminal(output.PendingOperation))
	}
	if output.CandidateContent != "" {
		fmt.Fprintf(&data, "candidate content:\n%s\n",
			strings.TrimRight(textutil.SanitizeTerminalMultiline(output.CandidateContent), "\n"))
	}
	if output.PendingReplacementGmailMessageID != "" {
		fmt.Fprintf(&data, "pending replacement Gmail message ID: %s\n",
			textutil.SanitizeTerminal(output.PendingReplacementGmailMessageID))
	}
	if output.Status == "accepted_local_failed" && output.ProviderObservation != nil &&
		output.ProviderObservation.State == "present" && output.ProviderObservation.Present {
		fmt.Fprintf(&data, "acknowledged replacement receipt: %s\n",
			textutil.SanitizeTerminal(formatGmailDraftLifecycleObservation(*output.ProviderObservation)))
	}
	if output.Observation != nil && output.Status == "pending" {
		fmt.Fprintf(&data, "old provider receipt: %s\n",
			textutil.SanitizeTerminal(formatGmailDraftLifecycleObservation(*output.Observation)))
	}
	providerOutcome := output.PendingCode
	if providerOutcome == "" {
		observations := []*gmailDraftLifecycleObservation{output.ProviderObservation, output.Observation}
		if output.Status == "pending" {
			observations = []*gmailDraftLifecycleObservation{output.Observation, output.ProviderObservation}
		}
		for _, observation := range observations {
			if observation == nil {
				continue
			}
			providerOutcome = observation.Code
			if providerOutcome == "" {
				providerOutcome = observation.State
			}
			if providerOutcome != "" {
				break
			}
		}
	}
	if providerOutcome != "" {
		fmt.Fprintf(&data, "provider outcome: %s\n", textutil.SanitizeTerminal(providerOutcome))
	}
	if output.Status == "pending" || output.Status == "accepted_local_failed" || output.ManualReconciliation {
		fmt.Fprintf(&data, "old draft ID remains blocked at revision %d\n", output.Revision)
		fmt.Fprintln(&data, "manual action: reconcile the provider receipt and local state before retrying")
	}
	return emit(api.CLIRunEvent{Type: stream, Data: data.String()})
}

func formatGmailDraftLifecycleReceipt(receipt gmailDraftLifecycleReceipt) string {
	return fmt.Sprintf("gmail_draft_id=%s gmail_message_id=%s thread_id=%s",
		receipt.GmailDraftID, receipt.GmailMessageID, receipt.ThreadID)
}

func formatGmailDraftLifecycleObservation(observation gmailDraftLifecycleObservation) string {
	return fmt.Sprintf("state=%s code=%s gmail_draft_id=%s gmail_message_id=%s thread_id=%s present=%t",
		observation.State, observation.Code, observation.GmailDraftID,
		observation.GmailMessageID, observation.ThreadID, observation.Present)
}

func (a *storeAPIAdapter) loadManagedGmailDraftSource(ctx context.Context, draft store.GmailDraft) (*store.Source, error) {
	source, err := a.store.GetSourceByIDContext(ctx, draft.SourceID)
	if err != nil {
		return nil, draftReplyError("invalid_source", err)
	}
	if err := authorizeGmailDraft(a.gmailDraftPolicy, source.ID, source.SourceType); err != nil {
		return nil, err
	}
	return source, nil
}

func (a *storeAPIAdapter) runCLIGmailDraftLifecycle(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.GmailDraft,
	emit func(api.CLIRunEvent) error,
) error {
	if intent.Operation == api.CLIRunDraftGetCommand {
		output, err := a.gmailDraftLifecycleOutput(ctx, draft, "ok", &gmailDraftLifecycleObservation{State: "not_checked", Code: "not_checked"}, nil)
		if err != nil {
			return draftReplyError("draft_read_failed", err)
		}
		return emitGmailDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
	}
	if draft.Revision != intent.Revision {
		return draftReplyError("revision_mismatch", fmt.Errorf("expected revision %d, found %d", intent.Revision, draft.Revision))
	}
	if draft.DiscardedAt != nil {
		if intent.Operation == api.CLIRunDraftDeleteCommand {
			output, err := a.gmailDraftLifecycleOutput(ctx, draft, "already_discarded", nil, nil)
			if err != nil {
				return draftReplyError("draft_read_failed", err)
			}
			return emitGmailDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
		}
		return draftReplyError("draft_discarded", errors.New("discarded drafts cannot be edited"))
	}
	if draft.Pending != nil {
		return draftReplyError("pending_operation", store.ErrGmailDraftPending)
	}
	source, err := a.loadManagedGmailDraftSource(ctx, draft)
	if err != nil {
		return err
	}
	if err := gmailDraftScopeGate(source, oauth.ScopesGmailDraftWrite); err != nil {
		return err
	}
	execution, err := a.store.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", err)
		}
		return draftReplyError("sync_lock_failed", err)
	}
	released := false
	defer func() {
		if !released {
			_ = execution.Release()
		}
	}()
	finish := func() {
		if released {
			return
		}
		released = true
		if err := execution.Release(); err != nil {
			logger.Error("release source after Gmail draft lifecycle", "source_id", source.ID, "error", err)
		}
	}
	refresh := func() {
		evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
		defer cancelEvidence()
		a.refreshDraftCache(evidenceCtx, source)
	}
	draft, err = a.store.GetGmailDraftContext(ctx, intent.DraftID)
	if err != nil {
		return draftReplyError("draft_not_found", err)
	}
	if draft.Revision != intent.Revision {
		return draftReplyError("revision_mismatch", errors.New("draft changed while acquiring source ownership"))
	}
	currentRaw, err := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return draftReplyError("draft_read_failed", err)
	}
	var replacement imaplib.ReplyDraft
	if intent.Operation == api.CLIRunDraftEditCommand {
		replacement, err = imaplib.BuildDraftReplacement(currentRaw, intent.Body, time.Now(), "")
		if err != nil {
			return draftReplyError("invalid_draft", err)
		}
	}
	clientFactory := a.gmailDraftClientFactory
	if clientFactory == nil {
		clientFactory = defaultGmailDraftClientFactory
	}
	client, err := clientFactory(ctx, source)
	if err != nil {
		return draftReplyError("invalid_source", err)
	}
	defer func() { _ = client.Close() }()
	observed, err := client.GetDraft(ctx, draft.CurrentReceipt.GmailDraftID)
	if err != nil {
		if _, ok := errors.AsType[*gmail.NotFoundError](err); ok && intent.Operation == api.CLIRunDraftDeleteCommand {
			evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
			defer cancelEvidence()
			finished, finishErr := a.store.FinishGmailDraftDeleteContext(evidenceCtx, intent.DraftID, intent.Revision, true)
			if finishErr != nil {
				return draftReplyError("cleanup_local_failed", finishErr)
			}
			finish()
			defer refresh()
			output, outputErr := a.gmailDraftLifecycleOutput(evidenceCtx, finished, "already_absent", nil, &gmailDraftLifecycleObservation{State: "absent", Code: "already_absent", Present: false})
			if outputErr != nil {
				return draftReplyError("draft_read_failed", outputErr)
			}
			return emitGmailDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
		}
		code := gmailReadErrorCode(err)
		return draftReplyError(code, err)
	}
	evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
	defer cancelEvidence()
	if observed.Message.ID != draft.CurrentReceipt.GmailMessageID {
		parsed, parseErr := msgmime.Parse(observed.Message.Raw)
		if parseErr != nil {
			return draftReplyError("changed_externally", parseErr)
		}
		if _, messageErr := a.store.GetMessageContext(evidenceCtx, draft.CurrentMessageID); messageErr != nil {
			return draftReplyError("draft_read_failed", messageErr)
		}
		replyTo, replyToErr := a.store.GetMessageReplyToMessageIDContext(evidenceCtx, draft.CurrentMessageID)
		if replyToErr != nil {
			return draftReplyError("draft_read_failed", replyToErr)
		}
		observedReceipt := store.GmailDraftReceipt{
			SourceID: source.ID, GmailDraftID: observed.ID,
			GmailMessageID: observed.Message.ID, ThreadID: observed.Message.ThreadID,
		}
		participants := gmailDraftParticipants(parsed)
		adopted, adoptErr := a.store.AdoptGmailDraftObservationContext(
			evidenceCtx, intent.DraftID, intent.Revision, observedReceipt, participants,
			gmailDraftMessagePersistData(source.ID, replyTo.Int64, parsed, observed.Message.Raw, observedReceipt,
				messageRFC822ID(parsed)),
		)
		if adoptErr != nil {
			return draftReplyError("local_persistence_failed", adoptErr)
		}
		finish()
		defer refresh()
		output, outputErr := a.gmailDraftLifecycleOutput(evidenceCtx, adopted, "changed_externally", &gmailDraftLifecycleObservation{
			State: "changed", Code: "changed_externally", GmailDraftID: observed.ID,
			GmailMessageID: observed.Message.ID, ThreadID: observed.Message.ThreadID, Present: true,
		}, nil)
		if outputErr != nil {
			return draftReplyError("draft_read_failed", outputErr)
		}
		if err := emitGmailDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output); err != nil {
			return draftReplyError("output_failed", err)
		}
		return draftReplyError("changed_externally", errors.New("Gmail draft changed outside msgvault"))
	}
	if intent.Operation == api.CLIRunDraftEditCommand {
		return a.runGmailDraftEdit(ctx, intent, draft, source, client, replacement, finish, refresh, emit)
	}
	return a.runGmailDraftDelete(ctx, intent, draft, source, client, finish, refresh, emit)
}

func (a *storeAPIAdapter) runGmailDraftEdit(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.GmailDraft,
	source *store.Source,
	client gmail.DraftAPI,
	replacement imaplib.ReplyDraft,
	finish func(),
	refresh func(),
	emit func(api.CLIRunEvent) error,
) error {
	claimed, err := a.store.ClaimGmailDraftContext(ctx, intent.DraftID, intent.Revision, store.GmailDraftOperationEdit, replacement.Raw)
	if err != nil {
		return draftReplyError("claim_failed", err)
	}
	updated, err := client.UpdateDraft(ctx, draft.CurrentReceipt.GmailDraftID, replacement.Raw, draft.CurrentReceipt.ThreadID)
	if err != nil {
		evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
		defer cancelEvidence()
		code := gmailWriteErrorCode(err)
		var writeErr *gmail.DraftWriteError
		if errors.As(err, &writeErr) && writeErr.State == gmail.DraftStateRejected ||
			errors.As(err, &writeErr) && writeErr.State == gmail.DraftStateCancelled {
			active, abortErr := a.store.AbortGmailDraftContext(evidenceCtx, intent.DraftID, intent.Revision)
			if abortErr != nil {
				return draftReplyError("local_persistence_failed", abortErr)
			}
			output, outputErr := a.gmailDraftLifecycleOutput(evidenceCtx, active, code, nil, nil)
			if outputErr == nil {
				_ = emitGmailDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
			}
			return draftReplyError(code, gmailWriteCause(err))
		}
		_ = a.store.RecordGmailDraftOutcomeContext(evidenceCtx, intent.DraftID, intent.Revision, code, "")
		latest, _ := a.store.GetGmailDraftContext(evidenceCtx, intent.DraftID)
		output, outputErr := a.gmailDraftLifecycleOutput(evidenceCtx, latest, "pending", nil, nil)
		if outputErr == nil {
			output.ManualReconciliation = true
			_ = emitGmailDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		}
		return draftReplyError(code, gmailWriteCause(err))
	}
	if updated == nil {
		evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
		defer cancelEvidence()
		_ = a.store.RecordGmailDraftOutcomeContext(evidenceCtx, intent.DraftID, intent.Revision, "remote_unknown", "")
		latest, _ := a.store.GetGmailDraftContext(evidenceCtx, intent.DraftID)
		output, outputErr := a.gmailDraftLifecycleOutput(evidenceCtx, latest, "pending", nil, nil)
		if outputErr == nil {
			output.ManualReconciliation = true
			_ = emitGmailDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		}
		return draftReplyError("remote_unknown", errors.New("Gmail update returned no draft"))
	}
	evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
	defer cancelEvidence()
	replacementReceipt := store.GmailDraftReceipt{
		SourceID: source.ID, GmailDraftID: updated.ID,
		GmailMessageID: updated.Message.ID, ThreadID: updated.Message.ThreadID,
	}
	if err := a.store.RecordGmailDraftOutcomeContext(evidenceCtx, intent.DraftID, intent.Revision, "accepted_local_failed", updated.Message.ID); err != nil {
		return a.reportGmailDraftAcceptedLocalFailure(
			emit, intent.JSON, claimed, replacementReceipt, replacement.Raw, err,
		)
	}
	replyTo, err := a.store.GetMessageReplyToMessageIDContext(evidenceCtx, draft.CurrentMessageID)
	if err != nil {
		return a.reportGmailDraftAcceptedLocalFailure(
			emit, intent.JSON, claimed, replacementReceipt, replacement.Raw, err,
		)
	}
	published, err := a.store.PublishGmailDraftReplacementContext(
		evidenceCtx, intent.DraftID, intent.Revision, updated.Message.ID,
		gmailDraftParticipants(replacement.Parsed),
		gmailDraftMessagePersistData(source.ID, replyTo.Int64, replacement.Parsed, replacement.Raw, replacementReceipt, messageRFC822ID(replacement.Parsed)),
	)
	if err != nil {
		return a.reportGmailDraftAcceptedLocalFailure(
			emit, intent.JSON, claimed, replacementReceipt, replacement.Raw, err,
		)
	}
	finish()
	defer refresh()
	output, err := a.gmailDraftLifecycleOutput(evidenceCtx, published, "edited", nil, nil)
	if err != nil {
		return draftReplyError("draft_read_failed", err)
	}
	return emitGmailDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
}

func (a *storeAPIAdapter) runGmailDraftDelete(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.GmailDraft,
	source *store.Source,
	client gmail.DraftAPI,
	finish func(),
	refresh func(),
	emit func(api.CLIRunEvent) error,
) error {
	_, err := a.store.ClaimGmailDraftContext(ctx, intent.DraftID, intent.Revision, store.GmailDraftOperationDelete, nil)
	if err != nil {
		return draftReplyError("claim_failed", err)
	}
	err = client.DeleteDraft(ctx, draft.CurrentReceipt.GmailDraftID)
	if err != nil {
		evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
		defer cancelEvidence()
		code := gmailWriteErrorCode(err)
		var writeErr *gmail.DraftWriteError
		if errors.As(err, &writeErr) && writeErr.Code == "draft_absent" {
			finished, finishErr := a.store.FinishGmailDraftDeleteContext(evidenceCtx, intent.DraftID, intent.Revision, false)
			if finishErr != nil {
				return draftReplyError("cleanup_local_failed", finishErr)
			}
			finish()
			defer refresh()
			output, outputErr := a.gmailDraftLifecycleOutput(evidenceCtx, finished, "already_absent", nil, &gmailDraftLifecycleObservation{State: "absent", Code: "already_absent"})
			if outputErr != nil {
				return draftReplyError("draft_read_failed", outputErr)
			}
			return emitGmailDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
		}
		if errors.As(err, &writeErr) && (writeErr.State == gmail.DraftStateRejected || writeErr.State == gmail.DraftStateCancelled) {
			active, abortErr := a.store.AbortGmailDraftContext(evidenceCtx, intent.DraftID, intent.Revision)
			if abortErr != nil {
				return draftReplyError("local_persistence_failed", abortErr)
			}
			output, outputErr := a.gmailDraftLifecycleOutput(evidenceCtx, active, code, nil, nil)
			if outputErr == nil {
				_ = emitGmailDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
			}
			return draftReplyError(code, gmailWriteCause(err))
		}
		_ = a.store.RecordGmailDraftOutcomeContext(evidenceCtx, intent.DraftID, intent.Revision, code, "")
		latest, _ := a.store.GetGmailDraftContext(evidenceCtx, intent.DraftID)
		output, outputErr := a.gmailDraftLifecycleOutput(evidenceCtx, latest, "pending", nil, nil)
		if outputErr == nil {
			output.ManualReconciliation = true
			_ = emitGmailDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		}
		return draftReplyError(code, gmailWriteCause(err))
	}
	evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
	defer cancelEvidence()
	finished, err := a.store.FinishGmailDraftDeleteContext(evidenceCtx, intent.DraftID, intent.Revision, false)
	if err != nil {
		return draftReplyError("cleanup_local_failed", err)
	}
	finish()
	defer refresh()
	output, err := a.gmailDraftLifecycleOutput(evidenceCtx, finished, "deleted", nil, nil)
	if err != nil {
		return draftReplyError("draft_read_failed", err)
	}
	return emitGmailDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
}

func messageRFC822ID(parsed *msgmime.Message) string {
	id := msgmime.NormalizeMessageID(parsed.MessageID)
	if id == "" {
		return ""
	}
	return "<" + id + ">"
}

func parseDraftSendAsArgs(args []string) (string, bool, error) {
	if !api.IsCLIRunDraftSendAs(args) {
		return "", false, draftReplyError("invalid_args", errors.New("expected draft-send-as"))
	}
	var account string
	jsonOutput := false
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "--json") {
			if arg != "--json" && arg != "--json=true" {
				return "", false, draftReplyError("invalid_args", errors.New("--json accepts no value"))
			}
			jsonOutput = true
			continue
		}
		if strings.HasPrefix(arg, "--log-") || arg == "--verbose" {
			continue
		}
		if account != "" || !utf8.ValidString(arg) || strings.TrimSpace(arg) == "" || strings.ContainsAny(arg, "\x00\r\n") {
			return "", false, draftReplyError("invalid_args", errors.New("draft-send-as requires one account"))
		}
		account = arg
	}
	if account == "" {
		return "", false, draftReplyError("invalid_args", errors.New("draft-send-as requires one account"))
	}
	return account, jsonOutput, nil
}

type gmailSendAsOutput struct {
	SourceID int64            `json:"source_id"`
	Account  string           `json:"account"`
	Entries  []gmailSendAsRow `json:"send_as"`
}

type gmailSendAsRow struct {
	Email              string `json:"email"`
	DisplayName        string `json:"display_name,omitempty"`
	Primary            bool   `json:"primary"`
	Default            bool   `json:"default"`
	VerificationStatus string `json:"verification_status"`
	ConfirmedIdentity  bool   `json:"confirmed_identity"`
}

func (a *storeAPIAdapter) runCLIDraftSendAs(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if len(req.Env) != 0 || req.Cwd != "" {
		return draftReplyError("invalid_args", errors.New("draft-send-as accepts no environment or working directory"))
	}
	account, jsonOutput, err := parseDraftSendAsArgs(req.Args)
	if err != nil {
		return err
	}
	source, err := sourceops.ResolveExactOne(a.store, sourceops.Selector{Account: account, SourceType: "gmail"})
	if err != nil {
		return draftReplyError("invalid_source", err)
	}
	if err := gmailDraftScopeGate(source, oauth.ScopesGmailSendAsList); err != nil {
		return err
	}
	clientFactory := a.gmailDraftClientFactory
	if clientFactory == nil {
		clientFactory = defaultGmailDraftClientFactory
	}
	client, err := clientFactory(ctx, source)
	if err != nil {
		return draftReplyError("invalid_source", err)
	}
	defer func() { _ = client.Close() }()
	entries, err := client.ListSendAs(ctx)
	if err != nil {
		return draftReplyError(gmailReadErrorCode(err), err)
	}
	identities, err := a.store.ListAccountIdentitiesContext(ctx, source.ID)
	if err != nil {
		return draftReplyError("invalid_from", err)
	}
	output := gmailSendAsOutput{SourceID: source.ID, Account: source.Identifier, Entries: make([]gmailSendAsRow, len(entries))}
	for i, entry := range entries {
		output.Entries[i] = gmailSendAsRow{
			Email: entry.Email, DisplayName: entry.DisplayName, Primary: entry.Primary,
			Default: entry.Default, VerificationStatus: entry.VerificationStatus,
			ConfirmedIdentity: hasConfirmedSourceIdentity(identities, entry.Email),
		}
	}
	if emit == nil {
		return nil
	}
	if jsonOutput {
		data, err := jsonv2.Marshal(output)
		if err != nil {
			return draftReplyError("output_failed", err)
		}
		return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: string(data) + "\n"})
	}
	var data strings.Builder
	for _, entry := range output.Entries {
		fmt.Fprintf(&data, "%s\t%s\tprimary=%t\tdefault=%t\tverification=%s\tconfirmed=%t\n",
			textutil.SanitizeTerminal(entry.Email), textutil.SanitizeTerminal(entry.DisplayName),
			entry.Primary, entry.Default, textutil.SanitizeTerminal(entry.VerificationStatus), entry.ConfirmedIdentity)
	}
	return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: data.String()})
}
