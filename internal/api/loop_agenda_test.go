package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/katatest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopAgendaFilesReplyAndReplays(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	id := f.PersistEmail(storetest.EmailSpec{Thread: "thread", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Subject: "Contract", Body: "Can you send the contract?", SentAt: now.Add(-48 * time.Hour)})
	participant := f.EnsureParticipant("alex@example.test", "", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project, AutoAgenda: new(false)}, f.Store, "http://localhost:8080")
	type filing struct {
		result kataissues.Result
		err    error
	}
	results := make(chan filing, 2)
	for range 2 {
		go func() {
			result, err := agenda.File(t.Context(), strconv.FormatInt(id, 10), now)
			results <- filing{result, err}
		}()
	}
	a, b := <-results, <-results
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	assert.Equal(t, a.result.Issue.UID, b.result.Issue.UID)
	assert.NotEqual(t, a.result.Replayed, b.result.Replayed)
	first := a.result
	assert.Contains(t, first.Issue.Body, "Can you send the contract?")
	assert.Contains(t, first.Issue.Body, fmt.Sprintf("http://localhost:8080/messages/%d", id))
	assert.Equal(t, "replies", first.Issue.Metadata["msgvault.list"])
	assert.NotEmpty(t, first.Issue.Metadata["msgvault.person"])
	assert.NotContains(t, first.Issue.Metadata, "deadline_on")
	assert.Positive(t, person.ID)
	again, err := agenda.File(t.Context(), strconv.FormatInt(id, 10), now)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Issue.UID, again.Issue.UID)
	native.Endpoint(server).CloseIssue(t, first.Issue.UID)
	closed, err := agenda.File(t.Context(), strconv.FormatInt(id, 10), now)
	require.NoError(t, err)
	assert.True(t, closed.Replayed)
	assert.Equal(t, first.Issue.UID, closed.Issue.UID)
	apiServer := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, LoopAgenda: agenda, Logger: testLogger()})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/agenda", id), nil)
	response := httptest.NewRecorder()
	apiServer.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var receipt LoopAgendaResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &receipt))
	assert.True(t, receipt.Replayed)
	assert.Equal(t, first.Issue.UID, receipt.Issue.UID)
	native.Endpoint(server).DeleteIssue(t, first.Issue.UID, first.Issue.QualifiedRef)
	_, err = agenda.File(t.Context(), strconv.FormatInt(id, 10), now)
	assert.ErrorIs(t, err, kataissues.ErrIssueDeleted)
	require.NoError(t, agenda.Run(t.Context(), now))
}

func TestLoopAgendaRunLimitsCreatesAndSkipsAwaiting(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	for i := range 26 {
		f.PersistEmail(storetest.EmailSpec{Thread: fmt.Sprintf("thread-%d", i), From: fmt.Sprintf("person%d@example.test", i), To: []string{storetest.OwnerAddress}, Subject: "Request", Body: "Can you send it?", SentAt: now.Add(-48 * time.Hour)})
	}
	f.PersistEmail(storetest.EmailSpec{Thread: "waiting", From: storetest.OwnerAddress, To: []string{"other@example.test"}, Subject: "Question", Body: "Can you reply?", SentAt: now.Add(-96 * time.Hour)})
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	cfg := config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	agenda := NewLoopAgenda(cfg, f.Store, "http://localhost:8080")
	require.NoError(t, agenda.Run(t.Context(), now))
	client, err := connectKata(t.Context(), cfg)
	require.NoError(t, err)
	issues, _, err := client.OpenIssues(t.Context(), katatest.Project, 100)
	require.NoError(t, err)
	assert.Len(t, issues, 25)
	require.NoError(t, agenda.Run(t.Context(), now))
	issues, _, err = client.OpenIssues(t.Context(), katatest.Project, 100)
	require.NoError(t, err)
	assert.Len(t, issues, 26)
}

type promiseAgendaArchive struct {
	*store.Store
	promise store.PersonCommitment
}

func (a *promiseAgendaArchive) GetPersonCommitmentContext(_ context.Context, _ int64, _ time.Time) (store.PersonCommitment, error) {
	return a.promise, nil
}

func TestLoopAgendaFilesPromise(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	quote := strings.Repeat("I will send the draft. ", 50) + "Due December 1."
	id := f.PersistEmail(storetest.EmailSpec{Thread: "promise", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Subject: "Draft", Body: quote, SentAt: now.Add(-time.Hour)})
	participant := f.EnsureParticipant("alex@example.test", "", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}, f.Store, "http://localhost:8080")
	agenda.archive = &promiseAgendaArchive{Store: f.Store, promise: store.PersonCommitment{ID: 12, PersonID: person.ID, AnchorMessageID: id, Quote: quote, Start: 0, End: int64(len([]rune(quote))), Summary: "Send the draft", DueOn: new("2026-12-01")}}
	result, err := agenda.File(t.Context(), "c12", now)
	require.NoError(t, err)
	assert.Contains(t, result.Issue.Body, quote[:1000])
	assert.Contains(t, result.Issue.Body, quote[1000:])
	assert.Contains(t, result.Issue.Body, fmt.Sprintf("http://localhost:8080/messages/%d", id))
	assert.Equal(t, "promises", result.Issue.Metadata["msgvault.list"])
	assert.NotEmpty(t, result.Issue.Metadata["msgvault.person"])
	assert.Equal(t, "2026-12-01", result.Issue.Metadata["deadline_on"])
}

func TestLoopAgendaFilesLaterLoopUnderOpenIssue(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	firstID := f.PersistEmail(storetest.EmailSpec{Thread: "thread", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Subject: "First", Body: "Can you send the draft?", SentAt: now.Add(-72 * time.Hour)})
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}, f.Store, "http://localhost:8080")
	first, err := agenda.File(t.Context(), strconv.FormatInt(firstID, 10), now)
	require.NoError(t, err)
	f.PersistEmail(storetest.EmailSpec{Thread: "thread", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: "Here it is.", SentAt: now.Add(-60 * time.Hour)})
	laterID := f.PersistEmail(storetest.EmailSpec{Thread: "thread", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Subject: "Follow up", Body: "Can you revise it?", SentAt: now.Add(-48 * time.Hour)})
	later, err := agenda.File(t.Context(), strconv.FormatInt(laterID, 10), now)
	require.NoError(t, err)
	assert.Equal(t, first.Issue.QualifiedRef, later.Issue.ParentRef)
}
