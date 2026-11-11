package personenrichment_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
)

func TestWorkerColdPolicyRetainsOriginalWorkThroughDispatchAndCommit(t *testing.T) {
	for _, phase := range []string{"current", "dispatch", "commit"} {
		t.Run(phase, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newWorkerFixture(t, "cold-policy-"+phase, nil)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			_, err := f.store.ConfigureConversationPolicy(ctx, conversationpolicy.RecommendedSettings(), time.Now().UTC())
			requirements.NoError(err)
			f.enqueue(t)
			f.store.DB().SetMaxOpenConns(1)
			change := func() {
				before, err := f.store.GetPersonContext(ctx, f.person.ID)
				require.NoError(t, err)
				settings := conversationpolicy.RecommendedSettings()
				settings.ActivityWindowDays++
				_, err = f.store.ConfigureConversationPolicy(ctx, settings, time.Now().UTC())
				require.NoError(t, err)
				after, err := f.store.GetPersonContext(ctx, f.person.ID)
				require.NoError(t, err)
				require.Equal(t, before.Revision, after.Revision, "epoch guard is independent of durable person revision")
			}
			var calls atomic.Int32
			factory := func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
				if phase == "dispatch" {
					change()
				}
				return &functionProvider{start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
					calls.Add(1)
					result := workerResult(t, request, f.target, "policy-request", "", false, "", personenrichment.Cost{})
					if phase == "commit" {
						change()
					}
					return personenrichment.Attempt{State: personenrichment.AttemptComplete, RequestID: result.RequestID, AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion, ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result}, nil
				}}, nil
			}
			worker := f.newWorker(t, map[string]personenrichment.ProviderFactory{f.config.Name: factory}, map[string]personenrichment.ProviderConfig{f.config.Name: f.config}, func(string) (string, bool) { return "synthetic-key", true })
			processed, err := worker.RunOnce(ctx, f.run.ID)
			assertions.True(processed)
			if phase == "current" {
				requirements.NoError(err)
			} else {
				requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			}
			wantCalls := int32(1)
			if phase == "dispatch" {
				wantCalls = 0
			}
			assertions.Equal(wantCalls, calls.Load())
			var generations int
			requirements.NoError(f.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_generations`).Scan(&generations))
			if phase == "current" {
				assertions.Equal(1, generations)
			} else {
				assertions.Zero(generations)
			}
		})
	}
}

func TestWorkerColdPolicyResumedHTTPJobKeepsDurableAuthority(t *testing.T) {
	for _, phase := range []string{"current", "before_poll", "restored", "input_edit"} {
		t.Run(phase, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			f := newWorkerFixture(t, "resumed-policy-"+phase, nil)
			_, err := f.store.ConfigureConversationPolicy(ctx, conversationpolicy.RecommendedSettings(), time.Now().UTC())
			requirements.NoError(err)
			organization, err := f.store.CreateOrganizationContext(ctx, store.OrganizationInput{Name: "Example Labs"})
			requirements.NoError(err)
			_, err = f.store.AddEmploymentContext(ctx, store.EmploymentInput{PersonID: f.person.ID, OrganizationID: organization.ID, IsCurrent: new(true), Source: store.ProvenanceUser})
			requirements.NoError(err)
			source, err := f.store.GetOrCreateSource("slack", "resumed-policy")
			requirements.NoError(err)
			conversation, err := f.store.EnsureConversationWithType(source.ID, "resumed-policy", "direct_chat", "Synthetic policy chat")
			requirements.NoError(err)
			requirements.NoError(f.store.SetConversationPolicyOverride(ctx, source.ID, "resumed-policy", conversationpolicy.Full))
			var participant int64
			requirements.NoError(f.store.DB().QueryRowContext(ctx, f.store.Rebind("SELECT participant_id FROM person_participants WHERE person_id=?"), f.person.ID).Scan(&participant))
			_, err = f.store.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "policy-job-input", ConversationID: conversation, MessageType: "chat", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
			requirements.NoError(err)
			catalog, err := f.store.BuildPersonFactCatalogContext(ctx, true)
			requirements.NoError(err)
			startBody := sixtyfourFixture(t, "sixtyfour_start.json")
			completeBody, err := json.Marshal(map[string]any{"task_id": "opaque-job-42", "status": "completed", "charge_amount": 12, "result": map[string]any{"structured_data": map[string]any{f.target.Key: "Synthetic profile fact", "name": "Worker Person", "company": "Example Labs"}, "confidence_score": 9, "findings": []any{}}})
			requirements.NoError(err)
			var starts, polls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					n := starts.Add(1)
					body := startBody
					if n > 1 {
						body = bytes.ReplaceAll(startBody, []byte("opaque-job-42"), []byte("fresh-job-43"))
					}
					_, _ = w.Write(body)
				} else {
					polls.Add(1)
					_, _ = w.Write(completeBody)
				}
			}))
			defer server.Close()
			f.config = sixtyfourConfig(server.URL+"/start", server.URL+"/job-status")
			f.config.TargetKeys = []string{f.target.Key}
			f.config.RequestTimeout = 30 * time.Second
			f.profile, err = f.config.Profile(catalog)
			requirements.NoError(err)
			_, err = f.store.EnsurePersonEnrichmentProfile(ctx, f.profile)
			requirements.NoError(err)
			_, _, err = f.store.GrantPersonEnrichmentConsent(ctx, f.profile.Fingerprint, "test")
			requirements.NoError(err)
			now := time.Now().UTC()
			options := f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config})
			options.Clock = func() time.Time { return now }
			resumed := false
			factory := func(cfg personenrichment.ProviderConfig, key string) (personenrichment.Provider, error) {
				if resumed && phase == "before_poll" {
					settings := conversationpolicy.RecommendedSettings()
					settings.ActivityWindowDays++
					_, err := f.store.ConfigureConversationPolicy(ctx, settings, time.Now().UTC())
					require.NoError(t, err)
				}
				return personenrichment.NewSixtyfourProvider(cfg, key, server.Client())
			}
			newWorker := func() *personenrichment.Worker {
				worker, err := personenrichment.NewWorker(f.store, f.store, f.gate(t, func(string) (string, bool) { return "synthetic-key", true }), map[string]personenrichment.ProviderFactory{f.config.Name: factory}, options)
				require.NoError(t, err)
				return worker
			}
			f.store.DB().SetMaxOpenConns(1)
			processed, err := newWorker().RunOnce(ctx, f.run.ID)
			requirements.NoError(err)
			requirements.True(processed)
			requirements.Equal(int32(1), starts.Load())
			requirements.Zero(polls.Load())
			work, err := f.store.ListPersonEnrichmentWorkContext(ctx, store.PersonEnrichmentWorkFilter{PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10})
			requirements.NoError(err)
			requirements.Len(work, 1)
			attempts, err := f.store.ListPersonEnrichmentAttemptsContext(ctx, store.PersonEnrichmentAttemptFilter{PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10})
			requirements.NoError(err)
			requirements.Len(attempts, 1)
			original := attempts[0]
			before, err := f.store.GetPersonContext(ctx, f.person.ID)
			requirements.NoError(err)
			switch phase {
			case "restored":
				requirements.NoError(f.store.SetConversationPolicyOverride(ctx, source.ID, "resumed-policy", conversationpolicy.Cold))
				requirements.NoError(f.store.SetConversationPolicyOverride(ctx, source.ID, "resumed-policy", conversationpolicy.Full))
			case "input_edit":
				_, err = f.store.UpdatePersonDisplayNameContext(ctx, f.person.ID, before.Revision, new("Changed Person"))
				requirements.NoError(err)
			}
			if phase == "restored" || phase == "input_edit" {
				after, err := f.store.GetPersonContext(ctx, f.person.ID)
				requirements.NoError(err)
				requirements.Greater(after.Revision, before.Revision)
			}
			now = work[0].DueAt.Add(time.Second)
			resumed = true
			processed, err = newWorker().RunOnce(ctx, f.run.ID)
			if phase == "before_poll" {
				requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			} else {
				requirements.NoError(err)
			}
			assertions.True(processed)
			wantStarts := int32(1)
			if phase == "restored" || phase == "input_edit" {
				wantStarts = 2
			}
			assertions.Equal(wantStarts, starts.Load())
			attempts, err = f.store.ListPersonEnrichmentAttemptsContext(ctx, store.PersonEnrichmentAttemptFilter{PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10})
			requirements.NoError(err)
			requirements.Len(attempts, int(wantStarts))
			if len(attempts) == 2 && attempts[0].ID != original.ID {
				attempts[0], attempts[1] = attempts[1], attempts[0]
			}
			assertions.Equal(original.ID, attempts[0].ID)
			assertions.Equal(original.RequestHash, attempts[0].RequestHash)
			assertions.Equal(original.PayloadHash, attempts[0].PayloadHash)
			assertions.Equal(original.PersonRevision, attempts[0].PersonRevision)
			if phase == "current" {
				assertions.Equal(int32(1), polls.Load())
				assertions.Equal("succeeded", attempts[0].State)
				assertions.NotNil(attempts[0].FactGenerationKey)
			} else {
				assertions.Zero(polls.Load())
				assertions.NotEqual("succeeded", attempts[0].State)
				assertions.Nil(attempts[0].FactGenerationKey)
			}
		})
	}
}
