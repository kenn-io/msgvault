package meetingimport

import (
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func configureNativeMeetingImports(t *testing.T, st *store.Store, global, available bool) {
	t.Helper()
	cfg := store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-meeting-import", ImportNotifications: global}
	if available {
		cfg.Capabilities = []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}
	}
	_, err := st.ConfigureMCPEvents(t.Context(), cfg)
	Require.NoError(t, err)
}

func TestNativeMeetingImportRequiresBothNotificationChoices(t *testing.T) {
	for _, tc := range []struct{ global, run bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		name := "silent"
		if tc.global {
			name = "global_" + name
		}
		if tc.run {
			name += "_requested"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureNativeMeetingImports(t, st, tc.global, true)
			req := validImportRequest(t)
			req.NotifyImports = tc.run
			result, err := NewImporter(st, Hooks{}).Import(t.Context(), req)
			if tc.run && !tc.global {
				require.Error(err)
				for _, table := range []string{"sources", "account_identities", "participants", "conversations", "messages", "sync_runs", "import_runs", "mcp_event_log"} {
					var n int
					require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
					assert.Zero(n, "unavailable notification request must fail before "+table)
				}
				return
			}
			require.NoError(err)
			require.NotEmpty(result.ImportRunID)
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			want := 0
			if tc.global && tc.run {
				want = 1
			}
			assert.Equal(want, events)
		})
	}
}

func TestNativeMeetingUnavailableNotificationConsumerRejectsBeforeWrites(t *testing.T) {
	st := testutil.NewTestStore(t)
	configureNativeMeetingImports(t, st, true, false)
	req := validImportRequest(t)
	req.NotifyImports = true
	_, err := NewImporter(st, Hooks{}).Import(t.Context(), req)
	Require.Error(t, err)
	for _, table := range []string{"sources", "sync_runs", "import_runs", "messages"} {
		var n int
		Require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
		Assert.Zero(t, n)
	}
}

func TestNativeMeetingFreshNotifyingRequestsDoNotReplay(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureNativeMeetingImports(t, st, true, true)
	req := validImportRequest(t)
	req.NotifyImports = true
	native := NewImporter(st, Hooks{})
	var prior string
	for range 2 {
		result, err := native.Import(t.Context(), req)
		require.NoError(err)
		require.NotEmpty(result.ImportRunID)
		assert.NotEqual(prior, result.ImportRunID)
		prior = result.ImportRunID
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM import_runs WHERE import_format='meeting_import' AND completed=TRUE AND notify_imports=TRUE`: 2,
		`SELECT COUNT(*) FROM messages`:                                                      1,
		`SELECT COUNT(*) FROM import_message_decisions`:                                      1,
		`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`: 1,
	} {
		var got int
		require.NoError(st.DB().QueryRow(query).Scan(&got))
		assert.Equal(want, got, query)
	}
}

func TestNativeMeetingNotificationChoiceSurvivesNormalizationWithoutChangingRawIdentity(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	req := validImportRequest(t)
	silent, err := req.Normalize()
	require.NoError(err)
	before, err := BuildSnapshot(silent)
	require.NoError(err)
	req.NotifyImports = true
	notifying, err := req.Normalize()
	require.NoError(err)
	assert.True(notifying.NotifyImports)
	after, err := BuildSnapshot(notifying)
	require.NoError(err)
	assert.Equal(before.SourceMessageID, after.SourceMessageID)
	assert.Equal(before.Raw, after.Raw, "notification policy is not meeting evidence")
}

func TestNativeMeetingDecodeNotificationChoiceIsStrictAndClosed(t *testing.T) {
	for _, value := range []string{"true", "false", "null", "1", "\"true\""} {
		t.Run(value, func(t *testing.T) {
			body := strings.Replace(validRequestJSON, "{", `{"notify_imports":`+value+`,`, 1)
			req, err := DecodeRequest(strings.NewReader(body), MaxRequestBytes)
			if value != "true" && value != "false" {
				Require.Error(t, err)
				return
			}
			Require.NoError(t, err)
			Assert.Equal(t, value == "true", req.NotifyImports)
		})
	}
	for _, field := range []string{`"import_run_id":"forged"`, `"principal":"owner:forged"`, `"live":true`} {
		body := strings.Replace(validRequestJSON, "{", "{"+field+",", 1)
		_, err := DecodeRequest(strings.NewReader(body), MaxRequestBytes)
		Require.Error(t, err, "notification policy adds no retry or authority fields")
	}
}
