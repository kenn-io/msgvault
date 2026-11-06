package kataissues_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agreements"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/taskclient"
)

type budgetAgreementTask struct {
	kataissues.Kata

	writer    agreementTestWriter
	bodyBytes int
	writes    int
}

func (b *budgetAgreementTask) GetTask(ctx context.Context, project, id string) (taskclient.KataTask, error) {
	task, err := b.Kata.GetTask(ctx, project, id)
	task.Body = strings.Repeat("x", b.bodyBytes)
	return task, err
}

func (b *budgetAgreementTask) MutateMetadata(ctx context.Context, project, id, revision string, patch map[string]any) (taskclient.KataTask, error) {
	b.writes++
	return b.writer.MutateMetadata(ctx, project, id, revision, patch)
}

func TestAgreementFullTaskByteBudgetBoundary(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		t.Run(strconv.Itoa(delta), func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)

			f := newFixture(t)
			issue, request := agreementRequest(t, f)
			task, err := f.service.Kata.GetTask(t.Context(), "example", strings.TrimPrefix(issue, "example#"))
			must.NoError(err)
			resolved, err := f.service.Evidence.ResolveAround(t.Context(), request.Evidence[0], 0)
			must.NoError(err)
			preview := task
			preview.Body = ""
			preview.Metadata = maps.Clone(task.Metadata)
			preview.Metadata[kataissues.AgreementMetadataKey] = agreements.Envelope{Version: 1, Events: []agreements.Event{{Input: request, Quotes: []string{resolved.Evidence.Excerpt}}}}
			encoded, err := json.Marshal(preview)
			must.NoError(err)
			writer, ok := f.service.Kata.(agreementTestWriter)
			must.True(ok)
			// Inject a large but valid remote task response while retaining the
			// real archive, linked evidence and native guarded metadata writer.
			remote := &budgetAgreementTask{Kata: f.service.Kata, writer: writer, bodyBytes: (512 << 10) - len(encoded) + delta}
			f.service.Kata = remote
			_, err = f.service.RecordAgreement(t.Context(), issue, request)
			if delta > 0 {
				must.ErrorIs(err, kataissues.ErrAgreementFull)
				check.Zero(remote.writes)
			} else {
				must.NoError(err)
				check.Equal(1, remote.writes)
			}
		})
	}
}

func TestAgreementExactReplayAtCapacitySkipsCurrentSourceAndTaskBudgets(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	_, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	accept := linkedAgreementEvent(t, f, issue, "a", "accept", "obligor", request.ID, "I will send it Friday.", 1)
	_, err = f.service.RecordAgreement(t.Context(), issue, accept)
	must.NoError(err)
	for count := 2; count < agreements.MaxEvents; count++ {
		proposal := kataissues.AgreementInput{ID: fmt.Sprintf("p%d", count), Action: "propose", Actor: "obligor", Target: "a", ExpectedEvents: count, Reviewed: true, Terms: request.Terms, Evidence: request.Evidence}
		_, err = f.service.RecordAgreement(t.Context(), issue, proposal)
		must.NoError(err)
	}
	writer, ok := f.service.Kata.(agreementTestWriter)
	must.True(ok)
	remote := &budgetAgreementTask{Kata: f.service.Kata, writer: writer, bodyBytes: 600 << 10}
	f.service.Kata = remote
	f.service.Evidence = resolvedAs(kataevidence.Unavailable)
	view, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	check.True(view.Replayed)
	check.Len(view.History, agreements.MaxEvents)
	check.Equal(request.Terms, view.Accepted)
	check.Zero(remote.writes)
}

func TestAgreementReplayComparesEveryOriginalInputField(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := newFixture(t)
	issue, request := agreementRequest(t, f)
	contextRef := agreementEvidence(t, f, "context", "The budget goes to the planning group.")
	_, err := f.service.Link(t.Context(), issue, []kataevidence.Reference{contextRef})
	must.NoError(err)
	request.Evidence = append(request.Evidence, contextRef)
	_, err = f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	encoded, err := json.Marshal(request)
	must.NoError(err)
	f.service.Evidence = resolvedAs(kataevidence.Unavailable)
	for _, tc := range []struct {
		name   string
		change func(*kataissues.AgreementInput)
	}{
		{"actor", func(in *kataissues.AgreementInput) { in.Actor = "obligor" }},
		{"count", func(in *kataissues.AgreementInput) { in.ExpectedEvents = 1 }},
		{"source version", func(in *kataissues.AgreementInput) { in.Evidence[0].Message.BodySHA256 = strings.Repeat("f", 64) }},
		{"reference order", func(in *kataissues.AgreementInput) { in.Evidence[0], in.Evidence[1] = in.Evidence[1], in.Evidence[0] }},
		{"due", func(in *kataissues.AgreementInput) { in.Terms.Due = "2026-11-09" }},
		{"party", func(in *kataissues.AgreementInput) { in.Terms.Obligor = "Taylor" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)

			var changed kataissues.AgreementInput
			must.NoError(json.Unmarshal(encoded, &changed))
			tc.change(&changed)
			_, err := f.service.RecordAgreement(t.Context(), issue, changed)
			must.ErrorIs(err, kataissues.ErrAgreementConflict)
		})
	}
	view, err := f.service.RecordAgreement(t.Context(), issue, request)
	must.NoError(err)
	check.True(view.Replayed)
	check.Len(view.History, 1)
}
