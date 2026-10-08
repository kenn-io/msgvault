package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/mail"
	"net/url"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/emailattribution"
)

// Account attribution maps each email and calendar row to the confirmed
// account that received or sent it: messages.account_address plus
// messages.account_path. account_path IS NULL marks a row whose attribution is
// still pending; the account-attribution re-derivation pass fills it in.
const (
	accountPathInbound  = "inbound"
	accountPathSent     = "sent"
	accountPathCalendar = "calendar"

	// accountHeaderRawPrefix bounds the stored blob bytes read for one header
	// block. The slack lets a compressed prefix decode past MaxHeaderBytes.
	accountHeaderRawPrefix = emailattribution.MaxHeaderBytes + 64<<10

	deliveryKindOriginal  = "original"
	deliveryKindDelivered = "delivered"

	accountRepairPageSize = 500

	// accountPendingPredicate selects rows whose attribution is still pending.
	// idx_messages_account_pending repeats it verbatim so the planner matches
	// the repair query to that partial index.
	accountPendingPredicate = `account_path IS NULL AND COALESCE(message_type, '') IN ('', 'email', 'calendar_event')`
)

// accountPendingPageQuery reads one page of a source's pending rows.
const accountPendingPageQuery = `SELECT id FROM messages
	WHERE source_id = ? AND ` + accountPendingPredicate + ` AND id > ?
	ORDER BY id LIMIT ?`

var (
	// errAttributionLockMissing means a transaction tried to derive a row
	// whose source it did not lock through withAttributionTxContext.
	errAttributionLockMissing = errors.New("account attribution requires the source lock")
	// errAttributionLockUpgrade means a shared attribution transaction asked
	// for the exclusive identity lock, which would invert the lock order.
	errAttributionLockUpgrade = errors.New("shared account attribution transaction cannot take the exclusive identity lock")
)

// attributionLock names the locks a transaction takes before the sync fence.
// Exclusive takes the identity row for writing and excludes every other
// attribution transaction, so it lists no sources.
type attributionLock struct {
	Exclusive bool
	Sources   []int64
}

type attributionLockState struct {
	exclusive bool
	sources   map[int64]struct{}
	// flips are label-definition changes the entry applies before commit.
	flips []*labelOutboundFlips
	// identityKeys caches each source's confirmed identities for a shared
	// transaction, where they cannot change.
	identityKeys map[int64][]string
}

// sourceIdentityKeys returns a source's confirmed identities, read once per
// shared transaction. Identities change only under the exclusive lock, so an
// exclusive transaction reads them every time.
func (st *attributionLockState) sourceIdentityKeys(
	ctx context.Context, tx *loggedTx, sourceID int64,
) ([]string, error) {
	if st.exclusive {
		return sourceIdentityKeysTx(ctx, tx, sourceID)
	}
	if keys, ok := st.identityKeys[sourceID]; ok {
		return keys, nil
	}
	keys, err := sourceIdentityKeysTx(ctx, tx, sourceID)
	if err != nil {
		return nil, err
	}
	if st.identityKeys == nil {
		st.identityKeys = make(map[int64][]string)
	}
	st.identityKeys[sourceID] = keys
	return keys, nil
}

func (st *attributionLockState) holds(sourceID int64) bool {
	if st == nil {
		return false
	}
	if st.exclusive {
		return true
	}
	_, ok := st.sources[sourceID]
	return ok
}

// withAttributionTxContext opens every transaction that derives attribution
// or changes an input some message's attribution reads. Lock order is the
// identity row (shared or exclusive), then each listed source row ascending,
// then the sync fence, then fn.
func (s *Store) withAttributionTxContext(
	ctx context.Context, lock attributionLock, fn func(*loggedTx) error,
) error {
	sources := slices.Clone(lock.Sources)
	slices.Sort(sources)
	sources = slices.Compact(sources)
	if lock.Exclusive {
		sources = nil
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error {
		if lock.Exclusive {
			if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
				return err
			}
		} else if err := s.shareIdentityLockTxContext(ctx, tx); err != nil {
			return err
		}
		state := &attributionLockState{exclusive: lock.Exclusive, sources: make(map[int64]struct{}, len(sources))}
		for _, sourceID := range sources {
			// The same row lock lockSyncSourceTx takes. A missing source has
			// no rows to derive, so callers keep reporting their own error.
			if _, err := tx.ExecContext(ctx,
				`UPDATE sources SET updated_at = updated_at WHERE id = ?`, sourceID); err != nil {
				return fmt.Errorf("lock source %d for account attribution: %w", sourceID, err)
			}
			state.sources[sourceID] = struct{}{}
		}
		tx.attribution = state
		if s.attributionAfterLockHook != nil {
			s.attributionAfterLockHook(sources)
		}
		return nil
	}, func(tx *loggedTx) error {
		if err := fn(tx); err != nil {
			return err
		}
		for _, flips := range tx.attribution.flips {
			if err := s.applyLabelOutboundFlipsTx(flips); err != nil {
				return err
			}
		}
		tx.attribution.flips = nil
		return nil
	})
}

// attributionLockForMessage is the shared lock a write of one message needs:
// its source when the row is email or calendar, nothing otherwise.
func attributionLockForMessage(sourceID int64, messageType string) attributionLock {
	if (IsEmailMessageType(messageType) || messageType == "calendar_event") && sourceID > 0 {
		return attributionLock{Sources: []int64{sourceID}}
	}
	return attributionLock{}
}

// labelFlipsTx returns a collector the transaction's attribution entry applies
// before commit, or nil outside an attribution transaction.
func labelFlipsTx(ctx context.Context, tx *loggedTx, sourceID int64) *labelOutboundFlips {
	if tx.attribution == nil {
		return nil
	}
	flips := newLabelOutboundFlips(ctx, tx, sourceID)
	tx.attribution.flips = append(tx.attribution.flips, flips)
	return flips
}

// refreshAccountAttributionAfterWriteTx derives a row a write just stored.
// A transaction that could not name the row's source before it began leaves
// the row pending for the account-attribution pass instead.
func (s *Store) refreshAccountAttributionAfterWriteTx(
	ctx context.Context, tx *loggedTx, id int64, delivery deliveryInput,
) error {
	var sourceID int64
	var messageType string
	err := tx.QueryRowContext(ctx,
		`SELECT source_id, COALESCE(message_type, '') FROM messages WHERE id = ?`, id,
	).Scan(&sourceID, &messageType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read message %d for account attribution: %w", id, err)
	}
	if (IsEmailMessageType(messageType) || messageType == "calendar_event") && !tx.attribution.holds(sourceID) {
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET account_address = NULL, account_path = NULL
			WHERE id = ? AND (account_address IS NOT NULL OR account_path IS NOT NULL)`, id); err != nil {
			return fmt.Errorf("mark message %d account attribution pending: %w", id, err)
		}
		return nil
	}
	_, err = s.refreshAccountAttributionTx(ctx, tx, id, delivery)
	return err
}

// shareIdentityLockTxContext holds the identity revision row FOR SHARE on
// PostgreSQL, so identity writers wait for attribution readers while readers
// of different sources run together. SQLite has one writer, so the source lock
// that follows already serializes everything.
func (s *Store) shareIdentityLockTxContext(ctx context.Context, tx *loggedTx) error {
	if !s.IsPostgreSQL() {
		return nil
	}
	share := func() error {
		var value string
		return tx.QueryRowContext(ctx,
			`SELECT value FROM archive_metadata WHERE key = ? FOR SHARE`, identityRevisionKey,
		).Scan(&value)
	}
	err := share()
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
			`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`),
			identityRevisionKey); err != nil {
			return fmt.Errorf("seed identity revision: %w", err)
		}
		err = share()
	}
	if err != nil {
		return fmt.Errorf("share identity revision: %w", err)
	}
	return nil
}

// messageSourceIDContext reads a message's source before a transaction opens.
// messages.source_id never changes after insert. A missing message yields 0.
func (s *Store) messageSourceIDContext(ctx context.Context, messageID int64) (int64, error) {
	var sourceID int64
	err := s.db.QueryRowContext(ctx, `SELECT source_id FROM messages WHERE id = ?`, messageID).Scan(&sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read message %d source: %w", messageID, err)
	}
	return sourceID, nil
}

// withMessageAttributionTxContext opens a shared attribution transaction that
// locks messageID's source.
func (s *Store) withMessageAttributionTxContext(
	ctx context.Context, messageID int64, fn func(*loggedTx) error,
) error {
	sourceID, err := s.messageSourceIDContext(ctx, messageID)
	if err != nil {
		return err
	}
	lock := attributionLock{}
	if sourceID != 0 {
		lock.Sources = []int64{sourceID}
	}
	return s.withAttributionTxContext(ctx, lock, fn)
}

func sentFolderLabelSQL(l string) string {
	return l + ".system_role = 'sent'"
}

func sentGmailLabelSQL(l, src string) string {
	return "(" + src + ".source_type = 'gmail' AND " + l + ".source_label_id = 'SENT')"
}

// outboundEvidenceLabelSQL is true for a label that provider metadata marks as
// mail the account wrote rather than received: a Sent or Drafts folder role,
// or Gmail's SENT or DRAFT system label.
func outboundEvidenceLabelSQL(l, src string) string {
	return "(" + l + ".system_role IN ('" + LabelSystemRoleSent + "', '" + LabelSystemRoleDrafts + "') OR (" +
		src + ".source_type = 'gmail' AND " + l + ".source_label_id IN ('SENT', 'DRAFT')))"
}

// draftEvidenceSQL is true when message m is a draft: a Drafts folder role,
// Gmail's DRAFT label, an IMAP membership with the \Draft flag, which marks a
// draft even in a mailbox the server gives no Drafts role, or a draft record
// naming it as the current or replaced snapshot. src must be m's source row.
func draftEvidenceSQL(m, src string) string {
	return draftEvidenceInSchemaSQL("", m, src)
}

// draftEvidenceInSchemaSQL is draftEvidenceSQL reading its evidence tables
// from schema, such as "src." for an attached source archive.
func draftEvidenceInSchemaSQL(schema, m, src string) string {
	return `(EXISTS (SELECT 1 FROM ` + schema + `message_labels ml JOIN ` + schema + `labels l ON l.id = ml.label_id
		WHERE ml.message_id = ` + m + `.id AND l.source_id = ` + m + `.source_id
		  AND (l.system_role = '` + LabelSystemRoleDrafts + `'
		       OR (` + src + `.source_type = 'gmail' AND l.source_label_id = 'DRAFT')))
		OR EXISTS (SELECT 1 FROM ` + schema + `imap_message_memberships imm
		WHERE imm.message_id = ` + m + `.id AND imm.source_id = ` + m + `.source_id
		  AND LOWER(CAST(imm.flags AS TEXT)) LIKE '%"\\draft"%' ESCAPE '!')
		OR EXISTS (SELECT 1 FROM ` + schema + `imap_drafts d WHERE d.current_message_id = ` + m + `.id)
		OR EXISTS (SELECT 1 FROM ` + schema + `imap_drafts d WHERE d.pending_original_message_id = ` + m + `.id)
		OR EXISTS (SELECT 1 FROM ` + schema + `gmail_drafts d WHERE d.current_message_id = ` + m + `.id)
		OR EXISTS (SELECT 1 FROM ` + schema + `gmail_drafts d WHERE d.pending_original_message_id = ` + m + `.id))`
}

// sourceHasSentFolderSQL is true when source src has a Sent folder, so
// provider metadata decides the direction of its mail. Gmail always does.
func sourceHasSentFolderSQL(src string) string {
	return `(` + src + `.source_type = 'gmail' OR EXISTS (SELECT 1 FROM labels sl
		WHERE sl.source_id = ` + src + `.id AND sl.system_role = '` + LabelSystemRoleSent + `'))`
}

// outboundEvidenceExistsSQL is true when message m is mail the account wrote:
// it carries a Sent or Drafts label of its own source, the IMAP \Draft flag,
// or messages.draft_authored, which keeps a draft snapshot written after its
// draft evidence is gone. src must be m's source row.
func outboundEvidenceExistsSQL(m, src string) string {
	return `(` + m + `.draft_authored
		OR EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = ` + m + `.id AND l.source_id = ` + m + `.source_id
		  AND ` + outboundEvidenceLabelSQL("l", src) + `)
		OR ` + draftEvidenceSQL(m, src) + `)`
}

// accountSink returns the mailbox a source delivers into, normalized, or ""
// when the source identifier is not a mailbox.
func accountSink(sourceType, identifier string) string {
	address := strings.TrimSpace(identifier)
	switch sourceType {
	case "imap", "imaps", "imap+starttls":
		address = imapIdentifierUser(address)
	}
	if !isExactMailbox(address) {
		return ""
	}
	return NormalizeIdentifierForCompare(address)
}

// imapIdentifierUser returns the username of an "imaps://user@host:port"
// identifier, or the identifier unchanged when it has no IMAP scheme. The
// username may hold its own "@", and an identifier url.Parse rejects (an
// unescaped "%" in a legacy username) falls back to the text before the
// last "@".
func imapIdentifierUser(identifier string) string {
	scheme, rest, ok := strings.Cut(identifier, "://")
	if !ok {
		return identifier
	}
	switch strings.ToLower(scheme) {
	case "imap", "imaps", "imap+starttls":
	default:
		return identifier
	}
	if u, err := url.Parse(identifier); err == nil {
		if u.User == nil {
			return identifier
		}
		return u.User.Username()
	}
	at := strings.LastIndex(rest, "@")
	if at <= 0 {
		return identifier
	}
	user := rest[:at]
	if unescaped, err := url.PathUnescape(user); err == nil {
		user = unescaped
	}
	return user
}

func isExactMailbox(address string) bool {
	parsed, err := mail.ParseAddress(address)
	return err == nil && parsed.Address == address && strings.Contains(address, "@")
}

// deliveryInput tells the owner where delivery evidence comes from. The zero
// value reuses stored rows, or reads stored MIME when the row is pending.
type deliveryInput struct {
	parsed     *emailattribution.Headers
	malformed  bool
	reloadMIME bool
}

// deliveryFromMIME parses delivery evidence from a full MIME payload.
func deliveryFromMIME(raw []byte) deliveryInput {
	block := headerBlock(raw)
	if block == nil {
		return deliveryInput{parsed: &emailattribution.Headers{}, malformed: true}
	}
	headers, malformed, err := parseDeliveryBlock(block)
	if err != nil {
		return deliveryInput{parsed: &emailattribution.Headers{}, malformed: true}
	}
	return deliveryInput{parsed: &headers, malformed: malformed}
}

// headerBlock returns the outer header block, or nil when it is larger than
// emailattribution.MaxHeaderBytes. It looks only at the bytes the bounded
// repair reader sees, so ingest and repair find the same block.
func headerBlock(raw []byte) []byte {
	prefix := raw[:min(len(raw), emailattribution.MaxHeaderBytes+1)]
	if end := mimeHeaderEnd(prefix); end > 0 {
		return prefix[:end]
	}
	if len(prefix) > emailattribution.MaxHeaderBytes {
		return nil
	}
	return prefix
}

func parseDeliveryBlock(block []byte) (emailattribution.Headers, bool, error) {
	if mimeHeaderEnd(block) == 0 {
		// A header-only payload has no blank line; terminate it for the reader.
		block = append(slices.Clip(block), "\r\n\r\n"...)
	}
	return emailattribution.ParseHeaders(block)
}

// refreshAccountAttributionTx is the one owner of messages.account_address
// and messages.account_path. It derives the pair for one row from stored
// inputs and writes it only when it changed. malformed reports delivery
// evidence that could not be decoded. It never bumps the derived-data
// revision; the cache journal trigger records the change.
func (s *Store) refreshAccountAttributionTx(
	ctx context.Context, tx *loggedTx, id int64, delivery deliveryInput,
) (bool, error) {
	var (
		sourceID               int64
		messageType            string
		oldAddress, oldPath    sql.NullString
		sourceType, identifier string
		syncConfig             sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT m.source_id, COALESCE(m.message_type, ''), m.account_address, m.account_path,
		       src.source_type, COALESCE(src.identifier, ''), src.sync_config
		FROM messages m JOIN sources src ON src.id = m.source_id
		WHERE m.id = ?`, id,
	).Scan(&sourceID, &messageType, &oldAddress, &oldPath, &sourceType, &identifier, &syncConfig)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read account attribution inputs for message %d: %w", id, err)
	}
	if !IsEmailMessageType(messageType) && messageType != "calendar_event" {
		if !oldAddress.Valid && !oldPath.Valid {
			return false, nil
		}
		return false, s.writeAccountAttributionTx(ctx, tx, id, messageType, "", "")
	}
	if !tx.attribution.holds(sourceID) {
		return false, fmt.Errorf("message %d of source %d: %w", id, sourceID, errAttributionLockMissing)
	}
	if s.accountAttributionAfterReadHook != nil {
		s.accountAttributionAfterReadHook(id)
	}
	write := func(address, path string) error {
		if oldPath.Valid && oldPath.String == path && oldAddress.String == address && oldAddress.Valid == (address != "") {
			return nil
		}
		return s.writeAccountAttributionTx(ctx, tx, id, messageType, address, path)
	}

	if messageType == "calendar_event" {
		address, err := s.calendarAccountAddressTx(ctx, tx, sourceID, syncConfig)
		if err != nil {
			return false, err
		}
		return false, write(address, accountPathCalendar)
	}

	var headers emailattribution.Headers
	malformed := false
	switch {
	case delivery.parsed != nil:
		headers, malformed = *delivery.parsed, delivery.malformed
		if err := replaceDeliveryAddressesTx(ctx, tx, id, headers); err != nil {
			return false, err
		}
	case delivery.reloadMIME || !oldPath.Valid:
		headers, malformed, err = s.readDeliveryHeadersTx(ctx, tx, id)
		if err != nil {
			return false, err
		}
		if err := replaceDeliveryAddressesTx(ctx, tx, id, headers); err != nil {
			return false, err
		}
	default:
		headers, err = loadDeliveryAddressesTx(ctx, tx, id)
		if err != nil {
			return false, err
		}
	}

	matches, err := matchMessageIdentitiesWith(ctx, tx, []int64{id})
	if err != nil {
		return malformed, err
	}
	match := matches[id]

	// Drafts take the sent path: the account wrote them, so received: skips
	// them. Draft evidence also sets draft_authored, which keeps the row
	// written after an edit or delete retires the draft and its evidence.
	var sent, draft, authored, hasSentFolder bool
	if err := tx.QueryRowContext(ctx, `
		SELECT `+outboundEvidenceExistsSQL("m", "src")+`, `+draftEvidenceSQL("m", "src")+`, m.draft_authored,
		       `+sourceHasSentFolderSQL("src")+`
		FROM messages m JOIN sources src ON src.id = m.source_id
		WHERE m.id = ?`, id,
	).Scan(&sent, &draft, &authored, &hasSentFolder); err != nil {
		return malformed, fmt.Errorf("read outbound evidence for message %d: %w", id, err)
	}
	if draft && !authored {
		if err := markDraftAuthoredTx(ctx, tx, id); err != nil {
			return malformed, err
		}
	}

	candidates, err := tx.attribution.sourceIdentityKeys(ctx, tx, sourceID)
	if err != nil {
		return malformed, err
	}
	result := emailattribution.Attribute(emailattribution.Evidence{
		Original:     headers.Original,
		Delivered:    headers.Delivered,
		Visible:      match.Visible,
		Sender:       match.Sender,
		NoSentFolder: !hasSentFolder,
	}, candidates, accountSink(sourceType, identifier), sent)
	path := accountPathInbound
	if result.Sent {
		path = accountPathSent
	}
	return malformed, write(result.Address, path)
}

// writeAccountAttributionTx writes the pair. The message_type guard keeps a
// concurrent retype, which clears the pair on its own transaction, from being
// overwritten by a derivation that read the old type.
func (s *Store) writeAccountAttributionTx(
	ctx context.Context, tx *loggedTx, id int64, messageType, address, path string,
) error {
	var addressValue, pathValue any
	if address != "" {
		addressValue = address
	}
	if path != "" {
		pathValue = path
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE messages SET account_address = ?, account_path = ?
		WHERE id = ? AND COALESCE(message_type, '') = ?`,
		addressValue, pathValue, id, messageType,
	); err != nil {
		return fmt.Errorf("write account attribution for message %d: %w", id, err)
	}
	return nil
}

func sourceIdentityKeysTx(ctx context.Context, tx *loggedTx, sourceID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT address_key FROM account_identities WHERE source_id = ? AND address_key <> ''`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("read account identities of source %d: %w", sourceID, err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan account identity: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

type calendarSourceConfig struct {
	AccountEmail string `json:"account_email"`
	CalendarID   string `json:"calendar_id"`
	Primary      bool   `json:"primary"`
}

// calendarMailbox maps a calendar source to the mailbox it belongs to: the
// account email for the primary calendar, or a calendar ID that is itself a
// mailbox. Google group and resource calendars map to "".
func calendarMailbox(syncConfig sql.NullString) string {
	if !syncConfig.Valid || strings.TrimSpace(syncConfig.String) == "" {
		return ""
	}
	var cfg calendarSourceConfig
	if err := json.Unmarshal([]byte(syncConfig.String), &cfg); err != nil {
		return ""
	}
	address := cfg.CalendarID
	if cfg.Primary || cfg.CalendarID == "primary" {
		address = cfg.AccountEmail
	}
	address = strings.TrimSpace(address)
	if !isExactMailbox(address) {
		return ""
	}
	domain := strings.ToLower(address[strings.LastIndex(address, "@")+1:])
	if strings.HasSuffix(domain, ".calendar.google.com") {
		return ""
	}
	return NormalizeIdentifierForCompare(address)
}

func (s *Store) calendarAccountAddressTx(
	ctx context.Context, tx *loggedTx, sourceID int64, syncConfig sql.NullString,
) (string, error) {
	address := calendarMailbox(syncConfig)
	if address == "" {
		return "", nil
	}
	var confirmed int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM account_identities WHERE source_id = ? AND address_key = ?`,
		sourceID, address).Scan(&confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check calendar account of source %d: %w", sourceID, err)
	}
	return address, nil
}

// readDeliveryHeadersTx reads a bounded prefix of the stored MIME and parses
// its delivery headers. Undecodable payloads report malformed with no
// evidence; SQL errors return.
func (s *Store) readDeliveryHeadersTx(
	ctx context.Context, tx *loggedTx, id int64,
) (emailattribution.Headers, bool, error) {
	var prefix []byte
	var compression sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT `+s.dialect.BlobPrefixSQL("raw_data")+`, compression
		FROM message_raw WHERE message_id = ? AND raw_format = 'mime'`,
		accountHeaderRawPrefix, id,
	).Scan(&prefix, &compression)
	if errors.Is(err, sql.ErrNoRows) {
		return emailattribution.Headers{}, false, nil
	}
	if err != nil {
		return emailattribution.Headers{}, false, fmt.Errorf("read raw MIME headers for message %d: %w", id, err)
	}
	headers, malformed := decodeDeliveryHeaders(prefix, compression)
	return headers, malformed, nil
}

// decodeDeliveryHeaders reads delivery evidence from a stored blob prefix.
// A payload that cannot be decoded yields no evidence and reports malformed.
func decodeDeliveryHeaders(prefix []byte, compression sql.NullString) (emailattribution.Headers, bool) {
	block, decodeErr := decodeMessageRawHeaderBounded(prefix, compression, emailattribution.MaxHeaderBytes)
	if decodeErr != nil {
		return emailattribution.Headers{}, true
	}
	headers, malformed, parseErr := parseDeliveryBlock(block)
	if parseErr != nil {
		return emailattribution.Headers{}, true
	}
	return headers, malformed
}

type deliveryAddress struct{ kind, address string }

func deliveryAddressSet(headers emailattribution.Headers) []deliveryAddress {
	var out []deliveryAddress
	for _, a := range headers.Original {
		out = append(out, deliveryAddress{deliveryKindOriginal, a})
	}
	for _, a := range headers.Delivered {
		out = append(out, deliveryAddress{deliveryKindDelivered, a})
	}
	slices.SortFunc(out, func(a, b deliveryAddress) int {
		if c := strings.Compare(a.kind, b.kind); c != 0 {
			return c
		}
		return strings.Compare(a.address, b.address)
	})
	return slices.Compact(out)
}

func loadDeliveryAddressRowsTx(ctx context.Context, tx *loggedTx, id int64) ([]deliveryAddress, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT kind, address FROM message_delivery_addresses WHERE message_id = ? ORDER BY kind, address`, id)
	if err != nil {
		return nil, fmt.Errorf("read delivery addresses for message %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []deliveryAddress
	for rows.Next() {
		var row deliveryAddress
		if err := rows.Scan(&row.kind, &row.address); err != nil {
			return nil, fmt.Errorf("scan delivery address: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func loadDeliveryAddressesTx(ctx context.Context, tx *loggedTx, id int64) (emailattribution.Headers, error) {
	rows, err := loadDeliveryAddressRowsTx(ctx, tx, id)
	if err != nil {
		return emailattribution.Headers{}, err
	}
	var headers emailattribution.Headers
	for _, row := range rows {
		if row.kind == deliveryKindOriginal {
			headers.Original = append(headers.Original, row.address)
		} else {
			headers.Delivered = append(headers.Delivered, row.address)
		}
	}
	return headers, nil
}

func replaceDeliveryAddressesTx(
	ctx context.Context, tx *loggedTx, id int64, headers emailattribution.Headers,
) error {
	want := deliveryAddressSet(headers)
	have, err := loadDeliveryAddressRowsTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if slices.Equal(want, have) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_delivery_addresses WHERE message_id = ?`, id); err != nil {
		return fmt.Errorf("clear delivery addresses for message %d: %w", id, err)
	}
	for _, row := range want {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_delivery_addresses (message_id, kind, address) VALUES (?, ?, ?)`,
			id, row.kind, row.address); err != nil {
			return fmt.Errorf("store delivery address for message %d: %w", id, err)
		}
	}
	return nil
}

// refreshAccountAttributionForMessagesTx refreshes each row once from stored
// inputs.
func (s *Store) refreshAccountAttributionForMessagesTx(ctx context.Context, tx *loggedTx, ids []int64) error {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := s.refreshAccountAttributionTx(ctx, tx, id, deliveryInput{}); err != nil {
			return err
		}
	}
	return nil
}

// refreshCalendarAccountAttributionTx re-derives every calendar event of a
// source after its mapped mailbox changes.
func (s *Store) refreshCalendarAccountAttributionTx(ctx context.Context, tx *loggedTx, sourceID int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM messages WHERE source_id = ? AND message_type = 'calendar_event'`, sourceID)
	if err != nil {
		return fmt.Errorf("list calendar events of source %d: %w", sourceID, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan calendar event of source %d: %w", sourceID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list calendar events of source %d: %w", sourceID, err)
	}
	return s.refreshAccountAttributionForMessagesTx(ctx, tx, ids)
}

// refreshAccountAttributionIfOutboundChangedTx runs a label or membership
// mutation, then refreshes the message when its stored direction no longer
// matches its outbound evidence. Comparing against the stored path, rather
// than evidence before the mutation, also catches flag writes made earlier in
// the transaction. A sent copy from a source without a Sent folder has no
// outbound evidence, so it re-derives to the same answer. Pending rows are
// left to the repair pass.
func (s *Store) refreshAccountAttributionIfOutboundChangedTx(
	ctx context.Context, tx *loggedTx, messageID int64, mutate func() error,
) error {
	if err := mutate(); err != nil {
		return err
	}
	if err := markDraftAuthoredTx(ctx, tx, messageID); err != nil {
		return err
	}
	var path sql.NullString
	var outbound bool
	err := tx.QueryRowContext(ctx, `
		SELECT m.account_path, `+outboundEvidenceExistsSQL("m", "src")+`
		FROM messages m JOIN sources src ON src.id = m.source_id
		WHERE m.id = ?`, messageID).Scan(&path, &outbound)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read outbound evidence for message %d: %w", messageID, err)
	}
	if !path.Valid || (path.String != accountPathInbound && path.String != accountPathSent) ||
		(path.String == accountPathSent) == outbound {
		return nil
	}
	_, err = s.refreshAccountAttributionTx(ctx, tx, messageID, deliveryInput{})
	return err
}

// labelOutboundFlips collects messages of one source whose labels changed
// outbound evidence because a label definition changed. Callers capture the labels a
// write can touch, mutate them, then apply the flips before commit.
type labelOutboundFlips struct {
	ctx      context.Context
	tx       *loggedTx
	sourceID int64
	before   map[int64]string
	// sentFolderBefore records whether the source had a Sent folder before
	// the write; nil until the first capture.
	sentFolderBefore *bool
}

func newLabelOutboundFlips(ctx context.Context, tx *loggedTx, sourceID int64) *labelOutboundFlips {
	return &labelOutboundFlips{ctx: ctx, tx: tx, sourceID: sourceID}
}

// outboundEvidenceByLabelQuery reads each label's evidence as empty (none),
// 'sent' or 'draft'. A Sent label that becomes a Drafts label keeps members
// written but must still record their draft authorship, so it counts as a
// change too.
func outboundEvidenceByLabelQuery(where string) string {
	return `SELECT l.id, CASE
			WHEN l.system_role = '` + LabelSystemRoleDrafts + `' OR (src.source_type = 'gmail' AND l.source_label_id = 'DRAFT') THEN 'draft'
			WHEN COALESCE(` + outboundEvidenceLabelSQL("l", "src") + `, FALSE) THEN 'sent'
			ELSE '' END
		FROM labels l JOIN sources src ON src.id = l.source_id
		WHERE l.source_id = ? AND ` + where
}

// captureLabels records the outbound evidence of every existing label a write to
// these provider IDs or names can update, rename onto or merge. Labels created
// by the write have no members and need no capture.
func (f *labelOutboundFlips) captureLabels(sourceLabelIDs, names []string) error {
	if f == nil || (len(sourceLabelIDs) == 0 && len(names) == 0) {
		return nil
	}
	if f.sentFolderBefore == nil {
		has, err := sourceHasSentFolderTx(f.ctx, f.tx, f.sourceID)
		if err != nil {
			return err
		}
		f.sentFolderBefore = &has
	}
	if f.before == nil {
		f.before = make(map[int64]string)
	}
	keys := append(slices.Clone(sourceLabelIDs), names...)
	for start := 0; start < len(keys); start += 400 {
		chunk := keys[start:min(start+400, len(keys))]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, 1+2*len(chunk))
		args = append(args, f.sourceID)
		for range 2 {
			for _, k := range chunk {
				args = append(args, k)
			}
		}
		rows, err := f.tx.QueryContext(f.ctx, outboundEvidenceByLabelQuery(
			"(l.source_label_id IN ("+placeholders+") OR l.name IN ("+placeholders+"))"), args...)
		if err != nil {
			return fmt.Errorf("read label outbound evidence: %w", err)
		}
		for rows.Next() {
			var id int64
			var sent string
			if err := rows.Scan(&id, &sent); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan label outbound evidence: %w", err)
			}
			if _, seen := f.before[id]; !seen {
				f.before[id] = sent
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("read label outbound evidence: %w", err)
		}
	}
	return nil
}

// applyLabelOutboundFlipsTx refreshes members of every captured label whose outbound
// evidence changed. A label merged away moves its members onto a surviving
// captured label, so a vanished label with different evidence refreshes the
// members of every survivor whose evidence differs from it.
func (s *Store) applyLabelOutboundFlipsTx(f *labelOutboundFlips) error {
	if f == nil {
		return nil
	}
	if err := markPendingIfSentFolderChangedTx(f); err != nil {
		return err
	}
	if len(f.before) == 0 {
		return nil
	}
	ctx, tx := f.ctx, f.tx
	ids := make([]int64, 0, len(f.before))
	for id := range f.before {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	after := make(map[int64]string, len(ids))
	err := queryInChunksContext(ctx, tx, ids, []any{f.sourceID},
		outboundEvidenceByLabelQuery("l.id IN (%s)"), func(rows *loggedRows) error {
			var id int64
			var sent string
			if err := rows.Scan(&id, &sent); err != nil {
				return fmt.Errorf("scan label outbound evidence: %w", err)
			}
			after[id] = sent
			return nil
		})
	if err != nil {
		return fmt.Errorf("read label outbound evidence: %w", err)
	}
	refresh := make(map[int64]struct{})
	for _, id := range ids {
		now, exists := after[id]
		if !exists {
			for survivor, survivorSent := range after {
				if survivorSent != f.before[id] {
					refresh[survivor] = struct{}{}
				}
			}
			continue
		}
		if now != f.before[id] {
			refresh[id] = struct{}{}
		}
	}
	f.before = nil
	if len(refresh) == 0 {
		return nil
	}
	labelIDs := make([]int64, 0, len(refresh))
	for id := range refresh {
		labelIDs = append(labelIDs, id)
	}
	slices.Sort(labelIDs)
	var messageIDs []int64
	err = queryInChunksContext(ctx, tx, labelIDs, []any{f.sourceID}, `
		SELECT m.id FROM messages m
		WHERE m.source_id = ?
		  AND EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id AND ml.label_id IN (%s))`,
		func(rows *loggedRows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("scan label member: %w", err)
			}
			messageIDs = append(messageIDs, id)
			return nil
		})
	if err != nil {
		return fmt.Errorf("read members of changed labels: %w", err)
	}
	return s.refreshAccountAttributionForMessagesTx(ctx, tx, messageIDs)
}

func sourceHasSentFolderTx(ctx context.Context, tx *loggedTx, sourceID int64) (bool, error) {
	var has bool
	err := tx.QueryRowContext(ctx,
		`SELECT `+sourceHasSentFolderSQL("src")+` FROM sources src WHERE src.id = ?`, sourceID).Scan(&has)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read Sent folder of source %d: %w", sourceID, err)
	}
	return has, nil
}

// markPendingIfSentFolderChangedTx returns every email row of the source to
// pending when the write gave the source its first Sent folder or removed its
// last one. Whether a confirmed sender alone marks a sent copy depends on
// that, and the next sync derives the rows again.
func markPendingIfSentFolderChangedTx(f *labelOutboundFlips) error {
	if f.sentFolderBefore == nil {
		return nil
	}
	before := *f.sentFolderBefore
	f.sentFolderBefore = nil
	after, err := sourceHasSentFolderTx(f.ctx, f.tx, f.sourceID)
	if err != nil || after == before {
		return err
	}
	if _, err := f.tx.ExecContext(f.ctx, `
		UPDATE messages SET account_address = NULL, account_path = NULL
		WHERE source_id = ? AND COALESCE(message_type, '') IN ('', 'email') AND account_path IS NOT NULL`,
		f.sourceID); err != nil {
		return fmt.Errorf("mark source %d account attribution pending: %w", f.sourceID, err)
	}
	return nil
}

// markAccountAttributionPendingForAddressesTx returns to pending the rows of
// sourceID whose attribution can depend on the given addresses, and returns
// their IDs. It runs in exclusive transactions, after an identity or the
// source mailbox changed. Marking instead of deriving keeps that transaction
// short; the caller derives the rows after commit with
// deriveAccountAttributionAfterCommit.
func (s *Store) markAccountAttributionPendingForAddressesTx(
	ctx context.Context, tx *loggedTx, sourceID int64, addresses []string,
) ([]int64, error) {
	normalized := make([]string, 0, len(addresses))
	for _, a := range addresses {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			normalized = append(normalized, a)
		}
	}
	slices.Sort(normalized)
	normalized = slices.Compact(normalized)
	if len(normalized) == 0 {
		return nil, nil
	}
	ids := make(map[int64]struct{})
	scan := func(rows *loggedRows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan account attribution target: %w", err)
		}
		ids[id] = struct{}{}
		return nil
	}
	for _, query := range []string{
		`SELECT d.message_id FROM message_delivery_addresses d
		 JOIN messages m ON m.id = d.message_id
		 WHERE m.source_id = ? AND d.address IN (%s)`,
		`SELECT m.id FROM messages m
		 WHERE m.source_id = ? AND m.account_path IS NOT NULL
		   AND EXISTS (
		     SELECT 1 FROM message_recipients mr
		     WHERE mr.message_id = m.id
		       AND mr.recipient_type IN ('from', 'to', 'cc')
		       AND mr.email_address IS NOT NULL
		       AND LOWER(mr.email_address) IN (%s))`,
		`SELECT m.id FROM messages m WHERE m.source_id = ? AND m.account_address IN (%s)`,
	} {
		if err := queryInChunksContext(ctx, tx, normalized, []any{sourceID}, query, scan); err != nil {
			return nil, fmt.Errorf("find messages mentioning changed identities: %w", err)
		}
	}

	confirmations := make([]normalizedIdentityConfirmation, 0, len(normalized))
	for _, a := range normalized {
		confirmations = append(confirmations, normalizedIdentityConfirmation{identifier: a, normalized: a})
	}
	participantIDs, err := participantIDsForConfirmationsContext(ctx, tx, sourceID, confirmations)
	if err != nil {
		return nil, err
	}
	fallback, err := participantFallbackMessageIDsTx(ctx, tx, &sourceID, participantIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range fallback {
		ids[id] = struct{}{}
	}

	var sourceType, identifier string
	var syncConfig sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT source_type, COALESCE(identifier, ''), sync_config FROM sources WHERE id = ?`, sourceID,
	).Scan(&sourceType, &identifier, &syncConfig); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read source %d: %w", sourceID, err)
	}
	if sink := accountSink(sourceType, identifier); sink != "" && slices.Contains(normalized, sink) {
		if err := scanIDsTx(ctx, tx, scan, `
			SELECT id FROM messages
			WHERE source_id = ? AND account_path = 'inbound' AND account_address IS NULL`, sourceID); err != nil {
			return nil, err
		}
	}
	if mailbox := calendarMailbox(syncConfig); mailbox != "" && slices.Contains(normalized, mailbox) {
		if err := scanIDsTx(ctx, tx, scan,
			`SELECT id FROM messages WHERE source_id = ? AND message_type = 'calendar_event'`, sourceID); err != nil {
			return nil, err
		}
	}

	list := slices.Sorted(maps.Keys(ids))
	if err := execInChunksContext(ctx, tx, list, nil,
		`UPDATE messages SET account_address = NULL, account_path = NULL WHERE id IN (%s)`); err != nil {
		return nil, fmt.Errorf("mark account attribution pending for source %d: %w", sourceID, err)
	}
	return list, nil
}

// deriveAccountAttributionAfterCommit derives rows an identity or mailbox
// change returned to pending, a page per transaction. The change has already
// committed, so a failure is logged and the rows stay pending until the
// source's next sync or repair-derived.
func (s *Store) deriveAccountAttributionAfterCommit(ctx context.Context, sourceID int64, ids []int64) {
	if len(ids) == 0 {
		return
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	_, err := s.deriveAccountAttributionPagesContext(ctx, sourceID, nil,
		func(_ *loggedTx, after int64, limit int) ([]int64, error) {
			start, _ := slices.BinarySearch(ids, after+1)
			return ids[start:min(start+limit, len(ids))], nil
		})
	if err != nil {
		slog.Warn("derive account attribution after an identity change failed; the rows stay pending",
			"source_id", sourceID, "messages", len(ids), "error", err)
	}
}

func scanIDsTx(ctx context.Context, tx *loggedTx, scan func(*loggedRows) error, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("find account attribution targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// participantFallbackMessageIDsTx finds email rows whose identity matches
// still read participants: a sender_id with no non-empty From envelope, or a
// To/Cc row with no envelope address. Envelope rows never depend on
// participants. A nil sourceID searches every source.
func participantFallbackMessageIDsTx(
	ctx context.Context, tx *loggedTx, sourceID *int64, participantIDs []int64,
) ([]int64, error) {
	if len(participantIDs) == 0 {
		return nil, nil
	}
	sourceFilter, prefix := "", []any(nil)
	if sourceID != nil {
		sourceFilter, prefix = "m.source_id = ? AND ", []any{*sourceID}
	}
	var out []int64
	scan := func(rows *loggedRows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan participant fallback message: %w", err)
		}
		out = append(out, id)
		return nil
	}
	for _, query := range []string{
		`SELECT m.id FROM messages m
		 WHERE ` + sourceFilter + `COALESCE(m.message_type, '') IN ('', 'email')
		   AND m.sender_id IN (%s)
		   AND NOT EXISTS (
		     SELECT 1 FROM message_recipients mr
		     WHERE mr.message_id = m.id AND mr.recipient_type = 'from'
		       AND mr.email_address IS NOT NULL AND TRIM(mr.email_address) <> '')`,
		// Driven from the participant's recipient rows, so it reads only the
		// messages they appear in rather than every message.
		`SELECT m.id FROM messages m
		 WHERE ` + sourceFilter + `COALESCE(m.message_type, '') IN ('', 'email')
		   AND m.id IN (
		     SELECT mr.message_id FROM message_recipients mr
		     WHERE mr.participant_id IN (%s) AND mr.recipient_type IN ('from', 'to', 'cc')
		       AND (mr.email_address IS NULL OR TRIM(mr.email_address) = ''))`,
	} {
		if err := queryInChunksContext(ctx, tx, participantIDs, prefix, query, scan); err != nil {
			return nil, fmt.Errorf("find participant fallback messages: %w", err)
		}
	}
	return out, nil
}

// refreshAccountAttributionForParticipantsTx re-derives rows whose identity
// matches read these participants. It runs in exclusive transactions.
func (s *Store) refreshAccountAttributionForParticipantsTx(
	ctx context.Context, tx *loggedTx, participantIDs []int64,
) error {
	ids, err := participantFallbackMessageIDsTx(ctx, tx, nil, participantIDs)
	if err != nil {
		return err
	}
	return s.refreshAccountAttributionForMessagesTx(ctx, tx, ids)
}

// AccountAttributionRepairSummary reports one account-attribution repair run.
// Undecodable counts rows whose stored delivery headers could not be read;
// they are still attributed from their envelope.
type AccountAttributionRepairSummary struct {
	Scanned     int64
	Undecodable int64
}

// RepairAccountAttributionContext derives every pending email and calendar
// row of one source, a page per transaction. Committed pages stay derived, so
// a cancelled run resumes where it stopped. SQL errors and cancellation stop
// the run; undecodable payloads are counted and attributed from the envelope.
// A source with nothing pending costs one indexed lookup, so every sync runs
// it.
//
// A change to the attribution rules that changes output must clear
// account_path for the affected rows, or derived rows keep the old answer.
func (s *Store) RepairAccountAttributionContext(
	ctx context.Context, sourceID int64, progress func(AccountAttributionRepairSummary),
) (AccountAttributionRepairSummary, error) {
	var present int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM sources WHERE id = ?`, sourceID).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountAttributionRepairSummary{}, nil
	}
	if err != nil {
		return AccountAttributionRepairSummary{}, fmt.Errorf("read source %d: %w", sourceID, err)
	}
	return s.deriveAccountAttributionPagesContext(ctx, sourceID, progress,
		func(tx *loggedTx, after int64, limit int) ([]int64, error) {
			var ids []int64
			err := scanIDsTx(ctx, tx, func(rows *loggedRows) error {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
				return nil
			}, accountPendingPageQuery, sourceID, after, limit)
			return ids, err
		})
}

// deriveAccountAttributionPagesContext derives rows of sourceID one page at a
// time, each page in its own shared attribution transaction. page returns up
// to limit IDs greater than after, in ascending order; an empty page ends the
// run.
func (s *Store) deriveAccountAttributionPagesContext(
	ctx context.Context, sourceID int64, progress func(AccountAttributionRepairSummary),
	page func(tx *loggedTx, after int64, limit int) ([]int64, error),
) (AccountAttributionRepairSummary, error) {
	limit := accountRepairPageSize
	if s.accountRepairPageSizeOverride > 0 {
		limit = s.accountRepairPageSizeOverride
	}
	var summary AccountAttributionRepairSummary
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		var pageSummary AccountAttributionRepairSummary
		var last int64
		err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
			pageSummary, last = AccountAttributionRepairSummary{}, 0
			ids, err := page(tx, after, limit)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := ctx.Err(); err != nil {
					return err
				}
				malformed, err := s.refreshAccountAttributionTx(ctx, tx, id, deliveryInput{})
				if err != nil {
					return err
				}
				pageSummary.Scanned++
				if malformed {
					pageSummary.Undecodable++
				}
				last = id
			}
			return nil
		})
		if err != nil {
			return summary, fmt.Errorf("derive account attribution for source %d: %w", sourceID, err)
		}
		if pageSummary.Scanned == 0 {
			return summary, nil
		}
		summary.Scanned += pageSummary.Scanned
		summary.Undecodable += pageSummary.Undecodable
		after = last
		if progress != nil {
			progress(summary)
		}
	}
}

// markDraftAuthoredTx records draft authorship when the message has draft
// evidence now. It runs after any label or membership change, including ones
// that leave the message's direction unchanged, so the bit is set before a
// later change can retire the draft and its evidence.
func markDraftAuthoredTx(ctx context.Context, tx *loggedTx, messageID int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE messages SET draft_authored = TRUE
		WHERE id = ? AND draft_authored = FALSE
		  AND EXISTS (SELECT 1 FROM sources src WHERE src.id = messages.source_id
		              AND `+draftEvidenceSQL("messages", "src")+`)`, messageID); err != nil {
		return fmt.Errorf("record draft authorship of message %d: %w", messageID, err)
	}
	return nil
}
