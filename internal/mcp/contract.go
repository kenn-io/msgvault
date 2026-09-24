package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolRequest struct {
	arguments                     map[string]any
	session                       *sdkmcp.ServerSession
	inputResponses                sdkmcp.InputResponseMap
	toolName                      string
	requestState                  string
	confirmations                 *confirmationChallenges
	confirmationSessionKey        string
	requireConfirmationSessionKey bool
}

type confirmationChallenge struct {
	session    *sdkmcp.ServerSession
	sessionKey string
	digest     [32]byte
	expires    time.Time
}

type confirmationChallenges struct {
	mu      sync.Mutex
	pending map[string]confirmationChallenge
	now     func() time.Time
}

const (
	confirmationChallengeTTL = 5 * time.Minute
	maxPendingConfirmations  = 1024
)

func newConfirmationChallenges() *confirmationChallenges {
	return &confirmationChallenges{pending: make(map[string]confirmationChallenge), now: time.Now}
}

func (c *confirmationChallenges) issue(session *sdkmcp.ServerSession, sessionKey, toolName string, arguments map[string]any, message string) (string, error) {
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
	for key, challenge := range c.pending {
		if !challenge.expires.After(now) {
			delete(c.pending, key)
		}
	}
	if len(c.pending) >= maxPendingConfirmations {
		return "", errors.New("too many pending confirmation requests")
	}
	c.pending[token] = confirmationChallenge{session: session, sessionKey: sessionKey, digest: digest, expires: now.Add(confirmationChallengeTTL)}
	return token, nil
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

func (r toolRequest) GetArguments() map[string]any {
	return r.arguments
}

func (r toolRequest) confirmUserAction(ctx context.Context, message string) error {
	if r.session == nil || (r.requireConfirmationSessionKey && r.confirmationSessionKey == "") {
		return errors.New("user confirmation is unavailable")
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
			return errors.New("action cancelled; explicit user confirmation is required")
		}
		if r.confirmations == nil || !r.confirmations.consume(r.requestState, r.session, r.confirmationSessionKey, r.toolName, r.arguments, message) {
			return errors.New("action cancelled; confirmation does not match a pending request")
		}
		return nil
	}
	if version := r.session.InitializeParams().ProtocolVersion; version >= "2026-07-28" {
		return &confirmationRequiredError{params: params}
	}
	result, err := r.session.Elicit(ctx, params)
	if err != nil {
		return fmt.Errorf("user confirmation is unavailable: %w", err)
	}
	if result.Action != "accept" || result.Content["confirm"] != true {
		return errors.New("action cancelled; explicit user confirmation is required")
	}
	return nil
}

type confirmationRequiredError struct {
	params *sdkmcp.ElicitParams
}

func (e *confirmationRequiredError) Error() string {
	return "user confirmation required"
}

func confirmationToolError(err error) (*toolResult, error) {
	if _, ok := errors.AsType[*confirmationRequiredError](err); ok {
		return nil, err
	}
	return toolErrorResult(err.Error()), nil
}

type embeddedResource struct {
	uri      string
	mimeType string
	blob     string
}

type toolResult struct {
	text              string
	structuredContent jsontext.Value
	embeddedResource  *embeddedResource
	isError           bool
}

type internalError struct {
	operation string
	cause     error
}

func (e *internalError) Error() string {
	return fmt.Sprintf("%s: %v", e.operation, e.cause)
}

func (e *internalError) Unwrap() error {
	return e.cause
}

func newInternalError(operation string, err error) error {
	if _, ok := errors.AsType[*internalError](err); ok {
		return err
	}
	return &internalError{operation: operation, cause: err}
}

func jsonResult(v any) (*toolResult, error) {
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return nil, &internalError{
			operation: "marshal tool result",
			cause:     err,
		}
	}

	raw := jsontext.Value(data)
	return &toolResult{
		text:              string(raw),
		structuredContent: raw,
	}, nil
}

func toolErrorResult(text string) *toolResult {
	return &toolResult{
		text:    text,
		isError: true,
	}
}
