package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/katatest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopAgendaUnavailableWithoutAgenda(t *testing.T) {
	f := storetest.New(t)
	server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, Logger: testLogger()})
	for _, path := range []string{"/api/v1/loops/1/agenda", "/api/v1/loops/commitments/c1/agenda"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.JSONEq(t, `{"error":"kata_unavailable","message":"Kata issues are unavailable"}`, response.Body.String())
		})
	}
}

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
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project, AutoAgenda: new(false)}, f.Store, "https://vault.example.test")
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
	assert.Contains(t, first.Issue.Body, fmt.Sprintf("https://vault.example.test/messages/%d", id))
	assert.Equal(t, "replies", first.Issue.Metadata["msgvault.list"])
	assert.Equal(t, person.VCardUID, first.Issue.Metadata["msgvault.person"])
	conversation, err := f.Store.MessageConversation(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(conversation.ConversationID, 10), first.Issue.Metadata["msgvault.conversation"])
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
		address := fmt.Sprintf("person%d@example.test", i)
		f.PersistEmail(storetest.EmailSpec{Thread: fmt.Sprintf("thread-%d", i), From: address, To: []string{storetest.OwnerAddress}, Subject: "Request", Body: "Can you send it?", SentAt: now.Add(-48 * time.Hour)})
		participant := f.EnsureParticipant(address, "", "example.test")
		_, _, err := f.Store.CreatePersonFromParticipant(participant)
		require.NoError(t, err)
	}
	f.PersistEmail(storetest.EmailSpec{Thread: "waiting", From: storetest.OwnerAddress, To: []string{"other@example.test"}, Subject: "Question", Body: "Can you reply?", SentAt: now.Add(-96 * time.Hour)})
	native := katatest.New(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		native.Service.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	agenda := NewLoopAgenda(cfg, f.Store, "https://vault.example.test")
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
	requests.Store(0)
	require.NoError(t, agenda.Run(t.Context(), now))
	assert.LessOrEqual(t, requests.Load(), int64(29))
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
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}, f.Store, "https://vault.example.test")
	agenda.archive = &promiseAgendaArchive{Store: f.Store, promise: store.PersonCommitment{ID: 12, PersonID: person.ID, AnchorMessageID: id, Quote: quote, Start: 0, End: int64(len([]rune(quote))), Summary: "Send the draft", Direction: "owed_by", PromisedAt: now.Add(-time.Hour), DueOn: new("2026-12-01")}}
	result, err := agenda.File(t.Context(), "c12", now)
	require.NoError(t, err)
	assert.Contains(t, result.Issue.Body, quote[:1000])
	assert.Contains(t, result.Issue.Body, quote[1000:])
	assert.Contains(t, result.Issue.Body, fmt.Sprintf("https://vault.example.test/messages/%d", id))
	assert.Equal(t, "promises", result.Issue.Metadata["msgvault.list"])
	assert.Equal(t, "Send the draft", result.Issue.Title)
	assert.Equal(t, person.VCardUID, result.Issue.Metadata["msgvault.person"])
	conversation, err := f.Store.MessageConversation(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(conversation.ConversationID, 10), result.Issue.Metadata["msgvault.conversation"])
	assert.Equal(t, "2026-12-01", result.Issue.Metadata["deadline_on"])
}

func TestLoopAgendaFilesLaterLoopUnderOpenIssue(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	firstID := f.PersistEmail(storetest.EmailSpec{Thread: "thread", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Subject: "First", Body: "Can you send the draft?", SentAt: now.Add(-72 * time.Hour)})
	participant := f.EnsureParticipant("alex@example.test", "", "example.test")
	_, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
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

func TestLoopAgendaRequiresLinkedPerson(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	id := f.PersistEmail(storetest.EmailSpec{Thread: "unlinked", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Subject: "Request", Body: "Can you reply?", SentAt: now.Add(-48 * time.Hour)})
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	cfg := config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	agenda := NewLoopAgenda(cfg, f.Store, "http://localhost:8080")
	apiServer := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, LoopAgenda: agenda, Logger: testLogger()})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/agenda", id), nil)
	response := httptest.NewRecorder()
	apiServer.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusConflict, response.Code)
	assert.JSONEq(t, `{"error":"person_required","message":"link the contact to a person first: alex@example.test"}`, response.Body.String())
	require.NoError(t, agenda.Run(t.Context(), now))
	client, err := connectKata(t.Context(), cfg)
	require.NoError(t, err)
	issues, _, err := client.OpenIssues(t.Context(), katatest.Project, 100)
	require.NoError(t, err)
	assert.Empty(t, issues)
	participant := f.EnsureParticipant("alex@example.test", "", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	require.NoError(t, agenda.Run(t.Context(), now))
	issues, _, err = client.OpenIssues(t.Context(), katatest.Project, 100)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, person.VCardUID, issues[0].Metadata["msgvault.person"])
}

func TestLoopAgendaRouteValidation(t *testing.T) {
	f := storetest.New(t)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{}, f.Store, "")
	server := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, LoopAgenda: agenda, Logger: testLogger()})
	for _, tc := range []struct {
		path   string
		status int
		code   string
	}{
		{"/api/v1/loops/c12/agenda", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/loops/0/agenda", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/loops/999999/agenda", http.StatusNotFound, "loop_not_open"},
		{"/api/v1/loops/commitments/bad/agenda", http.StatusBadRequest, "invalid_request"},
		{"/api/v1/loops/commitments/c999999/agenda", http.StatusNotFound, "commitment_not_found"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, nil))
			assert.Equal(t, tc.status, response.Code)
			assert.Contains(t, response.Body.String(), tc.code)
		})
	}
}

func TestLoopAgendaRunBeyondFirstPage(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	for i := range 201 {
		f.PersistEmail(storetest.EmailSpec{Thread: fmt.Sprintf("unlinked-%d", i), From: "unlinked@example.test", To: []string{storetest.OwnerAddress}, Subject: "Request", Body: "Can you send it?", SentAt: now.Add(-time.Duration(300-i) * time.Hour)})
	}
	f.PersistEmail(storetest.EmailSpec{Thread: "linked", From: "linked@example.test", To: []string{storetest.OwnerAddress}, Subject: "Request", Body: "Can you send it?", SentAt: now.Add(-48 * time.Hour)})
	participant := f.EnsureParticipant("linked@example.test", "", "example.test")
	_, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	native := katatest.New(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		native.Service.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	agenda := NewLoopAgenda(cfg, f.Store, "http://127.0.0.1:8080")
	require.NoError(t, agenda.Run(t.Context(), now))
	client, err := connectKata(t.Context(), cfg)
	require.NoError(t, err)
	issues, _, err := client.OpenIssues(t.Context(), katatest.Project, 10)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Contains(t, issues[0].Body, "http://127.0.0.1:8080/messages/")
	assert.Contains(t, issues[0].Body, "Can you send it?")
	requests.Store(0)
	require.NoError(t, agenda.Run(t.Context(), now))
	// One marker lookup and connection setup, with no per-issue fetch.
	assert.LessOrEqual(t, requests.Load(), int64(4))
}

type agendaTestArchive struct {
	*store.Store
	promises   []store.PersonCommitment
	incomplete bool
}

func (a *agendaTestArchive) GetPersonCommitmentContext(_ context.Context, id int64, _ time.Time) (store.PersonCommitment, error) {
	for _, c := range a.promises {
		if c.ID == id {
			return c, nil
		}
	}
	return store.PersonCommitment{}, sql.ErrNoRows
}
func (a *agendaTestArchive) WalkOpenPersonCommitments(_ context.Context, _ time.Time, visit func(store.PersonCommitment) error) error {
	for _, c := range a.promises {
		if err := visit(c); err != nil {
			return err
		}
	}
	return nil
}
func (a *agendaTestArchive) LoopMarksContext(ctx context.Context) (map[int64]store.LoopMark, error) {
	if a.incomplete {
		return nil, loops.ErrEvidenceIncomplete
	}
	return a.Store.LoopMarksContext(ctx)
}

func TestLoopAgendaPromiseScopeAndOrder(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	firstAt := now.Add(-72 * time.Hour)
	body := "I will send the draft."
	firstID := f.PersistEmail(storetest.EmailSpec{Thread: "scope", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: firstAt})
	laterID := f.PersistEmail(storetest.EmailSpec{Thread: "scope", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you revise it?", SentAt: now.Add(-48 * time.Hour)})
	p := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(p)
	require.NoError(t, err)
	otherP := f.EnsureParticipant("sam@example.test", "Sam", "example.test")
	other, _, err := f.Store.CreatePersonFromParticipant(otherP)
	require.NoError(t, err)
	promise := store.PersonCommitment{ID: 1, PersonID: person.ID, AnchorMessageID: firstID, Direction: "owed_by", Quote: body, Start: 0, End: int64(len(body)), Summary: "Send draft", PromisedAt: firstAt}
	second := promise
	second.ID = 2
	second.Summary = "Send copy"
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	cfg := config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	agenda := NewLoopAgenda(cfg, f.Store, "http://localhost:8080")
	agenda.archive = &agendaTestArchive{Store: f.Store, promises: []store.PersonCommitment{promise, second}}
	require.NoError(t, agenda.Run(t.Context(), now))
	one, err := agenda.File(t.Context(), "c1", now)
	require.NoError(t, err)
	two, err := agenda.File(t.Context(), "c2", now)
	require.NoError(t, err)
	reply, err := agenda.File(t.Context(), strconv.FormatInt(laterID, 10), now)
	require.NoError(t, err)
	assert.Empty(t, one.Issue.ParentRef)
	assert.Empty(t, two.Issue.ParentRef)
	assert.Equal(t, one.Issue.QualifiedRef, reply.Issue.ParentRef)
	laterPromiseID := f.PersistEmail(storetest.EmailSpec{Thread: "scope", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: now.Add(-time.Hour)})
	third := promise
	third.AnchorMessageID = laterPromiseID
	third.ID = 3
	third.PersonID = other.ID
	third.PromisedAt = now.Add(-time.Hour)
	archive := agenda.archive.(*agendaTestArchive)
	archive.promises = append(archive.promises, third)
	different, err := agenda.File(t.Context(), "c3", now)
	require.NoError(t, err)
	assert.Empty(t, different.Issue.ParentRef)
	// A promise after an earlier reply for the same person nests under it.
	replyID := f.PersistEmail(storetest.EmailSpec{Thread: "reply-first", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you send it?", SentAt: now.Add(-48 * time.Hour)})
	earlierReply, err := agenda.File(t.Context(), strconv.FormatInt(replyID, 10), now)
	require.NoError(t, err)
	lastID := f.PersistEmail(storetest.EmailSpec{Thread: "reply-first", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: now.Add(-time.Hour)})
	fourth := promise
	fourth.AnchorMessageID = lastID
	fourth.ID = 4
	fourth.PromisedAt = now.Add(-time.Hour)
	archive.promises = append(archive.promises, fourth)
	last, err := agenda.File(t.Context(), "c4", now)
	require.NoError(t, err)
	assert.Equal(t, earlierReply.Issue.QualifiedRef, last.Issue.ParentRef)
}

func TestLoopAgendaWaitingAndIncompleteScan(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	body := "I will send it."
	id := f.PersistEmail(storetest.EmailSpec{Thread: "waiting", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: body, SentAt: now.Add(-time.Hour)})
	p := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), p, new("Alex"))
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	cfg := config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	agenda := NewLoopAgenda(cfg, f.Store, "http://localhost:8080")
	agenda.archive = &agendaTestArchive{Store: f.Store, incomplete: true, promises: []store.PersonCommitment{{ID: 1, PersonID: person.ID, AnchorMessageID: id, Direction: "owed_to", Quote: body, End: int64(len(body)), Summary: "Send it", PromisedAt: now.Add(-time.Hour)}}}
	require.ErrorIs(t, agenda.Run(t.Context(), now), loops.ErrEvidenceIncomplete)
	result, err := agenda.File(t.Context(), "c1", now)
	require.NoError(t, err)
	assert.True(t, result.Replayed)
	assert.Equal(t, "Waiting on Alex: Send it", result.Issue.Title)
	assert.Equal(t, "waiting", result.Issue.Metadata["msgvault.list"])
	apiServer := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, LoopAgenda: agenda, Logger: testLogger()})
	response := httptest.NewRecorder()
	apiServer.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/agenda", id), nil))
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, response.Body.String(), "loop_scan_incomplete")
}

func TestLoopAgendaRunSkipsChangedQuote(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	body := "I will send it."
	id := f.PersistEmail(storetest.EmailSpec{Thread: "changed", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: now.Add(-time.Hour)})
	p := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(p)
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	cfg := config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	agenda := NewLoopAgenda(cfg, f.Store, "http://localhost:8080")
	agenda.archive = &agendaTestArchive{Store: f.Store, promises: []store.PersonCommitment{{ID: 1, PersonID: person.ID, AnchorMessageID: id, Direction: "owed_by", Quote: "changed text", End: int64(len(body)), Summary: "Send it", PromisedAt: now.Add(-time.Hour)}}}
	require.NoError(t, agenda.Run(t.Context(), now))
	_, err = agenda.File(t.Context(), "c1", now)
	assert.ErrorIs(t, err, kataevidence.ErrChanged)
}

func TestLoopAgendaRunSkipsPersonWithoutStableIdentity(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	body := "I will send it."
	id := f.PersistEmail(storetest.EmailSpec{Thread: "identity", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: now.Add(-time.Hour)})
	p := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(p)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE persons SET vcard_uid = ? WHERE id = ?"), "", person.ID)
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	cfg := config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	agenda := NewLoopAgenda(cfg, f.Store, "http://localhost:8080")
	agenda.archive = &agendaTestArchive{Store: f.Store, promises: []store.PersonCommitment{{ID: 1, PersonID: person.ID, AnchorMessageID: id, Direction: "owed_by", Quote: body, End: int64(len(body)), Summary: "Send it", PromisedAt: now.Add(-time.Hour)}}}
	_, err = agenda.File(t.Context(), "c1", now)
	require.ErrorIs(t, err, personagenda.ErrPersonIdentity)
	require.NoError(t, agenda.Run(t.Context(), now))
}
