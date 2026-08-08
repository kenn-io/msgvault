package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const concurrentInsertCalls = 8

func TestPostgreSQLConcurrentParticipantObservationInsertIsIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL concurrency regression")
	}
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier(
		"example", "participant-1", "Test User",
	)
	require.NoError(err)
	installPostgreSQLInsertDelay(t, st, "participant_contact_observations", "delay_observation_insert")

	input := store.ParticipantContactObservationInput{
		AddressKind:   store.ContactAddressEmail,
		OriginalValue: "user@example.com",
		Envelope:      store.ValueEnvelope{Source: store.ProvenanceArchiveObservation},
	}
	type callResult struct {
		result *store.RecordContactObservationResult
		err    error
	}
	results := make([]callResult, concurrentInsertCalls)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range results {
		wait.Go(func() {
			<-start
			results[i].result, results[i].err = st.RecordContactObservationContext(
				ctx, participantID, input,
			)
		})
	}
	close(start)
	wait.Wait()

	created := 0
	ids := make(map[int64]struct{})
	for _, result := range results {
		require.NoError(result.err)
		require.NotNil(result.result)
		if result.result.Created {
			created++
		}
		ids[result.result.Observation.Envelope.ID] = struct{}{}
	}
	assert.Equal(1, created)
	assert.Len(ids, 1)
	assert.Equal(1, tableRowCount(t, st, "participant_contact_observations"))
}

func TestPostgreSQLConcurrentIdentityMatchCandidateInsertIsIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL concurrency regression")
	}
	ctx := context.Background()
	leftID, err := st.EnsureParticipantByIdentifier("example", "left", "Left User")
	require.NoError(err)
	rightID, err := st.EnsureParticipantByIdentifier("example", "right", "Right User")
	require.NoError(err)
	installPostgreSQLInsertDelay(t, st, "identity_match_candidates", "delay_candidate_insert")

	input := store.IdentityMatchCandidateInput{
		LeftKind:  store.IdentityMatchParticipant,
		LeftID:    leftID,
		RightKind: store.IdentityMatchParticipant,
		RightID:   rightID,
		Basis:     store.IdentityMatchEmail,
		State:     store.IdentityMatchStateCandidate,
		Source:    store.ProvenanceArchiveObservation,
	}
	type callResult struct {
		candidate *store.IdentityMatchCandidate
		created   bool
		err       error
	}
	results := make([]callResult, concurrentInsertCalls)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range results {
		wait.Go(func() {
			<-start
			results[i].candidate, results[i].created, results[i].err =
				st.UpsertIdentityMatchCandidateContext(ctx, input)
		})
	}
	close(start)
	wait.Wait()

	created := 0
	ids := make(map[int64]struct{})
	for _, result := range results {
		require.NoError(result.err)
		require.NotNil(result.candidate)
		if result.created {
			created++
		}
		ids[result.candidate.ID] = struct{}{}
	}
	assert.Equal(1, created)
	assert.Len(ids, 1)
	assert.Equal(1, tableRowCount(t, st, "identity_match_candidates"))
}

func TestPostgreSQLConcurrentCommunicationServiceSlugIsIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL concurrency regression")
	}
	installPostgreSQLInsertDelay(t, st, "communication_services", "delay_service_insert")

	input := store.CommunicationServiceInput{
		Slug: "example-bridge", DisplayLabel: "Example Bridge",
		ScopePolicy:   store.ScopePolicyNone,
		Normalization: store.NormalizationLower, NormalizationVersion: 1,
	}
	type callResult struct {
		service *store.CommunicationService
		created bool
		err     error
	}
	results := make([]callResult, concurrentInsertCalls)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range results {
		wait.Go(func() {
			<-start
			results[i].service, results[i].created, results[i].err =
				st.EnsureCommunicationServiceContext(t.Context(), input)
		})
	}
	close(start)
	wait.Wait()

	created := 0
	ids := make(map[int64]struct{})
	for _, result := range results {
		require.NoError(result.err)
		require.NotNil(result.service)
		if result.created {
			created++
		}
		ids[result.service.ID] = struct{}{}
	}
	assert.Equal(1, created)
	assert.Len(ids, 1)
	assert.Equal(1, serviceSlugCount(t, st, "example-bridge"))
}

func TestPostgreSQLConcurrentCommunicationServiceAliasClaimHasStableConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL concurrency regression")
	}
	installPostgreSQLInsertDelay(t, st, "communication_service_aliases", "delay_service_alias_insert")

	inputs := []store.CommunicationServiceInput{
		{
			Slug: "example-bridge-left", DisplayLabel: "Example Bridge Left",
			Aliases: []string{"shared-example-alias"}, ScopePolicy: store.ScopePolicyNone,
			Normalization: store.NormalizationLower, NormalizationVersion: 1,
		},
		{
			Slug: "example-bridge-right", DisplayLabel: "Example Bridge Right",
			Aliases: []string{"shared-example-alias"}, ScopePolicy: store.ScopePolicyNone,
			Normalization: store.NormalizationLower, NormalizationVersion: 1,
		},
	}
	type callResult struct {
		service *store.CommunicationService
		err     error
	}
	results := make([]callResult, len(inputs))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range inputs {
		wait.Go(func() {
			<-start
			results[i].service, _, results[i].err =
				st.EnsureCommunicationServiceContext(t.Context(), inputs[i])
		})
	}
	close(start)
	wait.Wait()

	succeeded := 0
	conflicted := 0
	for _, result := range results {
		if result.err == nil {
			succeeded++
			require.NotNil(result.service)
			continue
		}
		if assert.ErrorIs(result.err, store.ErrServiceAliasConflict) {
			conflicted++
		}
	}
	assert.Equal(1, succeeded)
	assert.Equal(1, conflicted)
	resolved, err := st.ResolveCommunicationServiceContext(t.Context(), "shared-example-alias")
	require.NoError(err)
	assert.Contains([]string{"example-bridge-left", "example-bridge-right"}, resolved.Slug)
}

func installPostgreSQLInsertDelay(t *testing.T, st *store.Store, table, trigger string) {
	t.Helper()
	function := trigger + "_fn"
	_, err := st.DB().ExecContext(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_sleep(0.5);
			RETURN NEW;
		END
		$$;
		CREATE TRIGGER %s BEFORE INSERT ON %s
		FOR EACH ROW EXECUTE FUNCTION %s()`, function, trigger, table, function))
	require.NoError(t, err)
}

func tableRowCount(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM "+table,
	).Scan(&count))
	return count
}

func serviceSlugCount(t *testing.T, st *store.Store, slug string) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
		"SELECT COUNT(*) FROM communication_services WHERE slug = ?"), slug,
	).Scan(&count))
	return count
}
