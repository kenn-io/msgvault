package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

var task5StableToolNames = []string{
	ToolAggregate,
	ToolExportAttachment,
	ToolFindSimilarMessages,
	ToolGetAttachment,
	ToolGetMessage,
	ToolGetStats,
	ToolListMessages,
	ToolSearchByDomains,
	ToolSearchInMessage,
	ToolSearchMessageBodies,
	ToolSearchMessages,
	ToolSearchMetadata,
	ToolSemanticSearchMessages,
	ToolStageDeletion,
}

type task5Fixture struct {
	opts            ServeOptions
	exportDir       string
	attachmentBytes []byte
	saver           *captureDeletionManifestSaver
}

func newTask5Fixture(t *testing.T, shape string) task5Fixture {
	t.Helper()

	now := time.Date(2026, 7, 28, 12, 34, 56, 0, time.UTC)
	attachmentBytes := []byte("deterministic attachment bytes")
	attachment := query.AttachmentInfo{
		ID:          7,
		Filename:    "archive-note.txt",
		MimeType:    "text/plain",
		Size:        int64(len(attachmentBytes)),
		ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		StoragePath: "unused-by-injected-reader",
	}
	message := &query.MessageDetail{
		ID:                   42,
		SourceID:             1,
		SourceMessageID:      "source-message-42",
		ConversationID:       9,
		SourceConversationID: "source-conversation-9",
		Subject:              "Deterministic archive note",
		MessageType:          "email",
		Snippet:              "A nested deterministic result",
		SentAt:               now,
		SizeEstimate:         2048,
		HasAttachments:       true,
		From:                 []query.Address{{Email: "alice@example.com", Name: "Alice Example"}},
		To:                   []query.Address{{Email: "bob@example.com", Name: "Bob Example"}},
		BodyText:             "needle appears in the deterministic archive body\nneedle appears again",
		BodyHTML:             "<p>needle appears in the deterministic archive body</p>",
		Labels:               []string{"inbox", "task5"},
		Attachments:          []query.AttachmentInfo{attachment},
	}
	similarMessage := &query.MessageDetail{
		ID:                   43,
		SourceID:             1,
		SourceMessageID:      "source-message-43",
		ConversationID:       10,
		SourceConversationID: "source-conversation-10",
		Subject:              "A related archive note",
		MessageType:          "email",
		Snippet:              "Related nested result",
		SentAt:               now.Add(-time.Hour),
		SizeEstimate:         1024,
		From:                 []query.Address{{Email: "bob@example.com", Name: "Bob Example"}},
		To:                   []query.Address{{Email: "alice@example.com", Name: "Alice Example"}},
		BodyText:             "related deterministic archive body",
		Labels:               []string{"archive"},
	}
	summary := query.MessageSummary{
		ID:                   message.ID,
		SourceID:             message.SourceID,
		SourceMessageID:      message.SourceMessageID,
		ConversationID:       message.ConversationID,
		SourceConversationID: message.SourceConversationID,
		Subject:              message.Subject,
		Snippet:              message.Snippet,
		FromEmail:            "alice@example.com",
		FromName:             "Alice Example",
		To:                   slices.Clone(message.To),
		SentAt:               message.SentAt,
		SizeEstimate:         message.SizeEstimate,
		HasAttachments:       true,
		AttachmentCount:      1,
		Labels:               slices.Clone(message.Labels),
		MessageType:          message.MessageType,
	}
	bodySummary := summary
	bodySummary.BodyContextSnippets = []string{"needle appears in the deterministic archive body"}

	engine := &querytest.MockEngine{
		SearchFastResults: []query.MessageSummary{summary},
		SearchResults:     []query.MessageSummary{bodySummary},
		ListResults:       []query.MessageSummary{summary},
		Messages: map[int64]*query.MessageDetail{
			message.ID:        message,
			similarMessage.ID: similarMessage,
		},
		Attachments: map[int64]*query.AttachmentInfo{attachment.ID: &attachment},
		Stats: &query.TotalStats{
			MessageCount:       2,
			ActiveMessageCount: 2,
			TotalSize:          3072,
			AttachmentCount:    1,
			AttachmentSize:     int64(len(attachmentBytes)),
			LabelCount:         3,
			AccountCount:       1,
		},
		Accounts: []query.AccountInfo{{
			ID: 1, SourceType: "gmail", Identifier: "alice@example.com", DisplayName: "Archive Account",
		}},
		AggregateRows: []query.AggregateRow{{
			Key: "example.com", Count: 2, TotalSize: 3072, AttachmentSize: int64(len(attachmentBytes)), AttachmentCount: 1, TotalUnique: 1,
		}},
		GmailIDs: []string{"source-message-42"},
		SearchFastCountFunc: func(context.Context, *search.Query, query.MessageFilter) (int64, error) {
			return 1, nil
		},
	}

	saver := &captureDeletionManifestSaver{}
	opts := ServeOptions{
		Engine: engine,
		AttachmentReader: attachmentReaderFunc(func(_ context.Context, contentHash string) ([]byte, error) {
			if contentHash != attachment.ContentHash {
				return nil, fmt.Errorf("attachment content hash = %q, want %q", contentHash, attachment.ContentHash)
			}
			return slices.Clone(attachmentBytes), nil
		}),
		ManifestSaver: saver,
	}

	rrf, vectorScore := 0.45, 0.91
	remoteHybrid := hybridSearcherFunc(func(_ context.Context, _ HybridSearchRequest) (*HybridSearchResult, error) {
		return &HybridSearchResult{
			Hits: []HybridSearchHit{{
				ID: 42, RRFScore: &rrf, VectorScore: &vectorScore, SubjectBoosted: true,
				Matches: []HybridSearchMatch{{Snippet: "needle appears in the deterministic archive body", Score: 0.91}},
			}},
			PoolSaturated: true,
			Generation:    HybridGeneration{ID: 7, Model: "fixture-embed", Dimension: 4, Fingerprint: "fixture-embed:4", State: "active"},
			HasMore:       false,
		}, nil
	})
	remoteSimilar := similarSearcherFunc(func(_ context.Context, req SimilarSearchRequest) (*SimilarSearchResult, error) {
		return &SimilarSearchResult{
			SeedMessageID: req.MessageID,
			Generation:    HybridGeneration{ID: 7, Model: "fixture-embed", Dimension: 4, Fingerprint: "fixture-embed:4", State: "active"},
			Messages: []query.MessageSummary{{
				ID: similarMessage.ID, SourceID: similarMessage.SourceID, SourceMessageID: similarMessage.SourceMessageID,
				ConversationID: similarMessage.ConversationID, SourceConversationID: similarMessage.SourceConversationID,
				Subject: similarMessage.Subject, Snippet: similarMessage.Snippet, FromEmail: "bob@example.com",
				SentAt: similarMessage.SentAt, SizeEstimate: similarMessage.SizeEstimate, Labels: slices.Clone(similarMessage.Labels),
			}},
		}, nil
	})

	switch shape {
	case "000":
	case "100":
		opts.HybridSearcher = remoteHybrid
	case "001":
		opts.SimilarSearcher = remoteSimilar
	case "101":
		opts.HybridSearcher = remoteHybrid
		opts.SimilarSearcher = remoteSimilar
	case "111":
		cfg := testSimilarVectorConfig()
		backend := &fakeBackend{
			loadVec:    []float32{1, 0, 0, 0},
			active:     testSimilarActiveGeneration(cfg),
			searchHits: []vector.Hit{{MessageID: 42, Score: 0.95, Rank: 1}, {MessageID: 43, Score: 0.87, Rank: 2}},
			stats:      map[vector.GenerationID]vector.Stats{7: {EmbeddingCount: 2}},
			chunkHits: map[int64][]vector.ChunkHit{
				42: {{ChunkIndex: 0, ChunkCharStart: 0, ChunkCharEnd: 18, Score: 0.92}},
			},
		}
		opts.VectorCfg = cfg
		opts.Backend = backend
		opts.HybridEngine = hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
			ExpectedFingerprint: cfg.GenerationFingerprint(), RRFK: 60, KPerSignal: 10,
		})
	default:
		require.FailNow(t, "unknown task 5 capability shape", "shape: %s", shape)
	}

	return task5Fixture{
		opts:            opts,
		exportDir:       t.TempDir(),
		attachmentBytes: attachmentBytes,
		saver:           saver,
	}
}

func task5ExpectedTools(shape string, allowWrites bool) []string {
	want := make([]string, 0, len(task5StableToolNames))
	for _, name := range task5StableToolNames {
		if !allowWrites && (name == ToolExportAttachment || name == ToolStageDeletion) {
			continue
		}
		if shape != "001" && shape != "101" && shape != "111" && name == ToolFindSimilarMessages {
			continue
		}
		want = append(want, name)
	}
	return want
}

func task5ToolArguments(name, shape, exportDir string) (map[string]any, bool) {
	switch name {
	case ToolSearchMessages:
		return map[string]any{"query": "needle", "limit": 1}, true
	case ToolSearchMetadata:
		return map[string]any{"query": "subject:archive", "account": "alice@example.com", "limit": 1}, true
	case ToolSearchMessageBodies:
		return map[string]any{"query": "needle", "limit": 1}, true
	case ToolSemanticSearchMessages:
		if shape == "000" || shape == "001" {
			return map[string]any{"query": "needle"}, true
		}
		return map[string]any{"query": "needle", "mode": "vector", "limit": 1, "explain": true}, true
	case ToolGetMessage:
		return map[string]any{"id": 42, "max_chars": 80, "body_format": "text"}, true
	case ToolGetAttachment:
		return map[string]any{"attachment_id": 7}, true
	case ToolExportAttachment:
		return map[string]any{"attachment_id": 7, "destination": exportDir}, true
	case ToolListMessages:
		return map[string]any{"account": "alice@example.com", "limit": 1}, true
	case ToolGetStats:
		return map[string]any{}, true
	case ToolAggregate:
		return map[string]any{"group_by": "domain", "account": "alice@example.com", "limit": 1}, true
	case ToolStageDeletion:
		return map[string]any{"from": "alice@example.com"}, true
	case ToolSearchByDomains:
		return map[string]any{"domains": "example.com", "limit": 1}, true
	case ToolFindSimilarMessages:
		return map[string]any{"message_id": 42, "limit": 1}, true
	case ToolSearchInMessage:
		if shape == "111" {
			return map[string]any{"id": 42, "query": "needle", "mode": "vector", "min_score": 0.5, "limit": 2}, true
		}
		return map[string]any{"id": 42, "query": "needle", "limit": 2}, true
	default:
		return nil, false
	}
}

func task5ConnectClient(t *testing.T, opts ServeOptions, allowWrites bool) *sdkmcp.ClientSession {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(opts, allowWrites).Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, serverSession.Close()) })

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "msgvault-contract-test", Version: "contract-test-version"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, clientSession.Close()) })
	return clientSession
}

func task5AssertJSONParity(t *testing.T, toolName string, result *sdkmcp.CallToolResult) {
	t.Helper()
	require.NotNil(t, result)
	if result.IsError {
		require.FailNow(t, "unexpected tool error", "%s: %s", toolName, task5SDKResultText(result))
	}
	require.NotNil(t, result.StructuredContent)
	require.NotEmpty(t, result.Content)

	text, ok := result.Content[0].(*sdkmcp.TextContent)
	require.True(t, ok, "first content type: %T", result.Content[0])
	require.NotEmpty(t, text.Text)
	var textJSON any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &textJSON))
	structuredBytes, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var structuredJSON any
	require.NoError(t, json.Unmarshal(structuredBytes, &structuredJSON))
	assert.Equal(t, textJSON, structuredJSON)
}

func task5SDKResultText(result *sdkmcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	text, _ := result.Content[0].(*sdkmcp.TextContent)
	if text == nil {
		return ""
	}
	return text.Text
}

func task5StructuredAs[T any](t *testing.T, result *sdkmcp.CallToolResult) T {
	t.Helper()
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var value T
	require.NoError(t, json.Unmarshal(data, &value))
	return value
}

func task5AssertRepresentativeResult(
	t *testing.T,
	toolName string,
	result *sdkmcp.CallToolResult,
	fixture task5Fixture,
) {
	t.Helper()
	checks := assert.New(t)
	must := require.New(t)

	switch toolName {
	case ToolSearchMetadata:
		value := task5StructuredAs[struct {
			Data []struct {
				ID      int64  `json:"id"`
				Subject string `json:"subject"`
				To      []struct {
					Email string `json:"Email"`
					Name  string `json:"Name"`
				} `json:"to"`
			} `json:"data"`
			Total int64 `json:"total"`
		}](t, result)
		must.Len(value.Data, 1)
		checks.Equal(int64(42), value.Data[0].ID)
		checks.Equal("Deterministic archive note", value.Data[0].Subject)
		must.Len(value.Data[0].To, 1)
		checks.Equal("bob@example.com", value.Data[0].To[0].Email)
		checks.Equal("Bob Example", value.Data[0].To[0].Name)
		checks.Equal(int64(1), value.Total)
	case ToolSearchMessageBodies:
		value := task5StructuredAs[struct {
			Mode string `json:"mode"`
			Data []struct {
				ID      int64 `json:"id"`
				Matches []struct {
					Snippet string `json:"snippet"`
				} `json:"matches"`
			} `json:"data"`
		}](t, result)
		checks.Equal("keyword", value.Mode)
		must.Len(value.Data, 1)
		checks.Equal(int64(42), value.Data[0].ID)
		must.Len(value.Data[0].Matches, 1)
		checks.Equal("needle appears in the deterministic archive body", value.Data[0].Matches[0].Snippet)
	case ToolSemanticSearchMessages:
		value := task5StructuredAs[struct {
			Mode       string `json:"mode"`
			Generation struct {
				ID          int64  `json:"id"`
				Model       string `json:"model"`
				Fingerprint string `json:"fingerprint"`
			} `json:"generation"`
			Data []struct {
				ID    int64 `json:"id"`
				Score struct {
					Vector *float64 `json:"vector"`
				} `json:"score"`
				Matches []struct {
					Snippet string   `json:"snippet"`
					Score   *float64 `json:"score"`
				} `json:"matches"`
			} `json:"data"`
		}](t, result)
		checks.Equal("vector", value.Mode)
		checks.Equal(int64(7), value.Generation.ID)
		checks.Equal("nomic-embed", value.Generation.Model)
		checks.NotEmpty(value.Generation.Fingerprint)
		must.Len(value.Data, 1)
		checks.Equal(int64(42), value.Data[0].ID)
		must.NotNil(value.Data[0].Score.Vector)
		checks.InDelta(0.95, *value.Data[0].Score.Vector, 0.0001)
		must.Len(value.Data[0].Matches, 1)
		checks.NotEmpty(value.Data[0].Matches[0].Snippet)
		must.NotNil(value.Data[0].Matches[0].Score)
		checks.InDelta(0.92, *value.Data[0].Matches[0].Score, 0.0001)
	case ToolGetMessage:
		value := task5StructuredAs[struct {
			ID              int64  `json:"id"`
			SourceMessageID string `json:"source_message_id"`
			Subject         string `json:"subject"`
			BodyText        string `json:"body_text"`
			From            []struct {
				Email string `json:"Email"`
				Name  string `json:"Name"`
			} `json:"from"`
			Attachments []struct {
				ID       int64  `json:"ID"`
				Filename string `json:"Filename"`
				MimeType string `json:"MimeType"`
			} `json:"attachments"`
		}](t, result)
		checks.Equal(int64(42), value.ID)
		checks.Equal("source-message-42", value.SourceMessageID)
		checks.Equal("Deterministic archive note", value.Subject)
		checks.Contains(value.BodyText, "needle appears")
		must.Len(value.From, 1)
		checks.Equal("alice@example.com", value.From[0].Email)
		checks.Equal("Alice Example", value.From[0].Name)
		must.Len(value.Attachments, 1)
		checks.Equal(int64(7), value.Attachments[0].ID)
		checks.Equal("archive-note.txt", value.Attachments[0].Filename)
		checks.Equal("text/plain", value.Attachments[0].MimeType)
	case ToolGetAttachment:
		value := task5StructuredAs[struct {
			Filename string `json:"filename"`
			MIMEType string `json:"mime_type"`
			Size     int64  `json:"size"`
		}](t, result)
		checks.Equal("archive-note.txt", value.Filename)
		checks.Equal("text/plain", value.MIMEType)
		checks.Equal(int64(len(fixture.attachmentBytes)), value.Size)
		must.Len(result.Content, 2)
		resource, ok := result.Content[1].(*sdkmcp.EmbeddedResource)
		must.True(ok, "attachment content type: %T", result.Content[1])
		must.NotNil(resource.Resource)
		checks.Equal("msgvault://attachment/7", resource.Resource.URI)
		checks.Equal("text/plain", resource.Resource.MIMEType)
		checks.Equal(fixture.attachmentBytes, resource.Resource.Blob)
	case ToolExportAttachment:
		value := task5StructuredAs[struct {
			Path     string `json:"path"`
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
		}](t, result)
		wantPath := filepath.Join(fixture.exportDir, "archive-note.txt")
		checks.Equal(wantPath, value.Path)
		checks.Equal("archive-note.txt", value.Filename)
		checks.Equal(int64(len(fixture.attachmentBytes)), value.Size)
		exported, err := os.ReadFile(wantPath)
		must.NoError(err)
		checks.Equal(fixture.attachmentBytes, exported)
	case ToolGetStats:
		value := task5StructuredAs[struct {
			Stats struct {
				MessageCount    int64 `json:"MessageCount"`
				TotalSize       int64 `json:"TotalSize"`
				AttachmentCount int64 `json:"AttachmentCount"`
			} `json:"stats"`
			Accounts []struct {
				ID         int64  `json:"ID"`
				Identifier string `json:"Identifier"`
			} `json:"accounts"`
		}](t, result)
		checks.Equal(int64(2), value.Stats.MessageCount)
		checks.Equal(int64(3072), value.Stats.TotalSize)
		checks.Equal(int64(1), value.Stats.AttachmentCount)
		must.Len(value.Accounts, 1)
		checks.Equal(int64(1), value.Accounts[0].ID)
		checks.Equal("alice@example.com", value.Accounts[0].Identifier)
	case ToolAggregate:
		value := task5StructuredAs[struct {
			Data []struct {
				Key             string
				Count           int64
				TotalSize       int64
				AttachmentSize  int64
				AttachmentCount int64
				TotalUnique     int64
			} `json:"data"`
		}](t, result)
		must.Len(value.Data, 1)
		checks.Equal("example.com", value.Data[0].Key)
		checks.Equal(int64(2), value.Data[0].Count)
		checks.Equal(int64(3072), value.Data[0].TotalSize)
		checks.Equal(int64(len(fixture.attachmentBytes)), value.Data[0].AttachmentSize)
		checks.Equal(int64(1), value.Data[0].AttachmentCount)
		checks.Equal(int64(1), value.Data[0].TotalUnique)
	case ToolFindSimilarMessages:
		value := task5StructuredAs[struct {
			SeedMessageID int64 `json:"seed_message_id"`
			Returned      int   `json:"returned"`
			Generation    struct {
				ID    int64  `json:"id"`
				Model string `json:"model"`
			} `json:"generation"`
			Messages []struct {
				ID      int64  `json:"id"`
				Subject string `json:"subject"`
			} `json:"messages"`
		}](t, result)
		checks.Equal(int64(42), value.SeedMessageID)
		checks.Equal(1, value.Returned)
		checks.Equal(int64(7), value.Generation.ID)
		checks.Equal("nomic-embed", value.Generation.Model)
		must.Len(value.Messages, 1)
		checks.Equal(int64(43), value.Messages[0].ID)
		checks.Equal("A related archive note", value.Messages[0].Subject)
	case ToolSearchInMessage:
		value := task5StructuredAs[struct {
			Data []struct {
				Snippet string   `json:"snippet"`
				Score   *float64 `json:"score"`
			} `json:"data"`
			Total int64 `json:"total"`
		}](t, result)
		must.Len(value.Data, 1)
		checks.NotEmpty(value.Data[0].Snippet)
		must.NotNil(value.Data[0].Score)
		checks.InDelta(0.92, *value.Data[0].Score, 0.0001)
		checks.Equal(int64(1), value.Total)
	case ToolStageDeletion:
		value := task5StructuredAs[struct {
			MessageCount int    `json:"message_count"`
			Status       string `json:"status"`
		}](t, result)
		checks.Equal(1, value.MessageCount)
		checks.Equal("pending", value.Status)
		must.Len(fixture.saver.manifests, 1)
		manifest := fixture.saver.manifests[0]
		checks.Equal([]string{"source-message-42"}, manifest.GmailIDs)
		checks.Equal([]string{"alice@example.com"}, manifest.Filters.Senders)
		checks.Equal("mcp", manifest.CreatedBy)
	}
}

func TestAllAdvertisedToolOutputs(t *testing.T) {
	checks := assert.New(t)
	shapes := []string{"000", "100", "001", "101", "111"}
	advertised := make(map[string]bool, len(task5StableToolNames))
	successful := make(map[string]bool, len(task5StableToolNames))

	for _, shape := range shapes {
		for _, allowWrites := range []bool{false, true} {
			policyName := "read_only"
			if allowWrites {
				policyName = "writes"
			}
			t.Run(shape+"/"+policyName, func(t *testing.T) {
				checks := assert.New(t)
				must := require.New(t)
				fixture := newTask5Fixture(t, shape)
				client := task5ConnectClient(t, fixture.opts, allowWrites)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()

				listed, err := client.ListTools(ctx, nil)
				must.NoError(err)
				actualNames := make([]string, 0, len(listed.Tools))
				for _, tool := range listed.Tools {
					actualNames = append(actualNames, tool.Name)
				}
				checks.Equal(task5ExpectedTools(shape, allowWrites), actualNames)

				for _, tool := range listed.Tools {
					advertised[tool.Name] = true
					arguments, ok := task5ToolArguments(tool.Name, shape, fixture.exportDir)
					must.True(ok, "advertised tool %q lacks a literal argument fixture", tool.Name)
					result, err := client.CallTool(ctx, &sdkmcp.CallToolParams{Name: tool.Name, Arguments: arguments})
					must.NoError(err, "call %s", tool.Name)
					if tool.Name == ToolSemanticSearchMessages && (shape == "000" || shape == "001") {
						must.True(result.IsError)
						must.NotEmpty(result.Content)
						text, ok := result.Content[0].(*sdkmcp.TextContent)
						must.True(ok, "semantic unavailable content type: %T", result.Content[0])
						checks.Contains(text.Text, "vector_not_enabled")
						continue
					}
					task5AssertJSONParity(t, tool.Name, result)
					if shape == "111" && allowWrites {
						task5AssertRepresentativeResult(t, tool.Name, result, fixture)
					}
					successful[tool.Name] = true
				}

				if allowWrites {
					must.Len(fixture.saver.manifests, 1, "stage_deletion saver calls")
					checks.Equal("mcp", fixture.saver.manifests[0].CreatedBy)
				} else {
					checks.Empty(fixture.saver.manifests, "read-only catalog must not invoke manifest saver")
				}
			})
		}
	}

	checks.Equal(task5StableToolNames, sortedTrueKeys(advertised), "tools advertised across matrix")
	checks.Equal(task5StableToolNames, sortedTrueKeys(successful), "tools with at least one SDK-validated success")
}

func sortedTrueKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if value {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}
