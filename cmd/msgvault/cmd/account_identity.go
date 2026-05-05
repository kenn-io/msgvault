package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/wesm/msgvault/internal/store"
)

// noDefaultIdentityHelp is the flag help text for --no-default-identity.
// Each ingest command registers its own bool variable and reuses this constant.
const noDefaultIdentityHelp = "Suppress auto-default-identity at account creation. " +
	"Note: a one-time legacy [identity] config migration may still write confirmed " +
	"identifiers to the account on first post-upgrade startup."

// confirmDefaultIdentity writes one confirmed identifier to a freshly
// created source's identity. Best-effort: any error is logged and swallowed
// so a partially failed identity write never breaks ingest. Empty identifiers
// are a silent no-op.
//
// Skips the write when the source already has at least one identity row.
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
func confirmDefaultIdentity(out io.Writer, s *store.Store, sourceID int64, account, identifier, signal string) {
	id := strings.TrimSpace(identifier)
	if id == "" {
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
