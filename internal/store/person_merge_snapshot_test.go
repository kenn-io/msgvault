package store

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCapturePersonMergeSnapshotIncludesRootsBindingsAndReferencedRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, err := Open(filepath.Join(t.TempDir(), "capture.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	survivorParticipant, err := st.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
	require.NoError(err)
	absorbedParticipant, err := st.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
	require.NoError(err)
	survivor, created, err := st.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	require.True(created)
	absorbed, created, err := st.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	require.True(created)

	_, err = st.AddPersonNameContext(t.Context(), survivor.ID, PersonNameInput{
		NameKind: PersonNameFormatted, Formatted: new("Survivor Name"),
		Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	_, err = st.AddPersonNameContext(t.Context(), absorbed.ID, PersonNameInput{
		NameKind: PersonNameFormatted, Formatted: new("Absorbed Name"),
		Envelope: ValueEnvelopeInput{Source: ProvenanceVCardImport, SourceRef: new("card-2")},
	})
	require.NoError(err)
	_, err = st.RetirePersonUIDAliasContext(t.Context(), "retired-before-merge", &survivor.ID, "test")
	require.NoError(err)
	source, err := st.GetOrCreateSource("gmail", "snapshot@example.com")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
		LeftKind: IdentityMatchPerson, LeftID: survivor.ID,
		RightKind: IdentityMatchPerson, RightID: absorbed.ID,
		Basis: IdentityMatchDisplayName, State: IdentityMatchStateCandidate,
		Source: ProvenanceUser, SourceID: &source.ID,
	})
	require.NoError(err)
	evidence, err := st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, IdentityMatchEvidenceInput{
		EvidenceKind: "shared_name", Source: ProvenanceUser, SourceID: &source.ID,
	})
	require.NoError(err)
	_, err = st.db.Exec(`INSERT INTO identity_match_candidate_redirects
		(retired_candidate_id, surviving_candidate_id, endpoints_collapsed)
		VALUES (?, ?, FALSE)`, candidate.ID+1000, candidate.ID)
	require.NoError(err)

	var snapshot personMergeSnapshot
	require.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
		var captureErr error
		snapshot, captureErr = st.capturePersonMergeSnapshotTx(
			t.Context(), tx, survivor.ID, absorbed.ID,
		)
		return captureErr
	}))

	require.Len(snapshot.Persons, 2)
	assert.Equal(survivor.ID, snapshot.Persons[0].ID)
	assert.Equal([]int64{survivorParticipant}, snapshot.Persons[0].ParticipantIDs)
	assert.Equal(absorbed.ID, snapshot.Persons[1].ID)
	assert.Equal([]int64{absorbedParticipant}, snapshot.Persons[1].ParticipantIDs)

	rowsByTable := make(map[string][]personMergeSnapshotRow)
	for _, row := range snapshot.Rows {
		rowsByTable[row.TableName] = append(rowsByTable[row.TableName], row)
	}
	assert.Len(rowsByTable["person_names"], 2)
	assert.Len(rowsByTable["person_uid_aliases"], 1)
	assert.Len(rowsByTable["identity_match_candidates"], 1)
	assert.Len(rowsByTable["identity_match_candidate_redirects"], 1)
	assert.Len(rowsByTable["identity_match_candidate_sources"], 1)
	require.Len(rowsByTable["identity_match_evidence"], 1)
	assert.Len(rowsByTable["identity_match_evidence_sources"], 1)
	assert.Equal(evidence.ID, rowsByTable["identity_match_evidence"][0].RowID)
	assert.Equal(personMergeOriginSurvivor, rowsByTable["person_names"][0].OriginSide)
	assert.Equal(personMergeOriginAbsorbed, rowsByTable["person_names"][1].OriginSide)
	assert.NotEmpty(rowsByTable["person_names"][0].RowKey,
		"numeric primary keys need a stable journal key")
	assert.NotEmpty(rowsByTable["person_uid_aliases"][0].RowKey,
		"text primary keys need a stable journal key")
	assert.NotEmpty(rowsByTable["identity_match_candidate_sources"][0].RowKey,
		"composite primary keys need a stable journal key")

	firstCompressed, firstHash, err := encodePersonMergeSnapshot(snapshot)
	require.NoError(err)
	secondCompressed, secondHash, err := encodePersonMergeSnapshot(snapshot)
	require.NoError(err)
	assert.Equal(firstHash, secondHash)
	assert.Equal(firstCompressed, secondCompressed)
}

func TestPersonMergeTableInventoryClassifiesEveryPersonReference(t *testing.T) {
	require := require.New(t)
	st, err := Open(filepath.Join(t.TempDir(), "inventory.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	assertPersonMergeTableInventory(t, st)
}

func TestPostgresPersonMergeTableInventoryClassifiesEveryPersonReference(t *testing.T) {
	skipUnlessPostgresInternal(t)
	assertPersonMergeTableInventory(t, newPGStoreInternal(t))
}

func assertPersonMergeTableInventory(t *testing.T, st *Store) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)

	actual := make([]string, 0)
	query := `SELECT child.name, foreign_key."from"
		FROM sqlite_master child
		JOIN pragma_foreign_key_list(child.name) foreign_key
		WHERE child.type = 'table' AND child.name NOT LIKE 'sqlite_%'
		  AND foreign_key."table" = 'persons'
		ORDER BY child.name, foreign_key."from"`
	if st.IsPostgreSQL() {
		query = `SELECT child.relname, columns.attname
			FROM pg_catalog.pg_constraint constraints
			JOIN pg_catalog.pg_namespace namespace ON namespace.oid = constraints.connamespace
			JOIN pg_catalog.pg_class child ON child.oid = constraints.conrelid
			JOIN pg_catalog.pg_class target ON target.oid = constraints.confrelid
			CROSS JOIN LATERAL unnest(constraints.conkey) AS key(attnum)
			JOIN pg_catalog.pg_attribute columns
			  ON columns.attrelid = child.oid AND columns.attnum = key.attnum
			WHERE constraints.contype = 'f'
			  AND namespace.nspname = current_schema()
			  AND target.relnamespace = namespace.oid
			  AND target.relname = 'persons'
			ORDER BY child.relname, columns.attname`
	}
	rows, err := st.db.Query(query)
	require.NoError(err)
	for rows.Next() {
		var table, column string
		require.NoError(rows.Scan(&table, &column))
		actual = append(actual, table+"."+column)
	}
	require.NoError(rows.Err())
	require.NoError(rows.Close())
	sort.Strings(actual)

	classified := make([]string, 0)
	for table, spec := range personMergeTableRegistry {
		assert.Equal(table, spec.TableName)
		assert.NotEmpty(spec.KeyColumn, "key column for %s", table)
		for _, reference := range spec.PersonReferences {
			if reference.Kind == personMergeReferenceDirect {
				classified = append(classified, table+"."+reference.IDColumn)
			}
		}
	}
	sort.Strings(classified)
	assert.Equal(actual, classified,
		"every live foreign key to persons must have an explicit merge policy")

	for _, want := range []struct {
		table, idColumn, kindColumn, kindValue string
	}{
		{table: "person_attribute_values", idColumn: "value_record_id", kindColumn: "value_record_type", kindValue: "person"},
		{table: "organization_attribute_values", idColumn: "value_record_id", kindColumn: "value_record_type", kindValue: "person"},
		{table: "identity_match_candidates", idColumn: "left_id", kindColumn: "left_kind", kindValue: "person"},
		{table: "identity_match_candidates", idColumn: "right_id", kindColumn: "right_kind", kindValue: "person"},
	} {
		spec, ok := personMergeTableRegistry[want.table]
		require.True(ok, "polymorphic person table %s is classified", want.table)
		assert.Contains(spec.PersonReferences, personMergeReference{
			Kind: personMergeReferencePolymorphic, IDColumn: want.idColumn,
			KindColumn: want.kindColumn, KindValue: want.kindValue,
		}, "polymorphic reference %s.%s", want.table, want.idColumn)
	}
}

func TestPersonMergeSnapshotCodecIsDeterministicAndDetectsCorruption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	displayName := "Survivor"
	participantID := int64(31)
	snapshot := personMergeSnapshot{
		Version: personMergeSnapshotVersion,
		Persons: []personMergeSnapshotPerson{{
			ID: 7, VCardUID: "uid-7", DisplayName: &displayName,
			Revision: 2, VCardProjectionRevision: 4,
			CreatedAt: "2026-08-18T12:00:00Z", UpdatedAt: "2026-08-19T01:02:03Z",
			ParticipantIDs: []int64{31, 32},
		}},
		Rows: []personMergeSnapshotRow{{
			TableName: "person_media", RowID: 9, OriginSide: personMergeOriginSurvivor,
			ProvenanceKind: personMergeProvenanceParticipantExact,
			ParticipantID:  &participantID,
			Columns: []personMergeSnapshotColumn{
				{Name: "id", Value: personMergeSnapshotValue{Kind: personMergeSnapshotInteger, Integer: new(int64(9))}},
				{Name: "content_hash", Value: personMergeSnapshotValue{Kind: personMergeSnapshotText, Text: new("sha256:abc")}},
				{Name: "inline_data", Value: personMergeSnapshotValue{Kind: personMergeSnapshotBytes, Bytes: []byte{0, 1, 2, 255}}},
				{Name: "active_until", Value: personMergeSnapshotValue{Kind: personMergeSnapshotNull}},
			},
		}},
	}

	firstCompressed, firstHash, err := encodePersonMergeSnapshot(snapshot)
	require.NoError(err)
	secondCompressed, secondHash, err := encodePersonMergeSnapshot(snapshot)
	require.NoError(err)
	assert.Equal(firstHash, secondHash)
	assert.True(bytes.Equal(firstCompressed, secondCompressed), "zlib output must be deterministic")

	decoded, err := decodePersonMergeSnapshot(firstCompressed, firstHash)
	require.NoError(err)
	assert.Equal(snapshot, decoded)

	badHash := firstHash[:63] + "0"
	if badHash == firstHash {
		badHash = firstHash[:63] + "1"
	}
	_, err = decodePersonMergeSnapshot(firstCompressed, badHash)
	require.Error(err)
	require.ErrorIs(err, ErrPersonMergeSnapshotCorrupt)

	corrupt := append([]byte(nil), firstCompressed...)
	corrupt[len(corrupt)/2] ^= 0xff
	_, err = decodePersonMergeSnapshot(corrupt, firstHash)
	require.Error(err)
	require.ErrorIs(err, ErrPersonMergeSnapshotCorrupt)

	unknownVersion := snapshot
	unknownVersion.Version++
	unknownCompressed, unknownHash := encodeUncheckedPersonMergeSnapshot(t, unknownVersion)
	_, err = decodePersonMergeSnapshot(unknownCompressed, unknownHash)
	require.ErrorIs(err, ErrPersonMergeSnapshotCorrupt)
}

func encodeUncheckedPersonMergeSnapshot(
	t *testing.T, snapshot personMergeSnapshot,
) ([]byte, string) {
	t.Helper()
	canonical, err := json.Marshal(snapshot)
	require.NoError(t, err)
	digest := sha256.Sum256(canonical)
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, err = writer.Write(canonical)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return compressed.Bytes(), hex.EncodeToString(digest[:])
}

func TestNormalizePersonMergeSnapshotJSONPreservesLargeIntegers(t *testing.T) {
	value, err := normalizePersonMergeSnapshotValue(
		[]byte(`{"large":9007199254740993,"small":1}`), "JSON",
	)
	require.NoError(t, err)
	require.NotNil(t, value.Text)
	assert.Equal(t, `{"large":9007199254740993,"small":1}`, *value.Text)
}

func TestIntroductionMergeSnapshotsOmitBody(t *testing.T) {
	checkIntroductionMergeSnapshotsOmitBody(t, openTestStore(t))
}

func TestPostgresIntroductionMergeSnapshotsOmitBody(t *testing.T) {
	skipUnlessPostgresInternal(t)
	checkIntroductionMergeSnapshotsOmitBody(t, newPGStoreInternal(t))
}

func checkIntroductionMergeSnapshotsOmitBody(t *testing.T, st *Store) {
	t.Helper()
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "snapshot-intro")
	require.NoError(t, err)
	pa, err := st.EnsureParticipant("intro-a@example.com", "A", "example.com")
	require.NoError(t, err)
	pb, err := st.EnsureParticipant("intro-b@example.com", "B", "example.com")
	require.NoError(t, err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)

	conv, err := st.EnsureConversation(ctx, src.ID, "snapshot-meeting", "Meeting")
	require.NoError(t, err)
	meeting, err := st.UpsertMessage(&Message{ConversationID: conv, SourceID: src.ID, SourceMessageID: "snapshot-context", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(t, err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO message_recipients (message_id,participant_id,recipient_type,email_address) VALUES (?,?,'from','intro-a@example.com'),(?,?,'to','intro-b@example.com')`), meeting, pa, meeting, pb)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageRawWithFormatContext(ctx, meeting, []byte(`{"organizer":{"email":"intro-a@example.com"},"attendees":[{"email":"intro-b@example.com"}]}`), "meeting_json"))
	_, err = st.PreparePersonIntroductionContext(ctx, PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID, Body: "sensitive synthetic text", MeetingMessageID: &meeting}, "owner")
	require.NoError(t, err)
	merged, err := st.MergePersonsContext(ctx, PersonMergeRequest{SurvivorID: a.ID, AbsorbedID: b.ID, ExpectedSurvivorRevision: a.Revision, ExpectedAbsorbedRevision: b.Revision, IdempotencyKey: "intro-snapshot", Actor: "test"})
	require.NoError(t, err)
	var blob []byte
	var digest string
	require.NoError(t, st.DB().QueryRowContext(ctx, st.Rebind(`SELECT snapshot_blob, snapshot_sha256 FROM person_merges WHERE id = ?`), merged.Merge.ID).Scan(&blob, &digest))
	snapshot, err := decodePersonMergeSnapshot(blob, digest)
	require.NoError(t, err)
	var introRows []personMergeSnapshotRow
	for _, row := range snapshot.Rows {
		if row.TableName == "person_introductions" {
			introRows = append(introRows, row)
		}
	}
	require.Len(t, introRows, 1)
	for _, column := range introRows[0].Columns {
		assert.NotEqual(t, "body", column.Name)
	}
	var postJSON string
	require.NoError(t, st.DB().QueryRowContext(ctx, st.Rebind(`SELECT post_merge_row_json FROM person_merge_rows WHERE merge_id = ? AND table_name = 'person_introductions'`), merged.Merge.ID).Scan(&postJSON))
	var post personMergeSnapshotRow
	require.NoError(t, json.Unmarshal([]byte(postJSON), &post))
	for _, column := range post.Columns {
		assert.NotEqual(t, "body", column.Name)
	}
	// Exercise the insertion restoration owner directly with a real body-free
	// merge snapshot. Introduction rows normally remain live after repointing;
	// split preserves later user deletions rather than resurrecting those rows.
	pc, err := st.EnsureParticipant("restored@example.test", "Restored", "example.test")
	require.NoError(t, err)
	restored, _, err := st.CreatePersonFromParticipantContext(ctx, pc)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM messages WHERE id=?`), meeting)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(ctx, `DELETE FROM person_introductions`)
	require.NoError(t, err)
	require.NoError(t, st.withTxContext(ctx, func(tx *loggedTx) error {
		return st.insertPersonSplitSnapshotRowTx(ctx, tx, introRows[0], a.ID, restored.ID, a.ID, b.ID, "", "")
	}))
	var meetingID sql.NullInt64
	require.NoError(t, st.DB().QueryRowContext(ctx, `SELECT meeting_message_id FROM person_introductions`).Scan(&meetingID))
	assert.False(t, meetingID.Valid)
}

func TestIntroductionProtectsUserOwnedPerson(t *testing.T) {
	checkIntroductionProtectsUserOwnedPerson(t, openTestStore(t))
}

func TestPostgresIntroductionProtectsUserOwnedPerson(t *testing.T) {
	skipUnlessPostgresInternal(t)
	checkIntroductionProtectsUserOwnedPerson(t, newPGStoreInternal(t))
}

func checkIntroductionProtectsUserOwnedPerson(t *testing.T, st *Store) {
	t.Helper()
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "protection-intro")
	require.NoError(t, err)
	pa, err := st.EnsureParticipant("protect-a@example.com", "A", "example.com")
	require.NoError(t, err)
	pb, err := st.EnsureParticipant("protect-b@example.com", "B", "example.com")
	require.NoError(t, err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	_, err = st.PreparePersonIntroductionContext(ctx, PersonIntroductionPrepare{SourceID: src.ID, PersonAID: a.ID, PersonBID: b.ID}, "owner")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM person_participants WHERE person_id IN (?, ?)`), a.ID, b.ID)
	require.NoError(t, err)
	for _, person := range []*Person{a, b} {
		require.NoError(t, st.withTxContext(ctx, func(tx *loggedTx) error {
			protected, err := st.personHasUserOwnedStateTx(ctx, tx, person.ID, person.Revision)
			require.NoError(t, err)
			assert.True(t, protected)
			return nil
		}))
	}
}
