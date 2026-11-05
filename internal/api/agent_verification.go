package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/verification"
)

// MessageBodyLoader supplies complete message context in the caller's snapshot.
type MessageBodyLoader interface {
	PopulateMessageBodiesContext(ctx context.Context, messages []store.APIMessage) error
}

// verificationWithheld classifies complete messages in the authorized snapshot.
// A nil map means the caller has authority to read verification content.
func (s *Server) verificationWithheld(w http.ResponseWriter, r *http.Request, ids []int64) (map[int64]bool, bool) {
	if !s.withholdsVerification(r) {
		return nil, true
	}
	loader, ok := s.store.(MessageBodyLoader)
	messages := make([]store.APIMessage, len(ids))
	for i, id := range ids {
		messages[i].ID = id
	}
	if !ok || loader.PopulateMessageBodiesContext(r.Context(), messages) != nil {
		writeAPIHTTPError(w, newAPIHTTPError(503, "source_scope_unavailable", "Message content classification is unavailable"))
		return nil, false
	}
	withheld := make(map[int64]bool, len(ids))
	for _, msg := range messages {
		withheld[msg.ID] = verification.Is(messageVerificationContent(&msg))
	}
	return withheld, true
}

func messageVerificationContent(msg *store.APIMessage) verification.Content {
	names := make([]string, len(msg.Attachments))
	for i, att := range msg.Attachments {
		names[i] = att.Filename
	}
	return verification.Content{Subject: msg.Subject, Snippet: msg.Snippet, Title: msg.ConversationTitle, Filenames: names, Text: msg.BodyText, HTML: msg.BodyHTML}
}

func (s *Server) withholdsVerification(r *http.Request) bool {
	grant := s.requestAuthentication(r).Grant
	return grant != nil && !grant.HasPermission(agentgrant.PermissionVerificationRead)
}

func withholdRows[T any](s *Server, w http.ResponseWriter, r *http.Request, rows []T, id func(*T) int64, mask func(*T)) (map[int64]bool, bool) {
	ids := make([]int64, len(rows))
	for i := range rows {
		ids[i] = id(&rows[i])
	}
	withheld, ok := s.verificationWithheld(w, r, ids)
	if ok {
		for i := range rows {
			if withheld[id(&rows[i])] {
				mask(&rows[i])
			}
		}
	}
	return withheld, ok
}

func (s *Server) maskMessageSummaries(w http.ResponseWriter, r *http.Request, rows []MessageSummary, contexts []BodySearchContext) bool {
	withheld, ok := withholdRows(s, w, r, rows, func(row *MessageSummary) int64 { return row.ID }, func(row *MessageSummary) {
		row.Subject, row.Snippet = verification.Masked, verification.Masked
	})
	for i := range contexts {
		if withheld[contexts[i].MessageID] {
			for j := range contexts[i].ContextSnippets {
				contexts[i].ContextSnippets[j] = verification.Masked
			}
		}
	}
	return ok
}

func (s *Server) maskQuerySummaries(w http.ResponseWriter, r *http.Request, rows []query.MessageSummary) bool {
	_, ok := withholdRows(s, w, r, rows, func(row *query.MessageSummary) int64 { return row.ID }, func(row *query.MessageSummary) {
		row.Subject, row.Snippet, row.MatchSnippet, row.ConversationTitle = verification.Masked, verification.Masked, verification.Masked, verification.Masked
		if row.BodyText != "" {
			row.BodyText = verification.Masked
		}
		for j := range row.AttachmentNames {
			row.AttachmentNames[j] = verification.Masked
		}
	})
	return ok
}

func (s *Server) maskThread(w http.ResponseWriter, r *http.Request, rows []query.ThreadMessage) bool {
	_, ok := withholdRows(s, w, r, rows, func(row *query.ThreadMessage) int64 { return row.ID }, func(row *query.ThreadMessage) { row.Subject = verification.Masked })
	return ok
}

func (s *Server) maskMessageDetail(r *http.Request, msg *query.MessageDetail) {
	if !s.withholdsVerification(r) {
		return
	}
	names := make([]string, len(msg.Attachments))
	for i, att := range msg.Attachments {
		names[i] = att.Filename
	}
	if !verification.Is(verification.Content{Subject: msg.Subject, Snippet: msg.Snippet, Title: msg.ConversationTitle, Filenames: names, Text: msg.BodyText, HTML: msg.BodyHTML}) {
		return
	}
	msg.Subject, msg.Snippet, msg.ConversationTitle, msg.BodyText, msg.BodyHTML = verification.Masked, verification.Masked, verification.Masked, verification.Masked, ""
	for i := range msg.Attachments {
		msg.Attachments[i].Filename = verification.Masked
	}
}

func (s *Server) maskStoreMessage(r *http.Request, msg *store.APIMessage) {
	if !s.withholdsVerification(r) || !verification.Is(messageVerificationContent(msg)) {
		return
	}
	msg.Subject, msg.Snippet, msg.ConversationTitle, msg.Body, msg.BodyText, msg.BodyHTML = verification.Masked, verification.Masked, verification.Masked, verification.Masked, verification.Masked, ""
	for i := range msg.Attachments {
		msg.Attachments[i].Filename = verification.Masked
	}
}

func (s *Server) withholdAttachmentName(w http.ResponseWriter, r *http.Request, att *query.AttachmentInfo) (refuse, ok bool) {
	withheld, ok := s.verificationWithheld(w, r, []int64{att.MessageID})
	if withheld[att.MessageID] {
		att.Filename = verification.Masked
	}
	return withheld != nil, ok
}

// The caller owns content. Sniffing preserves the original download bytes.
func textAttachmentReader(content io.Reader, candidates []query.AttachmentInfo, refuse bool) (io.Reader, *apiHTTPError) {
	if !refuse {
		return content, nil
	}
	denied := newAPIHTTPError(403, "verification_masked", "Attachment text requires verification.read")
	for _, att := range candidates {
		declared := strings.ToLower(att.MimeType)
		if strings.HasPrefix(declared, "text/") || strings.HasPrefix(declared, "message/") || strings.EqualFold(filepath.Ext(att.Filename), ".eml") {
			return nil, denied
		}
	}
	reader := bufio.NewReaderSize(content, 512)
	prefix, err := reader.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, newAPIHTTPError(500, "internal_error", "Failed to read attachment")
	}
	detected := http.DetectContentType(prefix)
	if strings.HasPrefix(detected, "text/") || strings.HasPrefix(detected, "message/") {
		return nil, denied
	}
	return reader, nil
}
