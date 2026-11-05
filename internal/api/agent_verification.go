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

// verificationWithheld classifies complete messages in the authorized snapshot.
// A nil map means the caller has authority to read verification content.
func (s *Server) verificationWithheld(w http.ResponseWriter, r *http.Request, ids []int64) (map[int64]bool, bool) {
	grant := s.requestAuthentication(r).Grant
	if grant == nil || grant.HasPermission(agentgrant.PermissionVerificationRead) {
		return nil, true
	}
	loader, ok := s.store.(interface {
		PopulateMessageBodiesContext(ctx context.Context, messages []store.APIMessage) error
	})
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

func withholdField(value *string, withheld bool) {
	if withheld {
		*value = verification.Masked
	}
}

func (s *Server) withholdsVerification(r *http.Request) bool {
	grant := s.requestAuthentication(r).Grant
	return grant != nil && !grant.HasPermission(agentgrant.PermissionVerificationRead)
}

func (s *Server) maskMessageSummaries(w http.ResponseWriter, r *http.Request, rows []MessageSummary, contexts []BodySearchContext) bool {
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	withheld, ok := s.verificationWithheld(w, r, ids)
	if !ok || withheld == nil {
		return ok
	}
	for i := range rows {
		if withheld[rows[i].ID] {
			rows[i].Subject, rows[i].Snippet = verification.Masked, verification.Masked
		}
	}
	for i := range contexts {
		if withheld[contexts[i].MessageID] {
			for j := range contexts[i].ContextSnippets {
				contexts[i].ContextSnippets[j] = verification.Masked
			}
		}
	}
	return true
}

func (s *Server) maskQuerySummaries(w http.ResponseWriter, r *http.Request, rows []query.MessageSummary) bool {
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	withheld, ok := s.verificationWithheld(w, r, ids)
	if !ok || withheld == nil {
		return ok
	}
	for i := range rows {
		hide := withheld[rows[i].ID]
		if hide {
			rows[i].Subject, rows[i].Snippet, rows[i].MatchSnippet = verification.Masked, verification.Masked, verification.Masked
			if rows[i].BodyText != "" {
				rows[i].BodyText = verification.Masked
			}
		}
		withholdField(&rows[i].ConversationTitle, hide)
		for j := range rows[i].AttachmentNames {
			withholdField(&rows[i].AttachmentNames[j], hide)
		}
	}
	return true
}

func (s *Server) maskThread(w http.ResponseWriter, r *http.Request, rows []query.ThreadMessage) bool {
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	withheld, ok := s.verificationWithheld(w, r, ids)
	if !ok || withheld == nil {
		return ok
	}
	for i := range rows {
		if withheld[rows[i].ID] {
			rows[i].Subject = verification.Masked
		}
	}
	return true
}

func (s *Server) maskMessageDetail(r *http.Request, msg *query.MessageDetail) {
	if !s.withholdsVerification(r) {
		return
	}
	names := make([]string, len(msg.Attachments))
	for i, att := range msg.Attachments {
		names[i] = att.Filename
	}
	hide := verification.Is(verification.Content{Subject: msg.Subject, Snippet: msg.Snippet, Title: msg.ConversationTitle, Filenames: names, Text: msg.BodyText, HTML: msg.BodyHTML})
	if hide {
		msg.Subject, msg.Snippet, msg.BodyText, msg.BodyHTML = verification.Masked, verification.Masked, verification.Masked, ""
	}
	withholdField(&msg.ConversationTitle, hide)
	for i := range msg.Attachments {
		withholdField(&msg.Attachments[i].Filename, hide)
	}
}

func (s *Server) maskStoreMessage(r *http.Request, msg *store.APIMessage) {
	if !s.withholdsVerification(r) {
		return
	}
	hide := verification.Is(messageVerificationContent(msg))
	if hide {
		msg.Subject, msg.Snippet, msg.Body, msg.BodyText, msg.BodyHTML = verification.Masked, verification.Masked, verification.Masked, verification.Masked, ""
	}
	withholdField(&msg.ConversationTitle, hide)
	for i := range msg.Attachments {
		withholdField(&msg.Attachments[i].Filename, hide)
	}
}

// Classify every parent holding the hash while the read snapshot is held.
func (s *Server) prepareVerificationAttachments(w http.ResponseWriter, r *http.Request, attachments []query.AttachmentInfo) (map[int64]bool, bool) {
	if !s.withholdsVerification(r) {
		return nil, true
	}
	if len(attachments) > 0 && attachments[0].ContentHash != "" {
		var err error
		attachments, err = s.queryEngineForContext(r.Context()).GetAttachmentsByHash(r.Context(), attachments[0].ContentHash)
		if err != nil {
			writeAPIHTTPError(w, newAPIHTTPError(503, "source_scope_unavailable", "Message content classification is unavailable"))
			return nil, false
		}
	}

	ids := make([]int64, 0, len(attachments))
	seen := make(map[int64]bool)
	for _, att := range attachments {
		if !seen[att.MessageID] {
			ids = append(ids, att.MessageID)
			seen[att.MessageID] = true
		}
	}
	return s.verificationWithheld(w, r, ids)
}

// Sniff only the file type. The parent messages decide access to text files.
func (s *Server) verificationAttachmentReader(content io.Reader, att *query.AttachmentInfo, withheld map[int64]bool) (io.Reader, *apiHTTPError) {
	if withheld == nil {
		return content, nil
	}
	text := strings.HasPrefix(strings.ToLower(att.MimeType), "text/") || strings.HasPrefix(strings.ToLower(att.MimeType), "message/") || strings.EqualFold(filepath.Ext(att.Filename), ".eml")
	withholdField(&att.Filename, withheld[att.MessageID])
	parentWithheld := false
	for _, hide := range withheld {
		parentWithheld = parentWithheld || hide
	}
	if !parentWithheld {
		return content, nil
	}
	reader := bufio.NewReaderSize(content, 512)
	if !text {
		prefix, err := reader.Peek(512)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, newAPIHTTPError(500, "internal_error", "Failed to read attachment")
		}
		detected := http.DetectContentType(prefix)
		text = strings.HasPrefix(detected, "text/") || strings.HasPrefix(detected, "message/")
	}
	if text {
		return nil, newAPIHTTPError(403, "verification_masked", "Attachment text requires verification.read")
	}
	return reader, nil
}
