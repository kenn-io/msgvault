package chatwoot

import (
	"context"
	"fmt"
	"math"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/chatwootapi"
	"golang.org/x/time/rate"
)

type contractAPI struct {
	*chatwootapi.API

	server      *httptest.Server
	mediaRouter *chatwootMediaRouter
}

func newContractAPI(t *testing.T, pageCap int, messages []map[string]any) *contractAPI {
	t.Helper()
	api := &contractAPI{API: chatwootapi.New(pageCap, messages, func() time.Time { return now() })}
	api.server = httptest.NewServer(api.API)
	t.Cleanup(api.server.Close)
	return api
}

func (api *contractAPI) client(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(api.server.URL, 3, "synthetic-token")
	require.NoError(t, err)
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	client.messageRangeCap = api.Cap
	if api.mediaRouter != nil {
		api.mediaRouter.attach(client)
	}
	return client
}

func contractMessage(id, at int64, sender map[string]any) map[string]any {
	return chatwootapi.Message(id, at, sender)
}

func contractRegister(t *testing.T, st *store.Store, api *contractAPI) (*Importer, *store.Source) {
	t.Helper()
	importer := NewImporter(st, api.client(t))
	sources, err := importer.Register(t.Context(), []Inbox{{ID: 7, Name: "Example Inbox"}})
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, "chatwoot", sources[0].SourceType)
	return importer, sources[0]
}

func contractMessageIDs(t *testing.T, st *store.Store) []int64 {
	t.Helper()
	rows, err := st.DB().Query(`SELECT source_message_id FROM messages WHERE message_type = 'chatwoot'`)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var raw string
		require.NoError(t, rows.Scan(&raw))
		id, err := strconv.ParseInt(raw, 10, 64)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	slices.Sort(ids)
	return ids
}

// Replacing range traversal with a min/max cursor or treating a short page as
// complete must lose records in at least one of these independent fixtures.
func TestImportContractRangesPreserveDisorderedIDs(t *testing.T) {
	for _, pageCap := range []int{1, 3, 1000} {
		t.Run(fmt.Sprintf("cap_%d", pageCap), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			want := []int64{1, 2, 7, 9, 15, 33, 41, 70, 111, 901, 904, 1301, 2100, math.MaxInt32}
			messages := make([]map[string]any, 0, len(want))
			for i, id := range want {
				messages = append(messages, contractMessage(id, 1767225600+int64((i*7)%5), nil))
			}
			api := newContractAPI(t, pageCap, messages)
			st := testutil.NewTestStore(t)
			importer, _ := contractRegister(t, st, api)
			_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
			require.NoError(err)
			assert.Equal(want, contractMessageIDs(t, st))
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
			require.NoError(err)
			assert.Equal(want, contractMessageIDs(t, st), "restart must not create duplicates")
		})
	}
}

func TestImportContractMaximumIDMediaAndCall(t *testing.T) {
	for _, mode := range []string{"initial", "limited", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			media := newMediaRefreshServer(t)
			router := newChatwootMediaRouter(t, media.server)
			attachment := map[string]any{"id": int64(2001), "file_type": "audio"}
			message := contractMessage(math.MaxInt32, now().Unix(), map[string]any{"id": int64(8), "type": "user"})
			message["content_type"], message["message_type"] = "voice_call", 1
			message["call"] = map[string]any{"id": 601, "direction": "outgoing", "status": "completed"}
			message["attachments"] = []any{attachment}
			api := newContractAPI(t, 1, []map[string]any{contractMessage(math.MaxInt32-1, now().Add(-time.Second).Unix(), nil), message})
			api.mediaRouter = router
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			opts := mediaRefreshOptions(t)
			if mode == "limited" {
				opts.Limit = 1
			}
			if mode == "legacy" {
				api.Conversations[42] = api.Conversations[42][:1]
				_, err := imp.Import(t.Context(), opts)
				require.NoError(err)
				legacy := savedState(t, st, source)
				legacy.Conversations["42"] = &conversationState{Pending: []idRange{{math.MaxInt32 - 1, math.MaxInt32}}}
				blob, err := legacy.marshal()
				require.NoError(err)
				parsed, err := parseSyncState(blob, legacy.Scope)
				require.NoError(err)
				assert.Equal(legacy.Conversations["42"].Pending, parsed.Conversations["42"].Pending)
				syncID, err := st.StartSyncContext(t.Context(), source.ID, SourceType)
				require.NoError(err)
				require.NoError(st.CompleteSyncAndUpdateSourceCursorContext(t.Context(), syncID, source.ID, blob))
				api.Conversations[42] = append(api.Conversations[42], message)
			}
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			for tries := 0; sum.Partial && tries < 10; tries++ {
				sum, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
			}
			require.False(sum.Partial)
			if mode == "legacy" {
				_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
			}
			chatID := contractArchivedMessageID(t, st, "2147483647")
			meetingID := contractArchivedMessageID(t, st, "call:2147483647")
			require.Contains(savedState(t, st, source).Conversations["42"].Artifacts, "2147483647")
			attachment["data_url"] = router.url(t, media.server, "/recording-a.ogg")
			attachment["transcribed_text"] = "maximumquartz transcript"
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
			require.NoError(err)
			for _, id := range []int64{chatID, meetingID} {
				body, err := st.GetMessageBodyText(id)
				require.NoError(err)
				assert.Contains(body, "maximumquartz")
				_, payloads := readMediaRefreshBytes(t, st, id, opts.AttachmentsDir)
				assert.Equal([]string{"synthetic recording A bytes"}, payloads)
			}
			opts.Limit, opts.Full = 0, true
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
			require.NoError(err)
			assert.Equal(chatID, contractArchivedMessageID(t, st, "2147483647"))
			assert.Equal(meetingID, contractArchivedMessageID(t, st, "call:2147483647"))
		})
	}
}

// A capped response keeps every unhandled hole; a --full walk resumes its saved
// ranges instead of restarting at the first page.
func TestImportContractLimitedRunsResume(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pageCap int
		full    bool
	}{{"capped", 3, false}, {"full", 20, true}} {
		t.Run(tc.name, func(t *testing.T) {
			want := []int64{1, 2, 7, 9, 15, 33, 41}
			messages := make([]map[string]any, 0, len(want))
			for i, id := range want {
				messages = append(messages, contractMessage(id, 1767225600+int64(len(want)-i), nil))
			}
			api := newContractAPI(t, tc.pageCap, messages)
			st := testutil.NewTestStore(t)
			contractRegister(t, st, api)
			for range len(want) + 2 {
				sum, err := NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, Limit: 2, Full: tc.full})
				require.NoError(t, err)
				require.LessOrEqual(t, sum.MessagesProcessed, 2, "a run respects its message limit")
				if len(contractMessageIDs(t, st)) == len(want) {
					break
				}
			}
			assert.Equal(t, want, contractMessageIDs(t, st), "unfinished ranges survive importer restarts")
		})
	}
}

func TestImportContractRejectsIgnoredRangeBoundsWithoutPoisoningResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 20, []map[string]any{contractMessage(101, 1767225600, nil), contractMessage(102, 1767225601, nil)})
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	api.Mu.Lock()
	api.IgnoreBounds = true
	api.Mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := importer.Import(ctx, ImportOptions{InboxID: 7, IncludePrivate: true})
	require.Error(err, "an incompatible range API must fail explicitly")
	require.NotErrorIs(err, context.DeadlineExceeded, "reject violated bounds rather than looping until cancellation")
	api.Mu.Lock()
	api.IgnoreBounds = false
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	assert.Equal([]int64{101, 102}, contractMessageIDs(t, st))
}

func TestOversizedResponsesSplitAndResume(t *testing.T) {
	for _, artifact := range []bool{false, true} {
		t.Run(strconv.FormatBool(artifact), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			api := newContractAPI(t, 1000, nil)
			for id := int64(100); id < 360; id++ {
				api.AddMessage(1, id, now())
			}
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			opts := ImportOptions{InboxID: 7}
			var lastAttachment map[string]any
			if artifact {
				for _, m := range api.Conversations[1] {
					lastAttachment = map[string]any{"id": m["id"], "file_type": "audio"}
					m["attachments"] = []any{lastAttachment}
				}
				_, err := imp.Import(t.Context(), opts)
				require.NoError(err)
			}
			for _, m := range api.Conversations[1] {
				m["content"] = strings.Repeat("x", 150000)
				if !artifact {
					delete(m, "attachments")
				}
			}
			if artifact {
				lastAttachment["transcribed_text"] = "latequartz transcript"
			}
			imp.requestBudget = 8
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			require.True(sum.Partial)
			require.NotEmpty(savedState(t, st, source).Conversations)
			for range 60 {
				restarted := NewImporter(st, api.client(t))
				restarted.requestBudget = 20
				sum, err = restarted.Import(t.Context(), opts)
				require.NoError(err)
				if artifact {
					body, err := st.GetMessageBodyText(contractArchivedMessageID(t, st, "359"))
					require.NoError(err)
					if strings.Contains(body, "latequartz") {
						break
					}
				} else if !sum.Partial {
					break
				}
			}
			if artifact {
				body, err := st.GetMessageBodyText(contractArchivedMessageID(t, st, "359"))
				require.NoError(err)
				assert.Contains(body, "latequartz")
				assert.Contains(savedState(t, st, source).Conversations["1"].Artifacts, "100", "early artifacts remain live")
			} else {
				assert.False(sum.Partial)
			}
			assert.Len(contractMessageIDs(t, st), 260)
		})
	}
}

func TestOversizedSingleMessageFailsClearly(t *testing.T) {
	api := newContractAPI(t, 1000, nil)
	api.AddMessage(1, 101, now())
	api.Conversations[1][0]["content"] = strings.Repeat("x", maxAPIBytes)
	st := testutil.NewTestStore(t)
	imp, _ := contractRegister(t, st, api)
	_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.ErrorIs(t, err, ErrResponseTooLarge)
	assert.Contains(t, err.Error(), "single message")
}
