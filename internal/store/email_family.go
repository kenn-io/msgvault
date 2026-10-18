package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/emailfamily"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/sourcescope"
)

// ErrNotFound hides missing and inaccessible email family seeds alike.
var ErrNotFound = errors.New("email family message not found")

const emailFamilyMemberLimit = 500
const emailFamilyKeyLimit = 5000

// EmailFamilyOptions narrows the caller's authorized sources. Nil adds no filter.
type EmailFamilyOptions struct{ SourceIDs []int64 }

// EmailFamilyMember preserves each archived copy and the facts used to evaluate it.
type EmailFamilyMember struct {
	emailfamily.Activity

	SourceMessageID  string                `json:"source_message_id"`
	EvidenceKinds    []string              `json:"evidence_kinds"`
	ConversationID   int64                 `json:"conversation_id"`
	SourceIdentifier string                `json:"source_identifier"`
	AccountAddress   string                `json:"account_address"`
	AccountPath      string                `json:"account_path"`
	Subject          string                `json:"subject"`
	Snippet          string                `json:"snippet"`
	SentAt           *time.Time            `json:"sent_at,omitempty"`
	ReceivedAt       *time.Time            `json:"received_at,omitempty"`
	InternalDate     *time.Time            `json:"internal_date,omitempty"`
	Labels           []string              `json:"labels"`
	RawSHA256        string                `json:"raw_sha256"`
	Envelopes        []EmailFamilyEnvelope `json:"envelopes"`
}

type EmailFamilyEnvelope struct {
	Kind    string `json:"kind"`
	Address string `json:"address"`
}

type EmailFamily struct {
	AnchorMessageID int64                 `json:"anchor_message_id"`
	Members         []EmailFamilyMember   `json:"members"`
	Handling        emailfamily.Handling  `json:"handling"`
	Truncated       bool                  `json:"truncated"`
	Revision        string                `json:"revision"`
	Decisions       []EmailFamilyDecision `json:"decisions"`
}

func emailFamilyScope(ctx context.Context, ids []int64) context.Context {
	if ids == nil {
		return ctx
	}
	if allowed := sourcescope.IDs(ctx); allowed != nil {
		narrowed := make([]int64, 0, len(ids))
		for _, id := range ids {
			if slices.Contains(allowed, id) {
				narrowed = append(narrowed, id)
			}
		}
		ids = narrowed
	}
	return sourcescope.WithSourceIDs(ctx, ids)
}

// EmailFamilyContext resolves live visible copies using indexed explicit keys in
// one repeatable snapshot. Reads never repair data or fetch provider content.
func (s *Store) EmailFamilyContext(ctx context.Context, id int64, options EmailFamilyOptions) (*EmailFamily, error) {
	ctx = emailFamilyScope(ctx, options.SourceIDs)
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.emailFamilySnapshot(ctx, id)
}

func (s *Store) emailFamilySnapshot(ctx context.Context, id int64) (*EmailFamily, error) {
	return s.emailFamilySnapshotIDs(ctx, []int64{id}, nil)
}

func (s *Store) emailFamilySnapshotIDs(ctx context.Context, ids []int64, projected *EmailFamilyDecision) (*EmailFamily, error) {
	if err := s.authorizeEmailFamilyEndpoints(ctx, ids, true); err != nil {
		return nil, err
	}
	result := &EmailFamily{AnchorMessageID: ids[0], Members: []EmailFamilyMember{}, Decisions: []EmailFamilyDecision{}}
	pending := slices.Clone(ids)
	decisionSeen := map[int64]bool{}
	handled := map[int64]bool{}
	edgeCount := 0
	if projected != nil {
		result.Decisions = append(result.Decisions, *projected)
		if projected.State == "active" {
			edgeCount += len(projected.MessageIDs)
			if edgeCount > emailFamilyDecisionEdgeLimit {
				result.Truncated = true
			}
		}
		if projected.Kind == "handled" {
			for _, id := range projected.MessageIDs {
				handled[id] = true
			}
		}
	}
	seen := map[int64]bool{}
	keys := map[string]bool{}
	scope, args := sourceScopeClause(ctx, "m.source_id")
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		if seen[current] {
			continue
		}
		if len(seen) == emailFamilyMemberLimit {
			result.Truncated = true
			break
		}
		member, err := s.emailFamilyMember(ctx, current, scope, args)
		if errors.Is(err, ErrNotFound) && !slices.Contains(ids, current) {
			continue
		}
		if err != nil {
			return nil, err
		}
		seen[current] = true
		result.Members = append(result.Members, *member)
		values := []string{member.Linkage.MessageID}
		values = append(values, member.Linkage.Replies...)
		values = append(values, member.Linkage.References...)
		values = append(values, member.Linkage.AttachedOriginals...)
		if member.Linkage.Version != mime.EmailLinkageVersion {
			values = nil
		}
		for _, key := range values {
			if key == "" || keys[key] {
				continue
			}
			if len(keys) == emailFamilyKeyLimit {
				result.Truncated = true
				break
			}
			keys[key] = true
			queryArgs := append([]any{mime.EmailLinkageVersion, key}, args...)
			rows, err := s.db.QueryContext(ctx, `SELECT m.id FROM messages m
    WHERE EXISTS (SELECT 1 FROM email_link_key_state e WHERE e.message_id=m.id AND e.extraction_version=?)
      AND EXISTS (SELECT 1 FROM email_link_keys k WHERE k.message_id=m.id AND k.value=?)
      AND m.deleted_at IS NULL AND COALESCE(m.message_type,'') IN ('','email')`+scope+` ORDER BY m.id LIMIT 501`, queryArgs...)
			if err != nil {
				return nil, err
			}
			count := 0
			for rows.Next() {
				var next int64
				if err := rows.Scan(&next); err != nil {
					_ = rows.Close()
					return nil, err
				}
				count++
				if !seen[next] {
					pending = append(pending, next)
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
			if count > emailFamilyMemberLimit {
				result.Truncated = true
			}
		}
		if member.SourceType == "gmail" && member.ProviderThreadID != "" {
			queryArgs := append([]any{member.SourceID, member.ProviderThreadID}, args...)
			rows, err := s.db.QueryContext(ctx, `SELECT m.id FROM messages m JOIN conversations c ON c.id=m.conversation_id
    WHERE m.source_id=? AND c.source_conversation_id=? AND m.deleted_at IS NULL AND COALESCE(m.message_type,'') IN ('','email')`+scope+` ORDER BY m.id LIMIT 501`, queryArgs...)
			if err != nil {
				return nil, err
			}
			count := 0
			for rows.Next() {
				var next int64
				if err := rows.Scan(&next); err != nil {
					_ = rows.Close()
					return nil, err
				}
				count++
				if !seen[next] {
					pending = append(pending, next)
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
			if count > emailFamilyMemberLimit {
				result.Truncated = true
			}
		}
		var decisions []EmailFamilyDecision
		var count int
		var truncated bool
		if edgeCount <= emailFamilyDecisionEdgeLimit {
			decisions, count, truncated, err = s.emailFamilyEdges(ctx, current, decisionSeen, emailFamilyDecisionEdgeLimit-edgeCount)
		}
		if err != nil {
			return nil, err
		}
		edgeCount += count
		if truncated {
			result.Truncated = true
		}
		for _, decision := range decisions {
			decisionSeen[decision.ID] = true
			if decision.Kind == "link" && (len(decision.MessageIDs) != decision.ExpectedMemberCount || len(decision.MessageIDs) < 2) {
				continue
			}
			if err := s.authorizeEmailFamilyEndpoints(ctx, decision.MessageIDs, true); decision.Kind == "link" && errors.Is(err, ErrNotFound) {
				continue
			} else if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, err
			}
			result.Decisions = append(result.Decisions, decision)
			if decision.Kind == "link" && len(decision.MessageIDs) >= 2 {
				for _, next := range decision.MessageIDs {
					if !seen[next] {
						pending = append(pending, next)
					}
				}
			}
			if decision.Kind == "handled" {
				for _, next := range decision.MessageIDs {
					handled[next] = true
				}
			}
		}
		// Bound the pending work as well as the final population, keeping its lowest IDs.
		slices.Sort(pending)
		pending = slices.Compact(pending)
		if len(pending) > emailFamilyMemberLimit {
			pending = pending[:emailFamilyMemberLimit]
			result.Truncated = true
		}
	}
	slices.SortFunc(result.Members, func(a, b EmailFamilyMember) int { return compareInt64(a.MessageID, b.MessageID) })
	identities := map[int64][]string{}
	identityScope, identityArgs := sourceScopeClause(ctx, "source_id")
	rows, err := s.db.QueryContext(ctx, `SELECT source_id,address_key FROM account_identities WHERE address_key<>''`+identityScope+` ORDER BY source_id,address_key`, identityArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var source int64
		var address string
		if err := rows.Scan(&source, &address); err != nil {
			_ = rows.Close()
			return nil, err
		}
		identities[source] = append(identities[source], address)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	activities := make([]emailfamily.Activity, 0, len(result.Members))
	for i := range result.Members {
		attributeEmailFamily(&result.Members[i], identities)
		result.Members[i].LocalHandled = handled[result.Members[i].MessageID]
		activities = append(activities, result.Members[i].Activity)
	}
	result.Handling = emailfamily.Evaluate(activities)
	if result.Truncated {
		result.Handling.State = "unknown"
		result.Handling.Reasons = append(result.Handling.Reasons, "family_truncated")
	}
	for i := range result.Decisions {
		// Handled batches do not add their unrelated endpoints to this family.
		if result.Decisions[i].Kind == "handled" {
			visible := []int64{}
			for _, id := range result.Decisions[i].MessageIDs {
				if seen[id] {
					visible = append(visible, id)
				}
			}
			result.Decisions[i].MessageIDs = visible
		}
	}
	slices.SortFunc(result.Decisions, func(a, b EmailFamilyDecision) int { return compareInt64(a.ID, b.ID) })
	// Include every input, not wall-clock time or unrelated archive state.
	canonical, err := json.Marshal(struct {
		Members    []EmailFamilyMember
		Identities map[int64][]string
		Truncated  bool
		Decisions  []EmailFamilyDecision
	}{result.Members, identities, result.Truncated, result.Decisions}, json.Deterministic(true), jsontext.AllowInvalidUTF8(true))
	if err != nil {
		return nil, err
	}
	result.Revision = fmt.Sprintf("%x", sha256.Sum256(canonical))
	return result, nil
}

func compareInt64(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func (s *Store) emailFamilyMember(ctx context.Context, id int64, scope string, args []any) (*EmailFamilyMember, error) {
	m := &EmailFamilyMember{Labels: []string{}, Envelopes: []EmailFamilyEnvelope{}, EvidenceKinds: []string{}}
	var sent, received, internal sql.NullTime
	var providerComplete bool
	queryArgs := append([]any{id}, args...)
	err := s.db.QueryRowContext(ctx, `SELECT m.id,m.source_id,m.conversation_id,src.source_type,src.identifier,COALESCE(m.source_message_id,''),COALESCE(c.source_conversation_id,''),
 COALESCE(m.account_address,''),COALESCE(m.account_path,''),COALESCE(m.subject,''),COALESCE(m.snippet,''),m.sent_at,m.received_at,m.internal_date,
 COALESCE(e.extraction_version,0),COALESCE(e.outcome,'pending'),COALESCE(e.raw_sha256,''),
 `+sourceHasSentFolderSQL("src")+`, `+draftEvidenceSQL("m", "src")+`,
 EXISTS(SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=m.id AND l.source_id=m.source_id AND (`+sentFolderLabelSQL("l")+` OR `+sentGmailLabelSQL("l", "src")+`))
 FROM messages m JOIN sources src ON src.id=m.source_id JOIN conversations c ON c.id=m.conversation_id
 LEFT JOIN email_link_key_state e ON e.message_id=m.id
 WHERE m.id=? AND m.deleted_at IS NULL AND COALESCE(m.message_type,'') IN ('','email')`+scope, queryArgs...).Scan(
		&m.MessageID, &m.SourceID, &m.ConversationID, &m.SourceType, &m.SourceIdentifier, &m.SourceMessageID, &m.ProviderThreadID, &m.AccountAddress, &m.AccountPath, &m.Subject, &m.Snippet,
		&sent, &received, &internal, &m.Linkage.Version, &m.Linkage.Outcome, &m.RawSHA256, &providerComplete, &m.Draft, &m.PositiveSent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read family member: %w", err)
	}
	for _, pair := range []struct {
		value  sql.NullTime
		target **time.Time
	}{{sent, &m.SentAt}, {received, &m.ReceivedAt}, {internal, &m.InternalDate}} {
		if pair.value.Valid {
			value := pair.value.Time
			*pair.target = &value
			if m.OccurredAt.IsZero() {
				m.OccurredAt = value
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind,value FROM email_link_keys WHERE message_id=? ORDER BY kind,ordinal`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			_ = rows.Close()
			return nil, err
		}
		switch kind {
		case "self":
			m.Linkage.MessageID = value
		case "reply":
			m.Linkage.Replies = append(m.Linkage.Replies, value)
		case "reference":
			m.Linkage.References = append(m.Linkage.References, value)
		case "attached":
			m.Linkage.AttachedOriginals = append(m.Linkage.AttachedOriginals, value)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT recipient_type,COALESCE(email_address,'') FROM message_recipients WHERE message_id=? ORDER BY recipient_type,COALESCE(email_address,'')`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e EmailFamilyEnvelope
		if err := rows.Scan(&e.Kind, &e.Address); err != nil {
			_ = rows.Close()
			return nil, err
		}
		m.Envelopes = append(m.Envelopes, e)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT COALESCE(l.name,'') FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=? AND l.source_id=? ORDER BY l.name,l.id`, id, m.SourceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		m.Labels = append(m.Labels, name)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if m.Linkage.MessageID != "" {
		m.EvidenceKinds = append(m.EvidenceKinds, "message_id")
	}
	if len(m.Linkage.Replies) > 0 {
		m.EvidenceKinds = append(m.EvidenceKinds, "in_reply_to")
	}
	if len(m.Linkage.References) > 0 {
		m.EvidenceKinds = append(m.EvidenceKinds, "references")
	}
	if len(m.Linkage.AttachedOriginals) > 0 {
		m.EvidenceKinds = append(m.EvidenceKinds, "forwarded_original")
	}
	if m.SourceType == "gmail" && m.ProviderThreadID != "" {
		m.EvidenceKinds = append(m.EvidenceKinds, "gmail_thread")
	}
	if !providerComplete {
		m.UnavailableReasons = append(m.UnavailableReasons, "provider_evidence_unavailable")
	}
	return m, nil
}

func emailFamilyAddress(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value {
		return ""
	}
	return strings.ToLower(value)
}

func attributeEmailFamily(m *EmailFamilyMember, identities map[int64][]string) {
	own := func(address string) bool {
		for _, addresses := range identities {
			if slices.Contains(addresses, address) {
				return true
			}
		}
		return false
	}
	fromCount := 0
	sender := ""
	hasRecipient, ownedRecipient, missing := false, false, false
	for _, e := range m.Envelopes {
		address := emailFamilyAddress(e.Address)
		switch e.Kind {
		case meetingSenderRole:
			fromCount++
			sender = address
			missing = missing || address == ""
		case "to", "cc", "bcc":
			hasRecipient = true
			if address == "" {
				missing = true
				continue
			}
			ownedRecipient = ownedRecipient || own(address)
			m.ExternalRecipient = m.ExternalRecipient || !own(address)
		}
	}
	inboundAccount := m.AccountPath == accountPathInbound &&
		slices.Contains(identities[m.SourceID], emailFamilyAddress(m.AccountAddress))
	m.ConfirmedSourceSender = fromCount == 1 && slices.Contains(identities[m.SourceID], sender)
	m.Incoming = fromCount == 1 && sender != "" && !own(sender) && (ownedRecipient || inboundAccount)
	// Confirmed inbound account attribution can stand in for missing recipient envelopes.
	identityComplete := fromCount == 1 && (hasRecipient || inboundAccount) && !missing && len(identities[m.SourceID]) > 0
	if !identityComplete {
		m.UnavailableReasons = append(m.UnavailableReasons, "identity_unavailable")
	}
	m.AttributionComplete = identityComplete && !slices.Contains(m.UnavailableReasons, "provider_evidence_unavailable")
	// A stale extraction version cannot confer current linkage completeness.
	if m.Linkage.Version != mime.EmailLinkageVersion {
		m.Linkage.Outcome = "pending"
	}
}
