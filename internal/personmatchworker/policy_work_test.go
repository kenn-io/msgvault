package personmatchworker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/personmatch"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestScoringColdPolicyChecksEachHTTPRequestAndPublication(t *testing.T) {
	for _, phase := range []string{"current", "retry_current", "before_policy", "before_input", "retry_policy", "retry_input", "commit_policy"} {
		t.Run(phase, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			st := storetest.New(t).Store
			_, err := st.ConfigureConversationPolicy(ctx, conversationpolicy.RecommendedSettings(), time.Now().UTC())
			requirements.NoError(err)
			left, err := st.EnsureParticipantByIdentifier("beeper", "policy-left", "Policy Left")
			requirements.NoError(err)
			right, err := st.EnsureParticipantByIdentifier("apple_id", "policy-right", "Policy Right")
			requirements.NoError(err)
			candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: left, RightKind: store.IdentityMatchParticipant, RightID: right, Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation})
			requirements.NoError(err)
			for i, kind := range []string{"email", "stable_provider_id"} {
				source, err := st.GetOrCreateSource("email", []string{"policy-first", "policy-second"}[i])
				requirements.NoError(err)
				_, err = st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: kind, Detail: new("policy@example.test"), Source: store.ProvenanceArchiveObservation, SourceID: &source.ID})
				requirements.NoError(err)
			}
			change := func() error {
				if strings.HasSuffix(phase, "input") {
					_, err := st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: "email", Detail: new("changed@example.test"), Source: store.ProvenanceUser})
					return err
				}
				settings := conversationpolicy.RecommendedSettings()
				settings.ActivityWindowDays++
				_, err := st.ConfigureConversationPolicy(ctx, settings, time.Now().UTC())
				return err
			}
			var workerStore Store = st
			if strings.HasPrefix(phase, "before_") {
				workerStore = revokeAfterClaimStore{Store: st, revoke: change}
			}
			var requests atomic.Int32
			changeErrors := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					if phase == "retry_current" {
						http.Error(w, "retry", http.StatusTooManyRequests)
						return
					}
					if phase == "retry_policy" || phase == "retry_input" || phase == "commit_policy" {
						changeErrors <- change()
						if phase != "commit_policy" {
							http.Error(w, "retry", http.StatusTooManyRequests)
							return
						}
					}
				}
				_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.81}}}`))
			}))
			defer server.Close()
			cfg := personmatch.Config{Enabled: true, ModelID: personmatch.ModelID, MinimumProbability: 0.80, CredentialEnv: "MSGVAULT_JEV_POLICY_FIXTURE", BatchSize: 2, RetentionDeclaration: "fixture retention"}
			t.Setenv(cfg.CredentialEnv, "synthetic-key")
			disclosure, err := cfg.Disclosure()
			requirements.NoError(err)
			_, _, err = st.GrantPersonMatchConsentContext(ctx, disclosure, "fixture_operator", nil)
			requirements.NoError(err)
			st.DB().SetMaxOpenConns(1)
			results, err := (Worker{Store: workerStore, Config: cfg, Endpoint: server.URL + "/v1/systemone", HTTPClient: server.Client()}).Run(ctx, 1)
			if phase == "retry_policy" || phase == "retry_input" || phase == "commit_policy" {
				select {
				case changeErr := <-changeErrors:
					requirements.NoError(changeErr)
				case <-ctx.Done():
					requirements.NoError(ctx.Err(), "provider mutation did not finish")
				}
			}
			if phase == "retry_policy" || phase == "commit_policy" || phase == "before_policy" {
				requirements.ErrorIs(err, store.ErrConversationPolicyStale)
			} else {
				requirements.NoError(err)
			}
			wantRequests := int32(1)
			if phase == "retry_current" {
				wantRequests = 2
			} else if strings.HasPrefix(phase, "before_") {
				wantRequests = 0
			}
			assertions.Equal(wantRequests, requests.Load(), "obsolete packet must not be disclosed")
			judgments, err := st.ListIdentityMatchJudgmentsContext(ctx, candidate.ID, 10)
			requirements.NoError(err)
			if phase == "current" || phase == "retry_current" {
				requirements.Len(results, 1)
				assertions.Equal("scored", results[0].Status)
				requirements.Len(judgments, 1)
				assertions.Equal("scored", judgments[0].Status)
			} else if strings.HasSuffix(phase, "input") {
				requirements.Len(results, 1)
				assertions.Equal("stale", results[0].Status)
				requirements.Len(judgments, 1)
				assertions.Equal("stale", judgments[0].Status)
				assertions.Nil(judgments[0].Probability)
			} else {
				assertions.Empty(judgments, "obsolete policy work cannot publish a judgment")
			}
		})
	}
}
