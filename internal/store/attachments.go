package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// PendingAttachmentMessage identifies a message with at least one provider
// attachment marker that has not been downloaded yet.
type PendingAttachmentMessage struct {
	MessageID        int64
	SourceMessageID  string
	ChatID           string // conversations.source_conversation_id
	ConversationType string
	ParticipantCount int
}

// BeeperPendingAttachmentMessage preserves the existing Beeper importer API.
type BeeperPendingAttachmentMessage = PendingAttachmentMessage

// DiscordPendingAttachmentMessage identifies a Discord message with pending media.
type DiscordPendingAttachmentMessage = PendingAttachmentMessage

// DiscordAttachmentMessage identifies a Discord message with provider-managed
// attachment rows, regardless of whether every row is already downloaded.
type DiscordAttachmentMessage = PendingAttachmentMessage

func (s *Store) replaceMessageProviderAttachments(messageID int64, providerPrefix string, refs []AttachmentRef) error {
	return s.replaceMessageAttachmentsWhere(
		messageID, `source_attachment_id LIKE ?`, false, refs, providerPrefix+"%",
	)
}

func (s *Store) messageProviderAttachments(messageID int64, providerPrefix string) (map[string]AttachmentRef, error) {
	rows, err := s.db.Query(`
		SELECT COALESCE(filename, ''), COALESCE(mime_type, ''), storage_path, COALESCE(content_hash, ''), size, source_attachment_id,
		       COALESCE(media_type, ''), COALESCE(width, 0), COALESCE(height, 0), COALESCE(duration_ms, 0),
		       COALESCE(CAST(attachment_metadata AS TEXT), ''), attachment_role, role_source,
		       COALESCE(source_part_key, ''), COALESCE(content_id, ''),
		       COALESCE(attachment_state, ''), COALESCE(attachment_skip_reason, '')
		FROM attachments
		WHERE message_id = ? AND source_attachment_id LIKE ?
	`, messageID, providerPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]AttachmentRef{}
	for rows.Next() {
		var ref AttachmentRef
		var size int64
		if err := rows.Scan(
			&ref.Filename, &ref.MimeType, &ref.StoragePath, &ref.ContentHash,
			&size, &ref.SourceAttachmentID, &ref.MediaType, &ref.Width,
			&ref.Height, &ref.DurationMS, &ref.Metadata, &ref.Role, &ref.RoleSource,
			&ref.SourcePartKey, &ref.ContentID, &ref.State, &ref.SkipReason,
		); err != nil {
			return nil, err
		}
		ref.Size = int(size)
		out[ref.SourceAttachmentID] = ref
	}
	return out, rows.Err()
}

func (s *Store) listRetryableAttachmentMessages(
	sourceID int64, providerPrefix string, policy attachmentpolicy.Policy,
) ([]PendingAttachmentMessage, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.source_message_id, c.source_conversation_id,
		       c.conversation_type, COALESCE(c.participant_count, 0),
		       COALESCE(a.attachment_state, ''), COALESCE(a.size, 0),
		       COALESCE(a.content_hash, ''), a.storage_path,
		       COALESCE(a.media_type, ''), COALESCE(CAST(c.metadata AS TEXT), '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN attachments a ON a.message_id = m.id
		WHERE m.source_id = ?
		  AND a.source_attachment_id LIKE ?
		ORDER BY m.id, a.id
	`, sourceID, providerPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var items []PendingAttachmentMessage
	var lastAdded int64
	for rows.Next() {
		var item PendingAttachmentMessage
		var state attachmentpolicy.DownloadState
		var size int64
		var contentHash, storagePath, mediaType, conversationMetadata string
		if err := rows.Scan(&item.MessageID, &item.SourceMessageID, &item.ChatID,
			&item.ConversationType, &item.ParticipantCount, &state, &size,
			&contentHash, &storagePath, &mediaType, &conversationMetadata); err != nil {
			return nil, err
		}
		item.ParticipantCount = policyParticipantCount(
			providerPrefix, item.ParticipantCount, conversationMetadata, policy,
		)
		if item.MessageID == lastAdded || mediaType == "link" || contentHash != "" ||
			casAttachmentDownloaded(AttachmentRef{StoragePath: storagePath}) {
			continue
		}
		eligible := attachmentpolicy.RetryEligible(state)
		if state == attachmentpolicy.StateSkipped {
			eligible = policy.Allows(attachmentpolicy.Conversation{
				Type: item.ConversationType, ParticipantCount: item.ParticipantCount,
			}, size)
		}
		if eligible {
			items = append(items, item)
			lastAdded = item.MessageID
		}
	}
	return items, rows.Err()
}

// ListBeeperRetryableAttachmentMessages returns only unfinished Beeper media.
func (s *Store) ListBeeperRetryableAttachmentMessages(sourceID int64, policy attachmentpolicy.Policy) ([]BeeperPendingAttachmentMessage, error) {
	return s.listRetryableAttachmentMessages(sourceID, "beeper:", policy)
}

// BeeperRetryPolicyResult describes the local policy state found before a
// Beeper media backfill makes provider requests.
type BeeperRetryPolicyResult struct {
	NewlySkipped                  int64
	HasExcluded                   bool
	AttachmentsOverCap            int64
	AttachmentsOverCapBytes       int64
	AttachmentsOverCapUnknownSize int64
}

// ApplyBeeperRetryableAttachmentPolicy converts unfinished Beeper rows that
// the current policy excludes into durable typed skip markers and reports
// whether excluded work remains. This lets a backfill stay local both when a
// policy is tightened and on later runs where that policy is unchanged.
func (s *Store) ApplyBeeperRetryableAttachmentPolicy(
	ctx context.Context, sourceID int64, policy attachmentpolicy.Policy,
) (BeeperRetryPolicyResult, error) {
	type exclusion struct {
		attachmentID int64
		reason       attachmentpolicy.SkipReason
		size         int64
	}
	var result BeeperRetryPolicyResult
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT a.id, c.conversation_type, COALESCE(c.participant_count, 0),
			       COALESCE(CAST(c.metadata AS TEXT), ''),
			       COALESCE(a.size, 0), COALESCE(a.attachment_state, '')
			FROM attachments a
			JOIN messages m ON m.id = a.message_id
			JOIN conversations c ON c.id = m.conversation_id
			WHERE m.source_id = ?
			  AND a.source_attachment_id LIKE 'beeper:%'
			  AND COALESCE(a.content_hash, '') = ''
			  AND COALESCE(a.media_type, '') <> 'link'
			  AND COALESCE(a.attachment_state, '') IN ('', ?, ?, ?)
			ORDER BY a.id
		`, sourceID, attachmentpolicy.StatePending, attachmentpolicy.StateFailed,
			attachmentpolicy.StateSkipped)
		if err != nil {
			return fmt.Errorf("list retryable Beeper policy candidates: %w", err)
		}
		var exclusions []exclusion
		for rows.Next() {
			var attachmentID, size int64
			var conversation attachmentpolicy.Conversation
			var conversationMetadata string
			var state attachmentpolicy.DownloadState
			if err := rows.Scan(&attachmentID, &conversation.Type, &conversation.ParticipantCount,
				&conversationMetadata, &size, &state); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan retryable Beeper policy candidate: %w", err)
			}
			conversation.ParticipantCount = policyParticipantCount(
				sourceTypeBeeper, conversation.ParticipantCount, conversationMetadata, policy,
			)
			if reason := policy.Evaluate(conversation, size); reason != "" {
				result.HasExcluded = true
				if attachmentpolicy.RetryEligible(state) {
					exclusions = append(exclusions, exclusion{
						attachmentID: attachmentID, reason: reason, size: size,
					})
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("list retryable Beeper policy candidates: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close retryable Beeper policy candidates: %w", err)
		}
		for _, exclusion := range exclusions {
			updateResult, err := tx.ExecContext(ctx, `
				UPDATE attachments
				SET attachment_state = ?, attachment_skip_reason = ?
				WHERE id = ?
				  AND COALESCE(attachment_state, '') IN ('', ?, ?)
			`, attachmentpolicy.StateSkipped, exclusion.reason, exclusion.attachmentID,
				attachmentpolicy.StatePending, attachmentpolicy.StateFailed)
			if err != nil {
				return fmt.Errorf("skip Beeper attachment %d by policy: %w", exclusion.attachmentID, err)
			}
			changed, err := updateResult.RowsAffected()
			if err != nil {
				return fmt.Errorf("count skipped Beeper attachment %d: %w", exclusion.attachmentID, err)
			}
			result.NewlySkipped += changed
			if changed > 0 && exclusion.reason == attachmentpolicy.SkipSizeCap {
				result.AttachmentsOverCap += changed
				size := max(int64(0), exclusion.size)
				if size > math.MaxInt64-result.AttachmentsOverCapBytes {
					result.AttachmentsOverCapBytes = math.MaxInt64
				} else {
					result.AttachmentsOverCapBytes += size
				}
				// Legacy and pending markers do not record whether size metadata
				// was exact or only the minimum observed by a capped stream.
				result.AttachmentsOverCapUnknownSize += changed
			}
		}
		return nil
	})
	if err != nil {
		return BeeperRetryPolicyResult{}, err
	}
	return result, nil
}

// ListSlackRetryableAttachmentMessages returns only unfinished Slack media.
func (s *Store) ListSlackRetryableAttachmentMessages(sourceID int64, policy attachmentpolicy.Policy) ([]PendingAttachmentMessage, error) {
	return s.listRetryableAttachmentMessages(sourceID, "slack:", policy)
}

// ListDiscordRetryableAttachmentMessages returns only unfinished Discord media.
func (s *Store) ListDiscordRetryableAttachmentMessages(sourceID int64, policy attachmentpolicy.Policy) ([]DiscordPendingAttachmentMessage, error) {
	return s.listRetryableAttachmentMessages(sourceID, "discord:", policy)
}

// ConversationMembership is one archived conversation as media policy sees it.
type ConversationMembership struct {
	Conversation attachmentpolicy.Conversation
	// RosterArchived reports whether the provider has recorded the exact size
	// of a roster it read and no later read has failed. False means the count
	// comes from accumulated participant rows, or that the most recent roster
	// read is archived as having failed (a count read earlier may still sit
	// beside the marker); either way a media backfill must re-resolve
	// membership before it can trust the participant threshold.
	RosterArchived bool
}

// AttachmentConversation returns the archived context used by media policy.
func (s *Store) AttachmentConversation(messageID int64) (attachmentpolicy.Conversation, error) {
	membership, err := s.AttachmentConversationMembership(messageID)
	return membership.Conversation, err
}

// AttachmentConversationMembership returns the archived context used by media
// policy together with the state of the provider's membership record.
func (s *Store) AttachmentConversationMembership(messageID int64) (ConversationMembership, error) {
	var conversation attachmentpolicy.Conversation
	var observedParticipants int
	var sourceType, metadata string
	err := s.db.QueryRow(`
		SELECT c.conversation_type, COALESCE(c.participant_count, 0),
		       (SELECT COUNT(DISTINCT cp.participant_id)
		        FROM conversation_participants cp WHERE cp.conversation_id = c.id),
		       src.source_type, COALESCE(CAST(c.metadata AS TEXT), '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		WHERE m.id = ?
	`, messageID).Scan(&conversation.Type, &conversation.ParticipantCount, &observedParticipants,
		&sourceType, &metadata)
	if observedParticipants > conversation.ParticipantCount {
		conversation.ParticipantCount = observedParticipants
	}
	conversation.ParticipantCount = attachmentPolicyParticipantCount(
		sourceType, conversation.ParticipantCount, metadata,
	)
	record := decodeMembershipRecord(metadata)
	return ConversationMembership{
		Conversation:   conversation,
		RosterArchived: record.counted && !record.unknown,
	}, err
}

// MessageConversationRef identifies the archived conversation a message belongs
// to, including the provider-side key.
type MessageConversationRef struct {
	ConversationID       int64
	SourceConversationID string
	Type                 string
}

// MessageConversation returns the conversation a message belongs to. Media
// backfills use it to re-resolve provider membership that the original sync
// could not read, so the archived participant count becomes authoritative
// before policy is evaluated again.
func (s *Store) MessageConversation(messageID int64) (MessageConversationRef, error) {
	var ref MessageConversationRef
	err := s.db.QueryRow(`
		SELECT c.id, COALESCE(c.source_conversation_id, ''), COALESCE(c.conversation_type, '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.id = ?
	`, messageID).Scan(&ref.ConversationID, &ref.SourceConversationID, &ref.Type)
	if err != nil {
		return MessageConversationRef{}, fmt.Errorf("conversation for message %d: %w", messageID, err)
	}
	return ref, nil
}

// Provider identities used to select membership semantics. Callers pass
// either a source type or the provider's attachment-ID prefix.
const (
	sourceTypeDiscord = "discord"
	sourceTypeTeams   = "teams"
	sourceTypeSlack   = "slack"
	sourceTypeBeeper  = "beeper"
)

// membershipRecord is the provider-maintained membership a conversation's
// metadata carries. Providers that can read a roster archive its exact size
// there; one that could not read it archives that explicitly, so an unresolved
// roster is distinguishable from a conversation with no members. Both may be
// present at once: a count read earlier stays for reference while the unknown
// marker says the latest read failed and the roster is unresolved.
type membershipRecord struct {
	memberCount int
	counted     bool
	unknown     bool
}

func decodeMembershipRecord(metadata string) membershipRecord {
	if strings.TrimSpace(metadata) == "" {
		return membershipRecord{}
	}
	var stored struct {
		MemberCount        *int `json:"member_count"`
		MemberCountUnknown bool `json:"member_count_unknown"`
	}
	if err := json.Unmarshal([]byte(metadata), &stored); err != nil {
		return membershipRecord{}
	}
	record := membershipRecord{unknown: stored.MemberCountUnknown}
	if stored.MemberCount != nil {
		record.memberCount, record.counted = *stored.MemberCount, true
	}
	return record
}

// attachmentPolicyParticipantCount resolves the participant count media policy
// evaluates for an archived conversation. Discord stages an authoritative
// catalog count that may only raise the observed one, so recomputed
// conversation stats cannot silently relax an exclusion. Teams, Slack, and
// Beeper archive the exact roster they read (Beeper: the participant total it
// reports, which a truncated listing's rows undercount), which is
// authoritative in both directions: participant rows only ever accumulate, so
// membership that shrank below the threshold would otherwise keep excluding
// the conversation's media forever. Conversation stats keep the observed count
// for UI and search.
func attachmentPolicyParticipantCount(sourceType string, observed int, metadata string) int {
	record := decodeMembershipRecord(metadata)
	if !record.counted {
		return observed
	}
	switch strings.TrimSuffix(sourceType, ":") {
	case sourceTypeDiscord:
		return max(record.memberCount, observed)
	case sourceTypeTeams, sourceTypeSlack, sourceTypeBeeper:
		return record.memberCount
	default:
		return observed
	}
}

// policyParticipantCount resolves the participant count with an unresolved
// roster applied: while a limit is configured, a conversation with no
// authoritative roster archived — a provider could not read one, or none was
// ever recorded — must not evaluate as a conversation under the threshold on
// the strength of its accumulated participant rows, which undercount a
// truncated or partially observed membership. The resulting skips stay
// retryable — a run that reads the roster archives the real count and a later
// backfill re-evaluates them. Purge deliberately does not use this: excluding
// media deletes blobs, so an unresolved roster there must retain rather than
// fail closed.
func policyParticipantCount(
	sourceType string, observed int, metadata string, policy attachmentpolicy.Policy,
) int {
	record := decodeMembershipRecord(metadata)
	if policy.MaxParticipants > 0 && (!record.counted || record.unknown) {
		return policy.MaxParticipants + 1
	}
	return attachmentPolicyParticipantCount(sourceType, observed, metadata)
}

func (s *Store) listPendingAttachmentMessages(sourceID int64, providerPrefix string) ([]PendingAttachmentMessage, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.source_message_id, c.source_conversation_id
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ?
		  AND EXISTS (
		    SELECT 1 FROM attachments a
		    WHERE a.message_id = m.id
		      AND a.source_attachment_id LIKE ?
		      AND (a.content_hash IS NULL OR a.content_hash = '')
		      AND COALESCE(a.media_type, '') <> 'link'
		  )
	`, sourceID, providerPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []PendingAttachmentMessage
	for rows.Next() {
		var item PendingAttachmentMessage
		if err := rows.Scan(&item.MessageID, &item.SourceMessageID, &item.ChatID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReplaceMessageDiscordAttachments replaces Discord-managed attachment rows.
// Pending rows retain an observed CDN URL or deterministic provider sentinel.
// Hashless rows with a trusted local CAS path are duplicate-content aliases.
func (s *Store) ReplaceMessageDiscordAttachments(messageID int64, refs []AttachmentRef) error {
	refs = normalizeDiscordAttachmentRefs(refs)
	return s.replaceMessageProviderAttachments(messageID, "discord:", refs)
}

// normalizeDiscordAttachmentRefs fills deterministic pending markers and
// recovers hashes from trusted CAS paths. Stable Discord attachment IDs become
// source-part keys in the generic replacement path, so duplicate bytes no
// longer need hashless alias rows.
func normalizeDiscordAttachmentRefs(refs []AttachmentRef) []AttachmentRef {
	normalized := append([]AttachmentRef(nil), refs...)
	for i := range normalized {
		if normalized[i].StoragePath == "" {
			attachmentID := strings.TrimPrefix(normalized[i].SourceAttachmentID, "discord:")
			normalized[i].StoragePath = "discord:pending:" + attachmentID
		}
		if normalized[i].ContentHash == "" {
			pathHash, ok := casPathHash(normalized[i].StoragePath)
			if !ok {
				continue
			}
			normalized[i].ContentHash = pathHash
		}
	}
	return normalized
}

// IsDiscordAttachmentDownloaded reports whether a Discord row references a
// trusted local SHA-256 CAS path. A duplicate-content alias may omit its hash;
// URLs, provider sentinels, malformed paths, and hash/path mismatches are not
// considered downloaded.
func IsDiscordAttachmentDownloaded(ref AttachmentRef) bool {
	return casAttachmentDownloaded(ref)
}

// casAttachmentDownloaded reports whether a provider attachment row
// references a trusted local SHA-256 CAS path (shared by the Discord and
// Slack media paths). A duplicate-content alias may omit its hash; URLs,
// provider sentinels, malformed paths, and hash/path mismatches are not
// considered downloaded.
func casAttachmentDownloaded(ref AttachmentRef) bool {
	pathHash, ok := casPathHash(ref.StoragePath)
	if !ok {
		return false
	}
	return ref.ContentHash == "" || ref.ContentHash == pathHash
}

// casPathHash extracts the SHA-256 content hash from a trusted local
// content-addressed storage path in the <hash[:2]>/<hash> layout. Any other
// layout — URLs, provider sentinels, uppercase or malformed hex, prefix
// mismatches — returns false.
func casPathHash(storagePath string) (string, bool) {
	if len(storagePath) != 67 || storagePath[2] != '/' {
		return "", false
	}
	contentHash := storagePath[3:]
	if contentHash != strings.ToLower(contentHash) || storagePath[:2] != contentHash[:2] {
		return "", false
	}
	if _, err := hex.DecodeString(contentHash); err != nil {
		return "", false
	}
	return contentHash, true
}

// MessageDiscordAttachments returns Discord-managed rows keyed by source ID.
func (s *Store) MessageDiscordAttachments(messageID int64) (map[string]AttachmentRef, error) {
	refs, err := s.messageProviderAttachments(messageID, "discord:")
	if err != nil {
		return nil, err
	}
	for sourceAttachmentID, ref := range refs {
		if ref.ContentHash == "" {
			if pathHash, ok := casPathHash(ref.StoragePath); ok {
				ref.ContentHash = pathHash
				refs[sourceAttachmentID] = ref
			}
		}
	}
	return refs, nil
}

// ListDiscordPendingAttachmentMessages returns messages containing at least
// one Discord attachment that does not resolve to a trusted local CAS path.
func (s *Store) ListDiscordPendingAttachmentMessages(sourceID int64) ([]DiscordPendingAttachmentMessage, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.source_message_id, c.source_conversation_id,
		       a.storage_path, COALESCE(a.content_hash, '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN attachments a ON a.message_id = m.id
		WHERE m.source_id = ?
		  AND a.source_attachment_id LIKE ?
		ORDER BY m.id, a.id
	`, sourceID, "discord:%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var pending []DiscordPendingAttachmentMessage
	var current DiscordPendingAttachmentMessage
	var haveCurrent, currentPending bool
	flushCurrent := func() {
		if haveCurrent && currentPending {
			pending = append(pending, current)
		}
	}
	for rows.Next() {
		var item DiscordPendingAttachmentMessage
		var ref AttachmentRef
		if err := rows.Scan(
			&item.MessageID, &item.SourceMessageID, &item.ChatID,
			&ref.StoragePath, &ref.ContentHash,
		); err != nil {
			return nil, err
		}
		if !haveCurrent || item.MessageID != current.MessageID {
			flushCurrent()
			current = item
			haveCurrent = true
			currentPending = false
		}
		if !IsDiscordAttachmentDownloaded(ref) {
			currentPending = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flushCurrent()
	return pending, nil
}

// ListDiscordAttachmentMessages returns every source-scoped message with at
// least one Discord-managed attachment. The one-query selection is used by a
// full media refresh; callers that only want incomplete rows use
// ListDiscordPendingAttachmentMessages.
func (s *Store) ListDiscordAttachmentMessages(sourceID int64) ([]DiscordAttachmentMessage, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.source_message_id, c.source_conversation_id,
		       c.conversation_type, COALESCE(c.participant_count, 0),
		       COALESCE(CAST(c.metadata AS TEXT), '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ?
		  AND EXISTS (
		    SELECT 1
		    FROM attachments a
		    WHERE a.message_id = m.id
		      AND a.source_attachment_id LIKE ?
		  )
		ORDER BY m.id
	`, sourceID, "discord:%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []DiscordAttachmentMessage
	for rows.Next() {
		var message DiscordAttachmentMessage
		var metadata string
		if err := rows.Scan(&message.MessageID, &message.SourceMessageID, &message.ChatID,
			&message.ConversationType, &message.ParticipantCount, &metadata); err != nil {
			return nil, err
		}
		message.ParticipantCount = attachmentPolicyParticipantCount(
			"discord", message.ParticipantCount, metadata,
		)
		messages = append(messages, message)
	}
	return messages, rows.Err()
}
