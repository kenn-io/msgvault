package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type beeperAudio struct {
	messageID, attachmentID int64
	sourceMessageID, hash   string
	raw                     []byte
}

func newBeeperMediaFixture(t *testing.T) *storetest.Fixture {
	t.Helper()
	f := storetest.New(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE sources SET source_type = 'beeper', identifier = ? WHERE id = ?`), "signal", f.Source.ID)
	require.NoError(t, err)
	return f
}

// addBeeperAudio writes the message, raw JSON and stored audio row that a
// Beeper capture produces for one voice note.
func addBeeperAudio(t *testing.T, st *store.Store, sourceID, conversationID int64, sourceMessageID, hash string) beeperAudio {
	t.Helper()
	messageID, err := st.UpsertMessage(&store.Message{ConversationID: conversationID, SourceID: sourceID,
		SourceMessageID: sourceMessageID, MessageType: "beeper", SizeEstimate: 100})
	require.NoError(t, err)
	raw := []byte(fmt.Sprintf(`{"id":%q,"attachments":[{"id":"mxc://audio/%s"}]}`, sourceMessageID, sourceMessageID))
	require.NoError(t, st.UpsertMessageRawWithFormat(messageID, raw, "beeper_json"))
	require.NoError(t, st.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "voice.wav", MIMEType: "audio/wav", StoragePath: hash[:2] + "/" + hash,
		ContentHash: hash, Size: 44, SourceAttachmentID: "beeper:mxc://audio/" + sourceMessageID,
		SourcePartKey: "beeper:mxc://audio/" + sourceMessageID, MediaType: "voice_note",
		State: attachmentpolicy.StateStored, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	var attachmentID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT id FROM attachments WHERE message_id = ? AND content_hash = ?`), messageID, hash).Scan(&attachmentID))
	return beeperAudio{messageID: messageID, attachmentID: attachmentID, sourceMessageID: sourceMessageID, hash: hash, raw: raw}
}

func (a beeperAudio) mapping(destination, revision, processingKey string) store.BeeperMediaMapping {
	rawDigest := sha256.Sum256(a.raw)
	transcript := ""
	if processingKey != "" {
		transcript = strings.Repeat("b", 64)
	}
	return store.BeeperMediaMapping{
		DestinationKey: destination, OccurrenceRef: "msgvault:" + a.sourceMessageID, Revision: revision,
		SourceType: "beeper", SourceIdentifier: "signal", SourceConversationID: "default-thread",
		SourceMessageID: a.sourceMessageID, SourceAttachmentID: "beeper:mxc://audio/" + a.sourceMessageID,
		SourcePartKey: "beeper:mxc://audio/" + a.sourceMessageID, MessageID: a.messageID,
		AttachmentID: a.attachmentID, SourceSHA256: a.hash, ByteLength: 44,
		RawHash: hex.EncodeToString(rawDigest[:]), TranscriptSHA256: transcript,
		OccurrenceJSON: `{"ref":"msgvault:` + a.sourceMessageID + `","revision":"` + revision + `"}`,
		Filename:       "voice.wav", MIMEType: "audio/wav", ProcessingKey: processingKey,
	}
}

func retainOperation(mapping store.BeeperMediaMapping) store.BeeperMediaOperation {
	return store.BeeperMediaOperation{Kind: store.BeeperMediaOperationRetain, DestinationKey: mapping.DestinationKey,
		OccurrenceRef: mapping.OccurrenceRef, Revision: mapping.Revision}
}

// retainAudio records a mapping and applies an accepted retention receipt.
func retainAudio(t *testing.T, st *store.Store, mapping store.BeeperMediaMapping, occurrenceID string) {
	t.Helper()
	require.NoError(t, st.ReconcileBeeperMediaMapping(t.Context(), mapping))
	prepared, err := st.PrepareBeeperMediaOperation(t.Context(), retainOperation(mapping))
	require.NoError(t, err)
	applied, err := st.FinishBeeperMediaOperation(t.Context(), prepared, store.BeeperMediaResult{
		VaultUID: "vault", DocbankSourceID: "source-" + mapping.SourceSHA256[:4], SourceVersionID: "version",
		ContentVersionID: "content", DocbankOccurrenceID: occurrenceID, CoverageState: "unprocessed",
	})
	require.NoError(t, err)
	require.True(t, applied)
}

func liveMessageIDs(t *testing.T, st *store.Store) []string {
	t.Helper()
	mappings, err := st.ListLiveBeeperMediaMappings(t.Context(), "live", "", 100)
	require.NoError(t, err)
	ids := []string{}
	for _, mapping := range mappings {
		ids = append(ids, mapping.SourceMessageID+"="+mapping.DocbankOccurrenceID)
	}
	return ids
}

func TestBeeperMediaOperationReplay(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newBeeperMediaFixture(t)
	audio := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, "message-1", strings.Repeat("a", 64))
	mapping := audio.mapping("destination-a", "revision-1", "processing-a")
	require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), mapping))

	operation, ok, err := f.Store.NextBeeperMediaOperation(t.Context(), mapping.DestinationKey, time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(store.BeeperMediaOperationRetain, operation.Kind)
	parsed, err := uuid.Parse(operation.OperationID)
	require.NoError(err)
	assert.Equal(uuid.Version(4), parsed.Version())
	first, err := f.Store.PrepareBeeperMediaOperation(t.Context(), operation)
	require.NoError(err)
	second, err := f.Store.PrepareBeeperMediaOperation(t.Context(), operation)
	require.NoError(err)
	assert.Equal(operation.OperationID, first.OperationID)
	assert.Equal(first, second)
	assert.Equal(mapping.OccurrenceJSON, first.FrozenRequestJSON)

	wrong := first
	wrong.OperationID = uuid.NewString()
	applied, err := f.Store.FinishBeeperMediaOperation(t.Context(), wrong, store.BeeperMediaResult{
		VaultUID: "vault", DocbankSourceID: "wrong", DocbankOccurrenceID: "wrong"})
	require.NoError(err)
	assert.False(applied)
	applied, err = f.Store.FinishBeeperMediaOperation(t.Context(), first, store.BeeperMediaResult{
		VaultUID: "vault", DocbankSourceID: "source", SourceVersionID: "version",
		ContentVersionID: "content", DocbankOccurrenceID: "occurrence", CoverageState: "unprocessed"})
	require.NoError(err)
	assert.True(applied)
	applied, err = f.Store.FinishBeeperMediaOperation(t.Context(), first, store.BeeperMediaResult{
		VaultUID: "vault", DocbankSourceID: "late", DocbankOccurrenceID: "late"})
	require.NoError(err)
	assert.False(applied)

	artifact, ok, err := f.Store.NextBeeperMediaOperation(t.Context(), mapping.DestinationKey, time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(store.BeeperMediaOperationArtifact, artifact.Kind)
	artifact.FrozenRequestJSON = `{"occurrence_id":"occurrence","sha256":"first"}`
	artifact.DocbankSourceID, artifact.DocbankOccurrenceID = "source", "occurrence"
	savedArtifact, err := f.Store.PrepareBeeperMediaOperation(t.Context(), artifact)
	require.NoError(err)
	artifact.FrozenRequestJSON = `{"occurrence_id":"other","sha256":"changed"}`
	replayed, err := f.Store.PrepareBeeperMediaOperation(t.Context(), artifact)
	require.NoError(err)
	assert.Equal(savedArtifact.OperationID, replayed.OperationID)
	assert.JSONEq(`{"occurrence_id":"occurrence","sha256":"first"}`, replayed.FrozenRequestJSON)
	applied, err = f.Store.FinishBeeperMediaOperation(t.Context(), replayed, store.BeeperMediaResult{SuppliedInputID: "input"})
	require.NoError(err)
	assert.True(applied)

	process, ok, err := f.Store.NextBeeperMediaOperation(t.Context(), mapping.DestinationKey, time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(store.BeeperMediaOperationProcess, process.Kind)
	assert.Equal("input", process.SuppliedInputID)
	assert.NotEqual(replayed.OperationID, process.OperationID)
	process.FrozenRequestJSON = `{"profile":"supplied-transcript","supplied_input_id":"input"}`
	process, err = f.Store.PrepareBeeperMediaOperation(t.Context(), process)
	require.NoError(err)
	assert.Equal("source", process.DocbankSourceID)
	applied, err = f.Store.FinishBeeperMediaOperation(t.Context(), process, store.BeeperMediaResult{
		JobID: "job", OperationState: "queued", CoverageState: "pending"})
	require.NoError(err)
	assert.True(applied)

	status, ok, err := f.Store.NextBeeperMediaOperation(t.Context(), mapping.DestinationKey, time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(store.BeeperMediaOperationStatus, status.Kind)
	assert.Equal(process.OperationID, status.OperationID)
	applied, err = f.Store.FinishBeeperMediaOperation(t.Context(), status, store.BeeperMediaResult{OperationState: "running"})
	require.NoError(err)
	assert.True(applied)
	// A rejected poll blocks the delivery but keeps its job for restart.
	applied, err = f.Store.FinishBeeperMediaOperation(t.Context(), status, store.BeeperMediaResult{ErrorCode: "unauthorized"})
	require.NoError(err)
	assert.True(applied)
	_, ok, err = f.Store.NextBeeperMediaOperation(t.Context(), mapping.DestinationKey, time.Now().UTC().Add(time.Hour))
	require.NoError(err)
	assert.False(ok)
	require.NoError(f.Store.ReconsiderBlockedBeeperMediaOperations(t.Context(), mapping.DestinationKey))
	resumed, ok, err := f.Store.NextBeeperMediaOperation(t.Context(), mapping.DestinationKey, time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(store.BeeperMediaOperationStatus, resumed.Kind)
	assert.Equal("job", resumed.JobID)
	assert.Equal(process.OperationID, resumed.OperationID)
	applied, err = f.Store.FinishBeeperMediaOperation(t.Context(), resumed, store.BeeperMediaResult{
		OperationState: "succeeded", CoverageState: "transcribed", Terminal: true})
	require.NoError(err)
	assert.True(applied)
	var phase, operationState, coverage string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT phase, operation_state, coverage_state FROM beeper_media_deliveries
		WHERE destination_key = ? AND processing_key = ?`), mapping.DestinationKey, mapping.ProcessingKey).
		Scan(&phase, &operationState, &coverage))
	assert.Equal("done", phase)
	assert.Equal("succeeded", operationState)
	assert.Equal("transcribed", coverage)

	// A completion for a superseded revision cannot publish over the new one.
	other := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, "message-2", strings.Repeat("c", 64))
	old := other.mapping("destination-a", "revision-old", "")
	require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), old))
	oldOperation, err := f.Store.PrepareBeeperMediaOperation(t.Context(), retainOperation(old))
	require.NoError(err)
	newer := other.mapping("destination-a", "revision-new", "")
	require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), newer))
	applied, err = f.Store.FinishBeeperMediaOperation(t.Context(), oldOperation, store.BeeperMediaResult{
		VaultUID: "vault", DocbankSourceID: "stale", DocbankOccurrenceID: "stale"})
	require.NoError(err)
	assert.False(applied)
	newOperation, err := f.Store.PrepareBeeperMediaOperation(t.Context(), retainOperation(newer))
	require.NoError(err)
	assert.NotEqual(oldOperation.OperationID, newOperation.OperationID)
	var oldState, oldSource string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT retention_state, source_id FROM beeper_media_occurrences
		WHERE destination_key = ? AND revision = ?`), "destination-a", "revision-old").Scan(&oldState, &oldSource))
	assert.Equal("revoked", oldState)
	assert.Empty(oldSource)
}

// TestBeeperMediaReconsiderBlocked reopens remote blocks at daemon start while
// local source gaps and revoked rows stay put.
func TestBeeperMediaReconsiderBlocked(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newBeeperMediaFixture(t)
	finish := map[string]store.BeeperMediaResult{
		"remote":    {ErrorCode: "forbidden"},
		"codec":     {ErrorCode: "unsupported_media"},
		"source":    {ErrorCode: "source_changed"},
		"withdrawn": {ErrorCode: "no_live_occurrence", Revoked: true},
	}
	for i, name := range []string{"remote", "codec", "source", "withdrawn"} {
		audio := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, name, strings.Repeat(string(rune('a'+i)), 64))
		mapping := audio.mapping("reconsider", "r1", "")
		require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), mapping))
		prepared, err := f.Store.PrepareBeeperMediaOperation(t.Context(), retainOperation(mapping))
		require.NoError(err)
		applied, err := f.Store.FinishBeeperMediaOperation(t.Context(), prepared, finish[name])
		require.NoError(err)
		require.True(applied)
	}

	require.NoError(f.Store.ReconsiderBlockedBeeperMediaOperations(t.Context(), "reconsider"))
	rows, err := f.Store.DB().Query(f.Store.Rebind(`
		SELECT source_message_id, retention_state, next_action_at IS NOT NULL
		FROM beeper_media_occurrences WHERE destination_key = ?`), "reconsider")
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	states := map[string]string{}
	for rows.Next() {
		var message, state string
		var scheduled bool
		require.NoError(rows.Scan(&message, &state, &scheduled))
		states[message] = fmt.Sprintf("%s:%t", state, scheduled)
	}
	require.NoError(rows.Err())
	assert.Equal(map[string]string{
		"remote": "pending:true", "codec": "blocked:false", "source": "blocked:false", "withdrawn": "revoked:false",
	}, states)
	operation, ok, err := f.Store.NextBeeperMediaOperation(t.Context(), "reconsider", time.Now().UTC().Add(time.Hour))
	require.NoError(err)
	require.True(ok)
	assert.Equal("msgvault:remote", operation.OccurrenceRef)
}

func TestBeeperMediaLiveMappings(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newBeeperMediaFixture(t)
	hash := strings.Repeat("d", 64)
	first := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, "m1", hash)
	second := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, "m2", hash)
	firstMapping, secondMapping := first.mapping("live", "r1", ""), second.mapping("live", "r1", "")
	retainAudio(t, f.Store, firstMapping, "occurrence-1")
	retainAudio(t, f.Store, secondMapping, "occurrence-2")
	assert.Equal([]string{"m1=occurrence-1", "m2=occurrence-2"}, liveMessageIDs(t, f.Store))

	_, err := f.Store.MergeDuplicates(second.messageID, []int64{first.messageID}, "batch-hide")
	require.NoError(err)
	assert.Equal([]string{"m2=occurrence-2"}, liveMessageIDs(t, f.Store))
	_, err = f.Store.UndoDedup("batch-hide")
	require.NoError(err)
	require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), firstMapping))
	assert.Equal([]string{"m1=occurrence-1", "m2=occurrence-2"}, liveMessageIDs(t, f.Store))

	require.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "m1"))
	assert.Equal([]string{"m2=occurrence-2"}, liveMessageIDs(t, f.Store))
	require.NoError(f.Store.ClearMessageDeletedFromSource(f.Source.ID, "m1"))
	require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), firstMapping))
	assert.Equal([]string{"m1=occurrence-1", "m2=occurrence-2"}, liveMessageIDs(t, f.Store))

	// Same-key byte replacement withdraws only the replaced occurrence.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET content_hash = ? WHERE id = ?`),
		strings.Repeat("e", 64), second.attachmentID)
	require.NoError(err)
	assert.Equal([]string{"m1=occurrence-1"}, liveMessageIDs(t, f.Store))

	// A changed raw archive withholds the mapping until reconciliation.
	require.NoError(f.Store.UpsertMessageRawWithFormat(first.messageID,
		[]byte(`{"id":"m1","edited":true,"attachments":[{"id":"mxc://audio/m1"}]}`), "beeper_json"))
	assert.Empty(liveMessageIDs(t, f.Store))
	changed := firstMapping
	digest := sha256.Sum256([]byte(`{"id":"m1","edited":true,"attachments":[{"id":"mxc://audio/m1"}]}`))
	changed.RawHash = hex.EncodeToString(digest[:])
	require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), changed))
	assert.Equal([]string{"m1=occurrence-1"}, liveMessageIDs(t, f.Store))

	// Hard deletion removes the last live occurrence.
	_, err = f.Store.MergeDuplicates(second.messageID, []int64{first.messageID}, "batch-delete")
	require.NoError(err)
	_, err = f.Store.DeleteDedupedBatch("batch-delete")
	require.NoError(err)
	assert.Empty(liveMessageIDs(t, f.Store))
	var revoked int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM beeper_media_occurrences WHERE retention_state = 'revoked'`).Scan(&revoked))
	assert.Equal(2, revoked)
}

func TestBeeperMediaDiscovery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newBeeperMediaFixture(t)
	var audios []beeperAudio
	for i := range 101 {
		audios = append(audios, addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID,
			fmt.Sprintf("m%03d", i), fmt.Sprintf("%064x", i+1)))
	}
	gmail, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	gmailConversation, err := f.Store.EnsureConversation(gmail.ID, "gmail-thread", "Thread")
	require.NoError(err)
	addBeeperAudio(t, f.Store, gmail.ID, gmailConversation, "gmail-audio", strings.Repeat("f", 64))
	_, err = f.Store.MergeDuplicates(audios[1].messageID, []int64{audios[0].messageID}, "hidden")
	require.NoError(err)

	page, err := f.Store.ListBeeperMediaCandidates(t.Context(), 0, 100)
	require.NoError(err)
	require.Len(page, 100)
	assert.Equal(audios[1].attachmentID, page[0].AttachmentID)
	rest, err := f.Store.ListBeeperMediaCandidates(t.Context(), page[99].AttachmentID, 100)
	require.NoError(err)
	assert.Empty(rest)
	_, err = f.Store.ListBeeperMediaCandidates(t.Context(), 0, 1001)
	require.Error(err)

	scan, err := f.Store.LoadBeeperMediaScan(t.Context(), "discovery")
	require.NoError(err)
	advanced := store.BeeperMediaScan{DestinationKey: "discovery", AfterAttachmentID: page[99].AttachmentID, PassHighWater: 200}
	swapped, err := f.Store.AdvanceBeeperMediaScan(t.Context(), "discovery", scan, advanced)
	require.NoError(err)
	assert.True(swapped)
	swapped, err = f.Store.AdvanceBeeperMediaScan(t.Context(), "discovery", scan, store.BeeperMediaScan{DestinationKey: "discovery"})
	require.NoError(err)
	assert.False(swapped)
	loaded, err := f.Store.LoadBeeperMediaScan(t.Context(), "discovery")
	require.NoError(err)
	assert.Equal(advanced, loaded)

	// A failing item moves behind its peers instead of starving them.
	poison, healthy := audios[1].mapping("discovery", "r", ""), audios[2].mapping("discovery", "r", "")
	require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), poison))
	require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), healthy))
	next, ok, err := f.Store.NextBeeperMediaOperation(t.Context(), "discovery", time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(poison.OccurrenceRef, next.OccurrenceRef)
	applied, err := f.Store.FinishBeeperMediaOperation(t.Context(), next, store.BeeperMediaResult{ErrorCode: "server_error", Retry: true})
	require.NoError(err)
	require.True(applied)
	next, ok, err = f.Store.NextBeeperMediaOperation(t.Context(), "discovery", time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(healthy.OccurrenceRef, next.OccurrenceRef)

	// Registration can be abandoned and repeated; the journal replays only
	// after a completed reconciliation and advances after local commits.
	consumer, created, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey)
	require.NoError(err)
	require.True(created)
	require.NoError(f.Store.UnregisterAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey))
	again, created, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey)
	require.NoError(err)
	require.True(created)
	assert.GreaterOrEqual(again.BaselineSequence, consumer.BaselineSequence)
	_, err = f.Store.ListAttachmentChanges(t.Context(), store.BeeperMediaAttachmentConsumerKey, 100)
	require.Error(err)
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(t.Context(), store.BeeperMediaAttachmentConsumerKey, again.BaselineSequence))
	late := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, "late", strings.Repeat("9", 64))
	changes, err := f.Store.ListAttachmentChanges(t.Context(), store.BeeperMediaAttachmentConsumerKey, 100)
	require.NoError(err)
	require.NotEmpty(changes)
	require.NoError(f.Store.ReconcileBeeperMediaMapping(t.Context(), late.mapping("discovery", "r", "")))
	require.NoError(f.Store.AdvanceAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey, changes[len(changes)-1].Sequence))
	changes, err = f.Store.ListAttachmentChanges(t.Context(), store.BeeperMediaAttachmentConsumerKey, 100)
	require.NoError(err)
	assert.Empty(changes)
}

func TestBeeperMediaSchemaReopen(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	open := func() *store.Store {
		st, err := store.Open(path)
		require.NoError(err)
		require.NoError(st.InitSchema())
		return st
	}

	st := open()
	source, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "default-thread", "Thread")
	require.NoError(err)
	audio := addBeeperAudio(t, st, source.ID, conversation, "message-1", strings.Repeat("a", 64))
	_, err = st.DB().Exec(`DROP TABLE beeper_media_deliveries`)
	require.NoError(err)
	_, err = st.DB().Exec(`DROP TABLE beeper_media_occurrences`)
	require.NoError(err)
	require.NoError(st.Close())

	// An archive written before this route opens with the new tables.
	st = open()
	var messages int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
	mapping := audio.mapping("destination", "revision", "processing")
	require.NoError(st.ReconcileBeeperMediaMapping(t.Context(), mapping))
	pending, ok, err := st.NextBeeperMediaOperation(t.Context(), "destination", time.Now().UTC())
	require.NoError(err)
	require.True(ok)

	invalid := mapping
	invalid.OccurrenceRef, invalid.SourceSHA256 = "msgvault:invalid", "not-a-digest"
	require.Error(st.ReconcileBeeperMediaMapping(t.Context(), invalid))
	_, err = st.DB().Exec(`INSERT INTO beeper_media_occurrences
		(destination_key, occurrence_ref, revision, source_type, source_identifier, source_conversation_id,
		 source_message_id, source_attachment_id, source_part_key, source_sha256, byte_length,
		 occurrence_json, request_filename, request_mime_type)
		VALUES ('destination', 'msgvault:message-1', 'revision', 'beeper', 'signal', 'c', 'm', 'a', 'p',
		        'x', 1, '{}', 'f', 'audio/wav')`)
	require.Error(err)
	require.NoError(st.Close())

	st = open()
	defer func() { require.NoError(st.Close()) }()
	replayed, ok, err := st.NextBeeperMediaOperation(t.Context(), "destination", time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(pending.OperationID, replayed.OperationID)
	var occurrences, deliveries int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM beeper_media_occurrences`).Scan(&occurrences))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM beeper_media_deliveries`).Scan(&deliveries))
	assert.Equal(1, occurrences)
	assert.Equal(1, deliveries)
}
