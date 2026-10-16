package archive_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/archive"
)

type scopedSourceFixture struct {
	source       *store.Source
	conversation int64
	messages     []int64
	attachment   int64
}

func newSourceScopeFixture(t *testing.T) (*store.Store, archive.QueryEngine, http.Handler, []scopedSourceFixture) {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	fixtures := make([]scopedSourceFixture, 2)
	for i, identifier := range []string{"reader-a@example.com", "reader-b@example.com"} {
		source, err := st.GetOrCreateSource("gmail", identifier)
		requirements.NoError(err)
		conversation, err := st.EnsureConversation(source.ID, "thread", "Telescope")
		requirements.NoError(err)
		fixtures[i] = scopedSourceFixture{source: source, conversation: conversation}
		for j := range 2 {
			id, err := st.UpsertMessage(&store.Message{
				SourceID: source.ID, ConversationID: conversation,
				SourceMessageID: fmt.Sprintf("message-%d", j), MessageType: "email",
				Subject: sql.NullString{String: fmt.Sprintf("telescope source-%d message-%d", i, j), Valid: true},
				SentAt:  sql.NullTime{Time: time.Date(2026, 1, 1+i, j, 0, 0, 0, time.UTC), Valid: true},
			})
			requirements.NoError(err)
			requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: identifier + " telescope body", Valid: true}, sql.NullString{}))
			requirements.NoError(st.UpsertFTS(id, "telescope", "telescope body", "", "", ""))
			fixtures[i].messages = append(fixtures[i].messages, id)
		}
		requirements.NoError(st.UpsertAttachment(fixtures[i].messages[0], identifier+".txt", "text/plain", "", "", 12))
		requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM attachments WHERE message_id=?"), fixtures[i].messages[0]).Scan(&fixtures[i].attachment))
	}
	_, err := st.CreateCollection("both", "", []int64{fixtures[0].source.ID, fixtures[1].source.ID})
	requirements.NoError(err)
	_, err = st.CreateCollection("other", "", []int64{fixtures[1].source.ID})
	requirements.NoError(err)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	srv := archive.NewServer(archive.ServerOptions{Store: st, Engine: engine})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	return st, engine, srv.Handler(nil), fixtures
}

func sourceScopeRequest(ctx context.Context, t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestLibrarySourceScopeReads(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	_, _, handler, sources := newSourceScopeFixture(t)
	allowed, other := sources[0], sources[1]
	ctx := archive.WithSourceScope(t.Context(), []int64{allowed.source.ID})
	for _, tc := range []struct {
		path  string
		total int64
	}{
		{"/api/v1/messages?page_size=1", 2},
		{"/api/v1/search?q=telescope&page_size=1", 2},
		{"/api/v1/search?q=telescope%20%7C%20missing&page_size=1", 2},
		{"/api/v1/search?q=missing%20%7C%20telescope&collection=both", 2},
		{"/api/v1/search?q=telescope&collection=both", 2},
		{"/api/v1/search?q=telescope&account=reader-a@example.com", 2},
	} {
		t.Run(tc.path, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			w := sourceScopeRequest(ctx, t, handler, tc.path)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			var result struct {
				Total    int64 `json:"total"`
				Messages []struct {
					ID int64 `json:"id"`
				} `json:"messages"`
			}
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &result))
			assertions.Equal(tc.total, result.Total)
			for _, message := range result.Messages {
				assertions.Contains(allowed.messages, message.ID)
			}
			if tc.total != 0 {
				assertions.NotEmpty(result.Messages)
			}
		})
	}
	for _, path := range []string{
		fmt.Sprintf("/api/v1/messages/%d", allowed.messages[0]),
		fmt.Sprintf("/api/v1/conversations/%d?anchor=%d", allowed.conversation, allowed.messages[0]),
		fmt.Sprintf("/api/v1/attachments/%d", allowed.attachment),
	} {
		w := sourceScopeRequest(ctx, t, handler, path)
		assertions.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.NotContains(w.Body.String(), other.source.Identifier)
	}
	for _, pair := range [][2]string{
		{fmt.Sprintf("/api/v1/messages/%d", other.messages[0]), "/api/v1/messages/999999"},
		{fmt.Sprintf("/api/v1/conversations/%d?anchor=%d", other.conversation, other.messages[0]), "/api/v1/conversations/999999?anchor=999999"},
		{fmt.Sprintf("/api/v1/attachments/%d", other.attachment), "/api/v1/attachments/999999"},
	} {
		denied := sourceScopeRequest(ctx, t, handler, pair[0])
		missing := sourceScopeRequest(ctx, t, handler, pair[1])
		assertions.Equal(missing.Body.String(), denied.Body.String())
	}
	for _, path := range []string{
		fmt.Sprintf("/api/v1/messages/%d", other.messages[0]),
		fmt.Sprintf("/api/v1/conversations/%d?anchor=%d", other.conversation, other.messages[0]),
		fmt.Sprintf("/api/v1/conversations/%d?anchor=%d&start=2027-01-01T00:00:00Z", other.conversation, other.messages[0]),
		fmt.Sprintf("/api/v1/attachments/%d", other.attachment),
	} {
		w := sourceScopeRequest(ctx, t, handler, path)
		assertions.Equal(http.StatusNotFound, w.Code, w.Body.String())
		assertions.NotContains(w.Body.String(), other.source.Identifier)
	}
	// Prime owner caches, then request the same catalog under authorization.
	w := sourceScopeRequest(t.Context(), t, handler, "/api/v1/cli/accounts")
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	w = sourceScopeRequest(ctx, t, handler, "/api/v1/cli/accounts")
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var accounts struct {
		Accounts []struct {
			ID              int64 `json:"id"`
			MessageCount    int64 `json:"message_count"`
			VirtualAccounts []struct {
				SourceID int64 `json:"source_id"`
			} `json:"virtual_accounts"`
		} `json:"accounts"`
	}
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &accounts))
	requirements.Len(accounts.Accounts, 1)
	assertions.Equal(allowed.source.ID, accounts.Accounts[0].ID)
	assertions.Equal(int64(2), accounts.Accounts[0].MessageCount)
	requirements.NotEmpty(accounts.Accounts[0].VirtualAccounts)
	for _, account := range accounts.Accounts[0].VirtualAccounts {
		assertions.Equal(allowed.source.ID, account.SourceID)
	}
}

func TestLibrarySourceScopeAccountResolution(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, _, handler, sources := newSourceScopeFixture(t)
	for i, source := range sources {
		requirements.NoError(st.UpdateSourceDisplayName(source.source.ID, "Work"))
		_, err := st.UpdateSourceSettingsContext(t.Context(), source.source.ID,
			store.SourceSettingsUpdate{Alias: new(fmt.Sprintf("account-%d", i))})
		requirements.NoError(err)
	}
	ctx := archive.WithSourceScope(t.Context(), []int64{sources[0].source.ID})
	for _, account := range []string{"Work", "account-0", "ACCOUNT-0"} {
		w := sourceScopeRequest(ctx, t, handler, "/api/v1/search?q=telescope&account="+account)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), `"total":2`)
		assertions.NotContains(w.Body.String(), sources[1].source.Identifier)
	}
	for _, ids := range [][]int64{{sources[0].source.ID}, nil} {
		ctx := archive.WithSourceScope(t.Context(), ids)
		for _, account := range []string{"reader-b@example.com", "missing@example.com", "account-1", "ACCOUNT-1"} {
			w := sourceScopeRequest(ctx, t, handler, "/api/v1/search?q=telescope&account="+account)
			assertions.Equal(http.StatusBadRequest, w.Code, w.Body.String())
			assertions.Contains(w.Body.String(), `"error":"invalid_scope"`)
		}
	}
	w := sourceScopeRequest(archive.WithSourceScope(t.Context(), nil), t, handler, "/api/v1/search?q=telescope&account=Work")
	assertions.Equal(http.StatusBadRequest, w.Code, w.Body.String())
	for _, source := range sources {
		assertions.NotContains(w.Body.String(), source.source.Identifier)
	}
}

func TestLibrarySourceScopeEmptyAndUnclassified(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	_, _, handler, sources := newSourceScopeFixture(t)
	for _, ids := range [][]int64{nil, {}} {
		ctx := archive.WithSourceScope(t.Context(), ids)
		for _, path := range []string{"/api/v1/messages", "/api/v1/search?q=telescope", "/api/v1/search?q=missing%20%7C%20telescope"} {
			w := sourceScopeRequest(ctx, t, handler, path)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			var result struct {
				Total    int64 `json:"total"`
				Messages []any `json:"messages"`
			}
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &result))
			assertions.Zero(result.Total)
			assertions.Empty(result.Messages)
		}
		w := sourceScopeRequest(ctx, t, handler, "/api/v1/search?q=telescope&collection=both")
		assertions.Equal(http.StatusBadRequest, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), `"error":"invalid_scope"`)
		w = sourceScopeRequest(ctx, t, handler, "/api/v1/cli/accounts")
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), `"accounts":[]`)
		w = sourceScopeRequest(ctx, t, handler, fmt.Sprintf("/api/v1/messages/%d", sources[0].messages[0]))
		assertions.Equal(http.StatusNotFound, w.Code, w.Body.String())
	}
	ctx := archive.WithSourceScope(t.Context(), []int64{sources[0].source.ID})
	for _, path := range []string{"/api/v1/stats", "/api/v1/search?q=telescope&mode=vector", "/api/v1/search?q=telescope&mode=hybrid", "/api/v1/attachments/abcdef/content"} {
		w := sourceScopeRequest(ctx, t, handler, path)
		assertions.Equal(http.StatusForbidden, w.Code, w.Body.String())
	}
	w := sourceScopeRequest(t.Context(), t, handler, "/api/v1/messages")
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), `"total":4`)
}

func TestLibrarySourceScopeImmutableConcurrent(t *testing.T) {
	_, _, handler, sources := newSourceScopeFixture(t)
	ids := []int64{sources[0].source.ID}
	ctx := archive.WithSourceScope(t.Context(), ids)
	ids[0] = sources[1].source.ID
	for i := range 10 {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			assertions := assert.New(t)
			requirements := require.New(t)
			index := i % 2
			requestContext := ctx
			if index == 1 {
				requestContext = archive.WithSourceScope(t.Context(), []int64{sources[1].source.ID})
			}
			w := sourceScopeRequest(requestContext, t, handler, "/api/v1/search?q=telescope")
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			assertions.Contains(w.Body.String(), fmt.Sprintf("source-%d", index))
			assertions.NotContains(w.Body.String(), fmt.Sprintf("source-%d", 1-index))
		})
	}
}

func TestLibrarySourceScopeFiltersNativeObjectReads(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, engine, _, sources := newSourceScopeFixture(t)
	ctx := archive.WithSourceScope(t.Context(), []int64{sources[0].source.ID})
	other := sources[1]
	messages, total, err := st.SearchMessagesQueryContext(ctx, &search.Query{
		TextTerms: []string{"telescope"}, AccountIDs: []int64{other.source.ID},
	}, 0, 20)
	requirements.NoError(err)
	assertions.Empty(messages)
	assertions.Zero(total)
	message, err := st.GetMessageContext(ctx, other.messages[0])
	requirements.ErrorIs(err, store.ErrMessageNotFound)
	assertions.Nil(message)
	detail, err := engine.GetMessage(ctx, other.messages[0])
	requirements.NoError(err)
	assertions.Nil(detail)
	attachment, err := engine.GetAttachment(ctx, other.attachment)
	requirements.NoError(err)
	assertions.Nil(attachment)
	exists, err := st.ConversationExistsContext(ctx, other.conversation)
	requirements.NoError(err)
	assertions.False(exists)
	window, err := st.GetConversationWindowContext(ctx, other.conversation, other.messages[0], 2, 2, nil, nil)
	requirements.NoError(err)
	assertions.Empty(window.Messages)
	assertions.Zero(window.Total)
}

type sourceScopeCustomStore struct{ *store.Store }

func TestLibrarySourceScopeRequiresNativeStore(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := archive.NewServer(archive.ServerOptions{Store: &sourceScopeCustomStore{st}})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	w := sourceScopeRequest(archive.WithSourceScope(t.Context(), []int64{1}), t, srv.Handler(nil), "/api/v1/messages")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
}

type sourceScopeBlockedResponseWriter struct {
	*httptest.ResponseRecorder

	beforeWrite func()
}

func (w *sourceScopeBlockedResponseWriter) Write(body []byte) (int, error) {
	w.beforeWrite()
	n, err := w.ResponseRecorder.Write(body)
	if err != nil {
		return n, fmt.Errorf("record scoped response: %w", err)
	}
	return n, nil
}

func TestLibrarySourceScopeReleasesSnapshotBeforeResponse(t *testing.T) {
	for _, endpoint := range []string{"message", "conversation"} {
		t.Run(endpoint, func(t *testing.T) {
			requirements := require.New(t)
			st, _, handler, sources := newSourceScopeFixture(t)
			allowed := sources[0]
			path := fmt.Sprintf("/api/v1/messages/%d", allowed.messages[0])
			if endpoint == "conversation" {
				path = fmt.Sprintf("/api/v1/conversations/%d?anchor=%d", allowed.conversation, allowed.messages[0])
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			ctx = archive.WithSourceScope(ctx, []int64{allowed.source.ID})
			writing, resume := make(chan struct{}, 4), make(chan struct{})
			var requests sync.WaitGroup
			finish := sync.OnceFunc(func() {
				close(resume)
				requests.Wait()
			})
			t.Cleanup(finish)
			responses := make([]*httptest.ResponseRecorder, 4)
			for i := range responses {
				responses[i] = httptest.NewRecorder()
				writer := &sourceScopeBlockedResponseWriter{
					ResponseRecorder: responses[i],
					beforeWrite: sync.OnceFunc(func() {
						writing <- struct{}{}
						<-resume
					}),
				}
				requests.Go(func() {
					handler.ServeHTTP(writer, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
				})
			}
			for range responses {
				select {
				case <-writing:
				case <-ctx.Done():
					requirements.FailNow("responses did not reach the write boundary")
				}
			}
			// Writes are held after the real queries, as with slow response consumers.
			requirements.Zero(st.DB().Stats().InUse, "response writes must not retain archive connections")
			_, err := st.GetOrCreateSourceContext(ctx, "test", "independent@example.com")
			requirements.NoError(err, "archive writes must proceed before responses finish")
			finish()
			for _, response := range responses {
				requirements.Equal(http.StatusOK, response.Code, response.Body.String())
				assert.NotEmpty(t, response.Body.Bytes())
			}
		})
	}
}

func TestLibrarySourceScopeHidesUnauthorizedCollections(t *testing.T) {
	assertions := assert.New(t)
	_, _, handler, sources := newSourceScopeFixture(t)
	ctx := archive.WithSourceScope(t.Context(), []int64{sources[0].source.ID})
	// "other" holds only the unauthorized source, so it must read as missing.
	for _, filter := range []string{"collection", "account"} {
		denied := sourceScopeRequest(ctx, t, handler, "/api/v1/search?q=telescope&"+filter+"=other")
		missing := sourceScopeRequest(ctx, t, handler, "/api/v1/search?q=telescope&"+filter+"=nosuch")
		assertions.Equal(http.StatusBadRequest, denied.Code, denied.Body.String())
		assertions.Equal(strings.ReplaceAll(missing.Body.String(), "nosuch", "other"), denied.Body.String())
	}
	w := sourceScopeRequest(t.Context(), t, handler, "/api/v1/search?q=telescope&collection=other")
	assertions.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), `"total":2`)
}

func TestLibrarySourceScopeHidesUnauthorizedMergeTarget(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	retired, err := st.GetOrCreateSource("gmail", "retired@example.com")
	requirements.NoError(err)
	active, err := st.GetOrCreateSource("gmail", "active@example.com")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: retired.ID, IntoSourceID: active.ID})
	requirements.NoError(err)
	srv := archive.NewServer(archive.ServerOptions{Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL())})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	handler := srv.Handler(nil)
	for _, tc := range []struct {
		name       string
		ids        []int64
		wantTarget int64
	}{
		{"target unauthorized", []int64{retired.ID}, 0},
		{"target authorized", []int64{retired.ID, active.ID}, active.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			w := sourceScopeRequest(archive.WithSourceScope(t.Context(), tc.ids), t, handler, "/api/v1/cli/accounts")
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			type account struct {
				ID                 int64 `json:"id"`
				MergedIntoSourceID int64 `json:"merged_into_source_id"`
			}
			var response struct {
				Accounts []account `json:"accounts"`
			}
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
			merged := slices.IndexFunc(response.Accounts, func(a account) bool { return a.ID == retired.ID })
			requirements.GreaterOrEqual(merged, 0, w.Body.String())
			assert.Equal(t, tc.wantTarget, response.Accounts[merged].MergedIntoSourceID)
		})
	}
}
