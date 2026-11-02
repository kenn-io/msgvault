package api

import (
	"context"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
)

func (s *Server) fillStoreSenderAuth(ctx context.Context, messages []APIMessage) {
	ids := make([]int64, len(messages))
	for i, m := range messages {
		ids[i] = m.ID
	}
	reader, ok := s.queryEngineForContext(ctx).(query.SenderAuthReader)
	if !ok {
		if archive, available := s.store.(agentReadSnapshotStore); available {
			engine := query.NewEngine(archive.DB(), archive.IsPostgreSQL())
			query.ConfigureSenderAuth(engine, s.senderAuthPins)
			reader, ok = engine.(query.SenderAuthReader)
		}
	}
	var results map[int64]*senderauth.Verdict
	if ok {
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
}
