package meta

import (
	"context"
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
)

func TestDMCheckpointForeignSourceRefused(t *testing.T) {
	for _, kind := range []string{"source", "platform", "version", "cursor", "cursor_control", "scan_start", "frame", "index", "message_id"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, fixture, imp, source := setupDMBackfill(t, PlatformMessenger, 2)
			src, err := st.GetSourceByTypeAndIdentifier(source.Platform.SourceType(), source.Identifier())
			require.NoError(err)
			s := dmSyncState{Version: "dm1", Source: source.Identifier(), Platform: PlatformMessenger}
			switch kind {
			case "source":
				s.Source = "app:99:page:100"
			case "platform":
				s.Platform = PlatformInstagram
			case "version":
				s.Version = "dm2"
			case "cursor":
				s.After = strings.Repeat("x", 4097)
			case "cursor_control":
				s.After = "cursor\n"
			case "scan_start":
				s.ScanStarted = "invalid"
			case "frame":
				s.Pending = []Conversation{{ID: "t_1"}}
				s.Frame = &dmFrame{ThreadID: "t_1"}
				for i := range 21 {
					s.Frame.IDs = append(s.Frame.IDs, "m_"+strconv.Itoa(i))
				}
			case "index":
				s.Pending = []Conversation{{ID: "t_1"}}
				s.Frame = &dmFrame{ThreadID: "t_1", Next: -1}
			case "message_id":
				s.Pending = []Conversation{{ID: "t_1"}}
				s.Frame = &dmFrame{ThreadID: "t_1", IDs: []string{"../invalid"}}
			}
			blob, err := json.Marshal(s)
			require.NoError(err)
			run, err := st.StartSync(src.ID, SourceTypeFacebookMessages)
			require.NoError(err)
			require.NoError(st.FailSyncWithCheckpoint(run, "synthetic interruption", &store.Checkpoint{PageToken: string(blob)}))
			_, err = imp.Import(t.Context(), source, ImportOptions{})
			require.ErrorContains(err, "invalid inbox checkpoint")
			assert.Zero(fixture.calls)
		})
	}
}

func TestDMImportResumesAfterWebhookRun(t *testing.T) {
	for _, p := range []DMPlatform{PlatformMessenger, PlatformInstagram} {
		t.Run(string(p), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, f, imp, src := setupDMBackfill(t, p, 7)
			sum, err := imp.Import(t.Context(), src, ImportOptions{MaxRequests: 5})
			require.NoError(err)
			require.True(sum.Partial)
			s, err := st.GetSourceByTypeAndIdentifier(p.SourceType(), src.Identifier())
			require.NoError(err)
			run, err := st.StartSync(s.ID, p.SourceType()+"_webhook")
			require.NoError(err)
			require.NoError(st.CompleteSyncContext(t.Context(), run, ""))
			before := f.nodes["m_0"]
			sum, err = imp.Import(t.Context(), src, ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(before, f.nodes["m_0"])
			count, err := st.CountMessagesWithRaw(s.ID)
			require.NoError(err)
			assert.EqualValues(7, count)
		})
	}
}

type dmYieldingAPI struct {
	DMAPI

	cancel context.CancelCauseFunc
}

func (a dmYieldingAPI) Message(ctx context.Context, id string) (DMMessage, error) {
	if id == "m_1" {
		a.cancel(jobctx.ErrYieldedToWaiter)
	}
	return a.DMAPI.Message(ctx, id)
}
func TestDMImportSchedulerYieldHandoff(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, src := setupDMBackfill(t, PlatformMessenger, 4)
	base := imp.api
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	imp.api = dmYieldingAPI{DMAPI: base, cancel: cancel}
	sum, err := imp.Import(ctx, src, ImportOptions{})
	require.ErrorIs(err, context.Canceled)
	require.True(sum.Partial)
	require.NoError(jobctx.ErrorAfterYield(ctx, err))
	s, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	run, err := st.GetLatestCheckpointedSyncByTypeContext(t.Context(), s.ID, src.Platform.SourceType())
	require.NoError(err)
	assert.Equal(store.SyncStatusCancelled, run.Status)
	assert.Zero(run.ErrorsCount)
	imp.api = base
	sum, err = imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(1, f.nodes["m_0"])
}
