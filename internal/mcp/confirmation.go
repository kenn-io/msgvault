package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type confirmationChallenge struct {
	session       *sdkmcp.ServerSession
	sessionKey    string
	digest        [32]byte
	expires       time.Time
	referenceTime time.Time
}

type confirmationChallenges struct {
	mu      sync.Mutex
	pending map[string]confirmationChallenge
	now     func() time.Time
	closed  bool
}

const (
	confirmationChallengeTTL = 5 * time.Minute
	maxPendingConfirmations  = 1024
)

func newConfirmationChallenges() *confirmationChallenges {
	return &confirmationChallenges{pending: make(map[string]confirmationChallenge), now: time.Now}
}

func (c *confirmationChallenges) issue(session *sdkmcp.ServerSession, sessionKey, toolName string, arguments map[string]any, message string, referenceTimes ...time.Time) (string, error) {
	if session == nil && sessionKey == "" {
		return "", errors.New("confirmation requires an active session")
	}
	encoded, err := json.Marshal(struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
		Message   string         `json:"message"`
	}{Tool: toolName, Arguments: arguments, Message: message}, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode confirmation action: %w", err)
	}
	digest := sha256.Sum256(encoded)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("create confirmation challenge: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", errors.New("confirmation unavailable")
	}
	for key, challenge := range c.pending {
		if !challenge.expires.After(now) {
			delete(c.pending, key)
		}
	}
	if len(c.pending) >= maxPendingConfirmations {
		return "", errors.New("too many pending confirmation requests")
	}
	reference := now
	if len(referenceTimes) > 0 && !referenceTimes[0].IsZero() {
		reference = referenceTimes[0]
	}
	c.pending[token] = confirmationChallenge{session: session, sessionKey: sessionKey, digest: digest, expires: now.Add(confirmationChallengeTTL), referenceTime: reference}
	return token, nil
}

// referenceTime restores the first request's clock without consuming approval.
// The normal consume path still verifies the complete arguments and disclosure.
func (c *confirmationChallenges) referenceTime(token string, session *sdkmcp.ServerSession, sessionKey string) (time.Time, bool) {
	if token == "" || (session == nil && sessionKey == "") {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	challenge, ok := c.pending[token]
	if !ok || c.closed || !challenge.expires.After(c.now()) {
		return time.Time{}, false
	}
	matched := sessionKey != "" && challenge.sessionKey == sessionKey || sessionKey == "" && challenge.sessionKey == "" && challenge.session == session
	if !matched {
		return time.Time{}, false
	}
	return challenge.referenceTime, true
}

func (c *confirmationChallenges) consume(token string, session *sdkmcp.ServerSession, sessionKey, toolName string, arguments map[string]any, message string) bool {
	if token == "" || (session == nil && sessionKey == "") {
		return false
	}
	encoded, err := json.Marshal(struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
		Message   string         `json:"message"`
	}{Tool: toolName, Arguments: arguments, Message: message}, json.Deterministic(true))
	if err != nil {
		return false
	}
	digest := sha256.Sum256(encoded)
	c.mu.Lock()
	defer c.mu.Unlock()
	challenge, ok := c.pending[token]
	if !ok {
		return false
	}
	delete(c.pending, token)
	matchedSession := sessionKey != "" && challenge.sessionKey == sessionKey || sessionKey == "" && challenge.sessionKey == "" && challenge.session == session
	return matchedSession && challenge.expires.After(c.now()) && challenge.digest == digest
}

// confirmUserAction requires client confirmation for this call. The client is
// responsible for obtaining user approval before it confirms.
func (r toolRequest) confirmUserAction(ctx context.Context, message string) error {
	if r.session == nil || (r.requireConfirmationSessionKey && r.confirmationSessionKey == "") {
		return errors.New("client confirmation is unavailable")
	}
	params := &sdkmcp.ElicitParams{
		Message: message,
		RequestedSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"confirm": {Type: "boolean", Description: "Confirm this specific action"},
			},
			Required: []string{"confirm"},
		},
	}
	if response, ok := r.inputResponses["confirm"]; ok {
		result, ok := response.(*sdkmcp.ElicitResult)
		if !ok || result.Action != "accept" || result.Content["confirm"] != true {
			return errors.New("action cancelled; explicit client confirmation is required")
		}
		if r.confirmations == nil || !r.confirmations.consume(r.requestState, r.session, r.confirmationSessionKey, r.toolName, r.arguments, message) {
			return errors.New("action cancelled; confirmation does not match a pending request")
		}
		return nil
	}
	if version := r.session.InitializeParams().ProtocolVersion; version >= "2026-07-28" {
		return &confirmationRequiredError{params: params, referenceTime: r.confirmationTime}
	}
	if r.requireConfirmationSessionKey {
		return errors.New("client confirmation is unsupported on legacy HTTP")
	}
	result, err := r.session.Elicit(ctx, params)
	if err != nil {
		return fmt.Errorf("client confirmation is unavailable: %w", err)
	}
	if result.Action != "accept" || result.Content["confirm"] != true {
		return errors.New("action cancelled; explicit client confirmation is required")
	}
	return nil
}

type confirmationRequiredError struct {
	referenceTime time.Time
	params        *sdkmcp.ElicitParams
}

func (e *confirmationRequiredError) Error() string {
	return "client confirmation required"
}

func confirmationToolError(err error) (*toolResult, error) {
	if _, ok := errors.AsType[*confirmationRequiredError](err); ok {
		return nil, err
	}
	return toolErrorResult(err.Error()), nil
}

type confirmationConfig struct {
	manager           *confirmationChallenges
	sessionKey        string
	requireSessionKey bool
}

func (c *confirmationChallenges) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.pending)
	c.closed = true
}
