package backupapp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/store"
)

func TestBackupManifestDisclosesAndRestoresDocumentPlaintext(t *testing.T) {
	for _, stagedRanges := range []bool{false, true} {
		name := "published chunks"
		if stagedRanges {
			name = "staged ranges without chunks"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			archive := t.TempDir()
			databasePath := filepath.Join(archive, "msgvault.db")
			attachmentsDirectory := filepath.Join(archive, "attachments")
			require.NoError(os.MkdirAll(attachmentsDirectory, 0o700))
			database, err := store.OpenForTest(databasePath)
			require.NoError(err)
			require.NoError(database.InitSchema())
			hash := strings.Repeat("b", 64)
			_, err = database.DB().Exec(`
		INSERT INTO document_extraction_profiles
			(id, fingerprint, provider, endpoint, region, model, retention_posture, training_posture, allowed_media_types, policy_json, enabled, retired_at)
		VALUES ('active', ?, 'docling', 'http://127.0.0.1:5001', 'local', 'synthetic', 'operator-controlled', 'operator-controlled', '["application/pdf"]', '{}', TRUE, NULL),
			('retired', ?, 'docling', 'http://127.0.0.1:5001', 'local', 'synthetic', 'operator-controlled', 'operator-controlled', '["application/pdf"]', '{}', FALSE, CURRENT_TIMESTAMP);
		INSERT INTO document_provider_consents (profile_id, profile_fingerprint, retention_posture, training_posture)
		VALUES ('active', ?, 'operator-controlled', 'operator-controlled');
		INSERT INTO document_extractions (id, profile_id, canonical_blob_hash, state, local_bytes)
		VALUES ('one', 'active', ?, 'ready', 128), ('two', 'retired', ?, 'ready', 128), ('three', 'active', ?, 'terminal', 128);
		INSERT INTO document_extraction_heads (profile_id, canonical_blob_hash, extraction_input_key, extraction_id)
		VALUES ('active', ?, 'original', 'one'), ('retired', ?, 'original', 'two')`, strings.Repeat("a", 64), strings.Repeat("c", 64), strings.Repeat("a", 64), hash, hash, hash, hash, hash)
			require.NoError(err)
			if stagedRanges {
				// Backup treats evidence as opaque bytes; only its stored character count is disclosed.
				_, err = database.DB().Exec(`INSERT INTO document_extraction_ranges
			(extraction_id, first_page, last_page, total_pages, evidence, evidence_checksum, markdown, char_count, payload_bytes)
		VALUES ('three', 1, 50, 51, 'checkpoint sentinel', 'synthetic checksum', '', 19, 19)`)
			} else {
				_, err = database.DB().Exec(`INSERT INTO document_chunks
			(extraction_id, chunk_key, ordinal, text, heading_path, first_unit_index, last_unit_index, checksum, char_count)
		VALUES ('one', 'synthetic', 0, 'synthetic normalized attachment evidence', '[]', 0, 0, 'synthetic checksum', 40)`)
			}
			require.NoError(err)
			require.NoError(database.Close())

			repository, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
			require.NoError(err)
			app := backupapp.New("document-disclosure-test")
			manifest, err := backup.Create(t.Context(), repository, app, backup.CreateOptions{
				DBPath: databasePath, ContentDir: attachmentsDirectory, DataDir: archive,
			})
			require.NoError(err)
			assert.True(backupapp.ManifestContainsDocumentPlaintext(manifest))
			require.Len(manifest.Auxiliary, 1)
			assert.Equal("document-derivatives", manifest.Auxiliary[0].Name)
			assert.Contains(manifest.Auxiliary[0].Format, "contains-normalized-plaintext=true")
			assert.Greater(manifest.MinReaderVersion, 2,
				"pre-auxiliary readers must refuse a snapshot carrying the disclosure")

			restoreDirectory := filepath.Join(t.TempDir(), "restore")
			capture := &capturingDocumentAuxiliaryTarget{}
			_, err = backup.Restore(t.Context(), repository, app, backup.RestoreOptions{
				TargetDir: restoreDirectory, AuxiliaryTarget: capture,
			})
			require.NoError(err)
			require.Len(capture.artifacts, 1)
			var disclosure backupapp.DocumentDisclosure
			require.NoError(json.Unmarshal(capture.artifacts[0].Data, &disclosure))
			assert.Equal(3, disclosure.SchemaVersion)
			assert.Equal(int64(2), disclosure.Profiles)
			assert.Equal(int64(1), disclosure.EnabledProfiles)
			assert.Equal(int64(1), disclosure.ConsentedProfiles)
			assert.Equal(int64(3), disclosure.Extractions)
			assert.Equal(int64(2), disclosure.CurrentHeads)
			if stagedRanges {
				assert.Zero(disclosure.Chunks)
				assert.Equal(int64(1), disclosure.StagedRanges)
				assert.Equal(int64(19), disclosure.StagedCharacters)
			} else {
				assert.Equal(int64(1), disclosure.Chunks)
				assert.Zero(disclosure.StagedRanges)
			}
			staged, err := backupapp.NewDocumentAuxiliaryTarget().StageAuxiliary(t.Context(), capture.artifacts)
			require.NoError(err)
			require.NoError(staged.Commit(t.Context()))
			restored, err := sql.Open("sqlite3", filepath.Join(restoreDirectory, "msgvault.db"))
			require.NoError(err)
			defer func() { require.NoError(restored.Close()) }()
			var text string
			if stagedRanges {
				require.NoError(restored.QueryRow(`SELECT evidence FROM document_extraction_ranges`).Scan(&text))
				assert.Contains(text, "checkpoint sentinel")
			} else {
				require.NoError(restored.QueryRow(`SELECT text FROM document_chunks`).Scan(&text))
				assert.Equal("synthetic normalized attachment evidence", text)
			}
		})
	}
}

func TestDocumentAuxiliaryTargetRejectsUndisclosedArtifacts(t *testing.T) {
	target := backupapp.NewDocumentAuxiliaryTarget()
	_, err := target.StageAuxiliary(context.Background(), []backup.RestoredAuxiliary{{
		Name: "unexpected", Format: "application/json", Data: []byte(`{}`),
	}})
	require.ErrorContains(t, err, "unsupported")
}

func TestDocumentAuxiliaryTargetRestoresSnapshotWithoutDisclosure(t *testing.T) {
	require := require.New(t)
	archive := t.TempDir()
	databasePath, attachmentsDirectory := seedCompatArchive(t, archive)
	repository, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	app := backupapp.New("document-disclosure-compat-test")
	manifest, err := backup.Create(t.Context(), repository, app, backup.CreateOptions{
		DBPath: databasePath, ContentDir: attachmentsDirectory, DataDir: archive,
	})
	require.NoError(err)
	require.Empty(manifest.Auxiliary)

	restoreDirectory := filepath.Join(t.TempDir(), "restore")
	_, err = backup.Restore(t.Context(), repository, app, backup.RestoreOptions{
		TargetDir: restoreDirectory, AuxiliaryTarget: backupapp.NewDocumentAuxiliaryTarget(),
	})
	require.NoError(err)
	require.FileExists(filepath.Join(restoreDirectory, "msgvault.db"))
}

func TestDocumentAuxiliaryTargetAcceptsLegacyDisclosureSchema(t *testing.T) {
	for _, payload := range []string{
		`{"schema_version":1,"contains_normalized_attachment_plaintext":true,"chunks":1,"characters":4}`,
		`{"schema_version":2,"contains_normalized_attachment_plaintext":true,"chunks":1,"characters":4}`,
	} {
		target := backupapp.NewDocumentAuxiliaryTarget()
		staged, err := target.StageAuxiliary(t.Context(), []backup.RestoredAuxiliary{{Name: "document-derivatives", Format: "application/vnd.msgvault.document-derivatives+json; contains-normalized-plaintext=true", Data: []byte(payload)}})
		require.NoError(t, err)
		require.NoError(t, staged.Commit(t.Context()))
	}
}

type capturingDocumentAuxiliaryTarget struct {
	artifacts []backup.RestoredAuxiliary
}

func (t *capturingDocumentAuxiliaryTarget) StageAuxiliary(
	_ context.Context,
	artifacts []backup.RestoredAuxiliary,
) (backup.AuxiliaryRestore, error) {
	t.artifacts = append(t.artifacts, artifacts...)
	return noOpDocumentAuxiliaryRestore{}, nil
}

type noOpDocumentAuxiliaryRestore struct{}

func (noOpDocumentAuxiliaryRestore) Commit(context.Context) error   { return nil }
func (noOpDocumentAuxiliaryRestore) Rollback(context.Context) error { return nil }
