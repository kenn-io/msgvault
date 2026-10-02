package plaud

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"
)

var ErrContract = errors.New("plaud MCP contract error")
var wrapperStart = regexp.MustCompile(`<untrusted-user-data-([A-Za-z0-9_-]+)\s+source="plaud-recording">`)

type Session struct {
	cs         *mcp.ClientSession
	limiter    *rate.Limiter
	retryDelay time.Duration
}

type ToolInfo struct {
	Name        string
	Description string
	InputSchema jsontext.Value
}

func Connect(ctx context.Context, endpoint string, handler auth.OAuthHandler) (*Session, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "msgvault", Version: "dev"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, OAuthHandler: handler, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to plaud MCP: %w", err)
	}
	return &Session{cs: cs, limiter: rate.NewLimiter(2, 2), retryDelay: time.Second}, nil
}

func (s *Session) Close() error {
	if err := s.cs.Close(); err != nil {
		return fmt.Errorf("close plaud MCP session: %w", err)
	}
	return nil
}

func (s *Session) ToolInventory(ctx context.Context) ([]ToolInfo, error) {
	var out []ToolInfo
	seen := map[string]bool{}
	cursor := ""
	for {
		res, err := s.cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("list plaud tools: %w", err)
		}
		for _, t := range res.Tools {
			schema, err := json.Marshal(t.InputSchema, json.Deterministic(true))
			if err != nil {
				return nil, err
			}
			out = append(out, ToolInfo{t.Name, t.Description, schema})
		}
		if res.NextCursor == "" {
			return out, nil
		}
		if seen[res.NextCursor] {
			return nil, fmt.Errorf("%w: repeated tools cursor", ErrContract)
		}
		seen[res.NextCursor] = true
		cursor = res.NextCursor
	}
}

// toolPayload reads only the matching data wrapper; surrounding prose is not
// archived or interpreted. A successful empty transcript is handled separately.
func toolPayload(res *mcp.CallToolResult) (jsontext.Value, error) {
	if res == nil {
		return nil, fmt.Errorf("%w: missing result", ErrContract)
	}
	if res.IsError {
		return nil, errors.New("plaud tool returned an error")
	}
	if res.StructuredContent != nil {
		return json.Marshal(res.StructuredContent, json.Deterministic(true))
	}
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	text := strings.TrimSpace(b.String())
	if loc := wrapperStart.FindStringSubmatchIndex(text); loc != nil {
		closeTag := "</untrusted-user-data-" + text[loc[2]:loc[3]] + ">"
		tail := text[loc[1]:]
		body, _, found := strings.Cut(tail, closeTag)
		if !found {
			return nil, fmt.Errorf("%w: unclosed recording wrapper", ErrContract)
		}
		text = strings.TrimSpace(body)
	}
	raw := jsontext.Value(text)
	if !raw.IsValid() {
		return nil, fmt.Errorf("%w: invalid tool payload", ErrContract)
	}
	return raw, nil
}

func (s *Session) call(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	for attempt := range 4 {
		if err := s.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("wait for plaud rate limit: %w", err)
		}
		res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		transient := false
		if err != nil {
			var n net.Error
			transient = errors.As(err, &n) || transientFailure(err.Error())
		}
		if res != nil && res.IsError {
			for _, c := range res.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					v := strings.ToLower(t.Text)
					transient = transient || transientFailure(v)
				}
			}
			err = fmt.Errorf("plaud tool %s returned an error", name)
		}
		if err == nil {
			return res, nil
		}
		if !transient || attempt == 3 {
			return nil, fmt.Errorf("plaud tool %s: %w", name, err)
		}
		timer := time.NewTimer(s.retryDelay * time.Duration(1<<attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("plaud retry exhausted")
}

// The official tools carry upstream API status errors as text. The SDK also
// uses HTTP status descriptions for rejected streamable-HTTP requests.
func transientFailure(message string) bool {
	message = strings.ToLower(message)
	for _, marker := range []string{"rate limit", "too many requests", "temporarily unavailable", "internal server error", "bad gateway", "service unavailable", "gateway timeout"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (s *Session) json(ctx context.Context, name string, args map[string]any) (jsontext.Value, error) {
	res, err := s.call(ctx, name, args)
	if err != nil {
		return nil, err
	}
	raw, err := toolPayload(res)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return raw, nil
}

func (s *Session) CurrentUser(ctx context.Context) (string, error) {
	raw, err := s.json(ctx, "get_current_user", map[string]any{})
	if err != nil {
		return "", err
	}
	var v struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("%w: invalid current user", ErrContract)
	}
	email := strings.ToLower(strings.TrimSpace(v.Email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", fmt.Errorf("%w: current user has no explicit account email", ErrContract)
	}
	return email, nil
}

func (s *Session) ListFiles(ctx context.Context, page, pageSize int) (FilePage, error) {
	raw, err := s.json(ctx, "list_files", map[string]any{"page": page, "page_size": pageSize})
	if err != nil {
		return FilePage{}, err
	}
	var v struct {
		Data     []jsontext.Value `json:"data"`
		Total    *int             `json:"total"`
		HasMore  *bool            `json:"has_more"`
		Complete *bool            `json:"complete"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Data == nil {
		return FilePage{}, fmt.Errorf("%w: list_files needs data array", ErrContract)
	}
	if v.Total != nil && *v.Total < 0 {
		return FilePage{}, fmt.Errorf("%w: negative list total", ErrContract)
	}
	if v.Complete != nil && !*v.Complete {
		return FilePage{}, fmt.Errorf("%w: list_files reports incomplete enumeration", ErrContract)
	}
	out := FilePage{Total: v.Total, HasMore: v.HasMore, Complete: v.Complete}
	for _, item := range v.Data {
		f, err := decodeFile(item)
		if err != nil {
			return FilePage{}, err
		}
		out.Files = append(out.Files, f)
	}
	return out, nil
}

func (s *Session) Recording(ctx context.Context, id string) (Recording, error) {
	raw, err := s.json(ctx, "get_file", map[string]any{"file_id": id})
	if err != nil {
		return Recording{}, err
	}
	file, err := decodeFile(raw)
	if err != nil {
		return Recording{}, err
	}
	if file.ID != id {
		return Recording{}, fmt.Errorf("%w: recording identity changed", ErrContract)
	}
	rec := Recording{File: file, Notes: []Note{}, Segments: []Segment{}}
	raw, err = s.json(ctx, "get_note", map[string]any{"file_id": id})
	if err != nil {
		return Recording{}, err
	}
	rec.Notes, err = decodeNotes(raw)
	if err != nil {
		return Recording{}, err
	}
	for _, block := range []string{"transaction_polish", "transaction"} {
		found := false
		for _, b := range file.Blocks {
			found = found || b == block
		}
		if !found {
			continue
		}
		segs, err := s.transcript(ctx, id, block)
		if err != nil {
			return Recording{}, err
		}
		if len(segs) > 0 {
			rec.Segments = segs
			rec.TranscriptBlock = block
			break
		}
	}
	return rec, nil
}

func (s *Session) transcript(ctx context.Context, id, block string) ([]Segment, error) {
	out := []Segment{}
	cursor := ""
	seen := map[string]bool{}
	total := -1
	for {
		args := map[string]any{"file_id": id, "block": block, "limit": 500}
		if cursor != "" {
			args["cursor"] = cursor
		}
		res, err := s.call(ctx, "get_transcript", args)
		if err != nil {
			return nil, err
		}
		if len(out) == 0 && emptyTranscriptResult(res, block) {
			return out, nil
		}
		raw, err := toolPayload(res)
		if err != nil {
			return nil, err
		}
		var p struct {
			FileID   string           `json:"file_id"`
			Block    string           `json:"block"`
			Total    *int             `json:"total"`
			Offset   *int             `json:"offset"`
			Returned *int             `json:"returned"`
			Next     string           `json:"next_cursor"`
			Segments []jsontext.Value `json:"segments"`
		}
		if err := json.Unmarshal(raw, &p); err != nil || p.FileID != id || p.Block != block || p.Total == nil || p.Offset == nil || p.Returned == nil || p.Segments == nil {
			return nil, fmt.Errorf("%w: invalid transcript page", ErrContract)
		}
		if *p.Total < 0 || *p.Offset != len(out) || *p.Returned != len(p.Segments) || len(out)+len(p.Segments) > *p.Total || total >= 0 && total != *p.Total {
			return nil, fmt.Errorf("%w: inconsistent transcript pagination", ErrContract)
		}
		total = *p.Total
		for _, rawSeg := range p.Segments {
			seg, err := decodeSegment(rawSeg)
			if err != nil {
				return nil, err
			}
			out = append(out, seg)
		}
		if p.Next == "" {
			if len(out) != total {
				return nil, fmt.Errorf("%w: transcript ended before total", ErrContract)
			}
			return out, nil
		}
		if len(p.Segments) == 0 || seen[p.Next] || len(out) >= total {
			return nil, fmt.Errorf("%w: stalled transcript pagination", ErrContract)
		}
		seen[p.Next] = true
		cursor = p.Next
	}
}

func emptyTranscriptResult(res *mcp.CallToolResult, block string) bool {
	if res == nil || res.IsError {
		return false
	}
	if raw, err := toolPayload(res); err == nil {
		return strings.TrimSpace(string(raw)) == "[]"
	}
	if len(res.Content) != 1 {
		return false
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return false
	}
	text := strings.TrimSpace(tc.Text)
	return text == fmt.Sprintf("Block %q has no content for this recording yet.", block) || strings.HasPrefix(text, fmt.Sprintf("Block %q not available for this recording. Available blocks:", block))
}
