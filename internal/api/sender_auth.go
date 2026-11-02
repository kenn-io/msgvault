package api

import (
	"context"
	"database/sql"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
)

func (s *Server) fillStoreSenderAuth(ctx context.Context, messages []APIMessage) {
	if len(messages) == 0 {
		return
	}
	ids := make([]int64, len(messages))
	for i, m := range messages {
		ids[i] = m.ID
	}
	reader := s.storeSenderAuthReader(ctx)
	var results map[int64]*senderauth.Verdict
	if reader != nil {
		results = reader.ReadSenderAuth(ctx, ids)
	}
	for i := range messages {
		verdict := results[messages[i].ID]
		if verdict == nil {
			verdict = senderauth.UnknownVerdict("no_raw")
		}
		from := messages[i].FromEmail
		if from == "" {
			from = messages[i].From
		}
		messages[i].SenderAuth = query.SenderAuthForAddresses(verdict, []query.Address{{Email: from}})
	}
	s.fillStoreSenderClasses(ctx, messages)
}

// Store-backed responses derive receipt metadata from the same archive and
// request snapshot as their selected records, even without an analytics engine.
func (s *Server) storeSenderAuthReader(ctx context.Context) query.SenderAuthReader {
	if archive, ok := s.store.(interface {
		DB() *sql.DB
		IsPostgreSQL() bool
	}); ok && archive.DB() != nil {
		var dialect query.Dialect = query.SQLiteQueryDialect{}
		if archive.IsPostgreSQL() {
			dialect = query.PostgreSQLQueryDialect{}
		}
		reader := query.NewEngineWithDialect(archive.DB(), dialect)
		reader.SetSenderAuthPins(s.senderAuthPins)
		return reader
	}
	reader, _ := s.queryEngineForContext(ctx).(query.SenderAuthReader)
	return reader
}
