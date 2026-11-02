package api

import (
	"context"

	"go.kenn.io/msgvault/internal/query"
)

func (s *Server) configureSenderClass(engine query.Engine) {
	if resolver, ok := s.store.(query.SenderClassResolver); ok {
		query.ConfigureSenderClass(engine, resolver)
	}
}

func (s *Server) fillStoreSenderClasses(ctx context.Context, messages []APIMessage) {
	inputs := make([]query.SenderClassInput, len(messages))
	for i, message := range messages {
		from := message.FromEmail
		if from == "" {
			from = message.From
		}
		inputs[i] = query.SenderClassInput{ID: message.ID, From: []query.Address{{Email: from}}, SenderAuth: message.SenderAuth}
	}
	engine := s.queryEngineForContext(ctx)
	if archiveEngine, ok := s.storeSenderAuthReader(ctx).(query.Engine); ok {
		engine = archiveEngine
		s.configureSenderClass(engine)
	}
	classes := query.ReadSenderClasses(ctx, engine, inputs)
	for i := range messages {
		messages[i].Readout = classes[messages[i].ID]
	}
}
