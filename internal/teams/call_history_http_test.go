package teams_test

import (
	"encoding/json/v2"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTeamsCallHistoryHTTPRoundtrip(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	archive := testutil.NewTestStore(t)
	peer, err := archive.EnsureParticipantByIdentifier("teams", "peer", "Example Peer")
	requirements.NoError(err)
	person, _, err := archive.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	engine := query.NewEngine(archive.DB(), archive.IsPostgreSQL())
	defer func() { requirements.NoError(engine.Close()) }()
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-teams-api-key"}}, Store: archive, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
	do := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Api-Key", "synthetic-teams-api-key")
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		server.Router().ServeHTTP(out, req)
		requirements.Equal(status, out.Code, out.Body.String())
		return out
	}
	// Build the envelope without reserializing the record: original whitespace
	// and escapes must reach raw storage intact.
	raw := ` { "id":"end", "chatId":"chat", "messageType":"systemEventMessage", "createdDateTime":"2026-10-01T10:00:00Z", "extra":"\u0061", "eventDetail":{"@odata.type":"#microsoft.graph.callEndedEventMessageDetail","callId":"call","callEventType":"meeting","callDuration":"PT60S","callParticipants":[{"participant":{"user":{"id":"peer"}}}]}} `
	source, err := archive.GetOrCreateSource("teams", "example")
	requirements.NoError(err)
	convID, err := archive.EnsureConversation(source.ID, "chat", "Example call")
	requirements.NoError(err)
	messageID, err := archive.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: "teams-event", MessageType: "teams"})
	requirements.NoError(err)
	requirements.NoError(archive.UpsertMessageRawWithFormat(messageID, []byte(raw), "teams_json"))
	exported := do("GET", "/api/v1/cli/message/raw?id="+strconv.FormatInt(messageID, 10), "", 200)
	assertions.Equal(raw, exported.Body.String())
	raw = exported.Body.String()
	binding, err := json.Marshal(map[string]int64{"peer": peer})
	requirements.NoError(err)
	envelope := `{"format":"teams","account":"example","record":` + raw + `,"bindings":` + string(binding) + `}`
	var imported api.CallImportResponse
	for range 2 {
		out := do("POST", "/api/v1/import/calls", envelope, 200)
		requirements.NoError(json.Unmarshal(out.Body.Bytes(), &imported))
	}
	stored, err := archive.GetMessageRaw(imported.MessageID)
	requirements.NoError(err)
	assertions.Equal(strings.TrimSpace(raw), string(stored))
	started := strings.ReplaceAll(strings.ReplaceAll(raw, `"id":"end"`, `"id":"start"`), "callEndedEventMessageDetail", "callStartedEventMessageDetail")
	do("POST", "/api/v1/import/calls", `{"format":"teams","account":"example","record":`+started+`}`, 200)
	var summary api.CallSummaryResponse
	out := do("GET", "/api/v1/calls/summary", "", 200)
	requirements.NoError(json.Unmarshal(out.Body.Bytes(), &summary))
	requirements.Len(summary.Items, 1)
	assertions.Equal(person.ID, summary.Items[0].PersonID)
	assertions.Equal(0, summary.Items[0].Answered)
	assertions.Equal(1, summary.Items[0].Unknown)
	// Replacement removes old bindings instead of accumulating them.
	do("POST", "/api/v1/import/calls", `{"format":"teams","account":"example","record":`+raw+`}`, 200)
	assertions.JSONEq(`{"items":[]}`, do("GET", "/api/v1/calls/summary", "", 200).Body.String())
	do("POST", "/api/v1/import/calls", envelope, 200)
	withdrawal := strings.Replace(envelope, `"id":"end"`, `"id":"end","deletedDateTime":"2026-10-02T10:00:00Z"`, 1)
	do("POST", "/api/v1/import/calls", withdrawal, 200)
	do("POST", "/api/v1/import/calls", envelope, 409)
	assertions.JSONEq(`{"items":[]}`, do("GET", "/api/v1/calls/summary", "", 200).Body.String())
	var evidence api.CallEvidenceResponse
	out = do("GET", "/api/v1/calls", "", 200)
	requirements.NoError(json.Unmarshal(out.Body.Bytes(), &evidence))
	assertions.Len(evidence.Items, 2, "start/end retain independent observations")
}
