package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

type metaSnippetFixtureAPI struct{ body string }

func (f metaSnippetFixtureAPI) VerifyPage(_ context.Context, expectedPage string) (meta.Page, error) {
	return meta.Page{ID: expectedPage, Name: "Example Page"}, nil
}

func (metaSnippetFixtureAPI) LinkedInstagramAccount(context.Context, string, string) (meta.IGUser, error) {
	return meta.IGUser{}, nil
}

func (metaSnippetFixtureAPI) Conversations(context.Context, string, string, string) (meta.ConversationPage, error) {
	return meta.ConversationPage{Data: []meta.Conversation{{
		ID: "thread_1", UpdatedTime: "2026-11-01T00:00:00+0000",
		Participants: []meta.DMParticipant{{ID: "300", Name: "Example Customer"}},
	}}}, nil
}

func (metaSnippetFixtureAPI) ConversationMessageIDs(context.Context, string) ([]string, bool, error) {
	return []string{"message_1"}, false, nil
}

func (f metaSnippetFixtureAPI) Message(_ context.Context, id string) (meta.DMMessage, error) {
	raw, err := json.Marshal(map[string]string{"id": id, "message": f.body})
	if err != nil {
		return meta.DMMessage{}, err
	}
	return meta.DMMessage{
		ID: id, CreatedTime: "2026-11-01T00:00:00+0000",
		From:    meta.DMParticipant{ID: "300", Name: "Example Customer"},
		Message: f.body, MessagePresent: true, Raw: raw,
	}, nil
}

func TestAgentSearchReadOnlyGetsBoundedMetaInboxPreview(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	body := strings.Repeat("synthetic private message ", 30) + "PRIVATE-TAIL-OUTSIDE-PREVIEW"
	source := meta.DMSource{AppID: "9", PageID: "100", Platform: meta.PlatformMessenger}
	importer := meta.NewDMImporter(st, metaSnippetFixtureAPI{body: body})
	registered, err := importer.Register(t.Context(), source)
	requirements.NoError(err)
	_, err = importer.Import(t.Context(), source, meta.ImportOptions{Full: true})
	requirements.NoError(err)

	cfg := &config.Config{
		HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()},
		Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger(),
	})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	_, token, _, err := srv.agentGrants.Issue("search-only", []agentgrant.Permission{
		agentgrant.PermissionSearchRead,
	}, []agentgrant.SourceRef{{ID: registered.ID, Type: registered.SourceType, Identifier: registered.Identifier}}, time.Time{})
	requirements.NoError(err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var list MessageListResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &list))
	requirements.Len(list.Messages, 1)
	snippet := list.Messages[0].Snippet
	assertions.LessOrEqual(len([]rune(snippet)), 500)
	assertions.Equal(string([]rune(body)[:len([]rune(snippet))]), snippet)
	assertions.NotContains(snippet, "PRIVATE-TAIL-OUTSIDE-PREVIEW")
	assertions.NotContains(response.Body.String(), "PRIVATE-TAIL-OUTSIDE-PREVIEW")

	storedRows, err := st.StoredMessagesContext(t.Context(), registered.ID, "meta-dm-json", []string{"message_1"})
	requirements.NoError(err)
	stored, found := storedRows["message_1"]
	requirements.True(found)
	fullBody, err := st.GetMessageBodyText(stored.ID)
	requirements.NoError(err)
	assertions.Equal(body, fullBody, "the full message stays in body storage")
}

var _ meta.DMAPI = metaSnippetFixtureAPI{}
