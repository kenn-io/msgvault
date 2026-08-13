package backupapp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/kit/backup"
)

const (
	documentDisclosureName   = "document-derivatives"
	documentDisclosureFormat = "application/vnd.msgvault.document-derivatives+json; contains-normalized-plaintext=true"
	documentDisclosureSchema = 2
)

// DocumentDisclosure is a bounded, content-free declaration that a snapshot's
// database contains locally normalized attachment text. It deliberately
// records counts only; excerpts, filenames, paths, hashes, and provider output
// never leave the database through this artifact.
type DocumentDisclosure struct {
	SchemaVersion               int   `json:"schema_version"`
	ContainsNormalizedPlaintext bool  `json:"contains_normalized_attachment_plaintext"`
	Chunks                      int64 `json:"chunks"`
	Characters                  int64 `json:"characters"`
	Profiles                    int64 `json:"profiles"`
	EnabledProfiles             int64 `json:"enabled_profiles"`
	ConsentedProfiles           int64 `json:"consented_profiles"`
	Extractions                 int64 `json:"extractions"`
	CurrentHeads                int64 `json:"current_heads"`
}

func (v *frozenView) AuxiliaryArtifacts(ctx context.Context) ([]backup.AuxiliaryArtifact, error) {
	postgres := documentDisclosureUsesPostgres(ctx, v.tx)
	tableExists, err := documentDisclosureTableExists(ctx, v.tx, "document_chunks", postgres)
	if err != nil {
		return nil, err
	}
	if !tableExists {
		return nil, nil
	}
	disclosure := DocumentDisclosure{SchemaVersion: documentDisclosureSchema}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(char_count), 0)
		FROM document_chunks`).Scan(&disclosure.Chunks, &disclosure.Characters); err != nil {
		return nil, fmt.Errorf("backupapp: count document derivatives: %w", err)
	}
	if disclosure.Chunks == 0 {
		return nil, nil
	}
	profilesExist, err := documentDisclosureTableExists(ctx, v.tx, "document_extraction_profiles", postgres)
	if err != nil {
		return nil, err
	}
	if profilesExist {
		if err := v.tx.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(CASE WHEN enabled = TRUE AND retired_at IS NULL THEN 1 ELSE 0 END), 0)
			FROM document_extraction_profiles`).Scan(&disclosure.Profiles, &disclosure.EnabledProfiles); err != nil {
			return nil, fmt.Errorf("backupapp: count document extraction profiles: %w", err)
		}
	}
	consentsExist, err := documentDisclosureTableExists(ctx, v.tx, "document_provider_consents", postgres)
	if err != nil {
		return nil, err
	}
	if consentsExist {
		if err := v.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_provider_consents`).Scan(
			&disclosure.ConsentedProfiles,
		); err != nil {
			return nil, fmt.Errorf("backupapp: count document provider consents: %w", err)
		}
	}
	extractionsExist, err := documentDisclosureTableExists(ctx, v.tx, "document_extractions", postgres)
	if err != nil {
		return nil, err
	}
	if extractionsExist {
		if err := v.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_extractions`).Scan(
			&disclosure.Extractions,
		); err != nil {
			return nil, fmt.Errorf("backupapp: count document extractions: %w", err)
		}
	}
	headsExist, err := documentDisclosureTableExists(ctx, v.tx, "document_extraction_heads", postgres)
	if err != nil {
		return nil, err
	}
	if headsExist {
		if err := v.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_extraction_heads`).Scan(
			&disclosure.CurrentHeads,
		); err != nil {
			return nil, fmt.Errorf("backupapp: count current document extraction heads: %w", err)
		}
	}
	disclosure.ContainsNormalizedPlaintext = true
	payload, err := json.Marshal(disclosure)
	if err != nil {
		return nil, fmt.Errorf("backupapp: encode document derivative disclosure: %w", err)
	}
	return []backup.AuxiliaryArtifact{{
		Name: documentDisclosureName, Format: documentDisclosureFormat,
		Open: func(context.Context) (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(payload)), int64(len(payload)), nil
		},
	}}, nil
}

// documentDisclosureUsesPostgres probes PostgreSQL first. The probe succeeds
// without special privileges there; SQLite treats the unknown function as a
// statement-local error and keeps its read transaction usable.
func documentDisclosureUsesPostgres(ctx context.Context, tx *sql.Tx) bool {
	var version string
	return tx.QueryRowContext(ctx, `SELECT current_setting('server_version_num')`).Scan(&version) == nil
}

func documentDisclosureTableExists(
	ctx context.Context,
	tx *sql.Tx,
	table string,
	postgres bool,
) (bool, error) {
	query := `
		SELECT EXISTS (
			SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?
		)`
	if postgres {
		query = `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = current_schema() AND table_name = $1
			)`
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, query, table).Scan(&exists); err != nil {
		return false, fmt.Errorf("backupapp: inspect %s schema: %w", table, err)
	}
	return exists, nil
}

// ManifestContainsDocumentPlaintext reports the explicit manifest-level
// disclosure without reading the opaque artifact payload.
func ManifestContainsDocumentPlaintext(manifest *backup.Manifest) bool {
	if manifest == nil {
		return false
	}
	for _, artifact := range manifest.Auxiliary {
		if artifact.Name == documentDisclosureName && artifact.Format == documentDisclosureFormat {
			return true
		}
	}
	return false
}

// DocumentAuxiliaryTarget validates the disclosure artifact restored by Kit.
// The authoritative derivatives already live in the restored database, so a
// successful transaction has no additional files to publish.
type DocumentAuxiliaryTarget struct{}

func NewDocumentAuxiliaryTarget() *DocumentAuxiliaryTarget { return &DocumentAuxiliaryTarget{} }

func (t *DocumentAuxiliaryTarget) StageAuxiliary(
	_ context.Context,
	artifacts []backup.RestoredAuxiliary,
) (backup.AuxiliaryRestore, error) {
	if t == nil {
		return nil, errors.New("backupapp: document auxiliary target is nil")
	}
	if len(artifacts) == 0 {
		return documentAuxiliaryRestore{}, nil
	}
	if len(artifacts) != 1 || artifacts[0].Name != documentDisclosureName ||
		artifacts[0].Format != documentDisclosureFormat {
		return nil, errors.New("backupapp: restored auxiliary artifact set is unsupported")
	}
	var disclosure DocumentDisclosure
	decoder := json.NewDecoder(bytes.NewReader(artifacts[0].Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&disclosure); err != nil {
		return nil, fmt.Errorf("backupapp: decode document derivative disclosure: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("backupapp: document derivative disclosure has trailing data")
	}
	if (disclosure.SchemaVersion != 1 && disclosure.SchemaVersion != documentDisclosureSchema) ||
		!disclosure.ContainsNormalizedPlaintext || disclosure.Chunks <= 0 || disclosure.Characters < 0 {
		return nil, errors.New("backupapp: document derivative disclosure is invalid")
	}
	if disclosure.SchemaVersion == documentDisclosureSchema &&
		(disclosure.Profiles < 0 || disclosure.EnabledProfiles < 0 ||
			disclosure.ConsentedProfiles < 0 || disclosure.Extractions < 0 ||
			disclosure.CurrentHeads < 0 || disclosure.EnabledProfiles > disclosure.Profiles ||
			disclosure.ConsentedProfiles > disclosure.Profiles || disclosure.CurrentHeads > disclosure.Extractions) {
		return nil, errors.New("backupapp: document derivative disclosure counts are invalid")
	}
	return documentAuxiliaryRestore{}, nil
}

type documentAuxiliaryRestore struct{}

func (documentAuxiliaryRestore) Commit(context.Context) error   { return nil }
func (documentAuxiliaryRestore) Rollback(context.Context) error { return nil }

var (
	_ backup.AuxiliarySource = (*frozenView)(nil)
	_ backup.AuxiliaryTarget = (*DocumentAuxiliaryTarget)(nil)
)
