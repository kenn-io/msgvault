package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/verification"
)

type verificationRequestKey struct{}
type verificationRequest struct {
	sources  map[int64]agentgrant.SourceRef
	details  map[int64]*query.MessageDetail
	dropped  int
	examined int
}

func (s *Server) verificationQueryDrop(r *http.Request, canonical *query.MessageDetail, reveal bool) bool {
	if reveal {
		return false
	}
	queryText := r.URL.Query().Get("q")
	if queryText == "" {
		queryText = r.URL.Query().Get("search_query")
	}
	if !query.VerificationQueryMatches(canonical, query.VerificationQueryTokens(search.ParseOR(queryText))) {
		return false
	}
	state, err := s.verificationRequest(r)
	if err == nil {
		state.dropped++
	}
	return true
}

func verificationHitsDropped(r *http.Request) int {
	state, _ := r.Context().Value(verificationRequestKey{}).(*verificationRequest)
	if state == nil {
		return 0
	}
	return state.dropped
}

func verificationPageEnd(r *http.Request, offset, returned int) int {
	if state, ok := r.Context().Value(verificationRequestKey{}).(*verificationRequest); ok {
		return offset + state.examined
	}
	return offset + returned
}

func (s *Server) verificationRequest(r *http.Request) (*verificationRequest, error) {
	if cached, ok := r.Context().Value(verificationRequestKey{}).(*verificationRequest); ok {
		return cached, nil
	}
	return nil, errors.New("verification authorization snapshot unavailable")
}

func (s *Server) verificationReveal(r *http.Request, sourceID int64) (bool, error) {
	grant := s.requestAuthentication(r).Grant
	if grant == nil {
		return true, nil
	}
	state, err := s.verificationRequest(r)
	if err != nil {
		return false, err
	}
	source, ok := state.sources[sourceID]
	if !ok || !s.agentObjectSourceAllowed(r, sourceID) {
		return false, errors.New("verification source unavailable")
	}
	// Base operation permission was checked by beginAgentRead. This modifier
	// never adds a read right or expands the actual message's source scope.
	return grant.Allows(agentgrant.PermissionVerificationRead, source), nil
}

func verificationUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "verification_unavailable", "Canonical message protection is unavailable")
}

func (s *Server) prepareAgentDetail(r *http.Request, msg *query.MessageDetail) (*query.MessageDetail, error) {
	if s.requestAuthentication(r).Grant == nil {
		return msg, nil
	}
	if msg == nil {
		return nil, errors.New("canonical message unavailable")
	}
	reveal, err := s.verificationReveal(r, msg.SourceID)
	if err != nil {
		return nil, err
	}
	state, err := s.verificationRequest(r)
	if err != nil {
		return nil, err
	}
	state.details[msg.ID] = msg
	return query.PrepareVerification(msg, reveal, time.Now()), nil
}

func (s *Server) verificationCanonical(r *http.Request, id int64) (*query.MessageDetail, bool, error) {
	state, err := s.verificationRequest(r)
	if err != nil {
		return nil, false, err
	}
	msg := state.details[id]
	if msg == nil {
		engine := s.queryEngineForContext(r.Context())
		if engine == nil {
			if snapshotStore, ok := s.store.(agentReadSnapshotStore); ok {
				engine = query.NewEngine(snapshotStore.DB(), snapshotStore.IsPostgreSQL())
			} else {
				return nil, false, errors.New("canonical message engine unavailable")
			}
		}
		msg, err = engine.GetMessage(r.Context(), id)
		if err != nil {
			return nil, false, err
		}
		if msg == nil || msg.ID != id {
			return nil, false, errors.New("canonical message unavailable")
		}
		state.details[id] = msg
	}
	reveal, err := s.verificationReveal(r, msg.SourceID)
	return msg, reveal, err
}

func (s *Server) prepareAgentSummaries(w http.ResponseWriter, r *http.Request, rows []query.MessageSummary) ([]query.MessageSummary, bool) {
	if s.requestAuthentication(r).Grant == nil {
		return rows, true
	}
	state, err := s.verificationRequest(r)
	if err != nil {
		verificationUnavailable(w)
		return nil, false
	}
	state.examined = len(rows)
	out := make([]query.MessageSummary, 0, len(rows))
	for _, row := range rows {
		canonical, reveal, err := s.verificationCanonical(r, row.ID)
		if err != nil || canonical.SourceID != row.SourceID {
			verificationUnavailable(w)
			return nil, false
		}
		if !s.verificationQueryDrop(r, canonical, reveal) {
			out = append(out, query.PrepareSummaryVerification(row, canonical, reveal, time.Now()))
		}
	}
	return out, true
}

func (s *Server) prepareAgentStored(w http.ResponseWriter, r *http.Request, rows []APIMessage) ([]APIMessage, bool) {
	return s.prepareAgentStoredWithQueryFilter(w, r, rows, true)
}

func (s *Server) prepareAgentStoredDetail(w http.ResponseWriter, r *http.Request, rows []APIMessage) ([]APIMessage, bool) {
	return s.prepareAgentStoredWithQueryFilter(w, r, rows, false)
}

func (s *Server) prepareAgentStoredWithQueryFilter(w http.ResponseWriter, r *http.Request, rows []APIMessage, filterSearchHits bool) ([]APIMessage, bool) {
	if s.requestAuthentication(r).Grant == nil {
		return rows, true
	}
	state, err := s.verificationRequest(r)
	if err != nil {
		verificationUnavailable(w)
		return nil, false
	}
	state.examined = len(rows)
	out := slices.Clone(rows)
	for i := range out {
		canonical, reveal, err := s.verificationCanonical(r, out[i].ID)
		if err != nil || canonical.SourceID != out[i].SourceID {
			verificationUnavailable(w)
			return nil, false
		}
		in := query.VerificationInput(canonical)
		finding := verification.Detect(in)
		if finding == nil {
			continue
		}
		m := verification.NewMasker(finding)
		row := &out[i]
		if !reveal {
			row.Subject = m.Text(row.Subject)
			if row.Snippet != "" {
				row.Snippet = verification.ContentPlaceholder
			}
			if canonical.BodyText == "" && canonical.BodyHTML != "" {
				row.Body = m.HTML(row.Body)
			} else {
				row.Body = m.Text(row.Body)
			}
			row.BodyText, row.BodyHTML = m.Text(row.BodyText), m.HTML(row.BodyHTML)
			row.From, row.FromEmail, row.FromName, row.FromPhone = m.Text(row.From), m.Text(row.FromEmail), m.Text(row.FromName), m.Text(row.FromPhone)
			row.To, row.Cc, row.Bcc, row.Labels = m.Strings(row.To), m.Strings(row.Cc), m.Strings(row.Bcc), m.Strings(row.Labels)
			row.ConversationTitle = m.Text(row.ConversationTitle)
			row.Calendar = nil
			row.Headers = nil
			row.Attachments = slices.Clone(row.Attachments)
			for j := range row.Attachments {
				att := &row.Attachments[j]
				att.Filename, att.URL, att.Docbank = m.Text(att.Filename), m.Text(att.URL), nil
			}
		}
		// The summary fields are measured by the common DTO helper; full store
		// details take the engine path whenever the engine supports them.
		prepared := query.PrepareVerification(canonical, reveal, time.Now())
		row.Verification = prepared.Verification
		if !reveal && row.Snippet != "" && !slices.Contains(row.Verification.MaskedFields, "snippet") {
			row.Verification.MaskedFields = append(row.Verification.MaskedFields, "snippet")
		}
	}
	if !filterSearchHits {
		return out, true
	}
	filtered := make([]APIMessage, 0, len(out))
	for _, row := range out {
		canonical, reveal, err := s.verificationCanonical(r, row.ID)
		if err != nil {
			verificationUnavailable(w)
			return nil, false
		}
		if !s.verificationQueryDrop(r, canonical, reveal) {
			filtered = append(filtered, row)
		}
	}
	return filtered, true
}

func (s *Server) prepareAgentThread(w http.ResponseWriter, r *http.Request, page *query.ThreadPage) (*query.ThreadPage, bool) {
	if s.requestAuthentication(r).Grant == nil {
		return page, true
	}
	out := *page
	out.Messages = slices.Clone(page.Messages)
	for i, row := range out.Messages {
		canonical, reveal, err := s.verificationCanonical(r, row.ID)
		if err != nil || canonical.SourceID != page.SourceID || canonical.ConversationID != page.ConversationID {
			verificationUnavailable(w)
			return nil, false
		}
		out.Messages[i] = query.PrepareThreadVerification(row, canonical, reveal, time.Now())
	}
	return &out, true
}

func (s *Server) maskedAgentScope(r *http.Request) bool {
	if s.requestAuthentication(r).Grant == nil {
		return false
	}
	for _, id := range agentReadSourceIDs(r) {
		if id < 0 {
			continue
		}
		reveal, err := s.verificationReveal(r, id)
		if err != nil || !reveal {
			return true
		}
	}
	return false
}

func (s *Server) prepareAgentAttachment(r *http.Request, att *query.AttachmentInfo, content bool) (*query.AttachmentInfo, *apiHTTPError) {
	if s.requestAuthentication(r).Grant == nil {
		return att, nil
	}
	if att.MessageID == 0 {
		return nil, newAPIHTTPError(503, "verification_unavailable", "Attachment parent protection is unavailable")
	}
	msg, reveal, err := s.verificationCanonical(r, att.MessageID)
	if err != nil || msg.SourceID != att.SourceID {
		return nil, newAPIHTTPError(503, "verification_unavailable", "Attachment parent protection is unavailable")
	}
	f := verification.Detect(query.VerificationInput(msg))
	if reveal || f == nil {
		return att, nil
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(att.MimeType, ";")[0]))
	if content && (strings.HasPrefix(mime, "text/") || mime == "message/rfc822") {
		return nil, newAPIHTTPError(403, "verification_masked", "Credential-bearing text attachments require verification.read on the message source")
	}
	out := *att
	m := verification.NewMasker(f)
	out.Filename, out.URL, out.Docbank = m.Text(att.Filename), m.Text(att.URL), nil
	return &out, nil
}
