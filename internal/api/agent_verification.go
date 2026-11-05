package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/verification"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

type verificationSummary struct {
	ID               int64
	Subject, Snippet string
}

// verificationWithheld classifies complete messages in the authorized snapshot.
// A nil map means the caller has authority to read verification content.
func (s *Server) verificationWithheld(w http.ResponseWriter, r *http.Request, rows []verificationSummary) (map[int64]bool, bool) {
	grant := s.requestAuthentication(r).Grant
	if grant == nil || grant.HasPermission(agentgrant.PermissionVerificationRead) {
		return nil, true
	}
	loader, ok := s.store.(interface {
		PopulateMessageBodiesContext(ctx context.Context, messages []store.APIMessage) error
	})
	messages := make([]store.APIMessage, len(rows))
	for i, row := range rows {
		// Attachment and thread rows can carry only an ID. Load their metadata
		// in the same snapshot so a cue in a stored snippet still counts.
		if row.Subject == "" && row.Snippet == "" {
			msg, err := s.getMessage(r.Context(), row.ID)
			if err != nil || msg == nil {
				ok = false
				break
			}
			row.Subject, row.Snippet = msg.Subject, msg.Snippet
		}
		messages[i] = store.APIMessage{ID: row.ID, Subject: row.Subject, Snippet: row.Snippet}
	}
	if !ok || loader.PopulateMessageBodiesContext(r.Context(), messages) != nil {
		writeAPIHTTPError(w, newAPIHTTPError(503, "source_scope_unavailable", "Message content classification is unavailable"))
		return nil, false
	}
	withheld := make(map[int64]bool, len(rows))
	for _, msg := range messages {
		withheld[msg.ID] = verification.Is(verification.Content{Subject: msg.Subject, Snippet: msg.Snippet, Text: msg.BodyText, HTML: msg.BodyHTML})
	}
	return withheld, true
}

func withholdField(value *string, withheld bool) {
	if withheld || verification.Is(verification.Content{Text: *value}) {
		*value = verification.Masked
	}
}

func (s *Server) maskMessageSummaries(w http.ResponseWriter, r *http.Request, rows []MessageSummary, contexts []BodySearchContext) bool {
	messages := make([]verificationSummary, len(rows))
	for i, row := range rows {
		messages[i] = verificationSummary{row.ID, row.Subject, row.Snippet}
	}
	withheld, ok := s.verificationWithheld(w, r, messages)
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
	messages := make([]verificationSummary, len(rows))
	for i, row := range rows {
		messages[i] = verificationSummary{row.ID, row.Subject, row.Snippet}
	}
	withheld, ok := s.verificationWithheld(w, r, messages)
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
	messages := make([]verificationSummary, len(rows))
	for i, row := range rows {
		messages[i] = verificationSummary{ID: row.ID, Subject: row.Subject}
	}
	withheld, ok := s.verificationWithheld(w, r, messages)
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

func (s *Server) maskMessageDetail(w http.ResponseWriter, r *http.Request, msg *query.MessageDetail) bool {
	withheld, ok := s.verificationWithheld(w, r, []verificationSummary{{msg.ID, msg.Subject, msg.Snippet}})
	if !ok || withheld == nil {
		return ok
	}
	hide := withheld[msg.ID]
	if hide {
		msg.Subject, msg.Snippet, msg.BodyText, msg.BodyHTML = verification.Masked, verification.Masked, verification.Masked, ""
	}
	withholdField(&msg.ConversationTitle, hide)
	for i := range msg.Attachments {
		withholdField(&msg.Attachments[i].Filename, hide)
	}
	return true
}

func (s *Server) maskStoreMessage(w http.ResponseWriter, r *http.Request, msg *store.APIMessage) bool {
	withheld, ok := s.verificationWithheld(w, r, []verificationSummary{{msg.ID, msg.Subject, msg.Snippet}})
	if !ok || withheld == nil {
		return ok
	}
	hide := withheld[msg.ID]
	if hide {
		msg.Subject, msg.Snippet, msg.Body, msg.BodyText, msg.BodyHTML = verification.Masked, verification.Masked, verification.Masked, verification.Masked, ""
	}
	withholdField(&msg.ConversationTitle, hide)
	for i := range msg.Attachments {
		withholdField(&msg.Attachments[i].Filename, hide)
	}
	return true
}

// Parent and filename checks finish inside the snapshot, before blob I/O.
func (s *Server) prepareVerificationAttachment(w http.ResponseWriter, r *http.Request, att *query.AttachmentInfo) (*bool, bool) {
	withheld, ok := s.verificationWithheld(w, r, []verificationSummary{{ID: att.MessageID}})
	if !ok || withheld == nil {
		return nil, ok
	}
	withholdField(&att.Filename, withheld[att.MessageID])
	return new(withheld[att.MessageID]), true
}

// The caller owns content. Inspection preserves the original download bytes.
func (s *Server) verificationAttachmentReader(content io.Reader, att query.AttachmentInfo, parentWithheld *bool) (io.Reader, *apiHTTPError) {
	if parentWithheld == nil {
		return content, nil
	}
	reader := bufio.NewReaderSize(content, 512)
	isMIME := strings.HasPrefix(strings.ToLower(att.MimeType), "message/") || strings.EqualFold(filepath.Ext(att.Filename), ".eml")
	text := strings.HasPrefix(strings.ToLower(att.MimeType), "text/")
	if !isMIME && !text {
		prefix, err := reader.Peek(512)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, newAPIHTTPError(500, "internal_error", "Failed to read attachment")
		}
		text = strings.HasPrefix(http.DetectContentType(prefix), "text/")
	}
	if !isMIME && !text {
		return reader, nil
	}
	denied := newAPIHTTPError(403, "verification_masked", "Attachment text requires verification.read")
	if *parentWithheld {
		return nil, denied
	}
	limit := verification.MaxFieldBytes
	if isMIME {
		limit = 8 * 1024 * 1024
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, newAPIHTTPError(500, "internal_error", "Failed to read attachment")
	}
	budget := 8 * 1024 * 1024
	if len(data) > limit || verificationAttachmentContent(data, isMIME, &budget) {
		return nil, denied
	}
	return bytes.NewReader(data), nil
}

func verificationAttachmentContent(data []byte, isMIME bool, budget *int) bool {
	parsed, parseErr := msgmime.Parse(data)
	if parseErr != nil && isMIME {
		return true
	}
	if parseErr == nil {
		*budget -= len(parsed.BodyText) + len(parsed.BodyHTML)
		if *budget < 0 || verification.Is(verification.Content{Subject: parsed.Subject, Text: parsed.BodyText, HTML: parsed.BodyHTML}) {
			return true
		}
		for _, att := range parsed.Attachments {
			nested := strings.HasPrefix(strings.ToLower(att.ContentType), "message/") || strings.EqualFold(filepath.Ext(att.Filename), ".eml")
			if (nested || strings.HasPrefix(strings.ToLower(att.ContentType), "text/") || strings.HasPrefix(http.DetectContentType(att.Content), "text/")) && verificationAttachmentContent(att.Content, nested, budget) {
				return true
			}
		}
		if isMIME {
			return false
		}
	}
	if len(data) >= verification.MaxFieldBytes {
		return true
	}
	fallback := unicode.UTF8
	sample := data[:min(len(data), 512)]
	if len(sample) >= 4 {
		interleaved := true
		for i := 1; i < len(sample); i += 2 {
			interleaved = interleaved && sample[i] == 0
		}
		if interleaved {
			fallback = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
		}
	}
	decoded, _, err := transform.Bytes(unicode.BOMOverride(fallback.NewDecoder()), data)
	*budget -= len(decoded)
	return err != nil || *budget < 0 || verification.Is(verification.Content{Text: string(decoded), HTML: string(decoded)})
}
