package api

import (
	"context"

	"go.kenn.io/msgvault/internal/senderauth"
)

type messageHeaderReader interface {
	MessageHeaderBlockContext(context.Context, int64) ([]byte, error)
}

func (s *Server) senderAuth(ctx context.Context, id, sourceID int64, messageType, from string) *senderauth.Verdict {
	pins := s.senderAuthPins[sourceID]
	if len(pins) == 0 || messageType != "email" {
		return nil
	}
	reader, ok := s.store.(messageHeaderReader)
	if !ok {
		return &senderauth.Verdict{}
	}
	block, err := reader.MessageHeaderBlockContext(ctx, id)
	if err != nil {
		return &senderauth.Verdict{}
	}
	return senderauth.Evaluate(block, pins, from)
}
