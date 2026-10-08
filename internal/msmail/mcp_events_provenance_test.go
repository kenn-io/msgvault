package msmail

import (
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func enableNativeMailEvents(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
	require.NoError(t, err)
}
func nativeMailEventCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	return count
}

func TestMCPMSMailNativeDeltaCoverage(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "history", true: "empty"}[empty], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			enableNativeMailEvents(t, st)
			f := newFakeGraph(t)
			if !empty {
				f.put("history", "inbox")
			}
			_, err := f.sync(t, st)
			require.NoError(err)
			assert.Zero(nativeMailEventCount(t, st))
			f.put("live", "inbox")
			_, err = f.sync(t, st)
			require.NoError(err)
			assert.Equal(1, nativeMailEventCount(t, st))
			f.version["live"]++
			f.put("live", "archive")
			_, err = f.sync(t, st)
			require.NoError(err)
			assert.Equal(1, nativeMailEventCount(t, st), "move and update are not arrivals")
			f.expired["inbox"] = true
			f.put("recovery", "inbox")
			_, err = f.sync(t, st)
			require.NoError(err)
			assert.Equal(1, nativeMailEventCount(t, st), "expired cursor recovery stays silent")
			f.folders = append(f.folders, "projects")
			f.put("older", "projects")
			_, err = f.sync(t, st)
			require.NoError(err)
			assert.Equal(1, nativeMailEventCount(t, st), "new folder cannot borrow coverage")
			f.put("later", "projects")
			_, err = f.sync(t, st)
			require.NoError(err)
			assert.Equal(2, nativeMailEventCount(t, st))
		})
	}
}

func TestMCPMSMailInitialOverridesCallerMode(t *testing.T) {
	st := testutil.NewTestStore(t)
	enableNativeMailEvents(t, st)
	f := newFakeGraph(t)
	f.put("history", "inbox")
	_, err := f.sync(t, st.WithIngestContext(store.IngestContext{Mode: store.IngestLive}))
	require.NoError(t, err)
	assert.Zero(t, nativeMailEventCount(t, st), "native initial history must not inherit a caller's live view")
}

func TestMCPMSMailInterruptedWalksRemainHistory(t *testing.T) {
	for _, kind := range []string{"initial", "expired continuation", "expired first request"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			enableNativeMailEvents(t, st)
			f := newFakeGraph(t)
			if kind != "initial" {
				_, err := f.sync(t, st)
				require.NoError(err)
				f.expired["inbox"] = true
			}
			for _, id := range []string{"one", "two", "three", "four"} {
				f.put(id, "inbox")
			}
			if kind == "expired first request" {
				f.gone["inbox"] = true
			} else {
				f.stopAt = 2
			}
			_, err := f.sync(t, st)
			require.Error(err)
			assert.Zero(nativeMailEventCount(t, st))
			f.gone["inbox"] = false
			f.expired["inbox"] = false
			_, err = f.sync(t, st)
			require.NoError(err)
			assert.Zero(nativeMailEventCount(t, st))
			f.put("live", "inbox")
			_, err = f.sync(t, st)
			require.NoError(err)
			assert.Equal(1, nativeMailEventCount(t, st))
		})
	}
}

func TestMCPMSMailInterruptedLiveContinuation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	enableNativeMailEvents(t, st)
	f := newFakeGraph(t)
	_, err := f.sync(t, st)
	require.NoError(err)
	f.deltaPageSize = 2
	f.deltaStopAt = 2
	for _, id := range []string{"one", "two", "three", "four"} {
		f.put(id, "inbox")
	}
	_, err = f.sync(t, st)
	require.Error(err)
	assert.Equal(2, nativeMailEventCount(t, st))
	f.put("later", "inbox")
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(4, nativeMailEventCount(t, st), "continuation keeps its pinned live round")
	_, err = f.sync(t, st)
	require.NoError(err)
	assert.Equal(5, nativeMailEventCount(t, st))
}

func TestMCPMSMailFailedSnapshotRetryProvenance(t *testing.T) {
	for _, live := range []bool{false, true} {
		for _, failure := range []string{"attachment", "SQL"} {
			name := map[bool]string{false: "history", true: "live"}[live] + "/" + failure
			t.Run(name, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				st := testutil.NewTestStore(t)
				enableNativeMailEvents(t, st)
				f := newFakeGraph(t)
				if live {
					_, err := f.sync(t, st)
					require.NoError(err)
				}
				f.put("retry", "inbox")
				f.withAttachment["retry"] = true
				release := func() {}
				if failure == "SQL" {
					release = failMailSnapshotWrite(t, st, "message_raw")
				} else {
					blocker := filepath.Join(t.TempDir(), "file")
					require.NoError(os.WriteFile(blocker, nil, 0o600))
					f.attachDir = filepath.Join(blocker, "attachments")
				}
				for range 2 {
					_, err := f.sync(t, st)
					if failure == "SQL" {
						require.Error(err)
					} else {
						require.NoError(err)
					}
					assert.Zero(nativeMailEventCount(t, st))
					f.put("retry", "inbox")
				}
				release()
				f.attachDir = ""
				f.put("retry", "unlisted")
				_, err := f.sync(t, st)
				require.NoError(err)
				assert.Zero(nativeMailEventCount(t, st))
				f.folders = append(f.folders, "unlisted")
				_, err = f.sync(t, st)
				require.NoError(err)
				want := 0
				if live {
					want = 1
				}
				assert.Equal(want, nativeMailEventCount(t, st), "snapshot retry keeps its original phase across a folder move")
				_, err = f.sync(t, st)
				require.NoError(err)
				assert.Equal(want, nativeMailEventCount(t, st))
			})
		}
	}
}

// Exercise the persisted native checkpoint round trip, not a copy of its
// encoding logic. Arbitrary repeated observations and folder moves cannot
// upgrade history or lose affirmative live provenance.
func FuzzMCPMSMailRetryProvenance(f *testing.F) {
	f.Add(false, uint8(0), []byte("archive"))
	f.Add(true, uint8(2), []byte("projects"))
	f.Fuzz(func(t *testing.T, live bool, repeats uint8, folderBytes []byte) {
		if len(folderBytes) > 24 {
			t.Skip()
		}
		require := require.New(t)
		st := testutil.NewTestStore(t)
		enableNativeMailEvents(t, st)
		graph := newFakeGraph(t)
		if live {
			_, err := graph.sync(t, st)
			require.NoError(err)
		}
		graph.badValue["retry"] = true
		graph.put("retry", "inbox")
		for range 1 + int(repeats%3) {
			_, err := graph.sync(t, st)
			require.NoError(err)
			graph.put("retry", "inbox")
		}
		folder := "synthetic-" + hex.EncodeToString(folderBytes)
		graph.folders = append(graph.folders, folder)
		graph.put("retry", folder)
		graph.badValue["retry"] = false
		_, err := graph.sync(t, st)
		require.NoError(err)
		want := 0
		if live {
			want = 1
		}
		assert.Equal(t, want, nativeMailEventCount(t, st))
	})
}

func TestMCPMSMailLegacyAndUnknownRetryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, saved string
		live        bool
	}{
		{"legacy folder", "inbox", false},
		{"malformed", "{", false},
		{"missing folder", `{"version":1,"mode":"live"}`, false},
		{"unknown version", `{"version":2,"folder_id":"inbox","mode":"live"}`, false},
		{"unknown mode", `{"version":1,"folder_id":"inbox","mode":"future"}`, false},
		{"proven live", `{"version":1,"folder_id":"inbox","mode":"live"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			enableNativeMailEvents(t, st)
			f := newFakeGraph(t)
			summary, err := f.sync(t, st)
			require.NoError(err)
			previous, err := st.GetLastSuccessfulSync(summary.SourceID)
			require.NoError(err)
			var cursors map[string]string
			require.NoError(json.Unmarshal([]byte(previous.CursorAfter.String), &cursors))
			cursors[retryPrefix+"retry"] = tc.saved
			blob, err := json.Marshal(cursors)
			require.NoError(err)
			syncID, err := st.StartSync(summary.SourceID, SourceType)
			require.NoError(err)
			require.NoError(st.FailSyncWithCheckpoint(syncID, "synthetic pending retry", &store.Checkpoint{PageToken: string(blob)}))
			f.put("retry", "inbox")
			f.badValue["retry"] = true
			_, err = f.sync(t, st)
			require.NoError(err)
			f.badValue["retry"] = false
			_, err = f.sync(t, st)
			require.NoError(err)
			want := 0
			if tc.live {
				want = 1
			}
			assert.Equal(t, want, nativeMailEventCount(t, st))
		})
	}
}

func TestMSMailRetryEmptyFolderPreservesEvidence(t *testing.T) {
	for _, saved := range []string{"inbox", `{"version":1,"folder_id":"inbox","mode":"live"}`} {
		s := &syncer{cursors: map[string]string{retryPrefix + "retry": saved}, ingestMode: store.IngestLive}
		s.saveRetry("retry", "")
		assert.Equal(t, saved, s.cursors[retryPrefix+"retry"], "an incomplete lookup must not erase durable evidence")
	}
}
