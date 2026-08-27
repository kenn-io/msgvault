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

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	internalmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const sourceTypeTeams = "teams"

// conversationTypeChannel is the archived conversation type for team channels.
// Channel conversation keys are "<teamID>/<channelID>".
const conversationTypeChannel = "channel"

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

func (imp *Importer) scopedToSync(sourceID, syncID int64) *Importer {
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(sourceID, syncID)
	scoped.res = newParticipantResolver(scoped.store, scoped.client)
	return &scoped
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
	imp = imp.scopedToSync(src.ID, syncID)
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
		each = func(sourceID int64, fn func(messageID int64, bodyHTML string) error) error {
			return imp.store.ForEachTeamsIncompleteHostedContentBody(sourceID, opts.MediaPolicy, fn)
		}
	}
	resolver := newRosterResolver(imp)
	err = each(src.ID, func(messageID int64, bodyHTML string) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		itemOpts := opts
		membership, convErr := imp.store.AttachmentConversationMembership(messageID)
		if convErr != nil {
			return convErr
		}
		conversation := membership.Conversation
		if !membership.RosterArchived && opts.MediaPolicy.MaxParticipants > 0 {
			// No roster was archived for this conversation — the sync could not
			// read one, or it predates the record — so the threshold has nothing
			// authoritative to evaluate until this run resolves it.
			conversation.ParticipantCount = imp.refreshMembership(ctx, messageID, opts, resolver, sum)
		}
		itemOpts.MediaConversation = conversation
		if imp.downloadInlineImages(ctx, messageID, bodyHTML, itemOpts, sum) {
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

// chatCursorOverlap widens the incremental chat window backwards before
// querying. Graph only accepts an exclusive "gt" filter on
// lastModifiedDateTime, so a message sharing the cursor's exact timestamp
// would be skipped forever; Graph timestamps are millisecond-resolution, so
// such collisions are possible. Re-reading a small overlap costs a handful of
// messages per chat and is free of side effects: persistence upserts on
// (source_id, source_message_id), and maxTime is seeded from the stored cursor
// so a re-read of older messages cannot move it backwards.
const chatCursorOverlap = time.Second

// chatQuerySince rewinds a stored chat cursor by chatCursorOverlap. An empty
// or unparseable cursor is passed through untouched, so a first sync stays
// unfiltered and a malformed cursor degrades to the previous behavior rather
// than dropping the filter entirely.
func chatQuerySince(cursor string) string {
	if cursor == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil {
		return cursor
	}
	return t.Add(-chatCursorOverlap).Format(time.RFC3339Nano)
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
		// Only the roster's size and read outcome matter to media policy; the
		// members are resolved below, where their display names are kept too.
		roster := &memberRoster{memberCount: len(members), err: merr}
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
		imp.recordRosterOutcome(convID, roster, sum)
		chatOpts := opts
		chatOpts.MediaConversation = attachmentpolicy.Conversation{
			Type:             conversationType(ch.ChatType),
			ParticipantCount: roster.policyCount(opts.MediaPolicy),
		}

		since := state.ChatCursor(ch.ID)
		msgs, pageTruncated, err := imp.client.ListChatMessages(ctx, ch.ID, chatQuerySince(since), opts.Limit)
		if err != nil {
			sum.Errors++
			continue
		}
		var maxTime time.Time
		if since != "" {
			maxTime, _ = time.Parse(time.RFC3339Nano, since)
		}
		// The overlap window deliberately re-reads messages at the cursor
		// boundary, so "persisted" no longer implies "new". Resolve which of
		// these IDs the archive already holds, so MessagesAdded stays a count
		// of genuinely new messages. Identity is the only reliable test here:
		// a message sharing the cursor timestamp may be a boundary re-read or
		// the very tie the overlap exists to recover. On error, fall back to
		// counting every persist, matching the pre-overlap behavior.
		var preexisting map[string]int64
		if since != "" && len(msgs) > 0 {
			ids := make([]string, 0, len(msgs))
			for i := range msgs {
				ids = append(ids, chatSourceMessageID(ch.ID, msgs[i].ID))
			}
			if found, eerr := imp.store.MessageExistsBatch(sourceID, ids); eerr == nil {
				preexisting = found
			}
		}
		var convCount int
		var persistedIDs []int64
		for i := range msgs {
			if opts.Limit > 0 && convCount >= opts.Limit {
				break
			}
			gm := &msgs[i]
			messageID, added, perr := imp.persistMessage(ctx, convID, sourceID, chatSourceMessageID(ch.ID, gm.ID), gm, chatOpts, sum, toRecips)
			if perr != nil {
				return perr
			}
			if messageID != 0 {
				persistedIDs = append(persistedIDs, messageID)
			}
			if added {
				if _, seen := preexisting[chatSourceMessageID(ch.ID, gm.ID)]; !seen {
					sum.MessagesAdded++
				}
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
		// Standard channels inherit the team roster, so resolve it once per
		// team — lazily, since a team of only private channels never needs it —
		// and mirror it onto every channel conversation the way chats persist
		// their members. Media policy needs the count, and backfill/purge read
		// it back from the archived participant rows.
		var teamMembers *memberRoster
		teamRoster := func() *memberRoster {
			if teamMembers == nil {
				teamMembers = imp.resolveTeamRoster(ctx, team.ID)
				if teamMembers.err != nil && opts.MediaPolicy.MaxParticipants > 0 {
					// The roster is unknown, so the threshold cannot be
					// evaluated. Count the failure and fail closed below rather
					// than treat an unreadable team as a small one.
					sum.Errors++
				}
			}
			return teamMembers
		}
		for _, ch := range channels {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Private and shared channels are governed by their own membership,
			// which can be far smaller — or larger — than the team's, so the
			// team roster is resolved only for channels that inherit it.
			var roster *memberRoster
			if channelHasOwnRoster(ch.MembershipType) {
				roster = imp.resolveChannelRoster(ctx, team.ID, ch.ID)
				if roster.err != nil && opts.MediaPolicy.MaxParticipants > 0 {
					sum.Errors++
				}
			} else {
				roster = teamRoster()
			}
			key := team.ID + "/" + ch.ID
			title := team.DisplayName + " / " + ch.DisplayName
			convID, err := imp.store.EnsureConversationWithType(sourceID, key, conversationTypeChannel, title)
			if err != nil {
				return err
			}
			for _, pid := range roster.participantIDs {
				if cerr := imp.store.EnsureConversationParticipant(convID, pid, "member"); cerr != nil {
					sum.Errors++
				}
			}
			imp.recordRosterOutcome(convID, roster, sum)

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
			channelOpts := opts
			channelOpts.MediaConversation = attachmentpolicy.Conversation{
				Type:             conversationTypeChannel,
				ParticipantCount: roster.policyCount(opts.MediaPolicy),
			}
			for i := range collected {
				if opts.Limit > 0 && convCount >= opts.Limit {
					break
				}
				gm := &collected[i]
				messageID, added, perr := imp.persistMessage(ctx, convID, sourceID, channelSourceMessageID(team.ID, ch.ID, gm.ID), gm, channelOpts, sum, nil)
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

// memberRoster is a membership as resolved from Graph: the raw member count
// media policy evaluates, and the participant IDs to archive on the
// conversations it governs. A non-nil err means the roster could not be read,
// which callers must treat as unknown membership rather than as an empty one.
type memberRoster struct {
	participantIDs []int64
	memberCount    int
	err            error
}

// policyCount is the participant count media policy evaluates this roster
// against. A roster that could not be read must not pass as a conversation
// under the threshold, so it fails closed while a limit is configured; the
// skips that follow are retryable once the roster becomes readable.
func (r *memberRoster) policyCount(policy attachmentpolicy.Policy) int {
	if r.err != nil && policy.MaxParticipants > 0 {
		return policy.MaxParticipants + 1
	}
	return r.memberCount
}

// recordRosterOutcome archives the membership that later runs evaluate media
// policy against: the exact size of a roster that was read, or an explicit
// unknown marker when it could not be. Backfill and purge read this record
// rather than the archived participant rows, which accumulate every member
// ever seen and therefore never shrink when someone leaves.
func (imp *Importer) recordRosterOutcome(convID int64, roster *memberRoster, sum *ImportSummary) {
	var err error
	if roster.err != nil {
		err = imp.store.MarkConversationMemberCountUnknown(convID)
	} else {
		err = imp.store.SetConversationMemberCount(convID, roster.memberCount)
	}
	if err != nil {
		sum.Errors++
	}
}

// channelHasOwnRoster reports whether a channel's membership is separate from
// its team's. Standard channels inherit the team roster; private and shared
// channels have their own.
func channelHasOwnRoster(membershipType string) bool {
	switch strings.ToLower(membershipType) {
	case "private", "shared":
		return true
	default:
		return false
	}
}

// resolveTeamRoster fetches a team's members and resolves them to participants.
func (imp *Importer) resolveTeamRoster(ctx context.Context, teamID string) *memberRoster {
	members, err := imp.client.ListTeamMembers(ctx, teamID)
	if err != nil {
		return &memberRoster{err: err}
	}
	return imp.resolveRosterMembers(ctx, members)
}

// resolveChannelRoster fetches the members of a private or shared channel.
func (imp *Importer) resolveChannelRoster(ctx context.Context, teamID, channelID string) *memberRoster {
	members, err := imp.client.ListChannelMembers(ctx, teamID, channelID)
	if err != nil {
		return &memberRoster{err: err}
	}
	return imp.resolveRosterMembers(ctx, members)
}

// resolveChatRoster fetches a chat's members.
func (imp *Importer) resolveChatRoster(ctx context.Context, chatID string) *memberRoster {
	members, err := imp.client.ListChatMembers(ctx, chatID)
	if err != nil {
		return &memberRoster{err: err}
	}
	return imp.resolveRosterMembers(ctx, members)
}

func (imp *Importer) resolveRosterMembers(ctx context.Context, members []ChatMember) *memberRoster {
	roster := &memberRoster{
		participantIDs: make([]int64, 0, len(members)),
		memberCount:    len(members),
	}
	for _, m := range members {
		pid, rerr := imp.res.resolveMember(ctx, m)
		if rerr != nil || pid == 0 {
			continue
		}
		roster.participantIDs = append(roster.participantIDs, pid)
	}
	return roster
}

// teamChannels is one team's channel list, which decides whether a channel is
// governed by its own roster or by the team's.
type teamChannels struct {
	membershipTypes map[string]string
	err             error
}

// rosterResolver memoizes the Graph reads a media backfill needs to re-resolve
// channel membership: each team's channel list, and each roster it then fetches
// (keyed by team ID, or by "<teamID>/<channelID>" for private and shared
// channels). Every failure is counted once, when it is first observed.
type rosterResolver struct {
	imp      *Importer
	channels map[string]*teamChannels
	rosters  map[string]*memberRoster
}

func newRosterResolver(imp *Importer) *rosterResolver {
	return &rosterResolver{
		imp:      imp,
		channels: map[string]*teamChannels{},
		rosters:  map[string]*memberRoster{},
	}
}

// forConversation returns the roster governing one archived conversation:
// a chat's own members, or — for a "<teamID>/<channelID>" key — whichever of
// the channel's or its team's roster governs it.
func (r *rosterResolver) forConversation(
	ctx context.Context, ref store.MessageConversationRef, sum *ImportSummary,
) *memberRoster {
	if ref.Type != conversationTypeChannel {
		return r.cached("chat/"+ref.SourceConversationID, sum, func() *memberRoster {
			return r.imp.resolveChatRoster(ctx, ref.SourceConversationID)
		})
	}
	teamID, channelID, ok := strings.Cut(ref.SourceConversationID, "/")
	if !ok || teamID == "" || channelID == "" {
		// Not a "<teamID>/<channelID>" key, so there is no roster to fetch.
		return &memberRoster{err: fmt.Errorf(
			"channel conversation %q carries no team/channel key", ref.SourceConversationID)}
	}
	return r.forChannel(ctx, teamID, channelID, sum)
}

// forChannel returns the roster governing one channel.
func (r *rosterResolver) forChannel(ctx context.Context, teamID, channelID string, sum *ImportSummary) *memberRoster {
	channels, cached := r.channels[teamID]
	if !cached {
		channels = r.imp.listTeamChannels(ctx, teamID)
		r.channels[teamID] = channels
		if channels.err != nil {
			sum.Errors++
		}
	}
	if channels.err != nil {
		// Without the channel list there is no way to tell which roster
		// governs this channel, so membership stays unknown.
		return &memberRoster{err: channels.err}
	}
	membershipType, listed := channels.membershipTypes[channelID]
	if !listed {
		// The channel is no longer visible in Graph. A shared channel can hold
		// members the team does not, so the team roster is not a safe stand-in.
		return &memberRoster{err: fmt.Errorf("channel %s is not listed in team %s", channelID, teamID)}
	}
	if channelHasOwnRoster(membershipType) {
		return r.cached(teamID+"/"+channelID, sum, func() *memberRoster {
			return r.imp.resolveChannelRoster(ctx, teamID, channelID)
		})
	}
	return r.cached(teamID, sum, func() *memberRoster {
		return r.imp.resolveTeamRoster(ctx, teamID)
	})
}

func (r *rosterResolver) cached(key string, sum *ImportSummary, resolve func() *memberRoster) *memberRoster {
	if roster, ok := r.rosters[key]; ok {
		return roster
	}
	roster := resolve()
	r.rosters[key] = roster
	if roster.err != nil {
		sum.Errors++
	}
	return roster
}

// listTeamChannels reads a team's channels and indexes their membership types.
func (imp *Importer) listTeamChannels(ctx context.Context, teamID string) *teamChannels {
	channels, err := imp.client.ListChannels(ctx, teamID)
	if err != nil {
		return &teamChannels{err: err}
	}
	membershipTypes := make(map[string]string, len(channels))
	for _, ch := range channels {
		membershipTypes[ch.ID] = ch.MembershipType
	}
	return &teamChannels{membershipTypes: membershipTypes}
}

// refreshMembership re-resolves the roster of a conversation whose membership
// sync could not archive, and returns the participant count to evaluate media
// policy against. Sync fails closed on an unreadable roster using a count that
// only lives in memory, so the conversation is archived without one; read back
// as-is, the participant rows — which hold senders and every member ever seen —
// would relax the participant threshold and admit exactly the media it
// excluded. A roster that is still unreadable fails closed the same way sync
// does, and either outcome is archived so the next run starts from it.
func (imp *Importer) refreshMembership(
	ctx context.Context, messageID int64, opts ImportOptions,
	resolver *rosterResolver, sum *ImportSummary,
) int {
	unknownMembership := opts.MediaPolicy.MaxParticipants + 1
	ref, err := imp.store.MessageConversation(messageID)
	if err != nil {
		sum.Errors++
		return unknownMembership
	}
	roster := resolver.forConversation(ctx, ref, sum)
	imp.recordRosterOutcome(ref.ConversationID, roster, sum)
	if roster.err != nil {
		return unknownMembership
	}
	for _, pid := range roster.participantIDs {
		if cerr := imp.store.EnsureConversationParticipant(ref.ConversationID, pid, "member"); cerr != nil {
			sum.Errors++
		}
	}
	// Keep the conversation's own stats in step with the refreshed roster; the
	// archived record above is what policy evaluates.
	if rerr := imp.store.RecomputeConversationStatsForMessage(messageID); rerr != nil {
		sum.Errors++
	}
	return roster.memberCount
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
	inlineImagesChanged := imp.downloadInlineImages(ctx, messageID, gm.Body.Content, opts, sum)
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
			Filename:           recName,
			StoragePath:        recURL,
			SourceAttachmentID: "teams:recording:" + recURL,
		})
	}
	// Store attachment[] refs (reference/file/card) that carry a content URL.
	for _, att := range gm.Attachments {
		if att.ContentURL == "" {
			continue
		}
		attachmentID := att.ID
		if attachmentID == "" {
			attachmentID = att.ContentURL
		}
		linkAttachments = append(linkAttachments, store.AttachmentRef{
			Filename:           att.Name,
			MimeType:           att.ContentType,
			StoragePath:        att.ContentURL,
			SourceAttachmentID: "teams:link:" + attachmentID,
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
func (imp *Importer) downloadInlineImages(ctx context.Context, messageID int64, bodyHTML string, opts ImportOptions, sum *ImportSummary) bool {
	raws := hostedRe.FindAllString(bodyHTML, -1)
	if len(raws) == 0 {
		if err := imp.store.ReplaceMessageInlineAttachments(messageID, nil, false); err != nil {
			sum.Errors++
			return false
		}
		return true
	}
	existing, err := imp.store.MessageTeamsInlineAttachments(messageID)
	if err != nil {
		sum.Errors++
		return false
	}
	policy := opts.MediaPolicy
	maxBytes := policy.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 100 << 20
	}
	policy.MaxBytes = maxBytes

	seen := make(map[string]struct{}, len(raws))
	refs := make([]store.AttachmentRef, 0, len(raws))
	var copied int64
	replacementComplete := true
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
		sourceAttachmentID := "teams:inline:" + fetchPath
		if previous, ok := existing[sourceAttachmentID]; ok && previous.ContentHash != "" {
			refs = append(refs, previous)
			continue
		}
		marker := store.AttachmentRef{
			StoragePath: raw, SourceAttachmentID: sourceAttachmentID,
			State: attachmentpolicy.StatePending,
		}
		if previous, ok := existing[sourceAttachmentID]; ok && previous.Size > marker.Size {
			marker.Size = previous.Size
		}
		if opts.AttachmentsDir == "" {
			replacementComplete = false
			refs = append(refs, marker)
			continue
		}
		if reason := policy.Evaluate(opts.MediaConversation, int64(marker.Size)); reason != "" {
			replacementComplete = false
			marker.State = attachmentpolicy.StateSkipped
			marker.SkipReason = reason
			refs = append(refs, marker)
			sum.InlineImagesSkipped++
			continue
		}
		data, derr := imp.client.GetRawLimited(ctx, fetchPath, maxBytes)
		if derr != nil || len(data) == 0 {
			replacementComplete = false
			if errors.Is(derr, ErrMediaTooLarge) {
				marker.Size = attachmentpolicy.OversizeMarkerSize(maxBytes, int64(marker.Size))
				marker.State = attachmentpolicy.StateSkipped
				marker.SkipReason = attachmentpolicy.SkipSizeCap
				sum.InlineImagesSkipped++
			} else {
				marker.State = attachmentpolicy.StateFailed
				marker.SkipReason = attachmentpolicy.SkipFetchFailure
				sum.Errors++
			}
			refs = append(refs, marker)
			continue
		}
		att := &internalmime.Attachment{
			Filename:    "",
			ContentType: "",
			Content:     data,
		}
		storagePath, serr := export.StoreAttachmentFile(opts.AttachmentsDir, att)
		if serr != nil || storagePath == "" {
			replacementComplete = false
			marker.State = attachmentpolicy.StateFailed
			marker.SkipReason = attachmentpolicy.SkipFetchFailure
			refs = append(refs, marker)
			sum.Errors++
			continue
		}
		refs = append(refs, store.AttachmentRef{
			StoragePath:        storagePath,
			ContentHash:        att.ContentHash,
			Size:               len(data),
			SourceAttachmentID: sourceAttachmentID,
			Role:               store.AttachmentRoleInline,
			RoleSource:         store.AttachmentRoleSourceImporterSemantics,
			State:              attachmentpolicy.StateStored,
		})
		copied++
	}
	if err := imp.store.ReplaceMessageInlineAttachments(messageID, refs, !replacementComplete); err != nil {
		sum.Errors++
		return false
	}
	sum.InlineImagesCopied += copied
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
