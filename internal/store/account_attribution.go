package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/emailattribution"
)

const accountAttributionRuleVersion = 1

type AccountAttributionProgress struct {
	SourceID      int64 `json:"source_id"`
	LastMessageID int64 `json:"last_message_id"`
	HighWaterID   int64 `json:"high_water_id"`
	Completed     bool  `json:"completed"`
	Scanned       int64 `json:"scanned"`
}

func (s *Store) ensureAccountAttributionSchema(ctx context.Context) error {
	return s.runOnceMigration(ctx, "account_attribution_schema_v1", 1, false, func(ctx context.Context) error {
		// Index builds can exceed the PostgreSQL pool timeout on existing archives.
		return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			viewPrefix := "CREATE VIEW IF NOT EXISTS"
			if s.IsPostgreSQL() {
				viewPrefix = "CREATE OR REPLACE VIEW"
			}
			for _, stmt := range []string{
				`CREATE INDEX IF NOT EXISTS idx_messages_account_address ON messages(account_address, sent_at, id)`,
				`CREATE INDEX IF NOT EXISTS idx_messages_source_account ON messages(source_id, account_address, sent_at, id)`,
				`CREATE INDEX IF NOT EXISTS idx_messages_account_pending ON messages(source_id,id) WHERE account_attribution_basis='not-derived'`,
				viewPrefix + ` account_identity_group_memberships AS SELECT ai.source_id, 'fastmail-masked:' || LOWER(src.identifier) AS group_key, ai.address_key FROM account_identities ai JOIN sources src ON src.id = ai.source_id WHERE (',' || ai.source_signal || ',') LIKE '%,fastmail-masked-email,%'`,
			} {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("install account attribution: %w", err)
				}
			}
			return nil
		})
	})
}

func (s *Store) GetAccountAttributionContext(ctx context.Context, id int64) (emailattribution.Result, error) {
	var address, path sql.NullString
	var basis string
	err := s.db.QueryRowContext(ctx, `SELECT account_address, account_path, account_attribution_basis FROM messages WHERE id = ?`, id).Scan(&address, &path, &basis)
	return emailattribution.Result{Address: address.String, Path: path.String, Basis: basis}, err
}

// refreshAccountAttributionWith uses compact evidence once available. It never
// reads message bodies, and identity updates need no MIME decompression.
func (s *Store) refreshAccountAttributionWith(ctx context.Context, tx *loggedTx, id int64, raw []byte) error {
	var sourceID int64
	var sourceType, identifier string
	var messageType sql.NullString
	var metadata, config, oldAddress, oldPath sql.NullString
	var oldBasis string
	var sent bool
	lockClause := ""
	if s.IsPostgreSQL() {
		lockClause = " FOR UPDATE OF m"
	}
	err := tx.QueryRowContext(ctx, `SELECT m.source_id, m.message_type, src.source_type, src.identifier, m.metadata, src.sync_config,
  EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=m.id AND (l.system_role='sent' OR (src.source_type='gmail' AND l.source_label_id='SENT'))),
  m.account_address, m.account_path, m.account_attribution_basis
  FROM messages m JOIN sources src ON src.id=m.source_id WHERE m.id=?`+lockClause, id).Scan(&sourceID, &messageType, &sourceType, &identifier, &metadata, &config, &sent, &oldAddress, &oldPath, &oldBasis)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read account attribution input: %w", err)
	}
	typ := messageType.String
	if typ == "" {
		typ = MessageTypeEmail
	}
	if typ != MessageTypeEmail && typ != "calendar_event" && oldBasis == "not-derived" && !oldAddress.Valid && !oldPath.Valid {
		return nil
	}
	result := emailattribution.Result{Basis: "not-applicable"}
	var mentions []string
	switch typ {
	case MessageTypeEmail:
		var evidence emailattribution.Evidence
		var encoded string
		if raw == nil {
			err = tx.QueryRowContext(ctx, `SELECT evidence FROM message_account_evidence WHERE message_id=?`, id).Scan(&encoded)
			if errors.Is(err, sql.ErrNoRows) {
				var compressed []byte
				var compression sql.NullString
				rawErr := tx.QueryRowContext(ctx, `SELECT raw_data,compression FROM message_raw WHERE message_id=? AND raw_format='mime'`, id).Scan(&compressed, &compression)
				if rawErr == nil {
					raw, err = decodeMessageRaw(compressed, compression)
					if err != nil {
						return err
					}
				} else if !errors.Is(rawErr, sql.ErrNoRows) {
					return rawErr
				}
			} else if err != nil {
				return err
			}
		}
		if raw != nil {
			evidence = emailattribution.Parse(raw)
		} else if encoded != "" {
			if err = json.Unmarshal([]byte(encoded), &evidence); err != nil {
				return fmt.Errorf("decode account header evidence: %w", err)
			}
		} else {
			// Archives lacking raw MIME can still use immutable envelope snapshots.
			rows, e := tx.QueryContext(ctx, `SELECT email_address,recipient_type FROM message_recipients WHERE message_id=? AND email_address IS NOT NULL`, id)
			if e != nil {
				return e
			}
			for rows.Next() {
				var address, kind string
				if e = rows.Scan(&address, &kind); e != nil {
					_ = rows.Close()
					return e
				}
				address = NormalizeIdentifierForCompare(address)
				switch kind {
				case meetingSenderRole:
					evidence.From = append(evidence.From, address)
				case "to", "cc":
					evidence.Visible = append(evidence.Visible, address)
				case "bcc":
					evidence.Diagnostic = append(evidence.Diagnostic, address)
				}
			}
			e = rows.Err()
			_ = rows.Close()
			if e != nil {
				return e
			}
		}
		mentions = evidence.Mentions()
		encodedBytes, e := json.Marshal(evidence, json.Deterministic(true))
		if e != nil {
			return e
		}
		// Conditional upsert makes replay a true no-op for evidence and mentions.
		newEncoded := string(encodedBytes)
		var current string
		readErr := tx.QueryRowContext(ctx, `SELECT evidence FROM message_account_evidence WHERE message_id=?`, id).Scan(&current)
		if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
			return readErr
		}
		if current != newEncoded || errors.Is(readErr, sql.ErrNoRows) {
			if _, e = tx.ExecContext(ctx, `INSERT INTO message_account_evidence(message_id,evidence) VALUES (?,?) ON CONFLICT(message_id) DO UPDATE SET evidence=excluded.evidence`, id, newEncoded); e != nil {
				return e
			}
		}
		inbox, e := accountInboxAddress(sourceType, identifier, config)
		if e != nil {
			return e
		}
		candidates, e := s.accountCandidatesWith(ctx, tx, sourceID, append(mentions, inbox))
		if e != nil {
			return e
		}
		result = emailattribution.Attribute(evidence, candidates, inbox, sent)
		if !sent && (result.Basis == "source-default" || result.Basis == "missing-evidence" || result.Basis == "malformed-evidence") && inbox != "" {
			mentions = append(mentions, inbox)
		}
	case "calendar_event":
		var sc struct {
			CalendarID     string `json:"calendar_id"`
			AccountEmail   string `json:"account_email"`
			AccountAddress string `json:"account_address"`
		}
		if config.Valid && config.String != "" {
			if e := json.Unmarshal([]byte(config.String), &sc); e != nil {
				return fmt.Errorf("decode calendar account mapping: %w", e)
			}
		}
		if sc.CalendarID == "" && metadata.Valid {
			if e := json.Unmarshal([]byte(metadata.String), &sc); e != nil {
				return e
			}
		}
		address := sc.AccountAddress
		if address == "" {
			address = sc.CalendarID
			if address == "primary" {
				address = sc.AccountEmail
			}
		}
		result.Path = "calendar"
		result.Basis = "unmapped-calendar"
		parsed, parseErr := mail.ParseAddress(address)
		if parseErr == nil && parsed.Address == address && strings.Contains(address, "@") && (sc.AccountAddress != "" || !strings.HasSuffix(strings.ToLower(address), "@group.calendar.google.com")) {
			mentions = []string{NormalizeIdentifierForCompare(address)}
			candidates, e := s.accountCandidatesWith(ctx, tx, sourceID, []string{NormalizeIdentifierForCompare(address)})
			if e != nil {
				return e
			}
			result.Basis = "unconfirmed-calendar"
			if len(candidates) == 1 {
				result.Address = candidates[0]
				result.Basis = "calendar"
			}
		}
	}
	if err := s.replaceAccountMentionsWith(ctx, tx, sourceID, id, mentions); err != nil {
		return err
	}

	if oldAddress.String == result.Address && oldPath.String == result.Path && oldBasis == result.Basis {
		return nil
	}
	var address, path any
	if result.Address != "" {
		address = result.Address
	}
	if result.Path != "" {
		path = result.Path
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET account_address=?, account_path=?, account_attribution_basis=? WHERE id=?`, address, path, result.Basis, id); err != nil {
		return err
	}
	if !s.IsPostgreSQL() {
		if _, err = tx.ExecContext(ctx, `INSERT INTO cache_related_change_journal(dataset,message_id) VALUES ('message_facts',?)`, id); err != nil {
			return err
		}
	}
	// The message-scoped journal lets append-only syncs carry facts above the
	// published boundary. Changes to exported messages still force a refresh.
	return s.bumpDerivedDataRevision(tx, true)
}

// accountInboxAddress resolves a mailbox sink without treating connection URLs
// or display names as addresses. Candidate lookup still requires confirmation.
func accountInboxAddress(sourceType, identifier string, config sql.NullString) (string, error) {
	address := identifier
	if sourceType == "imap" {
		var cfg struct {
			Username string `json:"username"`
		}
		if config.Valid && config.String != "" {
			if err := json.Unmarshal([]byte(config.String), &cfg); err != nil {
				return "", fmt.Errorf("decode IMAP account username: %w", err)
			}
		}
		if cfg.Username != "" {
			address = cfg.Username
		} else if u, err := url.Parse(identifier); err == nil && u.Host != "" && u.User != nil {
			switch strings.ToLower(u.Scheme) {
			case "imap", "imaps", "imap+starttls":
				address = u.User.Username()
			}
		}
	}
	address = strings.TrimSpace(address)
	if parsed, err := mail.ParseAddress(address); err == nil && parsed.Address == address && strings.Contains(address, "@") {
		return NormalizeIdentifierForCompare(address), nil
	}
	return "", nil
}

func (s *Store) replaceAccountMentionsWith(ctx context.Context, tx *loggedTx, sourceID, id int64, mentions []string) error {
	mentions = append([]string(nil), mentions...)
	slices.Sort(mentions)
	mentions = slices.Compact(mentions)
	rows, err := tx.QueryContext(ctx, `SELECT address_key,source_id FROM message_account_mentions WHERE message_id=? ORDER BY address_key`, id)
	if err != nil {
		return err
	}
	var current []string
	sourceMatches := true
	for rows.Next() {
		var a string
		var existingSource int64
		if err = rows.Scan(&a, &existingSource); err != nil {
			_ = rows.Close()
			return err
		}
		sourceMatches = sourceMatches && existingSource == sourceID
		current = append(current, a)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if sourceMatches && slices.Equal(current, mentions) {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM message_account_mentions WHERE message_id=?`, id); err != nil {
		return err
	}
	for _, a := range mentions {
		if _, err = tx.ExecContext(ctx, `INSERT INTO message_account_mentions(source_id,message_id,address_key) VALUES (?,?,?)`, sourceID, id, a); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) accountCandidatesWith(ctx context.Context, tx *loggedTx, sourceID int64, mentions []string) ([]string, error) {
	var result []string
	err := queryInChunksContext(ctx, tx, mentions, []any{sourceID}, `SELECT address_key FROM account_identities WHERE source_id=? AND address_key<>'' AND address_key IN (%s)`, func(rows *loggedRows) error {
		var address string
		if err := rows.Scan(&address); err != nil {
			return err
		}
		result = append(result, address)
		return nil
	})
	return result, err
}

func (s *Store) RefreshAccountAttributionContext(ctx context.Context, id int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		err := s.refreshAccountAttributionWith(ctx, tx, id, nil)
		return err
	})
}

// RecomputeAccountAttributionForIdentitiesContext visits only indexed mention
// hits. New identities cannot cause a scan of every message in their source.
func (s *Store) RecomputeAccountAttributionForIdentitiesContext(ctx context.Context, sourceID int64, addresses []string) (int64, error) {
	var scanned int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		return s.recomputeAccountIdentitiesWith(ctx, tx, sourceID, addresses, &scanned)
	})
	return scanned, err
}

func (s *Store) recomputeAccountIdentitiesWith(ctx context.Context, tx *loggedTx, sourceID int64, addresses []string, scanned *int64) error {
	unique := make(map[int64]bool)
	normalized := make([]string, 0, len(addresses))
	for _, a := range addresses {
		normalized = append(normalized, NormalizeIdentifierForCompare(a))
	}
	err := queryInChunksContext(ctx, tx, normalized, []any{sourceID}, `SELECT message_id FROM message_account_mentions WHERE source_id=? AND address_key IN (%s)`, func(rows *loggedRows) error {
		var id int64
		if e := rows.Scan(&id); e != nil {
			return e
		}
		unique[id] = true
		return nil
	})
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err = s.refreshAccountAttributionWith(ctx, tx, id, nil); err != nil {
			return err
		}
		if scanned != nil {
			*scanned++
		}
	}
	return nil
}

// BackfillAccountAttributionContext persists a fixed high-water cursor with each
// bounded page. Checkpoints happen after commit; replay is safe after a crash.
func (s *Store) BackfillAccountAttributionContext(ctx context.Context, sourceID int64, batchSize int, checkpoint func(AccountAttributionProgress) error) (AccountAttributionProgress, error) {
	p := AccountAttributionProgress{SourceID: sourceID}
	if batchSize < 1 || batchSize > 500 {
		return p, errors.New("account repair page size must be between 1 and 500")
	}
	if err := ctx.Err(); err != nil {
		return p, err
	}
	for {
		committed := p
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			if e := s.lockIdentityMutationTxContext(ctx, tx); e != nil {
				return e
			}
			var version, completed int
			initialPage := false
			e := tx.QueryRowContext(ctx, `SELECT last_message_id,high_water_id,rule_version,completed FROM account_attribution_repair_progress WHERE source_id=?`+s.dialect.SelectForUpdate(), sourceID).Scan(&p.LastMessageID, &p.HighWaterID, &version, &completed)
			if errors.Is(e, sql.ErrNoRows) || (e == nil && version != accountAttributionRuleVersion) {
				initialPage = true
				p.LastMessageID = 0
				if e = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM messages WHERE source_id=?`, sourceID).Scan(&p.HighWaterID); e != nil {
					return e
				}
				if _, e = tx.ExecContext(ctx, `INSERT INTO account_attribution_repair_progress(source_id,last_message_id,high_water_id,rule_version,completed) VALUES (?,0,?,?,0) ON CONFLICT(source_id) DO UPDATE SET last_message_id=0, high_water_id=excluded.high_water_id,rule_version=excluded.rule_version,completed=0`, sourceID, p.HighWaterID, accountAttributionRuleVersion); e != nil {
					return e
				}
				completed = 0
			} else if e != nil {
				return e
			}
			if completed != 0 {
				p.Completed = true
				return nil
			}
			// The first page has no lower bound: zero and MinInt64 are legal IDs.
			// Ledger creation commits with that page, so resume always has a cursor.
			stmt := `SELECT id FROM messages WHERE source_id=? AND id<=? AND (message_type IN ('email','calendar_event') OR message_type IS NULL OR message_type='')`
			args := []any{sourceID, p.HighWaterID}
			if !initialPage {
				stmt += ` AND id>?`
				args = append(args, p.LastMessageID)
			}
			stmt += ` ORDER BY id LIMIT ?`
			args = append(args, batchSize)
			rows, e := tx.QueryContext(ctx, stmt, args...)
			if e != nil {
				return e
			}
			var ids []int64
			for rows.Next() {
				var id int64
				if e = rows.Scan(&id); e != nil {
					_ = rows.Close()
					return e
				}
				ids = append(ids, id)
			}
			e = rows.Err()
			_ = rows.Close()
			if e != nil {
				return e
			}
			for _, id := range ids {
				if e = s.refreshAccountAttributionWith(ctx, tx, id, nil); e != nil {
					return e
				}
			}
			p.Scanned += int64(len(ids))
			if len(ids) > 0 {
				p.LastMessageID = ids[len(ids)-1]
			} else {
				p.Completed = true
			}
			done := 0
			if p.Completed {
				done = 1
			}
			_, e = tx.ExecContext(ctx, `UPDATE account_attribution_repair_progress SET last_message_id=?,completed=? WHERE source_id=?`, p.LastMessageID, done, sourceID)
			return e
		})
		if err != nil {
			return committed, err
		}
		if checkpoint != nil {
			if err = checkpoint(p); err != nil {
				return p, fmt.Errorf("checkpoint account attribution: %w", err)
			}
		}
		if p.Completed {
			return p, nil
		}
	}
}
