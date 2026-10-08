package slack

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// Producer tests configure the real journal directly; native SDK discovery and
// authorization are covered separately before advertising Slack capability.
func enableNativeSlackEvents(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "slack", Kinds: []string{"message", "reaction"}}}})
	require.NoError(t, err)
}

func TestMCPNativeSlackIncrementalReadySnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := nativeEventsWorkspace(t)
	imp, opts := testImporter(t, f)
	opts.NoThreads = true
	now := tsBase.Add(24 * time.Hour)
	imp.now = func() time.Time { return now }
	enableNativeSlackEvents(t, imp.store)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	require.Zero(count, "initial native history is not a live arrival")
	liveTS := strconv.FormatInt(now.Add(time.Minute).Unix(), 10) + ".000100"
	f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: liveTS, User: "USENDER", Text: "Synthetic live <@UME>", Reactions: []map[string]any{{"name": "wave", "users": []string{"UME"}, "count": 1}}})
	now = now.Add(time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	require.Equal(1, count, "one ready message produces one exact-conversation occurrence")
	var messageID int64
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT id FROM messages WHERE source_message_id=?`), "CEVENTS:"+liveTS).Scan(&messageID))
	var text, raw string
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), messageID).Scan(&text))
	assert.Contains(text, "Synthetic live")
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT raw_format FROM message_raw WHERE message_id=?`), messageID).Scan(&raw))
	assert.Equal("slack_json", raw)
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id=? AND recipient_type='mention'`), messageID).Scan(&count))
	assert.Equal(1, count, "mentions accompany readiness")
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "overlap and replay never create a second message occurrence")
}

func TestMCPNativeSlackIncrementalThreadAndFullRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := nativeEventsWorkspace(t)
	imp, opts := testImporter(t, f)
	now := tsBase.Add(24 * time.Hour)
	imp.now = func() time.Time { return now }
	enableNativeSlackEvents(t, imp.store)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	rootTS := f.convs[0].Msgs[0].TS
	liveTS := strconv.FormatInt(now.Add(time.Minute).Unix(), 10) + ".000100"
	f.convs[0].Msgs[0].Replies = []fakeMsg{{TS: liveTS, ThreadTS: rootTS, User: "USENDER", Text: "Synthetic live thread reply"}}
	now = now.Add(time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))

	require.Equal(1, count, "native incremental thread coverage admits the new reply without relabeling its old root")
	var parentID int64
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT reply_to_message_id FROM messages WHERE source_message_id=?`), "CEVENTS:"+liveTS).Scan(&parentID))
	assert.Positive(parentID)
	oldReplyTS := strconv.FormatInt(tsBase.Add(time.Hour).Unix(), 10) + ".000100"
	f.convs[0].Msgs[0].Replies = append([]fakeMsg{{TS: oldReplyTS, ThreadTS: rootTS, User: "USENDER", Text: "Synthetic old thread recovery"}}, f.convs[0].Msgs[0].Replies...)
	opts.Full = true
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "full thread recovery is historical even when the message is newly archived")
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), "CEVENTS:"+oldReplyTS).Scan(&count))
	assert.Equal(1, count)
}

func TestMCPNativeSlackDeferredThreadRetainsLiveProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := nativeEventsWorkspace(t)
	imp, opts := testImporter(t, f)
	now := tsBase.Add(24 * time.Hour)
	imp.now = func() time.Time { return now }
	enableNativeSlackEvents(t, imp.store)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	rootTS := strconv.FormatInt(now.Add(time.Minute).Unix(), 10) + ".000100"
	replyTS := strconv.FormatInt(now.Add(2*time.Minute).Unix(), 10) + ".000100"
	f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: rootTS, User: "USENDER", Text: "Synthetic new thread", Replies: []fakeMsg{{TS: replyTS, ThreadTS: rootTS, User: "USENDER", Text: "Synthetic deferred live reply"}}})
	f.failReplies[rootTS] = true
	now = now.Add(time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.Error(err, "the provider failure must leave the native debt resumable")
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	require.Equal(1, count, "only the ready root was committed")
	delete(f.failReplies, rootTS)
	now = now.Add(time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(2, count, "retrying a native live thread preserves its original admission evidence")
}

func TestMCPNativeSlackIncrementalClockSkewDoesNotMuteArrival(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := nativeEventsWorkspace(t)
	imp, opts := testImporter(t, f)
	opts.NoThreads = true
	now := tsBase.Add(24 * time.Hour)
	imp.now = func() time.Time { return now }
	enableNativeSlackEvents(t, imp.store)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	// The native importer overlaps its completed window to absorb provider
	// clock skew. This message arrives after that window was fetched, though
	// its provider timestamp is just below the archive's clock-based pin.
	liveTS := strconv.FormatInt(now.Add(-30*time.Second).Unix(), 10) + ".000100"
	f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: liveTS, User: "USENDER", Text: "Synthetic skewed live arrival"})
	now = now.Add(time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), "CEVENTS:"+liveTS).Scan(&count))
	require.Equal(1, count, "the real native overlap path archives the arrival")
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "native incremental provenance, rather than timestamp comparison, admits the arrival")
}

func TestMCPNativeSlackMergedAuditDebtKeepsHistoricalSiblingsMuted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := nativeEventsWorkspace(t)
	imp, opts := testImporter(t, f)
	now := tsBase.Add(24 * time.Hour)
	imp.now = func() time.Time { return now }
	enableNativeSlackEvents(t, imp.store)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	// Schedule an audit before introducing search debt; the scheduler correctly
	// refuses to start a second walk while reply debt is outstanding.
	now = now.Add(8 * 24 * time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	root := &f.convs[0].Msgs[0]
	oldTS := strconv.FormatInt(tsBase.Add(time.Hour).Unix(), 10) + ".000100"
	newTS := strconv.FormatInt(now.Add(time.Minute).Unix(), 10) + ".000100"
	root.Replies = []fakeMsg{
		{TS: oldTS, ThreadTS: root.TS, User: "USENDER", Text: "Synthetic missed historical sibling"},
		{TS: newTS, ThreadTS: root.TS, User: "USENDER", Text: "Synthetic incremental reply"},
	}
	f.failReplies[root.TS] = true
	now = now.Add(time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.Error(err, "canonical audit and incremental search leave durable debt")
	delete(f.failReplies, root.TS)
	// A new importer has no in-memory admission state from the failed run.
	imp = NewImporter(imp.store, imp.client, opts.TeamID)
	imp.now = func() time.Time { return now }
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "full historical debt must not widen live admission below its recorded boundary")
	var sourceID string
	require.NoError(imp.store.DB().QueryRow(`SELECT m.source_message_id FROM messages m WHERE EXISTS (SELECT 1 FROM mcp_event_log e WHERE e.message_id = m.id)`).Scan(&sourceID))
	assert.Equal("CEVENTS:"+newTS, sourceID)
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "CEVENTS:"+oldTS).Scan(&count))
	assert.Equal(1, count, "the historical sibling is still archived")
}

func TestMCPNativeSlackSoloSearchReanchor(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(strconv.FormatBool(limited), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := nativeEventsWorkspace(t)
			imp, opts := testImporter(t, f)
			now := tsBase.Add(24 * time.Hour)
			imp.now = func() time.Time { return now }
			enableNativeSlackEvents(t, imp.store)
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			rootTS := tsFormat(now.Add(time.Minute))
			childTS := tsFormat(now.Add(2 * time.Minute))
			f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: rootTS, User: "USENDER", Text: "Synthetic recovered parent", Replies: []fakeMsg{{TS: childTS, ThreadTS: rootTS, User: "USENDER", Text: "Synthetic new child"}}})
			f.failHistory["CEVENTS"] = true
			f.searchOmitThreadTS = true
			if limited {
				opts.Limit = 2
			} else {
				f.failReplies[rootTS] = true
			}
			now = now.Add(time.Hour)
			_, err = imp.Import(t.Context(), opts)
			require.Error(err)
			var count int
			require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "CEVENTS:"+childTS).Scan(&count))
			assert.Zero(count, "the solo child must wait for its canonical parent before first insertion")
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count)
			delete(f.failReplies, rootTS)
			imp = NewImporter(imp.store, imp.client, opts.TeamID)
			imp.now = func() time.Time { return now }
			_, err = imp.Import(t.Context(), opts)
			if err != nil {
				require.ErrorContains(err, "partial Slack sync", "only the deliberately unavailable history may remain")
			}
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Equal(1, count)
			var linked int
			require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages child WHERE child.source_message_id = ? AND EXISTS (SELECT 1 FROM messages parent WHERE parent.id = child.reply_to_message_id AND parent.source_message_id = ?) AND EXISTS (SELECT 1 FROM mcp_event_log e WHERE e.message_id = child.id)`), "CEVENTS:"+childTS, "CEVENTS:"+rootTS).Scan(&linked))
			assert.Equal(1, linked, "first child occurrence has its readable parent")
		})
	}
}

func TestMCPNativeSlackHistoricalCheckpointRecovery(t *testing.T) {
	for _, kind := range []string{"legacy_window", "legacy_thread", "full_repair", "audit", "maintenance"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := nativeEventsWorkspace(t)
			imp, opts := testImporter(t, f)
			now := tsBase.Add(24 * time.Hour)
			imp.now = func() time.Time { return now }
			enableNativeSlackEvents(t, imp.store)
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			src, err := imp.store.GetOrCreateSource("slack", opts.TeamID+":"+opts.UserID)
			require.NoError(err)
			state, err := imp.loadResumeState(src.ID)
			require.NoError(err)
			cs := state.EnsureConv("CEVENTS")
			missed := tsFormat(now.Add(-time.Hour))
			root := &f.convs[0].Msgs[0]
			switch kind {
			case "legacy_window":
				missed = tsFormat(now.Add(time.Minute))
				f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: missed, User: "USENDER", Text: "Synthetic legacy window recovery"})
				cs.BackfillLatest = tsFormat(now.Add(time.Hour))
				cs.BackfillLiveAfter = ""
			case "maintenance":
				f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: missed, User: "USENDER", Text: "Synthetic maintenance recovery"})
				opts.Maintenance = true
			default:
				root.Replies = []fakeMsg{{TS: missed, ThreadTS: root.TS, User: "USENDER", Text: "Synthetic historical thread recovery"}}
				switch kind {
				case "legacy_thread":
					cs.RecordPendingThread(root.TS, 1)
				case "full_repair":
					state.RepairPending = true
					cs.Done = false
					cs.Cursor = ""
				case "audit":
					cs.ThreadsPending = true
					cs.AuditPending = true
				}
			}
			runID, err := imp.store.StartSync(src.ID, "slack")
			require.NoError(err)
			require.NoError(imp.store.CompleteSync(runID, mustMarshal(t, state)))
			imp = NewImporter(imp.store, imp.client, opts.TeamID)
			now = now.Add(time.Hour)
			imp.now = func() time.Time { return now }
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			var count int
			require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), "CEVENTS:"+missed).Scan(&count))
			assert.Equal(1, count, "the native recovery really archived the missing item")
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count, "historical checkpoint recovery must not become live on restart")
		})
	}
}

func TestMCPNativeSlackMergedSoloAuditDebtKeepsHistoricalSiblingsMuted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := nativeEventsWorkspace(t)
	imp, opts := testImporter(t, f)
	now := tsBase.Add(24 * time.Hour)
	imp.now = func() time.Time { return now }
	enableNativeSlackEvents(t, imp.store)
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	// Schedule an audit before introducing search debt; the scheduler correctly
	// refuses to start a second walk while reply debt is outstanding.
	now = now.Add(8 * 24 * time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	root := &f.convs[0].Msgs[0]
	oldTS := strconv.FormatInt(tsBase.Add(time.Hour).Unix(), 10) + ".000100"
	newTS := strconv.FormatInt(now.Add(time.Minute).Unix(), 10) + ".000100"
	root.Replies = []fakeMsg{
		{TS: oldTS, ThreadTS: root.TS, User: "USENDER", Text: "Synthetic missed historical sibling"},
		{TS: newTS, ThreadTS: root.TS, User: "USENDER", Text: "Synthetic incremental reply"},
	}
	f.searchOmitThreadTS = true
	f.failReplies[root.TS] = true
	now = now.Add(time.Hour)
	_, err = imp.Import(t.Context(), opts)
	require.Error(err, "canonical audit and incremental search leave durable debt")
	delete(f.failReplies, root.TS)
	// A new importer has no in-memory admission state from the failed run.
	imp = NewImporter(imp.store, imp.client, opts.TeamID)
	imp.now = func() time.Time { return now }
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	assert.Equal(1, count, "full historical debt must not widen live admission below its recorded boundary")
	var sourceID string
	require.NoError(imp.store.DB().QueryRow(`SELECT m.source_message_id FROM messages m WHERE EXISTS (SELECT 1 FROM mcp_event_log e WHERE e.message_id = m.id)`).Scan(&sourceID))
	assert.Equal("CEVENTS:"+newTS, sourceID)
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "CEVENTS:"+oldTS).Scan(&count))
	assert.Equal(1, count, "the historical sibling is still archived")
}

func TestMCPNativeSlackLimitedHistoryResumesSilently(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(strconv.FormatBool(repair), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := nativeEventsWorkspace(t)
			imp, opts := testImporter(t, f)
			now := tsBase.Add(24 * time.Hour)
			imp.now = func() time.Time { return now }
			enableNativeSlackEvents(t, imp.store)
			if repair {
				_, err := imp.Import(t.Context(), opts)
				require.NoError(err)
			}
			root := &f.convs[0].Msgs[0]
			replyTS := tsFormat(tsBase.Add(time.Hour))
			root.Replies = []fakeMsg{{TS: replyTS, ThreadTS: root.TS, User: "USENDER", Text: "Synthetic limited history reply"}}
			opts.Full = repair
			opts.Limit = 1
			first, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			assert.Zero(first.RepliesFetched, "the limit really deferred the reply")
			opts.Full = false
			opts.Limit = 0
			imp = NewImporter(imp.store, imp.client, opts.TeamID)
			now = now.Add(time.Hour)
			imp.now = func() time.Time { return now }
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			var count int
			require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), "CEVENTS:"+replyTS).Scan(&count))
			assert.Equal(1, count)
			require.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count, "a plain restart cannot relabel deferred history as live")
		})
	}
}

func TestMCPNativeSlackBroadcastBeforeParentDoesNotWedge(t *testing.T) {
	for _, rootAvailable := range []bool{false, true} {
		for _, eventsEnabled := range []bool{false, true} {
			t.Run("root="+strconv.FormatBool(rootAvailable)+"/events="+strconv.FormatBool(eventsEnabled), func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				f := nativeEventsWorkspace(t)
				imp, opts := testImporter(t, f)
				now := tsBase.Add(24 * time.Hour)
				imp.now = func() time.Time { return now }
				if eventsEnabled {
					enableNativeSlackEvents(t, imp.store)
				}
				_, err := imp.Import(t.Context(), opts)
				require.NoError(err)
				rootTS := tsFormat(now.Add(time.Minute))
				replyTS := tsFormat(now.Add(2 * time.Minute))
				reply := fakeMsg{TS: replyTS, ThreadTS: rootTS, Subtype: "thread_broadcast", User: "USENDER", Text: "Synthetic broadcast reply"}
				if rootAvailable {
					f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: rootTS, User: "USENDER", Text: "Synthetic new root", Replies: []fakeMsg{reply}})
				}
				f.convs[0].Msgs = append(f.convs[0].Msgs, reply)
				now = now.Add(time.Hour)
				_, err = imp.Import(t.Context(), opts)
				require.NoError(err, "newest-first history must advance past a broadcast with no archived parent")
				var count int
				require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), "CEVENTS:"+replyTS).Scan(&count))
				assert.Equal(1, count, "even an unavailable root must not prevent archiving its broadcast")
				require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM mcp_event_log e WHERE EXISTS (SELECT 1 FROM messages m WHERE m.id=e.message_id AND m.source_message_id=?)`), "CEVENTS:"+replyTS).Scan(&count))
				assert.Zero(count, "incomplete-parent recovery remains silent")
				if rootAvailable {
					require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages child WHERE child.source_message_id=? AND EXISTS (SELECT 1 FROM messages parent WHERE parent.id=child.reply_to_message_id AND parent.source_message_id=?)`), "CEVENTS:"+replyTS, "CEVENTS:"+rootTS).Scan(&count))
					assert.Equal(1, count, "the canonical thread drain repairs the parent link")
				}
				nextTS := tsFormat(now.Add(time.Minute))
				f.convs[0].Msgs = append(f.convs[0].Msgs, fakeMsg{TS: nextTS, User: "USENDER", Text: "Synthetic later arrival"})
				now = now.Add(time.Hour)
				_, err = imp.Import(t.Context(), opts)
				require.NoError(err)
				require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_message_id=?`), "CEVENTS:"+nextTS).Scan(&count))
				assert.Equal(1, count, "later incremental syncs remain usable")
			})
		}
	}
}
