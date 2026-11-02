package api

import (
	"net/http"
	"time"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/verification"
)

func (s *Server) handleVerificationArrivals(w http.ResponseWriter, r *http.Request) {
	// Owner polls must see newly archived messages even while analytics caches
	// are stale. Owner reads do not pass through the source-scope middleware, so
	// give the complete candidate scan and detail hydration one Store snapshot.
	if s.requestAuthentication(r).Grant == nil {
		release, apiErr := s.beginCallerRead(r)
		if apiErr != nil {
			verificationUnavailable(w)
			return
		}
		defer release()
	}
	snapshotStore, ok := s.store.(agentReadSnapshotStore)
	if !ok {
		verificationUnavailable(w)
		return
	}
	engine := query.NewEngine(snapshotStore.DB(), snapshotStore.IsPostgreSQL())
	options := verification.ListOptions{Sender: r.URL.Query().Get("sender"), Kind: r.URL.Query().Get("kind")}
	var err error
	if raw := r.URL.Query().Get("since"); raw != "" {
		options.Since, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, 400, "invalid_since", "since must be RFC3339")
			return
		}
	}
	if options.Limit, _, err = queryInt(r, "limit"); err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if options.IncludeExpired, _, err = queryBool(r, "include_expired"); err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if options.Reveal, _, err = queryBool(r, "reveal"); err != nil {
		s.rejectBadParam(w, err)
		return
	}
	now := time.Now().UTC()
	options, err = options.Normalize(now)
	if err != nil {
		writeError(w, 400, "invalid_verification_filter", err.Error())
		return
	}
	filter := query.MessageFilter{}
	var reveal func(int64) (bool, error)
	if s.requestAuthentication(r).Grant != nil {
		filter.SourceIDs = agentReadSourceIDs(r)
		reveal = func(id int64) (bool, error) { return s.verificationReveal(r, id) }
	}
	page, err := query.ScanVerifications(r.Context(), engine, filter, options, now, reveal)
	if err != nil {
		verificationUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
