package kataissues_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
)

// A remote metadata value can be malformed. Inject that response boundary while
// keeping the real task, archive resolver and revision adapter in the fixture.
type nullAgreementTask struct {
	kataissues.Kata

	writes int
}

type agreementTestWriter interface {
	MutateMetadata(ctx context.Context, project, taskID, revision string, patch map[string]any) (taskclient.KataTask, error)
}

type lostAgreementResponse struct {
	kataissues.Kata

	writer agreementTestWriter
}

func (l lostAgreementResponse) MutateMetadata(ctx context.Context, project, id, revision string, patch map[string]any) (taskclient.KataTask, error) {
	_, err := l.writer.MutateMetadata(ctx, project, id, revision, patch)
	if err != nil {
		return taskclient.KataTask{}, err
	}
	return taskclient.KataTask{}, taskclient.ErrUnreachable
}

func TestAgreementLostResponseReplayPreservesLaterAcceptanceAfterClosure(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	writer, ok := f.service.Kata.(agreementTestWriter)
	must.True(ok)
	f.service.Kata = lostAgreementResponse{Kata: f.service.Kata, writer: writer}
	_, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.ErrorIs(err, taskclient.ErrUnreachable)
	f.service = f.connect(t)
	accept := linkedAgreementEvent(t, f, issue, "accept", "accept", "obligor", request.ID, "I will send it Friday.", 1)
	_, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.NoError(err)
	closeAgreementTask(t, f, issue)
	f.service = f.connect(t)
	f.service.Evidence = resolvedAs(kataevidence.Unavailable)
	view, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	check.True(view.Replayed)
	check.Equal(accept.ID, view.AcceptedEventID)
	check.Equal(request.Terms, view.Accepted)
	check.Len(view.History, 2)
}

func closeAgreementTask(t *testing.T, f *fixture, issue string) {
	t.Helper()
	must := require.New(t)

	body := `{"actor":"fixture","reason":"wontfix","message":"Synthetic agreement task closed for lifecycle verification; no real work or archive data is involved."}`
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", f.kata.projectID, strings.TrimPrefix(issue, "example#")), strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+katatest.Token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	f.kata.service.Handler().ServeHTTP(response, request)
	must.Equal(http.StatusOK, response.Code, response.Body.String())
}

func TestAgreementClosedTaskAllowsOnlyReadsAndExactRetry(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	_, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	accept := linkedAgreementEvent(t, f, issue, "accept", "accept", "obligor", request.ID, "I will send it Friday.", 1)
	closeAgreementTask(t, f, issue)
	_, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.ErrorIs(err, kataissues.ErrAgreementClosed)
	f.service.Evidence = resolvedAs(kataevidence.Unavailable)
	view, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	check.True(view.Replayed)
	view, err = f.service.Agreement(t.Context(), issue)
	must.NoError(err)
	check.Len(view.History, 1)
}

type concurrentAgreementEvidence struct {
	kataissues.Evidence

	ready *sync.WaitGroup
}

type beforeAgreementResolve struct {
	kataissues.Evidence

	before func()
}

func (e beforeAgreementResolve) ResolveAround(ctx context.Context, ref kataevidence.Reference, around int) (kataevidence.Resolution, error) {
	e.before()
	return e.Evidence.ResolveAround(ctx, ref, around)
}

func TestAgreementClosureAfterReadPreventsAppend(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	_, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	accept := linkedAgreementEvent(t, f, issue, "accept", "accept", "obligor", request.ID, "I will send it Friday.", 1)
	f.service.Evidence = beforeAgreementResolve{Evidence: f.service.Evidence, before: func() { closeAgreementTask(t, f, issue) }}
	_, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.ErrorIs(err, taskclient.ErrConflict)
	view, err := f.service.Agreement(t.Context(), issue)
	must.NoError(err)
	check.Len(view.History, 1)
	check.Nil(view.Accepted)
}

func (e concurrentAgreementEvidence) ResolveAround(ctx context.Context, ref kataevidence.Reference, around int) (kataevidence.Resolution, error) {
	// Both writers have fetched the same task snapshot before either writes.
	e.ready.Done()
	e.ready.Wait()
	return e.Evidence.ResolveAround(ctx, ref, around)
}

func TestAgreementConcurrentWritersCannotBothAppend(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	_, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	a := linkedAgreementEvent(t, f, issue, "accept-a", "accept", "obligor", request.ID, "I will send the budget Friday.", 1)
	b := linkedAgreementEvent(t, f, issue, "accept-b", "accept", "obligor", request.ID, "Friday is confirmed for the budget.", 1)
	var ready sync.WaitGroup
	ready.Add(2)
	f.service.Evidence = concurrentAgreementEvidence{Evidence: f.service.Evidence, ready: &ready}
	results := make(chan error, 2)
	for _, input := range []kataissues.AgreementInput{a, b} {
		go func() { _, err := f.service.RecordAgreement(t.Context(), issue, input); results <- err }()
	}
	first, second := <-results, <-results
	if first == nil {
		must.ErrorIs(second, taskclient.ErrConflict)
	} else {
		must.ErrorIs(first, taskclient.ErrConflict)
		must.NoError(second)
	}
	view, err := f.service.Agreement(t.Context(), issue)
	must.NoError(err)
	check.Len(view.History, 2)
}

func (n *nullAgreementTask) GetTask(ctx context.Context, project, id string) (taskclient.KataTask, error) {
	task, err := n.Kata.GetTask(ctx, project, id)
	if err == nil {
		task.Metadata[kataissues.AgreementMetadataKey] = nil
	}
	return task, err
}

func (n *nullAgreementTask) MutateMetadata(context.Context, string, string, string, map[string]any) (taskclient.KataTask, error) {
	n.writes++
	return taskclient.KataTask{}, taskclient.ErrRequestRejected
}

func TestAgreementPresentNullIsNotMissingHistory(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	remote := &nullAgreementTask{Kata: f.service.Kata}
	f.service.Kata = remote
	_, err := f.service.Agreement(t.Context(), issue)
	must.ErrorIs(err, kataissues.ErrUnsupportedAgreement)
	_, err = f.service.RecordAgreement(t.Context(), issue, request)
	must.ErrorIs(err, kataissues.ErrUnsupportedAgreement)
	check.Zero(remote.writes)
}

func agreementEvidence(t *testing.T, f *fixture, key, text string) kataevidence.Reference {
	t.Helper()
	must := require.New(t)

	id := f.store.CreateMessage(key)
	_, err := f.store.Store.DB().Exec(f.store.Store.Rebind("INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)"), id, text)
	must.NoError(err)
	prepared, err := kataevidence.New(f.store.Store).Prepare(t.Context(), []kataevidence.Selector{{Kind: "message", MessageID: id, Quote: text}})
	must.NoError(err)
	return prepared[0].Reference
}

func agreementRequest(t *testing.T, f *fixture) (string, kataissues.AgreementInput) {
	t.Helper()
	must := require.New(t)

	ref := agreementEvidence(t, f, "agreement-request", "Can you send the budget by Friday?")
	issue, err := f.service.Create(t.Context(), "agreement-task", kataissues.CreateInput{Title: "Budget agreement", Evidence: []kataevidence.Reference{ref}})
	must.NoError(err)
	return issue.Issue.QualifiedRef, kataissues.AgreementInput{ID: "request-1", Action: "request", Actor: "beneficiary", Reviewed: true,
		Terms: &kataissues.AgreementTerms{Text: "Send the budget", Due: "2026-11-06", Obligor: "Alex", Beneficiary: "Sam"}, Evidence: []kataevidence.Reference{ref}}
}

func linkedAgreementEvent(t *testing.T, f *fixture, issue, id, action, actor, target, quote string, count int) kataissues.AgreementInput {
	t.Helper()
	must := require.New(t)

	ref := agreementEvidence(t, f, id, quote)
	_, err := f.service.Link(t.Context(), issue, []kataevidence.Reference{ref})
	must.NoError(err)
	return kataissues.AgreementInput{ID: id, Action: action, Actor: actor, Target: target, ExpectedEvents: count, Reviewed: true, Evidence: []kataevidence.Reference{ref}}
}

func TestAgreementKeepsAcceptedTermsAcrossProposalsAndRestart(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	view, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	check.Nil(view.Accepted, "a request does not establish a commitment")
	accept := linkedAgreementEvent(t, f, issue, "accept-1", "accept", "obligor", request.ID, "Yes, I will send it by Friday.", 1)
	view, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.NoError(err)
	check.Equal(request.Terms, view.Accepted)
	proposal := linkedAgreementEvent(t, f, issue, "proposal-1", "propose", "obligor", accept.ID, "Could I send it Monday instead?", 2)
	replacement := *request.Terms
	replacement.Due = "2026-11-09"
	proposal.Terms = &replacement
	view, err = f.service.RecordAgreement(t.Context(), issue, proposal)
	must.NoError(err)
	check.Equal(request.Terms, view.Accepted, "proposed terms must not overwrite accepted terms")
	check.Equal([]string{proposal.ID}, view.Pending)
	// A new service reads only durable Kata state, with no in-memory history.
	f.service = f.connect(t)
	view, err = f.service.Agreement(t.Context(), issue)
	must.NoError(err)
	check.Equal(request.Terms, view.Accepted)
	check.Equal(accept.ID, view.AcceptedEventID)
	replace := linkedAgreementEvent(t, f, issue, "replace-1", "replace", "beneficiary", proposal.ID, "Monday works; let's use that date.", 3)
	view, err = f.service.RecordAgreement(t.Context(), issue, replace)
	must.NoError(err)
	check.Equal(&replacement, view.Accepted)
	check.Empty(view.Pending)
	must.Len(view.History, 4)
	check.Equal("2026-11-06", view.History[0].Input.Terms.Due)
	check.Equal("Can you send the budget by Friday?", view.History[0].Quotes[0])
	check.Equal("Could I send it Monday instead?", view.History[2].Quotes[0])
	check.Equal(replace.Evidence, view.History[3].Input.Evidence)
}

func TestAgreementRefusesSelfAcceptanceUnlinkedEvidenceAndStaleUpdates(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	_, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	accept := linkedAgreementEvent(t, f, issue, "accept", "accept", "beneficiary", request.ID, "Sounds good.", 1)
	_, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.ErrorIs(err, kataissues.ErrInvalidRequest, "a request author cannot accept for the other party")
	accept.Actor = "obligor"
	accept.Reviewed = false
	_, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.ErrorIs(err, kataissues.ErrInvalidRequest)
	accept.Reviewed = true
	accept.ExpectedEvents = 0
	_, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.ErrorIs(err, kataissues.ErrAgreementConflict)
	accept.ExpectedEvents = 1
	accept.Evidence = []kataevidence.Reference{agreementEvidence(t, f, "not-linked", "I will send the budget Friday.")}
	_, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.ErrorIs(err, kataissues.ErrAgreementEvidenceUnlinked)
	view, err := f.service.Agreement(t.Context(), issue)
	must.NoError(err)
	check.Len(view.History, 1)
	check.Nil(view.Accepted)
}

func TestAgreementRetryDoesNotNeedLiveSourceAndDetectsPayloadConflict(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	_, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	accept := linkedAgreementEvent(t, f, issue, "accept", "accept", "obligor", request.ID, "I will send it Friday.", 1)
	_, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.NoError(err)
	f.service = f.connect(t)
	f.service.Evidence = resolvedAs(kataevidence.Unavailable)
	view, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	check.True(view.Replayed)
	check.Len(view.History, 2, "retry returns the latest view without appending old input")
	request.Terms.Text = "Something else"
	_, err = f.service.RecordAgreement(t.Context(), issue, request)
	must.ErrorIs(err, kataissues.ErrAgreementConflict)
	view, err = f.service.Agreement(t.Context(), issue)
	must.NoError(err)
	check.Equal("Send the budget", view.Accepted.Text)
	check.Equal("I will send it Friday.", view.History[1].Quotes[0])
}

func TestAgreementSourceAndRevisionFailuresLeaveHistoryUnchanged(t *testing.T) {
	for _, failure := range []string{"changed", "revision", "foreign-project", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			must := require.New(t)

			f := newFixture(t)
			issue, request := agreementRequest(t, f)
			switch failure {
			case "changed":
				f.service.Evidence = resolvedAs(kataevidence.Changed)
			case "revision":
				f.kata.editFirst.Store(true)
			case "foreign-project":
				issue = "elsewhere#abcd"
			case "malformed":
				_, err := f.service.Kata.MutateMetadataKey(t.Context(), "example", strings.TrimPrefix(issue, "example#"), kataissues.AgreementMetadataKey, nil, map[string]any{"version": 2, "events": []any{}})
				must.NoError(err)
			}
			_, err := f.service.RecordAgreement(t.Context(), issue, request)
			must.Error(err)
			if failure == "revision" {
				must.ErrorIs(err, taskclient.ErrConflict)
			}
			if failure == "changed" {
				must.ErrorIs(err, kataevidence.ErrChanged)
			}
		})
	}
}

func TestAgreementHistoryRefusesUnknownOrInvalidStoredFields(t *testing.T) {
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	_, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	task, err := f.service.Kata.GetTask(t.Context(), "example", strings.TrimPrefix(issue, "example#"))
	must.NoError(err)
	encoded, err := json.Marshal(task.Metadata[kataissues.AgreementMetadataKey])
	must.NoError(err)
	var metadata map[string]any
	must.NoError(json.Unmarshal(encoded, &metadata))
	metadata["future"] = "do not silently drop this"
	_, err = f.service.Kata.MutateMetadataKey(t.Context(), "example", task.UID, kataissues.AgreementMetadataKey, task.Metadata[kataissues.AgreementMetadataKey], metadata)
	must.NoError(err)
	_, err = f.service.Agreement(t.Context(), issue)
	must.ErrorIs(err, kataissues.ErrUnsupportedAgreement)
}
