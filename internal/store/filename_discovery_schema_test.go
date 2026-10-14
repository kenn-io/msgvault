package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Missing projection infrastructure, lost key uniqueness/cascades, or archive
// visibility coupling must fail these real database operations on both backends.
func TestFilenameDiscoverySchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := testutil.NewTestStore(t)
	var generation, revision, after, end, baseline, fence, last, skipped int64
	var phase string
	var gcTurn bool
	require.NoError(s.DB().QueryRow(`SELECT generation, revision, phase, scan_after,
		scan_end, baseline, catchup_fence, last_sequence, skipped, gc_turn
		FROM filename_discovery_state WHERE singleton = 1`).Scan(
		&generation, &revision, &phase, &after, &end, &baseline, &fence, &last, &skipped, &gcTurn))
	assert.Equal([]int64{0, 0, 0, 0, 0, 0, 0, 0}, []int64{generation, revision, after, end, baseline, fence, last, skipped})
	assert.Equal("bootstrap", phase)
	assert.False(gcTurn)
	_, err := s.DB().Exec(`INSERT INTO filename_discovery_state(singleton) VALUES(2)`)
	require.Error(err, "only the singleton state is valid")
	_, err = s.DB().Exec(`INSERT INTO filename_discovery_state(singleton) VALUES(1)`)
	require.Error(err, "singleton must be unique")
	_, err = s.DB().Exec(`INSERT INTO filename_discovery_state(singleton) VALUES(NULL)`)
	require.Error(err, "a null key cannot bypass singleton uniqueness")
	_, err = s.DB().Exec(`UPDATE filename_discovery_state SET phase='invalid' WHERE singleton=1`)
	require.Error(err, "only bootstrap, catchup, or ready maintenance states are valid")

	// Four-byte runes fill a permitted 512-rune full name. Only its fixed hash
	// is indexed, so PostgreSQL must not reject an oversized btree name key.
	name := strings.Repeat(strings.Repeat("𐐨", 63)+" ", 8)
	hash := sha256.Sum256([]byte(name))
	key := hex.EncodeToString(hash[:])
	for _, generation := range []int64{1, 2} {
		_, err = s.DB().Exec(s.Rebind(`INSERT INTO filename_discovery_entries
			(generation, attachment_id, normalized_name, name_hash, applied_sequence, skipped)
			VALUES (?, 991, ?, ?, 0, FALSE)`), generation, name, key)
		require.NoError(err, "projection does not depend on extant/live archive rows")
	}
	_, err = s.DB().Exec(s.Rebind(`INSERT INTO filename_discovery_entries
		(generation, attachment_id, normalized_name, name_hash) VALUES(1,991,?,?)`), name, key)
	require.Error(err, "one occurrence per generation")
	var id int64
	require.NoError(s.DB().QueryRow(s.Rebind(`SELECT attachment_id FROM filename_discovery_entries
		WHERE generation=1 AND name_hash=? ORDER BY attachment_id LIMIT 1`), key).Scan(&id))
	assert.Equal(int64(991), id)
	for _, token := range []string{"c4", "62", "61"} {
		_, err = s.DB().Exec(s.Rebind(`INSERT INTO filename_discovery_tokens
			(generation, token_key, attachment_id) VALUES(1,?,991)`), token)
		require.NoError(err)
		_, err = s.DB().Exec(s.Rebind(`INSERT INTO filename_discovery_deletes
			(generation, delete_key, token_key, attachment_id) VALUES(1,'60',?,991)`), token)
		require.NoError(err)
	}
	rows, err := s.DB().Query(`SELECT token_key FROM filename_discovery_tokens
		WHERE generation=1 AND token_key>='61' AND token_key<'d0' ORDER BY token_key, attachment_id`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var tokens []string
	for rows.Next() {
		var token string
		require.NoError(rows.Scan(&token))
		tokens = append(tokens, token)
	}
	require.NoError(rows.Err())
	require.NoError(rows.Close())
	assert.Equal([]string{"61", "62", "c4"}, tokens)
	_, err = s.DB().Exec(`INSERT INTO filename_discovery_tokens VALUES(1,'61',991)`)
	require.Error(err, "tokens are unique per occurrence")
	_, err = s.DB().Exec(`INSERT INTO filename_discovery_deletes VALUES(1,'60','61',991)`)
	require.Error(err, "deletion postings are unique per token/occurrence")
	_, err = s.DB().Exec(`INSERT INTO filename_discovery_tokens VALUES(1,'61',992)`)
	require.Error(err, "posting requires its projection entry")
	_, err = s.DB().Exec(`INSERT INTO filename_discovery_deletes VALUES(1,'60','61',992)`)
	require.Error(err, "deletion posting requires its projection entry")
	_, err = s.DB().Exec(`INSERT INTO filename_discovery_tokens VALUES(2,'7a34',991)`)
	require.NoError(err)
	_, err = s.DB().Exec(`INSERT INTO filename_discovery_deletes VALUES(2,'7a','7a34',991)`)
	require.NoError(err)
	_, err = s.DB().Exec(`DELETE FROM filename_discovery_entries WHERE generation=1 AND attachment_id=991`)
	require.NoError(err)
	for _, table := range []string{"filename_discovery_tokens", "filename_discovery_deletes"} {
		var count int
		require.NoError(s.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count))
		assert.Equal(1, count, "entry deletion preserves the other generation's posting")
		require.NoError(s.DB().QueryRow(`SELECT count(*) FROM ` + table + ` WHERE generation=1`).Scan(&count))
		assert.Zero(count, "entry deletion cascades all of its own postings")
	}
	var count int
	require.NoError(s.DB().QueryRow(`SELECT count(*) FROM filename_discovery_entries WHERE generation=2`).Scan(&count))
	assert.Equal(1, count)
}

func TestFilenameDiscoveryUpgradeEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	message := f.CreateMessage("filename-upgrade")
	id := createJournalAttachment(t, f, message, "part:1", "a")
	// Represent the upstream v5 archive, retaining its recording reference
	// state and attachment while removing only the new projection infrastructure.
	input := store.RecordingReferenceInput{RouteKey: "route", RefSHA256: "synthetic-hash", OccurrenceJSON: `{"ref":"synthetic-recording"}`}
	require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", message, true, []store.RecordingReferenceInput{input}))
	before, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
	require.NoError(err)
	require.Len(before, 1)
	for _, table := range []string{"filename_discovery_deletes", "filename_discovery_tokens", "filename_discovery_entries", "filename_discovery_state"} {
		_, err := f.Store.DB().Exec(`DROP TABLE IF EXISTS ` + table)
		require.NoError(err)
	}
	_, err = f.Store.DB().Exec(`UPDATE archive_metadata SET value='5' WHERE key='schema_version'`)
	require.NoError(err)
	require.ErrorContains(f.Store.ValidateSchemaContext(t.Context()), "run setup", "v5 readers must request setup rather than silently certify v6")
	for range 2 {
		require.NoError(f.Store.InitSchemaContext(t.Context()))
		version, err := f.Store.SchemaVersionContext(t.Context())
		require.NoError(err)
		assert.Equal(store.SchemaVersion, version)
		after, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
		require.NoError(err)
		assert.Equal(before, after, "migration must preserve upstream recording state and occurrence identity")
		for _, table := range []string{"filename_discovery_entries", "filename_discovery_tokens", "filename_discovery_deletes"} {
			var count int
			require.NoError(f.Store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count))
			assert.Zero(count, "initialization creates no projected archive rows")
		}
		var generation int64
		require.NoError(f.Store.DB().QueryRow(`SELECT generation FROM filename_discovery_state WHERE singleton=1`).Scan(&generation))
		assert.Zero(generation, "only maintenance starts a generation")
	}
	var extant int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM attachments WHERE id=?`), id).Scan(&extant))
	assert.Equal(id, extant)
	var consumers int
	require.NoError(f.Store.DB().QueryRow(`SELECT count(*) FROM attachment_change_consumers WHERE consumer_key='filename-discovery/v1'`).Scan(&consumers))
	assert.Zero(consumers, "schema setup must not register or traverse")
}

func TestFilenameDiscoveryReadOnlyOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var location string
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		s := testutil.NewTestStore(t)
		var database, schema string
		require.NoError(s.DB().QueryRow(`SELECT current_database(), current_schema()`).Scan(&database, &schema))
		u, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(err)
		u.Path = "/" + database
		params := u.Query()
		params.Set("search_path", schema)
		u.RawQuery = params.Encode()
		location = u.String()
	} else {
		location = filepath.Join(t.TempDir(), "readonly.db")
		s, err := store.OpenForTest(location)
		require.NoError(err)
		require.NoError(s.InitSchemaContext(t.Context()))
		require.NoError(s.Close())
	}
	ro, err := store.OpenReadOnlyContext(t.Context(), location)
	require.NoError(err, "read-only backend rejects any attempted DDL")
	t.Cleanup(func() { assert.NoError(ro.Close()) })
	require.NoError(ro.ValidateSchemaContext(t.Context()), "runtime reader accepts the completed exact version")
	version, err := ro.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
	var generation int64
	require.NoError(ro.DB().QueryRow(`SELECT generation FROM filename_discovery_state WHERE singleton=1`).Scan(&generation))
	assert.Zero(generation)
	var recordingRows int
	require.NoError(ro.DB().QueryRow(`SELECT count(*) FROM recording_references`).Scan(&recordingRows))
	assert.Zero(recordingRows, "upstream recording schema remains readable without DDL")
	_, err = ro.DB().Exec(`CREATE TABLE filename_readonly_probe(id BIGINT)`)
	require.Error(err, "the real connection enforces read-only access")
}
