package imessage

import (
	"context"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"log/slog"
	"testing"
)

// Cancel only on an observable repeated native failure, rather than imposing
// a runner-speed deadline on importer correctness. The reader and writer are real.
type repeatedNativeFailure struct {
	seen   map[int64]bool
	repeat bool
	cancel context.CancelFunc
}

func (h *repeatedNativeFailure) Enabled(context.Context, slog.Level) bool { return true }
func (h *repeatedNativeFailure) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *repeatedNativeFailure) WithGroup(string) slog.Handler            { return h }
func (h *repeatedNativeFailure) Handle(_ context.Context, record slog.Record) error {
	if record.Message != "failed to import message" {
		return nil
	}
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "rowid" {
			id := attr.Value.Int64()
			if h.seen[id] {
				h.repeat = true
				h.cancel()
			}
			h.seen[id] = true
		}
		return true
	})
	return nil
}

func TestImportImessageFailedPageAdvancesPastNativeRow(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path, db := newChatDB(t)
	_, err := db.Exec(`INSERT INTO message(date,text) VALUES(757425600,'Synthetic first'),(757425601,'Synthetic second')`)
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("apple_messages", "synthetic-device")
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_imessage_first_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.body_text='Synthetic first' THEN RAISE EXCEPTION 'synthetic first body failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imessage_first_abort BEFORE INSERT ON message_bodies FOR EACH ROW EXECUTE FUNCTION synthetic_imessage_first_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imessage_first_abort BEFORE INSERT ON message_bodies WHEN NEW.body_text='Synthetic first' BEGIN SELECT RAISE(ABORT,'synthetic first body failure'); END`)
	}
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observation := &repeatedNativeFailure{seen: make(map[int64]bool), cancel: cancel}
	client, err := NewClient(path, WithImessageLogger(slog.New(observation)))
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, client.Close()) })
	client.pageSize = 1
	summary, err := client.Import(ctx, st, source.ID)
	require.NoError(err, "per-message continuation must advance the native cursor")
	assert.False(observation.repeat)
	assert.Equal(1, summary.MessagesImported)
	assert.Equal(1, summary.Skipped)
	var id string
	require.NoError(st.DB().QueryRow(`SELECT source_message_id FROM messages`).Scan(&id))
	assert.Equal("2", id)
}
