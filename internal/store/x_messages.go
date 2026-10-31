package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/textutil"
)

// XPersistPostInput is one normalized provider version. Store owns these DTOs
// so the transport package never becomes a Store dependency.
type XPersistPostInput struct {
	SourceID           int64
	OwnerID            string
	PostID             string
	AuthorID           string
	ConversationID     string
	ReplyToPostID      string
	QuotedPostID       string
	EditHistory        []string
	MentionIDs         []string
	Text               string
	TextIncomplete     bool
	EstimatedTimestamp bool
	CreatedAt          time.Time
	FetchedAt          time.Time
	Availability       string
	Raw                []byte
	Author             *XAuthorInput
}

type XPersistPostResult struct {
	MessageID int64
	Added     bool
	Updated   bool
	Skipped   bool
}

type XPostTarget struct {
	CanonicalID    string
	CurrentID      string
	LatestKnownID  string
	MessageID      int64
	ConversationID int64
	Availability   string
}

var (
	ErrXPostNotFound = errors.New("X post not found")
	ErrXPostConflict = errors.New("conflicting X post evidence")
	ErrXReplyCycle   = errors.New("cyclic X reply")
)

type xPostMetadata struct {
	EstimatedTimestamp     bool      `json:"estimated_timestamp,omitempty"`
	CanonicalID            string    `json:"canonical_id"`
	CurrentID              string    `json:"current_id"`
	LatestKnownID          string    `json:"latest_known_id"`
	AuthorID               string    `json:"author_id"`
	OriginalConversationID string    `json:"original_conversation_id"`
	ReferencedMessageID    string    `json:"referenced_message_id,omitempty"`
	QuotedPostID           string    `json:"quoted_post_id,omitempty"`
	FetchedAt              time.Time `json:"fetched_at"`
	VersionCreatedAt       time.Time `json:"version_created_at"`
	VersionPosition        int       `json:"version_position"`
	TextIncomplete         bool      `json:"text_incomplete"`
	AuthorKnown            bool      `json:"author_known"`
	Availability           string    `json:"availability"`
	MentionIDs             []string  `json:"mention_ids,omitempty"`
}

type xAlias struct {
	ID, Canonical                                 string
	MessageID                                     sql.NullInt64
	Position                                      int
	AuthorID, NativeConversation                  sql.NullString
	Availability                                  string
	SuppressedAt, CheckedAt, FetchedAt, CreatedAt sql.NullTime
	CleanupPending                                int
}

type xAliasPlan struct {
	Canonical      string
	IDs            []string
	Rows           map[string]xAlias
	MessageID      int64
	SuppressedAt   sql.NullTime
	CleanupPending bool
}

type xStoredPost struct {
	Message       Message
	Metadata      xPostMetadata
	MetadataKnown bool
}

// PersistXPostContext publishes one accepted version, identity evidence, aliases
// and message snapshots in a single existing attribution transaction. Callers
// hold source execution for the page; this method performs no provider I/O.
func (s *Store) PersistXPostContext(ctx context.Context, input XPersistPostInput) (XPersistPostResult, error) {
	if err := validateXPostInput(input); err != nil {
		return XPersistPostResult{}, err
	}
	if err := s.requireSyncSource(input.SourceID); err != nil {
		return XPersistPostResult{}, err
	}
	var result XPersistPostResult
	err := s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		var err error
		result, err = s.persistXPostTx(ctx, tx, input)
		return err
	})
	if err != nil {
		return XPersistPostResult{}, fmt.Errorf("persist X post %s: %w", input.PostID, err)
	}
	return result, nil
}

func validateXPostInput(p XPersistPostInput) error {
	if p.SourceID <= 0 || !validXControlID(p.OwnerID) || !validXControlID(p.PostID) || !validXControlID(p.AuthorID) || !validXControlID(p.ConversationID) || p.CreatedAt.IsZero() || p.FetchedAt.IsZero() {
		return errors.New("invalid normalized X post")
	}
	if len(p.EditHistory) == 0 {
		return errors.New("X post requires edit history")
	}
	found := false
	for i, id := range p.EditHistory {
		if !validXControlID(id) || (i > 0 && compareXPostIDs(p.EditHistory[i-1], id) >= 0) {
			return errors.New("invalid X edit history")
		}
		found = found || id == p.PostID
		if id == p.ReplyToPostID {
			return ErrXReplyCycle
		}
	}
	if !found {
		return errors.New("X version absent from edit history")
	}
	for _, id := range []string{p.ReplyToPostID, p.QuotedPostID} {
		if id != "" && !validXControlID(id) {
			return errors.New("invalid X reference")
		}
	}
	for _, id := range p.MentionIDs {
		if !validXControlID(id) {
			return errors.New("invalid X mention")
		}
	}
	switch p.Availability {
	case "public", "unverified", "protected", "withheld", "removed", "deleted":
	default:
		return errors.New("invalid X availability")
	}
	if p.Author != nil && (p.Author.UserID != p.AuthorID || (p.Author.SourceID != 0 && p.Author.SourceID != p.SourceID)) {
		return errors.New("X author does not match post")
	}
	if len(p.Raw) > 8<<20 {
		return errors.New("X raw post exceeds capacity")
	}
	return nil
}

func compareXPostIDs(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

func (s *Store) persistXPostTx(ctx context.Context, tx *loggedTx, p XPersistPostInput) (XPersistPostResult, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	var sourceType, owner string
	if err := q.QueryRow("SELECT source_type,identifier FROM sources WHERE id=?", p.SourceID).Scan(&sourceType, &owner); err != nil {
		return XPersistPostResult{}, err
	}
	if sourceType != "x" || owner != p.OwnerID {
		return XPersistPostResult{}, errors.New("X post source/owner mismatch")
	}
	if err := requireSourceWritableWith(q, p.SourceID); err != nil {
		return XPersistPostResult{}, err
	}
	plan, err := s.planXAliasesTx(ctx, tx, p)
	if err != nil {
		return XPersistPostResult{}, err
	}
	suppressed, foreignPending, err := s.xGlobalSuppressionTx(ctx, tx, plan.IDs)
	if err != nil {
		return XPersistPostResult{}, err
	}
	if suppressed.Valid {
		plan.SuppressedAt = suppressed
		plan.CleanupPending = plan.CleanupPending || foreignPending
	}
	prior, err := s.loadXStoredPostTx(ctx, tx, p.SourceID, plan.MessageID)
	if err != nil {
		return XPersistPostResult{}, err
	}
	if plan.SuppressedAt.Valid || plan.CleanupPending {
		availability := "removed"
		if !plan.SuppressedAt.Valid && prior.Metadata.Availability != "" {
			availability = prior.Metadata.Availability
		}
		if err := s.saveXAliasesTx(ctx, tx, p, &plan, plan.MessageID, availability, true); err != nil {
			return XPersistPostResult{}, err
		}
		return XPersistPostResult{MessageID: plan.MessageID, Skipped: true}, nil
	}
	// Unknown availability is ID-only evidence even for normalized direct Store
	// callers. It cannot clear Task6's last explicit protected observation.
	author := XAuthorInput{SourceID: p.SourceID, UserID: p.AuthorID, FetchedAt: p.FetchedAt}
	if p.Author != nil && (p.Availability == "public" || p.Availability == "protected") {
		author = *p.Author
		author.SourceID = p.SourceID
		author.UserID = p.AuthorID
		author.FetchedAt = p.FetchedAt
	}
	if p.Availability == "protected" {
		author.Protected = true
	}
	if p.Author == nil && p.Availability == "public" {
		p.Availability = "unverified"
	}
	pid, err := s.recordXAuthorTx(ctx, tx, author)
	if err != nil {
		return XPersistPostResult{}, err
	}
	var tokens sql.NullString
	err = q.QueryRow(`SELECT type_tokens FROM participant_contact_observations WHERE participant_id=? AND source_id=? AND provider_user_id=? AND address_kind='provider_identity' AND source='archive_observation' AND active_until IS NULL AND superseded_at IS NULL ORDER BY id LIMIT 1`, pid, p.SourceID, beeperidentity.ProviderIdentityKey("x", "service", "", p.AuthorID)).Scan(&tokens)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return XPersistPostResult{}, err
	}
	if tokens.Valid && slices.Contains(strings.Split(tokens.String, ","), xAuthorProtected) {
		p.Availability = "protected"
	}
	if p.Availability != "public" && p.Availability != "unverified" {
		if err := s.saveXAliasesTx(ctx, tx, p, &plan, plan.MessageID, p.Availability, true); err != nil {
			return XPersistPostResult{}, err
		}
		if plan.MessageID != 0 && prior.MetadataKnown {
			prior.Metadata.Availability = p.Availability
			if err := setXMetadataTx(q, s.dialect, plan.MessageID, prior.Metadata); err != nil {
				return XPersistPostResult{}, err
			}
		}
		return XPersistPostResult{MessageID: plan.MessageID, Skipped: true}, nil
	}
	parent, err := s.checkXParentTx(ctx, tx, p.SourceID, plan.MessageID, plan.IDs, p.ReplyToPostID)
	if err != nil {
		return XPersistPostResult{}, err
	}
	canonicalConversation := p.ConversationID
	if slices.Contains(plan.IDs, p.ConversationID) && p.ReplyToPostID == "" {
		canonicalConversation = plan.Canonical
	} else {
		alias, err := s.xAliasTx(ctx, tx, p.SourceID, p.ConversationID)
		if err == nil {
			canonicalConversation = alias.Canonical
		} else if !errors.Is(err, ErrXPostNotFound) {
			return XPersistPostResult{}, err
		}
	}
	conversationID, err := ensureConversationWithType(q, s.dialect, p.SourceID, canonicalConversation, "channel", "X conversation "+canonicalConversation)
	if err != nil {
		return XPersistPostResult{}, err
	}
	if p.ReplyToPostID == "" && slices.Contains(plan.IDs, p.ConversationID) {
		if err := s.consolidateXConversationsTx(ctx, tx, p.SourceID, conversationID, plan.IDs); err != nil {
			return XPersistPostResult{}, err
		}
	}
	oldCurrent := prior.Metadata.CurrentID
	if oldCurrent == "" {
		oldCurrent = prior.Message.SourceMessageID
	}
	checked := prior.Metadata.FetchedAt
	if oldAlias, ok := plan.Rows[oldCurrent]; ok && oldAlias.CheckedAt.Valid && oldAlias.CheckedAt.Time.After(checked) {
		checked = oldAlias.CheckedAt.Time
	}
	isNew := plan.MessageID == 0
	newer := isNew || oldCurrent == "" || compareXPostIDs(p.PostID, oldCurrent) > 0
	same := p.PostID == oldCurrent
	fresh := !p.FetchedAt.Before(checked)
	richer := same && fresh && prior.Metadata.TextIncomplete && !p.TextIncomplete
	estimated := same && fresh && prior.Metadata.EstimatedTimestamp && !p.EstimatedTimestamp
	replace := newer || richer || estimated || (!prior.MetadataKnown && same)
	authorKnown := p.Author != nil && p.Availability == "public" && (p.Author.DisplayName != "" || p.Author.Username != "")
	metadata := prior.Metadata
	result := XPersistPostResult{MessageID: plan.MessageID}
	if replace {
		if richer || estimated {
			// Full text is additive same-version evidence. Omissions in that
			// response cannot erase the selected version's other known facts.
			p.MentionIDs = append(slices.Clone(p.MentionIDs), prior.Metadata.MentionIDs...)
			if p.ReplyToPostID == "" {
				p.ReplyToPostID = prior.Metadata.ReferencedMessageID
				parent, err = s.checkXParentTx(ctx, tx, p.SourceID, plan.MessageID, plan.IDs, p.ReplyToPostID)
				if err != nil {
					return result, err
				}
			}
			if p.QuotedPostID == "" {
				p.QuotedPostID = prior.Metadata.QuotedPostID
			}
			if prior.Metadata.AuthorKnown && !authorKnown {
				before, err := s.loadXRawEnvelopeTx(ctx, tx, plan.MessageID)
				if err != nil {
					return result, err
				}
				if oldAuthor, ok := before["author"]; ok && len(p.Raw) != 0 {
					var after map[string]jsontext.Value
					if err := json.Unmarshal(p.Raw, &after); err != nil {
						return result, err
					}
					after["author"] = oldAuthor
					p.Raw, err = json.Marshal(after)
					if err != nil {
						return result, err
					}
				}
			}
		}
		mentions := slices.Clone(p.MentionIDs)
		slices.Sort(mentions)
		mentions = slices.Compact(mentions)
		toIDs := make([]int64, 0, len(mentions))
		for _, id := range mentions {
			mid, err := s.ensureParticipantByIdentifierTx(ctx, tx, "x_user_id", id, "")
			if err != nil {
				return result, err
			}
			toIDs = append(toIDs, mid)
		}
		sentAt := p.CreatedAt.UTC()
		if prior.Message.SentAt.Valid && !prior.Metadata.EstimatedTimestamp && prior.Message.SentAt.Time.Before(sentAt) {
			sentAt = prior.Message.SentAt.Time
		}
		metadata = xPostMetadata{EstimatedTimestamp: p.EstimatedTimestamp, CanonicalID: plan.Canonical, CurrentID: p.PostID, LatestKnownID: plan.IDs[len(plan.IDs)-1], AuthorID: p.AuthorID, OriginalConversationID: p.ConversationID, ReferencedMessageID: p.ReplyToPostID, QuotedPostID: p.QuotedPostID, FetchedAt: p.FetchedAt.UTC(), VersionCreatedAt: p.CreatedAt.UTC(), VersionPosition: slices.Index(plan.IDs, p.PostID), TextIncomplete: p.TextIncomplete, AuthorKnown: authorKnown || (richer && prior.Metadata.AuthorKnown), Availability: p.Availability, MentionIDs: mentions}
		rawMetadata, err := json.Marshal(metadata)
		if err != nil {
			return result, err
		}
		fromName := p.AuthorID
		if authorKnown && p.Author.DisplayName != "" {
			fromName = p.Author.DisplayName
		} else if richer && prior.Metadata.AuthorKnown {
			var name sql.NullString
			if err := q.QueryRow("SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from' LIMIT 1", plan.MessageID).Scan(&name); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return result, err
			}
			if name.Valid && name.String != "" {
				fromName = name.String
			}
		}
		payload := &MessagePersistData{Message: &Message{SourceID: p.SourceID, SourceMessageID: plan.Canonical, MessageType: "x", ConversationID: conversationID, SentAt: sql.NullTime{Time: sentAt, Valid: true}, SenderID: sql.NullInt64{Int64: pid, Valid: true}, ReplyToMessageID: sql.NullInt64{Int64: parent, Valid: parent != 0}, IsFromMe: p.AuthorID == p.OwnerID, Snippet: sql.NullString{String: textutil.SnippetFromBody(p.Text), Valid: true}, SizeEstimate: int64(len(p.Text)), PreserveAttachmentStats: true}, BodyText: sql.NullString{String: p.Text, Valid: true}, Metadata: &sql.NullString{String: string(rawMetadata), Valid: true}, RawMIME: p.Raw, RawFormat: "x_json", PreserveLabels: true, Recipients: []RecipientSet{{Type: "from", ParticipantIDs: []int64{pid}, DisplayNames: []string{fromName}}, {Type: "to", ParticipantIDs: toIDs}}, FTS: &FTSDoc{Body: p.Text}, Conversation: &ConversationPersistData{SourceConversationID: canonicalConversation, ConversationType: "channel", Title: "X conversation " + canonicalConversation, Participants: []ConversationParticipantRef{{ParticipantID: pid, Role: "member"}}, PreserveExistingParticipants: true}}
		if plan.MessageID != 0 && prior.Message.SourceMessageID != plan.Canonical {
			if _, err := q.Exec("UPDATE messages SET source_message_id=? WHERE id=? AND source_id=?", plan.Canonical, plan.MessageID, p.SourceID); err != nil {
				return result, err
			}
		}
		messageID, err := s.persistMessageWithParticipantsTx(ctx, tx, nil, nil, func([]int64) *MessagePersistData { return payload }, nil, nil)
		if err != nil {
			return result, err
		}
		if plan.MessageID != 0 && messageID != plan.MessageID {
			return result, ErrXPostConflict
		}
		result.MessageID = messageID
		result.Added = isNew
		result.Updated = !isNew
		if parent != 0 {
			if _, err := q.Exec("UPDATE messages SET reply_to_message_id=? WHERE id=? AND source_id=?", parent, messageID, p.SourceID); err != nil {
				return result, err
			}
		}
	} else {
		// Older observations may repair original creation time and aliases, never
		// replace text/raw/recipients or the selected version's timestamp.
		changed := false
		if p.CreatedAt.Before(prior.Message.SentAt.Time) || !prior.Message.SentAt.Valid {
			if _, err := q.Exec("UPDATE messages SET sent_at=? WHERE id=? AND source_id=?", p.CreatedAt.UTC(), plan.MessageID, p.SourceID); err != nil {
				return result, err
			}
			changed = true
		}
		if prior.Message.SourceMessageID != plan.Canonical {
			if _, err := q.Exec("UPDATE messages SET source_message_id=? WHERE id=? AND source_id=?", plan.Canonical, plan.MessageID, p.SourceID); err != nil {
				return result, err
			}
			changed = true
		}
		previous, err := json.Marshal(metadata)
		if err != nil {
			return result, err
		}
		metadata.CanonicalID = plan.Canonical
		metadata.LatestKnownID = plan.IDs[len(plan.IDs)-1]
		metadata.VersionPosition = slices.Index(plan.IDs, metadata.CurrentID)
		if same && fresh {
			metadata.Availability = p.Availability
			if metadata.ReferencedMessageID == "" && p.ReplyToPostID != "" {
				metadata.ReferencedMessageID = p.ReplyToPostID
				if parent != 0 {
					if _, err := q.Exec("UPDATE messages SET reply_to_message_id=? WHERE id=? AND source_id=? AND reply_to_message_id IS NULL", parent, plan.MessageID, p.SourceID); err != nil {
						return result, err
					}
					if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
						return result, err
					}
				}
			}
			if metadata.QuotedPostID == "" {
				metadata.QuotedPostID = p.QuotedPostID
			}
			// Missing fields are not evidence that an ID-backed mention vanished.
			// Only a superset can enrich the selected version's recipient snapshot.
			mentions := slices.Clone(p.MentionIDs)
			slices.Sort(mentions)
			mentions = slices.Compact(mentions)
			if len(mentions) > len(metadata.MentionIDs) && !slices.ContainsFunc(metadata.MentionIDs, func(id string) bool { return !slices.Contains(mentions, id) }) {
				toIDs := make([]int64, 0, len(mentions))
				for _, id := range mentions {
					mid, err := s.ensureParticipantByIdentifierTx(ctx, tx, "x_user_id", id, "")
					if err != nil {
						return result, err
					}
					toIDs = append(toIDs, mid)
				}
				if err := replaceMessageRecipientsTx(q, plan.MessageID, RecipientSet{Type: "to", ParticipantIDs: toIDs}); err != nil {
					return result, err
				}
				metadata.MentionIDs = mentions
			}
			if !metadata.AuthorKnown && authorKnown {
				metadata.AuthorKnown = true
				if err := replaceMessageRecipientsTx(q, plan.MessageID, RecipientSet{Type: "from", ParticipantIDs: []int64{pid}, DisplayNames: []string{p.Author.DisplayName}}); err != nil {
					return result, err
				}
				if err := s.enrichXAuthorRawTx(ctx, tx, plan.MessageID, p.Raw); err != nil {
					return result, err
				}
			}
		}
		updated, err := json.Marshal(metadata)
		if err != nil {
			return result, err
		}
		if string(previous) != string(updated) {
			if err := setXMetadataTx(q, s.dialect, plan.MessageID, metadata); err != nil {
				return result, err
			}
			changed = true
		}
		result.Updated = changed
	}
	if err := s.saveXAliasesTx(ctx, tx, p, &plan, result.MessageID, p.Availability, false); err != nil {
		return result, err
	}
	if err := s.recomputeConversationStatsWith(q, "id = ?", conversationID); err != nil {
		return result, err
	}
	if _, err := q.Exec(`UPDATE conversations SET unread_count=(SELECT count(*) FROM messages WHERE conversation_id=? AND deleted_at IS NULL AND is_read=FALSE) WHERE id=?`, conversationID, conversationID); err != nil {
		return result, err
	}
	return result, nil
}

func setXMetadataTx(q querier, d Dialect, id int64, metadata xPostMetadata) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return setMessageMetadataWith(q, d, id, sql.NullString{String: string(data), Valid: true})
}

func (s *Store) xAliasTx(ctx context.Context, tx *loggedTx, source int64, id string) (xAlias, error) {
	var a xAlias
	err := tx.QueryRowContext(ctx, `SELECT post_id,canonical_id,message_id,version_position,author_id,source_conversation_id,availability,suppressed_at,last_checked_at,fetched_at,version_created_at,cleanup_pending FROM x_post_versions WHERE source_id=? AND post_id=?`, source, id).Scan(&a.ID, &a.Canonical, &a.MessageID, &a.Position, &a.AuthorID, &a.NativeConversation, &a.Availability, &a.SuppressedAt, &a.CheckedAt, &a.FetchedAt, &a.CreatedAt, &a.CleanupPending)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrXPostNotFound
	}
	return a, err
}

func (s *Store) planXAliasesTx(ctx context.Context, tx *loggedTx, p XPersistPostInput) (xAliasPlan, error) {
	plan := xAliasPlan{Canonical: p.EditHistory[0], Rows: make(map[string]xAlias)}
	var oldCanonical string
	for _, id := range p.EditHistory {
		a, err := s.xAliasTx(ctx, tx, p.SourceID, id)
		if errors.Is(err, ErrXPostNotFound) {
			continue
		}
		if err != nil {
			return plan, err
		}
		if oldCanonical != "" && oldCanonical != a.Canonical {
			return plan, ErrXPostConflict
		}
		oldCanonical = a.Canonical
	}
	var oldIDs []string
	if oldCanonical != "" {
		rows, err := tx.QueryContext(ctx, `SELECT post_id,canonical_id,message_id,version_position,author_id,source_conversation_id,availability,suppressed_at,last_checked_at,fetched_at,version_created_at,cleanup_pending FROM x_post_versions WHERE source_id=? AND canonical_id=? ORDER BY version_position,post_id`, p.SourceID, oldCanonical)
		if err != nil {
			return plan, err
		}
		for rows.Next() {
			var a xAlias
			if err := rows.Scan(&a.ID, &a.Canonical, &a.MessageID, &a.Position, &a.AuthorID, &a.NativeConversation, &a.Availability, &a.SuppressedAt, &a.CheckedAt, &a.FetchedAt, &a.CreatedAt, &a.CleanupPending); err != nil {
				_ = rows.Close()
				return plan, err
			}
			plan.Rows[a.ID] = a
			oldIDs = append(oldIDs, a.ID)
			if a.AuthorID.Valid && a.AuthorID.String != p.AuthorID {
				_ = rows.Close()
				return plan, ErrXPostConflict
			}
			if a.MessageID.Valid {
				if plan.MessageID != 0 && plan.MessageID != a.MessageID.Int64 {
					_ = rows.Close()
					return plan, ErrXPostConflict
				}
				plan.MessageID = a.MessageID.Int64
			}
			if a.SuppressedAt.Valid {
				plan.SuppressedAt = a.SuppressedAt
			}
			plan.CleanupPending = plan.CleanupPending || a.CleanupPending != 0
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return plan, err
		}
		if closeErr != nil {
			return plan, closeErr
		}
		if oldCanonical != plan.Canonical {
			switch {
			case plan.SuppressedAt.Valid:
				// Keep the durable canonical identity while extending a closed
				// chain, even when this response omits an earlier known alias.
				plan.Canonical = oldCanonical
			case len(oldIDs) == 1 && slices.Contains(p.EditHistory, oldCanonical):
			case len(p.EditHistory) == 1 && slices.Contains(oldIDs, p.EditHistory[0]):
				plan.Canonical = oldCanonical
			default:
				return plan, ErrXPostConflict
			}
		} else if len(oldIDs) > 1 && len(p.EditHistory) > 1 {
			for i := 0; i < min(len(oldIDs), len(p.EditHistory)); i++ {
				if oldIDs[i] != p.EditHistory[i] {
					return plan, ErrXPostConflict
				}
			}
		}
	}
	ids := make(map[string]bool, len(oldIDs)+len(p.EditHistory))
	for _, id := range oldIDs {
		ids[id] = true
	}
	for _, id := range p.EditHistory {
		ids[id] = true
	}
	for id := range ids {
		plan.IDs = append(plan.IDs, id)
	}
	slices.SortFunc(plan.IDs, compareXPostIDs)
	if plan.IDs[0] != plan.Canonical && !plan.SuppressedAt.Valid {
		return plan, ErrXPostConflict
	}
	return plan, nil
}

func (s *Store) loadXStoredPostTx(ctx context.Context, tx *loggedTx, source, id int64) (xStoredPost, error) {
	var old xStoredPost
	if id == 0 {
		return old, nil
	}
	var metadata sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,source_id,source_message_id,conversation_id,message_type,sent_at,sender_id,reply_to_message_id,metadata FROM messages WHERE id=? AND source_id=?`, id, source).Scan(&old.Message.ID, &old.Message.SourceID, &old.Message.SourceMessageID, &old.Message.ConversationID, &old.Message.MessageType, &old.Message.SentAt, &old.Message.SenderID, &old.Message.ReplyToMessageID, &metadata)
	if err != nil {
		return old, err
	}
	if old.Message.MessageType != "x" {
		return old, ErrXPostConflict
	}
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &old.Metadata); err != nil {
			return old, errors.New("invalid stored X metadata")
		}
		old.MetadataKnown = old.Metadata.CurrentID != ""
	}
	return old, nil
}

func (s *Store) saveXAliasesTx(ctx context.Context, tx *loggedTx, p XPersistPostInput, plan *xAliasPlan, messageID int64, availability string, unavailable bool) error {
	for position, id := range plan.IDs {
		a, exists := plan.Rows[id]
		if !exists {
			a = xAlias{ID: id, Availability: "unverified", AuthorID: sql.NullString{String: p.AuthorID, Valid: true}, NativeConversation: sql.NullString{String: p.ConversationID, Valid: true}}
		}
		if id == p.PostID && (!a.CheckedAt.Valid || !p.FetchedAt.Before(a.CheckedAt.Time)) {
			a.CheckedAt = sql.NullTime{Time: p.FetchedAt.UTC(), Valid: true}
			a.FetchedAt = a.CheckedAt
			a.Availability = availability
			a.CreatedAt = sql.NullTime{Time: p.CreatedAt.UTC(), Valid: true}
			a.NativeConversation = sql.NullString{String: p.ConversationID, Valid: true}
		}
		if unavailable {
			a.Availability = availability
			if messageID != 0 || plan.CleanupPending {
				a.CleanupPending = 1
			}
		}
		if plan.SuppressedAt.Valid {
			a.SuppressedAt = plan.SuppressedAt
			a.Availability = "removed"
		}
		if availability == "removed" || availability == "deleted" {
			a.SuppressedAt = sql.NullTime{Time: p.FetchedAt.UTC(), Valid: true}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO x_post_versions(source_id,post_id,canonical_id,message_id,version_position,author_id,source_conversation_id,availability,suppressed_at,last_checked_at,fetched_at,version_created_at,cleanup_pending) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(source_id,post_id) DO UPDATE SET canonical_id=excluded.canonical_id,message_id=excluded.message_id,version_position=excluded.version_position,author_id=excluded.author_id,source_conversation_id=excluded.source_conversation_id,availability=excluded.availability,suppressed_at=excluded.suppressed_at,last_checked_at=excluded.last_checked_at,fetched_at=excluded.fetched_at,version_created_at=excluded.version_created_at,cleanup_pending=excluded.cleanup_pending`, p.SourceID, id, plan.Canonical, sql.NullInt64{Int64: messageID, Valid: messageID != 0}, position, a.AuthorID, a.NativeConversation, a.Availability, a.SuppressedAt, a.CheckedAt, a.FetchedAt, a.CreatedAt, a.CleanupPending)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) consolidateXConversationsTx(ctx context.Context, tx *loggedTx, source, survivor int64, aliases []string) error {
	q := boundQuerier{ctx: ctx, q: tx}
	moved := false
	for _, id := range aliases {
		var loser int64
		err := q.QueryRow("SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?", source, id).Scan(&loser)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if loser == survivor {
			continue
		}
		var foreign bool
		if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM messages WHERE conversation_id=? AND (source_id<>? OR message_type<>'x')) OR EXISTS(SELECT 1 FROM chat_drafts WHERE conversation_id=? AND source_id<>?)`, loser, source, loser, source).Scan(&foreign); err != nil {
			return err
		}
		if foreign {
			return ErrXPostConflict
		}
		// Preserve an owner's local tier when only the losing provisional thread
		// has one. Conflicting curated tiers fail rather than erase either choice.
		var policies int
		if err := q.QueryRow("SELECT count(*) FROM conversation_policy WHERE conversation_id IN (?,?)", loser, survivor).Scan(&policies); err != nil {
			return err
		}
		if policies == 2 {
			var conflict bool
			if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM conversation_policy a JOIN conversation_policy b ON b.conversation_id=? WHERE a.conversation_id=? AND (a.effective_tier<>b.effective_tier OR a.override_tier IS DISTINCT FROM b.override_tier))`, survivor, loser).Scan(&conflict); err != nil {
				return err
			}
			if conflict {
				return ErrXPostConflict
			}
		} else if policies == 1 {
			if _, err := q.Exec("UPDATE conversation_policy SET conversation_id=? WHERE conversation_id=?", survivor, loser); err != nil {
				return err
			}
		}
		if _, err := q.Exec(`INSERT INTO conversation_participants(conversation_id,participant_id,role,joined_at,left_at) SELECT ?,participant_id,role,joined_at,left_at FROM conversation_participants WHERE conversation_id=? ON CONFLICT(conversation_id,participant_id) DO NOTHING`, survivor, loser); err != nil {
			return err
		}
		if _, err := q.Exec("UPDATE messages SET conversation_id=? WHERE source_id=? AND conversation_id=?", survivor, source, loser); err != nil {
			return err
		}
		// Move only the local FK. Native pins, reviewed revision and body remain.
		if _, err := q.Exec("UPDATE chat_drafts SET conversation_id=? WHERE source_id=? AND conversation_id=?", survivor, source, loser); err != nil {
			return err
		}
		if _, err := q.Exec("DELETE FROM conversations WHERE id=? AND source_id=?", loser, source); err != nil {
			return err
		}
		moved = true
	}
	if moved {
		return s.bumpDerivedDataRevisionContext(ctx, tx)
	}
	return nil
}

func (s *Store) checkXParentTx(ctx context.Context, tx *loggedTx, source, childID int64, ownIDs []string, parentID string) (int64, error) {
	if parentID == "" {
		return 0, nil
	}
	first := int64(0)
	seen := make(map[int64]bool)
	for parentID != "" {
		if slices.Contains(ownIDs, parentID) {
			return 0, ErrXReplyCycle
		}
		alias, err := s.xAliasTx(ctx, tx, source, parentID)
		if errors.Is(err, ErrXPostNotFound) {
			return first, nil
		}
		if err != nil {
			return 0, err
		}
		if !alias.MessageID.Valid {
			return first, nil
		}
		id := alias.MessageID.Int64
		if first == 0 {
			first = id
		}
		if id == childID || seen[id] {
			return 0, ErrXReplyCycle
		}
		seen[id] = true
		parent, err := s.loadXStoredPostTx(ctx, tx, source, id)
		if err != nil {
			return 0, err
		}
		parentID = parent.Metadata.ReferencedMessageID
	}
	return first, nil
}

// ResolveXPostContext resolves any retained version through indexed IDs, using
// the selected snapshot's availability, never an old alias's public status.
func (s *Store) ResolveXPostContext(ctx context.Context, source int64, postID string) (XPostTarget, error) {
	if !validXControlID(postID) {
		return XPostTarget{}, errors.New("invalid X post ID")
	}
	if err := s.requireSyncSource(source); err != nil {
		return XPostTarget{}, err
	}
	var result XPostTarget
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		result, err = s.resolveXPostTx(ctx, tx, source, postID)
		return err
	})
	return result, err
}

func (s *Store) resolveXPostTx(ctx context.Context, tx *loggedTx, source int64, postID string) (XPostTarget, error) {
	var result XPostTarget
	alias, err := s.xAliasTx(ctx, tx, source, postID)
	if err != nil {
		return XPostTarget{}, err
	}
	// Confirmed suppression applies across sources before the owner coordinator
	// finishes purging every copy of this connected native chain.
	suppressed := alias.SuppressedAt.Valid
	if !suppressed {
		global, _, err := s.xGlobalSuppressionTx(ctx, tx, []string{postID})
		if err != nil {
			return XPostTarget{}, err
		}
		suppressed = global.Valid
	}
	result = XPostTarget{CanonicalID: alias.Canonical, CurrentID: alias.ID, Availability: alias.Availability}
	rows, err := tx.QueryContext(ctx, "SELECT post_id,fetched_at FROM x_post_versions WHERE source_id=? AND canonical_id=? ORDER BY version_position,post_id", source, alias.Canonical)
	if err != nil {
		return XPostTarget{}, err
	}
	for rows.Next() {
		var id string
		var fetched sql.NullTime
		if err := rows.Scan(&id, &fetched); err != nil {
			_ = rows.Close()
			return XPostTarget{}, err
		}
		result.LatestKnownID = id
		if fetched.Valid {
			result.CurrentID = id
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return XPostTarget{}, err
	}
	if suppressed {
		result.Availability = "removed"
	}
	if !alias.MessageID.Valid {
		return result, nil
	}
	prior, err := s.loadXStoredPostTx(ctx, tx, source, alias.MessageID.Int64)
	if err != nil {
		return XPostTarget{}, err
	}
	result.MessageID = prior.Message.ID
	result.ConversationID = prior.Message.ConversationID
	if prior.MetadataKnown {
		result.CurrentID = prior.Metadata.CurrentID
	}
	current, err := s.xAliasTx(ctx, tx, source, result.CurrentID)
	if err != nil {
		return XPostTarget{}, err
	}
	result.Availability = current.Availability
	if suppressed || current.SuppressedAt.Valid {
		result.Availability = "removed"
	} else if result.CurrentID != result.LatestKnownID {
		result.Availability = "unverified"
	}
	return result, nil
}

// ResolveXParentsContext processes one bounded keyset page. A caller restarts
// at zero on a later pass so an old unresolved child is revisited after arrival.
func (s *Store) ResolveXParentsContext(ctx context.Context, source, after int64, limit int) (int64, error) {
	if err := s.requireSyncSource(source); err != nil {
		return 0, err
	}
	candidates, err := s.ListUnresolvedMessageRepliesAfterContext(ctx, source, "x", after, limit)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	err = s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		changed := false
		for _, candidate := range candidates {
			current, err := s.loadXStoredPostTx(ctx, tx, source, candidate.MessageID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if current.Message.ReplyToMessageID.Valid || current.Metadata.ReferencedMessageID == "" {
				continue
			}
			parent, err := s.checkXParentTx(ctx, tx, source, current.Message.ID, []string{current.Metadata.CanonicalID, current.Metadata.CurrentID}, current.Metadata.ReferencedMessageID)
			if err != nil {
				return err
			}
			if parent == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, "UPDATE messages SET reply_to_message_id=? WHERE id=? AND source_id=? AND reply_to_message_id IS NULL", parent, current.Message.ID, source); err != nil {
				return err
			}
			changed = true
		}
		if changed {
			return s.bumpDerivedDataRevisionContext(ctx, tx)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return candidates[len(candidates)-1].MessageID, nil
}

func (s *Store) loadXRawEnvelopeTx(ctx context.Context, tx *loggedTx, id int64) (map[string]jsontext.Value, error) {
	var compressed []byte
	var compression sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT raw_data,compression FROM message_raw WHERE message_id=?", id).Scan(&compressed, &compression); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	old, err := decodeMessageRaw(compressed, compression)
	if err != nil {
		return nil, err
	}
	var envelope map[string]jsontext.Value
	if err := json.Unmarshal(old, &envelope); err != nil {
		return nil, errors.New("invalid X raw envelope")
	}
	return envelope, nil
}

func (s *Store) enrichXAuthorRawTx(ctx context.Context, tx *loggedTx, id int64, incoming []byte) error {
	before, err := s.loadXRawEnvelopeTx(ctx, tx, id)
	if err != nil || before == nil || len(incoming) == 0 {
		return err
	}
	var after map[string]jsontext.Value
	if json.Unmarshal(incoming, &after) != nil {
		return errors.New("invalid X raw envelope")
	}
	if author, ok := after["author"]; ok {
		before["author"] = author
		data, err := json.Marshal(before)
		if err != nil {
			return err
		}
		return upsertMessageRawWithFormat(boundQuerier{ctx: ctx, q: tx}, id, data, "x_json")
	}
	return nil
}
