package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/wesm/msgvault/internal/remote"
	"github.com/wesm/msgvault/internal/store"
)

// runStartupMigrations pulls legacy identity addresses from the global config
// and runs the one-time migration. If migration was performed, the notice is
// logged and printed to stderr. If the migration is deferred because no source
// exists yet, it will be retried on a later command after a source has been
// created — and ingest commands that create the first source should call
// runPostSourceCreateMigrations after GetOrCreateSource so the deferred
// migration applies on the same invocation.
//
// Always returns nil unless the migration itself errors.
func runStartupMigrations(s *store.Store) error {
	addrs := cfg.Identity.Addresses
	res, err := s.RunStartupMigrations(addrs)
	if err != nil {
		logger.Warn("startup migration failed", "error", err)
		return err
	}
	// Success cases log at Info (the operation succeeded; res.Notice is
	// the user-facing surface on stderr). Reserved Warn for the actual
	// error path above.
	switch {
	case res.Deferred:
		logger.Info("legacy [identity] block in config detected (migration deferred until a source exists)",
			"address_count", res.AddressCount,
			"hint", "run 'msgvault add-account ...' to create a source; the migration will retry on the next command")
	case res.Applied:
		logger.Info("legacy identity migrated",
			"addresses", res.AddressCount,
			"sources", res.SourceCount)
	}
	if res.Notice != "" {
		fmt.Fprintln(os.Stderr, res.Notice)
	}
	return nil
}

// runStartupMigrationsForIngest is the pre-source-create hook for ingest
// commands. The only startup migration today is MigrateLegacyIdentityConfig,
// which writes to account_identities — and any pre-source-create write
// races confirmDefaultIdentity by populating identity rows before the
// source's own identifier is confirmed, causing confirmDefaultIdentity's
// `len(existing) > 0` guard to skip the source's own address (regression
// caught upstream at iter20).
//
// All ingest paths already invoke runPostSourceCreateMigrations after
// confirmDefaultIdentity, which handles the legacy migration correctly
// in the deferred (no-source) case and is a no-op once the migration
// sentinel is set. So this pre-source call is intentionally a no-op
// to avoid the race. Kept as a named hook so future startup work that
// genuinely belongs *before* source creation has an obvious place to
// land without re-introducing the legacy-identity race.
func runStartupMigrationsForIngest(s *store.Store) error {
	_ = s
	return nil
}

// runPostSourceCreateMigrations re-runs startup migrations after the caller
// has just created a source. The legacy identity migration defers when no
// source exists at startup, so on a fresh install the very first
// add-account / add-imap / add-o365 / import-* invocation needs a second
// pass to actually apply the migration on the same invocation that created
// the first source. Subsequent calls are O(1) — once the migration sentinel
// is set, MigrateLegacyIdentityConfig short-circuits.
func runPostSourceCreateMigrations(s *store.Store) error {
	return runStartupMigrations(s)
}

// MessageStore is the interface for commands that need basic message operations.
// Both store.Store and remote.Store implement this interface.
type MessageStore interface {
	GetStats() (*store.Stats, error)
	ListMessages(offset, limit int) ([]store.APIMessage, int64, error)
	GetMessage(id int64) (*store.APIMessage, error)
	SearchMessages(query string, offset, limit int) ([]store.APIMessage, int64, error)
	Close() error
}

// RemoteStore extends MessageStore with remote-specific operations.
type RemoteStore interface {
	MessageStore
	ListAccounts() ([]remote.AccountInfo, error)
}

// IsRemoteMode returns true if commands should use remote server.
// Resolution order:
//  1. --local flag → always local
//  2. [remote].url set in config → use remote
//  3. Default → use local DB
func IsRemoteMode() bool {
	if useLocal {
		return false
	}
	return cfg != nil && cfg.Remote.URL != ""
}

// OpenStore returns either a local or remote store based on configuration.
// If [remote].url is set in config and --local is not specified, uses remote.
// Otherwise uses local SQLite database.
func OpenStore() (MessageStore, error) {
	if IsRemoteMode() {
		return openRemoteStore()
	}
	return openLocalStore()
}

// OpenRemoteStore opens a remote store, returning error if not configured.
// Unlike OpenStore, this always attempts remote connection.
func OpenRemoteStore() (RemoteStore, error) {
	if cfg.Remote.URL == "" {
		return nil, fmt.Errorf("remote server not configured\n\n" +
			"Configure in ~/.msgvault/config.toml:\n" +
			"  [remote]\n" +
			"  url = \"http://nas:8080\"\n" +
			"  api_key = \"your-api-key\"\n" +
			"  allow_insecure = true  # for trusted networks")
	}
	return openRemoteStore()
}

// openLocalStore opens the local SQLite database.
func openLocalStore() (*store.Store, error) {
	dbPath := cfg.DatabaseDSN()
	return store.Open(dbPath)
}

// openLocalStoreAndInit opens the local SQLite database, initializes the
// schema, and runs startup migrations. Callers that previously called
// store.Open + s.InitSchema separately should migrate to this helper.
func openLocalStoreAndInit() (*store.Store, error) {
	s, err := openLocalStore()
	if err != nil {
		return nil, err
	}
	if err := s.InitSchema(); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := runStartupMigrations(s); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("startup migrations: %w", err)
	}
	return s, nil
}

// openRemoteStore creates a remote store client.
func openRemoteStore() (*remote.Store, error) {
	return remote.New(remote.Config{
		URL:           cfg.Remote.URL,
		APIKey:        cfg.Remote.APIKey,
		AllowInsecure: cfg.Remote.AllowInsecure,
		Timeout:       30 * time.Second,
	})
}

// MustBeLocal returns an error if remote mode is active.
// Use this for commands that only work with local database.
func MustBeLocal(cmdName string) error {
	if IsRemoteMode() && !useLocal {
		return fmt.Errorf("%s requires local database, "+
			"this command cannot run against a remote server, "+
			"use --local flag to force local database", cmdName)
	}
	return nil
}
