package backupapp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
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
			databasePath, attachmentsDirectory := seedCompatArchive(t, archive)
			database, err := sql.Open("sqlite3", databasePath)
			require.NoError(err)
			_, err = database.Exec(`
		CREATE TABLE document_chunks (
			id INTEGER PRIMARY KEY,
			text TEXT NOT NULL,
			char_count INTEGER NOT NULL
		);
		CREATE TABLE document_extraction_profiles (
			id TEXT PRIMARY KEY, enabled BOOLEAN NOT NULL, retired_at DATETIME
		);
		INSERT INTO document_extraction_profiles(id, enabled, retired_at)
		VALUES ('active', TRUE, NULL), ('retired', FALSE, CURRENT_TIMESTAMP);
		CREATE TABLE document_provider_consents (profile_id TEXT PRIMARY KEY);
		INSERT INTO document_provider_consents(profile_id) VALUES ('active');
		CREATE TABLE document_extractions (id TEXT PRIMARY KEY);
		INSERT INTO document_extractions(id) VALUES ('one'), ('two'), ('three');
		CREATE TABLE document_extraction_heads (extraction_id TEXT PRIMARY KEY);
		INSERT INTO document_extraction_heads(extraction_id) VALUES ('one'), ('two')`)
			require.NoError(err)
			if stagedRanges {
				_, err = database.Exec(`CREATE TABLE document_extraction_ranges (
			extraction_id TEXT NOT NULL REFERENCES document_extractions(id) ON DELETE CASCADE,
			first_page INTEGER NOT NULL CHECK (first_page >= 1),
			last_page INTEGER NOT NULL CHECK (last_page >= first_page),
			total_pages INTEGER NOT NULL CHECK (total_pages >= last_page),
			evidence TEXT NOT NULL,
			evidence_checksum TEXT NOT NULL,
			markdown TEXT NOT NULL,
			char_count BIGINT NOT NULL CHECK (char_count >= 0),
			payload_bytes BIGINT NOT NULL CHECK (payload_bytes > 0),
			PRIMARY KEY (extraction_id, first_page)
		)`)
				require.NoError(err)
				policy, err := document.NewEvidencePolicy(10000)
				require.NoError(err)
				source := document.SourceEvidenceV1{
					ContractVersion: document.SourceEvidenceContractV1,
					Completeness:    document.EvidenceComplete, Family: "pdf", UnitKind: document.EvidenceUnitPage,
				}
				for page := 1; page <= 50; page++ {
					text := ""
					if page == 1 {
						text = "checkpoint sentinel"
					}
					source.Units = append(source.Units, document.SourceEvidenceUnitV1{
						Order: page - 1, Text: text,
						Locator: document.SourceEvidenceLocatorV1{
							Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: int64(page), End: int64(page),
						},
					})
				}
				evidence, err := document.NormalizeEvidenceV1(source, policy)
				require.NoError(err)
				encoded, checksum, err := document.MarshalNormalizedEvidenceV1(evidence)
				require.NoError(err)
				_, err = database.Exec(`INSERT INTO document_extraction_ranges (
			extraction_id, first_page, last_page, total_pages, evidence, evidence_checksum, markdown, char_count, payload_bytes
		) VALUES ('three', 1, 50, 51, ?, ?, '', 19, ?)`, string(encoded), checksum, len(encoded))
				require.NoError(err)
			} else {
				_, err = database.Exec(`INSERT INTO document_chunks(text, char_count)
			VALUES ('synthetic normalized attachment evidence', 40)`)
				require.NoError(err)
			}
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
