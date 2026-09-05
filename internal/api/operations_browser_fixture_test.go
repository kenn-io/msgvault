package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
)

type browserOperationPrivateSentinels struct {
	Name                  string `json:"name"`
	Address               string `json:"address"`
	Filename              string `json:"filename"`
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	Endpoint              string `json:"endpoint"`
	RawError              string `json:"raw_error"`
	ArchiveUID            string `json:"archive_uid"`
	DatabaseID            string `json:"database_id"`
	GenerationFingerprint string `json:"generation_fingerprint"`
	Credential            string `json:"credential"`
}

func TestBrowserOperationFixtureUsesProductionPublicProjection(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "tests", "e2e", "fixtures", "operations-public.json"))
	require.NoError(t, err)
	var fixture OperationRunsResponse
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Len(t, fixture.Runs, 5)

	archive := &operationArchiveStore{mockStore: &mockStore{}, uid: operationTestArchiveUID}
	codec := operationTokenCodec{
		keyring: archive,
		random:  bytes.NewReader(bytes.Repeat([]byte{0x24}, operationTokenNonceBytes*len(fixture.Runs))),
	}
	for index, browserRun := range fixture.Runs {
		t.Run(string(browserRun.Kind), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			run := browserFixtureRun(t, index, browserRun)
			require.NoError(run.Validate())

			projected, err := operationRunSummary(t.Context(), codec, run, operationTestArchiveUID)
			require.NoError(err)
			assert.Equal(browserRun, projected)

			decoded, err := codec.decodeRunReference(t.Context(), browserRun.ID, operationTestArchiveUID)
			require.NoError(err)
			assert.Equal(run.ID, decoded)
		})
	}
}

func loadBrowserOperationPrivateSentinels(t *testing.T) browserOperationPrivateSentinels {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "tests", "e2e", "fixtures", "operations-private-sentinels.json"))
	require.NoError(t, err)
	var sentinels browserOperationPrivateSentinels
	require.NoError(t, json.Unmarshal(raw, &sentinels))
	require.Len(t, sentinels.values(), 11)
	for _, sentinel := range sentinels.values() {
		require.NotEmpty(t, sentinel)
	}
	return sentinels
}

func (s browserOperationPrivateSentinels) values() []string {
	return []string{
		s.Name, s.Address, s.Filename, s.Provider, s.Model, s.Endpoint,
		s.RawError, s.ArchiveUID, s.DatabaseID, s.GenerationFingerprint, s.Credential,
	}
}

func (s browserOperationPrivateSentinels) numericDatabaseID(t *testing.T) int64 {
	t.Helper()

	id, err := strconv.ParseInt(s.DatabaseID, 10, 64)
	require.NoError(t, err)
	require.Positive(t, id)
	return id
}

func browserFixtureRun(t *testing.T, index int, summary OperationRunSummary) operations.Run {
	t.Helper()

	var (
		id  operations.StableID
		err error
	)
	if summary.Kind == operations.KindPersonSweep {
		id, err = operations.NewTextID(summary.Kind, "browser-person-sweep")
	} else {
		id, err = operations.NewInt64ID(summary.Kind, int64(index+101))
	}
	require.NoError(t, err)

	run := operations.Run{
		ID: id, Lane: summary.Lane, State: summary.State, Trigger: summary.Trigger,
		StartedAt: summary.StartedAt, FinishedAt: summary.FinishedAt,
		Counters: make([]operations.PublicCounter, 0, len(summary.Counters)),
	}
	for _, counter := range summary.Counters {
		run.Counters = append(run.Counters, operations.PublicCounter{
			Name: counter.Name, Unit: counter.Unit, Value: counter.Value,
		})
	}
	if summary.Error != nil {
		run.Error = &operations.PublicError{Code: summary.Error.Code, Message: summary.Error.Message}
	}
	return run
}
