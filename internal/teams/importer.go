package teams

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/export"
	internalmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const sourceTypeTeams = "teams"

// recipientRef is a resolved participant ID + display name for a conversation member.
type recipientRef struct {
	ID   int64
	Name string
}

// Importer ingests Microsoft Teams messages into the msgvault store.
type Importer struct {
	store  *store.Store
	client *Client
	res    *participantResolver
}

// NewImporter creates an Importer backed by the given store and Graph client.
func NewImporter(s *store.Store, c *Client) *Importer {
	return &Importer{store: s, client: c, res: newParticipantResolver(s, c)}
}

// Import runs a full or incremental import of Teams chats (and optionally channels)
// for the account identified by opts.Email. Returns a summary of the run.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	start := time.Now()
	src, err := imp.store.GetOrCreateSource(sourceTypeTeams, opts.Email)
	if err != nil {
		return nil, err
	}
	sum := &ImportSummary{SourceID: src.ID}

	// Build the starting SyncState by merging the last successful sync's cursor
	// (baseline) with the latest interrupted checkpoint (if any). This lets a
	// resumed run skip conversations that were already covered before the crash.
	// opts.Full skips this entirely so every message is re-fetched (repair path).
	state := NewSyncState()
	if !opts.Full {
		if prev, perr := imp.store.GetLastSuccessfulSync(src.ID); perr == nil && prev != nil && prev.CursorAfter.Valid {
			if s, lerr := LoadSyncState(prev.CursorAfter.String); lerr == nil {
				state = s
			}
		}
		// Merge in any mid-run checkpoint from an interrupted run.
		// GetLatestCheckpointedSync returns the newest recoverable checkpoint
		// after the last completed run. Uncheckpointed interruptions are skipped;
		// a completion still makes every preceding checkpoint stale.
		if cp, cerr := imp.store.GetLatestCheckpointedSync(src.ID); cerr == nil && cp != nil && cp.CursorBefore.Valid {
			if cpState, lerr := LoadSyncState(cp.CursorBefore.String); lerr == nil {
				state.Merge(cpState)
			}
		}
	}

	syncID, err := imp.store.StartSync(src.ID, "teams")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = imp.store.FailSync(syncID, err.Error())
		}
	}()

	if err = imp.syncChats(ctx, src.ID, syncID, opts, state, sum); err != nil {
		return sum, err
	}
	if opts.IncludeChannels {
		if err = imp.syncChannels(ctx, src.ID, syncID, opts, state, sum); err != nil {
			return sum, err
		}
	}
	if err = imp.store.RecomputeConversationStats(src.ID); err != nil {
		return sum, err
	}

	blob, _ := state.Marshal()
	if err = imp.store.CompleteSync(syncID, blob); err != nil {
		return sum, err
	}
	sum.Duration = time.Since(start)
	return sum, nil
}

// BackfillInlineMedia re-fetches Teams hostedContents inline media for every
// already-imported message of opts.Email that has a hostedContents URL in its
// stored HTML body. Idempotent: content-addressed storage dedupes. Honors ctx
// cancellation between messages.
func (imp *Importer) BackfillInlineMedia(ctx context.Context, opts ImportOptions) (*ImportSummary, error) {
	if opts.AttachmentsDir == "" {
		return nil, errors.New("attachments dir required")
	}
	src, err := imp.store.GetOrCreateSource(sourceTypeTeams, opts.Email)
	if err != nil {
		return nil, err
	}
	sum := &ImportSummary{SourceID: src.ID}
	start := time.Now()

	each := imp.store.ForEachTeamsHostedContentBody
	if opts.OnlyIncomplete {
		each = imp.store.ForEachTeamsIncompleteHostedContentBody
	}
	err = each(src.ID, func(messageID int64, bodyHTML string) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if imp.downloadInlineImages(ctx, messageID, bodyHTML, opts.AttachmentsDir, sum) {
			if err := imp.store.RecomputeMessageAttachmentStats(messageID); err != nil {
				sum.Errors++
			}
		}
		sum.MessagesProcessed++
		if opts.Progress != nil && sum.MessagesProcessed%500 == 0 {
			opts.Progress(fmt.Sprintf("backfill: %d messages, %d images, %d errors",
				sum.MessagesProcessed, sum.InlineImagesCopied, sum.Errors))
		}
		return nil
	})
	sum.Duration = time.Since(start)
	return sum, err
}

func (imp *Importer) syncChats(ctx context.Context, sourceID, syncID int64, opts ImportOptions, state *SyncState, sum *ImportSummary) error {
	chats, err := imp.client.ListChats(ctx)
	if err != nil {
		return err
	}
	total := len(chats)
	for idx, ch := range chats {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		convID, err := imp.store.EnsureConversationWithType(sourceID, ch.ID, conversationType(ch.ChatType), ch.Topic)
		if err != nil {
			return err
		}
		// Resolve chat members once for this chat.
		// Member fetch failure is non-fatal; we proceed with empty toRecips
		// rather than aborting the chat import.
		members, merr := imp.client.ListChatMembers(ctx, ch.ID)
		chatComplete := true
		var toRecips []recipientRef
		if merr == nil {
			toRecips = make([]recipientRef, 0, len(members))
			for _, m := range members {
				pid, rerr := imp.res.resolveMember(ctx, m)
				if rerr != nil || pid == 0 {
					continue
				}
				if cerr := imp.store.EnsureConversationParticipant(convID, pid, "member"); cerr != nil {
					sum.Errors++
				}
				toRecips = append(toRecips, recipientRef{ID: pid, Name: m.DisplayName})
			}
		} else {
			chatComplete = false
			sum.Errors++
		}

		since := state.ChatCursor(ch.ID)
		msgs, pageTruncated, err := imp.client.ListChatMessages(ctx, ch.ID, since, opts.Limit)
		if err != nil {
			sum.Errors++
			continue
		}
		var maxTime time.Time
		if since != "" {
			maxTime, _ = time.Parse(time.RFC3339Nano, since)
		}
		var convCount int
		var persistedIDs []int64
		for i := range msgs {
			if opts.Limit > 0 && convCount >= opts.Limit {
				break
			}
			gm := &msgs[i]
			messageID, added, perr := imp.persistMessage(ctx, convID, sourceID, chatSourceMessageID(ch.ID, gm.ID), gm, opts, sum, toRecips)
			if perr != nil {
				return perr
			}
			if messageID != 0 {
				persistedIDs = append(persistedIDs, messageID)
			}
			if added {
				sum.MessagesAdded++
			}
			sum.MessagesProcessed++
			convCount++
			// Track the latest lastModifiedDateTime across persisted messages using
			// time.Time comparison to avoid any lexicographic-width hazard with
			// variable-precision fractional seconds.
			if t := gm.LastModifiedDateTime.UTC(); t.After(maxTime) {
				maxTime = t
			}
		}
		truncated := pageTruncated || (opts.Limit > 0 && convCount < len(msgs))
		if chatComplete && !truncated && !maxTime.IsZero() {
			state.SetChatCursor(ch.ID, maxTime.Format(time.RFC3339Nano))
		}
		imp.enqueueEmbeddings(ctx, opts, sum, persistedIDs)
		sum.ChatsProcessed++

		// Emit per-conversation progress (1-based index).
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("chat %d/%d (%s): %d messages", idx+1, total, conversationType(ch.ChatType), convCount))
		}

		// Flush checkpoint so an interrupted run can resume from this point.
		if blob, merr := state.Marshal(); merr == nil {
			_ = imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
				PageToken:         blob,
				MessagesProcessed: sum.MessagesProcessed,
				MessagesAdded:     sum.MessagesAdded,
				ErrorsCount:       sum.Errors,
			})
		}
	}
	return nil
}

func (imp *Importer) syncChannels(ctx context.Context, sourceID, syncID int64, opts ImportOptions, state *SyncState, sum *ImportSummary) error {
	teams, err := imp.client.ListJoinedTeams(ctx)
	if err != nil {
		return err
	}
	for _, team := range teams {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		channels, cerr := imp.client.ListChannels(ctx, team.ID)
		if cerr != nil {
			sum.Errors++
			continue
		}
		for _, ch := range channels {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			key := team.ID + "/" + ch.ID
			title := team.DisplayName + " / " + ch.DisplayName
			convID, err := imp.store.EnsureConversationWithType(sourceID, key, "channel", title)
			if err != nil {
				return err
			}

			prevDelta := state.ChannelDelta(key)
			var newDelta string
			channelComplete := true
			channelTruncated := false

			// Phase 0: collect all messages for this channel into a single slice,
			// deduped by ID. This ensures that when we link replies in phase 2,
			// the parent is already persisted regardless of page order.
			seen := make(map[string]int)
			var collected []ChatMessage

			addMsg := func(gm ChatMessage) {
				if idx, dup := seen[gm.ID]; dup {
					collected[idx] = gm
					return
				}
				if opts.Limit > 0 && len(collected) >= opts.Limit {
					channelTruncated = true
					return
				}
				seen[gm.ID] = len(collected)
				collected = append(collected, gm)
			}
			remainingLimit := func() int {
				if opts.Limit <= 0 {
					return 0
				}
				remaining := opts.Limit - len(collected)
				if remaining < 0 {
					return 0
				}
				return remaining
			}

			if prevDelta == "" {
				// First run: backfill root messages + replies, then prime delta cursor.
				roots, rootsTruncated, lerr := imp.client.ListChannelMessages(ctx, team.ID, ch.ID, remainingLimit())
				if lerr != nil {
					sum.Errors++
					continue
				}
				if rootsTruncated {
					channelTruncated = true
				}
				for i := range roots {
					addMsg(roots[i])
					replyLimit := remainingLimit()
					if opts.Limit > 0 && replyLimit == 0 {
						channelTruncated = true
						break
					}
					replies, repliesTruncated, rerr := imp.client.ListReplies(ctx, team.ID, ch.ID, roots[i].ID, replyLimit)
					if rerr != nil {
						sum.Errors++
						channelComplete = false
						continue
					}
					if repliesTruncated {
						channelTruncated = true
					}
					for j := range replies {
						addMsg(replies[j])
					}
				}
				if channelComplete && !channelTruncated {
					// Prime the delta cursor only after a complete roots+replies backfill.
					deltaMessages, dl, deltaTruncated, derr := imp.client.ChannelMessagesDelta(ctx, team.ID, ch.ID, "", remainingLimit())
					if derr != nil {
						sum.Errors++
					} else {
						if deltaTruncated {
							channelTruncated = true
						}
						for i := range deltaMessages {
							addMsg(deltaMessages[i])
						}
						if !channelTruncated {
							newDelta = dl
						}
					}
				}
			} else {
				// Subsequent run: use stored delta link.
				deltaMessages, dl, deltaTruncated, derr := imp.client.ChannelMessagesDelta(ctx, team.ID, ch.ID, prevDelta, remainingLimit())
				if derr != nil {
					// On 400/410, fall back to full re-page + re-prime.
					roots, rootsTruncated, lerr := imp.client.ListChannelMessages(ctx, team.ID, ch.ID, remainingLimit())
					if lerr != nil {
						sum.Errors++
						continue
					}
					if rootsTruncated {
						channelTruncated = true
					}
					for i := range roots {
						addMsg(roots[i])
						replyLimit := remainingLimit()
						if opts.Limit > 0 && replyLimit == 0 {
							channelTruncated = true
							break
						}
						replies, repliesTruncated, rerr := imp.client.ListReplies(ctx, team.ID, ch.ID, roots[i].ID, replyLimit)
						if rerr != nil {
							sum.Errors++
							channelComplete = false
							continue
						}
						if repliesTruncated {
							channelTruncated = true
						}
						for j := range replies {
							addMsg(replies[j])
						}
					}
					if channelComplete && !channelTruncated {
						primeMessages, pdl, primeTruncated, perr := imp.client.ChannelMessagesDelta(ctx, team.ID, ch.ID, "", remainingLimit())
						if perr != nil {
							sum.Errors++
						} else {
							if primeTruncated {
								channelTruncated = true
							}
							for i := range primeMessages {
								addMsg(primeMessages[i])
							}
							if !channelTruncated {
								newDelta = pdl
							}
						}
					}
				} else {
					if deltaTruncated {
						channelTruncated = true
					}
					for i := range deltaMessages {
						addMsg(deltaMessages[i])
					}
					if !channelTruncated {
						newDelta = dl
					}
				}
			}

			// Phase 1: persist collected messages, respecting the per-conversation
			// limit. Track messages with ReplyToID for the linking phase.
			var toLink []ChatMessage
			convCount := 0
			var persistedIDs []int64
			for i := range collected {
				if opts.Limit > 0 && convCount >= opts.Limit {
					break
				}
				gm := &collected[i]
				messageID, added, perr := imp.persistMessage(ctx, convID, sourceID, channelSourceMessageID(team.ID, ch.ID, gm.ID), gm, opts, sum, nil)
				if perr != nil {
					return perr
				}
				if messageID != 0 {
					persistedIDs = append(persistedIDs, messageID)
				}
				if added {
					sum.MessagesAdded++
				}
				sum.MessagesProcessed++
				convCount++
				if gm.ReplyToID != "" {
					toLink = append(toLink, *gm)
				}
			}

			// Phase 2: link replies to their parents. All persisted messages are
			// now in the store, so SetReplyTo will always find the parent regardless
			// of the order they appeared in the collected batch.
			for i := range toLink {
				if serr := imp.store.SetReplyTo(sourceID,
					channelSourceMessageID(team.ID, ch.ID, toLink[i].ID),
					channelSourceMessageID(team.ID, ch.ID, toLink[i].ReplyToID)); serr != nil {
					sum.Errors++
				}
			}

			truncated := channelTruncated || (opts.Limit > 0 && convCount < len(collected))
			if !truncated && newDelta != "" {
				state.SetChannelDelta(key, newDelta)
			}
			imp.enqueueEmbeddings(ctx, opts, sum, persistedIDs)
			sum.ChannelsProcessed++

			// Emit per-conversation progress.
			if opts.Progress != nil {
				opts.Progress(fmt.Sprintf("channel %s: %d messages", team.DisplayName+" / "+ch.DisplayName, convCount))
			}

			// Flush checkpoint so an interrupted run can resume from this point.
			if blob, merr := state.Marshal(); merr == nil {
				_ = imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
					PageToken:         blob,
					MessagesProcessed: sum.MessagesProcessed,
					MessagesAdded:     sum.MessagesAdded,
					ErrorsCount:       sum.Errors,
				})
			}
		}
	}
	return nil
}

// persistMessage writes a single message via the granular store path.
// Returns the internal message ID and true if persisted (best-effort; UpsertMessage upserts).
func (imp *Importer) persistMessage(ctx context.Context, convID, sourceID int64, sourceMessageID string, gm *ChatMessage, opts ImportOptions, sum *ImportSummary, toRecips []recipientRef) (int64, bool, error) {
	if err := imp.store.MigrateSourceMessageID(sourceID, convID, gm.ID, sourceMessageID); err != nil {
		return 0, false, err
	}
	if gm.DeletedDateTime != nil {
		if err := imp.store.MarkMessageDeleted(sourceID, sourceMessageID); err != nil {
			sum.Errors++
		}
		return 0, false, nil
	}
	msg, text := mapMessage(gm, convID, sourceID, sourceMessageID)
	if gm.From != nil {
		pid, rerr := imp.res.resolve(ctx, identityOf(gm.From))
		if rerr != nil {
			return 0, false, rerr
		}
		if pid != 0 {
			msg.SenderID = sql.NullInt64{Int64: pid, Valid: true}
		}
	}
	if msg.SenderID.Valid {
		if err := imp.store.EnsureConversationParticipant(convID, msg.SenderID.Int64, "member"); err != nil {
			sum.Errors++
		}
	}
	messageID, err := imp.store.UpsertMessage(&msg)
	if err != nil {
		return 0, false, err
	}
	bodyHTML := sql.NullString{}
	if gm.Body.ContentType == "html" {
		bodyHTML = sql.NullString{String: gm.Body.Content, Valid: true}
	}
	if err := imp.store.UpsertMessageBody(messageID, sql.NullString{String: text, Valid: text != ""}, bodyHTML); err != nil {
		return 0, false, err
	}
	inlineImagesChanged := imp.downloadInlineImages(ctx, messageID, gm.Body.Content, opts.AttachmentsDir, sum)
	// Archive the exact original message JSON. gm.Raw is captured verbatim at
	// decode time (ChatMessage.UnmarshalJSON), so it preserves every Graph field
	// including ones we do not model; fall back to re-marshalling only if a
	// message was constructed without going through a decode.
	raw := []byte(gm.Raw)
	if len(raw) == 0 {
		marshaled, marshalErr := json.Marshal(gm)
		if marshalErr != nil {
			return 0, false, fmt.Errorf("marshal teams message raw archive: %w", marshalErr)
		}
		raw = marshaled
	}
	if len(raw) > 0 {
		if err := imp.store.UpsertMessageRawWithFormat(messageID, raw, "teams_json"); err != nil {
			return 0, false, fmt.Errorf("archive teams message raw: %w", err)
		}
	}
	senderName := ""
	if id := identityOf(gm.From); id != nil {
		senderName = id.DisplayName
	}
	if err := imp.store.UpsertFTS(messageID, msg.Subject.String, text, senderName, "", ""); err != nil {
		sum.Errors++
	}

	// Capture the sender participant ID for filtering "to" rows.
	senderPID := msg.SenderID.Int64 // 0 if not set
	var fromIDs []int64
	var fromNames []string
	if msg.SenderID.Valid {
		fromIDs = append(fromIDs, msg.SenderID.Int64)
		if id := identityOf(gm.From); id != nil {
			fromNames = append(fromNames, id.DisplayName)
		} else {
			fromNames = append(fromNames, "")
		}
	}
	if err := imp.store.ReplaceMessageRecipients(messageID, "from", fromIDs, fromNames); err != nil {
		sum.Errors++
	}

	// Write "to" rows (all members except the sender). nil means member lookup
	// failed and the importer should preserve prior rows; empty means known empty.
	if toRecips != nil {
		var toIDs []int64
		var toNames []string
		for _, r := range toRecips {
			if r.ID == 0 || r.ID == senderPID {
				continue
			}
			toIDs = append(toIDs, r.ID)
			toNames = append(toNames, r.Name)
		}
		toIDs, toNames = dedupRecipients(toIDs, toNames)
		if err := imp.store.ReplaceMessageRecipients(messageID, "to", toIDs, toNames); err != nil {
			sum.Errors++
		}
	}

	// Write "mention" rows.
	var mentionIDs []int64
	var mentionNames []string
	for i := range gm.Mentions {
		m := &gm.Mentions[i]
		if m.Mentioned == nil {
			continue
		}
		id := identityOf(m.Mentioned)
		if id == nil {
			continue
		}
		pid, rerr := imp.res.resolve(ctx, id)
		if rerr != nil || pid == 0 {
			continue
		}
		mentionIDs = append(mentionIDs, pid)
		mentionNames = append(mentionNames, id.DisplayName)
	}
	mentionIDs, mentionNames = dedupRecipients(mentionIDs, mentionNames)
	if err := imp.store.ReplaceMessageRecipients(messageID, "mention", mentionIDs, mentionNames); err != nil {
		sum.Errors++
	}

	reactions := make([]store.ReactionRef, 0, len(gm.Reactions))
	for _, rc := range gm.Reactions {
		pid, _ := imp.res.resolve(ctx, identityOf(rc.User))
		if pid != 0 {
			reactions = append(reactions, store.ReactionRef{
				ParticipantID: pid,
				Type:          rc.ReactionType,
				Value:         rc.ReactionType,
				CreatedAt:     rc.CreatedDateTime,
			})
		}
	}
	if err := imp.store.ReplaceReactions(messageID, reactions); err != nil {
		sum.Errors++
	} else {
		sum.ReactionsAdded += int64(len(reactions))
	}

	var linkAttachments []store.AttachmentRef
	// Store the call-recording link (systemEventMessage eventDetail) as an attachment.
	if recURL, recName, ok := gm.callRecording(); ok {
		linkAttachments = append(linkAttachments, store.AttachmentRef{
			Filename:    recName,
			StoragePath: recURL,
		})
	}
	// Store attachment[] refs (reference/file/card) that carry a content URL.
	for _, att := range gm.Attachments {
		if att.ContentURL == "" {
			continue
		}
		linkAttachments = append(linkAttachments, store.AttachmentRef{
			Filename:    att.Name,
			MimeType:    att.ContentType,
			StoragePath: att.ContentURL,
		})
	}
	if err := imp.store.ReplaceMessageLinkAttachments(messageID, linkAttachments); err != nil {
		sum.Errors++
	} else {
		sum.AttachmentsFound += int64(len(linkAttachments))
	}
	if inlineImagesChanged {
		if err := imp.store.RecomputeMessageAttachmentStats(messageID); err != nil {
			sum.Errors++
		}
	}
	return messageID, true, nil
}

func (imp *Importer) enqueueEmbeddings(ctx context.Context, opts ImportOptions, sum *ImportSummary, messageIDs []int64) {
	if opts.EmbedEnqueuer == nil || len(messageIDs) == 0 {
		return
	}
	if err := opts.EmbedEnqueuer.EnqueueMessages(ctx, messageIDs); err != nil {
		sum.Errors++
	}
}

// dedupRecipients removes duplicate participant IDs from ids/names slices,
// preserving first-seen order and skipping zero IDs. ids and names must be
// the same length.
func dedupRecipients(ids []int64, names []string) ([]int64, []string) {
	seen := make(map[int64]struct{}, len(ids))
	outIDs := make([]int64, 0, len(ids))
	outNames := make([]string, 0, len(ids))
	for i, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		outIDs = append(outIDs, id)
		n := ""
		if i < len(names) {
			n = names[i]
		}
		outNames = append(outNames, n)
	}
	return outIDs, outNames
}

// hostedRe matches absolute hostedContents $value URLs embedded in Teams HTML bodies.
var hostedRe = regexp.MustCompile(`https?://[^"'\s)]+/hostedContents/[^"'\s)]+/\$value`)

// hostedFetchPath rewrites an absolute Graph hostedContents URL to a path
// relative to baseURL, so the client fetches it against the configured host
// (production Graph or an httptest server) WITHOUT duplicating baseURL's
// version segment. The stored URLs are absolute and version-qualified
// (".../v1.0/chats/.../hostedContents/.../$value"); since the client already
// prepends baseURL (".../v1.0"), passing u.Path verbatim yields
// ".../v1.0/v1.0/..." and 404s every fetch. Returns "" if rawURL is unparseable.
func hostedFetchPath(baseURL, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	b, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	if !u.IsAbs() || !strings.EqualFold(u.Scheme, b.Scheme) || !strings.EqualFold(u.Host, b.Host) {
		return ""
	}
	p := u.Path
	basePath := strings.TrimRight(b.Path, "/")
	if basePath != "" {
		if p != basePath && !strings.HasPrefix(p, basePath+"/") {
			return ""
		}
		p = strings.TrimPrefix(p, basePath)
		if p == "" {
			p = "/"
		}
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// downloadInlineImages scans bodyHTML for Graph hostedContents $value URLs and
// replaces the message's Teams-managed inline attachment rows with the current
// set. If any current hosted image cannot be fetched, existing rows are
// preserved so a transient Graph failure does not erase already-downloaded
// media.
func (imp *Importer) downloadInlineImages(ctx context.Context, messageID int64, bodyHTML, attachmentsDir string, sum *ImportSummary) bool {
	raws := hostedRe.FindAllString(bodyHTML, -1)
	if len(raws) == 0 {
		if err := imp.store.ReplaceMessageInlineAttachments(messageID, nil); err != nil {
			sum.Errors++
			return false
		}
		return true
	}
	if attachmentsDir == "" {
		return false
	}

	seen := make(map[string]struct{}, len(raws))
	refs := make([]store.AttachmentRef, 0, len(raws))
	for _, raw := range raws {
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}
		// Rewrite the absolute graph.microsoft.com URL to a path relative to
		// the client's configured base URL so the request hits the correct host
		// (e.g. an httptest server in tests, or production Graph in production)
		// without duplicating the version segment.
		fetchPath := hostedFetchPath(imp.client.BaseURL(), raw)
		if fetchPath == "" {
			sum.Errors++
			return false
		}
		data, derr := imp.client.GetRaw(ctx, fetchPath)
		if derr != nil || len(data) == 0 {
			sum.Errors++
			return false
		}
		att := &internalmime.Attachment{
			Filename:    "",
			ContentType: "",
			Content:     data,
		}
		storagePath, serr := export.StoreAttachmentFile(attachmentsDir, att)
		if serr != nil || storagePath == "" {
			sum.Errors++
			return false
		}
		refs = append(refs, store.AttachmentRef{
			StoragePath:        storagePath,
			ContentHash:        att.ContentHash,
			Size:               len(data),
			SourceAttachmentID: "teams:inline:" + fetchPath,
		})
	}
	if err := imp.store.ReplaceMessageInlineAttachments(messageID, refs); err != nil {
		sum.Errors++
		return false
	}
	sum.InlineImagesCopied += int64(len(refs))
	return true
}

// identityOf extracts the primary Identity from an IdentitySet,
// preferring the User field over Application.
func identityOf(set *IdentitySet) *Identity {
	if set == nil {
		return nil
	}
	if set.User != nil {
		return set.User
	}
	return set.Application
}
