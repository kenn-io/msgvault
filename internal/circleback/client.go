package circleback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"
)

// Session is a thin wrapper over an MCP client session with client-side
// pacing and JSON-tolerant tool-result decoding.
type Session struct {
	cs      *mcp.ClientSession
	limiter *rate.Limiter

	// rateLimitDelay is the first backoff pause after a provider rate-limit
	// rejection; each further attempt doubles it. Zero disables waiting so
	// tests exercise the retry loop without sleeping.
	rateLimitDelay time.Duration
}

// ToolInfo is the MCP tool metadata printed by sync-circleback --probe.
type ToolInfo struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ErrContract identifies a Circleback result that cannot be interpreted
// without risking silent data loss.
var ErrContract = errors.New("circleback MCP contract error")

// errRateLimited marks a provider rate-limit rejection. It is the only
// tool-result error worth retrying: unlike a contract or missing-result
// failure it is transient and clears on its own, and without a retry it
// fails the whole sync run. A first backfill issues one detail call per
// five meetings, so a large history reliably trips the provider's limit.
var errRateLimited = errors.New("circleback rate limited")

const (
	// rateLimitAttempts bounds total tries (one initial + retries) so a
	// sustained limit still surfaces instead of hanging the run. Meetings
	// written before the failure persist, and the next run resumes from
	// them, so giving up is recoverable.
	rateLimitAttempts = 5
	// rateLimitBaseDelay is the first pause; it doubles per attempt, so the
	// default budget is roughly 2+4+8+16 = 30s of waiting.
	rateLimitBaseDelay = 2 * time.Second
	// rateLimitMaxDelay caps a single pause so the doubling cannot stall a
	// run for minutes.
	rateLimitMaxDelay = 30 * time.Second
)

const archiveIntent = "Archiving Circleback meetings in msgvault."

// clientVersion labels msgvault in the MCP handshake.
var clientVersion = "dev"

// Connect opens an MCP session against endpoint using the given OAuth
// handler (see Manager.Handler).
func Connect(ctx context.Context, endpoint string, handler auth.OAuthHandler) (*Session, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "msgvault", Version: clientVersion}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:     endpoint,
		OAuthHandler: handler,
		// Pull-based sync only needs request/response; skipping the
		// standalone SSE stream avoids a persistent idle connection.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to circleback MCP at %s: %w", endpoint, err)
	}
	return &Session{cs: cs, limiter: rate.NewLimiter(2, 4), rateLimitDelay: rateLimitBaseDelay}, nil
}

// NewSessionForTesting wraps an already-connected mcp.ClientSession (e.g.
// over in-memory transports) in a Session.
func NewSessionForTesting(cs *mcp.ClientSession) *Session {
	return &Session{cs: cs, limiter: rate.NewLimiter(rate.Inf, 1)}
}

// Close shuts the session down.
func (s *Session) Close() error {
	if err := s.cs.Close(); err != nil {
		return fmt.Errorf("close circleback MCP session: %w", err)
	}
	return nil
}

// ToolInventory returns each tool's name, description, and input schema for
// sync-circleback --probe diagnostics.
func (s *Session) ToolInventory(ctx context.Context) ([]ToolInfo, error) {
	var out []ToolInfo
	cursor := ""
	for {
		res, err := s.cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("list circleback tools: %w", err)
		}
		for _, tool := range res.Tools {
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil {
				return nil, fmt.Errorf("marshal input schema for circleback tool %s: %w", tool.Name, err)
			}
			out = append(out, ToolInfo{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: schema,
			})
		}
		if res.NextCursor == "" {
			return out, nil
		}
		if res.NextCursor == cursor {
			return nil, errors.New("list circleback tools: server repeated the pagination cursor")
		}
		cursor = res.NextCursor
	}
}

// CallToolJSON calls an MCP tool and returns its result payload as raw JSON:
// StructuredContent when the server provides it, otherwise the concatenated
// text content (which Circleback uses to carry JSON).
// A provider rate-limit rejection is retried with exponential backoff; every
// other error returns immediately.
func (s *Session) CallToolJSON(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	delay := s.rateLimitDelay
	var lastErr error
	for attempt := 1; attempt <= rateLimitAttempts; attempt++ {
		payload, err := s.callToolOnce(ctx, name, args)
		if err == nil {
			return payload, nil
		}
		if !errors.Is(err, errRateLimited) {
			return nil, err
		}
		lastErr = err
		if attempt == rateLimitAttempts {
			break
		}
		if err := sleepContext(ctx, delay); err != nil {
			return nil, fmt.Errorf("circleback tool %s: %w", name, err)
		}
		delay = min(delay*2, rateLimitMaxDelay)
	}
	// lastErr already names the tool, so this does not repeat it.
	return nil, fmt.Errorf("still rate limited after %d attempts: %w", rateLimitAttempts, lastErr)
}

// callToolOnce performs one paced tool call.
func (s *Session) callToolOnce(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("wait for circleback rate limit: %w", err)
	}
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("circleback tool %s: %w", name, err)
	}
	payload, err := toolResultJSON(res)
	if err != nil {
		// %w keeps errRateLimited reachable through errors.Is so the
		// retry loop in CallToolJSON can still recognize it.
		return nil, fmt.Errorf("circleback tool %s: %w", name, err)
	}
	return payload, nil
}

// sleepContext waits for d, returning early if ctx is cancelled. A
// non-positive duration returns immediately.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isRateLimitMessage reports whether a tool-result error text is a provider
// rate-limit rejection. Circleback reports these in band as a plain message
// ("Rate limit exceeded. Please try again later.") with no status code or
// machine-readable field, so matching on the text is the only signal
// available; the extra variants cover equivalent phrasings.
func isRateLimitMessage(message string) bool {
	message = strings.ToLower(message)
	for _, marker := range []string{"rate limit", "rate-limit", "too many requests", "429"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// toolResultJSON extracts the JSON payload from a tool result.
func toolResultJSON(res *mcp.CallToolResult) (json.RawMessage, error) {
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if res.IsError {
		message := strings.TrimSpace(text.String())
		if isRateLimitMessage(message) {
			return nil, fmt.Errorf("%w: %s", errRateLimited, message)
		}
		return nil, fmt.Errorf("server returned an error: %s", message)
	}
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("re-marshal structured content: %w", err)
		}
		return b, nil
	}
	body := strings.TrimSpace(text.String())
	if body == "" {
		return nil, errors.New("empty tool result")
	}
	return json.RawMessage(body), nil
}

// SearchMeetings finds one zero-based page of meetings in [start, end). Empty
// times omit the corresponding bound.
func (s *Session) SearchMeetings(ctx context.Context, start, end string, pageIndex int) ([]Meeting, error) {
	args := map[string]any{
		"intent":    archiveIntent,
		"pageIndex": pageIndex,
	}
	if start != "" {
		args["startDate"] = start
	}
	if end != "" {
		args["endDate"] = end
	}
	raw, err := s.CallToolJSON(ctx, toolSearchMeetings, args)
	if err != nil {
		return nil, err
	}
	items, err := decodeItems(raw, "meetings")
	if err != nil {
		return nil, fmt.Errorf("decode SearchMeetings result: %w", err)
	}
	return decodeMeetings(items)
}

// ReadMeetings fetches full details for the given meeting IDs.
func (s *Session) ReadMeetings(ctx context.Context, ids []string) ([]Meeting, error) {
	raw, err := s.CallToolJSON(ctx, toolReadMeetings, map[string]any{
		"intent":     archiveIntent,
		"meetingIds": ids,
	})
	if err != nil {
		return nil, err
	}
	items, err := decodeItems(raw, "meetings")
	if err != nil {
		return nil, fmt.Errorf("decode ReadMeetings result: %w", err)
	}
	return decodeMeetings(items)
}

// GetTranscripts fetches transcripts for the given meeting IDs, keyed by
// meeting ID.
func (s *Session) GetTranscripts(ctx context.Context, ids []string) (map[string]*Transcript, error) {
	raw, err := s.CallToolJSON(ctx, toolGetTranscripts, map[string]any{
		"intent":     archiveIntent,
		"meetingIds": ids,
	})
	if err != nil {
		return nil, err
	}
	items, err := decodeItems(raw, "transcripts")
	if err != nil {
		return nil, fmt.Errorf("decode GetTranscriptsForMeetings result: %w", err)
	}
	out := make(map[string]*Transcript, len(items))
	for _, item := range items {
		tr, err := decodeTranscript(item)
		if err != nil {
			return nil, fmt.Errorf("decode GetTranscriptsForMeetings result: %w", err)
		}
		out[tr.ResolvedID()] = tr
	}
	return out, nil
}

func decodeTranscript(item json.RawMessage) (*Transcript, error) {
	var tr Transcript
	if err := json.Unmarshal(item, &tr); err != nil {
		return nil, fmt.Errorf("%w: decode transcript: %w", ErrContract, err)
	}
	tr.Raw = item
	if tr.ResolvedID() == "" {
		return nil, fmt.Errorf("%w: transcript has no id or meetingId", ErrContract)
	}
	if tr.Classification() == TranscriptUnrecognized {
		return nil, fmt.Errorf("%w: transcript %q has no transcript or text field", ErrContract, tr.ResolvedID())
	}
	return &tr, nil
}

// decodeMeetings unmarshals meeting items, preserving each verbatim payload.
func decodeMeetings(items []json.RawMessage) ([]Meeting, error) {
	out := make([]Meeting, 0, len(items))
	for _, item := range items {
		var m Meeting
		if err := json.Unmarshal(item, &m); err != nil {
			return nil, fmt.Errorf("decode meeting: %w", err)
		}
		m.Raw = item
		out = append(out, m)
	}
	return out, nil
}

// decodeItems accepts the envelope shapes tool results come in: a bare JSON
// array, an object with the named list key (or "items"/"results"), or a
// single object (treated as a one-item list).
func decodeItems(raw json.RawMessage, listKey string) ([]json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		return items, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	for _, key := range []string{listKey, "items", "results", "data"} {
		inner, ok := envelope[key]
		if !ok {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(inner, &items); err == nil {
			return items, nil
		}
	}
	// A single object: treat the envelope itself as one item.
	return []json.RawMessage{raw}, nil
}
