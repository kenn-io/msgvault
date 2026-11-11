package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/verification"
)

type MeetingRecordStore interface {
	MeetingRecordContext(ctx context.Context, ref store.MeetingLinkRef) (store.MeetingRecord, error)
}

func (s *Server) registerMeetingRecordRoute(api huma.API) {
	op := rawAPIV1Operation("getMeetingRecord", http.MethodGet, "/meetings/record", "Read an archived meeting from a note or event")
	op.Tags = []string{"Meetings"}
	op.Parameters = []*huma.Param{{Name: "ref", In: "query", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}}
	op.Responses = jsonResponsesFor[store.MeetingRecord](api)
	op.Errors = []int{400, 401, 403, 404, 500, 503}
	addErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleMeetingRecord)
}

func parseMeetingLinkRef(value string) (store.MeetingLinkRef, error) {
	ref, err := store.ParseMeetingLinkRef(value)
	if err != nil {
		return ref, err
	}
	if ref.MessageID > maxPublicMeetingID {
		return store.MeetingLinkRef{}, errors.New("invalid meeting reference")
	}
	return ref, nil
}

func (s *Server) writeMeetingLinkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, 404, "meeting_not_found", "Meeting or event occurrence not found; use msg:<id> for notes or copy REF from calendar events.")
	case errors.Is(err, store.ErrMeetingOccurrenceRequired):
		writeError(w, 400, "invalid_argument", err.Error())
	case errors.Is(err, store.ErrQueryTooBroad):
		writeError(w, 400, "query_too_broad", err.Error())
	default:
		if !s.writeIfContextError(w, err) {
			writeError(w, 500, "meeting_links_failed", err.Error())
		}
	}
}

func (s *Server) handleMeetingRecord(w http.ResponseWriter, r *http.Request) {
	ref, err := parseMeetingLinkRef(r.URL.Query().Get("ref"))
	if err != nil {
		writeError(w, 400, "invalid_argument", err.Error())
		return
	}
	backend, ok := s.store.(MeetingRecordStore)
	if !ok {
		writeError(w, 503, "meetings_unavailable", "Meeting links are unavailable")
		return
	}
	record, err := backend.MeetingRecordContext(r.Context(), ref)
	if err != nil {
		s.writeMeetingLinkError(w, err)
		return
	}
	ids := make([]int64, 0, len(record.Links)*2)
	for _, link := range record.Links {
		ids = append(ids, link.MessageID)
		if link.SubjectOriginID != 0 {
			ids = append(ids, link.SubjectOriginID)
		}
	}
	withheld, ok := s.verificationWithheld(w, r, ids)
	if !ok {
		return
	}
	for i := range record.Links {
		link := &record.Links[i]
		if withheld[link.MessageID] || withheld[link.SubjectOriginID] {
			link.Subject = verification.Masked
		}
	}
	writeJSON(w, 200, record)
}
