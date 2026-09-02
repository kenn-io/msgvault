package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

type personInferenceProfileV2Backfill struct {
	fingerprint           string
	auth                  peoplesweep.AuthScheme
	credential            peoplesweep.CredentialSource
	credentialRef         string
	outputMode            peoplesweep.OutputMode
	tokenLimitParameter   string
	reasoningEffort       string
	reasoningMode         string
	driverVersion         string
	executionBoundary     string
	packetRendererPolicy  string
	programFingerprint    string
	disclosedPacketFields []string
}

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

		backfills, err := readPersonInferenceProviderV2Backfills(ctx, tx)
		if err != nil {
			return err
		}
		for _, backfill := range backfills {
			disclosed, err := json.Marshal(backfill.disclosedPacketFields)
			if err != nil {
				return fmt.Errorf("encode people inference disclosed fields: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE person_inference_profiles
				SET auth_scheme = ?, credential_source = ?, credential_ref = ?,
				    output_mode = ?, token_limit_parameter = ?, reasoning_effort = ?,
				    reasoning_mode = ?, driver_version = ?, execution_boundary = ?,
				    packet_renderer_policy = ?, program_fingerprint = ?,
				    disclosed_packet_fields = `+s.dialect.JSONBindExpr()+`
				WHERE fingerprint = ?`,
				backfill.auth, backfill.credential, backfill.credentialRef,
				backfill.outputMode, backfill.tokenLimitParameter, backfill.reasoningEffort,
				backfill.reasoningMode, backfill.driverVersion, backfill.executionBoundary,
				backfill.packetRendererPolicy, backfill.programFingerprint, string(disclosed),
				backfill.fingerprint,
			); err != nil {
				return fmt.Errorf("backfill people inference profile %s: %w", backfill.fingerprint, err)
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
		return nil
	})
}

func postgresPersonInferenceAddColumn(statement string) string {
	return strings.Replace(statement, " ADD COLUMN ", " ADD COLUMN IF NOT EXISTS ", 1)
}

func readPersonInferenceProviderV2Backfills(
	ctx context.Context,
	tx *loggedTx,
) ([]personInferenceProfileV2Backfill, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT fingerprint, provider_kind, api_key_env, allow_anonymous,
		       CAST(policy_json AS TEXT)
		FROM person_inference_profiles`)
	if err != nil {
		return nil, fmt.Errorf("list people inference profiles for v2 migration: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var backfills []personInferenceProfileV2Backfill
	for rows.Next() {
		var fingerprint, kind, apiKeyEnv, policyJSON string
		var allowAnonymous bool
		if err := rows.Scan(&fingerprint, &kind, &apiKeyEnv, &allowAnonymous, &policyJSON); err != nil {
			return nil, fmt.Errorf("scan people inference profile for v2 migration: %w", err)
		}
		backfills = append(backfills, personInferenceProviderV2Backfill(
			fingerprint, kind, apiKeyEnv, allowAnonymous, policyJSON,
		))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate people inference profiles for v2 migration: %w", err)
	}
	return backfills, nil
}

func personInferenceProviderV2Backfill(
	fingerprint, kind, apiKeyEnv string,
	allowAnonymous bool,
	policyJSON string,
) personInferenceProfileV2Backfill {
	backfill := personInferenceProfileV2Backfill{
		fingerprint: fingerprint, auth: peoplesweep.AuthBearer,
		credential: peoplesweep.CredentialEnv, credentialRef: apiKeyEnv,
		outputMode: peoplesweep.OutputModeNativeJSONSchema, disclosedPacketFields: []string{},
	}
	if allowAnonymous {
		backfill.auth = peoplesweep.AuthNone
		backfill.credential = peoplesweep.CredentialNone
		backfill.credentialRef = ""
	}

	var profile peoplesweep.ProviderProfile
	if json.Unmarshal([]byte(policyJSON), &profile) == nil && profile.Protocol != "" {
		backfill.auth = profile.Auth
		backfill.credential = profile.Credential
		backfill.credentialRef = profile.CredentialRef
		backfill.outputMode = profile.OutputMode
		backfill.tokenLimitParameter = profile.TokenLimitParameter
		backfill.reasoningEffort = profile.ReasoningEffort
		backfill.reasoningMode = profile.ReasoningMode
		backfill.driverVersion = profile.DriverVersion
		backfill.executionBoundary = profile.ExecutionBoundary
		backfill.packetRendererPolicy = profile.PacketRendererPolicy
		backfill.programFingerprint = profile.ProgramFingerprint
		backfill.disclosedPacketFields = profile.DisclosedPacketFields
		return backfill
	}

	var legacy legacyPersonInferencePolicy
	if json.Unmarshal([]byte(policyJSON), &legacy) == nil {
		backfill.reasoningEffort = legacy.ReasoningEffort
		backfill.executionBoundary = legacy.ExecutionBoundary
		backfill.packetRendererPolicy = legacy.PacketRendererPolicy
		backfill.programFingerprint = legacy.ProgramFingerprint
		backfill.disclosedPacketFields = legacy.DisclosedPacketFields
	}
	switch kind {
	case peoplesweep.ProviderOpenAICompatible:
		backfill.tokenLimitParameter = "max_completion_tokens"
		backfill.driverVersion = peoplesweep.OpenAICompatibleProviderVersion
	case peoplesweep.ProviderCodexAppServer:
		backfill.driverVersion = peoplesweep.CodexAppServerProviderVersion
	}
	return backfill
}
