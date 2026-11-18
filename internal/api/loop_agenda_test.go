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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/loops"
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
	first, err := agenda.File(t.Context(), strconv.FormatInt(id, 10), now)
	require.NoError(t, err)
	assert.False(t, first.Replayed)
	service, err := agenda.backend.service(t.Context())
	require.NoError(t, err)
	identity, err := f.Store.MessageIdentityContext(t.Context(), id)
	require.NoError(t, err)
	_, filed, err := service.FindFiled(t.Context(), "loop-source.v1:"+identity.Message)
	require.NoError(t, err)
	assert.True(t, filed)
	assert.Contains(t, first.Issue.Body, "Can you send the contract?")
	assert.Contains(t, first.Issue.Body, fmt.Sprintf("https://vault.example.test/messages/%d", id))
	assert.Equal(t, "replies", first.Issue.Metadata["msgvault.list"])
	assert.Equal(t, person.VCardUID, first.Issue.Metadata["msgvault.person"])
	conversation, err := f.Store.MessageConversation(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(conversation.ConversationID, 10), first.Issue.Metadata["msgvault.conversation"])
	assert.NotContains(t, first.Issue.Metadata, "deadline_on")
	again, err := agenda.File(t.Context(), strconv.FormatInt(id, 10), now)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Issue.UID, again.Issue.UID)
	apiServer := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, LoopAgenda: agenda, Logger: testLogger()})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/agenda", id), nil)
	response := httptest.NewRecorder()
	apiServer.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var receipt LoopAgendaResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &receipt))
	assert.True(t, receipt.Replayed)
	assert.Equal(t, first.Issue.UID, receipt.Issue.UID)
	// Once the owner answers, the loop closes, and filing it again still returns its issue.
	f.PersistEmail(storetest.EmailSpec{Thread: "thread", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Subject: "Re: Contract", Body: "Attached.", SentAt: now.Add(-24 * time.Hour)})
	_, err = loops.Open(t.Context(), f.Store, id, now)
	require.ErrorIs(t, err, loops.ErrNotOpen)
	answered, err := agenda.File(t.Context(), strconv.FormatInt(id, 10), now)
	require.NoError(t, err)
	assert.True(t, answered.Replayed)
	assert.Equal(t, first.Issue.UID, answered.Issue.UID)
}

func TestLoopAgendaRunSkipsAwaiting(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	waitingID := f.PersistEmail(storetest.EmailSpec{Thread: "waiting", From: storetest.OwnerAddress, To: []string{"other@example.test"}, Subject: "Question", Body: "Can you reply?", SentAt: now.Add(-96 * time.Hour)})
	participant := f.EnsureParticipant("other@example.test", "", "example.test")
	_, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	cfg := config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	agenda := NewLoopAgenda(cfg, f.Store, "https://vault.example.test")
	_, err = agenda.File(t.Context(), strconv.FormatInt(waitingID, 10), now)
	require.ErrorIs(t, err, loops.ErrNotOpen)
	require.NoError(t, agenda.Run(t.Context(), now))
	client, err := connectKata(t.Context(), cfg)
	require.NoError(t, err)
	issues, _, err := client.OpenIssues(t.Context(), katatest.Project, 100)
	require.NoError(t, err)
	assert.Empty(t, issues)
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
	agenda.archive = &agendaTestArchive{Store: f.Store, promises: []store.PersonCommitment{{ID: 12, PersonID: person.ID, AnchorMessageID: id, Quote: quote, Start: 0, End: int64(len([]rune(quote))), Summary: "Send the draft", Direction: "owed_by", PromisedAt: now.Add(-time.Hour), DueOn: new("2026-12-01")}}}
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
	// A later promise in the same conversation nests only for the same person.
	otherParticipant := f.EnsureParticipant("sam@example.test", "", "example.test")
	other, _, err := f.Store.CreatePersonFromParticipant(otherParticipant)
	require.NoError(t, err)
	archive, ok := agenda.archive.(*agendaTestArchive)
	require.True(t, ok)
	third := archive.promises[0]
	third.ID, third.PersonID, third.PromisedAt = 3, other.ID, now
	third.AnchorMessageID = f.PersistEmail(storetest.EmailSpec{Thread: "promise", From: storetest.OwnerAddress, To: []string{"sam@example.test"}, Body: quote, SentAt: now})
	archive.promises = append(archive.promises, third)
	different, err := agenda.File(t.Context(), "c3", now)
	require.NoError(t, err)
	assert.Empty(t, agendaParent(t, native, server, different.Issue.UID))
	fourth := third
	fourth.ID, fourth.PersonID = 4, person.ID
	fourth.AnchorMessageID = f.PersistEmail(storetest.EmailSpec{Thread: "promise", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: quote, SentAt: now})
	archive.promises = append(archive.promises, fourth)
	same, err := agenda.File(t.Context(), "c4", now)
	require.NoError(t, err)
	assert.Equal(t, result.Issue.UID, agendaParent(t, native, server, same.Issue.UID))
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
	assert.Equal(t, first.Issue.UID, agendaParent(t, native, server, later.Issue.UID))
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
	native := katatest.New(t)
	kata := httptest.NewServer(native.Service.Handler())
	t.Cleanup(kata.Close)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: kata.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}, f.Store, "")
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

type agendaTestArchive struct {
	*store.Store

	promises   []store.PersonCommitment
	incomplete bool
}

// sourced gives a promise without a source key one of its own, as the store
// gives each promise its source message's key.
func sourced(c store.PersonCommitment) store.PersonCommitment {
	if c.SourceKey == "" {
		c.SourceKey = "source-" + strconv.FormatInt(c.ID, 10)
	}
	if c.State == "" {
		c.State = store.LoopStateOpen
	}
	return c
}

func (a *agendaTestArchive) GetPersonCommitmentContext(_ context.Context, id int64, _ time.Time) (store.PersonCommitment, error) {
	for _, c := range a.promises {
		if c.ID == id {
			return sourced(c), nil
		}
	}
	return store.PersonCommitment{}, sql.ErrNoRows
}
func (a *agendaTestArchive) WalkOpenPersonCommitments(_ context.Context, _ time.Time, visit func(store.PersonCommitment) error) error {
	for _, c := range a.promises {
		if err := visit(sourced(c)); err != nil {
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
	const copyQuote = "I will send a copy."
	firstID := f.PersistEmail(storetest.EmailSpec{Thread: "scope", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body + " " + copyQuote, SentAt: firstAt})
	laterID := f.PersistEmail(storetest.EmailSpec{Thread: "scope", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you revise it?", SentAt: now.Add(-48 * time.Hour)})
	p := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(p)
	require.NoError(t, err)
	otherP := f.EnsureParticipant("sam@example.test", "Sam", "example.test")
	other, _, err := f.Store.CreatePersonFromParticipant(otherP)
	require.NoError(t, err)
	promise := store.PersonCommitment{ID: 1, PersonID: person.ID, AnchorMessageID: firstID, Direction: "owed_by", Quote: body, Start: 0, End: int64(len(body)), Summary: "Send draft", PromisedAt: firstAt}
	// The same message holds a second, separately quoted promise.
	second := promise
	second.ID, second.Summary, second.Quote = 2, "Send copy", copyQuote
	second.Start, second.End = int64(len(body)+1), int64(len(body)+1+len(copyQuote))
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
	assert.Empty(t, agendaParent(t, native, server, one.Issue.UID))
	assert.Empty(t, agendaParent(t, native, server, two.Issue.UID))
	assert.Equal(t, one.Issue.UID, agendaParent(t, native, server, reply.Issue.UID))
	laterPromiseID := f.PersistEmail(storetest.EmailSpec{Thread: "scope", From: storetest.OwnerAddress, To: []string{"sam@example.test"}, Body: body, SentAt: now.Add(-time.Hour)})
	third := promise
	third.AnchorMessageID = laterPromiseID
	third.ID = 3
	third.PersonID = other.ID
	third.PromisedAt = now.Add(-time.Hour)
	archive, ok := agenda.archive.(*agendaTestArchive)
	require.True(t, ok)
	archive.promises = append(archive.promises, third)
	different, err := agenda.File(t.Context(), "c3", now)
	require.NoError(t, err)
	assert.Empty(t, agendaParent(t, native, server, different.Issue.UID))
	// A promise after an earlier reply for the same person nests under it.
	replyID := f.PersistEmail(storetest.EmailSpec{Thread: "reply-first", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you send it?", SentAt: now.Add(-48 * time.Hour)})
	earlierReply, err := agenda.File(t.Context(), strconv.FormatInt(replyID, 10), now)
	require.NoError(t, err)
	lastID := f.PersistEmail(storetest.EmailSpec{Thread: "reply-first", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: now.Add(-time.Hour)})
	fourth := promise
	fourth.AnchorMessageID = lastID
	fourth.ID = 4
	fourth.PersonID = person.ID
	fourth.PromisedAt = now.Add(-time.Hour)
	archive.promises = append(archive.promises, fourth)
	last, err := agenda.File(t.Context(), "c4", now)
	require.NoError(t, err)
	assert.Equal(t, earlierReply.Issue.UID, agendaParent(t, native, server, last.Issue.UID))
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
	t.Run("run continues past incomplete loop scan", func(t *testing.T) {
		require.ErrorIs(t, agenda.Run(t.Context(), now), loops.ErrEvidenceIncomplete)
		client, err := connectKata(t.Context(), cfg)
		require.NoError(t, err)
		issues, _, err := client.OpenIssues(t.Context(), katatest.Project, 100)
		require.NoError(t, err)
		assert.Len(t, issues, 1)
	})
	t.Run("waiting title and list", func(t *testing.T) {
		result, err := agenda.File(t.Context(), "c1", now)
		require.NoError(t, err)
		assert.Equal(t, "Waiting on Alex: Send it", result.Issue.Title)
		assert.Equal(t, "waiting", result.Issue.Metadata["msgvault.list"])
	})
	t.Run("incomplete scan HTTP mapping", func(t *testing.T) {
		apiServer := NewServerWithOptions(ServerOptions{Config: config.NewDefaultConfig(), Store: f.Store, LoopAgenda: agenda, Logger: testLogger()})
		response := httptest.NewRecorder()
		apiServer.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/loops/%d/agenda", id), nil))
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.Contains(t, response.Body.String(), "loop_scan_incomplete")
	})
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
	agenda.archive = &agendaTestArchive{Store: f.Store, promises: []store.PersonCommitment{{ID: 1, PersonID: person.ID, AnchorMessageID: id, Direction: "owed_by", Quote: body, End: int64(len(body)), Summary: "Send it", PromisedAt: now.Add(-time.Hour)}}}
	// The archived message is edited after the promise was found, so its quote no longer matches.
	editMessageBody(t, f, id, "I will send it soon.")
	require.NoError(t, agenda.Run(t.Context(), now))
	_, err = agenda.File(t.Context(), "c1", now)
	require.ErrorIs(t, err, kataevidence.ErrChanged)
	client, err := connectKata(t.Context(), cfg)
	require.NoError(t, err)
	issues, _, err := client.OpenIssues(t.Context(), katatest.Project, 10)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func editMessageBody(t *testing.T, f *storetest.Fixture, id int64, body string) {
	t.Helper()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), body, id)
	require.NoError(t, err)
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
	require.NoError(t, agenda.Run(t.Context(), now))
	client, err := connectKata(t.Context(), cfg)
	require.NoError(t, err)
	issues, _, err := client.OpenIssues(t.Context(), katatest.Project, 100)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestLoopAgendaReplayBeforeCitationValidation(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	body := "I will send it."
	id := f.PersistEmail(storetest.EmailSpec{Thread: "retry", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: now.Add(-time.Hour)})
	p := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(p)
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}, f.Store, "")
	archive := &agendaTestArchive{Store: f.Store, promises: []store.PersonCommitment{{ID: 1, PersonID: person.ID, AnchorMessageID: id, Quote: body, End: int64(len(body)), Summary: "Send it", Direction: "owed_by", PromisedAt: now.Add(-time.Hour)}}}
	agenda.archive = archive
	first, err := agenda.File(t.Context(), "c1", now)
	require.NoError(t, err)
	// After the archived message is edited, the citation no longer validates, but the issue already exists.
	editMessageBody(t, f, id, "I will send it soon.")
	replay, err := agenda.File(t.Context(), "c1", now)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, first.Issue.UID, replay.Issue.UID)
}

func TestLoopAgendaPromiseReplayAfterPersonMerge(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	body := "I will send it."
	id := f.PersistEmail(storetest.EmailSpec{Thread: "merge", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: now.Add(-time.Hour)})
	p := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	absorbed, _, err := f.Store.CreatePersonFromParticipant(p)
	require.NoError(t, err)
	p2 := f.EnsureParticipant("sam@example.test", "Sam", "example.test")
	survivor, _, err := f.Store.CreatePersonFromParticipant(p2)
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}, f.Store, "")
	archive := &agendaTestArchive{Store: f.Store, promises: []store.PersonCommitment{{ID: 1, PersonID: absorbed.ID, AnchorMessageID: id, Quote: body, End: int64(len(body)), Summary: "Send it", Direction: "owed_by", PromisedAt: now.Add(-time.Hour), SourceKey: "same-source"}}}
	agenda.archive = archive
	first, err := agenda.File(t.Context(), "c1", now)
	require.NoError(t, err)
	_, err = f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "agenda-merge", Actor: "test"})
	require.NoError(t, err)
	archive.promises[0].ID, archive.promises[0].PersonID = 2, survivor.ID
	require.NoError(t, agenda.Run(t.Context(), now))
	replay, err := agenda.File(t.Context(), "c2", now)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, first.Issue.UID, replay.Issue.UID)
}

func TestLoopAgendaPromiseKeyFollowsSource(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	body := "I will send it."
	first := f.PersistEmail(storetest.EmailSpec{Thread: "source-key", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: now.Add(-2 * time.Hour)})
	moved := f.PersistEmail(storetest.EmailSpec{Thread: "source-key", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: "Hi. " + body, SentAt: now.Add(-time.Hour)})
	p := f.EnsureParticipant("alex@example.test", "Alex", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(p)
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}, f.Store, "")
	promise := store.PersonCommitment{ID: 1, PersonID: person.ID, AnchorMessageID: first, Quote: body, End: int64(len(body)), Summary: "Send it", Direction: "owed_by", PromisedAt: now.Add(-2 * time.Hour), SourceKey: "source-a"}
	// The same source promise read from another row and position files nothing new.
	again := promise
	again.ID, again.AnchorMessageID, again.Start, again.End = 2, moved, 4, int64(4+len(body))
	// New wording at the first row and position, after the message is edited, is a new promise.
	const reworded = "I will mail it."
	replaced := promise
	replaced.ID, replaced.Quote, replaced.SourceKey = 3, reworded, "source-b"
	agenda.archive = &agendaTestArchive{Store: f.Store, promises: []store.PersonCommitment{promise, again, replaced}}
	filed, err := agenda.File(t.Context(), "c1", now)
	require.NoError(t, err)
	reused, err := agenda.File(t.Context(), "c2", now)
	require.NoError(t, err)
	assert.True(t, reused.Replayed)
	assert.Equal(t, filed.Issue.UID, reused.Issue.UID)
	editMessageBody(t, f, first, reworded)
	fresh, err := agenda.File(t.Context(), "c3", now)
	require.NoError(t, err)
	assert.False(t, fresh.Replayed)
	assert.NotEqual(t, filed.Issue.UID, fresh.Issue.UID)
}

func TestLoopAgendaReusedRowIDsFileNewIssues(t *testing.T) {
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL sequences never reuse a deleted row's ID")
	}
	now := time.Now().UTC()
	first := f.PersistEmail(storetest.EmailSpec{Thread: "removed", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you send it?", SentAt: now.Add(-72 * time.Hour)})
	_, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("alex@example.test", "", "example.test"))
	require.NoError(t, err)
	conversation, err := f.Store.MessageConversation(t.Context(), first)
	require.NoError(t, err)
	native := katatest.New(t)
	kata := httptest.NewServer(native.Service.Handler())
	t.Cleanup(kata.Close)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: kata.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}, f.Store, "")
	filed, err := agenda.File(t.Context(), strconv.FormatInt(first, 10), now)
	require.NoError(t, err)
	// Removing the newest conversation frees its row IDs, and SQLite gives them to the next import.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM conversations WHERE id = ?`), conversation.ConversationID)
	require.NoError(t, err)
	second := f.PersistEmail(storetest.EmailSpec{Thread: "imported", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you review it?", SentAt: now.Add(-48 * time.Hour)})
	require.Equal(t, first, second)
	reused, err := f.Store.MessageConversation(t.Context(), second)
	require.NoError(t, err)
	require.Equal(t, conversation.ConversationID, reused.ConversationID)
	fresh, err := agenda.File(t.Context(), strconv.FormatInt(second, 10), now)
	require.NoError(t, err)
	assert.False(t, fresh.Replayed)
	assert.NotEqual(t, filed.Issue.UID, fresh.Issue.UID)
	assert.Empty(t, agendaParent(t, native, kata, fresh.Issue.UID))
}

func TestLoopAgendaRefilesClosedPromise(t *testing.T) {
	f := storetest.New(t)
	now := time.Now().UTC()
	body := "I will send it."
	id := f.PersistEmail(storetest.EmailSpec{Thread: "closed", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: body, SentAt: now.Add(-time.Hour)})
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("alex@example.test", "Alex", "example.test"))
	require.NoError(t, err)
	native := katatest.New(t)
	kata := httptest.NewServer(native.Service.Handler())
	t.Cleanup(kata.Close)
	agenda := NewLoopAgenda(config.TaskIntegrationConfig{Enabled: true, Endpoint: kata.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}, f.Store, "")
	promise := store.PersonCommitment{ID: 1, PersonID: person.ID, AnchorMessageID: id, Quote: body, End: int64(len(body)), Summary: "Send it", Direction: "owed_by", PromisedAt: now.Add(-time.Hour)}
	const call = "I will call you."
	unfiled := promise
	unfiled.ID, unfiled.State, unfiled.Summary, unfiled.Quote, unfiled.End = 2, store.LoopStateDone, "Call", call, int64(len(call))
	unfiled.AnchorMessageID = f.PersistEmail(storetest.EmailSpec{Thread: "closed-call", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: call, SentAt: now.Add(-time.Hour)})
	archive := &agendaTestArchive{Store: f.Store, promises: []store.PersonCommitment{promise, unfiled}}
	agenda.archive = archive
	filed, err := agenda.File(t.Context(), "c1", now)
	require.NoError(t, err)
	// Marking the promise done or snoozing it keeps returning its issue.
	for _, state := range []string{store.LoopStateDone, store.LoopStateSnoozed} {
		archive.promises[0].State = state
		again, err := agenda.File(t.Context(), "c1", now)
		require.NoError(t, err, state)
		assert.True(t, again.Replayed, state)
		assert.Equal(t, filed.Issue.UID, again.Issue.UID, state)
	}
	// A closed promise that was never filed files nothing.
	_, err = agenda.File(t.Context(), "c2", now)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func agendaParent(t *testing.T, native katatest.Kata, server *httptest.Server, uid string) string {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", server.URL, native.ProjectID, uid), nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+katatest.Token)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { assert.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var body struct {
		Parent *struct {
			UID string `json:"uid"`
		} `json:"parent"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
	if body.Parent == nil {
		return ""
	}
	return body.Parent.UID
}
