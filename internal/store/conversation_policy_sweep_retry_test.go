package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func TestPeopleWorkersColdPolicyChecksEveryHTTPRepair(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "changed_input"}[changed], func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newBriefWorkerEndToEndFixture(t, "http-repair")
			st := f.journal.store
			st.DB().SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			var calls atomic.Int32
			mutation := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				candidate := `{"claims":[]}`
				if call == 1 {
					candidate = `{}` // Real schema validation must request a repair.
					if changed {
						_, err := st.DB().ExecContext(ctx, `UPDATE message_bodies SET body_text='Changed synthetic retry input'`)
						mutation <- err
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"model": "gpt-test", "choices": []any{map[string]any{"message": map[string]any{"content": candidate}}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
			}))
			defer server.Close()
			provider := f.config.Providers["default"]
			provider.Endpoint = server.URL + "/v1"
			provider.RequestTimeout = 30 * time.Second
			f.config.Providers["default"] = provider
			profile, err := f.config.Profile()
			requirements.NoError(err)
			_, err = st.EnsurePersonInferenceProfile(ctx, profile)
			requirements.NoError(err)
			_, _, err = st.GrantPersonInferenceConsent(ctx, profile.Fingerprint, "test-owner")
			requirements.NoError(err)
			requirements.NoError(st.RecordPersonInferenceCheck(ctx, store.PersonInferenceCheck{ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now().UTC(), DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ModelVersion: "gpt-test"}))
			registry, err := peoplesweep.NewDriverRegistry(server.Client(), nil, nil)
			requirements.NoError(err)
			runner, err := peoplesweep.NewRunner(f.config, st, registry, peoplesweep.NewCredentialResolver(nil, func(string) (string, bool) { return "synthetic-key", true }))
			requirements.NoError(err)
			f.worker.Config = f.config
			f.worker.Runner = runner
			result, err := f.worker.Run(ctx, peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeSkip})
			if changed {
				requirements.ErrorIs(err, store.ErrConversationPolicyStale)
				requirements.NoError(<-mutation)
				assertions.Equal(int32(1), calls.Load())
				assertions.Zero(result.PeopleSucceeded)
			} else {
				requirements.NoError(err)
				assertions.Equal(int32(2), calls.Load(), "current source permits the real schema repair")
				assertions.Equal(1, result.PeopleSucceeded)
			}
		})
	}
}
