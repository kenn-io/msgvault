package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

type probeToolCall struct {
	name string
	args map[string]any
}

type fakeCirclebackProbeSession struct {
	tools  []circleback.ToolInfo
	result json.RawMessage
	calls  []probeToolCall
}

func (f *fakeCirclebackProbeSession) ToolInventory(context.Context) ([]circleback.ToolInfo, error) {
	return f.tools, nil
}

func (f *fakeCirclebackProbeSession) CallToolJSON(_ context.Context, name string, args map[string]any) (json.RawMessage, error) {
	f.calls = append(f.calls, probeToolCall{name: name, args: args})
	return f.result, nil
}

func TestProbeCircleback_OfficialArgumentsAndSchemas(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	session := &fakeCirclebackProbeSession{
		tools: []circleback.ToolInfo{
			{
				Name:        "SearchMeetings",
				Description: "Search meeting metadata",
				InputSchema: json.RawMessage(`{
					"type":"object",
					"properties":{"intent":{"type":"string"},"pageIndex":{"type":"integer"}},
					"required":["intent","pageIndex"],
					"additionalProperties":false
				}`),
			},
		},
		result: json.RawMessage(`{"meetings":[]}`),
	}
	var out bytes.Buffer

	err := runCirclebackProbe(context.Background(), &out, session)

	require.NoError(err)
	require.Len(session.calls, 1, "probe must make exactly one sample call")
	call := session.calls[0]
	assert.Equal("SearchMeetings", call.name)
	assert.NotEmpty(call.args["intent"])
	assert.EqualValues(0, call.args["pageIndex"], "the probe must request the first zero-based page")
	assert.NotContains(call.args, "limit")
	assert.NotContains(call.args, "startDate")
	assert.NotContains(call.args, "endDate")
	assert.Contains(out.String(), "SearchMeetings")
	assert.Contains(out.String(), "Search meeting metadata")
	assert.Contains(out.String(), `"additionalProperties": false`)
	assert.Contains(out.String(), `"pageIndex"`)
	assert.Contains(out.String(), `{"meetings":[]}`)
}

func TestCirclebackLimitHelpExplainsMaintenanceItems(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	limit := syncCirclebackCmd.Flags().Lookup("limit")
	require.NotNil(limit)

	assert.Contains(limit.Usage, "newly searched meetings")
	assert.Contains(limit.Usage, "maintenance items")
	assert.Contains(limit.Usage, "unlimited run")
	assert.Contains(syncCirclebackCmd.Long, "may revisit")
	assert.Contains(syncCirclebackCmd.Long, "unlimited run")
}

func TestCirclebackSummaryReportsMaintenanceItems(t *testing.T) {
	assert := assert.New(t)
	var out bytes.Buffer

	writeCirclebackSummary(&out, &circleback.ImportSummary{
		MeetingsProcessed:  7,
		MeetingsAdded:      3,
		MaintenanceRetries: 2,
	})

	assert.Contains(out.String(), "Meetings processed: 7")
	assert.Contains(out.String(), "Maintenance items: 2")
	assert.NotContains(out.String(), "Transcript retries")
	assert.Contains(out.String(), "outside --limit")
}

func TestFinishCirclebackImportRefreshesOnlyAfterCommittedWrites(t *testing.T) {
	tests := []struct {
		name          string
		cancelContext bool
		sum           *circleback.ImportSummary
		importErr     error
		wantRefreshes int
		wantError     string
	}{
		{
			name:          "cancellation after write",
			cancelContext: true,
			sum:           &circleback.ImportSummary{MeetingsAdded: 1},
			importErr:     context.Canceled,
			wantRefreshes: 1,
			wantError:     "canceled",
		},
		{
			name:          "hard error after write",
			sum:           &circleback.ImportSummary{MeetingsUpdated: 1},
			importErr:     errors.New("provider failed"),
			wantRefreshes: 1,
			wantError:     "failed",
		},
		{
			name:          "cancellation before write",
			cancelContext: true,
			sum:           &circleback.ImportSummary{},
			importErr:     context.Canceled,
			wantError:     "canceled",
		},
		{
			name:      "hard error before write",
			sum:       &circleback.ImportSummary{},
			importErr: errors.New("provider failed"),
			wantError: "failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelContext {
				cancel()
			}
			refreshes := 0

			err := finishCirclebackImport(ctx, "alice@example.com", tc.sum, tc.importErr, func() {
				refreshes++
			})

			require.Error(err)
			if tc.cancelContext {
				require.ErrorIs(err, context.Canceled)
			}
			assert.Equal(tc.wantRefreshes, refreshes)
			assert.Contains(err.Error(), "circleback sync alice@example.com")
			assert.Contains(err.Error(), tc.wantError)
		})
	}
}

func TestFinishCirclebackImportRefreshesEarlierSourceWritesOnLaterFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	total := &circleback.ImportSummary{}
	accumulateCirclebackWrites(total, &circleback.ImportSummary{MeetingsAdded: 2})
	accumulateCirclebackWrites(total, &circleback.ImportSummary{MeetingsUpdated: 1})
	refreshes := 0

	err := finishCirclebackImport(context.Background(), "second", total, errors.New("connect failed"), func() {
		refreshes++
	})

	require.ErrorContains(err, "circleback sync second failed")
	assert.EqualValues(2, total.MeetingsAdded)
	assert.EqualValues(1, total.MeetingsUpdated)
	assert.Equal(1, refreshes, "a later source failure must refresh writes committed by earlier sources")
}

func TestFinishScheduledCirclebackImportUsesDetachedRefreshContext(t *testing.T) {
	hardErr := errors.New("scheduled provider failed")
	tests := []struct {
		name      string
		importErr error
		cancelNow bool
		wantError error
	}{
		{name: "clean import", cancelNow: false},
		{name: "hard-error partial import", importErr: hardErr, wantError: hardErr},
		{name: "canceled partial import", importErr: context.Canceled, cancelNow: true, wantError: context.Canceled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelNow {
				cancel()
			}
			refreshes := 0
			var refreshContextErr error

			err := finishScheduledCirclebackImport(
				ctx,
				"work",
				&circleback.ImportSummary{MeetingsAdded: 1},
				tc.importErr,
				func(refreshCtx context.Context, identifier string) {
					refreshes++
					cancel()
					refreshContextErr = refreshCtx.Err()
					assert.Equal("circleback:work", identifier)
				},
			)

			if tc.wantError != nil {
				require.ErrorIs(err, tc.wantError)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, refreshes)
			assert.NoError(refreshContextErr, "scheduled cache refresh must outlive cancellation of the sync context")
		})
	}
}

func TestConfiguredCirclebackMissingRegisteredSourceStopsBeforeConnect(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runConfiguredCirclebackSync(ctx, st, config.CirclebackSource{
		Identifier: "work", AccountEmail: "user-a@example.com",
	})

	require.Error(err)
	assert.Contains(err.Error(), "run msgvault add-circleback work")
	sources, listErr := st.ListSources(circleback.SourceType)
	require.NoError(listErr)
	assert.Empty(sources, "scheduled sync must not create an unregistered source")
}

func TestAddCirclebackIdentityConfirmsPrimaryWhenAliasExists(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeCircleback, "work")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "user-b@example.com", "manual"))
	var out bytes.Buffer

	registered, err := registerMeetingSource(&out, st, sourceTypeCircleback, "work", " User-Z@Example.COM ")

	require.NoError(err)
	assert.Equal(source.ID, registered.ID)
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 2)
	assert.Equal("user-b@example.com", identities[0].Address)
	assert.Equal("user-z@example.com", identities[1].Address)
	assert.Contains(out.String(), "Confirmed identity user-z@example.com")
	assert.Contains(out.String(), "sync-circleback work --full")
	assert.Nil(newAddCirclebackLocalCmd().Flags().Lookup("no-default-identity"),
		"meeting identity confirmation is mandatory")
}

func TestAddCirclebackConfiguredRemoteRejectsHostLocalOAuthBeforeProxy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)
	cfg.Circleback = []config.CirclebackSource{{
		Identifier:   "work",
		AccountEmail: "user-a@example.com",
	}}
	cmd := newAddCirclebackCmd()
	cmd.SetArgs([]string{"work"})

	err := cmd.Execute()

	require.Error(err)
	assert.Contains(err.Error(), "localhost OAuth callback")
	assert.Contains(err.Error(), "daemon host")
	assert.Contains(err.Error(), "--local")
	assert.Zero(int(requests.Load()), "routing error must happen before any daemon proxy request")
}

func TestAddCirclebackLocalOverrideAllowsHostLocalOAuth(t *testing.T) {
	savedCfg, savedUseLocal := cfg, useLocal
	t.Cleanup(func() {
		cfg = savedCfg
		useLocal = savedUseLocal
	})
	cfg = &config.Config{Remote: config.RemoteConfig{URL: "https://remote.example.com"}}
	useLocal = true

	require.NoError(t, validateAddCirclebackOAuthRouting())
}
