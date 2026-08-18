package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	activityWatermarkKey          = "activity_spine_watermark"
	activityTimezoneKey           = "activity_spine_timezone"
	activityTimezoneActiveKey     = "activity_spine_timezone_active"
	activityTimezoneGenerationKey = "activity_spine_timezone_generation"
	activityDirectLimitKey        = "activity_spine_max_direct_counterparts"
	activityDirectLimitActiveKey  = "activity_spine_max_direct_counterparts_active"
	activityDirectLimitGenKey     = "activity_spine_max_direct_counterparts_generation"
)

var ErrInvalidActivity = errors.New("invalid activity")

type ActivityDirection string

const (
	DirectionInbound  ActivityDirection = "inbound"
	DirectionOutbound ActivityDirection = "outbound"
	DirectionObserved ActivityDirection = "observed"
)

func (value ActivityDirection) Valid() bool {
	switch value {
	case DirectionInbound, DirectionOutbound, DirectionObserved:
		return true
	default:
		return false
	}
}

type ActivityChannel string

const (
	ChannelEmail   ActivityChannel = "email"
	ChannelChat    ActivityChannel = "chat"
	ChannelMeeting ActivityChannel = "meeting"
	ChannelOther   ActivityChannel = "other"
)

func (value ActivityChannel) Valid() bool {
	switch value {
	case ChannelEmail, ChannelChat, ChannelMeeting, ChannelOther:
		return true
	default:
		return false
	}
}

type ActivityRole string

const (
	RoleSender    ActivityRole = "sender"
	RoleAddressed ActivityRole = "addressed"
	RoleOrganizer ActivityRole = "organizer"
	RoleAttendee  ActivityRole = "attendee"
	RoleMember    ActivityRole = "member"
)

func (value ActivityRole) Valid() bool {
	switch value {
	case RoleSender, RoleAddressed, RoleOrganizer, RoleAttendee, RoleMember:
		return true
	default:
		return false
	}
}

type ActivityEvidence string

const (
	EvidenceDirect     ActivityEvidence = "direct"
	EvidenceCoPresence ActivityEvidence = "co_presence"
)

func (value ActivityEvidence) Valid() bool {
	switch value {
	case EvidenceDirect, EvidenceCoPresence:
		return true
	default:
		return false
	}
}

type ActivityRefKind string

const (
	RefKindMessage ActivityRefKind = "message"
	RefKindMeeting ActivityRefKind = "meeting"
)

func (value ActivityRefKind) Valid() bool {
	switch value {
	case RefKindMessage, RefKindMeeting:
		return true
	default:
		return false
	}
}

// IsMeetingMessageType reports whether a messages.message_type value is
// meeting evidence for activity classification: calendar events (gcal) and
// imported meeting transcripts (Granola, Circleback, generic meeting import).
// Classification and projection-token validation must share this predicate —
// they independently derive RefKind/Channel from the same inputs, and any
// disagreement makes the authoritative write reject the projection as stale.
func IsMeetingMessageType(messageType string) bool {
	return messageType == "calendar_event" || messageType == "meeting_transcript"
}

type ActivityEventPerson struct {
	PersonID int64
	Role     ActivityRole
	Evidence ActivityEvidence
}

func (link ActivityEventPerson) Validate() error {
	switch {
	case link.PersonID <= 0:
		return fmt.Errorf("%w: person id must be positive", ErrInvalidActivity)
	case !link.Role.Valid():
		return fmt.Errorf("%w: unknown role %q", ErrInvalidActivity, link.Role)
	case !link.Evidence.Valid():
		return fmt.Errorf("%w: unknown evidence %q", ErrInvalidActivity, link.Evidence)
	default:
		return nil
	}
}

type ActivityEvent struct {
	MessageID                        int64
	RefKind                          ActivityRefKind
	SourceID                         int64
	ConversationID                   *int64
	Channel                          ActivityChannel
	OccurredAt                       time.Time
	DateOrigin                       string
	DatePrecision                    string
	Timezone                         string
	UTCOffsetMinutes                 int
	LocalDate                        string
	Direction                        ActivityDirection
	OwnerSourceID                    *int64
	OwnerAddress                     string
	ProjectedLastModified            time.Time
	ProjectedIdentityRevision        int64
	ProjectedAccountIdentityRevision int64
	Persons                          []ActivityEventPerson
}

func (event ActivityEvent) Ref() string {
	return string(event.RefKind) + ":" + strconv.FormatInt(event.MessageID, 10)
}

func (event ActivityEvent) Validate() error {
	switch {
	case event.MessageID <= 0:
		return fmt.Errorf("%w: message id must be positive", ErrInvalidActivity)
	case !event.RefKind.Valid():
		return fmt.Errorf("%w: unknown reference kind %q", ErrInvalidActivity, event.RefKind)
	case event.SourceID <= 0:
		return fmt.Errorf("%w: source id must be positive", ErrInvalidActivity)
	case !event.Channel.Valid():
		return fmt.Errorf("%w: unknown channel %q", ErrInvalidActivity, event.Channel)
	case event.OccurredAt.IsZero():
		return fmt.Errorf("%w: occurred at is required", ErrInvalidActivity)
	case event.DateOrigin != "sent_at" &&
		event.DateOrigin != "received_at" &&
		event.DateOrigin != "internal_date":
		return fmt.Errorf("%w: unknown date origin %q", ErrInvalidActivity, event.DateOrigin)
	case event.DatePrecision != "timestamp" && event.DatePrecision != "day":
		return fmt.Errorf("%w: unknown date precision %q", ErrInvalidActivity, event.DatePrecision)
	case strings.TrimSpace(event.Timezone) == "":
		return fmt.Errorf("%w: timezone is required", ErrInvalidActivity)
	case event.UTCOffsetMinutes < -840 || event.UTCOffsetMinutes > 840:
		return fmt.Errorf("%w: UTC offset is outside supported range", ErrInvalidActivity)
	case !validActivityDate(event.LocalDate):
		return fmt.Errorf("%w: local date %q is invalid", ErrInvalidActivity, event.LocalDate)
	case !event.Direction.Valid():
		return fmt.Errorf("%w: unknown direction %q", ErrInvalidActivity, event.Direction)
	case event.ProjectedLastModified.IsZero():
		return fmt.Errorf("%w: projected last modified is required", ErrInvalidActivity)
	case event.ProjectedIdentityRevision < 0:
		return fmt.Errorf("%w: identity revision must be non-negative", ErrInvalidActivity)
	case event.ProjectedAccountIdentityRevision < 0:
		return fmt.Errorf("%w: account identity revision must be non-negative", ErrInvalidActivity)
	}
	seen := make(map[int64]struct{}, len(event.Persons))
	for _, link := range event.Persons {
		if err := link.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[link.PersonID]; duplicate {
			return fmt.Errorf("%w: duplicate person id %d", ErrInvalidActivity, link.PersonID)
		}
		seen[link.PersonID] = struct{}{}
	}
	return nil
}

func validActivityDate(value string) bool {
	parsed, err := time.Parse(time.DateOnly, value)
	return err == nil && parsed.Format(time.DateOnly) == value
}

type ActivityProjectionQueueItem struct {
	MessageID int64
	Revision  int64
}

func (s *Store) ListActivityProjectionQueueContext(
	ctx context.Context, limit int,
) ([]ActivityProjectionQueueItem, error) {
	if limit <= 0 {
		return []ActivityProjectionQueueItem{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT message_id, revision
		FROM activity_projection_queue
		WHERE revision > processed_revision
		ORDER BY message_id
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list activity projection queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]ActivityProjectionQueueItem, 0, limit)
	for rows.Next() {
		var item ActivityProjectionQueueItem
		if err := rows.Scan(&item.MessageID, &item.Revision); err != nil {
			return nil, fmt.Errorf("scan activity projection queue: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity projection queue: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close activity projection queue: %w", err)
	}
	return items, nil
}

func (s *Store) ActivityWatermarkContext(ctx context.Context) (int64, error) {
	value, err := s.archiveMetadataValueContext(ctx, activityWatermarkKey)
	if err != nil {
		return 0, err
	}
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid activity watermark %q: %w", value, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("invalid activity watermark %q: must be non-negative", value)
	}
	return parsed, nil
}

func (s *Store) SetActivityWatermarkContext(ctx context.Context, messageID int64) error {
	if messageID < 0 {
		return fmt.Errorf("%w: watermark must be non-negative", ErrInvalidActivity)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var value string
		err := tx.QueryRowContext(ctx,
			`SELECT value FROM archive_metadata WHERE key = ?`,
			activityWatermarkKey).Scan(&value)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read archive metadata %q: %w",
				activityWatermarkKey, err)
		default:
			current, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr == nil && current >= messageID {
				return nil
			}
		}
		return setArchiveMetadataValueTx(ctx, tx,
			activityWatermarkKey, strconv.FormatInt(messageID, 10))
	})
}

// ActivityTimezoneTransition is the exact persisted timezone generation
// observed by a candidate. Target is the active target while Active is true,
// otherwise it is the last completed timezone (and may be empty on a legacy
// archive).
type ActivityTimezoneTransition struct {
	Active     bool
	Target     string
	Generation int64
}

// ActivityDirectLimitTransition is the exact persisted classification-policy
// generation observed by a projection batch. Active transitions are replayed
// after a crash before a differently configured worker can claim its target.
type ActivityDirectLimitTransition struct {
	Active     bool
	Target     int
	Generation int64
}

// ActivityDirectLimitTransitionContext returns the current completed or active
// direct-counterpart classification policy.
func (s *Store) ActivityDirectLimitTransitionContext(
	ctx context.Context,
) (ActivityDirectLimitTransition, error) {
	return s.activityDirectLimitTransitionQueryContext(ctx, s.db)
}

// ClaimActivityDirectLimitTransitionContext serializes a policy change behind
// the identity mutation lock. An active transition is returned for crash help.
func (s *Store) ClaimActivityDirectLimitTransitionContext(
	ctx context.Context,
	target int,
) (ActivityDirectLimitTransition, error) {
	if target <= 0 {
		return ActivityDirectLimitTransition{},
			fmt.Errorf("%w: max direct counterparts must be positive", ErrInvalidActivity)
	}
	var result ActivityDirectLimitTransition
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		current, err := s.activityDirectLimitTransitionQueryContext(ctx, tx)
		if err != nil {
			return err
		}
		if current.Active ||
			(current.Target == target && current.Generation > 0) {
			result = current
			return nil
		}
		current.Active = true
		current.Target = target
		current.Generation++
		if err := setArchiveMetadataValueTx(ctx, tx,
			activityDirectLimitActiveKey, strconv.Itoa(target)); err != nil {
			return err
		}
		if err := setArchiveMetadataValueTx(ctx, tx,
			activityDirectLimitGenKey,
			strconv.FormatInt(current.Generation, 10)); err != nil {
			return err
		}
		result = current
		return nil
	})
	return result, err
}

// CompleteActivityDirectLimitTransitionContext commits a fully replayed policy
// only when the exact generation remains active and the queue is drained.
func (s *Store) CompleteActivityDirectLimitTransitionContext(
	ctx context.Context,
	observed ActivityDirectLimitTransition,
) error {
	if !observed.Active || observed.Generation <= 0 || observed.Target <= 0 {
		return fmt.Errorf("%w: active direct-counterpart transition is required",
			ErrInvalidActivity)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		current, err := s.activityDirectLimitTransitionQueryContext(ctx, tx)
		if err != nil {
			return err
		}
		if current != observed {
			return &ErrActivityProjectionStale{
				Reason: "direct-counterpart transition changed",
			}
		}
		var pending int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM activity_projection_queue
			WHERE revision > processed_revision
		`).Scan(&pending); err != nil {
			return fmt.Errorf("check pending activity queue: %w", err)
		}
		if pending != 0 {
			return &ErrActivityProjectionStale{
				Reason: "activity queue changed during direct-counterpart transition",
			}
		}
		if err := setArchiveMetadataValueTx(ctx, tx,
			activityDirectLimitKey, strconv.Itoa(observed.Target)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM archive_metadata WHERE key = ?`,
			activityDirectLimitActiveKey); err != nil {
			return fmt.Errorf("clear active direct-counterpart limit: %w", err)
		}
		return nil
	})
}

func (s *Store) activityDirectLimitTransitionQueryContext(
	ctx context.Context,
	queryer contextRowQuerier,
) (ActivityDirectLimitTransition, error) {
	var completed, active, generationValue sql.NullString
	err := queryer.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT
			MAX(CASE WHEN key = ? THEN value END),
			MAX(CASE WHEN key = ? THEN value END),
			MAX(CASE WHEN key = ? THEN value END)
		FROM archive_metadata
		WHERE key IN (?, ?, ?)
	`),
		activityDirectLimitKey,
		activityDirectLimitActiveKey,
		activityDirectLimitGenKey,
		activityDirectLimitKey,
		activityDirectLimitActiveKey,
		activityDirectLimitGenKey,
	).Scan(&completed, &active, &generationValue)
	if err != nil {
		return ActivityDirectLimitTransition{},
			fmt.Errorf("read direct-counterpart transition: %w", err)
	}
	var generation int64
	if generationValue.Valid {
		generation, err = strconv.ParseInt(generationValue.String, 10, 64)
		if err != nil || generation < 0 {
			return ActivityDirectLimitTransition{},
				fmt.Errorf("read direct-counterpart transition: invalid generation %q",
					generationValue.String)
		}
	}
	value := completed
	isActive := active.Valid
	if isActive {
		value = active
		if generation <= 0 {
			return ActivityDirectLimitTransition{},
				errors.New("read direct-counterpart transition: active generation must be positive")
		}
	}
	var target int
	if value.Valid {
		target, err = strconv.Atoi(value.String)
		if err != nil || target <= 0 {
			return ActivityDirectLimitTransition{},
				fmt.Errorf("read direct-counterpart transition: invalid target %q", value.String)
		}
	}
	return ActivityDirectLimitTransition{
		Active: isActive, Target: target, Generation: generation,
	}, nil
}

func (s *Store) ActivityTimezoneTransitionContext(
	ctx context.Context,
) (ActivityTimezoneTransition, error) {
	return s.activityTimezoneTransitionQueryContext(ctx, s.db)
}

// ClaimActivityTimezoneTransitionContext serializes transition claims behind
// the identity mutation lock. An already-active transition is returned so a
// differently configured worker can yield or help the persisted target.
func (s *Store) ClaimActivityTimezoneTransitionContext(
	ctx context.Context,
	target string,
) (ActivityTimezoneTransition, error) {
	if strings.TrimSpace(target) == "" {
		return ActivityTimezoneTransition{},
			fmt.Errorf("%w: timezone is required", ErrInvalidActivity)
	}
	// Replay keys on the persisted zone NAME. "Local" resolves against the
	// host's TZ setting, so a host change would skew new projections without
	// ever triggering a replay of existing rows.
	if target == "Local" {
		return ActivityTimezoneTransition{},
			fmt.Errorf("%w: timezone %q is host-dependent; use UTC or an IANA zone name",
				ErrInvalidActivity, target)
	}
	if _, err := time.LoadLocation(target); err != nil {
		return ActivityTimezoneTransition{},
			fmt.Errorf("%w: invalid timezone %q: %w", ErrInvalidActivity, target, err)
	}
	var result ActivityTimezoneTransition
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		current, err := s.activityTimezoneTransitionQueryContext(ctx, tx)
		if err != nil {
			return err
		}
		if current.Active ||
			(current.Target == target && current.Generation > 0) {
			result = current
			return nil
		}
		current.Active = true
		current.Target = target
		current.Generation++
		if err := setArchiveMetadataValueTx(
			ctx, tx, activityTimezoneActiveKey, target); err != nil {
			return err
		}
		if err := setArchiveMetadataValueTx(
			ctx, tx, activityTimezoneGenerationKey,
			strconv.FormatInt(current.Generation, 10)); err != nil {
			return err
		}
		result = current
		return nil
	})
	return result, err
}

// CompleteActivityTimezoneTransitionContext commits the completed timezone
// only when the exact active generation is still current and no pending queue
// generation exists.
func (s *Store) CompleteActivityTimezoneTransitionContext(
	ctx context.Context,
	observed ActivityTimezoneTransition,
) error {
	if !observed.Active || observed.Generation <= 0 ||
		strings.TrimSpace(observed.Target) == "" {
		return fmt.Errorf("%w: active timezone transition is required",
			ErrInvalidActivity)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		current, err := s.activityTimezoneTransitionQueryContext(ctx, tx)
		if err != nil {
			return err
		}
		if current != observed {
			return &ErrActivityProjectionStale{
				Reason: "timezone transition changed",
			}
		}
		var pending int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM activity_projection_queue
			WHERE revision > processed_revision
		`).Scan(&pending); err != nil {
			return fmt.Errorf("check pending activity queue: %w", err)
		}
		if pending != 0 {
			return &ErrActivityProjectionStale{
				Reason: "activity queue changed during timezone transition",
			}
		}
		if err := setArchiveMetadataValueTx(
			ctx, tx, activityTimezoneKey, observed.Target); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM archive_metadata WHERE key = ?`,
			activityTimezoneActiveKey); err != nil {
			return fmt.Errorf("clear active activity timezone: %w", err)
		}
		return nil
	})
}

func (s *Store) activityTimezoneTransitionQueryContext(
	ctx context.Context,
	queryer contextRowQuerier,
) (ActivityTimezoneTransition, error) {
	var completed, active sql.NullString
	var generationValue sql.NullString
	err := queryer.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT
			MAX(CASE WHEN key = ? THEN value END),
			MAX(CASE WHEN key = ? THEN value END),
			MAX(CASE WHEN key = ? THEN value END)
		FROM archive_metadata
		WHERE key IN (?, ?, ?)
	`),
		activityTimezoneKey,
		activityTimezoneActiveKey,
		activityTimezoneGenerationKey,
		activityTimezoneKey,
		activityTimezoneActiveKey,
		activityTimezoneGenerationKey,
	).Scan(&completed, &active, &generationValue)
	if err != nil {
		return ActivityTimezoneTransition{},
			fmt.Errorf("read activity timezone transition: %w", err)
	}
	var generation int64
	if generationValue.Valid {
		parsed, parseErr := strconv.ParseInt(generationValue.String, 10, 64)
		if parseErr != nil || parsed < 0 {
			return ActivityTimezoneTransition{},
				fmt.Errorf("read activity timezone transition: invalid generation %q",
					generationValue.String)
		}
		generation = parsed
	}
	if active.Valid {
		if generation <= 0 {
			return ActivityTimezoneTransition{},
				errors.New("read activity timezone transition: active generation must be positive")
		}
		if _, zoneErr := time.LoadLocation(active.String); zoneErr != nil {
			return ActivityTimezoneTransition{},
				fmt.Errorf("read activity timezone transition: invalid active timezone %q: %w",
					active.String, zoneErr)
		}
		return ActivityTimezoneTransition{
			Active: true, Target: active.String, Generation: generation,
		}, nil
	}
	if completed.Valid && completed.String != "" {
		if _, zoneErr := time.LoadLocation(completed.String); zoneErr != nil {
			return ActivityTimezoneTransition{},
				fmt.Errorf("read activity timezone transition: invalid completed timezone %q: %w",
					completed.String, zoneErr)
		}
	}
	return ActivityTimezoneTransition{
		Target: completed.String, Generation: generation,
	}, nil
}

func setArchiveMetadataValueTx(
	ctx context.Context,
	tx *loggedTx,
	key, value string,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO archive_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("write archive metadata %q: %w", key, err)
	}
	return nil
}

func (s *Store) archiveMetadataValueContext(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read archive metadata %q: %w", key, err)
	}
	return value, nil
}

const (
	activityReconciledIdentityRevisionKey = "activity_spine_reconciled_identity_revision"
	activityReconciledAccountRevisionKey  = "activity_spine_reconciled_account_identity_revision"
	activityProjectionMaxAttempts         = 5
	activityContactStateBatchDefault      = 500
	CadenceUnknown                        = "unknown"
)

// ErrActivityProjectionStale means a candidate's exact mutable-input
// observation no longer matches the archive. Callers must reload and
// reclassify it; retrying the unchanged projection cannot be correct.
type ErrActivityProjectionStale struct { //nolint:errname // Err prefix preserves errors.As call-site clarity.
	MessageID int64
	Reason    string
}

func (err *ErrActivityProjectionStale) Error() string {
	if err.MessageID == 0 {
		return "activity projection is stale: " + err.Reason
	}
	return fmt.Sprintf("activity projection for message %d is stale: %s",
		err.MessageID, err.Reason)
}

// ActivityProjectionToken is the exact Task 3 observation validated again in
// the authoritative write transaction.
type ActivityProjectionToken struct {
	MessageID               int64
	SourceID                int64
	LastModified            time.Time
	Queue                   ActivityQueueObservation
	TimezoneTransition      ActivityTimezoneTransition
	DirectLimitTransition   ActivityDirectLimitTransition
	IdentityRevision        int64
	AccountIdentityRevision int64
	ConversationID          *int64
	ConversationType        string
	MessageType             string
}

// ActivityProjection replaces one native event. A nil Event is an
// authoritative retraction.
type ActivityProjection struct {
	Token ActivityProjectionToken
	Event *ActivityEvent
}

// ActivityProjectionResult summarizes committed work. Counts are zero when an
// exact replay only drains a queue generation.
type ActivityProjectionResult struct {
	Processed       int
	EventsWritten   int
	EventsRetracted int
	ContactPersons  int
}

// ContactRevisions is the identity epoch against which contact state was
// classified.
type ContactRevisions struct {
	IdentityRevision        int64
	AccountIdentityRevision int64
}

// ActivityReconciledRevisionsContext returns the last completely reconciled
// identity epoch. Missing either metadata key is an unreconciled legacy state.
func (s *Store) ActivityReconciledRevisionsContext(
	ctx context.Context,
) (ContactRevisions, bool, error) {
	var identity, account sql.NullString
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`
		SELECT
			MAX(CASE WHEN key = ? THEN value END),
			MAX(CASE WHEN key = ? THEN value END)
		FROM archive_metadata
		WHERE key IN (?, ?)
	`),
		activityReconciledIdentityRevisionKey,
		activityReconciledAccountRevisionKey,
		activityReconciledIdentityRevisionKey,
		activityReconciledAccountRevisionKey,
	).Scan(&identity, &account)
	if err != nil {
		return ContactRevisions{}, false,
			fmt.Errorf("read reconciled activity revisions: %w", err)
	}
	if !identity.Valid || !account.Valid {
		return ContactRevisions{}, false, nil
	}
	identityValue, identityErr := strconv.ParseInt(identity.String, 10, 64)
	accountValue, accountErr := strconv.ParseInt(account.String, 10, 64)
	if identityErr != nil || accountErr != nil ||
		identityValue < 0 || accountValue < 0 {
		return ContactRevisions{}, false,
			errors.New("read reconciled activity revisions: invalid metadata")
	}
	return ContactRevisions{
		IdentityRevision:        identityValue,
		AccountIdentityRevision: accountValue,
	}, true, nil
}

// CompareAndSetActivityReconciledRevisionsContext records a completed pass
// only while the identity mutation lock proves the current epoch is unchanged.
func (s *Store) CompareAndSetActivityReconciledRevisionsContext(
	ctx context.Context,
	expected ContactRevisions,
) error {
	if expected.IdentityRevision < 0 || expected.AccountIdentityRevision < 0 {
		return fmt.Errorf("%w: reconciled revisions must be non-negative",
			ErrInvalidActivity)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		identity, err := s.currentIdentityRevisionTx(tx)
		if err != nil {
			return err
		}
		account, err := readAccountIdentityRevision(tx)
		if err != nil {
			return err
		}
		if identity != expected.IdentityRevision ||
			account != expected.AccountIdentityRevision {
			return &ErrActivityProjectionStale{
				Reason: "identity epoch changed before reconciliation",
			}
		}
		if err := setArchiveMetadataValueTx(ctx, tx,
			activityReconciledIdentityRevisionKey,
			strconv.FormatInt(identity, 10)); err != nil {
			return err
		}
		return setArchiveMetadataValueTx(ctx, tx,
			activityReconciledAccountRevisionKey,
			strconv.FormatInt(account, 10))
	})
}

// ContactState is the stored contact projection. Task 8 augments the cadence
// and inferred-channel fields at read time.
type ContactState struct {
	PersonID            int64           `json:"person_id"`
	FirstContactAt      *time.Time      `json:"first_contact_at,omitempty"`
	FirstContactRef     string          `json:"first_contact_ref,omitempty"`
	LastContactAt       *time.Time      `json:"last_contact_at,omitempty"`
	LastContactRef      string          `json:"last_contact_ref,omitempty"`
	LastContactChannel  ActivityChannel `json:"last_contact_channel,omitempty"`
	LastContactSourceID *int64          `json:"last_contact_source_id,omitempty"`
	LastContactOwner    string          `json:"last_contact_owner,omitempty"`
	LastInboundAt       *time.Time      `json:"last_inbound_at,omitempty"`
	LastInboundRef      string          `json:"last_inbound_ref,omitempty"`
	LastOutboundAt      *time.Time      `json:"last_outbound_at,omitempty"`
	LastOutboundRef     string          `json:"last_outbound_ref,omitempty"`
	InteractionCount    int64           `json:"interaction_count"`
	InferredChannel     ActivityChannel `json:"inferred_channel,omitempty"`
	CadenceDueAt        *time.Time      `json:"cadence_due_at,omitempty"`
	CadenceStatus       string          `json:"cadence_status"`
	Stale               bool            `json:"stale"`
	ComputedAt          time.Time       `json:"computed_at"`
}

func (s *Store) ContactRevisionsContext(
	ctx context.Context,
) (ContactRevisions, error) {
	var revisions ContactRevisions
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(MAX(CASE WHEN key = 'identity_revision'
				THEN CAST(value AS BIGINT) END), 0),
			COALESCE(MAX(CASE WHEN key = 'account_identity_revision'
				THEN CAST(value AS BIGINT) END), 0)
		FROM archive_metadata
		WHERE key IN ('identity_revision', 'account_identity_revision')
	`).Scan(
		&revisions.IdentityRevision,
		&revisions.AccountIdentityRevision,
	)
	if err != nil {
		return ContactRevisions{}, fmt.Errorf("read contact revisions: %w", err)
	}
	return revisions, nil
}

// MarkContactStateDirtyContext invalidates existing contact-state rows for the
// selected people. People without a computed row are intentionally a no-op.
func (s *Store) MarkContactStateDirtyContext(
	ctx context.Context,
	personIDs ...int64,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		return s.markContactStateDirtyTx(ctx, tx, personIDs...)
	})
}

// MarkAllContactStateDirtyContext invalidates every existing contact-state
// row. It does not create rows for people without computed state.
func (s *Store) MarkAllContactStateDirtyContext(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE person_contact_state
			SET dirty_at = COALESCE(dirty_at, %s)
		`, s.dialect.Now())); err != nil {
			return fmt.Errorf("mark all contact state dirty: %w", err)
		}
		return nil
	})
}

// markContactStateDirtyTx assumes its caller already holds the identity
// mutation lock, preserving identity-before-contact lock ordering.
func (s *Store) markContactStateDirtyTx(
	ctx context.Context,
	tx *loggedTx,
	personIDs ...int64,
) error {
	unique := make(map[int64]struct{}, len(personIDs))
	for _, personID := range personIDs {
		if personID <= 0 {
			return fmt.Errorf("%w: contact person id must be positive",
				ErrInvalidActivity)
		}
		unique[personID] = struct{}{}
	}
	sorted := sortedInt64Set(unique)
	if len(sorted) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sorted)), ",")
	args := make([]any, len(sorted))
	for index, personID := range sorted {
		args[index] = personID
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE person_contact_state
		SET dirty_at = COALESCE(dirty_at, %s)
		WHERE person_id IN (%s)
	`, s.dialect.Now(), placeholders), args...); err != nil {
		return fmt.Errorf("mark contact state dirty: %w", err)
	}
	return nil
}

// StaleContactStatePersonsContext returns people whose stored contact state is
// explicitly dirty, stamped against an older identity epoch, or missing even
// though direct activity evidence exists.
func (s *Store) StaleContactStatePersonsContext(
	ctx context.Context,
	limit int,
) ([]int64, error) {
	if limit <= 0 {
		limit = activityContactStateBatchDefault
	}
	var personIDs []int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		identityRevision, err := s.currentIdentityRevisionTx(tx)
		if err != nil {
			return err
		}
		accountRevision, err := readAccountIdentityRevision(tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT person_id
			FROM (
				SELECT person_id
				FROM person_contact_state
				WHERE dirty_at IS NOT NULL
				   OR identity_revision <> ?
				   OR account_identity_revision <> ?
				UNION
				SELECT aep.person_id
				FROM activity_event_persons aep
				WHERE aep.evidence = 'direct'
				  AND NOT EXISTS (
					SELECT 1
					FROM person_contact_state pcs
					WHERE pcs.person_id = aep.person_id
				  )
			) stale
			ORDER BY person_id
			LIMIT ?
		`, identityRevision, accountRevision, limit)
		if err != nil {
			return fmt.Errorf("select stale contact state: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var personID int64
			if err := rows.Scan(&personID); err != nil {
				return fmt.Errorf("scan stale contact person: %w", err)
			}
			personIDs = append(personIDs, personID)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate stale contact people: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close stale contact people: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return personIDs, nil
}

// ProjectActivityBatchContext atomically validates exact candidate tokens,
// replaces/retracts the spine, repairs contact state, and acknowledges the
// observed queue generations.
func (s *Store) ProjectActivityBatchContext(
	ctx context.Context,
	items []ActivityProjection,
) (ActivityProjectionResult, error) {
	sorted, revisions, err := validateActivityProjectionBatch(items)
	if err != nil {
		return ActivityProjectionResult{}, err
	}
	if len(sorted) == 0 {
		return ActivityProjectionResult{}, nil
	}

	var lastErr error
	for range activityProjectionMaxAttempts {
		if err := ctx.Err(); err != nil {
			return ActivityProjectionResult{}, err
		}
		result, err := s.projectActivityBatchOnce(ctx, sorted, revisions)
		if err == nil {
			return result, nil
		}
		var stale *ErrActivityProjectionStale
		if errors.As(err, &stale) || !activityProjectionRetryable(s, err) {
			return ActivityProjectionResult{}, err
		}
		lastErr = err
	}
	return ActivityProjectionResult{}, fmt.Errorf(
		"project activity batch: gave up after %d attempts: %w",
		activityProjectionMaxAttempts, lastErr)
}

func validateActivityProjectionBatch(
	items []ActivityProjection,
) ([]ActivityProjection, ContactRevisions, error) {
	if len(items) == 0 {
		return nil, ContactRevisions{}, nil
	}
	sorted := append([]ActivityProjection(nil), items...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Token.MessageID < sorted[j].Token.MessageID
	})
	revisions := ContactRevisions{
		IdentityRevision:        sorted[0].Token.IdentityRevision,
		AccountIdentityRevision: sorted[0].Token.AccountIdentityRevision,
	}
	timezoneTransition := sorted[0].Token.TimezoneTransition
	directLimitTransition := sorted[0].Token.DirectLimitTransition
	for index := range sorted {
		item := &sorted[index]
		token := item.Token
		switch {
		case token.MessageID <= 0:
			return nil, ContactRevisions{}, fmt.Errorf(
				"%w: projection message id must be positive", ErrInvalidActivity)
		case token.SourceID <= 0:
			return nil, ContactRevisions{}, fmt.Errorf(
				"%w: projection source id must be positive", ErrInvalidActivity)
		case token.LastModified.IsZero():
			return nil, ContactRevisions{}, fmt.Errorf(
				"%w: projection last modified is required", ErrInvalidActivity)
		case token.IdentityRevision < 0 || token.AccountIdentityRevision < 0:
			return nil, ContactRevisions{}, fmt.Errorf(
				"%w: projection revisions must be non-negative", ErrInvalidActivity)
		case index > 0 && token.MessageID == sorted[index-1].Token.MessageID:
			return nil, ContactRevisions{}, fmt.Errorf(
				"%w: duplicate projection message id %d",
				ErrInvalidActivity, token.MessageID)
		case token.IdentityRevision != revisions.IdentityRevision ||
			token.AccountIdentityRevision != revisions.AccountIdentityRevision:
			return nil, ContactRevisions{}, fmt.Errorf(
				"%w: one batch must share one identity epoch", ErrInvalidActivity)
		case token.TimezoneTransition != timezoneTransition:
			return nil, ContactRevisions{}, fmt.Errorf(
				"%w: one batch must share one timezone transition",
				ErrInvalidActivity)
		case token.DirectLimitTransition != directLimitTransition:
			return nil, ContactRevisions{}, fmt.Errorf(
				"%w: one batch must share one direct-counterpart transition",
				ErrInvalidActivity)
		}
		if err := validateActivityQueueObservation(token.Queue); err != nil {
			return nil, ContactRevisions{}, err
		}
		if item.Event == nil {
			continue
		}
		if err := item.Event.Validate(); err != nil {
			return nil, ContactRevisions{}, err
		}
		if err := validateActivityEventToken(*item.Event, token); err != nil {
			return nil, ContactRevisions{}, err
		}
		event := *item.Event
		event.Persons = sortedActivityPersons(event.Persons)
		item.Event = &event
	}
	return sorted, revisions, nil
}

func validateActivityQueueObservation(observation ActivityQueueObservation) error {
	if !observation.Exists {
		if observation.Revision != 0 || observation.ProcessedRevision != 0 {
			return fmt.Errorf(
				"%w: absent queue observation must have zero revisions",
				ErrInvalidActivity)
		}
		return nil
	}
	if observation.Revision < 1 ||
		observation.ProcessedRevision < 0 ||
		observation.ProcessedRevision > observation.Revision {
		return fmt.Errorf("%w: invalid queue observation", ErrInvalidActivity)
	}
	return nil
}

func validateActivityEventToken(event ActivityEvent, token ActivityProjectionToken) error {
	switch {
	case event.MessageID != token.MessageID:
		return fmt.Errorf("%w: event message id does not match token", ErrInvalidActivity)
	case event.SourceID != token.SourceID:
		return fmt.Errorf("%w: event source id does not match token", ErrInvalidActivity)
	case !sameOptionalInt64(event.ConversationID, token.ConversationID):
		return fmt.Errorf("%w: event conversation id does not match token", ErrInvalidActivity)
	case !event.ProjectedLastModified.Equal(token.LastModified):
		return fmt.Errorf("%w: event last modified does not match token", ErrInvalidActivity)
	case event.ProjectedIdentityRevision != token.IdentityRevision ||
		event.ProjectedAccountIdentityRevision != token.AccountIdentityRevision:
		return fmt.Errorf("%w: event identity epoch does not match token", ErrInvalidActivity)
	}
	if token.TimezoneTransition.Target != "" {
		switch event.DatePrecision {
		case "timestamp":
			if event.Timezone != token.TimezoneTransition.Target {
				return fmt.Errorf(
					"%w: timestamp event timezone does not match transition target",
					ErrInvalidActivity)
			}
			location, err := time.LoadLocation(token.TimezoneTransition.Target)
			if err != nil {
				return fmt.Errorf(
					"%w: transition target timezone is invalid",
					ErrInvalidActivity)
			}
			local := event.OccurredAt.UTC().In(location)
			_, offsetSeconds := local.Zone()
			if event.UTCOffsetMinutes != offsetSeconds/60 ||
				event.LocalDate != local.Format(time.DateOnly) {
				return fmt.Errorf(
					"%w: timestamp event date does not match transition target",
					ErrInvalidActivity)
			}
		case "day":
			if event.Timezone != "UTC" || event.UTCOffsetMinutes != 0 ||
				event.LocalDate != event.OccurredAt.UTC().Format(time.DateOnly) {
				return fmt.Errorf(
					"%w: day event must remain UTC and calendar-stable",
					ErrInvalidActivity)
			}
		}
	}
	expectedRef := RefKindMessage
	expectedChannel := ChannelOther
	if IsMeetingMessageType(token.MessageType) {
		expectedRef = RefKindMeeting
		expectedChannel = ChannelMeeting
	} else {
		switch token.ConversationType {
		case "email_thread":
			expectedChannel = ChannelEmail
		case "group_chat", "direct_chat", "channel":
			expectedChannel = ChannelChat
		}
	}
	if event.RefKind != expectedRef || event.Channel != expectedChannel {
		return fmt.Errorf(
			"%w: event classification does not match message inputs",
			ErrInvalidActivity)
	}
	switch {
	case event.Direction == DirectionObserved &&
		(event.OwnerSourceID != nil || event.OwnerAddress != ""):
		return fmt.Errorf(
			"%w: observed event cannot carry an owner", ErrInvalidActivity)
	case event.Direction != DirectionObserved &&
		(event.OwnerSourceID == nil || *event.OwnerSourceID != token.SourceID):
		return fmt.Errorf(
			"%w: contact event owner source does not match message source",
			ErrInvalidActivity)
	}
	return nil
}

func sameOptionalInt64(left, right *int64) bool {
	switch {
	case left == nil && right == nil:
		return true
	case left == nil || right == nil:
		return false
	default:
		return *left == *right
	}
}

func sortedActivityPersons(persons []ActivityEventPerson) []ActivityEventPerson {
	result := append([]ActivityEventPerson(nil), persons...)
	sort.Slice(result, func(i, j int) bool {
		return result[i].PersonID < result[j].PersonID
	})
	return result
}

type activityProjectionAttempt struct {
	item       ActivityProjection
	old        *ActivityEvent
	exact      bool
	reserved   bool
	directOld  []int64
	directNext []int64
}

func (s *Store) projectActivityBatchOnce(
	ctx context.Context,
	items []ActivityProjection,
	revisions ContactRevisions,
) (ActivityProjectionResult, error) {
	result := ActivityProjectionResult{Processed: len(items)}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		currentIdentity, err := s.currentIdentityRevisionTx(tx)
		if err != nil {
			return err
		}
		currentAccount, err := readAccountIdentityRevision(tx)
		if err != nil {
			return err
		}
		if currentIdentity != revisions.IdentityRevision ||
			currentAccount != revisions.AccountIdentityRevision {
			return &ErrActivityProjectionStale{Reason: "identity epoch changed"}
		}
		if err := s.validateActivityTimezoneTransitionTx(
			ctx, tx, items[0].Token.TimezoneTransition); err != nil {
			return err
		}
		if err := s.validateActivityDirectLimitTransitionTx(
			ctx, tx, items[0].Token.DirectLimitTransition); err != nil {
			return err
		}

		attempts := make([]activityProjectionAttempt, len(items))
		for index, item := range items {
			if err := s.validateActivityMessageTx(ctx, tx, item.Token); err != nil {
				return err
			}
			attempts[index].item = item
		}
		for index := range attempts {
			reserved, err := s.validateActivityQueueTx(
				ctx, tx, attempts[index].item.Token)
			if err != nil {
				return err
			}
			attempts[index].reserved = reserved
		}

		recompute := make(map[int64]struct{})
		contactUpdated := make(map[int64]struct{})
		var additions []ActivityEvent
		for index := range attempts {
			attempt := &attempts[index]
			old, err := s.loadActivityEventTx(
				ctx, tx, attempt.item.Token.MessageID)
			if err != nil {
				return err
			}
			attempt.old = old
			attempt.exact = activityEventsEqual(old, attempt.item.Event)
			attempt.directOld = directActivityPersons(old)
			attempt.directNext = directActivityPersons(attempt.item.Event)
			if attempt.exact {
				continue
			}
			if old == nil && attempt.item.Event != nil {
				additions = append(additions, *attempt.item.Event)
				for _, personID := range attempt.directNext {
					contactUpdated[personID] = struct{}{}
				}
				continue
			}
			for _, personID := range attempt.directOld {
				recompute[personID] = struct{}{}
				contactUpdated[personID] = struct{}{}
			}
			for _, personID := range attempt.directNext {
				recompute[personID] = struct{}{}
				contactUpdated[personID] = struct{}{}
			}
		}
		recomputeIDs := sortedInt64Set(recompute)
		if err := s.lockActivityContactPersonsTx(ctx, tx, recomputeIDs); err != nil {
			return err
		}
		preexistingDirty, err := s.activityContactDirtyStateTx(ctx, tx, recompute)
		if err != nil {
			return err
		}
		reconciled := false
		if len(contactUpdated) > 0 {
			reconciled, err = s.activityEpochReconciledTx(ctx, tx, revisions)
			if err != nil {
				return err
			}
		}

		for index := range attempts {
			attempt := &attempts[index]
			if attempt.exact {
				continue
			}
			if attempt.item.Event == nil {
				if attempt.old != nil {
					if _, err := tx.ExecContext(ctx,
						`DELETE FROM activity_events WHERE message_id = ?`,
						attempt.item.Token.MessageID); err != nil {
						return fmt.Errorf("retract activity event %d: %w",
							attempt.item.Token.MessageID, err)
					}
					result.EventsRetracted++
				}
				continue
			}
			if err := s.replaceActivityEventTx(ctx, tx, *attempt.item.Event); err != nil {
				return err
			}
			result.EventsWritten++
		}

		for _, event := range additions {
			for _, link := range event.Persons {
				if link.Evidence != EvidenceDirect {
					continue
				}
				if err := s.applyContactAdditionTx(
					ctx, tx, link.PersonID, event, revisions, reconciled); err != nil {
					return err
				}
			}
		}

		for _, personID := range recomputeIDs {
			clearDirty := reconciled && !preexistingDirty[personID]
			if err := s.recomputeContactStateTx(
				ctx, tx, personID, revisions, clearDirty); err != nil {
				return err
			}
		}
		result.ContactPersons = len(contactUpdated)

		for index := range attempts {
			if err := s.acknowledgeActivityQueueTx(
				ctx, tx, attempts[index].item.Token, attempts[index].reserved); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) validateActivityTimezoneTransitionTx(
	ctx context.Context,
	tx *loggedTx,
	observed ActivityTimezoneTransition,
) error {
	current, err := s.activityTimezoneTransitionQueryContext(ctx, tx)
	if err != nil {
		return err
	}
	if current != observed {
		return &ErrActivityProjectionStale{Reason: "timezone transition changed"}
	}
	return nil
}

func (s *Store) validateActivityDirectLimitTransitionTx(
	ctx context.Context,
	tx *loggedTx,
	observed ActivityDirectLimitTransition,
) error {
	current, err := s.activityDirectLimitTransitionQueryContext(ctx, tx)
	if err != nil {
		return err
	}
	if current != observed {
		return &ErrActivityProjectionStale{
			Reason: "direct-counterpart transition changed",
		}
	}
	return nil
}

func (s *Store) validateActivityMessageTx(
	ctx context.Context,
	tx *loggedTx,
	token ActivityProjectionToken,
) error {
	var (
		sourceID       int64
		conversationID sql.NullInt64
		messageType    string
		lastModified   sql.NullTime
	)
	err := tx.QueryRowContext(ctx, `
		SELECT source_id, conversation_id, COALESCE(message_type, ''), last_modified
		FROM messages
		WHERE id = ?`+s.dialect.SelectForUpdate(),
		token.MessageID,
	).Scan(&sourceID, &conversationID, &messageType, &lastModified)
	if errors.Is(err, sql.ErrNoRows) {
		return &ErrActivityProjectionStale{
			MessageID: token.MessageID,
			Reason:    "message no longer exists",
		}
	}
	if err != nil {
		return fmt.Errorf("lock activity message %d: %w", token.MessageID, err)
	}
	if !lastModified.Valid ||
		sourceID != token.SourceID ||
		messageType != token.MessageType ||
		!lastModified.Time.Equal(token.LastModified) ||
		!sameOptionalInt64(nullInt64Pointer(conversationID), token.ConversationID) {
		return &ErrActivityProjectionStale{
			MessageID: token.MessageID,
			Reason:    "message inputs changed",
		}
	}
	conversationType := ""
	if conversationID.Valid {
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(conversation_type, '') FROM conversations WHERE id = ?`,
			conversationID.Int64).Scan(&conversationType)
		if errors.Is(err, sql.ErrNoRows) {
			return &ErrActivityProjectionStale{
				MessageID: token.MessageID,
				Reason:    "conversation no longer exists",
			}
		}
		if err != nil {
			return fmt.Errorf("read activity conversation %d: %w",
				conversationID.Int64, err)
		}
	}
	if conversationType != token.ConversationType {
		return &ErrActivityProjectionStale{
			MessageID: token.MessageID,
			Reason:    "conversation type changed",
		}
	}
	return nil
}

func nullInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func (s *Store) validateActivityQueueTx(
	ctx context.Context,
	tx *loggedTx,
	token ActivityProjectionToken,
) (bool, error) {
	var revision, processed int64
	err := tx.QueryRowContext(ctx, `
		SELECT revision, processed_revision
		FROM activity_projection_queue
		WHERE message_id = ?`+s.dialect.SelectForUpdate(),
		token.MessageID,
	).Scan(&revision, &processed)
	if errors.Is(err, sql.ErrNoRows) {
		if token.Queue.Exists {
			return false, &ErrActivityProjectionStale{
				MessageID: token.MessageID,
				Reason:    "queue observation disappeared",
			}
		}
		statement := s.dialect.InsertOrIgnore(`
			INSERT OR IGNORE INTO activity_projection_queue
				(message_id, revision, processed_revision)
			VALUES (?, 1, 0)`)
		result, err := tx.ExecContext(ctx, statement, token.MessageID)
		if err != nil {
			return false, fmt.Errorf(
				"reserve activity queue for message %d: %w", token.MessageID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("inspect activity queue reservation: %w", err)
		}
		if affected != 1 {
			return false, &ErrActivityProjectionStale{
				MessageID: token.MessageID,
				Reason:    "legacy queue reservation lost a race",
			}
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock activity queue for message %d: %w",
			token.MessageID, err)
	}
	if !token.Queue.Exists ||
		revision != token.Queue.Revision ||
		processed != token.Queue.ProcessedRevision {
		return false, &ErrActivityProjectionStale{
			MessageID: token.MessageID,
			Reason:    "queue generation changed",
		}
	}
	return false, nil
}

func (s *Store) acknowledgeActivityQueueTx(
	ctx context.Context,
	tx *loggedTx,
	token ActivityProjectionToken,
	reserved bool,
) error {
	revision := token.Queue.Revision
	processed := token.Queue.ProcessedRevision
	if reserved {
		revision = 1
		processed = 0
	}
	if revision == processed {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE activity_projection_queue
		SET processed_revision = ?
		WHERE message_id = ?
		  AND revision = ?
		  AND processed_revision = ?
	`, revision, token.MessageID, revision, processed)
	if err != nil {
		return fmt.Errorf("acknowledge activity queue for message %d: %w",
			token.MessageID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect activity queue acknowledgement: %w", err)
	}
	if affected != 1 {
		return &ErrActivityProjectionStale{
			MessageID: token.MessageID,
			Reason:    "queue acknowledgement lost compare-and-swap",
		}
	}
	return nil
}

func (s *Store) loadActivityEventTx(
	ctx context.Context,
	tx *loggedTx,
	messageID int64,
) (*ActivityEvent, error) {
	var (
		event          ActivityEvent
		refKind        string
		channel        string
		direction      string
		conversationID sql.NullInt64
		ownerSourceID  sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT message_id, ref_kind, source_id, conversation_id, channel,
		       occurred_at, date_origin, date_precision, timezone,
		       utc_offset_minutes, local_date, direction, owner_source_id,
		       owner_address, projected_last_modified,
		       projected_identity_revision, projected_account_identity_revision
		FROM activity_events
		WHERE message_id = ?
	`, messageID).Scan(
		&event.MessageID, &refKind, &event.SourceID, &conversationID, &channel,
		&event.OccurredAt, &event.DateOrigin, &event.DatePrecision,
		&event.Timezone, &event.UTCOffsetMinutes, &event.LocalDate, &direction,
		&ownerSourceID, &event.OwnerAddress, &event.ProjectedLastModified,
		&event.ProjectedIdentityRevision,
		&event.ProjectedAccountIdentityRevision,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // Absence means this projection is an insertion.
	}
	if err != nil {
		return nil, fmt.Errorf("read activity event %d: %w", messageID, err)
	}
	event.RefKind = ActivityRefKind(refKind)
	event.Channel = ActivityChannel(channel)
	event.Direction = ActivityDirection(direction)
	event.ConversationID = nullInt64Pointer(conversationID)
	event.OwnerSourceID = nullInt64Pointer(ownerSourceID)

	rows, err := tx.QueryContext(ctx, `
		SELECT person_id, role, evidence
		FROM activity_event_persons
		WHERE message_id = ?
		ORDER BY person_id
	`, messageID)
	if err != nil {
		return nil, fmt.Errorf("read activity links %d: %w", messageID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var link ActivityEventPerson
		var role, evidence string
		if err := rows.Scan(&link.PersonID, &role, &evidence); err != nil {
			return nil, fmt.Errorf("scan activity link %d: %w", messageID, err)
		}
		link.Role = ActivityRole(role)
		link.Evidence = ActivityEvidence(evidence)
		event.Persons = append(event.Persons, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity links %d: %w", messageID, err)
	}
	return &event, nil
}

func activityEventsEqual(left, right *ActivityEvent) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if left.MessageID != right.MessageID ||
		left.RefKind != right.RefKind ||
		left.SourceID != right.SourceID ||
		!sameOptionalInt64(left.ConversationID, right.ConversationID) ||
		left.Channel != right.Channel ||
		!left.OccurredAt.Equal(right.OccurredAt) ||
		left.DateOrigin != right.DateOrigin ||
		left.DatePrecision != right.DatePrecision ||
		left.Timezone != right.Timezone ||
		left.UTCOffsetMinutes != right.UTCOffsetMinutes ||
		left.LocalDate != right.LocalDate ||
		left.Direction != right.Direction ||
		!sameOptionalInt64(left.OwnerSourceID, right.OwnerSourceID) ||
		left.OwnerAddress != right.OwnerAddress ||
		!left.ProjectedLastModified.Equal(right.ProjectedLastModified) ||
		left.ProjectedIdentityRevision != right.ProjectedIdentityRevision ||
		left.ProjectedAccountIdentityRevision !=
			right.ProjectedAccountIdentityRevision {
		return false
	}
	leftPersons := sortedActivityPersons(left.Persons)
	rightPersons := sortedActivityPersons(right.Persons)
	if len(leftPersons) != len(rightPersons) {
		return false
	}
	for index := range leftPersons {
		if leftPersons[index] != rightPersons[index] {
			return false
		}
	}
	return true
}

func directActivityPersons(event *ActivityEvent) []int64 {
	if event == nil {
		return nil
	}
	var result []int64
	for _, link := range event.Persons {
		if link.Evidence == EvidenceDirect {
			result = append(result, link.PersonID)
		}
	}
	slices.Sort(result)
	return result
}

func (s *Store) replaceActivityEventTx(
	ctx context.Context,
	tx *loggedTx,
	event ActivityEvent,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO activity_events (
			message_id, ref_kind, source_id, conversation_id, channel,
			occurred_at, date_origin, date_precision, timezone,
			utc_offset_minutes, local_date, direction, owner_source_id,
			owner_address, projected_last_modified,
			projected_identity_revision, projected_account_identity_revision
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			ref_kind = excluded.ref_kind,
			source_id = excluded.source_id,
			conversation_id = excluded.conversation_id,
			channel = excluded.channel,
			occurred_at = excluded.occurred_at,
			date_origin = excluded.date_origin,
			date_precision = excluded.date_precision,
			timezone = excluded.timezone,
			utc_offset_minutes = excluded.utc_offset_minutes,
			local_date = excluded.local_date,
			direction = excluded.direction,
			owner_source_id = excluded.owner_source_id,
			owner_address = excluded.owner_address,
			projected_last_modified = excluded.projected_last_modified,
			projected_identity_revision = excluded.projected_identity_revision,
			projected_account_identity_revision =
				excluded.projected_account_identity_revision
	`,
		event.MessageID, string(event.RefKind), event.SourceID,
		event.ConversationID, string(event.Channel), event.OccurredAt.UTC(),
		event.DateOrigin, event.DatePrecision, event.Timezone,
		event.UTCOffsetMinutes, event.LocalDate, string(event.Direction),
		event.OwnerSourceID, event.OwnerAddress,
		event.ProjectedLastModified.UTC(), event.ProjectedIdentityRevision,
		event.ProjectedAccountIdentityRevision,
	)
	if err != nil {
		return fmt.Errorf("write activity event %d: %w", event.MessageID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM activity_event_persons WHERE message_id = ?`,
		event.MessageID); err != nil {
		return fmt.Errorf("replace activity links %d: %w", event.MessageID, err)
	}
	for _, link := range event.Persons {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO activity_event_persons
				(message_id, person_id, role, evidence, local_date)
			VALUES (?, ?, ?, ?, ?)
		`, event.MessageID, link.PersonID, string(link.Role),
			string(link.Evidence), event.LocalDate); err != nil {
			return fmt.Errorf("write activity link for message %d person %d: %w",
				event.MessageID, link.PersonID, err)
		}
	}
	return nil
}

func sortedInt64Set(values map[int64]struct{}) []int64 {
	result := make([]int64, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

// lockActivityContactPersonsTx serializes recompute against cascade deletion
// of any other direct event for the same people. Evidence rows are locked in
// their primary-key order before contact rows are locked in person order. A
// deleting transaction that reached the evidence row first must commit before
// this returns, so subsequent evidence reads see the deletion. If this
// transaction gets the evidence row first, the delete trigger runs only after
// contact state commits and therefore leaves the row dirty.
//
// Missing contact-state rows need no placeholder: every existing direct
// evidence row is already locked. A concurrent delete cannot pass that lock
// and miss a later contact-state insert.
//
// Person IDs are chunked to stay within backend bound-variable limits (a
// maximum projection batch can carry tens of thousands of distinct people).
// Chunking preserves the protocol: every evidence lock is still acquired
// before any contact lock, chunks ascend over the sorted person set, and
// protocol followers are already serialized by the identity mutation lock,
// so no two of them interleave chunk acquisition.
const activityContactLockChunk = 500

func (s *Store) lockActivityContactPersonsTx(
	ctx context.Context,
	tx *loggedTx,
	personIDs []int64,
) error {
	if len(personIDs) == 0 {
		return nil
	}
	sorted := slices.Clone(personIDs)
	slices.Sort(sorted)

	for start := 0; start < len(sorted); start += activityContactLockChunk {
		chunk := sorted[start:min(start+activityContactLockChunk, len(sorted))]
		if err := s.lockRowsTx(ctx, tx, `
			SELECT message_id, person_id
			FROM activity_event_persons
			WHERE evidence = 'direct'
			  AND person_id IN (`+int64Placeholders(len(chunk))+`)
			ORDER BY message_id, person_id`,
			"activity contact evidence", 2, chunk); err != nil {
			return err
		}
	}
	for start := 0; start < len(sorted); start += activityContactLockChunk {
		chunk := sorted[start:min(start+activityContactLockChunk, len(sorted))]
		if err := s.lockRowsTx(ctx, tx, `
			SELECT person_id
			FROM person_contact_state
			WHERE person_id IN (`+int64Placeholders(len(chunk))+`)
			ORDER BY person_id`,
			"activity contact state", 1, chunk); err != nil {
			return err
		}
	}
	return nil
}

func int64Placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// lockRowsTx runs query with SELECT FOR UPDATE appended and drains the
// result, acquiring row locks without keeping any values.
func (s *Store) lockRowsTx(
	ctx context.Context,
	tx *loggedTx,
	query, label string,
	columns int,
	ids []int64,
) error {
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	rows, err := tx.QueryContext(ctx, query+s.dialect.SelectForUpdate(), args...)
	if err != nil {
		return fmt.Errorf("lock %s: %w", label, err)
	}
	scanned := make([]int64, columns)
	targets := make([]any, columns)
	for index := range scanned {
		targets[index] = &scanned[index]
	}
	for rows.Next() {
		if err := rows.Scan(targets...); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan locked %s: %w", label, err)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate locked %s: %w", label, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close locked %s: %w", label, err)
	}
	return nil
}

func (s *Store) activityContactDirtyStateTx(
	ctx context.Context,
	tx *loggedTx,
	personIDs map[int64]struct{},
) (map[int64]bool, error) {
	result := make(map[int64]bool, len(personIDs))
	for _, personID := range sortedInt64Set(personIDs) {
		var dirty sql.NullTime
		err := tx.QueryRowContext(ctx,
			`SELECT dirty_at FROM person_contact_state WHERE person_id = ?`,
			personID).Scan(&dirty)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read contact dirtiness for person %d: %w",
				personID, err)
		}
		result[personID] = dirty.Valid
	}
	return result, nil
}

func (s *Store) activityEpochReconciledTx(
	ctx context.Context,
	tx *loggedTx,
	revisions ContactRevisions,
) (bool, error) {
	identity, identityOK, err := activityMetadataRevisionTx(
		ctx, tx, activityReconciledIdentityRevisionKey)
	if err != nil {
		return false, err
	}
	account, accountOK, err := activityMetadataRevisionTx(
		ctx, tx, activityReconciledAccountRevisionKey)
	if err != nil {
		return false, err
	}
	return identityOK && accountOK &&
		identity == revisions.IdentityRevision &&
		account == revisions.AccountIdentityRevision, nil
}

func activityMetadataRevisionTx(
	ctx context.Context,
	tx *loggedTx,
	key string,
) (int64, bool, error) {
	var value string
	err := tx.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read archive metadata %q: %w", key, err)
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 0 {
		return 0, false, fmt.Errorf("parse archive metadata %q revision %q",
			key, value)
	}
	return revision, true, nil
}

func (s *Store) applyContactAdditionTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	event ActivityEvent,
	revisions ContactRevisions,
	reconciled bool,
) error {
	var inboundAt, outboundAt *time.Time
	var inboundID, outboundID *int64
	occurredAt := event.OccurredAt.UTC()
	messageID := event.MessageID
	switch event.Direction {
	case DirectionInbound:
		inboundAt, inboundID = &occurredAt, &messageID
	case DirectionOutbound:
		outboundAt, outboundID = &occurredAt, &messageID
	case DirectionObserved:
	}
	var sourceID *int64
	if event.OwnerSourceID != nil {
		value := *event.OwnerSourceID
		sourceID = &value
	}
	insertIdentity, insertAccount := int64(0), int64(0)
	var dirtyAt *time.Time
	if reconciled {
		insertIdentity = revisions.IdentityRevision
		insertAccount = revisions.AccountIdentityRevision
	} else {
		now := time.Now().UTC()
		dirtyAt = &now
	}
	statement := `
		INSERT INTO person_contact_state (
			person_id,
			first_contact_at, first_contact_message_id,
			last_contact_at, last_contact_message_id, last_contact_channel,
			last_contact_source_id, last_contact_owner,
			last_inbound_at, last_inbound_message_id,
			last_outbound_at, last_outbound_message_id,
			interaction_count, identity_revision, account_identity_revision,
			dirty_at, computed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(person_id) DO UPDATE SET
			first_contact_at = CASE WHEN
				person_contact_state.first_contact_at IS NULL
				OR excluded.first_contact_at < person_contact_state.first_contact_at
				OR (excluded.first_contact_at = person_contact_state.first_contact_at
				    AND excluded.first_contact_message_id <
				        person_contact_state.first_contact_message_id)
				THEN excluded.first_contact_at
				ELSE person_contact_state.first_contact_at END,
			first_contact_message_id = CASE WHEN
				person_contact_state.first_contact_at IS NULL
				OR excluded.first_contact_at < person_contact_state.first_contact_at
				OR (excluded.first_contact_at = person_contact_state.first_contact_at
				    AND excluded.first_contact_message_id <
				        person_contact_state.first_contact_message_id)
				THEN excluded.first_contact_message_id
				ELSE person_contact_state.first_contact_message_id END,
			last_contact_at = CASE WHEN
				person_contact_state.last_contact_at IS NULL
				OR excluded.last_contact_at > person_contact_state.last_contact_at
				OR (excluded.last_contact_at = person_contact_state.last_contact_at
				    AND excluded.last_contact_message_id >
				        person_contact_state.last_contact_message_id)
				THEN excluded.last_contact_at
				ELSE person_contact_state.last_contact_at END,
			last_contact_message_id = CASE WHEN
				person_contact_state.last_contact_at IS NULL
				OR excluded.last_contact_at > person_contact_state.last_contact_at
				OR (excluded.last_contact_at = person_contact_state.last_contact_at
				    AND excluded.last_contact_message_id >
				        person_contact_state.last_contact_message_id)
				THEN excluded.last_contact_message_id
				ELSE person_contact_state.last_contact_message_id END,
			last_contact_channel = CASE WHEN
				person_contact_state.last_contact_at IS NULL
				OR excluded.last_contact_at > person_contact_state.last_contact_at
				OR (excluded.last_contact_at = person_contact_state.last_contact_at
				    AND excluded.last_contact_message_id >
				        person_contact_state.last_contact_message_id)
				THEN excluded.last_contact_channel
				ELSE person_contact_state.last_contact_channel END,
			last_contact_source_id = CASE WHEN
				person_contact_state.last_contact_at IS NULL
				OR excluded.last_contact_at > person_contact_state.last_contact_at
				OR (excluded.last_contact_at = person_contact_state.last_contact_at
				    AND excluded.last_contact_message_id >
				        person_contact_state.last_contact_message_id)
				THEN excluded.last_contact_source_id
				ELSE person_contact_state.last_contact_source_id END,
			last_contact_owner = CASE WHEN
				person_contact_state.last_contact_at IS NULL
				OR excluded.last_contact_at > person_contact_state.last_contact_at
				OR (excluded.last_contact_at = person_contact_state.last_contact_at
				    AND excluded.last_contact_message_id >
				        person_contact_state.last_contact_message_id)
				THEN excluded.last_contact_owner
				ELSE person_contact_state.last_contact_owner END,
			last_inbound_at = CASE WHEN excluded.last_inbound_at IS NOT NULL
				AND (person_contact_state.last_inbound_at IS NULL
				     OR excluded.last_inbound_at > person_contact_state.last_inbound_at
				     OR (excluded.last_inbound_at =
				         person_contact_state.last_inbound_at
				         AND excluded.last_inbound_message_id >
				         person_contact_state.last_inbound_message_id))
				THEN excluded.last_inbound_at
				ELSE person_contact_state.last_inbound_at END,
			last_inbound_message_id = CASE WHEN excluded.last_inbound_at IS NOT NULL
				AND (person_contact_state.last_inbound_at IS NULL
				     OR excluded.last_inbound_at > person_contact_state.last_inbound_at
				     OR (excluded.last_inbound_at =
				         person_contact_state.last_inbound_at
				         AND excluded.last_inbound_message_id >
				         person_contact_state.last_inbound_message_id))
				THEN excluded.last_inbound_message_id
				ELSE person_contact_state.last_inbound_message_id END,
			last_outbound_at = CASE WHEN excluded.last_outbound_at IS NOT NULL
				AND (person_contact_state.last_outbound_at IS NULL
				     OR excluded.last_outbound_at > person_contact_state.last_outbound_at
				     OR (excluded.last_outbound_at =
				         person_contact_state.last_outbound_at
				         AND excluded.last_outbound_message_id >
				         person_contact_state.last_outbound_message_id))
				THEN excluded.last_outbound_at
				ELSE person_contact_state.last_outbound_at END,
			last_outbound_message_id = CASE WHEN excluded.last_outbound_at IS NOT NULL
				AND (person_contact_state.last_outbound_at IS NULL
				     OR excluded.last_outbound_at > person_contact_state.last_outbound_at
				     OR (excluded.last_outbound_at =
				         person_contact_state.last_outbound_at
				         AND excluded.last_outbound_message_id >
				         person_contact_state.last_outbound_message_id))
				THEN excluded.last_outbound_message_id
				ELSE person_contact_state.last_outbound_message_id END,
			interaction_count = person_contact_state.interaction_count + 1,
			dirty_at = COALESCE(person_contact_state.dirty_at, excluded.dirty_at),
			computed_at = excluded.computed_at
	`
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, statement,
		personID,
		occurredAt, event.MessageID,
		occurredAt, event.MessageID, string(event.Channel),
		sourceID, event.OwnerAddress,
		inboundAt, inboundID,
		outboundAt, outboundID,
		insertIdentity, insertAccount, dirtyAt, now,
	); err != nil {
		return fmt.Errorf("apply contact addition for person %d: %w", personID, err)
	}
	return nil
}

// RecomputeContactStateContext authoritatively rebuilds selected people from
// the spine. A reconciled same-epoch pass clears dirtiness; otherwise the
// rebuilt facts remain explicitly stale.
func (s *Store) RecomputeContactStateContext(
	ctx context.Context,
	personIDs []int64,
	revisions ContactRevisions,
) error {
	if len(personIDs) == 0 {
		return nil
	}
	unique := make(map[int64]struct{}, len(personIDs))
	for _, personID := range personIDs {
		if personID <= 0 {
			return fmt.Errorf("%w: contact person id must be positive",
				ErrInvalidActivity)
		}
		unique[personID] = struct{}{}
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		identity, err := s.currentIdentityRevisionTx(tx)
		if err != nil {
			return err
		}
		account, err := readAccountIdentityRevision(tx)
		if err != nil {
			return err
		}
		if identity != revisions.IdentityRevision ||
			account != revisions.AccountIdentityRevision {
			return &ErrActivityProjectionStale{Reason: "identity epoch changed"}
		}
		if err := s.lockActivityProjectionQueueFreshnessTx(ctx, tx); err != nil {
			return err
		}
		var pending int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM activity_projection_queue
			WHERE revision > processed_revision
		`).Scan(&pending); err != nil {
			return fmt.Errorf("count pending activity projections: %w", err)
		}
		if pending > 0 {
			return &ErrActivityProjectionStale{
				Reason: "activity projection queue is not drained",
			}
		}
		personIDs := sortedInt64Set(unique)
		if err := s.lockActivityContactPersonsTx(ctx, tx, personIDs); err != nil {
			return err
		}
		reconciled, err := s.activityEpochReconciledTx(ctx, tx, revisions)
		if err != nil {
			return err
		}
		for _, personID := range personIDs {
			if err := s.recomputeContactStateTx(
				ctx, tx, personID, revisions, reconciled); err != nil {
				return err
			}
		}
		return nil
	})
}

// lockActivityProjectionQueueFreshnessTx closes PostgreSQL's READ COMMITTED
// phantom window between observing an empty projection queue and publishing
// fresh contact state. Trigger inserts take ROW EXCLUSIVE and therefore wait
// behind this SHARE lock. The identity-mutation row must always be locked
// first, matching BeginExclusive's global PostgreSQL lock order.
//
// SQLite writer transactions already serialize the queue observation and
// contact-state write, so no extra statement is required there.
func (s *Store) lockActivityProjectionQueueFreshnessTx(
	ctx context.Context,
	tx *loggedTx,
) error {
	if !s.IsPostgreSQL() {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`LOCK TABLE activity_projection_queue IN SHARE MODE`); err != nil {
		return fmt.Errorf("lock activity projection queue freshness: %w", err)
	}
	return nil
}

type contactEvidenceRow struct {
	occurredAt time.Time
	messageID  int64
	channel    ActivityChannel
	direction  ActivityDirection
	sourceID   *int64
	owner      string
}

func (s *Store) recomputeContactStateTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	revisions ContactRevisions,
	clearDirty bool,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT ae.occurred_at, ae.message_id, ae.channel, ae.direction,
		       ae.owner_source_id, ae.owner_address
		FROM activity_events ae
		WHERE EXISTS (
			SELECT 1
			FROM activity_event_persons aep
			WHERE aep.message_id = ae.message_id
			  AND aep.person_id = ?
			  AND aep.evidence = 'direct'
		)
		ORDER BY ae.occurred_at, ae.message_id
	`, personID)
	if err != nil {
		return fmt.Errorf("load contact evidence for person %d: %w", personID, err)
	}
	var evidence []contactEvidenceRow
	for rows.Next() {
		var row contactEvidenceRow
		var channel, direction string
		var sourceID sql.NullInt64
		if err := rows.Scan(
			&row.occurredAt, &row.messageID, &channel, &direction,
			&sourceID, &row.owner,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan contact evidence for person %d: %w",
				personID, err)
		}
		row.channel = ActivityChannel(channel)
		row.direction = ActivityDirection(direction)
		row.sourceID = nullInt64Pointer(sourceID)
		evidence = append(evidence, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate contact evidence for person %d: %w",
			personID, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close contact evidence for person %d: %w",
			personID, err)
	}

	var first, last, inbound, outbound *contactEvidenceRow
	if len(evidence) > 0 {
		first = &evidence[0]
		last = &evidence[len(evidence)-1]
		for index := range evidence {
			switch evidence[index].direction {
			case DirectionInbound:
				inbound = &evidence[index]
			case DirectionOutbound:
				outbound = &evidence[index]
			case DirectionObserved:
			}
		}
	}
	return s.writeRecomputedContactStateTx(
		ctx, tx, personID, revisions, clearDirty,
		first, last, inbound, outbound, int64(len(evidence)))
}

func (s *Store) writeRecomputedContactStateTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	revisions ContactRevisions,
	clearDirty bool,
	first, last, inbound, outbound *contactEvidenceRow,
	count int64,
) error {
	var (
		firstAt, lastAt, inboundAt, outboundAt *time.Time
		firstID, lastID, inboundID, outboundID *int64
		lastChannel                            *string
		lastSource                             *int64
		lastOwner                              *string
	)
	if first != nil {
		firstAt, firstID = &first.occurredAt, &first.messageID
	}
	if last != nil {
		lastAt, lastID = &last.occurredAt, &last.messageID
		channel := string(last.channel)
		lastChannel = &channel
		lastSource = last.sourceID
		lastOwner = &last.owner
	}
	if inbound != nil {
		inboundAt, inboundID = &inbound.occurredAt, &inbound.messageID
	}
	if outbound != nil {
		outboundAt, outboundID = &outbound.occurredAt, &outbound.messageID
	}
	now := time.Now().UTC()
	insertIdentity, insertAccount := int64(0), int64(0)
	var dirtyAt *time.Time
	var updateEpoch string
	if clearDirty {
		insertIdentity = revisions.IdentityRevision
		insertAccount = revisions.AccountIdentityRevision
		updateEpoch = `
			identity_revision = excluded.identity_revision,
			account_identity_revision = excluded.account_identity_revision,
			dirty_at = NULL,`
	} else {
		dirtyAt = &now
		updateEpoch = `
			dirty_at = COALESCE(person_contact_state.dirty_at, excluded.dirty_at),`
	}
	statement := `
		INSERT INTO person_contact_state (
			person_id,
			first_contact_at, first_contact_message_id,
			last_contact_at, last_contact_message_id, last_contact_channel,
			last_contact_source_id, last_contact_owner,
			last_inbound_at, last_inbound_message_id,
			last_outbound_at, last_outbound_message_id,
			interaction_count, identity_revision, account_identity_revision,
			dirty_at, computed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(person_id) DO UPDATE SET
			first_contact_at = excluded.first_contact_at,
			first_contact_message_id = excluded.first_contact_message_id,
			last_contact_at = excluded.last_contact_at,
			last_contact_message_id = excluded.last_contact_message_id,
			last_contact_channel = excluded.last_contact_channel,
			last_contact_source_id = excluded.last_contact_source_id,
			last_contact_owner = excluded.last_contact_owner,
			last_inbound_at = excluded.last_inbound_at,
			last_inbound_message_id = excluded.last_inbound_message_id,
			last_outbound_at = excluded.last_outbound_at,
			last_outbound_message_id = excluded.last_outbound_message_id,
			interaction_count = excluded.interaction_count,` +
		updateEpoch + `
			computed_at = excluded.computed_at`
	if _, err := tx.ExecContext(ctx, statement,
		personID,
		firstAt, firstID,
		lastAt, lastID, lastChannel, lastSource, lastOwner,
		inboundAt, inboundID,
		outboundAt, outboundID,
		count, insertIdentity, insertAccount, dirtyAt, now,
	); err != nil {
		return fmt.Errorf("write contact state for person %d: %w", personID, err)
	}
	return nil
}

// ContactStateContext reads the stored state and reports identity, queue,
// reconciliation, and dirty staleness. The now argument is consumed by Task 8
// cadence evaluation.
func (s *Store) ContactStateContext(
	ctx context.Context,
	personID int64,
	now time.Time,
) (ContactState, error) {
	if personID <= 0 {
		return ContactState{}, ErrInvalidActivityRequest
	}
	var state ContactState
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		state, err = s.contactStateTxContext(ctx, tx, personID, now)
		return err
	})
	if err != nil {
		return ContactState{}, err
	}
	return state, nil
}

func activityStoredRef(kind sql.NullString, value sql.NullInt64) string {
	if !value.Valid {
		return ""
	}
	refKind := string(RefKindMessage)
	if kind.Valid && ActivityRefKind(kind.String).Valid() {
		refKind = kind.String
	}
	return refKind + ":" + strconv.FormatInt(value.Int64, 10)
}

func activityProjectionRetryable(s *Store, err error) bool {
	if s.dialect.IsBusyError(err) {
		return true
	}
	var sqlState sqlStateError
	return errors.As(err, &sqlState) && sqlState.SQLState() == "40001"
}
