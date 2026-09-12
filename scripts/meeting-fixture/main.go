// Command meeting-fixture imports synthetic provider responses for real-daemon
// browser acceptance. It is a test-data producer, never part of the user binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/store"
)

type meetingRef struct {
	MessageID int64 `json:"message_id"`
	SourceID  int64 `json:"source_id"`
}
type manifest struct {
	Meetings      map[string]meetingRef `json:"meetings"`
	ParticipantID int64                 `json:"participant_id"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: meeting-fixture DATA_DIR")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	result, err := seed(ctx, os.Args[1])
	cancel()
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(result)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func seed(ctx context.Context, dataDir string) (*manifest, error) {
	// #nosec G703 -- this test-only producer writes beneath its explicit scratch directory.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	if err := st.InitSchema(); err != nil {
		return nil, err
	}
	out := &manifest{Meetings: map[string]meetingRef{}}
	imports := []struct {
		provider, externalID string
		run                  func(context.Context, *store.Store) (int64, error)
	}{
		{"granola", "granola-span", importGranola},
		{"notion", "notion-scheduled", importNotion},
		{"circleback", "meeting:circleback-provider", importCircleback},
	}
	for _, entry := range imports {
		sourceID, err := entry.run(ctx, st)
		if err != nil {
			return nil, fmt.Errorf("import %s: %w", entry.provider, err)
		}
		ids, err := st.MessageExistsBatch(sourceID, []string{entry.externalID})
		if err != nil {
			return nil, err
		}
		id, ok := ids[entry.externalID]
		if !ok {
			return nil, fmt.Errorf("%s importer did not archive its meeting", entry.provider)
		}
		out.Meetings[entry.provider] = meetingRef{MessageID: id, SourceID: sourceID}
	}
	// Use the same Store transition that source deletion observation uses. No
	// archive row or normalized meeting projection is manufactured by this helper.
	if err := st.MarkMessageDeleted(out.Meetings["circleback"].SourceID, "meeting:circleback-provider"); err != nil {
		return nil, err
	}
	recipients, err := st.GetMessageRecipientsContext(ctx, out.Meetings["granola"].MessageID, "to")
	if err != nil {
		return nil, err
	}
	for _, recipient := range recipients {
		if recipient.EmailAddress == "alex@example.com" {
			out.ParticipantID = recipient.ParticipantID
		}
	}
	if out.ParticipantID == 0 {
		return nil, errors.New("importers did not record the synthetic attendee")
	}
	return out, nil
}

// Every unrecognized request fails. The production clients receive only a
// loopback base URL; none of these fixtures can fall through to a provider.
func jsonServer(responses map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, ok := responses[r.Method+" "+r.URL.Path]
		if !ok {
			http.Error(w, "unexpected synthetic provider request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
}

func importGranola(ctx context.Context, st *store.Store) (int64, error) {
	const note = `{"id":"granola-span","title":"Granola transcript span","created_at":"2026-02-03T10:00:00Z","updated_at":"2026-02-03T10:10:00Z","owner":{"name":"Archive Owner","email":"owner@example.org"},"attendees":[{"name":"Alex Example","email":"alex@example.com"}],"summary_markdown":"A synthetic transcript-span decision.","transcript":[{"speaker":{"name":"Alex Example","source":"speaker"},"text":"Granola transcript evidence.","start_time":"2026-02-03T10:00:00Z","end_time":"2026-02-03T10:10:00Z"}]}`
	server := jsonServer(map[string]string{"GET /v1/notes": `{"notes":[` + note + `],"hasMore":false}`, "GET /v1/notes/granola-span": note})
	defer server.Close()
	result, err := granola.NewImporter(st, granola.NewClient(server.URL, "synthetic-token")).Import(ctx, granola.ImportOptions{Identifier: "fixture-granola", AccountEmail: "owner@example.org", Full: true})
	if err != nil {
		return 0, err
	}
	return result.SourceID, nil
}

func importNotion(ctx context.Context, st *store.Store) (int64, error) {
	if _, err := st.GetOrCreateSource(notionmeetings.SourceType, "fixture-notion"); err != nil {
		return 0, err
	}
	const meeting = `{"object":"block","id":"notion-scheduled","type":"meeting_notes","has_children":true,"parent":{"type":"page_id","page_id":"notion-page"},"created_time":"2026-01-03T10:00:00Z","last_edited_time":"2026-01-03T11:00:00Z","meeting_notes":{"title":[{"plain_text":"Notion scheduled hour"}],"status":"notes_ready","children":{"summary_block_id":"notion-summary","notes_block_id":"notion-notes","transcript_block_id":"notion-transcript"},"calendar_event":{"start_time":"2026-01-03T10:00:00Z","end_time":"2026-01-03T11:00:00Z","attendees":["alex-user"]}}}`
	server := jsonServer(map[string]string{
		"POST /v1/blocks/meeting_notes/query":  `{"results":[` + meeting + `],"has_more":false}`,
		"GET /v1/blocks/notion-scheduled":      meeting,
		"GET /v1/blocks/notion-summary":        `{"object":"block","id":"notion-summary","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"A synthetic scheduled decision."}]}}`,
		"GET /v1/blocks/notion-notes":          `{"object":"block","id":"notion-notes","type":"paragraph","has_children":true,"paragraph":{"rich_text":[{"plain_text":"Recorded checkbox evidence."}]}}`,
		"GET /v1/blocks/notion-notes/children": `{"results":[{"object":"block","id":"notion-done","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Publish the Notion recap"}],"checked":true}}],"has_more":false}`,
		"GET /v1/blocks/notion-transcript":     `{"object":"block","id":"notion-transcript","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"Alex Example: Notion transcript evidence."}]}}`,
		"GET /v1/pages/notion-page/markdown":   `{"object":"page_markdown","id":"notion-page","truncated":false,"unknown_block_ids":[],"markdown":"## Transcript\nAlex Example: Notion transcript evidence."}`,
		"GET /v1/users":                        `{"results":[{"object":"user","id":"alex-user","type":"person","name":"Alex Example","person":{"email":"alex@example.com","email_verified":true}}],"has_more":false}`,
	})
	defer server.Close()
	result, err := notionmeetings.NewImporter(st, notionmeetings.NewClient(server.URL, "synthetic-token")).Import(ctx, notionmeetings.ImportOptions{Identifier: "fixture-notion", AccountEmail: "owner@example.org", Full: true})
	if err != nil {
		return 0, err
	}
	return result.SourceID, nil
}

func importCircleback(ctx context.Context, st *store.Store) (int64, error) {
	const meeting = `{"id":"circleback-provider","name":"Circleback provider half hour","createdAt":"2026-01-02T10:00:00Z","startTime":"2026-01-02T10:00:00Z","durationSeconds":1800,"organizer":{"name":"Archive Owner","email":"owner@example.org"},"attendees":[{"name":"Alex Example","email":"alex@example.com"}],"notes":"A synthetic provider-duration decision.","actionItems":[{"title":"Send the Circleback recap","status":"open","assignee":{"name":"Alex Example","email":"alex@example.com"},"dueDate":"2026-01-05"}],"insights":{}}`
	server := mcp.NewServer(&mcp.Implementation{Name: "synthetic-circleback", Version: "1"}, nil)
	for name, payload := range map[string]string{
		"SearchMeetings":            `{"meetings":[` + meeting + `]}`,
		"ReadMeetings":              `{"meetings":[` + meeting + `]}`,
		"GetTranscriptsForMeetings": `{"transcripts":[{"id":"circleback-provider","transcript":[{"speaker":"Alex Example","text":"Circleback transcript evidence.","start":0}]}]}`,
	} {
		mcp.AddTool[map[string]any, any](server, &mcp.Tool{Name: name, Description: "Synthetic archive provider fixture"}, func(_ context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
			if name == "SearchMeetings" && input["pageIndex"] != float64(0) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"meetings":[]}`}}}, nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: payload}}}, nil, nil
		})
	}
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "meeting-fixture", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		return 0, fmt.Errorf("connect synthetic meeting provider: %w", err)
	}
	provider := circleback.NewSessionForTesting(session)
	defer func() { _ = provider.Close() }()
	result, err := circleback.NewImporter(st, provider).Import(ctx, circleback.ImportOptions{Identifier: "fixture-circleback", AccountEmail: "owner@example.org", Full: true})
	if err != nil {
		return 0, err
	}
	return result.SourceID, nil
}
