package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/verification"
)

var errVerificationMasked = errors.New("verification_masked: credential-bearing content requires explicit reveal permission")
var errVerificationUnavailable = errors.New("verification_unavailable: canonical message protection is unavailable")

func (h *handlers) requireOwnerVerificationUnmask() *toolResult {
	if h.verificationProtectedByDaemon || h.unmaskVerification {
		return nil
	}
	return toolErrorResult("verification_masked: this content-derived output requires --unmask-verification")
}

type verificationCacheKey struct{}

func withVerificationCache(ctx context.Context) context.Context {
	if ctx.Value(verificationCacheKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, verificationCacheKey{}, make(map[int64]*query.MessageDetail))
}

func (h *handlers) guardVerificationSearch(ctx context.Context, rows []query.MessageSummary, q *search.Query) ([]query.MessageSummary, int, error) {
	if h.verificationProtectedByDaemon || h.unmaskVerification {
		return rows, 0, nil
	}
	tokens := query.VerificationQueryTokens(q)
	out := make([]query.MessageSummary, 0, len(rows))
	for _, row := range rows {
		canonical, err := h.canonicalVerification(ctx, row.ID)
		if err != nil {
			return nil, 0, err
		}
		if !query.VerificationQueryMatches(canonical, tokens) {
			out = append(out, row)
		}
	}
	return out, len(rows) - len(out), nil
}

func (h *handlers) canonicalVerification(ctx context.Context, id int64) (*query.MessageDetail, error) {
	cache, _ := ctx.Value(verificationCacheKey{}).(map[int64]*query.MessageDetail)
	if msg := cache[id]; msg != nil {
		return msg, nil
	}
	msg, err := h.engine.GetMessage(ctx, id)
	if err != nil || msg == nil || msg.ID != id {
		return nil, errVerificationUnavailable
	}
	if cache != nil {
		cache[id] = msg
	}
	return msg, nil
}

func (h *handlers) prepareVerificationDetail(msg *query.MessageDetail) *query.MessageDetail {
	if h.verificationProtectedByDaemon {
		return msg // the daemon owns the grant decision and its metadata
	}
	return query.PrepareVerification(msg, h.unmaskVerification, time.Now())
}

func (h *handlers) prepareVerificationSummaries(ctx context.Context, rows []query.MessageSummary) ([]query.MessageSummary, error) {
	if h.verificationProtectedByDaemon || h.unmaskVerification {
		return rows, nil
	}
	out := make([]query.MessageSummary, len(rows))
	for i, row := range rows {
		msg, err := h.canonicalVerification(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		if msg.ID != row.ID || row.SourceID != 0 && msg.SourceID != row.SourceID {
			return nil, errVerificationUnavailable
		}
		out[i] = query.PrepareSummaryVerification(row, msg, h.unmaskVerification, time.Now())
	}
	return out, nil
}

func (h *handlers) prepareVerificationThread(ctx context.Context, page *query.ThreadPage) (*query.ThreadPage, error) {
	if h.verificationProtectedByDaemon || h.unmaskVerification {
		return page, nil
	}
	if page == nil {
		return nil, errVerificationUnavailable
	}
	out := *page
	out.Messages = slices.Clone(page.Messages)
	for i, row := range out.Messages {
		msg, err := h.canonicalVerification(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		if msg.SourceID != page.SourceID || msg.ConversationID != page.ConversationID {
			return nil, errVerificationUnavailable
		}
		out.Messages[i] = query.PrepareThreadVerification(row, msg, h.unmaskVerification, time.Now())
	}
	return &out, nil
}

func (h *handlers) protectVerificationAttachment(ctx context.Context, att *query.AttachmentInfo) error {
	_, err := h.prepareVerificationAttachment(ctx, att)
	return err
}

func (h *handlers) guardVerificationSearchItems(ctx context.Context, items []searchMessageItem, parsed *search.Query) ([]searchMessageItem, int, error) {
	rows := make([]query.MessageSummary, len(items))
	for i := range items {
		rows[i] = items[i].MessageSummary
	}
	rows, dropped, err := h.guardVerificationSearch(ctx, rows, parsed)
	if err != nil || dropped == 0 {
		return items, dropped, err
	}
	allowed := make(map[int64]bool, len(rows))
	for _, row := range rows {
		allowed[row.ID] = true
	}
	filtered := make([]searchMessageItem, 0, len(rows))
	for _, item := range items {
		if allowed[item.ID] {
			filtered = append(filtered, item)
		}
	}
	return filtered, dropped, nil
}

func (h *handlers) prepareVerificationAttachment(ctx context.Context, att *query.AttachmentInfo) (*query.AttachmentInfo, error) {
	if h.verificationProtectedByDaemon || h.unmaskVerification {
		return att, nil
	}
	if att == nil || att.MessageID == 0 {
		return nil, &attachmentUnavailableError{message: errVerificationUnavailable.Error()}
	}
	reader, ok := h.engine.(query.AttachmentVerificationSnapshotReader)
	if !ok {
		return nil, &attachmentUnavailableError{message: errVerificationUnavailable.Error()}
	}
	current, msg, err := reader.GetAttachmentVerificationSnapshot(ctx, att.ID)
	if err != nil || current == nil || msg == nil ||
		current.ID != att.ID || current.MessageID != att.MessageID || current.SourceID != att.SourceID ||
		msg.ID != current.MessageID || msg.SourceID != current.SourceID || !sameAttachmentPayload(att, current) {
		return nil, &attachmentUnavailableError{message: errVerificationUnavailable.Error()}
	}
	finding := verification.Detect(query.VerificationInput(msg))
	if finding == nil {
		return current, nil
	}
	// Other attachment bytes (including screenshots/PDFs) are outside v1,
	// but their human-readable metadata still uses the parent's mask.
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(current.MimeType, ";")[0]))
	if strings.HasPrefix(mimeType, "text/") || mimeType == "message/rfc822" {
		return nil, &attachmentUnavailableError{message: errVerificationMasked.Error()}
	}
	out := *current
	masker := verification.NewMasker(finding)
	out.Filename, out.URL = masker.Text(current.Filename), masker.Text(current.URL)
	out.StoragePath, out.Docbank = "", nil
	return &out, nil
}

func sameAttachmentPayload(previous, current *query.AttachmentInfo) bool {
	if previous == nil || current == nil || previous.ID != current.ID ||
		previous.MessageID != current.MessageID || previous.SourceID != current.SourceID {
		return false
	}
	if previous.ContentHash != "" || current.ContentHash != "" {
		return strings.EqualFold(previous.ContentHash, current.ContentHash)
	}
	return previous.Size == current.Size && previous.StoragePath == current.StoragePath && previous.URL == current.URL
}

func (h *handlers) protectVerificationOriginal(ctx context.Context, reader query.OriginalMessageReader, ref query.MessageRef) error {
	if h.verificationProtectedByDaemon || h.unmaskVerification {
		return nil
	}
	id := ref.ID
	if id == 0 {
		page, err := reader.ListThread(ctx, query.ThreadQuery{MessageRef: ref, Limit: 1})
		if err != nil {
			return err
		}
		if page == nil || page.MessageID == 0 {
			return errVerificationUnavailable
		}
		id = page.MessageID
	}
	msg, err := h.canonicalVerification(ctx, id)
	if err != nil {
		return err
	}
	if verification.Detect(query.VerificationInput(msg)) != nil {
		return errVerificationMasked
	}
	return nil
}

func (h *handlers) protectVerificationOriginalMIME(ctx context.Context, original *query.OriginalMessage) error {
	if h.verificationProtectedByDaemon || h.unmaskVerification {
		return nil
	}
	if original == nil || original.MessageID == 0 {
		return errVerificationUnavailable
	}
	parsed, err := msgmime.Parse(original.MIME)
	if err != nil || parsed == nil || len(parsed.Errors) != 0 {
		return errVerificationMasked
	}
	canonical, err := h.canonicalVerification(ctx, original.MessageID)
	if err != nil {
		return err
	}
	in := query.VerificationInput(canonical)
	in.Subject = parsed.Subject
	in.BodyText = parsed.GetBodyText()
	in.BodyHTML = parsed.BodyHTML
	if verification.Detect(in) != nil {
		return errVerificationMasked
	}
	return nil
}
