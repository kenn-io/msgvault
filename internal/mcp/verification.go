package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/verification"
)

var errVerificationMasked = errors.New("verification_masked: credential-bearing content requires explicit reveal permission")
var errVerificationUnavailable = errors.New("verification_unavailable: canonical message protection is unavailable")

type verificationCacheKey struct{}

func withVerificationCache(ctx context.Context) context.Context {
	if ctx.Value(verificationCacheKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, verificationCacheKey{}, make(map[int64]*query.MessageDetail))
}

func (h *handlers) guardVerificationSearch(ctx context.Context, rows []query.MessageSummary, q *search.Query) ([]query.MessageSummary, int, error) {
	if h.delegatedOnly || h.unmaskVerification {
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
	if h.delegatedOnly {
		return msg // the daemon owns the grant decision and its metadata
	}
	return query.PrepareVerification(msg, h.unmaskVerification, time.Now())
}

func (h *handlers) prepareVerificationSummaries(ctx context.Context, rows []query.MessageSummary) ([]query.MessageSummary, error) {
	if h.delegatedOnly || h.unmaskVerification {
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
	if h.delegatedOnly || h.unmaskVerification {
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
	if h.unmaskVerification {
		return att, nil
	}
	if att.MessageID == 0 {
		return nil, &attachmentUnavailableError{message: errVerificationUnavailable.Error()}
	}
	msg, err := h.canonicalVerification(ctx, att.MessageID)
	if err != nil || msg.SourceID != att.SourceID {
		return nil, &attachmentUnavailableError{message: errVerificationUnavailable.Error()}
	}
	if h.delegatedOnly && msg.Verification != nil {
		if msg.Verification.Masked {
			return nil, &attachmentUnavailableError{message: errVerificationMasked.Error()}
		}
		// The daemon detail response carries the current source-scoped grant
		// decision. Recheck it for every cached attachment continuation.
		return att, nil
	}
	finding := verification.Detect(query.VerificationInput(msg))
	if finding == nil {
		return att, nil
	}
	// Other attachment bytes (including screenshots/PDFs) are outside v1,
	// but their human-readable metadata still uses the parent's mask.
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(att.MimeType, ";")[0]))
	if strings.HasPrefix(mimeType, "text/") || mimeType == "message/rfc822" {
		return nil, &attachmentUnavailableError{message: errVerificationMasked.Error()}
	}
	out := *att
	masker := verification.NewMasker(finding)
	out.Filename, out.URL = masker.Text(att.Filename), masker.Text(att.URL)
	out.StoragePath, out.Docbank = "", nil
	return &out, nil
}

func (h *handlers) protectVerificationOriginal(ctx context.Context, reader query.OriginalMessageReader, ref query.MessageRef) error {
	if h.delegatedOnly || h.unmaskVerification {
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
