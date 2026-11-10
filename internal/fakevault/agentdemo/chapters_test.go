package agentdemo_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/fakevault/agentdemo"
	"go.kenn.io/msgvault/internal/fakevault/agentdemo/agentdemotest"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/msgsegment"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

func archive(t *testing.T, seed uint64) (*store.Store, *agentdemo.Manifest) {
	t.Helper()
	if seed == 1 {
		return agentdemotest.Store(t)
	}
	dir := t.TempDir()
	m, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir, Seed: seed})
	require.NoError(t, err)
	st, err := store.OpenForTest(t.Context(), filepath.Join(dir, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return st, m
}
func count(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), query, args...).Scan(&n))
	return n
}
func loopIDs(t *testing.T, st *store.Store, m *agentdemo.Manifest) []int64 {
	t.Helper()
	result, err := loops.List(t.Context(), st, loops.Options{Limit: 200}, m.AsOf)
	require.NoError(t, err)
	require.False(t, result.CoverageIncomplete)
	ids := []int64{}
	for _, l := range result.Loops {
		ids = append(ids, l.Anchor.ID)
	}
	return ids
}
func TestCast(t *testing.T) {
	st, m := archive(t, 1)
	for handle, s := range m.Sources {
		t.Run(handle, func(t *testing.T) {
			var typ, identifier string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT source_type, identifier FROM sources WHERE id=?", s.ID).Scan(&typ, &identifier))
			assert.Equal(t, s.SourceType, typ)
			assert.Equal(t, s.Identifier, identifier)
		})
	}
	assert := assert.New(t)
	for _, h := range []string{"riley", "pat", "canary", "support_vendor", "noreply", "newsletter", "background"} {
		assert.Zero(count(t, st, "SELECT count(*) FROM person_participants WHERE participant_id=?", m.Participants[h].ID), h)
	}
	for _, h := range []string{"sam", "sam_phone"} {
		assert.Equal(1, count(t, st, "SELECT count(*) FROM person_participants WHERE person_id=? AND participant_id=?", m.Persons["sam"].ID, m.Participants[h].ID))
	}
}
func TestEmailAndChatLoopsMatchExpectations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, m := archive(t, 1)
	want := []int64{}
	for _, h := range append(expectHandles(t, m, "awaiting_owner_reply"), expectHandles(t, m, "awaiting_their_reply")...) {
		require.NotZero(m.Messages[h], h)
		want = append(want, m.Messages[h])
	}
	result, err := loops.List(t.Context(), st, loops.Options{Limit: 200}, m.AsOf)
	require.NoError(err)
	require.False(result.CoverageIncomplete)
	var ids []int64
	for _, l := range result.Loops {
		ids = append(ids, l.Anchor.ID)
	}
	assert.ElementsMatch(want, ids)
	for key, kind := range map[string]string{"awaiting_owner_reply": loops.ReplyOwed, "awaiting_their_reply": loops.AwaitingReply} {
		var expected, actual []int64
		for _, h := range expectHandles(t, m, key) {
			expected = append(expected, m.Messages[h])
		}
		for _, l := range result.Loops {
			if l.Kind == kind {
				actual = append(actual, l.Anchor.ID)
			}
		}
		assert.ElementsMatch(expected, actual, key)
	}
}
func TestEmailDraftProvenance(t *testing.T) {
	st, m := archive(t, 1)
	for _, h := range []string{"sam_trip_sent_from_draft", "sam_trip_unsent_draft"} {
		t.Run(h, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			var authored, own bool
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT draft_authored,is_from_me FROM messages WHERE id=?", m.Messages[h]).Scan(&authored, &own))
			assert.True(authored, h)
			assert.True(own, h)
			var labels []string
			rows, err := st.DB().QueryContext(t.Context(), "SELECT l.name FROM labels l JOIN message_labels ml ON ml.label_id=l.id WHERE ml.message_id=?", m.Messages[h])
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			for rows.Next() {
				var s string
				require.NoError(rows.Scan(&s))
				labels = append(labels, s)
			}
			require.NoError(rows.Err())
			if h == "sam_trip_sent_from_draft" {
				assert.Contains(labels, "SENT")
				assert.NotContains(labels, "DRAFT")
			} else {
				assert.Equal([]string{"DRAFT"}, labels)
			}
		})
	}
}
func TestEmailStarredLabelSynced(t *testing.T) {
	assert := assert.New(t)
	st, m := archive(t, 1)
	ids, total, err := st.SearchMessageIDsQueryContext(t.Context(), search.ParseOR("label:STARRED"), 50)
	require.NoError(t, err)
	assert.Equal(int64(1), total)
	assert.Equal([]int64{m.Messages["priya_budget_question"]}, ids)
}
func TestVoiceOwnMessageCounts(t *testing.T) {
	st, m := archive(t, 1)
	for h, want := range map[string]int{"weekend_crew": 8, "dinner_group": 2, "sam_signal_dm": 7, "sam_imessage_dm": 2, "sam_trip_email": 6} {
		assert.Equal(t, want, count(t, st, `SELECT count(*) FROM messages m WHERE conversation_id=? AND is_from_me=1 AND NOT EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=m.id AND l.name='DRAFT')`, m.Conversations[h].ID), h)
	}
}
func TestSharedAttachmentSpansTwoFamilies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, m := archive(t, 1)
	a := m.Attachments["contract_v2"]
	assert.ElementsMatch([]string{"jordan_contract_question", "priya_contract_share"}, a.MessageHandles)
	assert.Equal(2, count(t, st, "SELECT count(*) FROM attachments WHERE content_hash=?", a.ContentHash))
	ids, total, err := st.SearchMessageIDsQueryContext(t.Context(), search.ParseOR("has:attachment"), 50)
	require.NoError(err)
	assert.Equal(int64(2), total)
	for _, h := range a.MessageHandles {
		require.NotZero(m.Messages[h], h)
		assert.Equal(1, count(t, st, "SELECT count(*) FROM attachments WHERE content_hash=? AND message_id=?", a.ContentHash, m.Messages[h]))
		assert.Contains(ids, m.Messages[h], h)
		var has bool
		var n int
		require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT has_attachments,attachment_count FROM messages WHERE id=?", m.Messages[h]).Scan(&has, &n))
		assert.True(has, h)
		assert.Equal(1, n, h)
	}
}
func TestSignalSyncInterruptedWithCheckpoint(t *testing.T) {
	assert := assert.New(t)
	st, m := archive(t, 1)
	var status string
	var checkpoint sql.NullString
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT status,cursor_before FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1", m.Sources["signal"].ID).Scan(&status, &checkpoint))
	assert.Equal("failed", status)
	assert.True(checkpoint.Valid)
	assert.NotEmpty(checkpoint.String)
}
func TestGenerateIsDeterministicForSeedWithoutDefaultTransportHTTP(t *testing.T) {
	require := require.New(t)
	previous := http.DefaultTransport
	http.DefaultTransport = rejectTransport{t}
	t.Cleanup(func() { http.DefaultTransport = previous })
	a, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: t.TempDir(), Seed: 1})
	require.NoError(err)
	b, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: t.TempDir(), Seed: 1})
	require.NoError(err)
	assert.Equal(t, a, b)
}
func TestSeedChangesFillerOnly(t *testing.T) {
	assert := assert.New(t)
	sa, a := archive(t, 1)
	sb, b := archive(t, 2)
	assert.NotEqual(a.Digest, b.Digest)
	assert.ElementsMatch(loopIDs(t, sa, a), loopIDs(t, sb, b))
	// Every namespace and oracle is stable; only the seed and content digest differ.
	want, got := *a, *b
	want.Seed = got.Seed
	want.Digest = got.Digest
	assert.Equal(want, got)
}
func TestArchiveIntegrity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _ := archive(t, 1)
	var result string
	require.NoError(st.DB().QueryRowContext(t.Context(), "PRAGMA integrity_check").Scan(&result))
	assert.Equal("ok", result)
	rows, err := st.DB().QueryContext(t.Context(), "PRAGMA foreign_key_check")
	require.NoError(err)
	assert.False(rows.Next())
	require.NoError(rows.Err())
	require.NoError(rows.Close())
}

type rejectTransport struct{ t *testing.T }

func (r rejectTransport) RoundTrip(*http.Request) (*http.Response, error) {
	assert.Fail(r.t, "generator attempted default-transport HTTP")
	return nil, errors.New("default-transport HTTP prohibited")
}
func TestGenerateHonorsCancellation(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var progressed []string
	_, err := agentdemo.GenerateDir(ctx, agentdemo.Options{Dir: t.TempDir(), Progress: func(chapter string) {
		progressed = append(progressed, chapter)
		if chapter == "email" {
			cancel()
		}
	}})
	require.Equal([]string{"cast", "email"}, progressed, "cancel after the cast chapter completes")
	require.ErrorIs(err, context.Canceled)
}

var updateGolden = flag.Bool("update", false, "update agent demo golden manifest")

func TestManifestMatchesGolden(t *testing.T) {
	require := require.New(t)
	_, m := archive(t, 1)
	m.Digest = ""
	dir := t.TempDir()
	require.NoError(m.Write(dir))
	got, err := os.ReadFile(filepath.Join(dir, agentdemo.ManifestFile))
	require.NoError(err)
	if *updateGolden {
		require.NoError(os.MkdirAll("testdata", 0700))
		require.NoError(os.WriteFile("testdata/seed1.manifest.golden.json", got, 0600))
	}
	want, err := os.ReadFile("testdata/seed1.manifest.golden.json")
	require.NoError(err)
	if assert.JSONEq(t, string(want), string(got)) {
		return
	}
	// CI retains test events, so export exact bytes when a golden needs updating.
	const maxDumpBytes = 1 << 20
	if len(got) > maxDumpBytes {
		t.Logf("Golden dump omitted: %d bytes exceeds %d-byte diagnostic limit", len(got), maxDumpBytes)
		return
	}
	t.Logf("AGENT_DEMO_GOLDEN_BEGIN bytes=%d sha256=%x", len(got), sha256.Sum256(got))
	for chunk := range slices.Chunk([]byte(base64.StdEncoding.EncodeToString(got)), 128) {
		t.Logf("AGENT_DEMO_GOLDEN_CHUNK %s", chunk)
	}
	t.Log("AGENT_DEMO_GOLDEN_END")
}

func TestCastUsesReservedIdentifiersOnly(t *testing.T) {
	require := require.New(t)
	st, _ := archive(t, 1)
	rows, err := st.DB().QueryContext(t.Context(), "SELECT identifier_value FROM participant_identifiers")
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	for rows.Next() {
		var value string
		require.NoError(rows.Scan(&value))
		assert.Regexp(t, `(@example\.(com|org|net)$|@[a-z-]+\.example$|^\+155501001[0-9]{2}$|^[UB]0[A-Z]+$|^72345$)`, value)
	}
	require.NoError(rows.Err())
}
func TestEmailRecordsCanaries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, m := archive(t, 1)
	var subject string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT subject FROM messages WHERE id=?", m.Messages["canary"]).Scan(&subject))
	assert.Contains(subject, "CANARY-SUBJ-7f3a")
	body, err := st.GetMessageBodyText(t.Context(), m.Messages["canary"])
	require.NoError(err)
	assert.Contains(body, "CANARY-BODY-7f3a")
	assert.Len(m.Canaries, 4)
}
func TestVoiceEmailQuotesAreBelowOwnText(t *testing.T) {
	assert := assert.New(t)
	st, m := archive(t, 1)
	for _, h := range []string{"sam_trip_own_1", "sam_trip_own_2", "sam_trip_sent_from_draft", "sam_trip_own_4", "sam_trip_own_5", "sam_trip_own_6"} {
		body, err := st.GetMessageBodyText(t.Context(), m.Messages[h])
		require.NoError(t, err)
		assert.Contains(body, "On Mon,")
		assert.Contains(body, "> The route")
		quoted := false
		for _, seg := range msgsegment.Split(body).Segments {
			part := body[seg.Start:seg.End]
			if seg.Kind == msgsegment.Authored {
				assert.NotContains(part, "> The route")
				assert.NotContains(part, "On Mon,")
			}
			if seg.Kind == msgsegment.Quoted {
				quoted = true
			}
		}
		assert.True(quoted, h)
	}
}
func expectHandles(t *testing.T, m *agentdemo.Manifest, key string) []string {
	t.Helper()
	data, err := json.Marshal(m.Expect[key])
	require.NoError(t, err)
	var handles []string
	require.NoError(t, json.Unmarshal(data, &handles))
	require.NotEmpty(t, handles)
	return handles
}

func TestVoicePersonScopeMembership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, m := archive(t, 1)
	var oracle struct {
		PersonSam struct {
			Chat  []string `json:"chat"`
			Email []string `json:"email"`
			Never []string `json:"never"`
		} `json:"person_sam"`
	}
	data, err := json.Marshal(m.Expect["voice"])
	require.NoError(err)
	require.NoError(json.Unmarshal(data, &oracle))
	var ids []int64
	for _, id := range m.Messages {
		ids = append(ids, id)
	}
	provenance, err := st.PersonProvenanceForMessages(t.Context(), ids, personscope.Scope{ParticipantIDs: m.Persons["sam"].ParticipantIDs})
	require.NoError(err)
	found := map[string][]string{"chat": {}, "email": {}}
	ownCounts := map[string]int{"chat": 0, "email": 0}
	for handle, c := range m.Conversations {
		if c.ConversationType != "direct_chat" && c.ConversationType != "email_thread" {
			continue
		}
		family := m.Sources[c.Source].Family
		var matches int
		rows, err := st.DB().QueryContext(t.Context(), `SELECT m.id,m.is_from_me,EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=m.id AND l.name='DRAFT') FROM messages m WHERE conversation_id=?`, c.ID)
		require.NoError(err)
		for rows.Next() {
			var id int64
			var own, draft bool
			require.NoError(rows.Scan(&id, &own, &draft))
			p := provenance[id]
			if p == nil {
				continue
			}
			if !slices.Contains(p.Directions, personscope.FromPerson) && !slices.Contains(p.Directions, personscope.ToPerson) {
				continue
			}
			matches++
			if own && !draft {
				ownCounts[family]++
				assert.Contains(p.Roles, personscope.RoleTo)
			}
		}
		require.NoError(rows.Err())
		require.NoError(rows.Close())
		if matches > 0 {
			found[family] = append(found[family], handle)
		}
	}
	assert.ElementsMatch(oracle.PersonSam.Chat, found["chat"])
	assert.ElementsMatch(oracle.PersonSam.Email, found["email"])
	assert.Equal(9, ownCounts["chat"])
	assert.Equal(6, ownCounts["email"])
	for _, h := range oracle.PersonSam.Never {
		require.Contains(m.Conversations, h)
		require.Equal("group_chat", m.Conversations[h].ConversationType)
		require.Positive(count(t, st, "SELECT count(*) FROM messages WHERE conversation_id=?", m.Conversations[h].ID), h)
		rows, err := st.DB().QueryContext(t.Context(), "SELECT id FROM messages WHERE conversation_id=?", m.Conversations[h].ID)
		require.NoError(err)
		for rows.Next() {
			var id int64
			require.NoError(rows.Scan(&id))
			p := provenance[id]
			require.NotNil(p, "group provenance for message %d in %s", id, h)
			assert.Contains(p.Directions, personscope.Group)
		}
		require.NoError(rows.Err())
		require.NoError(rows.Close())
	}
}
func TestArchiveSurvivesBackupRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir, m := agentdemotest.Archive(t)
	verifyBlobs(t, dir)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	app := backupapp.New("agent-demo-test")
	snapshot, err := backup.Create(t.Context(), repo, app, backup.CreateOptions{DBPath: filepath.Join(dir, "msgvault.db"), ContentDir: filepath.Join(dir, "attachments"), DataDir: dir})
	require.NoError(err)
	restoredDir := filepath.Join(t.TempDir(), "restored")
	restored, err := backup.Restore(t.Context(), repo, app, backup.RestoreOptions{TargetDir: restoredDir})
	require.NoError(err)
	assert.Equal(snapshot.SnapshotID, restored.SnapshotID)
	st, err := store.OpenForTest(t.Context(), filepath.Join(restoredDir, "msgvault.db"))
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	assert.Equal(len(m.Messages), count(t, st, "SELECT count(*) FROM messages"))
	verifyBlobs(t, restoredDir)
}
func verifyBlobs(t *testing.T, dir string) {
	t.Helper()
	st, err := store.OpenForTest(t.Context(), filepath.Join(dir, "msgvault.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	rows, err := st.DB().QueryContext(t.Context(), "SELECT content_hash,storage_path,size FROM attachments")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var hash, path string
		var size int64
		require.NoError(t, rows.Scan(&hash, &path, &size))
		assert.Equal(t, hash[:2]+"/"+hash, path)
		data, err := os.ReadFile(filepath.Join(dir, "attachments", filepath.FromSlash(path)))
		require.NoError(t, err)
		sum := sha256.Sum256(data)
		assert.Equal(t, hash, hex.EncodeToString(sum[:]))
		assert.Equal(t, size, int64(len(data)))
	}
	require.NoError(t, rows.Err())
}
func TestDigestIgnoresAllocatedIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	_, baseline := archive(t, 1)
	dir := t.TempDir()
	st, err := store.OpenForTest(t.Context(), filepath.Join(dir, "msgvault.db"))
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	require.NoError(st.InitSchemaContext(t.Context()))
	extra, err := st.GetOrCreateSourceContext(t.Context(), "gmail", "offset@example.net")
	require.NoError(err)
	participant, err := st.EnsureParticipantContext(t.Context(), "offset@example.net", "Offset Example", "example.net")
	require.NoError(err)
	m, err := agentdemo.Generate(t.Context(), st, filepath.Join(dir, "attachments"), agentdemo.Options{Seed: 1, AsOf: agentdemo.DefaultAsOf})
	require.NoError(err)
	assert.NotEqual(baseline.Sources["gmail"].ID, m.Sources["gmail"].ID)
	assert.NotEqual(baseline.Participants["sam"].ID, m.Participants["sam"].ID)
	_, err = st.DB().ExecContext(t.Context(), "DELETE FROM sources WHERE id=?", extra.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), "DELETE FROM participants WHERE id=?", participant)
	require.NoError(err)
	digest, err := agentdemo.Digest(t.Context(), st.DB())
	require.NoError(err)
	assert.Equal(baseline.Digest, digest)
}
