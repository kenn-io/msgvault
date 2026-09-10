package api

import (
	"context"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// delegatedOperationAllowed is an exact-match allowlist of operation IDs.
func delegatedOperationAllowed(operationID string) bool {
	switch operationID {
	case "runCLI", "getCLIMessage", "getCLIMessageRaw", "getHealth":
		return true
	}
	return false
}

type agentGrantSourceResolver interface {
	GetSourceByIDContext(ctx context.Context, id int64) (*store.Source, error)
}

func (s *Server) authorizeDelegatedMessage(ctx context.Context, auth requestAuthentication, msg *query.MessageDetail) bool {
	if auth.Mode != AuthModeDelegated {
		return true
	}
	if msg == nil || msg.SourceID == 0 || auth.Grant == nil {
		return false
	}
	resolver, ok := s.store.(agentGrantSourceResolver)
	if !ok {
		return false
	}
	src, err := resolver.GetSourceByIDContext(ctx, msg.SourceID)
	if err != nil || src == nil {
		return false
	}
	ref := agentgrant.SourceRef{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}
	return auth.Grant.Allows(agentgrant.PermissionMessageRead, ref)
}
