package identityindex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/duckdbutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFactsEdgesAndDirectory(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	root, db := writeIdentityBaseFixture(t, false)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	requirementsForTest.NoError(err)
	assertionsForTest.NotEmpty(result.ConversationParticipantsFingerprint)
	assertionsForTest.Equal(int64(1), result.Stats.TotalMessages)
	assertionsForTest.Equal(int64(1), result.Stats.Sources)
	assertionsForTest.Equal(int64(1), result.Stats.UniqueSenders)
	assertionsForTest.Equal(int64(1), result.Stats.UniqueDomains)
	assertionsForTest.Equal(int64(50), result.Stats.TotalSizeBytes)
	assertionsForTest.Equal(int64(12), result.Stats.AttachmentSizeBytes)
	requirementsForTest.NotNil(result.Stats.MinYear)
	requirementsForTest.NotNil(result.Stats.MaxYear)
	assertionsForTest.Equal(int64(2026), *result.Stats.MinYear)
	assertionsForTest.Equal(int64(2026), *result.Stats.MaxYear)
	assertionsForTest.Equal(int64(1), parquetCount(t, db, root, DatasetEntryFacts))
	assertionsForTest.Equal(int64(2), parquetCount(t, db, root, DatasetDirectEdges))
	assertionsForTest.Equal(int64(1), parquetCount(t, db, root, DatasetConversationEdges))

	var authored, sender bool
	requirementsForTest.NoError(db.QueryRow(identityParquetSQL(root, DatasetDirectEdges)+
		" WHERE participant_id = 1").Scan(new(int64), new(int16), new(int64), new(string), &sender, &authored))
	assertionsForTest.True(sender)
	assertionsForTest.True(authored)

	var conversationID, conversationParticipantID int64
	requirementsForTest.NoError(db.QueryRow(identityParquetSQL(root, DatasetConversationEdges)).
		Scan(&conversationID, &conversationParticipantID, new(string)))
	assertionsForTest.Equal(int64(10), conversationID)
	assertionsForTest.Equal(int64(4), conversationParticipantID)

	var label, memberIDs, searchValues string
	var partial, owner bool
	requirementsForTest.NoError(db.QueryRow(`
		SELECT display_label, partial_label,
		       CAST(to_json(member_ids) AS VARCHAR),
		       CAST(to_json(search_values) AS VARCHAR),
		       is_owner
		FROM read_parquet(?)
		WHERE canonical_id = 2
	`, identityParquetGlob(root, DatasetDirectory)).
		Scan(&label, &partial, &memberIDs, &searchValues, &owner))
	assertionsForTest.Equal("Bob Alias", label)
	assertionsForTest.False(partial)
	assertionsForTest.JSONEq(`[2,3]`, memberIDs)
	assertionsForTest.Contains(searchValues, "bob@example.net")
	assertionsForTest.Contains(searchValues, "bob alias")
	assertionsForTest.False(owner)
}

func TestBuildEmptySchemas(t *testing.T) {
	assertions := assert.New(t)
	root, db := writeIdentityBaseFixture(t, true)
	var progressed []string

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		Progress: func(dataset string, _ time.Duration) {
			progressed = append(progressed, dataset)
		},
	})
	require.NoError(t, err)

	expected := map[string][]string{
		DatasetEntryFacts: {
			"message_id", "conversation_id", "source_id", "source_type",
			"occurred_at", "message_type", "conversation_type", "entry_kind",
			"is_chat", "is_from_me", "has_attachments", "attachment_count",
			"deleted_from_source", "occurred_year",
		},
		DatasetDirectEdges: {
			"message_id", "occurred_year", "participant_id",
			"participant_domain", "is_sender", "is_author",
		},
		DatasetConversationEdges: {
			"conversation_id", "participant_id", "participant_domain",
		},
		DatasetDirectory: {
			"canonical_id", "display_label", "partial_label",
			"member_ids", "search_values", "is_owner",
		},
		DatasetRollups: {
			"canonical_id", "activity_count", "file_count",
			"first_at", "last_at", "source_counts", "source_rollups",
		},
		DatasetDomainRollups: {
			"domain", "activity_count", "person_count", "file_count",
			"first_at", "last_at", "source_counts",
		},
		DatasetRelationships: {
			"canonical_id", "anchor_date", "sent_decayed", "received_decayed",
			"meetings_decayed", "sent_count", "meeting_count",
			"modality_mask", "last_at",
		},
		DatasetRelationshipDaily: {
			"canonical_id", "event_date", "sent_units", "received_units",
			"meeting_units", "sent_count", "meeting_count",
			"modality_mask", "last_at",
		},
	}
	assertions.ElementsMatch([]string{
		DatasetEntryFacts,
		DatasetDirectEdges,
		DatasetConversationEdges,
		DatasetDirectory,
		"logical_activity",
		DatasetRollups,
		DatasetDomainRollups,
		DatasetRelationships,
		DatasetRelationshipDaily,
	}, progressed)
	for dataset, wantColumns := range expected {
		assert.Equal(t, int64(0), parquetCount(t, db, root, dataset), dataset)
		t.Run(dataset, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			rows, queryErr := db.Query(identityParquetSQL(root, dataset) + " LIMIT 0")
			requirements.NoError(queryErr)
			defer func() {
				requirements.NoError(rows.Close())
			}()
			gotColumns, columnsErr := rows.Columns()
			requirements.NoError(columnsErr)
			requirements.NoError(rows.Err())
			assertions.Equal(wantColumns, gotColumns)
		})
	}
}

func TestBuildReusesMaterializedLogicalActivityAndCleansItUp(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeIdentityBaseFixture(t, false)
	moved := make(map[string]string)
	materialized := false

	restoreInputs := func() {
		for original, backup := range moved {
			if _, err := os.Stat(backup); err == nil {
				requirements.NoError(os.Rename(backup, original))
			}
		}
	}
	t.Cleanup(restoreInputs)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		Progress: func(dataset string, _ time.Duration) {
			switch dataset {
			case "logical_activity":
				materialized = true
				for _, name := range []string{
					DatasetEntryFacts,
					DatasetDirectEdges,
					DatasetConversationEdges,
					"participant_clusters",
					"owner_participants",
				} {
					original := filepath.Join(root, name)
					backup := original + ".unavailable"
					requirements.NoError(os.Rename(original, backup))
					moved[original] = backup
				}
			case DatasetRelationshipDaily:
				restoreInputs()
			}
		},
	})
	requirements.NoError(err)
	assertions.True(materialized)

	var temporaryTables int64
	requirements.NoError(db.QueryRow(`
		SELECT count(*)
		FROM duckdb_tables()
		WHERE temporary AND table_name LIKE 'identity_build_%'
	`).Scan(&temporaryTables))
	assertions.Zero(temporaryTables)
}

func TestValidateRejectsIncompleteDatasetSchema(t *testing.T) {
	requirements := require.New(t)
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	requirements.NoError(err)

	oldDirectory := moveIdentityDatasetAside(t, root, DatasetDirectory)
	writeIdentityParquet(t, db, root, DatasetDirectory, `
		SELECT canonical_id, display_label, partial_label, member_ids, search_values
		FROM read_parquet('`+quoteSQLString(identityParquetGlob(oldDirectory, DatasetDirectory))+`')`)

	err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
	requirements.ErrorContains(err, "validate identity_directory schema")

	requirements.NoError(os.RemoveAll(filepath.Join(root, DatasetDirectory)))
	writeIdentityParquet(t, db, root, DatasetDirectory, `
		SELECT canonical_id::VARCHAR AS canonical_id, display_label, partial_label,
		       member_ids, search_values, is_owner
		FROM read_parquet('`+quoteSQLString(identityParquetGlob(oldDirectory, DatasetDirectory))+`')`)
	err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
	requirements.ErrorContains(err,
		"column 1 expected canonical_id BIGINT, got canonical_id VARCHAR")
}

func TestValidateRejectsCachedMessageWithoutFact(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	require.NoError(t, err)

	for _, dataset := range []string{DatasetEntryFacts, DatasetDirectEdges} {
		oldRoot := moveIdentityDatasetAside(t, root, dataset)
		writeIdentityParquet(t, db, root, dataset, `
			SELECT *
			FROM read_parquet('`+quoteSQLString(identityParquetGlob(oldRoot, dataset))+`')
			WHERE false`)
	}

	err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
	require.ErrorContains(t, err, "cached messages have no fact")
}

func TestValidateRejectsDuplicateDomainKeys(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	require.NoError(t, err)

	oldDomains := moveIdentityDatasetAside(t, root, DatasetDomainRollups)
	source := quoteSQLString(identityParquetGlob(oldDomains, DatasetDomainRollups))
	writeIdentityParquet(t, db, root, DatasetDomainRollups, `
		SELECT * FROM read_parquet('`+source+`')
		UNION ALL
		SELECT * FROM read_parquet('`+source+`')`)

	err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
	require.ErrorContains(t, err, "duplicate domain keys")
}

func TestValidateRejectsSourceRollupDecompositionDrift(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	require.NoError(t, err)

	oldRollups := moveIdentityDatasetAside(t, root, DatasetRollups)
	writeIdentityParquet(t, db, root, DatasetRollups, `
		SELECT canonical_id, activity_count, file_count, first_at, last_at,
		       source_counts,
		       list_transform(source_rollups, item -> struct_pack(
			       source_id := item.source_id,
			       source_type := item.source_type,
			       activity_count := item.activity_count + 1,
			       file_count := item.file_count,
			       first_at := item.first_at,
			       last_at := item.last_at
		       )) AS source_rollups
		FROM read_parquet('`+
		quoteSQLString(identityParquetGlob(oldRollups, DatasetRollups))+`')`)

	err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
	require.ErrorContains(t, err, "source rollups do not decompose identity totals")
}

func TestValidateRejectsRelationshipDailyDecompositionDrift(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	require.NoError(t, err)

	oldDaily := moveIdentityDatasetAside(t, root, DatasetRelationshipDaily)
	writeIdentityParquet(t, db, root, DatasetRelationshipDaily, `
		SELECT canonical_id, event_date, sent_units + 1 AS sent_units,
		       received_units, meeting_units, sent_count + 1 AS sent_count,
		       meeting_count, modality_mask, last_at
		FROM read_parquet('`+
		quoteSQLString(identityParquetGlob(oldDaily, DatasetRelationshipDaily))+`')`)

	err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
	require.ErrorContains(t, err, "daily signals do not decompose relationship totals")
}

func TestValidateRejectsDailyRawCountDecompositionDrift(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	require.NoError(t, err)

	oldDaily := moveIdentityDatasetAside(t, root, DatasetRelationshipDaily)
	writeIdentityParquet(t, db, root, DatasetRelationshipDaily, `
		SELECT canonical_id, event_date, sent_units, received_units, meeting_units,
		       sent_count + 1 AS sent_count, meeting_count, modality_mask, last_at
		FROM read_parquet('`+
		quoteSQLString(identityParquetGlob(oldDaily, DatasetRelationshipDaily))+`')`)

	err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
	require.ErrorContains(t, err, "raw count decomposition")
}

func TestValidateRejectsNullRelationshipDecayComponent(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	require.NoError(t, err)

	oldRelationships := moveIdentityDatasetAside(t, root, DatasetRelationships)
	writeIdentityParquet(t, db, root, DatasetRelationships, `
		SELECT canonical_id, anchor_date, NULL::DOUBLE AS sent_decayed,
		       received_decayed, meetings_decayed, sent_count, meeting_count,
		       modality_mask, last_at
		FROM read_parquet('`+
		quoteSQLString(identityParquetGlob(oldRelationships, DatasetRelationships))+`')`)

	err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
	require.ErrorContains(t, err, "invalid decayed relationship components")
}

func TestValidateRejectsIncorrectRelationshipDecayValue(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	require.NoError(t, err)

	oldRelationships := moveIdentityDatasetAside(t, root, DatasetRelationships)
	writeIdentityParquet(t, db, root, DatasetRelationships, `
		SELECT canonical_id, anchor_date, sent_decayed + 0.25 AS sent_decayed,
		       received_decayed, meetings_decayed, sent_count, meeting_count,
		       modality_mask, last_at
		FROM read_parquet('`+
		quoteSQLString(identityParquetGlob(oldRelationships, DatasetRelationships))+`')`)

	err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
	require.ErrorContains(t, err, "decayed signals do not match daily components")
}

func TestValidateRejectsInvalidRelationshipComponentsAndMasks(t *testing.T) {
	tests := []struct {
		name       string
		dataset    string
		projection string
		wantError  string
	}{
		{
			name:    "nonfinite decayed component",
			dataset: DatasetRelationships,
			projection: `
				canonical_id, anchor_date, 'NaN'::DOUBLE AS sent_decayed,
				received_decayed, meetings_decayed, sent_count, meeting_count,
				modality_mask, last_at`,
			wantError: "invalid decayed relationship components",
		},
		{
			name:    "invalid relationship modality mask",
			dataset: DatasetRelationships,
			projection: `
				canonical_id, anchor_date, sent_decayed, received_decayed,
				meetings_decayed, sent_count, meeting_count,
				8::UTINYINT AS modality_mask, last_at`,
			wantError: "invalid raw relationship components or modality mask",
		},
		{
			name:    "negative daily component",
			dataset: DatasetRelationshipDaily,
			projection: `
				canonical_id, event_date, sent_units,
				-1::BIGINT AS received_units, meeting_units, sent_count,
				meeting_count, modality_mask, last_at`,
			wantError: "invalid daily relationship components or modality mask",
		},
		{
			name:    "null daily decomposition component",
			dataset: DatasetRelationshipDaily,
			projection: `
				canonical_id, event_date, sent_units, received_units,
				meeting_units, NULL::BIGINT AS sent_count, meeting_count,
				modality_mask, last_at`,
			wantError: "invalid daily relationship components or modality mask",
		},
		{
			name:    "invalid daily modality mask",
			dataset: DatasetRelationshipDaily,
			projection: `
				canonical_id, event_date, sent_units, received_units,
				meeting_units, sent_count, meeting_count,
				8::UTINYINT AS modality_mask, last_at`,
			wantError: "invalid daily relationship components or modality mask",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, db := writeIdentityBaseFixture(t, false)
			anchor := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
			_, err := Build(context.Background(), db, BuildOptions{
				Mode:           ModeFull,
				StagedBaseRoot: root,
				OutputRoot:     root,
				AnchorDate:     anchor,
			})
			require.NoError(t, err)

			oldDataset := moveIdentityDatasetAside(t, root, test.dataset)
			writeIdentityParquet(t, db, root, test.dataset, `
				SELECT `+test.projection+`
				FROM read_parquet('`+
				quoteSQLString(identityParquetGlob(oldDataset, test.dataset))+`')`)

			err = Validate(context.Background(), db, validationOptionsForRoot(root, anchor))
			require.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestAuthoredAliasRollupReceivesOnceAndPreservesDailySignals(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	root, db := writeIdentityBaseFixture(t, false)
	for _, dataset := range []string{"messages", "message_recipients"} {
		requirementsForTest.NoError(os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Subject'::VARCHAR, 'Preview'::VARCHAR,
			 TIMESTAMP '2026-07-30 17:45:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 2::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(100::BIGINT, 3::BIGINT, 'to'::VARCHAR, 'Bob Alias'::VARCHAR),
			(100::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Alice'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	requirementsForTest.NoError(err)

	var relationshipCount, receivedUnits int64
	requirementsForTest.NoError(db.QueryRow(`
		SELECT count(*), coalesce(sum(f.received_units), 0)::BIGINT
		FROM read_parquet(?) r
		LEFT JOIN read_parquet(?) f USING (canonical_id)
	`, identityParquetGlob(root, DatasetRelationships),
		identityParquetGlob(root, DatasetRelationshipDaily)).
		Scan(&relationshipCount, &receivedUnits))
	assertionsForTest.Equal(int64(1), relationshipCount)
	assertionsForTest.Equal(int64(1), receivedUnits)

	var activityCount, fileCount int64
	requirementsForTest.NoError(db.QueryRow(`
		SELECT activity_count, file_count
		FROM read_parquet(?) WHERE canonical_id = 2
	`, identityParquetGlob(root, DatasetRollups)).Scan(&activityCount, &fileCount))
	assertionsForTest.Equal(int64(1), activityCount)
	assertionsForTest.Equal(int64(1), fileCount)

	var domainActivity, domainPeople int64
	requirementsForTest.NoError(db.QueryRow(`
		SELECT activity_count, person_count
		FROM read_parquet(?) WHERE domain = 'community.test'
	`, identityParquetGlob(root, DatasetDomainRollups)).
		Scan(&domainActivity, &domainPeople))
	assertionsForTest.Equal(int64(1), domainActivity,
		"conversation membership contributes non-chat domain activity")
	assertionsForTest.Equal(int64(0), domainPeople,
		"conversation-only non-chat members do not contribute people fan-out")
}

func TestLogicalChatReductionUsesNewestFilteredMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeIdentityBaseFixture(t, false)
	for _, dataset := range []string{"messages", "message_recipients", "conversations"} {
		requirements.NoError(os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 ''::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-20 10:30:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 2::BIGINT, 'sms'::VARCHAR, false, 2026::INTEGER, 7::INTEGER),
			(101::BIGINT, 1::BIGINT, 'm-101'::VARCHAR, 10::BIGINT,
			 ''::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-20 10:30:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, 'sms'::VARCHAR, true, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(100::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Alice'::VARCHAR),
			(101::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(101::BIGINT, 2::BIGINT, 'to'::VARCHAR, 'Bob'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)
	writeIdentityParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'thread-10'::VARCHAR, 'Thread'::VARCHAR, 'direct_chat'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	requirements.NoError(err)

	paths := ActivityPaths{
		Facts:             identityParquetGlob(root, DatasetEntryFacts),
		DirectEdges:       identityParquetGlob(root, DatasetDirectEdges),
		ConversationEdges: identityParquetGlob(root, DatasetConversationEdges),
		Directory:         identityParquetGlob(root, DatasetDirectory),
		Clusters:          identityParquetGlob(root, "participant_clusters"),
		Owners:            identityParquetGlob(root, "owner_participants"),
	}
	assertLogicalUnit := func(t *testing.T, filter string, wantID int64, wantFromMe bool) {
		t.Helper()
		var gotID int64
		var gotFromMe bool
		query := LogicalActivitySQL(paths, filter) +
			" SELECT anchor_message_id, is_from_me FROM logical_units"
		require.NoError(t, db.QueryRow(query).Scan(&gotID, &gotFromMe))
		assert.Equal(t, wantID, gotID)
		assert.Equal(t, wantFromMe, gotFromMe)
	}
	assertLogicalUnit(t, "true", 101, true)
	assertLogicalUnit(t, "f.message_id = 100", 100, false)

	var chatMemberActivity, chatMemberPeople int64
	requirements.NoError(db.QueryRow(`
		SELECT r.activity_count, d.person_count
		FROM read_parquet(?) r
		JOIN read_parquet(?) d ON d.domain = 'community.test'
		WHERE r.canonical_id = 4
	`, identityParquetGlob(root, DatasetRollups),
		identityParquetGlob(root, DatasetDomainRollups)).
		Scan(&chatMemberActivity, &chatMemberPeople))
	assertions.Equal(int64(1), chatMemberActivity,
		"conversation membership contributes chat people fan-out")
	assertions.Equal(int64(1), chatMemberPeople)
}

func TestDailyRelationshipRollupKeepsRawGateMaskAndTimestamp(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	root, db := writeIdentityBaseFixture(t, false)
	for _, dataset := range []string{"messages", "message_recipients"} {
		requirementsForTest.NoError(os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Sent'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-28 09:00:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, 'email'::VARCHAR, true, 2026::INTEGER, 7::INTEGER),
			(101::BIGINT, 1::BIGINT, 'm-101'::VARCHAR, 10::BIGINT,
			 'Meeting'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-29 15:45:12',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, 'calendar_event'::VARCHAR, true, 2026::INTEGER, 7::INTEGER),
			(102::BIGINT, 1::BIGINT, 'm-102'::VARCHAR, 10::BIGINT,
			 'Owner absent'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-31 22:11:09',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 2::BIGINT, 'meeting_note'::VARCHAR, false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'to'::VARCHAR, 'Bob'::VARCHAR),
			(101::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(101::BIGINT, 2::BIGINT, 'to'::VARCHAR, 'Bob'::VARCHAR),
			(102::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(102::BIGINT, 4::BIGINT, 'to'::VARCHAR, 'Member'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	requirementsForTest.NoError(err)

	var canonicalID, sentCount, meetingCount int64
	var modalityMask uint8
	var lastAt time.Time
	requirementsForTest.NoError(db.QueryRow(`
		SELECT canonical_id, sent_count, meeting_count, modality_mask, last_at
		FROM read_parquet(?)
	`, identityParquetGlob(root, DatasetRelationships)).
		Scan(&canonicalID, &sentCount, &meetingCount, &modalityMask, &lastAt))
	assertionsForTest.Equal(int64(2), canonicalID)
	assertionsForTest.Equal(int64(1), sentCount)
	assertionsForTest.Equal(int64(1), meetingCount)
	assertionsForTest.Equal(ModalityEmail|ModalityMeeting, modalityMask)
	assertionsForTest.Equal(time.Date(2026, 7, 29, 15, 45, 12, 0, time.UTC), lastAt)

	var futureRows, futureSent, futureMeetings int64
	var futureMask uint8
	var futureLastAt time.Time
	requirementsForTest.NoError(db.QueryRow(`
		SELECT count(*), sum(sent_units)::BIGINT, sum(meeting_units)::BIGINT,
		       bit_or(modality_mask)::UTINYINT, max(last_at)
		FROM read_parquet(?)
	`, identityParquetGlob(root, DatasetRelationshipDaily)).
		Scan(&futureRows, &futureSent, &futureMeetings, &futureMask, &futureLastAt))
	assertionsForTest.Equal(int64(2), futureRows)
	assertionsForTest.Equal(int64(1), futureSent)
	assertionsForTest.Equal(int64(1), futureMeetings)
	assertionsForTest.Equal(ModalityEmail|ModalityMeeting, futureMask)
	assertionsForTest.Equal(lastAt, futureLastAt)
}

func TestValidateRejectsDailyIdentityWithoutRelationshipRollup(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	require.NoError(t, err)

	oldRelationships := filepath.Join(root, DatasetRelationships+"-old")
	require.NoError(t, os.Rename(
		filepath.Join(root, DatasetRelationships),
		oldRelationships,
	))
	writeIdentityParquet(t, db, root, DatasetRelationships, `
		SELECT * FROM read_parquet('`+
		strings.ReplaceAll(filepath.Join(oldRelationships, "*.parquet"), "'", "''")+`')
		WHERE false`)

	err = Validate(context.Background(), db, ValidationOptions{
		OutputRoot:             root,
		RequiredOutputDatasets: RequiredDatasets,
		Activity: ActivityPaths{
			Facts:             identityParquetGlob(root, DatasetEntryFacts),
			DirectEdges:       identityParquetGlob(root, DatasetDirectEdges),
			ConversationEdges: identityParquetGlob(root, DatasetConversationEdges),
			Directory:         identityParquetGlob(root, DatasetDirectory),
			Clusters:          identityParquetGlob(root, "participant_clusters"),
			Owners:            identityParquetGlob(root, "owner_participants"),
		},
		Participants:  identityParquetGlob(root, "participants"),
		Conversations: identityParquetGlob(root, "conversations"),
		AnchorDate:    anchor,
	})
	require.ErrorContains(t, err,
		"validate relationship_daily: 1 daily identities have no relationship rollup")
}

func TestBuildIncrementalEmitsDeltaAndComputesPostPublicationStats(t *testing.T) {
	assertionsForTest := assert.New(t)
	committedRoot, db := writeIdentityBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: committedRoot,
		OutputRoot:     committedRoot,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	stagedRoot, stagedDB := writeIdentityBaseFixture(t, false)
	rewriteIdentityFixtureIDs(t, stagedDB, stagedRoot, 200, 20)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeIncremental,
		CommittedRoot:  committedRoot,
		StagedBaseRoot: stagedRoot,
		OutputRoot:     stagedRoot,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assertionsForTest.Equal(int64(1), parquetCount(t, db, stagedRoot, DatasetEntryFacts))
	assertionsForTest.Equal(int64(2), parquetCount(t, db, stagedRoot, DatasetDirectEdges))
	var messageID int64
	require.NoError(t, db.QueryRow(identityParquetSQL(stagedRoot, DatasetEntryFacts)).
		Scan(&messageID, new(int64), new(int64), new(string), new(time.Time),
			new(string), new(string), new(string), new(bool), new(bool), new(bool),
			new(int32), new(bool), new(int16)))
	assertionsForTest.Equal(int64(200), messageID)
	assertionsForTest.Equal(int64(2), result.Stats.TotalMessages)
	assertionsForTest.Equal(int64(1), result.Stats.Sources)
	assertionsForTest.Equal(int64(100), result.Stats.TotalSizeBytes)
	assertionsForTest.Equal(int64(24), result.Stats.AttachmentSizeBytes)
	assertionsForTest.Equal(int64(1), parquetCount(t, db, committedRoot, DatasetEntryFacts))
}

func TestBuildDerivedOnlyUsesCommittedBaseAndStagedIdentityDimensions(t *testing.T) {
	assertionsForTest := assert.New(t)
	committedRoot, db := writeIdentityBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: committedRoot,
		OutputRoot:     committedRoot,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	stagedRoot := t.TempDir()
	writeIdentityParquet(t, db, stagedRoot, "participant_clusters", `
		SELECT * FROM (VALUES
			(2::BIGINT, 2::BIGINT),
			(3::BIGINT, 2::BIGINT),
			(4::BIGINT, 2::BIGINT)
		) AS t(participant_id, canonical_id)`)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeDerivedOnly,
		CommittedRoot:  committedRoot,
		StagedBaseRoot: stagedRoot,
		OutputRoot:     stagedRoot,
		AnchorDate:     time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	var memberIDs string
	require.NoError(t, db.QueryRow(`
		SELECT CAST(to_json(member_ids) AS VARCHAR)
		FROM read_parquet(?)
		WHERE canonical_id = 2
	`, identityParquetGlob(stagedRoot, DatasetDirectory)).Scan(&memberIDs))
	assertionsForTest.JSONEq(`[2,3,4]`, memberIDs)
	assertionsForTest.Equal(CacheStatsSummary{}, result.Stats,
		"derived refresh must preserve committed marker stats without rescanning raw Parquet")
	assertionsForTest.NoDirExists(filepath.Join(stagedRoot, DatasetEntryFacts))
	assertionsForTest.NoDirExists(filepath.Join(stagedRoot, DatasetDirectEdges))
	assertionsForTest.NoDirExists(filepath.Join(stagedRoot, DatasetConversationEdges))
}

func TestBuildStatsPreserveMessageSourceAndSenderValueSemantics(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeIdentityBaseFixture(t, false)
	for _, dataset := range []string{"sources", "participants", "message_recipients"} {
		requirements.NoError(os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@example.com'::VARCHAR, 'gmail'::VARCHAR),
			(2::BIGINT, 'unused@example.net'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`)
	writeIdentityParquet(t, db, root, "participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'alice@example.com'::VARCHAR, 'example.com'::VARCHAR, 'Alice'::VARCHAR, ''::VARCHAR),
			(2::BIGINT, 'bob@example.net'::VARCHAR, 'example.net'::VARCHAR, ''::VARCHAR, ''::VARCHAR),
			(3::BIGINT, 'alias@example.net'::VARCHAR, 'example.net'::VARCHAR, 'Bob Alias'::VARCHAR, ''::VARCHAR),
			(4::BIGINT, 'member@community.test'::VARCHAR, 'community.test'::VARCHAR, 'Member'::VARCHAR, ''::VARCHAR),
			(5::BIGINT, 'alice@example.com'::VARCHAR, 'example.com'::VARCHAR, 'Alice Alias'::VARCHAR, ''::VARCHAR)
		) AS t(id, email_address, domain, display_name, phone_number)`)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(100::BIGINT, 5::BIGINT, 'from'::VARCHAR, 'Alice Alias'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'to'::VARCHAR, ''::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	requirements.NoError(err)
	assertions.Equal(int64(1), result.Stats.Sources)
	assertions.Equal(int64(1), result.Stats.UniqueSenders)
	assertions.Equal(int64(1), result.Stats.UniqueDomains)
}

func writeIdentityBaseFixture(t *testing.T, empty bool) (string, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	db, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(root, "duckdb-tmp")),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	where := ""
	if empty {
		where = " WHERE false"
	}
	writeIdentityParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Subject'::VARCHAR, 'Preview'::VARCHAR,
			 TIMESTAMP '2026-07-20 10:30:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`+where)
	writeIdentityParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@example.com'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`+where)
	writeIdentityParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'thread-10'::VARCHAR, 'Thread'::VARCHAR, 'email_thread'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`+where)
	writeIdentityParquet(t, db, root, "participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'alice@example.com'::VARCHAR, 'example.com'::VARCHAR, 'Alice'::VARCHAR, ''::VARCHAR),
			(2::BIGINT, 'bob@example.net'::VARCHAR, 'example.net'::VARCHAR, ''::VARCHAR, ''::VARCHAR),
			(3::BIGINT, 'alias@example.net'::VARCHAR, 'example.net'::VARCHAR, 'Bob Alias'::VARCHAR, ''::VARCHAR),
			(4::BIGINT, 'member@community.test'::VARCHAR, 'community.test'::VARCHAR, 'Member'::VARCHAR, ''::VARCHAR)
		) AS t(id, email_address, domain, display_name, phone_number)`+where)
	writeIdentityParquet(t, db, root, "participant_identifiers", `
		SELECT * FROM (VALUES
			(2::BIGINT, 'email'::VARCHAR, 'bob@example.net'::VARCHAR, 'Bob Home'::VARCHAR, true),
			(3::BIGINT, 'chat'::VARCHAR, 'bob-chat'::VARCHAR, 'Bob Chat'::VARCHAR, true)
		) AS t(participant_id, identifier_type, identifier_value, display_value, is_primary)`+where)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'to'::VARCHAR, ''::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'cc'::VARCHAR, ''::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`+where)
	writeIdentityParquet(t, db, root, "conversation_participants", `
		SELECT * FROM (VALUES
			(10::BIGINT, 4::BIGINT)
		) AS t(conversation_id, participant_id)`+where)
	writeIdentityParquet(t, db, root, "owner_participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT)
		) AS t(source_id, participant_id)`+where)
	writeIdentityParquet(t, db, root, "participant_clusters", `
		SELECT * FROM (VALUES
			(2::BIGINT, 2::BIGINT),
			(3::BIGINT, 2::BIGINT)
		) AS t(participant_id, canonical_id)`+where)
	writeIdentityParquet(t, db, root, "attachments", `
		SELECT * FROM (VALUES
			(1::BIGINT, 100::BIGINT, 12::BIGINT, 'file.txt'::VARCHAR, 'text/plain'::VARCHAR)
		) AS t(attachment_id, message_id, size, filename, mime_type)`+where)
	return root, db
}

func rewriteIdentityFixtureIDs(
	t *testing.T,
	db *sql.DB,
	root string,
	messageID, conversationID int64,
) {
	t.Helper()
	for _, dataset := range []string{
		"messages",
		"conversations",
		"message_recipients",
		"conversation_participants",
		"attachments",
	} {
		require.NoError(t, os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "messages", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 1::BIGINT, 'm-%d'::VARCHAR, %d::BIGINT,
			 'Subject'::VARCHAR, 'Preview'::VARCHAR,
			 TIMESTAMP '2026-07-21 10:30:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`,
		messageID, messageID, conversationID))
	writeIdentityParquet(t, db, root, "conversations", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 'thread-%d'::VARCHAR, 'Thread'::VARCHAR, 'email_thread'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`,
		conversationID, conversationID))
	writeIdentityParquet(t, db, root, "message_recipients", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(%d::BIGINT, 2::BIGINT, 'to'::VARCHAR, ''::VARCHAR),
			(%d::BIGINT, 2::BIGINT, 'cc'::VARCHAR, ''::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`,
		messageID, messageID, messageID))
	writeIdentityParquet(t, db, root, "conversation_participants", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 4::BIGINT)
		) AS t(conversation_id, participant_id)`,
		conversationID))
	writeIdentityParquet(t, db, root, "attachments", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(2::BIGINT, %d::BIGINT, 12::BIGINT, 'file.txt'::VARCHAR, 'text/plain'::VARCHAR)
		) AS t(attachment_id, message_id, size, filename, mime_type)`,
		messageID))
}

func writeIdentityParquet(t *testing.T, db *sql.DB, root, dataset, query string) {
	t.Helper()
	dir := filepath.Join(root, dataset)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, dataset+".parquet")
	_, err := db.Exec(fmt.Sprintf(
		"COPY (%s) TO '%s' (FORMAT PARQUET, COMPRESSION 'zstd')",
		query,
		strings.ReplaceAll(path, "'", "''"),
	))
	require.NoError(t, err, dataset)
}

func parquetCount(t *testing.T, db *sql.DB, root, dataset string) int64 {
	t.Helper()
	var count int64
	query := "SELECT count(*) FROM read_parquet(?)"
	if dataset == DatasetEntryFacts || dataset == DatasetDirectEdges {
		query = "SELECT count(*) FROM read_parquet(?, hive_partitioning=true, union_by_name=true)"
	}
	require.NoError(t, db.QueryRow(query, identityParquetGlob(root, dataset)).
		Scan(&count), dataset)
	return count
}

func identityParquetSQL(root, dataset string) string {
	options := ""
	if dataset == DatasetEntryFacts || dataset == DatasetDirectEdges {
		options = ", hive_partitioning=true, union_by_name=true"
	}
	return "SELECT * FROM read_parquet('" +
		strings.ReplaceAll(identityParquetGlob(root, dataset), "'", "''") + "'" +
		options + ")"
}

func identityParquetGlob(root, dataset string) string {
	if dataset == DatasetEntryFacts || dataset == DatasetDirectEdges {
		return filepath.Join(root, dataset, "**", "*.parquet")
	}
	return filepath.Join(root, dataset, "*.parquet")
}

func moveIdentityDatasetAside(t *testing.T, root, dataset string) string {
	t.Helper()
	oldRoot := t.TempDir()
	require.NoError(t, os.Rename(
		filepath.Join(root, dataset),
		filepath.Join(oldRoot, dataset),
	))
	return oldRoot
}

func validationOptionsForRoot(root string, anchor time.Time) ValidationOptions {
	return ValidationOptions{
		OutputRoot:             root,
		RequiredOutputDatasets: RequiredDatasets,
		Activity: ActivityPaths{
			Facts:             identityParquetGlob(root, DatasetEntryFacts),
			DirectEdges:       identityParquetGlob(root, DatasetDirectEdges),
			ConversationEdges: identityParquetGlob(root, DatasetConversationEdges),
			Directory:         identityParquetGlob(root, DatasetDirectory),
			Clusters:          identityParquetGlob(root, "participant_clusters"),
			Owners:            identityParquetGlob(root, "owner_participants"),
		},
		Participants:  identityParquetGlob(root, "participants"),
		Conversations: identityParquetGlob(root, "conversations"),
		AnchorDate:    anchor,
	}
}
