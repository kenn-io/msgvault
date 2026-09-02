package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// migratePersonInferenceProviderV2 brings person_inference_profiles up to the
// named-profile schema and records synthetic checks in a separate table.
//
// Profile rows written before named profiles existed carry a policy without a
// protocol. That policy shape never shipped in a release, its fingerprint can
// no longer match any configured profile, and consent to it does not carry
// over to the reworked disclosure. Those rows and their consents are removed
// so every remaining profile decodes as a canonical ProviderProfile.
func (s *Store) migratePersonInferenceProviderV2(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		columns := []ColumnMigration{
			{`ALTER TABLE person_inference_profiles ADD COLUMN auth_scheme TEXT NOT NULL DEFAULT 'bearer'`, "person_inference_profiles.auth_scheme"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN credential_source TEXT NOT NULL DEFAULT 'env'`, "person_inference_profiles.credential_source"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN credential_ref TEXT NOT NULL DEFAULT ''`, "person_inference_profiles.credential_ref"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN output_mode TEXT NOT NULL DEFAULT 'native_json_schema'`, "person_inference_profiles.output_mode"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN token_limit_parameter TEXT NOT NULL DEFAULT ''`, "person_inference_profiles.token_limit_parameter"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`, "person_inference_profiles.reasoning_effort"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN reasoning_mode TEXT NOT NULL DEFAULT ''`, "person_inference_profiles.reasoning_mode"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN driver_version TEXT NOT NULL DEFAULT ''`, "person_inference_profiles.driver_version"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN execution_boundary TEXT NOT NULL DEFAULT ''`, "person_inference_profiles.execution_boundary"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN packet_renderer_policy TEXT NOT NULL DEFAULT ''`, "person_inference_profiles.packet_renderer_policy"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN program_fingerprint TEXT NOT NULL DEFAULT ''`, "person_inference_profiles.program_fingerprint"},
			{`ALTER TABLE person_inference_profiles ADD COLUMN disclosed_packet_fields JSON NOT NULL DEFAULT '[]'`, "person_inference_profiles.disclosed_packet_fields"},
		}
		if s.IsPostgreSQL() {
			for index := range columns {
				columns[index].SQL = postgresPersonInferenceAddColumn(columns[index].SQL)
			}
			columns[len(columns)-1].SQL = `ALTER TABLE person_inference_profiles ADD COLUMN IF NOT EXISTS disclosed_packet_fields JSONB NOT NULL DEFAULT '[]'::jsonb`
		}
		for _, column := range columns {
			if _, err := tx.ExecContext(ctx, column.SQL); err != nil && !s.dialect.IsDuplicateColumnError(err) {
				return fmt.Errorf("add %s: %w", column.Desc, err)
			}
		}

		checkedAtType := "TEXT"
		if s.IsPostgreSQL() {
			checkedAtType = "TIMESTAMPTZ"
		}
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS person_inference_checks (
				profile_fingerprint TEXT PRIMARY KEY REFERENCES person_inference_profiles(fingerprint),
				checked_at `+checkedAtType+` NOT NULL,
				driver_version TEXT NOT NULL,
				output_mode TEXT NOT NULL,
				provider_request_id TEXT NOT NULL DEFAULT '',
				model_version TEXT NOT NULL
			)`); err != nil {
			return fmt.Errorf("create person_inference_checks: %w", err)
		}

		stale, err := preProfilePersonInferenceFingerprints(ctx, tx)
		if err != nil {
			return err
		}
		for _, fingerprint := range stale {
			for _, statement := range []string{
				`DELETE FROM person_inference_consents WHERE profile_fingerprint = ?`,
				`DELETE FROM person_inference_checks WHERE profile_fingerprint = ?`,
				`DELETE FROM person_inference_profiles WHERE fingerprint = ?`,
			} {
				if _, err := tx.ExecContext(ctx, statement, fingerprint); err != nil {
					return fmt.Errorf("remove pre-profile people inference row %s: %w", fingerprint, err)
				}
			}
		}
		return nil
	})
}

func postgresPersonInferenceAddColumn(statement string) string {
	return strings.Replace(statement, " ADD COLUMN ", " ADD COLUMN IF NOT EXISTS ", 1)
}

// preProfilePersonInferenceFingerprints lists profile rows whose stored
// policy predates named protocol profiles.
func preProfilePersonInferenceFingerprints(ctx context.Context, tx *loggedTx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT fingerprint, CAST(policy_json AS TEXT)
		FROM person_inference_profiles`)
	if err != nil {
		return nil, fmt.Errorf("list people inference profiles for v2 migration: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var stale []string
	for rows.Next() {
		var fingerprint, policyJSON string
		if err := rows.Scan(&fingerprint, &policyJSON); err != nil {
			return nil, fmt.Errorf("scan people inference profile for v2 migration: %w", err)
		}
		var policy struct {
			Protocol string `json:"protocol"`
		}
		if json.Unmarshal([]byte(policyJSON), &policy) != nil || policy.Protocol == "" {
			stale = append(stale, fingerprint)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate people inference profiles for v2 migration: %w", err)
	}
	return stale, nil
}
