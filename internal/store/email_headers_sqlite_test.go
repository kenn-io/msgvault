package store_test

import (
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type emailHeaderWALFixture struct {
	st        *store.Store
	sourceID  int64
	messageID int64
	walPath   string
	indexPage uint32
}

func newEmailHeaderWALFixture(t *testing.T) emailHeaderWALFixture {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	// Use the current schema and projection state, not a reduced schema or a
	// fixture whose projections were deliberately disabled for measurement.
	requirements.NoError(st.InitSchema())
	st.DB().SetMaxOpenConns(1)
	st.DB().SetMaxIdleConns(1)
	// Every subsequent Store call uses this one retained connection. Closing
	// the final connection or auto-checkpointing could hide committed frames.
	_, err := st.DB().Exec(`PRAGMA wal_autocheckpoint = 0`)
	requirements.NoError(err)
	source, err := st.GetOrCreateSource("apple-mail", "wal@example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "wal-thread", "Synthetic thread")
	requirements.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversation,
		SourceMessageID: "wal-message", MessageType: store.MessageTypeEmail,
		Subject: sql.NullString{String: "Synthetic message", Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(messageID, sql.NullString{
		String: `{"unrelated":{"answer":42}}`, Valid: true,
	}))
	var path string
	requirements.NoError(st.DB().QueryRow(
		`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
	requirements.NotEmpty(path, "physical WAL requires a file-backed database")
	var indexPage uint32
	requirements.NoError(st.DB().QueryRow(`SELECT rootpage FROM sqlite_schema
		WHERE type = 'index' AND name = 'idx_messages_content_changed_at'`).Scan(&indexPage))
	requirements.Positive(indexPage, "the production index must remain present")
	return emailHeaderWALFixture{st, source.ID, messageID, path + "-wal", indexPage}
}

func (f emailHeaderWALFixture) checkpoint(t *testing.T) {
	t.Helper()
	var busy, frames, checkpointed int
	require.NoError(t, f.st.DB().QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&busy, &frames, &checkpointed))
	require.Zero(t, busy)
	require.Empty(t, f.walPages(t), "setup WAL must be truncated before measurement")
}

// walPages reads SQLite's 32-byte WAL header and 24-byte frame headers. Page
// numbers and page size are big-endian regardless of the checksum byte order.
// This fixture has only completed transactions and never rolls back a write
// between checkpoint and inspection, so its complete frames are committed.
func (f emailHeaderWALFixture) walPages(t *testing.T) []uint32 {
	t.Helper()
	requirements := require.New(t)
	data, err := os.ReadFile(f.walPath)
	requirements.NoError(err)
	if len(data) == 0 {
		return nil
	}
	requirements.GreaterOrEqual(len(data), 32)
	pageSize := int(binary.BigEndian.Uint32(data[8:12]))
	requirements.GreaterOrEqual(pageSize, 512)
	requirements.LessOrEqual(pageSize, 65536)
	frameSize := 24 + pageSize
	requirements.Zero((len(data)-32)%frameSize, "only complete WAL frames are measured")
	var pages []uint32
	for offset := 32; offset < len(data); offset += frameSize {
		pages = append(pages, binary.BigEndian.Uint32(data[offset:offset+4]))
	}
	if len(pages) > 0 {
		last := len(data) - frameSize
		requirements.NotZero(binary.BigEndian.Uint32(data[last+4:last+8]),
			"last frame must mark transaction commit")
	}
	return pages
}

type emailHeaderSnapshot struct {
	rfcID, metadata, subject sql.NullString
	reply                    sql.NullInt64
	modified, content        string
	revision                 int64
}

func readEmailHeaderSnapshot(t *testing.T, st *store.Store, id int64) emailHeaderSnapshot {
	t.Helper()
	requirements := require.New(t)
	var got emailHeaderSnapshot
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT rfc822_message_id,
		metadata, subject, reply_to_message_id, CAST(last_modified AS TEXT),
		CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), id).Scan(
		&got.rfcID, &got.metadata, &got.subject, &got.reply, &got.modified, &got.content))
	var err error
	got.revision, err = st.DerivedDataRevision()
	requirements.NoError(err)
	return got
}

// Removing the transaction's fence-aware header lock skip must reintroduce WAL
// index writes here, while unchanged facts alone would fail to catch the bug.
func TestEmailHeaderRepairSQLiteWAL(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newEmailHeaderWALFixture(t)
	requirements.NoError(f.st.RecordPstEmailHeadersContext(t.Context(), f.sourceID, f.messageID,
		"child@example.test", "missing-parent@example.test", "root@example.test"))
	runID, err := f.st.StartSync(f.sourceID, "import-emlx")
	requirements.NoError(err)
	scoped := f.st.ScopedToSync(f.sourceID, runID)
	before := readEmailHeaderSnapshot(t, f.st, f.messageID)
	requirements.Equal("child@example.test", before.rfcID.String, "nonempty headers must enter repair")
	f.checkpoint(t)

	// Positive control: a single-message fixture fits this index in its root
	// page. Keep the index and prove the original unfenced API writes it.
	requirements.NoError(f.st.RecordEmailHeadersContext(t.Context(), f.sourceID, f.messageID,
		"child@example.test", "missing-parent@example.test"))
	requirements.Contains(f.walPages(t), f.indexPage,
		"the control must expose writes to the real content_changed_at index")
	assertions.Equal(before, readEmailHeaderSnapshot(t, f.st, f.messageID))
	f.checkpoint(t)

	for range 3 {
		requirements.NoError(scoped.RecordEmailHeadersContext(t.Context(), f.sourceID, f.messageID,
			"child@example.test", "missing-parent@example.test"))
		requirements.NoError(scoped.RecordPstEmailHeadersContext(t.Context(), f.sourceID, f.messageID,
			"child@example.test", "missing-parent@example.test", "root@example.test"))
		// An unresolved parent still uses the same locking helper, but there
		// is no parent to link and therefore no archived fact to change.
		requirements.NoError(scoped.ResolveEmailReplyParentsContext(t.Context(), f.sourceID, 0, nil))
	}
	assertions.Empty(f.walPages(t), "already-fenced idempotent repair must not write WAL pages")
	assertions.Equal(before, readEmailHeaderSnapshot(t, f.st, f.messageID))
}

// Physical idempotence covers valid generated header facts; malformed metadata
// and conflicting stored facts are separate fixed repair regressions.
func FuzzEmailHeaderRepairIdempotentWAL(f *testing.F) {
	f.Add([]byte("email"), uint8(0))
	f.Add([]byte("email-parent"), uint8(2))
	f.Add([]byte("pst-parent-thread"), uint8(7))
	f.Add([]byte{}, uint8(5))
	f.Fuzz(func(t *testing.T, input []byte, variant uint8) {
		assertions := assert.New(t)
		requirements := require.New(t)
		fixture := newEmailHeaderWALFixture(t)
		// Bound materialized fixture size, not the domain drawn by Go's fuzzer.
		if len(input) > 64 {
			input = input[:64]
		}
		id := "id-" + hex.EncodeToString(input) + "@example.test"
		parent, key := "", ""
		if variant&2 != 0 {
			parent = "parent-" + hex.EncodeToString(input) + "@example.test"
		}
		if variant&4 != 0 {
			key = "root-" + hex.EncodeToString(input) + "@example.test"
		}
		record := func(st *store.Store) error {
			if variant&1 != 0 {
				return st.RecordPstEmailHeadersContext(t.Context(), fixture.sourceID,
					fixture.messageID, id, parent, key)
			}
			return st.RecordEmailHeadersContext(t.Context(), fixture.sourceID, fixture.messageID, id, parent)
		}
		runID, err := fixture.st.StartSync(fixture.sourceID, "import-emlx")
		requirements.NoError(err)
		scoped := fixture.st.ScopedToSync(fixture.sourceID, runID)
		requirements.NoError(record(scoped))
		before := readEmailHeaderSnapshot(t, fixture.st, fixture.messageID)
		requirements.Equal(id, before.rfcID.String, "property must exercise nonempty recorded headers")
		fixture.checkpoint(t)
		for range 2 {
			requirements.NoError(record(scoped))
		}
		assertions.Empty(fixture.walPages(t), "repeating completed repair must be physically idempotent")
		assertions.Equal(before, readEmailHeaderSnapshot(t, fixture.st, fixture.messageID))
	})
}
