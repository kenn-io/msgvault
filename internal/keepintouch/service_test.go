package keepintouch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	kata "go.kenn.io/kata"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
)

type outreachArchive struct {
	cursor  atomic.Int64
	people  []store.Person
	states  map[int64]store.ContactState
	aliases map[int64][]string
}

func (a *outreachArchive) ListKeepInTouchPeopleContext(_ context.Context, after int64, limit int) ([]store.Person, error) {
	var out []store.Person
	for _, p := range a.people {
		if p.ID > after && len(out) < limit {
			out = append(out, p)
		}
	}
	return out, nil
}
func (a *outreachArchive) ContactStateContext(_ context.Context, id int64, _ time.Time) (store.ContactState, error) {
	return a.states[id], nil
}
func (a *outreachArchive) ListPersonUIDsContext(_ context.Context, id int64) ([]string, error) {
	return a.aliases[id], nil
}
func (*outreachArchive) ArchiveUIDContext(context.Context) (string, error) {
	return "synthetic-archive", nil
}

func (a *outreachArchive) KeepInTouchCursorContext(context.Context) (int64, error) {
	return a.cursor.Load(), nil
}
func (a *outreachArchive) AdvanceKeepInTouchCursorContext(_ context.Context, expected, next int64) (bool, error) {
	return a.cursor.CompareAndSwap(expected, next), nil
}

type outreachDrafts struct {
	calls      atomic.Int64
	err        error
	failPerson int64
	before     func()
}

func (d *outreachDrafts) Build(_ context.Context, candidate Candidate) (Draft, error) {
	d.calls.Add(1)
	if d.before != nil {
		d.before()
	}
	if d.failPerson != 0 && candidate.Person.ID != d.failPerson {
		return Draft{Text: "Hi, how have you been?", Mode: "neutral_no_evidence"}, nil
	}
	return Draft{Text: "Hi, how have you been?", Mode: "neutral_no_evidence"}, d.err
}

type outreachFixtureState struct {
	Service *Service
	Archive *outreachArchive
	Drafts  *outreachDrafts
	Native  katatest.Kata
	Server  *httptest.Server
}

func outreachFixture(t *testing.T) outreachFixtureState {
	t.Helper()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	last := now.AddDate(0, 0, -30)
	due := last.AddDate(0, 0, 21)
	name := "Example Person"
	a := &outreachArchive{people: []store.Person{{ID: 1, VCardUID: "person-a", DisplayName: &name}}, states: map[int64]store.ContactState{1: {LastContactAt: &last, CadenceDueAt: &due, LastContactRef: "message:42", LastContactChannel: "chat"}}, aliases: map[int64][]string{1: {"person-a"}}}
	native := katatest.New(t)
	server := httptest.NewServer(native.Service.Handler())
	t.Cleanup(server.Close)
	// Fixture requests need no throughput guarantee on the shared host.
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: "example", HTTPClient: server.Client(), Timeout: time.Minute})
	Require.NoError(t, err)
	drafts := &outreachDrafts{}
	cfg := DefaultConfig()
	cfg.Owner = "operator"
	return outreachFixtureState{Service: &Service{Config: cfg, Archive: a, Kata: client, Project: "example", Drafts: drafts, Clock: func() time.Time { return now }}, Archive: a, Drafts: drafts, Native: native, Server: server}
}

func TestRenderIssueUsesReadableDraftLabels(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	lastContact := now.AddDate(0, 0, -30)
	due := now.AddDate(0, 0, -9)
	candidate := Candidate{
		Person: store.Person{ID: 7},
		UIDs:   []string{"person-seven"},
		State: store.ContactState{
			LastContactAt:      &lastContact,
			CadenceDueAt:       &due,
			LastContactRef:     "message:42",
			LastContactChannel: "chat",
		},
	}
	for _, test := range []struct {
		name  string
		mode  string
		label string
	}{
		{name: "grounded messages", mode: "grounded", label: "Draft from messages (review before sending)"},
		{name: "no evidence", mode: "neutral_no_evidence", label: "Neutral draft, no evidence (review before sending)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := Assert.New(t)
			body := renderIssue(candidate, Draft{Mode: test.mode, Text: "How have you been?"}, now)
			assert.Contains(body, test.label)
			assert.NotContains(body, test.mode)
			assert.Contains(body, "No message was sent. Closing this issue stops reminders until another qualifying contact.")
			assert.NotContains(body, "contact gap")
		})
	}
}

type failingOutreachCycleLookup struct {
	Kata

	err error
}

func (k failingOutreachCycleLookup) FindActionTaskComplete(context.Context, string) (taskclient.KataTask, bool, error) {
	return taskclient.KataTask{}, false, k.err
}

func TestKeepInTouchCycleLookupFailureExplainsOperation(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	fixture.Service.Kata = failingOutreachCycleLookup{Kata: fixture.Service.Kata, err: errors.New("synthetic lookup failure")}
	result, err := fixture.Service.Check(t.Context(), false)
	require.Error(err)
	require.Len(result.Failures, 1)
	assert.Contains(result.Failures[0].Error, "cannot search all Kata reminders: synthetic lookup failure")
	assert.NotContains(result.Failures[0].Error, "cycle")
}

func TestKeepInTouchPreviewDoesNotCallProvidersOrKata(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, drafts := fixture.Service, fixture.Drafts
	s.Kata = nil
	result, err := s.Check(t.Context(), true)
	require.NoError(err, "%+v", result.Failures)
	require.Len(result.Items, 1)
	assert.Equal("preview", result.Items[0].Status)
	assert.Zero(drafts.calls.Load())
}

func TestKeepInTouchPreviewStartsBeforeSavedCursor(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	addOutreachPerson(fixture.Archive, 2)
	fixture.Archive.cursor.Store(1)
	fixture.Service.Config.Limit = 1
	result, err := fixture.Service.Check(t.Context(), true)
	require.NoError(err, "%+v", result.Failures)
	require.Len(result.Items, 1)
	assert.Equal(int64(1), result.Items[0].PersonID)
	assert.Equal(int64(1), fixture.Archive.cursor.Load())
	assert.Zero(fixture.Drafts.calls.Load())
}

func TestKeepInTouchClosedCycleMergeAndNewContact(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, a, drafts, native, server := fixture.Service, fixture.Archive, fixture.Drafts, fixture.Native, fixture.Server
	result, err := s.Check(t.Context(), false)
	require.NoError(err, "%+v", result.Failures)
	require.Len(result.Items, 1)
	uid := result.Items[0].Issue.UID
	require.NotEmpty(uid)
	native.Endpoint(server).CloseIssue(t, uid)
	_, err = s.Check(t.Context(), false)
	require.NoError(err)
	assert.Equal(int64(1), drafts.calls.Load())
	// A merge retires the old UID but preserves its closed cycle.
	a.people[0].VCardUID = "person-survivor"
	a.aliases[1] = []string{"person-survivor", "person-a"}
	_, err = s.Check(t.Context(), false)
	require.NoError(err)
	assert.Equal(int64(1), drafts.calls.Load())
	now := s.Clock()
	state := a.states[1]
	recent := now
	recentDue := recent.AddDate(0, 0, 21)
	state.LastContactAt = &recent
	state.CadenceDueAt = &recentDue
	a.states[1] = state
	_, err = s.Check(t.Context(), false)
	require.NoError(err)
	assert.Equal(int64(1), drafts.calls.Load())
	s.Clock = func() time.Time { return now.AddDate(0, 0, 15) }
	_, err = s.Check(t.Context(), false)
	require.NoError(err)
	assert.Equal(int64(2), drafts.calls.Load())
}

func TestKeepInTouchOpenReminderSuppressesNewCycle(t *testing.T) {
	fixture := outreachFixture(t)
	s, a, drafts := fixture.Service, fixture.Archive, fixture.Drafts
	_, err := s.Check(t.Context(), false)
	Require.NoError(t, err)
	state := a.states[1]
	last := state.LastContactAt.AddDate(0, 0, 1)
	due := last.AddDate(0, 0, 21)
	state.LastContactAt = &last
	state.CadenceDueAt = &due
	a.states[1] = state
	_, err = s.Check(t.Context(), false)
	Require.NoError(t, err)
	Assert.Equal(t, int64(1), drafts.calls.Load())
}

func TestKeepInTouchConcurrentChecksCreateOneIssue(t *testing.T) {
	s := outreachFixture(t).Service
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := s.Check(t.Context(), false); failures <- err })
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		Require.NoError(t, err)
	}
	tasks, err := s.Kata.ListPersonTasksComplete(t.Context(), "person-a")
	Require.NoError(t, err)
	Assert.Len(t, tasks, 1)
}

func TestKeepInTouchContactChangeDuringDraftDefersPublication(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, a, drafts := fixture.Service, fixture.Archive, fixture.Drafts
	drafts.before = func() {
		state := a.states[1]
		now := s.Clock()
		due := now.AddDate(0, 0, 21)
		state.LastContactAt = &now
		state.CadenceDueAt = &due
		a.states[1] = state
	}
	result, err := s.Check(t.Context(), false)
	require.NoError(err)
	assert.Empty(result.Items)
	tasks, err := s.Kata.ListPersonTasksComplete(t.Context(), "person-a")
	require.NoError(err)
	assert.Empty(tasks)
}

func TestKeepInTouchFailureConsumesAttemptCap(t *testing.T) {
	fixture := outreachFixture(t)
	s, a, drafts := fixture.Service, fixture.Archive, fixture.Drafts
	drafts.err = errors.New("synthetic refusal")
	s.Config.MaxDraftsPerRun = 1
	a.people = append(a.people, store.Person{ID: 2, VCardUID: "person-b"})
	a.aliases[2] = []string{"person-b"}
	a.states[2] = a.states[1]
	_, err := s.Check(t.Context(), false)
	Require.Error(t, err)
	Assert.Equal(t, int64(1), drafts.calls.Load())
}

func TestKeepInTouchCreateFailureRetriesWinningDraft(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, drafts, native := fixture.Service, fixture.Drafts, fixture.Native
	var drop atomic.Bool
	drop.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") && drop.Swap(false) {
			http.Error(w, "synthetic refusal", http.StatusServiceUnavailable)
			return
		}
		native.Service.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: "example", HTTPClient: server.Client(), Timeout: time.Minute})
	require.NoError(err)
	s.Kata = client
	_, err = s.Check(t.Context(), false)
	require.Error(err)
	_, err = s.Check(t.Context(), false)
	require.NoError(err)
	assert.Equal(int64(2), drafts.calls.Load())
	tasks, err := s.Kata.ListPersonTasksComplete(t.Context(), "person-a")
	require.NoError(err)
	assert.Len(tasks, 1)
}

func TestKeepInTouchForeignReceiptIsNotDelivery(t *testing.T) {
	require := Require.New(t)

	s := outreachFixture(t).Service
	result, err := s.Check(t.Context(), false)
	require.NoError(err)
	require.Len(result.Items, 1)
	client, ok := s.Kata.(*taskclient.KataClient)
	require.True(ok)
	issue, err := client.GetTask(t.Context(), "example", result.Items[0].Issue.UID)
	require.NoError(err)
	_, err = client.MutateMetadata(t.Context(), "example", issue.UID, issue.Revision, map[string]any{taskclient.OutreachDeliveryMetadataKey: "another-cycle:operator"})
	require.NoError(err)
	_, err = s.Check(t.Context(), false)
	Assert.Error(t, err, "a foreign receipt must not be counted as delivery of this cycle")
}

func TestKeepInTouchMovedReminderAndPersonAgenda(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, a, drafts, native, server := fixture.Service, fixture.Archive, fixture.Drafts, fixture.Native, fixture.Server
	result, err := s.Check(t.Context(), false)
	require.NoError(err)
	require.Len(result.Items, 1)
	client, ok := s.Kata.(*taskclient.KataClient)
	require.True(ok)
	agenda, err := (personagenda.Service{Tasks: client, People: a, Project: "example"}).List(t.Context(), 1)
	require.NoError(err)
	require.Len(agenda.Items, 1)
	assert.Equal(OutreachList, agenda.Items[0].List)
	target, err := native.Service.EnsureProject(t.Context(), kata.ProjectSpec{UID: "01HZNQ7VFPK1XGD8R5MABCD4EY", Name: "other"})
	require.NoError(err)
	task, err := client.GetTask(t.Context(), "example", result.Items[0].Issue.UID)
	require.NoError(err)
	native.Endpoint(server).MoveIssue(t, task.UID, task.Revision, target.Project.UID)
	previous := a.states[1]
	state := previous
	last := state.LastContactAt.AddDate(0, 0, 1)
	due := last.AddDate(0, 0, 21)
	state.LastContactAt = &last
	state.CadenceDueAt = &due
	a.states[1] = state
	_, err = s.Check(t.Context(), false)
	require.NoError(err)
	assert.Equal(int64(1), drafts.calls.Load(), "moved open reminder suppresses a new cycle")
	endpoint := native.Endpoint(server)
	endpoint.ProjectID = target.Project.ID
	endpoint.CloseIssue(t, task.UID)
	a.states[1] = previous
	_, err = s.Check(t.Context(), false)
	require.NoError(err)
	assert.Equal(int64(1), drafts.calls.Load(), "moved closed reminder still answers its cycle")
}

func TestKeepInTouchFailingPrefixDoesNotStarveNextRun(t *testing.T) {
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, a, drafts := fixture.Service, fixture.Archive, fixture.Drafts
	s.Config.MaxDraftsPerRun = 1
	drafts.err = errors.New("synthetic persistent refusal")
	drafts.failPerson = 1
	a.people = append(a.people, store.Person{ID: 2, VCardUID: "person-b"})
	a.aliases[2] = []string{"person-b"}
	a.states[2] = a.states[1]
	_, err := s.Check(t.Context(), false)
	require.Error(err)
	result, err := s.Check(t.Context(), false)
	require.NoError(err, "%+v", result.Failures)
	require.Len(result.Items, 1)
	Assert.Equal(t, int64(2), result.Items[0].PersonID)
}

func addOutreachPerson(a *outreachArchive, id int64) {
	uid := fmt.Sprintf("synthetic-person-%d", id)
	a.people = append(a.people, store.Person{ID: id, VCardUID: uid})
	a.aliases[id] = []string{uid}
	a.states[id] = a.states[1]
}
func TestKeepInTouchPendingDeliverySurvivesDraftCap(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, a, drafts := fixture.Service, fixture.Archive, fixture.Drafts
	addOutreachPerson(a, 2)
	s.Config.MaxDraftsPerRun = 1
	drafts.err = errors.New("synthetic refusal")
	drafts.failPerson = 1
	cycle := CycleKey("synthetic-archive", a.aliases[2][0], *a.states[2].LastContactAt)
	_, _, err := s.Kata.CreateTaskReused(t.Context(), s.Project, cycle, taskclient.KataCreate{Title: "Pending synthetic reminder", Body: "Original draft", Metadata: map[string]any{taskclient.ActionMetadataKey: cycle, KindMetadataKey: "synthetic-archive", OwnerMetadataKey: s.Config.Owner, EvidenceMetadataKey: []EvidenceSnapshot{}, DraftModeMetadataKey: "neutral_no_evidence", personagenda.PersonMetadataKey: a.aliases[2][0]}})
	require.NoError(err)
	result, err := s.Check(t.Context(), false)
	require.Error(err)
	require.Len(result.Items, 1)
	assert.Equal(int64(2), result.Items[0].PersonID)
	assert.Equal(1, result.DraftAttempts)
}
func TestKeepInTouchPagesPastOpenReminders(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, a, drafts := fixture.Service, fixture.Archive, fixture.Drafts
	for id := int64(2); id <= 101; id++ {
		addOutreachPerson(a, id)
	}
	for id := int64(1); id <= 100; id++ {
		_, _, err := s.Kata.CreateTaskReused(t.Context(), s.Project, fmt.Sprint("old-cycle-", id), taskclient.KataCreate{Title: "Existing synthetic reminder", Body: "Review", Metadata: map[string]any{KindMetadataKey: "synthetic-archive", personagenda.PersonMetadataKey: a.aliases[id][0]}})
		require.NoError(err, "create synthetic prefix reminder for person %d", id)
	}
	result, err := s.Check(t.Context(), false)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(int64(101), result.Items[0].PersonID)
	assert.Equal(int64(1), drafts.calls.Load())
}
func TestKeepInTouchLosesCreateResponseAfterCommit(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, drafts, native := fixture.Service, fixture.Drafts, fixture.Native
	var committedStatus atomic.Int64
	var drop atomic.Bool
	drop.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") && drop.Swap(false) {
			recorder := httptest.NewRecorder()
			native.Service.Handler().ServeHTTP(recorder, r)
			committedStatus.Store(int64(recorder.Code))
			http.Error(w, "synthetic lost response", http.StatusServiceUnavailable)
			return
		}
		native.Service.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: "example", HTTPClient: server.Client(), Timeout: time.Minute})
	require.NoError(err)
	s.Kata = client
	result, err := s.Check(t.Context(), false)
	require.NoError(err)
	require.Len(result.Items, 1)
	tasks, err := s.Kata.ListPersonTasksComplete(t.Context(), "person-a")
	require.NoError(err)
	require.Len(tasks, 1)
	assert.Less(committedStatus.Load(), int64(300))
	assert.Contains(tasks[0].Body, "Hi, how have you been?")
	assert.Equal(int64(1), drafts.calls.Load())
}
func TestKeepInTouchCadenceEditAndRunDateKeepClosedCycle(t *testing.T) {
	require := Require.New(t)

	fixture := outreachFixture(t)
	s, a, drafts, native, server := fixture.Service, fixture.Archive, fixture.Drafts, fixture.Native, fixture.Server
	result, err := s.Check(t.Context(), false)
	require.NoError(err)
	require.Len(result.Items, 1)
	native.Endpoint(server).CloseIssue(t, result.Items[0].Issue.UID)
	state := a.states[1]
	due := state.LastContactAt.AddDate(0, 0, 25)
	state.CadenceDueAt = &due
	a.states[1] = state
	now := s.Clock()
	s.Clock = func() time.Time { return now.AddDate(0, 0, 10) }
	_, err = s.Check(t.Context(), false)
	require.NoError(err)
	Assert.Equal(t, int64(1), drafts.calls.Load())
}
