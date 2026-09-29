package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

// noDefaultIdentityHelp is the flag help text for --no-default-identity.
// Each ingest command registers its own bool variable and reuses this constant.
const noDefaultIdentityHelp = "Suppress automatic default identity confirmation. " +
	"Note: a one-time legacy [identity] config migration may still write confirmed " +
	"identifiers to the account on first post-upgrade startup."

const savedDefaultIdentityHelp = noDefaultIdentityHelp +
	" Saved for later syncs and re-authorization; omit the flag to keep the choice, " +
	"or use --no-default-identity=false to re-enable defaults."

// setDefaultIdentityOptOut changes the saved choice only for an explicit flag.
// source must contain the config from before registration: IMAP add commands
// replace provider settings, so an omitted flag must carry the old choice forward.
func setDefaultIdentityOptOut(cmd *cobra.Command, s *store.Store, source *store.Source, optOut bool) error {
	src, err := s.GetSourceByID(source.ID)
	if err != nil {
		return fmt.Errorf("read identity preference: %w", err)
	}
	if !cmd.Flags().Changed("no-default-identity") {
		if src.SyncConfig == source.SyncConfig {
			return nil
		}
		var previous struct {
			NoDefaultIdentity bool `json:"no_default_identity"`
		}
		if source.SyncConfig.Valid {
			if err := json.Unmarshal([]byte(source.SyncConfig.String), &previous); err != nil {
				return fmt.Errorf("parse saved identity preference: %w", err)
			}
		}
		optOut = previous.NoDefaultIdentity
	}
	cfg := make(map[string]jsontext.Value)
	if src.SyncConfig.Valid {
		if err := json.Unmarshal([]byte(src.SyncConfig.String), &cfg); err != nil {
			return fmt.Errorf("parse identity preference: %w", err)
		}
	}
	if optOut {
		if cfg == nil {
			cfg = make(map[string]jsontext.Value)
		}
		cfg["no_default_identity"] = jsontext.Value("true")
	} else {
		if _, exists := cfg["no_default_identity"]; !exists {
			return nil
		}
		delete(cfg, "no_default_identity")
	}
	encoded, err := json.Marshal(cfg, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode identity preference: %w", err)
	}
	if err := s.UpdateSourceSyncConfig(source.ID, string(encoded)); err != nil {
		return fmt.Errorf("save identity preference: %w", err)
	}
	return nil
}

// confirmDefaultIdentity writes one confirmed identifier to a freshly
// created source's identity. Best-effort: any error is logged and swallowed
// so a partially failed identity write never breaks ingest. Empty identifiers
// are a silent no-op.
//
// Skips the write when the source opted out or already has an identity row.
// add-account / add-imap / add-o365 / import-* commands all call this on
// every invocation (including reruns and rebinds), so without this guard
// an identity the user explicitly removed via `identity remove` would be
// re-added on the next ingest re-run, silently affecting dedup sent-copy
// detection. The guard preserves the documented "freshly created source"
// intent while degrading gracefully if the user has removed every
// identity (in which case the default is restored, which is desirable).
//
// **Ordering note:** ingest commands MUST call confirmDefaultIdentity
// BEFORE runPostSourceCreateMigrations on the same invocation. The
// legacy [identity] migration uses set-semantics merge, so calling the
// default-identity write first and the migration second produces the
// correct merged state. Calling them in the other order populates
// account_identities with the legacy addresses first, then the
// `len(existing) > 0` guard suppresses the source's own account
// identifier entirely (regression caught in iter15). See the per-ingest
// command order in addaccount.go etc.
//
// account is the user-facing account name shown in the confirmation message.
// Callers should gate this behind the per-command --no-default-identity flag.
func confirmDefaultIdentity(out io.Writer, s *store.Store, sourceID int64, account, identifier, signal string, logger *slog.Logger) {
	id := strings.TrimSpace(identifier)
	if id == "" {
		return
	}
	src, err := s.GetSourceByID(sourceID)
	var cfg struct {
		NoDefaultIdentity bool `json:"no_default_identity"`
	}
	if err == nil && src.SyncConfig.Valid {
		err = json.Unmarshal([]byte(src.SyncConfig.String), &cfg)
	}
	if err != nil {
		logger.Warn("auto-default-identity preference check failed",
			"source_id", sourceID, "account", account, "error", err.Error())
		return
	}
	if cfg.NoDefaultIdentity {
		return
	}
	existing, err := s.ListAccountIdentities(sourceID)
	if err != nil {
		logger.Warn("auto-default-identity precheck failed",
			"source_id", sourceID,
			"account", account,
			"error", err.Error())
		return
	}
	if len(existing) > 0 {
		return
	}
	if err := s.AddAccountIdentity(sourceID, id, signal); err != nil {
		logger.Warn("auto-default-identity write failed",
			"source_id", sourceID,
			"account", account,
			"identifier", id,
			"signal", signal,
			"error", err.Error())
		return
	}
	_, _ = fmt.Fprintf(out, "Confirmed identity %s on %s (signal: %s).\n", id, account, signal)
}
