package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
)

// DeliveryPolicy is an application gate for an explicit send. It grants no
// provider credentials, confirmation exemption, or permission to edit policies.
type DeliveryPolicy string

const (
	DeliveryDraftOnly   DeliveryPolicy = "draft_only"
	DeliverySendAllowed DeliveryPolicy = "send_allowed"
)

// DeliveryPersonAllRoutes acknowledges approval of the person-wide default.
const DeliveryPersonAllRoutes = "person_all_routes"

var (
	ErrDeliveryPolicyInvalid          = errors.New("invalid delivery policy request")
	ErrDeliveryPolicyRevisionConflict = errors.New("delivery policy revision conflict")
	ErrDeliveryPersonUnknown          = errors.New("unknown or retired canonical person UID")
)

// DeliveryPolicyQuery names a person, or one of the person's saved email
// addresses. An empty Endpoint is the person-wide default. An address is the
// value the contact writer stores (trimmed, lower case); it must be one mailbox.
type DeliveryPolicyQuery struct {
	PersonUID string `json:"person_uid"`
	Endpoint  string `json:"endpoint,omitempty"`
}

// DeliveryPolicyState holds the stored and effective delivery policies for a person or email address.
type DeliveryPolicyState struct {
	PersonUID         string          `json:"person_uid"`
	PersonID          int64           `json:"-"` // internal; the wire names the person by PersonUID
	Revision          int64           `json:"revision"`
	StoredPolicy      *DeliveryPolicy `json:"stored_policy,omitempty"`
	EffectivePolicy   DeliveryPolicy  `json:"effective_policy"`
	InheritanceSource string          `json:"inheritance_source"`
	Reason            string          `json:"reason"`
	Endpoint          string          `json:"endpoint,omitempty"`

	points          []int64 // the person's saved declared points for Endpoint, for the writer
	approvedThrough sql.NullInt64
}

type DeliveryPolicyWrite struct {
	PersonUID            string         `json:"person_uid"`
	Endpoint             string         `json:"endpoint,omitempty"`
	ExpectedRevision     int64          `json:"expected_revision"`
	Policy               DeliveryPolicy `json:"policy,omitempty"`
	ScopeAcknowledgement string         `json:"scope_acknowledgement,omitempty"`
	Actor                string         `json:"-"` // supplied by the trusted caller, never a wire field
	Reason               string         `json:"reason"`
}
type DeliveryPolicyAudit struct {
	Actor        string          `json:"actor"`
	Reason       string          `json:"reason"`
	CreatedAt    time.Time       `json:"created_at"`
	Scope        string          `json:"scope"`
	BeforePolicy *DeliveryPolicy `json:"before_policy,omitempty"`
	AfterPolicy  *DeliveryPolicy `json:"after_policy,omitempty"`
	// ContactPointID is the saved address row an approval binds to.
	ContactPointID *int64 `json:"contact_point_id,omitempty"`
}

// DeliveryPolicyReceipt omits Audit when the write changed nothing.
type DeliveryPolicyReceipt struct {
	Before DeliveryPolicyState  `json:"before"`
	After  DeliveryPolicyState  `json:"after"`
	Audit  *DeliveryPolicyAudit `json:"audit,omitempty"`
}

func deliveryStringValid(v string) bool {
	return v != "" && len(v) <= 1024 && strings.TrimSpace(v) == v && !strings.ContainsFunc(v, unicode.IsControl)
}

// deliveryInvalidError is a rejected request with a fixed cause, which
// DeliveryErrorCode reports even through wrapping.
type deliveryInvalidError struct{ reason string }

func (e *deliveryInvalidError) Error() string {
	return e.reason + ": " + ErrDeliveryPolicyInvalid.Error()
}
func (e *deliveryInvalidError) Unwrap() error { return ErrDeliveryPolicyInvalid }

func deliveryInvalid(reason string) error { return &deliveryInvalidError{reason: reason} }

// parseDeliveryMailbox returns the lower-case mailbox net/mail resolves s to.
func parseDeliveryMailbox(s string) (string, bool) {
	address, ok := parseMailboxAddress(s)
	return strings.ToLower(address), ok
}

// deliveryMailbox is the key the contact writer stores for an address, or ""
// when the endpoint is not exactly one bare mailbox. net/mail only validates.
func deliveryMailbox(endpoint string) string {
	if !deliveryStringValid(endpoint) {
		return ""
	}
	key, err := NormalizeServiceValue(nil, ContactAddressEmail, endpoint)
	if err != nil {
		return ""
	}
	if parsed, ok := parseDeliveryMailbox(key); !ok || parsed != key {
		return ""
	}
	return key
}

// resolveDeliveryQuery validates a query and replaces its endpoint with the
// canonical mailbox key.
func resolveDeliveryQuery(q DeliveryPolicyQuery) (DeliveryPolicyQuery, error) {
	if !deliveryStringValid(q.PersonUID) {
		return q, ErrDeliveryPolicyInvalid
	}
	if q.Endpoint != "" {
		if q.Endpoint = deliveryMailbox(q.Endpoint); q.Endpoint == "" {
			return q, ErrDeliveryPolicyInvalid
		}
	}
	return q, nil
}

func (s *Store) GetDeliveryPolicyContext(ctx context.Context, q DeliveryPolicyQuery) (*DeliveryPolicyState, error) {
	q, err := resolveDeliveryQuery(q)
	if err != nil {
		return nil, err
	}
	var state *DeliveryPolicyState
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		state, _, err = s.deliveryStateTx(ctx, tx, q, false, false, nil)
		return err
	})
	return state, err
}

// deliveryStateTx also returns the reason an exact address cannot be approved
// right now, empty when it can. A non-nil cache lets one snapshot share holder
// scans across recipients.
func (s *Store) deliveryStateTx(ctx context.Context, tx *loggedTx, q DeliveryPolicyQuery, lock, approving bool, cache *deliveryHolderCache) (*DeliveryPolicyState, string, error) {
	if cache == nil {
		cache = newDeliveryHolderCache()
	}
	state := &DeliveryPolicyState{PersonUID: q.PersonUID, Endpoint: q.Endpoint, EffectivePolicy: DeliveryDraftOnly, InheritanceSource: "system_default", Reason: "draft_required"}
	query := `SELECT id, revision + delivery_revision FROM persons WHERE vcard_uid=?`
	if lock {
		query += s.dialect.SelectForUpdate()
	}
	err := tx.QueryRowContext(ctx, query, q.PersonUID).Scan(&state.PersonID, &state.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrDeliveryPersonUnknown
	}
	if err != nil {
		return nil, "", err
	}
	mailbox := q.Endpoint
	var override, stored DeliveryPolicy
	var hasOverride, hasDefault bool
	var boundPoint sql.NullInt64
	rows, err := tx.QueryContext(ctx, `SELECT mailbox, policy, contact_point_id, approved_through_contact_point_id FROM delivery_policies WHERE person_id=? AND mailbox IN ('', ?)`, state.PersonID, mailbox)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var key string
		var policy DeliveryPolicy
		var point, approvedThrough sql.NullInt64
		if err := rows.Scan(&key, &policy, &point, &approvedThrough); err != nil {
			_ = rows.Close()
			return nil, "", err
		}
		if key == "" {
			stored, hasDefault = policy, true
			state.approvedThrough = approvedThrough
		} else {
			override, hasOverride, boundPoint = policy, true, point
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, "", err
	}
	switch {
	case q.Endpoint == "" && hasDefault:
		state.StoredPolicy, state.EffectivePolicy, state.InheritanceSource = new(stored), stored, "person_default"
	case hasOverride:
		state.StoredPolicy, state.EffectivePolicy, state.InheritanceSource = new(override), override, "contact_method"
	case q.Endpoint != "" && hasDefault:
		state.EffectivePolicy, state.InheritanceSource = stored, "person_default"
	}
	if q.Endpoint == "" {
		if state.EffectivePolicy == DeliverySendAllowed {
			state.Reason = "explicit_person_default"
		}
		return state, "", nil
	}
	// Contact points only matter to an approval or to an allowing policy: a
	// restricting write or a read that already resolves to draft_only reports the
	// same state without them.
	if !approving && state.EffectivePolicy == DeliveryDraftOnly {
		return state, "", nil
	}
	own, holders, err := s.deliveryEmailPointsTx(ctx, tx, state.PersonID, mailbox, cache)
	if err != nil {
		return nil, "", err
	}
	state.points = own
	unavailable := ""
	switch {
	case len(own) == 0:
		unavailable = "detached_endpoint"
	case holders > 1:
		unavailable = "ambiguous_endpoint"
	}
	// An override approval stays on the saved address row it was granted on.
	bindingChanged := hasOverride && override == DeliverySendAllowed && unavailable == "" &&
		(!boundPoint.Valid || !slices.Contains(own, boundPoint.Int64))
	covered := false
	if state.EffectivePolicy == DeliverySendAllowed && state.InheritanceSource == "person_default" && unavailable == "" {
		covered, err = s.deliveryApprovalCoversPointsTx(ctx, tx, q.PersonUID, state.approvedThrough, own)
		if err != nil {
			return nil, "", err
		}
	}
	if state.EffectivePolicy == DeliverySendAllowed {
		switch {
		case unavailable != "":
			state.EffectivePolicy, state.Reason = DeliveryDraftOnly, unavailable
		case bindingChanged:
			state.EffectivePolicy, state.Reason = DeliveryDraftOnly, "binding_changed"
		case state.InheritanceSource == "person_default" && !covered:
			state.EffectivePolicy, state.Reason = DeliveryDraftOnly, "approval_predates_address"
		case state.InheritanceSource == "person_default":
			state.Reason = "explicit_person_default"
		default:
			state.Reason = "explicit_approval"
		}
	}
	return state, unavailable, nil
}

func (s *Store) deliveryApprovalCoversPointsTx(ctx context.Context, tx *loggedTx, personUID string, approvedThrough sql.NullInt64, pointIDs []int64) (bool, error) {
	if !approvedThrough.Valid {
		return false, nil
	}
	for _, pointID := range pointIDs {
		if pointID > approvedThrough.Int64 {
			continue
		}
		var covered bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM person_contact_points,
		(SELECT created_at FROM delivery_policy_audit WHERE person_uid=? AND scope=? ORDER BY revision DESC LIMIT 1) approval
		WHERE id=? AND (active_from IS NULL OR `+s.deliveryActiveFromComparison("approval.created_at")+`))`,
			personUID, DeliveryPersonAllRoutes, pointID).Scan(&covered)
		if err != nil || covered {
			return covered, err
		}
	}
	return false, nil
}

// deliveryHolder is one active email contact point whose value resolves to a
// mailbox, under any spelling net/mail accepts.
type deliveryHolder struct {
	id, owner int64
	plain     bool // saved as the bare mailbox, not a display-name or quoted spelling
	declared  bool
}

// deliveryHolderCache shares holder scans across the recipients of one read
// snapshot (or one write); it must not outlive that transaction.
type deliveryHolderCache struct {
	plain   map[string][]deliveryHolder            // by mailbox
	spelled map[string]map[string][]deliveryHolder // by domain, then mailbox
}

func newDeliveryHolderCache() *deliveryHolderCache {
	return &deliveryHolderCache{plain: map[string][]deliveryHolder{}, spelled: map[string]map[string][]deliveryHolder{}}
}

// deliveryActiveEmailSQL and deliverySpelledSQL (which includes a literal tab,
// the one folding whitespace net/mail accepts besides space) must match the partial index
// idx_person_contact_points_email_spelling in both schemas, so the database can
// prove the index covers the query.
// Non-ASCII values need Go case folding because SQL LOWER varies by backend.
const (
	deliveryActiveEmailSQL = `address_kind='email' AND active_until IS NULL AND superseded_at IS NULL`
	deliverySpelledSQL     = `(normalized_value LIKE '% %' OR normalized_value LIKE '%	%' OR normalized_value LIKE '%<%' OR normalized_value LIKE '%>%' OR normalized_value LIKE '%"%' OR normalized_value LIKE '%(%' OR normalized_value LIKE '%)%' OR normalized_value LIKE '%:%' OR normalized_value LIKE '%;%' OR normalized_value LIKE '%,%' OR normalized_value <> LOWER(normalized_value) OR LENGTH(normalized_value) <> OCTET_LENGTH(normalized_value))`
)

// deliveryEmailPointsTx reads the active email contact points that hold the
// mailbox. The person's own declared points are those saved in plain form. Any
// person also holds it under any spelling net/mail resolves to it (brackets,
// quotes, groups); net/mail copies the domain verbatim, so a value containing
// the domain is the complete candidate set for those.
func (s *Store) deliveryEmailPointsTx(ctx context.Context, tx *loggedTx, personID int64, mailbox string, cache *deliveryHolderCache) ([]int64, int, error) {
	domain := mailbox[strings.LastIndex(mailbox, "@")+1:]
	plain, ok := cache.plain[mailbox]
	if !ok {
		var err error
		if plain, err = s.deliveryPlainHoldersTx(ctx, tx, mailbox); err != nil {
			return nil, 0, err
		}
		cache.plain[mailbox] = plain
	}
	byMailbox, ok := cache.spelled[domain]
	if !ok {
		var err error
		if byMailbox, err = s.deliverySpelledHoldersTx(ctx, tx, domain); err != nil {
			return nil, 0, err
		}
		cache.spelled[domain] = byMailbox
	}
	var own []int64
	people := map[int64]bool{}
	seen := map[int64]bool{}
	for _, h := range slices.Concat(plain, byMailbox[mailbox]) {
		// A literal such as an IPv6 domain is both plain and spelled-shaped.
		if seen[h.id] {
			continue
		}
		seen[h.id] = true
		people[h.owner] = true
		if h.plain && h.owner == personID && h.declared {
			own = append(own, h.id)
		}
	}
	slices.Sort(own)
	return own, len(people), nil
}

func (s *Store) deliveryActiveFromComparison(at string) string {
	if s.IsPostgreSQL() {
		return "active_from <= " + at
	}
	return "julianday(active_from) <= julianday(" + at + ")"
}

// deliveryPlainHoldersTx finds the points saved exactly as the mailbox. A plain
// lower-case value parses to itself, so it can only hold a mailbox it equals,
// and the equality is served by idx_person_contact_points_current_lookup.
func (s *Store) deliveryPlainHoldersTx(ctx context.Context, tx *loggedTx, mailbox string) ([]deliveryHolder, error) {
	holders, err := s.deliveryHoldersTx(ctx, tx, `SELECT id, person_id, source, normalized_value, service_id IS NULL AND scope_kind IS NULL AND scope_value IS NULL FROM person_contact_points
		WHERE `+deliveryActiveEmailSQL+` AND (active_from IS NULL OR `+s.deliveryActiveFromComparison("?")+`) AND normalized_value=?
		ORDER BY id`, time.Now().UTC(), mailbox)
	return holders[mailbox], err
}

// deliverySpelledHoldersTx finds display-name, quoted, grouped and similar
// spellings in the domain through the partial spelling index.
func (s *Store) deliverySpelledHoldersTx(ctx context.Context, tx *loggedTx, domain string) (map[string][]deliveryHolder, error) {
	escaped := strings.NewReplacer(`!`, `!!`, `%`, `!%`, `_`, `!_`).Replace(domain)
	from := "person_contact_points"
	if !s.IsPostgreSQL() {
		// SQLite otherwise prefers the lookup index and walks every email row;
		// PostgreSQL has no INDEXED BY and picks the partial index on cost.
		from += " INDEXED BY idx_person_contact_points_email_spelling"
	}
	byMailbox, err := s.deliveryHoldersTx(ctx, tx, `SELECT id, person_id, source, normalized_value, service_id IS NULL AND scope_kind IS NULL AND scope_value IS NULL FROM `+from+`
		WHERE `+deliveryActiveEmailSQL+` AND `+deliverySpelledSQL+` AND (active_from IS NULL OR `+s.deliveryActiveFromComparison("?")+`)
		AND (LOWER(normalized_value) LIKE ? ESCAPE '!' OR LENGTH(normalized_value) <> OCTET_LENGTH(normalized_value))
		ORDER BY id`, time.Now().UTC(), "%"+escaped+"%")
	if err != nil {
		return nil, err
	}
	return byMailbox, nil
}

func (s *Store) deliveryHoldersTx(ctx context.Context, tx *loggedTx, query string, args ...any) (map[string][]deliveryHolder, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	byMailbox := map[string][]deliveryHolder{}
	for rows.Next() {
		var h deliveryHolder
		var source Provenance
		var value string
		if err := rows.Scan(&h.id, &h.owner, &source, &value, &h.declared); err != nil {
			return nil, err
		}
		// Any spelling net/mail resolves to a mailbox (display name, comment,
		// quotes, group) makes its owner a holder of it.
		mailbox, ok := parseDeliveryMailbox(value)
		if !ok {
			continue
		}
		h.plain, h.declared = value == mailbox, h.declared && source.IsDeclared()
		byMailbox[mailbox] = append(byMailbox[mailbox], h)
	}
	return byMailbox, rows.Err()
}

func (s *Store) SetDeliveryPolicyContext(ctx context.Context, w DeliveryPolicyWrite) (*DeliveryPolicyReceipt, error) {
	return s.writeDeliveryPolicy(ctx, w, false)
}
func (s *Store) ClearDeliveryPolicyContext(ctx context.Context, w DeliveryPolicyWrite) (*DeliveryPolicyReceipt, error) {
	return s.writeDeliveryPolicy(ctx, w, true)
}

// bumpDeliveryRevisionTx moves the revision clients compare on a policy write
// without moving persons.revision, so a pending enrichment result stays valid.
func (s *Store) bumpDeliveryRevisionTx(ctx context.Context, tx *loggedTx, personID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE persons SET delivery_revision = delivery_revision + 1 WHERE id=?`, personID)
	return err
}

// writeDeliveryPolicy edits one policy row, bumps the delivery revision so a
// stale policy write cannot repeat or undo it, and records an audit row, all in
// one transaction. A write that changes nothing touches nothing.
func (s *Store) writeDeliveryPolicy(ctx context.Context, w DeliveryPolicyWrite, reset bool) (*DeliveryPolicyReceipt, error) {
	q, err := resolveDeliveryQuery(DeliveryPolicyQuery{PersonUID: w.PersonUID, Endpoint: w.Endpoint})
	if err != nil {
		return nil, err
	}
	if (!reset && w.Policy != DeliveryDraftOnly && w.Policy != DeliverySendAllowed) || w.ExpectedRevision < 1 || !deliveryStringValid(w.Actor) || !deliveryStringValid(w.Reason) {
		return nil, ErrDeliveryPolicyInvalid
	}
	if !reset && w.Policy == DeliverySendAllowed && w.Endpoint == "" && w.ScopeAcknowledgement != DeliveryPersonAllRoutes {
		return nil, deliveryInvalid("scope_acknowledgement_missing")
	}
	var receipt *DeliveryPolicyReceipt
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		holders := newDeliveryHolderCache()
		before, unavailable, err := s.deliveryStateTx(ctx, tx, q, true, !reset && w.Policy == DeliverySendAllowed, holders)
		if err != nil {
			return err
		}
		if before.Revision != w.ExpectedRevision {
			return ErrDeliveryPolicyRevisionConflict
		}
		// Clearing stays possible after the address goes away.
		if !reset && w.Policy == DeliverySendAllowed && unavailable != "" {
			return deliveryInvalid(unavailable)
		}
		// A restriction skips the cross-person scan but cannot create a policy for an address the
		// person never held; revoking an existing override stays possible after the address goes away.
		if !reset && w.Policy == DeliveryDraftOnly && q.Endpoint != "" && before.StoredPolicy == nil {
			plain, ok := holders.plain[q.Endpoint]
			if !ok {
				plain, err = s.deliveryPlainHoldersTx(ctx, tx, q.Endpoint)
				if err != nil {
					return err
				}
				holders.plain[q.Endpoint] = plain
			}
			if !slices.ContainsFunc(plain, func(h deliveryHolder) bool { return h.owner == before.PersonID }) {
				return deliveryInvalid("detached_endpoint")
			}
		}
		var approvedThrough sql.NullInt64
		uncoveredAddress := false
		if !reset && w.Policy == DeliverySendAllowed && q.Endpoint == "" {
			points, err := s.deliveryHoldersTx(ctx, tx, `SELECT id, person_id, source, normalized_value, service_id IS NULL AND scope_kind IS NULL AND scope_value IS NULL FROM person_contact_points
				WHERE person_id=? AND `+deliveryActiveEmailSQL+` AND (active_from IS NULL OR `+s.deliveryActiveFromComparison("?")+`)`, before.PersonID, time.Now().UTC())
			if err != nil {
				return err
			}
			for _, holders := range points {
				var own []int64
				for _, h := range holders {
					if h.plain && h.declared {
						approvedThrough.Int64 = max(approvedThrough.Int64, h.id)
						own = append(own, h.id)
					}
				}
				if len(own) != 0 && !uncoveredAddress {
					covered, err := s.deliveryApprovalCoversPointsTx(ctx, tx, q.PersonUID, before.approvedThrough, own)
					if err != nil {
						return err
					}
					uncoveredAddress = !covered
				}
			}
			approvedThrough.Valid = true
		}
		unchanged := before.StoredPolicy == nil
		if !reset {
			// A lapsed approval binding counts as a change, so re-approving rebinds it.
			unchanged = before.StoredPolicy != nil && *before.StoredPolicy == w.Policy && before.Reason != "binding_changed"
			if uncoveredAddress {
				unchanged = false
			}
		}
		if unchanged {
			receipt = &DeliveryPolicyReceipt{Before: *before, After: *before}
			return nil
		}
		scope, mailbox := DeliveryPersonAllRoutes, q.Endpoint
		if mailbox != "" {
			scope = mailbox
		}
		var point sql.NullInt64
		if reset {
			_, err = tx.ExecContext(ctx, `DELETE FROM delivery_policies WHERE person_id=? AND mailbox=?`, before.PersonID, mailbox)
		} else {
			if w.Policy == DeliverySendAllowed && mailbox != "" {
				point = sql.NullInt64{Int64: before.points[0], Valid: true}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO delivery_policies(person_id,mailbox,policy,contact_point_id,approved_through_contact_point_id) VALUES (?,?,?,?,?)
				ON CONFLICT(person_id,mailbox) DO UPDATE SET policy=excluded.policy, contact_point_id=excluded.contact_point_id, approved_through_contact_point_id=excluded.approved_through_contact_point_id`,
				before.PersonID, mailbox, w.Policy, point, approvedThrough)
		}
		if err != nil {
			return err
		}
		if err = s.bumpDeliveryRevisionTx(ctx, tx, before.PersonID); err != nil {
			return err
		}
		after, _, err := s.deliveryStateTx(ctx, tx, q, false, false, holders)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err = insertDeliveryAuditTx(ctx, tx, deliveryAuditRow{
			PersonUID: w.PersonUID, Revision: after.Revision, Scope: scope, Before: before.StoredPolicy,
			After: after.StoredPolicy, Actor: w.Actor, Reason: w.Reason, At: now, Point: point,
		}); err != nil {
			return err
		}
		receipt = &DeliveryPolicyReceipt{Before: *before, After: *after, Audit: &DeliveryPolicyAudit{
			Actor: w.Actor, Reason: w.Reason, CreatedAt: now, Scope: scope,
			BeforePolicy: before.StoredPolicy, AfterPolicy: after.StoredPolicy, ContactPointID: nullInt64Ptr(point),
		}}
		return nil
	})
	return receipt, err
}

// deliveryAuditRow is one delivery_policy_audit row.
type deliveryAuditRow struct {
	PersonUID, Scope, Actor, Reason string
	Revision                        int64
	Before, After                   *DeliveryPolicy
	At                              time.Time
	Point                           sql.NullInt64
}

func insertDeliveryAuditTx(ctx context.Context, tx *loggedTx, r deliveryAuditRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO delivery_policy_audit(person_uid,revision,scope,before_policy,after_policy,actor,reason,created_at,contact_point_id) VALUES (?,?,?,?,?,?,?,?,?)`,
		r.PersonUID, r.Revision, r.Scope, r.Before, r.After, r.Actor, r.Reason, r.At, r.Point)
	return err
}

// endDeliveryPoliciesTx ends delivery policies of one person with an audit row
// each. A merge or delete never widens what was approved: sending stays
// draft_only until the owner approves again. With wideSendOnly, only a
// person-wide send_allowed ends, since a merge grows its coverage to addresses
// the owner never approved; exact-address approvals stay bound to their contact points.
func (s *Store) endDeliveryPoliciesTx(ctx context.Context, tx *loggedTx, owner *Person, revision int64, actor, reason string, wideSendOnly bool) error {
	scopeFilter := ""
	if wideSendOnly {
		scopeFilter = " AND mailbox='' AND policy='send_allowed'"
	}
	now := time.Now().UTC()
	// The caller passes persons.revision; clients see it plus the delivery revision.
	var delivery int64
	if err := tx.QueryRowContext(ctx, `SELECT delivery_revision FROM persons WHERE id=?`, owner.ID).Scan(&delivery); err != nil {
		return err
	}
	revision += delivery
	rows, err := tx.QueryContext(ctx, `SELECT mailbox, policy FROM delivery_policies WHERE person_id=?`+scopeFilter+` ORDER BY mailbox`, owner.ID)
	if err != nil {
		return err
	}
	var ended []deliveryAuditRow
	for rows.Next() {
		var mailbox string
		var policy DeliveryPolicy
		if err := rows.Scan(&mailbox, &policy); err != nil {
			_ = rows.Close()
			return err
		}
		scope := mailbox
		if mailbox == "" {
			scope = DeliveryPersonAllRoutes
		}
		ended = append(ended, deliveryAuditRow{
			PersonUID: owner.VCardUID, Revision: revision, Scope: scope, Before: &policy,
			Actor: actor, Reason: reason, At: now,
		})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, row := range ended {
		if err := insertDeliveryAuditTx(ctx, tx, row); err != nil {
			return err
		}
	}
	if len(ended) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM delivery_policies WHERE person_id=?`+scopeFilter, owner.ID); err != nil {
			return err
		}
	}
	return nil
}
