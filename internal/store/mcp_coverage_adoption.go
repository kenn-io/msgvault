package store

import (
	"context"
	"database/sql"
)

const mcpScopeCoverageAdoptionKey = "mcp_events_scope_coverage_adoption_v1"

// adoptMCPScopeCoverage permits exactly one compatible upgrade from the legacy
// conversation-only family matrix. The full new fingerprint remains durable:
// later removal or narrowing of a derived family must create a capture gap.
// The caller owns the identity, sync/source and Events clock fences.
func (s *Store) adoptMCPScopeCoverage(ctx context.Context, tx *sql.Tx, runtime *mcpRuntime, oldFingerprint, fingerprint string) (bool, error) {
	var recorded bool
	// #nosec G701 -- SQL is fixed; Rebind only changes placeholders and all values are bound.
	if err := tx.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM archive_metadata WHERE key=?)`), mcpScopeCoverageAdoptionKey).Scan(&recorded); err != nil {
		return false, err
	}
	if recorded {
		return false, nil
	}
	compatible := false
	if oldFingerprint != "" && oldFingerprint != fingerprint {
		legacy, projected := mcpLegacyScopeCoverageFingerprint(runtime)
		if projected && legacy == oldFingerprint {
			var derivedAuthority bool
			if err := tx.QueryRowContext(ctx, `SELECT
				EXISTS (SELECT 1 FROM mcp_event_subscriptions WHERE name IN ('msgvault.source_message_archived','msgvault.inbox_message_archived'))
				OR EXISTS (SELECT 1 FROM mcp_event_log WHERE family IN ('msgvault.source_message_archived','msgvault.inbox_message_archived'))`).Scan(&derivedAuthority); err != nil {
				return false, err
			}
			compatible = !derivedAuthority
		}
	}
	// Consume the exception on every successful first configuration, including
	// fresh, disabled and incompatible archives. Retention cannot reopen it.
	// #nosec G701 -- SQL is fixed; Rebind only changes placeholders and all values are bound.
	_, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata (key,value) VALUES (?,'1') ON CONFLICT (key) DO NOTHING`), mcpScopeCoverageAdoptionKey)
	return compatible, err
}

func mcpLegacyScopeCoverageFingerprint(runtime *mcpRuntime) (string, bool) {
	legacy := runtime.config
	legacy.Capabilities = nil
	projected := false
	for _, capability := range runtime.config.Capabilities {
		derived := capability.Family == mcpSourceMessageFamily || capability.Family == mcpInboxMessageFamily
		covered := len(capability.Kinds) > 0
		parent := runtime.kinds["msgvault.message_archived\x00"+capability.SourceType]
		for _, kind := range capability.Kinds {
			covered = covered && parent[kind]
		}
		if derived && covered {
			projected = true
			continue
		}
		legacy.Capabilities = append(legacy.Capabilities, capability)
	}
	_, fingerprint := mcpNewRuntime(legacy)
	return fingerprint, projected
}
