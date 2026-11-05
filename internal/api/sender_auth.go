package api

import (
	"context"

	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/store"
)

type messageHeaderReader interface {
	MessageHeaderBlockContext(ctx context.Context, id int64) ([]byte, error)
}

func (s *Server) senderAuth(ctx context.Context, id, sourceID int64, messageType, from string) *senderauth.Verdict {
	pins := s.senderAuthPins[sourceID]
	if len(pins) == 0 || !store.IsEmailMessageType(messageType) {
		return nil
	}
	reader, ok := s.store.(messageHeaderReader)
	if !ok {
		return &senderauth.Verdict{}
	}
	block, err := reader.MessageHeaderBlockContext(ctx, id)
	if err != nil {
		s.logger.Warn("failed to read sender authentication headers", "message_id", id, "source_id", sourceID, "error", err)
		return &senderauth.Verdict{}
	}
	return senderauth.Evaluate(block, pins, from)
}
