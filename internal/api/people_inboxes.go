package api

import (
	"net/http"

	"go.kenn.io/msgvault/internal/query"
)

func (s *Server) handleParticipantInboxes(w http.ResponseWriter, r *http.Request) {
	participantID, ok := positiveParticipantPathID(w, r)
	if !ok {
		return
	}
	engine := s.queryEngineForContext(r.Context())
	inboxes, ok := engine.(query.PeopleInboxAnalyzer)
	if !ok {
		s.writeExploreUnavailable(r.Context(), w, query.CacheAbsent)
		return
	}
	people, ok := engine.(query.PeopleAnalyzer)
	if !ok {
		s.writeExploreUnavailable(r.Context(), w, query.CacheAbsent)
		return
	}

	person, err := people.GetPerson(
		r.Context(), participantID, query.Context{}, s.clusterMemberIDs(participantID),
	)
	if err != nil {
		s.writeExploreError(r.Context(), w, err)
		return
	}
	if person == nil {
		writeError(w, http.StatusNotFound, "participant_not_found", "Participant cluster not found")
		return
	}

	canonicalID, err := inboxes.ResolveCanonicalParticipant(r.Context(), participantID)
	if err != nil {
		s.writeExploreError(r.Context(), w, err)
		return
	}
	result, err := inboxes.ListPersonInboxes(r.Context(), query.PersonInboxRequest{CanonicalID: canonicalID})
	if err != nil {
		s.writeExploreError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
