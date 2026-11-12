package credentialprovider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"uuid"
)

// renewMargin is how long before expiry a held credential stops being used, so
// a request does not reach the endpoint just after its credential expires.
const renewMargin = 30 * time.Second

// RefusalError reports a helper's refusal. Status and Reason are fixed codes.
type RefusalError struct {
	Status Status
	Reason string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("credential provider refused: %s (%s)", e.Status, e.Reason)
}

// InboundToken is a bearer for the source endpoint. Pass it back to Renew when
// the endpoint rejects it.
type InboundToken struct {
	Bearer       string
	credentialID uuid.UUID
}

// InboundSource holds one source credential in process memory and serializes
// its exchanges. Requests are not journaled: after a restart or a refusal the
// next exchange is a new authorize.
type InboundSource struct {
	argv  []string
	scope InboundScope

	mu   sync.Mutex
	held *heldCredential
}

type heldCredential struct {
	requestID  uuid.UUID
	id         uuid.UUID
	generation int64
	bearer     string
	started    time.Time
	usable     time.Duration
}

// NewInboundSource checks argv and scope without starting the helper.
func NewInboundSource(argv []string, scope InboundScope) (*InboundSource, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" || !validInboundScope(scope) {
		return nil, ErrInvalidRequest
	}
	return &InboundSource{argv: argv, scope: scope}, nil
}

// Token returns the held credential while it is usable and otherwise renews or
// authorizes one.
func (s *InboundSource) Token(ctx context.Context) (InboundToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held != nil && s.held.valid(time.Now()) {
		return s.held.token(), nil
	}
	return s.exchangeLocked(ctx)
}

// Renew replaces a credential the endpoint rejected. If another caller already
// replaced it, Renew returns the held successor without starting the helper.
func (s *InboundSource) Renew(ctx context.Context, rejected InboundToken) (InboundToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held != nil && s.held.id != rejected.credentialID && s.held.valid(time.Now()) {
		return s.held.token(), nil
	}
	return s.exchangeLocked(ctx)
}

func (s *InboundSource) exchangeLocked(ctx context.Context) (InboundToken, error) {
	request := InboundRequest{InboundScope: s.scope, Version: 1, Operation: "authorize", RequestID: uuid.New()}
	if s.held != nil {
		request.Operation = "renew"
		request.PreviousRequestID = s.held.requestID
		request.CredentialID = s.held.id
		request.ExpectedGeneration = s.held.generation
	}
	started := time.Now()
	response, err := ExchangeInbound(ctx, s.argv, request)
	if err != nil {
		return InboundToken{}, err
	}
	if response.Status != StatusReady {
		s.held = nil
		return InboundToken{}, &RefusalError{Status: response.Status, Reason: response.Reason}
	}
	lifetime := response.ExpiresAt.Sub(response.IssuedAt)
	s.held = &heldCredential{
		requestID:  request.RequestID,
		id:         response.CredentialID,
		generation: response.CredentialGeneration,
		bearer:     response.Credential.Token,
		started:    started,
		usable:     lifetime - min(lifetime/2, renewMargin),
	}
	return s.held.token(), nil
}

// valid bounds both monotonic and wall-clock elapsed time. The monotonic clock
// may stop while the host sleeps; the wall clock does not.
func (h *heldCredential) valid(now time.Time) bool {
	return now.Sub(h.started) < h.usable && now.Round(0).Sub(h.started.Round(0)) < h.usable
}

func (h *heldCredential) token() InboundToken {
	return InboundToken{Bearer: h.bearer, credentialID: h.id}
}
