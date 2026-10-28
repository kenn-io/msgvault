package matrix

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	SourceType           = "matrix"
	rawFormat            = "matrix_json"
	encryptedPlaceholder = store.MatrixEncryptedPlaceholder
	pageSize             = 100
)

var errRelationTargetMissing = errors.New("matrix relation target is not archived yet")

type RoomState struct {
	Backfilled bool   `json:"backfilled,omitzero"`
	PrevBatch  string `json:"prev_batch,omitempty"`
	// SyncedTo is the /sync next_batch through which this room's timeline is
	// archived; it stops the next gap walk.
	SyncedTo string `json:"synced_to,omitempty"`
	// GapFrom and GapTo hold an interrupted gap walk: the next page to read
	// and the next_batch of the run that stopped, archived from the gap onward.
	GapFrom           string   `json:"gap_from,omitempty"`
	GapTo             string   `json:"gap_to,omitempty"`
	DeferredRelations []string `json:"deferred_relations,omitempty"`
}

const syncStateFormatVersion = 1
const mediaDiscoveryVersion = 1

var errCursorVersion = errors.New("unsupported Matrix cursor format version")

type SyncState struct {
	FormatVersion            int                        `json:"format_version"`
	MediaDiscoveryVersion    int                        `json:"media_discovery_version,omitzero"`
	MediaDiscoveryScope      string                     `json:"media_discovery_scope,omitempty"`
	MediaDiscoveryAfter      int64                      `json:"media_discovery_after,omitzero"`
	MediaDiscoveryIncomplete bool                       `json:"media_discovery_incomplete,omitzero"`
	RecoveryRooms            map[string]recoveryAttempt `json:"recovery_rooms,omitzero"`
	NextBatch                string                     `json:"next_batch,omitempty"`
	Rooms                    map[string]*RoomState      `json:"rooms"`
}

func newSyncState() *SyncState {
	return &SyncState{FormatVersion: syncStateFormatVersion, Rooms: map[string]*RoomState{}}
}

func loadSyncState(blob string) (*SyncState, error) {
	state := newSyncState()
	if blob != "" {
		var version struct {
			FormatVersion int `json:"format_version"`
		}
		if err := json.Unmarshal([]byte(blob), &version); err != nil {
			return nil, fmt.Errorf("decode Matrix sync state: %w", err)
		}
		if version.FormatVersion > syncStateFormatVersion {
			return nil, fmt.Errorf("%w %d", errCursorVersion, version.FormatVersion)
		}
		if err := json.Unmarshal([]byte(blob), state); err != nil {
			return nil, fmt.Errorf("decode Matrix sync state: %w", err)
		}
		if state.MediaDiscoveryVersion > mediaDiscoveryVersion {
			return nil, fmt.Errorf("%w: media discovery %d", errCursorVersion, state.MediaDiscoveryVersion)
		}
	}
	state.FormatVersion = syncStateFormatVersion
	if state.Rooms == nil {
		state.Rooms = map[string]*RoomState{}
	}
	return state, nil
}

func (s *SyncState) marshal() (string, error) {
	b, err := json.Marshal(s, json.Deterministic(true))
	return string(b), err
}

type ImportOptions struct {
	NoMedia        bool
	AttachmentsDir string
	MediaPolicy    attachmentpolicy.Policy
	MediaMutation  func(context.Context, func() error) error
	UserID         string
	Full           bool
	SkipKeyBackup  bool
	Rooms          []string
	ExcludeRooms   []string
	Progress       func(string)
}

type ImportSummary struct {
	AttachmentsDownloaded int64
	AttachmentsPending    int64
	AttachmentsFailed     int64
	Recovered             int64 // Message rows replaced, excluding edits and reactions.
	BackupFailure         error
	PendingEncrypted      int64
	RoomsProcessed        int64
	MessagesProcessed     int64
	MessagesAdded         int64
	MessagesUpdated       int64
	ErrorsCount           int64
	EventsSkipped         int64
	RelationsUnresolved   int64
}

type Importer struct {
	store                  *store.Store
	runtime                *Runtime
	users                  map[id.UserID]int64
	newAfter               map[string]int64
	mediaAttempted         map[string]bool
	mediaUnavailable       *bool
	mediaTransientFailures *int
}

func NewImporter(s *store.Store, runtime *Runtime) *Importer {
	return &Importer{store: s, runtime: runtime, users: map[id.UserID]int64{}, newAfter: map[string]int64{}, mediaAttempted: map[string]bool{}, mediaUnavailable: new(bool), mediaTransientFailures: new(int)}
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, err error) {
	if opts.UserID == "" {
		return nil, errors.New("matrix user ID required")
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.UserID)
	if err != nil {
		return nil, err
	}
	imp.runtime.backupState = backupState{key: imp.runtime.key, skipBackup: opts.SkipKeyBackup}
	state := newSyncState()
	{
		var encoded string
		checkpoint, checkpointErr := imp.store.GetLatestCheckpointedSyncByType(source.ID, SourceType)
		if checkpointErr == nil && checkpoint.CursorBefore.Valid {
			encoded = checkpoint.CursorBefore.String
		} else if checkpointErr != nil && !errors.Is(checkpointErr, store.ErrSyncRunNotFound) {
			return nil, checkpointErr
		} else {
			prev, prevErr := imp.store.GetLastSuccessfulSyncByType(source.ID, SourceType)
			if prevErr == nil && prev.CursorAfter.Valid {
				encoded = prev.CursorAfter.String
			} else if prevErr != nil && !errors.Is(prevErr, store.ErrSyncRunNotFound) {
				return nil, prevErr
			}
		}
		if encoded != "" {
			state, err = loadSyncState(encoded)
			if err != nil && (!opts.Full || errors.Is(err, errCursorVersion)) {
				return nil, err
			}
		}
	}
	if opts.Full {
		state = newSyncState()
	}
	syncID, err := imp.store.StartSync(source.ID, SourceType)
	if err != nil {
		return nil, err
	}
	scoped := imp.store.ScopedToSync(source.ID, syncID)
	imp = NewImporter(scoped, imp.runtime)
	sum = &ImportSummary{}
	completed := false
	defer func() {
		if err != nil && !completed {
			_ = scoped.FailSync(syncID, err.Error())
		}
	}()

	since := state.NextBatch
	// mautrix omits an empty set_presence parameter, which makes the homeserver
	// mark the client online. The client-server spec defines "offline" as "the
	// client is not marked as being online when it uses this API": it does not
	// set the account offline. Synapse only updates presence for values other
	// than offline, so this leaves presence set by other clients untouched;
	// "unavailable" would mark the account idle.
	resp, err := imp.runtime.Client.SyncRequest(ctx, 0, since, "", since == "", event.Presence("offline"))
	if err != nil {
		return sum, fmt.Errorf("matrix sync: %w", err)
	}
	directRooms, err := imp.directRooms(ctx)
	if err != nil {
		return sum, err
	}
	if err = imp.reclassifyArchivedRooms(ctx, source.ID, directRooms); err != nil {
		return sum, err
	}
	roomIDs := make([]id.RoomID, 0, len(resp.Rooms.Join))
	for roomID := range resp.Rooms.Join {
		if roomIncluded(roomID.String(), opts.Rooms, opts.ExcludeRooms) {
			roomIDs = append(roomIDs, roomID)
		}
	}
	slices.Sort(roomIDs)
	for _, roomID := range roomIDs {
		room := resp.Rooms.Join[roomID]
		if err = imp.importRoom(ctx, source.ID, syncID, roomID, room, resp.NextBatch, directRooms[roomID], state, opts, sum); err != nil {
			return sum, err
		}
	}
	recoveryErr := imp.recoverPlaceholders(ctx, source.ID, state, opts, sum)
	if recoveryErr != nil || sum.BackupFailure != nil {
		sum.ErrorsCount++
	}
	if recoveryErr == nil {
		state.NextBatch = resp.NextBatch
	}
	if err = imp.checkpoint(syncID, state, sum); err != nil {
		return sum, err
	}
	if ctx.Err() != nil {
		return sum, errors.Join(recoveryErr, ctx.Err())
	}
	state.NextBatch = resp.NextBatch
	scope := mediaDiscoveryScope(opts)
	if state.MediaDiscoveryScope != scope {
		state.MediaDiscoveryVersion = 0
		state.MediaDiscoveryScope = scope
		state.MediaDiscoveryAfter, state.MediaDiscoveryIncomplete = 0, false
	}
	if state.MediaDiscoveryVersion < mediaDiscoveryVersion {
		complete, discoveryErr := imp.discoverLegacyMedia(ctx, source.ID, state, opts)
		if discoveryErr != nil {
			return sum, discoveryErr
		}
		if complete {
			state.MediaDiscoveryVersion = mediaDiscoveryVersion
			state.MediaDiscoveryScope = scope
		}
		if err = imp.checkpoint(syncID, state, sum); err != nil {
			return sum, err
		}
	}
	if err = imp.retryArchivedMedia(ctx, source.ID, opts, sum); err != nil {
		return sum, err
	}
	sum.AttachmentsPending, sum.AttachmentsFailed, err = scoped.MatrixOutstandingMedia(ctx, source.ID, opts.Rooms, opts.ExcludeRooms)
	if err != nil {
		return sum, fmt.Errorf("count outstanding Matrix media: %w", err)
	}
	if err = scoped.RecomputeConversationStatsContext(ctx, source.ID); err != nil {
		return sum, fmt.Errorf("recompute Matrix conversation stats: %w", err)
	}
	finalState, err := state.marshal()
	if err != nil {
		return sum, err
	}
	if err = scoped.CompleteSyncAndUpdateSourceCursor(syncID, source.ID, finalState); err != nil {
		return sum, err
	}
	completed = true
	return sum, recoveryErr
}

func (imp *Importer) reclassifyArchivedRooms(ctx context.Context, sourceID int64, directRooms map[id.RoomID]bool) error {
	rooms, err := imp.store.ListMatrixRoomIDsContext(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("list archived Matrix rooms: %w", err)
	}
	for _, roomID := range rooms {
		roomType := "group_chat"
		if directRooms[id.RoomID(roomID)] {
			roomType = "direct_chat"
		}
		if _, err := imp.store.EnsureConversationWithType(sourceID, roomID, roomType, ""); err != nil {
			return fmt.Errorf("reclassify Matrix room %s: %w", roomID, err)
		}
	}
	return nil
}

func roomIncluded(roomID string, include, exclude []string) bool {
	if slices.Contains(exclude, roomID) {
		return false
	}
	return len(include) == 0 || slices.Contains(include, roomID)
}

func (imp *Importer) directRooms(ctx context.Context) (map[id.RoomID]bool, error) {
	var direct event.DirectChatsEventContent
	if err := imp.runtime.Client.GetAccountData(ctx, event.AccountDataDirectChats.Type, &direct); err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			return map[id.RoomID]bool{}, nil
		}
		return nil, fmt.Errorf("load Matrix direct-chat map: %w", err)
	}
	out := map[id.RoomID]bool{}
	for _, rooms := range direct {
		for _, roomID := range rooms {
			out[roomID] = true
		}
	}
	return out, nil
}

func (imp *Importer) importRoom(ctx context.Context, sourceID, syncID int64, roomID id.RoomID, room *mautrix.SyncJoinedRoom, nextBatch string, direct bool, state *SyncState, opts ImportOptions, sum *ImportSummary) error {
	defer imp.runtime.releaseRoom(roomID)
	rs := state.Rooms[roomID.String()]
	hadRoomState := rs != nil
	gapTo := ""
	if rs == nil || opts.Full {
		// SyncedTo is set before the first checkpoint so an interruption still
		// bounds the next run's gap walk.
		rs = &RoomState{PrevBatch: room.Timeline.PrevBatch, SyncedTo: nextBatch}
		state.Rooms[roomID.String()] = rs
	} else {
		gapTo = rs.SyncedTo
	}
	roomType := "group_chat"
	if direct {
		roomType = "direct_chat"
	}
	title, titlePresent := roomTitle(room.State.Events)
	if timelineTitle, timelineTitlePresent := roomTitle(room.Timeline.Events); timelineTitlePresent {
		title = timelineTitle
		titlePresent = true
	}
	members, memberNames, err := imp.members(ctx, roomID)
	if err != nil {
		return err
	}
	// Current membership makes a useful initial fallback title, but an
	// incremental /sync usually omits room state. Do not replace a previously
	// archived room name with the member list on every incremental run.
	if title == "" && !titlePresent && (!hadRoomState || opts.Full) {
		title = textutil.SanitizeTerminal(strings.Join(memberNames, ", "))
	}
	convID, err := imp.store.EnsureConversationWithType(sourceID, roomID.String(), roomType, title)
	if err != nil {
		return fmt.Errorf("ensure Matrix room %s: %w", roomID, err)
	}
	if titlePresent && title == "" {
		if err := imp.store.SetConversationTitle(sourceID, convID, ""); err != nil {
			return fmt.Errorf("clear Matrix room %s title: %w", roomID, err)
		}
	}
	if err := imp.store.ReplaceConversationParticipants(convID, members); err != nil {
		return fmt.Errorf("replace Matrix room members: %w", err)
	}
	if err := imp.store.SetConversationMemberCount(convID, len(members)); err != nil {
		return err
	}
	conversation := attachmentpolicy.Conversation{Type: roomType, ParticipantCount: len(members)}
	deferred, err := decodeDeferredRelations(rs.DeferredRelations)
	if err != nil {
		return fmt.Errorf("decode deferred Matrix relations for room %s: %w", roomID, err)
	}
	deferredIDs := make(map[id.EventID]struct{}, len(deferred))
	for _, evt := range deferred {
		deferredIDs[evt.ID] = struct{}{}
	}
	rememberDeferred := func(evt *event.Event) error {
		if evt == nil || evt.ID == "" {
			return nil
		}
		evt.RoomID = roomID
		if _, exists := deferredIDs[evt.ID]; exists {
			return nil
		}
		raw, marshalErr := json.Marshal(evt, json.Deterministic(true))
		if marshalErr != nil {
			return fmt.Errorf("encode deferred Matrix relation %s: %w", evt.ID, marshalErr)
		}
		deferred = append(deferred, evt)
		deferredIDs[evt.ID] = struct{}{}
		rs.DeferredRelations = append(rs.DeferredRelations, string(raw))
		return nil
	}
	forgetDeferred := func(eventID id.EventID) error {
		if _, exists := deferredIDs[eventID]; !exists {
			return nil
		}
		delete(deferredIDs, eventID)
		kept := deferred[:0]
		for _, candidate := range deferred {
			if candidate.ID != eventID {
				kept = append(kept, candidate)
			}
		}
		deferred = kept
		rs.DeferredRelations = rs.DeferredRelations[:0]
		for _, candidate := range deferred {
			raw, err := json.Marshal(candidate, json.Deterministic(true))
			if err != nil {
				return fmt.Errorf("encode retained Matrix relation %s: %w", candidate.ID, err)
			}
			rs.DeferredRelations = append(rs.DeferredRelations, string(raw))
		}
		return nil
	}
	persist := func(evt *event.Event) error {
		if evt != nil {
			evt.RoomID = roomID
		}
		err := imp.persistEvent(ctx, sourceID, convID, evt, conversation, opts, sum)
		if errors.Is(err, errRelationTargetMissing) {
			return rememberDeferred(evt)
		}
		return err
	}
	ingest := func(evt *event.Event) error {
		if evt != nil && evt.Unsigned.RedactedBecause != nil {
			if err := forgetDeferred(evt.ID); err != nil {
				return err
			}
		}
		if shouldDeferRelation(evt) {
			return rememberDeferred(evt)
		}
		return persist(evt)
	}
	for _, evt := range room.Timeline.Events {
		if err := ingest(evt); err != nil {
			return err
		}
	}
	// Save relations before the first history request. A failed page fetch
	// must not advance past work that needs an older target.
	if err := imp.checkpoint(syncID, state, sum); err != nil {
		return err
	}
	saveProgress := func(string) error { return imp.checkpoint(syncID, state, sum) }
	saveGap := func(end string) error {
		if end != "" {
			rs.GapFrom = end
		}
		return saveProgress(end)
	}
	if rs.GapFrom != "" {
		// An earlier run stopped inside this gap. Finish it from the saved page;
		// that run had archived everything after the gap, through GapTo.
		if err := imp.paginate(ctx, roomID, rs.GapFrom, gapTo, ingest, saveGap); err != nil {
			return err
		}
		gapTo, rs.SyncedTo, rs.GapFrom, rs.GapTo = rs.GapTo, rs.GapTo, "", ""
		if err := saveProgress(""); err != nil {
			return err
		}
	}
	if gapTo != "" && room.Timeline.Limited {
		// The previous sync token stops the walk where this room's archive ends.
		rs.GapTo = nextBatch
		if err := imp.paginate(ctx, roomID, room.Timeline.PrevBatch, gapTo, ingest, saveGap); err != nil {
			return err
		}
	}
	rs.SyncedTo, rs.GapFrom, rs.GapTo = nextBatch, "", ""
	if err := imp.checkpoint(syncID, state, sum); err != nil {
		return err
	}
	if !rs.Backfilled {
		if err := imp.paginate(ctx, roomID, rs.PrevBatch, "", ingest, func(end string) error {
			rs.PrevBatch = end
			return saveProgress(end)
		}); err != nil {
			return err
		}
	}
	// /messages walks from new to old, so a relation may arrive before its
	// target. Replay all relations after the history is present, oldest first,
	// so neither an original body nor an older edit can overwrite the latest.
	slices.SortFunc(deferred, compareRelations)
	for _, evt := range deferred {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := imp.persistEvent(ctx, sourceID, convID, evt, conversation, opts, sum); err != nil {
			if errors.Is(err, errRelationTargetMissing) {
				sum.RelationsUnresolved++
				continue
			}
			return err
		}
	}
	if err := imp.resolvePendingReplies(ctx, sourceID, convID, sum); err != nil {
		return err
	}
	rs.Backfilled = true
	rs.DeferredRelations = nil
	sum.RoomsProcessed++
	if opts.Progress != nil {
		opts.Progress(fmt.Sprintf("%s: %d members", roomID, len(members)))
	}
	return imp.checkpoint(syncID, state, sum)
}

func compareRelations(a, b *event.Event) int {
	if priority := cmp.Compare(relationReplayPriority(a), relationReplayPriority(b)); priority != 0 {
		return priority
	}
	if a.Timestamp != b.Timestamp {
		return cmp.Compare(a.Timestamp, b.Timestamp)
	}
	return strings.Compare(a.ID.String(), b.ID.String())
}

func relationReplayPriority(evt *event.Event) int {
	if evt != nil && evt.Type == event.EventRedaction {
		return 1
	}
	return 0
}

func (imp *Importer) resolvePendingReplies(ctx context.Context, sourceID, convID int64, sum *ImportSummary) error {
	var afterID int64
	for {
		replies, err := imp.store.ListUnresolvedMessageRepliesAfterContext(ctx, sourceID, SourceType, afterID, pageSize, convID)
		if err != nil || len(replies) == 0 {
			return err
		}
		for _, reply := range replies {
			afterID = reply.MessageID
			var metadata struct {
				Target id.EventID `json:"referenced_message_id"`
			}
			if err := json.Unmarshal([]byte(reply.Metadata), &metadata); err != nil {
				return err
			}
			if metadata.Target == "" {
				continue
			}
			err = imp.resolveDeferredReply(ctx, sourceID, convID, &event.Event{ID: id.EventID(reply.SourceMessageID)}, metadata.Target)
			if errors.Is(err, errRelationTargetMissing) {
				sum.RelationsUnresolved++
			} else if err != nil {
				return err
			}
		}
	}
}

func (imp *Importer) resolveDeferredReply(ctx context.Context, sourceID, convID int64, evt *event.Event, target id.EventID) error {
	found, err := imp.store.MessageExistsBatch(sourceID, []string{evt.ID.String(), target.String()})
	if err != nil {
		return err
	}
	messageID, targetID := found[evt.ID.String()], found[target.String()]
	if messageID == 0 || targetID == 0 {
		return errRelationTargetMissing
	}
	targetMessage, err := imp.store.GetMessageRelationTarget(targetID)
	if err != nil {
		return err
	}
	if targetMessage.ConversationID != convID {
		return nil
	}
	return imp.store.SetMessageReplyContext(ctx, messageID, targetID)
}

// paginate walks room history backward from `from`, stopping at `to` when set.
func (imp *Importer) paginate(ctx context.Context, roomID id.RoomID, from, to string, ingest func(*event.Event) error, saved func(end string) error) error {
	for from != "" {
		page, err := imp.runtime.Client.Messages(ctx, roomID, from, to, mautrix.DirectionBackward, nil, pageSize)
		if err != nil {
			return fmt.Errorf("read Matrix room %s history: %w", roomID, err)
		}
		for _, evt := range slices.Backward(page.Chunk) {
			if err := ingest(evt); err != nil {
				return err
			}
		}
		// An empty page with an end token can still precede older history.
		if page.End == "" || page.End == from {
			return saved("")
		}
		from = page.End
		if err := saved(from); err != nil {
			return err
		}
	}
	return nil
}

// parseContent parses evt's content once and reports whether it is usable.
// Unsupported or malformed payloads are skipped so one bad event cannot stop
// every later sync.
func parseContent(evt *event.Event) bool {
	if evt.Content.Parsed != nil {
		return true
	}
	if evt.Content.ParseRaw(evt.Type) != nil {
		// ParseRaw leaves a partial value behind on failure.
		evt.Content.Parsed = nil
		return false
	}
	return true
}

func shouldDeferRelation(evt *event.Event) bool {
	if evt == nil {
		return false
	}
	switch evt.Type {
	case event.EventReaction, event.EventRedaction:
		return true
	case event.EventMessage, event.EventSticker:
		if !parseContent(evt) {
			return false
		}
		return evt.Content.AsMessage().RelatesTo.GetReplaceID() != ""
	default:
		return false
	}
}

func decodeDeferredRelations(rawEvents []string) ([]*event.Event, error) {
	events := make([]*event.Event, 0, len(rawEvents))
	for _, raw := range rawEvents {
		evt, err := decodeArchivedEvent([]byte(raw))
		if err != nil {
			return nil, err
		}
		events = append(events, evt)
	}
	return events, nil
}

func (imp *Importer) checkpoint(syncID int64, state *SyncState, sum *ImportSummary) error {
	blob, err := state.marshal()
	if err != nil {
		return err
	}
	return imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		PageToken: blob, MessagesProcessed: sum.MessagesProcessed, MessagesAdded: sum.MessagesAdded,
		MessagesUpdated: sum.MessagesUpdated, ErrorsCount: sum.ErrorsCount,
	})
}

func roomTitle(events []*event.Event) (string, bool) {
	for _, evt := range slices.Backward(events) {
		if evt.Type == event.StateRoomName {
			if evt.Content.Parsed == nil {
				_ = evt.Content.ParseRaw(evt.Type)
			}
			// Room names are set by any member with permission and are shown in
			// single-line TUI rows, so strip terminal control sequences here.
			return strings.TrimSpace(textutil.SanitizeTerminal(evt.Content.AsRoomName().Name)), true
		}
	}
	return "", false
}

func (imp *Importer) members(ctx context.Context, roomID id.RoomID) ([]store.ConversationParticipantRef, []string, error) {
	resp, err := imp.runtime.Client.JoinedMembers(ctx, roomID)
	if err != nil {
		return nil, nil, fmt.Errorf("list Matrix room %s members: %w", roomID, err)
	}
	refs := make([]store.ConversationParticipantRef, 0, len(resp.Joined))
	names := make([]string, 0, len(resp.Joined))
	for userID, member := range resp.Joined {
		pid, err := imp.participant(userID, member.DisplayName)
		if err != nil {
			return nil, nil, err
		}
		refs = append(refs, store.ConversationParticipantRef{ParticipantID: pid, Role: "member"})
		if userID != imp.runtime.Client.UserID {
			name := strings.TrimSpace(member.DisplayName)
			if name == "" {
				name = userID.String()
			}
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return refs, names, nil
}

func (imp *Importer) participant(userID id.UserID, displayName string) (int64, error) {
	if pid := imp.users[userID]; pid != 0 {
		return pid, nil
	}
	pid, err := imp.store.EnsureParticipantByIdentifier(SourceType, userID.String(), displayName)
	if err != nil {
		return 0, fmt.Errorf("ensure Matrix participant %s: %w", userID, err)
	}
	imp.users[userID] = pid
	return pid, nil
}

func (imp *Importer) persistEvent(ctx context.Context, sourceID, convID int64, evt *event.Event, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	if evt == nil || evt.ID == "" {
		return nil
	}
	if evt.Unsigned.RedactedBecause != nil {
		target := evt.ID
		if evt.Type == event.EventRedaction {
			// Redacting a redaction does not undo it; apply the original target.
			redaction := *evt
			redaction.Unsigned.RedactedBecause = nil
			if redaction.Content.Parsed == nil {
				_ = redaction.Content.ParseRaw(redaction.Type)
			}
			target = redaction.Redacts
			if target == "" {
				target = redaction.Content.AsRedaction().Redacts
			}
			if target == "" {
				return nil
			}
		}
		_, err := imp.redact(ctx, sourceID, convID, evt.RoomID, target, conversation, opts, sum)
		return err
	}
	if !parseContent(evt) {
		sum.EventsSkipped++
		return nil
	}
	raw, err := json.Marshal(evt, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode Matrix event %s: %w", evt.ID, err)
	}
	if evt.Type == event.EventEncrypted {
		return imp.persistMessage(ctx, sourceID, convID, evt, raw, encryptedPlaceholder, nil, conversation, opts, sum)
	}
	return imp.persistPlainEvent(ctx, sourceID, convID, evt, raw, conversation, opts, sum)
}

func (imp *Importer) persistPlainEvent(ctx context.Context, sourceID, convID int64, evt *event.Event, raw []byte, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	switch evt.Type {
	case event.EventMessage, event.EventSticker:
		content := mediaContent(evt)
		if target := content.RelatesTo.GetReplaceID(); target != "" {
			if content.NewContent == nil {
				return nil
			}
			return imp.persistEdit(ctx, sourceID, convID, evt, target, content.NewContent, conversation, opts, sum)
		}
		return imp.persistMessage(ctx, sourceID, convID, evt, raw, messageBody(evt.Content.AsMessage()), content, conversation, opts, sum)
	case event.EventRedaction:
		target := evt.Redacts
		if target == "" {
			target = evt.Content.AsRedaction().Redacts
		}
		if target == "" {
			return nil
		}
		handled, err := imp.redact(ctx, sourceID, convID, evt.RoomID, target, conversation, opts, sum)
		if err == nil && !handled {
			return errRelationTargetMissing
		}
		return err
	case event.EventReaction:
		return imp.persistReaction(sourceID, convID, evt)
	}
	return nil
}

// redact applies a Matrix redaction to whatever archived item the event ID
// names in this room, and reports whether it found one.
func (imp *Importer) redact(ctx context.Context, sourceID, convID int64, roomID id.RoomID, target id.EventID, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) (bool, error) {
	deleted, err := imp.store.DeleteReactionBySourceID(sourceID, convID, target.String())
	if err != nil {
		return deleted, err
	}
	var pendingID int64
	if !deleted {
		pendingID, err = imp.store.MatrixPendingEditTarget(ctx, convID, target.String())
		if err != nil {
			return false, err
		}
	}
	if pendingID != 0 {
		edits, err := imp.pendingEdits(pendingID)
		if err != nil {
			return false, err
		}
		edits = slices.DeleteFunc(edits, func(edit *event.Event) bool { return edit.ID == target })
		metadata, err := imp.mergedMetadata(pendingID, map[string]any{"matrix_pending_edits": edits})
		if err != nil {
			return false, err
		}
		if err := imp.store.SetMessageMetadataContext(ctx, pendingID, sql.NullString{String: string(metadata), Valid: true}); err != nil {
			return false, err
		}
		deleted = true
	}
	found, err := imp.store.MessageExistsBatch(sourceID, []string{target.String()})
	if err != nil {
		return false, err
	}
	if messageID := found[target.String()]; messageID != 0 {
		message, err := imp.store.GetMessageRelationTarget(messageID)
		if err != nil || message.ConversationID != convID {
			return true, err
		}
		if err := imp.store.MarkMessageDeleted(sourceID, target.String()); err != nil {
			return true, err
		}
		deleted = true
	}
	messageID, err := imp.store.MessageIDByMetadataValue(convID, editEventKey, target.String())
	if err != nil || messageID == 0 {
		return deleted, err
	}
	return true, imp.restoreAfterEditRedaction(ctx, sourceID, roomID, messageID, target, conversation, opts, sum)
}

func messageBody(content *event.MessageEventContent) string {
	if content == nil {
		return ""
	}
	if content.RelatesTo.GetReplyTo() != "" {
		// A reply's quoted fallback belongs to the message it quotes.
		withoutFallback := *content
		withoutFallback.RemoveReplyFallback()
		withoutFallback.Body = event.TrimReplyFallbackText(withoutFallback.Body)
		content = &withoutFallback
	}
	if content.MsgType.IsText() {
		if content.Format == event.FormatHTML && content.FormattedBody != "" {
			return textutil.SanitizeTerminalMultiline(msgmime.StripHTML(content.FormattedBody))
		}
		return textutil.SanitizeTerminalMultiline(content.Body)
	}
	var label string
	switch content.MsgType {
	case event.MsgImage:
		label = "[image]"
	case event.MsgVideo:
		label = "[video]"
	case event.MsgAudio:
		label = "[audio]"
	case event.MsgFile:
		label = "[file: " + content.GetFileName() + "]"
	default:
		return textutil.SanitizeTerminalMultiline(content.Body)
	}
	if caption := content.GetCaption(); caption != "" {
		label += " " + caption
	}
	return textutil.SanitizeTerminalMultiline(label)
}

func (imp *Importer) persistMessage(ctx context.Context, sourceID, convID int64, evt *event.Event, raw []byte, body string, content *event.MessageEventContent, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	var replyToMessageID int64
	replyTargetMissing := false
	if content != nil && content.RelatesTo != nil {
		if reply := content.RelatesTo.GetReplyTo(); reply != "" {
			found, err := imp.store.MessageExistsBatch(sourceID, []string{reply.String()})
			if err != nil {
				return err
			}
			replyToMessageID = found[reply.String()]
			if replyToMessageID == 0 {
				replyTargetMissing = true
			} else {
				target, err := imp.store.GetMessageRelationTarget(replyToMessageID)
				if err != nil {
					return err
				}
				if target.ConversationID != convID {
					replyToMessageID = 0
				}
			}
		}
	}
	existing, err := imp.store.MessageExistsBatch(sourceID, []string{evt.ID.String()})
	if err != nil {
		return err
	}
	if existingID := existing[evt.ID.String()]; existingID != 0 && !evt.Mautrix.WasEncrypted {
		// Matrix events never change, so only a reply link may still be missing.
		if replyToMessageID != 0 {
			return imp.store.SetMessageReplyContext(ctx, existingID, replyToMessageID)
		}
		if replyTargetMissing {
			return imp.rememberPendingReply(ctx, existingID, content.RelatesTo.GetReplyTo())
		}
		return nil
	}
	senderID, err := imp.participant(evt.Sender, "")
	if err != nil {
		return err
	}
	when := time.UnixMilli(evt.Timestamp).UTC()
	msg := &store.Message{
		ConversationID: convID, SourceID: sourceID, SourceMessageID: evt.ID.String(), MessageType: SourceType,
		SentAt: sql.NullTime{Time: when, Valid: evt.Timestamp > 0}, ReceivedAt: sql.NullTime{Time: when, Valid: evt.Timestamp > 0},
		SenderID: sql.NullInt64{Int64: senderID, Valid: senderID != 0}, IsFromMe: evt.Sender == imp.runtime.Client.UserID,
		Snippet: sql.NullString{String: snippet(body), Valid: body != ""}, SizeEstimate: int64(len(body)),
	}
	var download func() error
	persist := func(imp *Importer) error {
		s := imp.store
		messageID, err := s.PersistMessageContext(ctx, &store.MessagePersistData{
			Message: msg, BodyText: sql.NullString{String: body, Valid: body != ""}, RawMIME: raw, RawFormat: rawFormat,
			FTS: &store.FTSDoc{Body: body}, PreserveLabels: true,
		})
		if err != nil {
			return err
		}
		if existing[evt.ID.String()] != 0 && evt.Mautrix.WasEncrypted {
			if err := s.AdvanceDerivedDataRevisionContext(ctx); err != nil {
				return err
			}
		}
		if evt.Type == event.EventEncrypted || body == encryptedPlaceholder {
			metadata, err := imp.mergedMetadata(messageID, map[string]any{"matrix_event_type": evt.Type.Type})
			if err != nil {
				return err
			}
			if err := s.SetMessageMetadataContext(ctx, messageID, sql.NullString{String: string(metadata), Valid: true}); err != nil {
				return err
			}
		}
		if content != nil && content.MsgType.IsMedia() {
			if err := imp.prepareMedia(ctx, messageID, content, evt.Type, conversation, opts, sum, &download); err != nil {
				return err
			}
		}
		if evt.Type == event.EventEncrypted && existing[evt.ID.String()] == 0 {
			if _, tracked := imp.newAfter[evt.RoomID.String()]; !tracked {
				imp.newAfter[evt.RoomID.String()] = messageID - 1
			}
		}
		if replyToMessageID != 0 {
			return s.SetMessageReplyContext(ctx, messageID, replyToMessageID)
		}
		if replyTargetMissing {
			return imp.rememberPendingReply(ctx, messageID, content.RelatesTo.GetReplyTo())
		}
		return nil
	}
	if content != nil && content.MsgType.IsMedia() {
		worker := *imp
		err = imp.mediaTransaction(ctx, opts, func(s *store.Store) error {
			worker.store = s
			return persist(&worker)
		})
		worker.store = imp.store
		if err == nil && download != nil {
			err = download()
		}
	} else {
		err = persist(imp)
	}
	if err != nil {
		return fmt.Errorf("persist Matrix event %s: %w", evt.ID, err)
	}
	after, tracked := imp.newAfter[evt.RoomID.String()]
	if !evt.Mautrix.WasEncrypted || !tracked || existing[evt.ID.String()] <= after {
		sum.MessagesProcessed++
	}
	if evt.Mautrix.WasEncrypted {
		sum.Recovered++
		if existing[evt.ID.String()] != 0 {
			sum.MessagesUpdated++
		}
	} else {
		if evt.Type == event.EventEncrypted {
			sum.PendingEncrypted++
		}
		sum.MessagesAdded++
	}
	return nil
}

func (imp *Importer) rememberPendingReply(ctx context.Context, messageID int64, target id.EventID) error {
	metadata, err := imp.mergedMetadata(messageID, map[string]any{"referenced_message_id": target.String()})
	if err != nil {
		return err
	}
	return imp.store.SetMessageMetadataContext(ctx, messageID, sql.NullString{String: string(metadata), Valid: true})
}

// maxPendingEdits bounds the edits queued on an encrypted original. Replay
// applies them oldest first, so only the newest ones can affect the result.
const maxPendingEdits = 50

// editEventKey names the message metadata field holding the applied edit, so
// a later redaction of that edit can find its message.
const editEventKey = "matrix_edit_event_id"

const editTSKey = "matrix_edit_ts"

type appliedEdit struct {
	EventID string `json:"matrix_edit_event_id"`
	TS      int64  `json:"matrix_edit_ts"`
}

func (e appliedEdit) olderThan(evt *event.Event) bool {
	return e.EventID == "" || e.TS < evt.Timestamp || (e.TS == evt.Timestamp && e.EventID < evt.ID.String())
}

func (imp *Importer) persistEdit(ctx context.Context, sourceID, convID int64, evt *event.Event, target id.EventID, content *event.MessageEventContent, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	found, err := imp.store.MessageExistsBatch(sourceID, []string{target.String()})
	if err != nil {
		return err
	}
	messageID := found[target.String()]
	if messageID == 0 {
		return errRelationTargetMissing
	}
	targetMessage, err := imp.store.GetMessageRelationTarget(messageID)
	if err != nil {
		return err
	}
	if targetMessage.ConversationID != convID {
		return nil
	}
	original, err := imp.archivedEvent(messageID)
	if err != nil {
		return err
	}
	if original.Type == event.EventEncrypted {
		// The outer sender survives decryption, and replay rejects any other sender.
		if evt.Sender != original.Sender {
			return nil
		}
		edits, err := imp.pendingEdits(messageID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(edits, func(edit *event.Event) bool { return edit.ID == evt.ID }) {
			edits = append(edits, evt)
			if len(edits) > maxPendingEdits {
				slices.SortFunc(edits, compareRelations)
				edits = edits[len(edits)-maxPendingEdits:]
			}
			metadata, err := imp.mergedMetadata(messageID, map[string]any{"matrix_pending_edits": edits})
			if err != nil {
				return err
			}
			if err := imp.store.SetMessageMetadata(messageID, sql.NullString{String: string(metadata), Valid: true}); err != nil {
				return err
			}
		}
		return errRelationTargetMissing
	}
	if !validReplacement(original, evt) {
		return nil
	}
	current, err := imp.appliedEdit(messageID)
	if err != nil {
		return err
	}
	if !current.olderThan(evt) {
		return nil
	}
	return imp.applyEdit(ctx, messageID, editedBody(original, evt.Content.AsMessage().NewContent), content, original.Type, appliedEdit{EventID: evt.ID.String(), TS: evt.Timestamp}, conversation, opts, sum)
}

// validReplacement applies Matrix's replacement rules: same sender and event
// type, no state events, no edit of an edit.
func validReplacement(original, edit *event.Event) bool {
	if original.Sender != edit.Sender || original.Type != edit.Type || original.StateKey != nil || edit.StateKey != nil {
		return false
	}
	return original.Content.AsMessage().RelatesTo.GetReplaceID() == ""
}

// editedBody renders an edit's new content with the original's relation,
// which Matrix keeps for replacements, so a reply's quoted fallback stays out.
func editedBody(original *event.Event, content *event.MessageEventContent) string {
	withRelation := *content
	withRelation.RelatesTo = original.Content.AsMessage().RelatesTo
	return messageBody(&withRelation)
}

func decodeArchivedEvent(raw []byte) (*event.Event, error) {
	var evt event.Event
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, err
	}
	return &evt, nil
}

var errReadArchivedEvent = errors.New("read archived Matrix event")

func (imp *Importer) archivedEvent(messageID int64) (*event.Event, error) {
	raw, err := imp.store.GetMessageRaw(messageID)
	if err != nil {
		return nil, fmt.Errorf("%w %d: %w", errReadArchivedEvent, messageID, err)
	}
	original, err := decodeArchivedEvent(raw)
	if err != nil {
		return nil, fmt.Errorf("decode archived Matrix event %d: %w", messageID, err)
	}
	if original.Content.Parsed == nil {
		if err := original.Content.ParseRaw(original.Type); err != nil && !errors.Is(err, event.ErrUnsupportedContentType) {
			return nil, fmt.Errorf("parse archived Matrix event %s: %w", original.ID, err)
		}
	}
	return original, nil
}

func (imp *Importer) appliedEdit(messageID int64) (appliedEdit, error) {
	var edit appliedEdit
	metadata, err := imp.store.GetMessageMetadata(messageID)
	if err != nil || !metadata.Valid {
		return edit, err
	}
	if err := json.Unmarshal([]byte(metadata.String), &edit); err != nil {
		return edit, fmt.Errorf("decode Matrix edit metadata for message %d: %w", messageID, err)
	}
	return edit, nil
}

func (imp *Importer) applyEdit(ctx context.Context, messageID int64, body string, content *event.MessageEventContent, eventType event.Type, edit appliedEdit, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	var selected any = content
	if !content.MsgType.IsMedia() {
		selected = map[string]any{"msgtype": content.MsgType}
	}
	values := map[string]any{editEventKey: edit.EventID, editTSKey: edit.TS, "matrix_event_type": eventType.Type, "matrix_media_content": selected, "matrix_media_content_version": 1}
	metadata, err := imp.mergedMetadata(messageID, values)
	if err != nil {
		return err
	}
	return imp.selectMediaVersion(ctx, messageID, body, content, eventType, metadata, true, conversation, opts, sum)
}

func (imp *Importer) pendingEdits(messageID int64) ([]*event.Event, error) {
	metadata, err := imp.store.GetMessageMetadata(messageID)
	if err != nil || !metadata.Valid {
		return nil, err
	}
	var pending struct {
		Edits []*event.Event `json:"matrix_pending_edits"`
	}
	if err := json.Unmarshal([]byte(metadata.String), &pending); err != nil {
		return nil, err
	}
	return pending.Edits, nil
}

func (imp *Importer) replayPendingEdits(ctx context.Context, sourceID, convID, messageID int64, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	edits, err := imp.pendingEdits(messageID)
	if err != nil || len(edits) == 0 {
		return err
	}
	slices.SortFunc(edits, compareRelations)
	for _, evt := range edits {
		if err := imp.persistEvent(ctx, sourceID, convID, evt, conversation, opts, sum); err != nil {
			return err
		}
	}
	metadata, err := imp.mergedMetadata(messageID, map[string]any{"matrix_pending_edits": nil})
	if err != nil {
		return err
	}
	return imp.store.SetMessageMetadataContext(ctx, messageID, sql.NullString{String: string(metadata), Valid: true})
}

func (imp *Importer) setBody(messageID int64, body string) error {
	if err := imp.store.UpdateMessageDerivedText(messageID,
		sql.NullString{String: body, Valid: body != ""}, sql.NullString{},
		sql.NullString{String: snippet(body), Valid: body != ""}, store.FTSDoc{Body: body}); err != nil {
		return err
	}
	return imp.store.SetMessageSizeEstimate(messageID, int64(len(body)))
}

// restoreAfterEditRedaction asks the homeserver for the original's surviving
// edits and shows the newest one, or the original text when none is left.
func (imp *Importer) restoreAfterEditRedaction(ctx context.Context, sourceID int64, roomID id.RoomID, messageID int64, redacted id.EventID, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) error {
	original, err := imp.archivedEvent(messageID)
	if err != nil {
		return err
	}
	var newest *event.Event
	var newestContent *event.MessageEventContent
	var claims map[string]string
	var earliest map[[sha256.Size]byte]id.EventID
	from := ""
	for {
		page, err := imp.runtime.Client.GetRelations(ctx, roomID, original.ID, &mautrix.ReqGetRelations{RelationType: event.RelReplace, From: from})
		if err != nil {
			return fmt.Errorf("list Matrix edits of %s: %w", original.ID, err)
		}
		for _, edit := range page.Chunk {
			replayKey := ""
			var conversationID int64
			if edit == nil || edit.ID == redacted {
				continue
			}
			if edit.Type == event.EventEncrypted {
				edit.RoomID = roomID
				target, err := imp.store.GetMessageRelationTarget(messageID)
				if err != nil {
					return err
				}
				plain, index, decryptErr := imp.runtime.decryptWithIndex(ctx, edit)
				if decryptErr != nil {
					if err := imp.persistEvent(ctx, sourceID, target.ConversationID, edit, conversation, opts, &ImportSummary{}); err != nil {
						return err
					}
					continue
				}
				// The relation sits outside the ciphertext, so a copy can target another message.
				if key, ok := ciphertextKey(edit); ok {
					if earliest == nil {
						if earliest, err = imp.earliestCiphertextCopies(ctx, target.ConversationID); err != nil {
							return err
						}
					}
					if first := earliest[key]; first != "" && first != edit.ID {
						continue
					}
				}
				replayKey = fmt.Sprintf("matrix_megolm_%s_%d", edit.Content.AsEncrypted().SessionID, index)
				conversationID = target.ConversationID
				edit = plain
			}
			if edit.Content.Parsed == nil && edit.Content.ParseRaw(edit.Type) != nil {
				continue
			}
			content := mediaContent(edit)
			if content.NewContent == nil || content.RelatesTo.GetReplaceID() != original.ID || !validReplacement(original, edit) {
				continue
			}
			if replayKey != "" {
				allowed, err := imp.claimRecoveredEvent(ctx, conversationID, true, replayKey, edit.ID.String(), &claims)
				if err != nil {
					return err
				}
				if !allowed {
					continue
				}
			}
			if newest == nil || (appliedEdit{EventID: newest.ID.String(), TS: newest.Timestamp}).olderThan(edit) {
				newest, newestContent = edit, content.NewContent
			}
		}
		if page.NextBatch == "" || page.NextBatch == from {
			break
		}
		from = page.NextBatch
	}
	if newest != nil {
		return imp.applyEdit(ctx, messageID, editedBody(original, newest.Content.AsMessage().NewContent), newestContent, original.Type, appliedEdit{EventID: newest.ID.String(), TS: newest.Timestamp}, conversation, opts, sum)
	}
	metadata, err := imp.mergedMetadata(messageID, map[string]any{editEventKey: nil, editTSKey: nil, "matrix_media_content": nil, "matrix_media_content_version": nil})
	if err != nil {
		return err
	}
	return imp.selectMediaVersion(ctx, messageID, messageBody(original.Content.AsMessage()), mediaContent(original), original.Type, metadata, false, conversation, opts, sum)
}

func (imp *Importer) persistReaction(sourceID, convID int64, evt *event.Event) error {
	relation := evt.Content.AsReaction().RelatesTo
	found, err := imp.store.MessageExistsBatch(sourceID, []string{relation.EventID.String()})
	if err != nil {
		return err
	}
	messageID := found[relation.EventID.String()]
	if messageID == 0 {
		return errRelationTargetMissing
	}
	target, err := imp.store.GetMessageRelationTarget(messageID)
	if err != nil {
		return err
	}
	if target.ConversationID != convID {
		return nil
	}
	participantID, err := imp.participant(evt.Sender, "")
	if err != nil {
		return err
	}
	return imp.store.UpsertReactionWithSourceID(
		messageID, participantID, "emoji", relation.Key, evt.ID.String(), time.UnixMilli(evt.Timestamp).UTC(),
	)
}

func snippet(body string) string {
	runes := []rune(body)
	if len(runes) > 100 {
		return string(runes[:100])
	}
	return body
}

func (imp *Importer) mergedMetadata(messageID int64, values map[string]any) ([]byte, error) {
	stored, err := imp.store.GetMessageMetadata(messageID)
	if err != nil {
		return nil, err
	}
	metadata := map[string]any{}
	if stored.Valid {
		if err := checkMediaContentVersion([]byte(stored.String)); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(stored.String), &metadata); err != nil {
			return nil, err
		}
	}
	for key, value := range values {
		if value == nil {
			delete(metadata, key)
		} else {
			metadata[key] = value
		}
	}
	return json.Marshal(metadata, json.Deterministic(true))
}
