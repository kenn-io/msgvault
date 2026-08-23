package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// CardDAVDiscoveryInput is one complete, successfully enumerated discovery
// snapshot. Its Books list is authoritative for active books; deliberately
// ignored books and books with unresolved remote-first work retain their stored
// identity while temporarily absent.
type CardDAVDiscoveryInput struct {
	BaseURL  string
	Username string
	// CredentialsChanged advances the connection fence even when the URL and
	// username are unchanged, invalidating work authenticated with an old secret.
	CredentialsChanged bool
	PrincipalURL       string
	HomeURL            string
	HomeURLs           []string
	Books              []CardDAVDiscoveredBook
}

// CardDAVDiscoveredBook is the store-neutral form of one discovered book.
// Nil capabilities mean the server did not advertise the privilege.
type CardDAVDiscoveredBook struct {
	CanonicalURL           string
	DiscoveryAliasURL      string
	DisplayName            string
	DiscoveryIndex         int
	SupportsSyncCollection bool
	SupportsMultiget       bool
	SupportedVCardVersions []string
	CanCreate              *bool
	CanUpdate              *bool
	CanDelete              *bool
}

type CardDAVAccount struct {
	ID                   int64
	BaseURL              string
	Username             string
	PrincipalURL         string
	HomeURL              string
	HomeURLs             []string
	ConnectionGeneration int64
	DiscoveryRevision    int64
}

type CardDAVAddressBook struct {
	ID                     int64
	AccountID              int64
	CanonicalURL           string
	DiscoveryAliasURL      string
	DisplayName            string
	DiscoveryIndex         int
	SupportsSyncCollection bool
	SupportsMultiget       bool
	SupportedVCardVersions []string
	CanCreate              *bool
	CanUpdate              *bool
	CanDelete              *bool
	IsWriteTarget          bool
	IsSubscribed           bool
	IsLookupSource         bool
	SyncToken              string
	SyncRevision           int64
	NeedsFullReconcile     bool
	LastSeenRevision       int64
}

type CardDAVBookRoles struct {
	IsWriteTarget  bool
	IsSubscribed   bool
	IsLookupSource bool
}

var (
	ErrCardDAVAddressBookNotFound     = errors.New("CardDAV address book not found")
	ErrCardDAVWriteTargetSubscribed   = errors.New("CardDAV write target must remain subscribed")
	ErrCardDAVReadOnlyAddressBook     = errors.New("CardDAV address book is read-only")
	ErrCardDAVRoleChangePending       = errors.New("CardDAV role change would strand remote ownership or intent")
	ErrCardDAVCredentialChangePending = errors.New("CardDAV credential change blocked by pending remote-first state")
	ErrCardDAVIdentityChangeOwned     = errors.New("CardDAV identity change blocked by owned remote state")
)

// ReplaceCardDAVDiscoveryContext atomically replaces the complete discovery
// snapshot. Existing role choices survive rediscovery; deliberately ignored
// books and books with unresolved remote-first work also survive a temporary
// absence. Only the first complete discovery may choose a write target.
func (s *Store) ReplaceCardDAVDiscoveryContext(
	ctx context.Context, input CardDAVDiscoveryInput,
) (_ *CardDAVAccount, _ []CardDAVAddressBook, retErr error) {
	if err := validateCardDAVDiscoveryInput(input); err != nil {
		return nil, nil, err
	}
	homeURLs := cardDAVDiscoveryHomeURLs(input)
	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin CardDAV discovery replacement: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = tx.Rollback()
		}
	}()
	logged := &loggedTx{Tx: tx, rebind: s.Rebind}
	if err := lockCardDAVDiscoveryReplacement(ctx, tx, s.Rebind, s.IsPostgreSQL()); err != nil {
		return nil, nil, err
	}

	account, err := getCardDAVAccountForUpdateFrom(ctx, tx, s.Rebind, s.dialect.SelectForUpdate())
	if err != nil {
		return nil, nil, err
	}
	identityChanged := account != nil &&
		(account.BaseURL != input.BaseURL || account.Username != input.Username)
	if identityChanged || input.CredentialsChanged {
		if err := cardDAVConnectionChangeBlocker(ctx, logged, identityChanged); err != nil {
			return nil, nil, err
		}
	}
	existing, err := listCardDAVBooksFrom(ctx, tx, s.Rebind)
	if err != nil {
		return nil, nil, err
	}
	if identityChanged {
		if err := s.lockIdentityMutationTxContext(ctx, logged); err != nil {
			return nil, nil, err
		}
		for _, oldBook := range existing {
			if err := s.dropCardDAVBookResourcesForIdentityChangeTx(ctx, logged, oldBook.ID); err != nil {
				return nil, nil, fmt.Errorf("clean prior CardDAV connection book: %w", err)
			}
		}
		if _, err := logged.ExecContext(ctx,
			`DELETE FROM carddav_address_books WHERE account_id = 1`); err != nil {
			return nil, nil, fmt.Errorf("delete prior CardDAV connection books: %w", err)
		}
		if _, err := logged.ExecContext(ctx,
			`DELETE FROM carddav_retry_gate WHERE account_id = 1`); err != nil {
			return nil, nil, fmt.Errorf("clear prior CardDAV connection retry gate: %w", err)
		}
		existing = nil
	}
	firstDiscovery := account == nil || account.DiscoveryRevision == 0 || identityChanged
	if account == nil {
		account = &CardDAVAccount{ID: 1, ConnectionGeneration: 1}
	} else if identityChanged || input.CredentialsChanged {
		account.ConnectionGeneration++
	}
	account.BaseURL = input.BaseURL
	account.Username = input.Username
	account.PrincipalURL = input.PrincipalURL
	account.HomeURL = input.HomeURL
	account.HomeURLs = append([]string(nil), homeURLs...)
	account.DiscoveryRevision++
	if _, err := tx.ExecContext(ctx, s.Rebind(`
		INSERT INTO carddav_accounts (
			id, base_url, username, principal_url, home_url,
			connection_generation, discovery_revision, discovered_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			base_url = excluded.base_url,
			username = excluded.username,
			principal_url = excluded.principal_url,
			home_url = excluded.home_url,
			connection_generation = excluded.connection_generation,
			discovery_revision = excluded.discovery_revision,
			discovered_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP`),
		account.ID, account.BaseURL, account.Username, account.PrincipalURL,
		account.HomeURL, account.ConnectionGeneration, account.DiscoveryRevision,
	); err != nil {
		return nil, nil, fmt.Errorf("save CardDAV account discovery: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.Rebind(
		`DELETE FROM carddav_account_home_urls WHERE account_id = ?`), account.ID); err != nil {
		return nil, nil, fmt.Errorf("clear CardDAV account home URLs: %w", err)
	}
	for index, homeURL := range homeURLs {
		if _, err := tx.ExecContext(ctx, s.Rebind(`
			INSERT INTO carddav_account_home_urls (account_id, home_url, discovery_index)
			VALUES (?, ?, ?)`), account.ID, homeURL, index); err != nil {
			return nil, nil, fmt.Errorf("save CardDAV account home URL: %w", err)
		}
	}

	matches := make([]*CardDAVAddressBook, len(input.Books))
	claimedBookIDs := make(map[int64]bool, len(input.Books))
	for index, discovered := range input.Books {
		matches[index], err = matchDiscoveredCardDAVBook(existing, discovered)
		if err != nil {
			return nil, nil, err
		}
		if matches[index] == nil {
			continue
		}
		if claimedBookIDs[matches[index].ID] {
			return nil, nil, errors.New("multiple discovered CardDAV books match the same stored book")
		}
		claimedBookIDs[matches[index].ID] = true
	}
	for _, matched := range matches {
		if matched == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, s.Rebind(
			`DELETE FROM carddav_address_book_urls WHERE address_book_id = ?`), matched.ID); err != nil {
			return nil, nil, fmt.Errorf("clear discovered CardDAV address book URL identities: %w", err)
		}
	}
	writeTargetChosen := false
	seenBookIDs := make(map[int64]bool, len(input.Books))
	for index, discovered := range input.Books {
		matched := matches[index]
		versions, err := json.Marshal(discovered.SupportedVCardVersions)
		if err != nil {
			return nil, nil, fmt.Errorf("encode CardDAV supported vCard versions: %w", err)
		}
		if matched != nil {
			canonicalURL := discovered.CanonicalURL
			aliasURL := discovered.DiscoveryAliasURL
			if aliasURL == "" && matched.DiscoveryAliasURL != "" {
				if cardDAVURLIdentityEqual(matched.DiscoveryAliasURL, canonicalURL) {
					canonicalURL = matched.CanonicalURL
				}
				aliasURL = matched.DiscoveryAliasURL
			}
			canonicalURLChanged := canonicalURL != matched.CanonicalURL
			if _, err := tx.ExecContext(ctx, s.Rebind(`
				UPDATE carddav_address_books SET
					canonical_url = ?, discovery_alias_url = NULLIF(?, ''),
					display_name = ?, discovery_index = ?,
					supports_sync_collection = ?, supports_multiget = ?,
					supported_vcard_versions = `+s.dialect.JSONBindExpr()+`, can_create = ?, can_update = ?, can_delete = ?,
					sync_token = CASE WHEN ? THEN '' ELSE sync_token END,
					needs_full_reconcile = CASE WHEN ? THEN TRUE ELSE needs_full_reconcile END,
					sync_revision = sync_revision + CASE WHEN ? THEN 1 ELSE 0 END,
					last_seen_revision = ?, updated_at = CURRENT_TIMESTAMP
				WHERE id = ?`), canonicalURL, aliasURL,
				discovered.DisplayName, discovered.DiscoveryIndex,
				discovered.SupportsSyncCollection, discovered.SupportsMultiget,
				string(versions), discovered.CanCreate, discovered.CanUpdate, discovered.CanDelete,
				canonicalURLChanged, canonicalURLChanged, canonicalURLChanged,
				account.DiscoveryRevision, matched.ID,
			); err != nil {
				return nil, nil, fmt.Errorf("update discovered CardDAV address book: %w", err)
			}
			if err := insertCardDAVBookURLIdentities(ctx, tx, s.Rebind,
				account.ID, matched.ID, canonicalURL, aliasURL); err != nil {
				return nil, nil, err
			}
			seenBookIDs[matched.ID] = true
			continue
		}

		isWriteTarget := firstDiscovery && !writeTargetChosen &&
			cardDAVDiscoveredBookSupportsWriteLifecycle(discovered)
		if isWriteTarget {
			writeTargetChosen = true
		}
		var bookID int64
		if err := tx.QueryRowContext(ctx, s.Rebind(`
			INSERT INTO carddav_address_books (
				account_id, canonical_url, discovery_alias_url, display_name, discovery_index,
				supports_sync_collection, supports_multiget, supported_vcard_versions,
				can_create, can_update, can_delete,
				is_write_target, is_subscribed, is_lookup_source, last_seen_revision
			) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ?, ?, ?, ?, ?, TRUE, ?)
			RETURNING id`),
			account.ID, discovered.CanonicalURL, discovered.DiscoveryAliasURL,
			discovered.DisplayName, discovered.DiscoveryIndex,
			discovered.SupportsSyncCollection, discovered.SupportsMultiget, string(versions),
			discovered.CanCreate, discovered.CanUpdate, discovered.CanDelete,
			isWriteTarget, isWriteTarget, account.DiscoveryRevision,
		).Scan(&bookID); err != nil {
			return nil, nil, fmt.Errorf("insert discovered CardDAV address book: %w", err)
		}
		if err := insertCardDAVBookURLIdentities(ctx, tx, s.Rebind,
			account.ID, bookID, discovered.CanonicalURL, discovered.DiscoveryAliasURL); err != nil {
			return nil, nil, err
		}
		seenBookIDs[bookID] = true
	}
	for _, stale := range existing {
		if seenBookIDs[stale.ID] {
			continue
		}
		ignored := !stale.IsWriteTarget && !stale.IsSubscribed && !stale.IsLookupSource
		if ignored {
			continue
		}
		protected, err := cardDAVBookHasProtectedStateTx(ctx, logged, stale.ID,
			cardDAVProtectedStateScope{IncludeAllConflicts: true})
		if err != nil {
			return nil, nil, err
		}
		if protected {
			continue
		}
		if err := s.dropCardDAVBookResourcesTx(ctx, logged, stale.ID); err != nil {
			return nil, nil, fmt.Errorf("clean unseen CardDAV address book: %w", err)
		}
		if _, err := logged.ExecContext(ctx, `DELETE FROM carddav_address_books WHERE id = ?`, stale.ID); err != nil {
			return nil, nil, fmt.Errorf("prune unseen CardDAV address book: %w", err)
		}
	}
	books, err := listCardDAVBooksFrom(ctx, tx, s.Rebind)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit CardDAV discovery replacement: %w", err)
	}
	return account, books, nil
}

// ValidateCardDAVConnectionChangeContext is the no-network admission check
// used by account setup. ReplaceCardDAVDiscoveryContext repeats the check
// under the discovery transaction so a racing writer still fails closed.
func (s *Store) ValidateCardDAVConnectionChangeContext(
	ctx context.Context, baseURL, username string, credentialsChanged bool,
) error {
	account, err := s.GetCardDAVAccountContext(ctx)
	if err != nil || account == nil {
		return err
	}
	identityChanged := account.BaseURL != baseURL || account.Username != username
	if !identityChanged && !credentialsChanged {
		return nil
	}
	return cardDAVConnectionChangeBlocker(ctx, s.db, identityChanged)
}

func cardDAVConnectionChangeBlocker(
	ctx context.Context, queryer contextRowQuerier, identityChanged bool,
) error {
	query := `SELECT
		EXISTS (SELECT 1 FROM carddav_publications WHERE pending_operation IS NOT NULL)
		OR EXISTS (SELECT 1 FROM carddav_conflicts WHERE pending_operation IS NOT NULL)`
	want := ErrCardDAVCredentialChangePending
	if identityChanged {
		query = `SELECT
			EXISTS (SELECT 1 FROM carddav_publications)
			OR EXISTS (SELECT 1 FROM carddav_conflicts)`
		want = ErrCardDAVIdentityChangeOwned
	}
	var blocked bool
	if err := queryer.QueryRowContext(ctx, query).Scan(&blocked); err != nil {
		return fmt.Errorf("check CardDAV connection change ownership: %w", err)
	}
	if blocked {
		return want
	}
	return nil
}

type cardDAVProtectedStateScope struct {
	IncludeCurrentWriteTarget  bool
	IncludeUnresolvedConflicts bool
	IncludeAllConflicts        bool
}

func cardDAVBookHasProtectedStateTx(
	ctx context.Context, tx *loggedTx, bookID int64, scope cardDAVProtectedStateScope,
) (bool, error) {
	var protected bool
	if err := tx.QueryRowContext(ctx, `WITH affected_books AS (
		SELECT id FROM carddav_address_books
		WHERE account_id = 1
		  AND (id = ? OR (? AND is_write_target = TRUE))
	)
	SELECT
		EXISTS (SELECT 1 FROM carddav_publications
			WHERE address_book_id IN (SELECT id FROM affected_books))
		OR EXISTS (SELECT 1 FROM carddav_conflicts
			WHERE address_book_id IN (SELECT id FROM affected_books)
			  AND (pending_operation IS NOT NULL
			       OR (? AND status = 'unresolved') OR ?))`,
		bookID, scope.IncludeCurrentWriteTarget, scope.IncludeUnresolvedConflicts,
		scope.IncludeAllConflicts,
	).Scan(&protected); err != nil {
		return false, fmt.Errorf("check CardDAV address book protected state: %w", err)
	}
	return protected, nil
}

func lockCardDAVDiscoveryReplacement(
	ctx context.Context, tx *sql.Tx, rebind func(string) string, postgres bool,
) error {
	if postgres {
		var singleton int
		if err := tx.QueryRowContext(ctx,
			`SELECT singleton FROM carddav_discovery_lock WHERE singleton = 1 FOR UPDATE`,
		).Scan(&singleton); err != nil {
			return fmt.Errorf("lock CardDAV discovery replacement: %w", err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, rebind(
		`UPDATE carddav_discovery_lock SET singleton = singleton WHERE singleton = ?`), 1); err != nil {
		return fmt.Errorf("lock CardDAV discovery replacement: %w", err)
	}
	return nil
}

func insertCardDAVBookURLIdentities(
	ctx context.Context, tx *sql.Tx, rebind func(string) string,
	accountID, bookID int64, canonicalURL, aliasURL string,
) error {
	for _, identity := range []struct {
		role, rawURL string
	}{{"canonical", canonicalURL}, {"alias", aliasURL}} {
		if identity.rawURL == "" {
			continue
		}
		normalized, err := normalizeCardDAVURLIdentity(identity.rawURL)
		if err != nil {
			return fmt.Errorf("normalize CardDAV %s URL identity: %w", identity.role, err)
		}
		if _, err := tx.ExecContext(ctx, rebind(`
			INSERT INTO carddav_address_book_urls (
				account_id, address_book_id, url_role, normalized_url
			) VALUES (?, ?, ?, ?)`), accountID, bookID, identity.role, normalized); err != nil {
			return fmt.Errorf("save CardDAV %s URL identity: %w", identity.role, err)
		}
	}
	return nil
}

func (s *Store) GetCardDAVAccountContext(ctx context.Context) (*CardDAVAccount, error) {
	return getCardDAVAccountFrom(ctx, s.db.DB, s.Rebind)
}

func (s *Store) ListCardDAVAddressBooksContext(ctx context.Context) ([]CardDAVAddressBook, error) {
	return listCardDAVBooksFrom(ctx, s.db.DB, s.Rebind)
}

// SetCardDAVBookRolesContext applies one complete role state. The account row
// serializes write-target swaps, while sync revisions fence pulls and pending
// writes prepared against the previous state.
func (s *Store) SetCardDAVBookRolesContext(
	ctx context.Context, bookID int64, roles CardDAVBookRoles,
) error {
	if bookID <= 0 {
		return ErrCardDAVAddressBookNotFound
	}
	if roles.IsWriteTarget && !roles.IsSubscribed {
		return ErrCardDAVWriteTargetSubscribed
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts
			WHERE id = 1`+s.dialect.SelectForUpdate()).Scan(&generation); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCardDAVAddressBookNotFound
			}
			return fmt.Errorf("lock CardDAV account roles: %w", err)
		}

		var current CardDAVBookRoles
		var canCreate, canUpdate, canDelete sql.NullBool
		if err := tx.QueryRowContext(ctx, `SELECT can_create, can_update, can_delete, is_write_target,
			is_subscribed, is_lookup_source FROM carddav_address_books
			WHERE id = ?`+s.dialect.SelectForUpdate(), bookID).Scan(
			&canCreate, &canUpdate, &canDelete,
			&current.IsWriteTarget, &current.IsSubscribed, &current.IsLookupSource,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCardDAVAddressBookNotFound
			}
			return fmt.Errorf("lock CardDAV address book roles: %w", err)
		}
		if roles.IsWriteTarget && (cardDAVCapabilityDenied(canCreate) ||
			cardDAVCapabilityDenied(canUpdate) || cardDAVCapabilityDenied(canDelete)) {
			return ErrCardDAVReadOnlyAddressBook
		}
		if current == roles {
			return nil
		}
		protectedScope := cardDAVProtectedStateScope{}
		checkProtectedState := false
		if current.IsWriteTarget != roles.IsWriteTarget {
			checkProtectedState = true
			protectedScope.IncludeCurrentWriteTarget = true
			protectedScope.IncludeUnresolvedConflicts = true
		}
		removesMaterializedMappings := (current.IsSubscribed && !roles.IsSubscribed) ||
			(current.IsLookupSource && !roles.IsSubscribed && !roles.IsLookupSource)
		if removesMaterializedMappings {
			checkProtectedState = true
			protectedScope.IncludeUnresolvedConflicts = true
		}
		if checkProtectedState {
			protected, err := cardDAVBookHasProtectedStateTx(ctx, tx, bookID,
				protectedScope)
			if err != nil {
				return err
			}
			if protected {
				return ErrCardDAVRoleChangePending
			}
		}

		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if roles.IsWriteTarget && !current.IsWriteTarget {
			if _, err := tx.ExecContext(ctx, `UPDATE carddav_address_books SET
				is_write_target = FALSE, sync_revision = sync_revision + 1,
				updated_at = `+s.dialect.Now()+`
				WHERE account_id = 1 AND is_write_target = TRUE AND id <> ?`, bookID); err != nil {
				return fmt.Errorf("release previous CardDAV write target: %w", err)
			}
		}

		widened := (!current.IsSubscribed && roles.IsSubscribed) ||
			(!current.IsLookupSource && roles.IsLookupSource)
		if current.IsSubscribed && !roles.IsSubscribed {
			if err := s.demoteCardDAVBookResourcesTx(ctx, tx, bookID); err != nil {
				return err
			}
		}
		if !roles.IsSubscribed && !roles.IsLookupSource {
			if err := s.dropCardDAVBookResourcesTx(ctx, tx, bookID); err != nil {
				return err
			}
			widened = false
		}

		result, err := tx.ExecContext(ctx, `UPDATE carddav_address_books SET
			is_write_target = ?, is_subscribed = ?, is_lookup_source = ?,
			needs_full_reconcile = CASE
				WHEN ? THEN FALSE
				WHEN ? THEN TRUE
				ELSE needs_full_reconcile
			END, sync_revision = sync_revision + 1,
			updated_at = `+s.dialect.Now()+` WHERE id = ?`,
			roles.IsWriteTarget, roles.IsSubscribed, roles.IsLookupSource,
			!roles.IsSubscribed && !roles.IsLookupSource, widened, bookID)
		if err != nil {
			return fmt.Errorf("set CardDAV address book roles: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("count CardDAV address book role update: %w", err)
		} else if affected != 1 {
			return ErrCardDAVAddressBookNotFound
		}
		return nil
	})
}

func cardDAVDiscoveredBookSupportsWriteLifecycle(book CardDAVDiscoveredBook) bool {
	return book.CanCreate != nil && *book.CanCreate &&
		(book.CanUpdate == nil || *book.CanUpdate) &&
		(book.CanDelete == nil || *book.CanDelete)
}

func cardDAVCapabilityDenied(capability sql.NullBool) bool {
	return capability.Valid && !capability.Bool
}

type cardDAVQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getCardDAVAccountFrom(
	ctx context.Context, queryer cardDAVQueryer, rebind func(string) string,
) (*CardDAVAccount, error) {
	return getCardDAVAccountForUpdateFrom(ctx, queryer, rebind, "")
}

func getCardDAVAccountForUpdateFrom(
	ctx context.Context, queryer cardDAVQueryer, rebind func(string) string, suffix string,
) (*CardDAVAccount, error) {
	var account CardDAVAccount
	err := queryer.QueryRowContext(ctx, rebind(`
		SELECT id, base_url, username, principal_url, home_url,
		       connection_generation, discovery_revision
		FROM carddav_accounts WHERE id = 1`)+suffix).Scan(
		&account.ID, &account.BaseURL, &account.Username, &account.PrincipalURL,
		&account.HomeURL, &account.ConnectionGeneration, &account.DiscoveryRevision,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // An unconfigured singleton account is a valid absence state.
	}
	if err != nil {
		return nil, fmt.Errorf("get CardDAV account: %w", err)
	}
	rows, err := queryer.QueryContext(ctx, rebind(`
		SELECT home_url FROM carddav_account_home_urls
		WHERE account_id = ? ORDER BY discovery_index`), account.ID)
	if err != nil {
		return nil, fmt.Errorf("list CardDAV account home URLs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	for rows.Next() {
		var homeURL string
		if err := rows.Scan(&homeURL); err != nil {
			return nil, fmt.Errorf("scan CardDAV account home URL: %w", err)
		}
		account.HomeURLs = append(account.HomeURLs, homeURL)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate CardDAV account home URLs: %w", err)
	}
	if len(account.HomeURLs) == 0 {
		account.HomeURLs = []string{account.HomeURL}
	}
	return &account, nil
}

func listCardDAVBooksFrom(
	ctx context.Context, queryer cardDAVQueryer, rebind func(string) string,
) ([]CardDAVAddressBook, error) {
	rows, err := queryer.QueryContext(ctx, rebind(`
		SELECT id, account_id, canonical_url, discovery_alias_url, display_name,
		       discovery_index, supports_sync_collection, supports_multiget,
		       supported_vcard_versions, can_create, can_update, can_delete,
		       is_write_target, is_subscribed, is_lookup_source,
		       sync_token, sync_revision, needs_full_reconcile, last_seen_revision
		FROM carddav_address_books
		ORDER BY discovery_index, id`))
	if err != nil {
		return nil, fmt.Errorf("list CardDAV address books: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	books := []CardDAVAddressBook{}
	for rows.Next() {
		var book CardDAVAddressBook
		var alias sql.NullString
		var versions []byte
		if err := rows.Scan(
			&book.ID, &book.AccountID, &book.CanonicalURL, &alias, &book.DisplayName,
			&book.DiscoveryIndex, &book.SupportsSyncCollection, &book.SupportsMultiget,
			&versions, &book.CanCreate, &book.CanUpdate, &book.CanDelete,
			&book.IsWriteTarget, &book.IsSubscribed, &book.IsLookupSource,
			&book.SyncToken, &book.SyncRevision, &book.NeedsFullReconcile, &book.LastSeenRevision,
		); err != nil {
			return nil, fmt.Errorf("scan CardDAV address book: %w", err)
		}
		if alias.Valid {
			book.DiscoveryAliasURL = alias.String
		}
		if err := json.Unmarshal(versions, &book.SupportedVCardVersions); err != nil {
			return nil, fmt.Errorf("decode CardDAV supported vCard versions: %w", err)
		}
		books = append(books, book)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate CardDAV address books: %w", err)
	}
	return books, nil
}

func validateCardDAVDiscoveryInput(input CardDAVDiscoveryInput) error {
	for label, value := range map[string]string{
		"base URL": input.BaseURL, "principal URL": input.PrincipalURL, "home URL": input.HomeURL,
	} {
		if _, err := normalizeCardDAVURLIdentity(value); err != nil {
			return fmt.Errorf("invalid CardDAV %s", label)
		}
	}
	homeURLs := cardDAVDiscoveryHomeURLs(input)
	seenHomes := map[string]bool{}
	for _, homeURL := range homeURLs {
		key, err := normalizeCardDAVURLIdentity(homeURL)
		if err != nil {
			return errors.New("invalid CardDAV home URL")
		}
		if seenHomes[key] {
			return fmt.Errorf("duplicate CardDAV home URL %q", homeURL)
		}
		seenHomes[key] = true
	}
	primary, _ := normalizeCardDAVURLIdentity(input.HomeURL)
	first, _ := normalizeCardDAVURLIdentity(homeURLs[0])
	if primary != first {
		return errors.New("CardDAV primary home URL must be first")
	}
	seen := map[string]bool{}
	for _, book := range input.Books {
		if book.DiscoveryIndex < 0 {
			return errors.New("CardDAV discovery index must not be negative")
		}
		for label, value := range map[string]string{"canonical URL": book.CanonicalURL, "discovery alias URL": book.DiscoveryAliasURL} {
			if value == "" && label == "discovery alias URL" {
				continue
			}
			key, err := normalizeCardDAVURLIdentity(value)
			if err != nil {
				return fmt.Errorf("invalid CardDAV book %s", label)
			}
			if seen[key] {
				return fmt.Errorf("duplicate CardDAV book URL %q", value)
			}
			seen[key] = true
		}
	}
	return nil
}

func cardDAVDiscoveryHomeURLs(input CardDAVDiscoveryInput) []string {
	if len(input.HomeURLs) > 0 {
		return input.HomeURLs
	}
	return []string{input.HomeURL}
}

func matchDiscoveredCardDAVBook(
	existing []CardDAVAddressBook, discovered CardDAVDiscoveredBook,
) (*CardDAVAddressBook, error) {
	var match *CardDAVAddressBook
	for index := range existing {
		book := &existing[index]
		if !cardDAVURLsOverlap(book.CanonicalURL, book.DiscoveryAliasURL, discovered.CanonicalURL, discovered.DiscoveryAliasURL) {
			continue
		}
		if match != nil && match.ID != book.ID {
			return nil, errors.New("CardDAV discovery aliases match multiple stored books")
		}
		clone := *book
		match = &clone
	}
	return match, nil
}

func cardDAVURLsOverlap(leftCanonical, leftAlias, rightCanonical, rightAlias string) bool {
	for _, left := range []string{leftCanonical, leftAlias} {
		for _, right := range []string{rightCanonical, rightAlias} {
			if left != "" && right != "" && cardDAVURLIdentityEqual(left, right) {
				return true
			}
		}
	}
	return false
}

func cardDAVURLIdentityEqual(left, right string) bool {
	leftIdentity, leftErr := normalizeCardDAVURLIdentity(left)
	rightIdentity, rightErr := normalizeCardDAVURLIdentity(right)
	return leftErr == nil && rightErr == nil && leftIdentity == rightIdentity
}

// normalizeCardDAVURLIdentity folds only URL components that are
// case-insensitive. Collection paths, queries, and their escaping remain
// byte-sensitive because CardDAV servers may assign them distinct resources.
func normalizeCardDAVURLIdentity(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("invalid CardDAV URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("invalid CardDAV URL scheme")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", errors.New("invalid CardDAV URL host")
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	identity := scheme + "://" + host + parsed.EscapedPath()
	if parsed.ForceQuery || parsed.RawQuery != "" {
		identity += "?" + parsed.RawQuery
	}
	return identity, nil
}
