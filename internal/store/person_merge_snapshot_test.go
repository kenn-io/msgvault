package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"

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

	_, err = st.AddPersonNameContext(context.Background(), survivor.ID, PersonNameInput{
		NameKind: PersonNameFormatted, Formatted: new("Survivor Name"),
		Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(err)
	_, err = st.AddPersonNameContext(context.Background(), absorbed.ID, PersonNameInput{
		NameKind: PersonNameFormatted, Formatted: new("Absorbed Name"),
		Envelope: ValueEnvelopeInput{Source: ProvenanceVCardImport, SourceRef: new("card-2")},
	})
	require.NoError(err)
	_, err = st.RetirePersonUIDAliasContext(context.Background(), "retired-before-merge", &survivor.ID, "test")
	require.NoError(err)
	source, err := st.GetOrCreateSource("gmail", "snapshot@example.com")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(context.Background(), IdentityMatchCandidateInput{
		LeftKind: IdentityMatchPerson, LeftID: survivor.ID,
		RightKind: IdentityMatchPerson, RightID: absorbed.ID,
		Basis: IdentityMatchDisplayName, State: IdentityMatchStateCandidate,
		Source: ProvenanceUser, SourceID: &source.ID,
	})
	require.NoError(err)
	evidence, err := st.AddIdentityMatchEvidenceContext(context.Background(), candidate.ID, IdentityMatchEvidenceInput{
		EvidenceKind: "shared_name", Source: ProvenanceUser, SourceID: &source.ID,
	})
	require.NoError(err)
	_, err = st.db.Exec(`INSERT INTO identity_match_candidate_redirects
		(retired_candidate_id, surviving_candidate_id, endpoints_collapsed)
		VALUES (?, ?, FALSE)`, candidate.ID+1000, candidate.ID)
	require.NoError(err)

	var snapshot personMergeSnapshot
	require.NoError(st.withTxContext(context.Background(), func(tx *loggedTx) error {
		var captureErr error
		snapshot, captureErr = st.capturePersonMergeSnapshotTx(
			context.Background(), tx, survivor.ID, absorbed.ID,
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
	dbURL := skipUnlessPostgresInternal(t)
	assertPersonMergeTableInventory(t, newPGStoreInternal(t, dbURL))
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
		query = `SELECT constraints.table_name, columns.column_name
			FROM information_schema.table_constraints constraints
			JOIN information_schema.key_column_usage columns
			  ON columns.constraint_catalog = constraints.constraint_catalog
			 AND columns.constraint_schema = constraints.constraint_schema
			 AND columns.constraint_name = constraints.constraint_name
			JOIN information_schema.constraint_column_usage target
			  ON target.constraint_catalog = constraints.constraint_catalog
			 AND target.constraint_schema = constraints.constraint_schema
			 AND target.constraint_name = constraints.constraint_name
			WHERE constraints.constraint_type = 'FOREIGN KEY'
			  AND constraints.table_schema = current_schema()
			  AND target.table_schema = current_schema()
			  AND target.table_name = 'persons'
			ORDER BY constraints.table_name, columns.column_name`
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
