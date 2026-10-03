package matrix

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

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
	encryptedPlaceholder = "[encrypted message — keys unavailable]"
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

type SyncState struct {
	NextBatch string                `json:"next_batch,omitempty"`
	Rooms     map[string]*RoomState `json:"rooms"`
	// Undecryptable only carries entries written before pending events moved
	// to matrix_undecryptable_events. Import migrates and clears it on load.
	Undecryptable map[string]UndecryptableEvent `json:"undecryptable,omitempty"`
	// PendingInStore marks state written once pending events live in the store.
	PendingInStore bool `json:"pending_in_store"`
	recoveryKnown  bool
}

type UndecryptableEvent struct {
	RoomID         string `json:"room_id"`
	EncryptedEvent string `json:"encrypted_event,omitempty"`
}

func newSyncState() *SyncState {
	return &SyncState{Rooms: map[string]*RoomState{}, PendingInStore: true, recoveryKnown: true}
}

func loadSyncState(blob string) (*SyncState, error) {
	state := newSyncState()
	if blob != "" {
		var fields map[string]any
		if err := json.Unmarshal([]byte(blob), &fields); err != nil {
			return nil, fmt.Errorf("decode Matrix sync state: %w", err)
		}
		_, legacyKnown := fields["undecryptable"]
		_, storeKnown := fields["pending_in_store"]
		state.recoveryKnown = legacyKnown || storeKnown
		if err := json.Unmarshal([]byte(blob), state); err != nil {
			return nil, fmt.Errorf("decode Matrix sync state: %w", err)
		}
	}
	if state.Rooms == nil {
		state.Rooms = map[string]*RoomState{}
	}
	state.PendingInStore = true
	return state, nil
}

func (s *SyncState) marshal() (string, error) {
	b, err := json.Marshal(s, json.Deterministic(true))
	return string(b), err
}

type ImportOptions struct {
	UserID       string
	Full         bool
	Rooms        []string
	ExcludeRooms []string
	Progress     func(string)
}

type ImportSummary struct {
	RoomsProcessed         int64
	MessagesProcessed      int64
	MessagesAdded          int64
	Undecryptable          int64
	UndecryptableRecovered int64
	EventsSkipped          int64
	RelationsUnresolved    int64
}

type Importer struct {
	store   *store.Store
	runtime *Runtime
	users   map[id.UserID]int64
}

func NewImporter(s *store.Store, runtime *Runtime) *Importer {
	return &Importer{store: s, runtime: runtime, users: map[id.UserID]int64{}}
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, err error) {
	if opts.UserID == "" {
		return nil, errors.New("matrix user ID required")
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.UserID)
	if err != nil {
		return nil, err
	}
	state := newSyncState()
	if opts.Full {
		// A full history replay resets cursors, but encrypted placeholders may
		// outlive homeserver retention. Re-seed them from the local archive so
		// newly arrived keys still recover ciphertext omitted by the replay.
		state.recoveryKnown = false
	} else {
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
			if err != nil {
				return nil, err
			}
		} else {
			// A first sync may follow a subset export, which starts Matrix sync
			// over but keeps the placeholders it copied.
			state.recoveryKnown = false
		}
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
	if err = imp.migrateLegacyUndecryptable(source.ID, state); err != nil {
		return sum, err
	}
	if imp.runtime.canDecrypt() && !state.recoveryKnown {
		if err = imp.seedLegacyUndecryptable(ctx, source.ID); err != nil {
			return sum, err
		}
		state.recoveryKnown = true
		if err = imp.checkpoint(syncID, state, sum); err != nil {
			return sum, err
		}
	}
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
	imp.runtime.ProcessSync(ctx, resp, since)
	directRooms, err := imp.directRooms(ctx)
	if err != nil {
		return sum, err
	}
	if err = imp.reclassifyArchivedRooms(source.ID, state, directRooms); err != nil {
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
	state.NextBatch = resp.NextBatch
	settleDeferred, err := imp.retryUndecryptable(ctx, source.ID, state, opts, sum)
	if err != nil {
		return sum, err
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
	// Pending rows kept alive until the cursor stored their deferred
	// relations can be retired now.
	if err = settleDeferred(); err != nil {
		return sum, err
	}
	return sum, nil
}

func (imp *Importer) reclassifyArchivedRooms(sourceID int64, state *SyncState, directRooms map[id.RoomID]bool) error {
	for roomID := range state.Rooms {
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
	// The ciphertext of a decrypted event is the proof that it was archived,
	// so it is retained only once the decrypted event is stored. A relation
	// carried in the room state is stored only by the next checkpoint, so its
	// ciphertext waits for one.
	var awaitingCheckpoint []retainedCiphertext
	checkpointRoom := func() error {
		if err := imp.checkpoint(syncID, state, sum); err != nil {
			return err
		}
		for _, item := range awaitingCheckpoint {
			if err := imp.retainEncryptedRaw(sourceID, item.eventID, item.roomID, item.ciphertext); err != nil {
				return err
			}
		}
		awaitingCheckpoint = nil
		return nil
	}
	persist := func(evt *event.Event) error {
		if evt != nil {
			evt.RoomID = roomID
		}
		if err := imp.recordLegacyCiphertext(sourceID, evt); err != nil {
			return err
		}
		err := imp.persistEvent(ctx, sourceID, convID, evt, sum)
		if errors.Is(err, errRelationTargetMissing) {
			candidate, candidateErr := imp.deferredRelationAfterPersist(sourceID, evt)
			if candidateErr != nil {
				return candidateErr
			}
			return rememberDeferred(candidate)
		}
		return err
	}
	ingest := func(evt *event.Event) error {
		if evt != nil && evt.Unsigned.RedactedBecause != nil {
			if err := forgetDeferred(evt.ID); err != nil {
				return err
			}
		}
		if evt != nil {
			// /sync timeline events omit room_id, and Megolm sessions are
			// looked up by room, so set it before the first decryption.
			evt.RoomID = roomID
		}
		candidate, ciphertext, deferRelation, deferErr := imp.relationForDeferral(ctx, evt)
		if deferErr != nil {
			return deferErr
		}
		if deferRelation {
			if err := rememberDeferred(candidate); err != nil {
				return err
			}
			if ciphertext != nil {
				awaitingCheckpoint = append(awaitingCheckpoint, retainedCiphertext{evt.ID.String(), roomID.String(), ciphertext})
			}
			return nil
		}
		if err := persist(candidate); err != nil {
			return err
		}
		if ciphertext == nil {
			return nil
		}
		if _, carried := deferredIDs[candidate.ID]; carried {
			awaitingCheckpoint = append(awaitingCheckpoint, retainedCiphertext{evt.ID.String(), roomID.String(), ciphertext})
			return nil
		}
		return imp.retainEncryptedRaw(sourceID, evt.ID.String(), roomID.String(), ciphertext)
	}
	for _, evt := range room.Timeline.Events {
		if err := ingest(evt); err != nil {
			return err
		}
	}
	// Save relations before the first history request. A failed page fetch
	// must not advance past work that needs an older target.
	if err := checkpointRoom(); err != nil {
		return err
	}
	saveProgress := func(string) error { return checkpointRoom() }
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
	if err := checkpointRoom(); err != nil {
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
	slices.SortFunc(deferred, func(a, b *event.Event) int {
		if priority := cmp.Compare(relationReplayPriority(a), relationReplayPriority(b)); priority != 0 {
			return priority
		}
		if a.Timestamp != b.Timestamp {
			return cmp.Compare(a.Timestamp, b.Timestamp)
		}
		return strings.Compare(a.ID.String(), b.ID.String())
	})
	// History is complete here, so a relation whose target is still missing
	// points at something never archived and is dropped. An encrypted
	// placeholder is an archived target: replies, reactions and redactions
	// apply to it, and its edits are read from the homeserver's relations
	// once it is decrypted.
	for _, evt := range deferred {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := imp.replayDeferredRelation(ctx, sourceID, convID, evt, sum); err != nil {
			if errors.Is(err, errRelationTargetMissing) {
				if finalizeErr := imp.finalizeDeferredRecovery(sourceID, evt, sum); finalizeErr != nil {
					return finalizeErr
				}
				sum.RelationsUnresolved++
				continue
			}
			return err
		}
		if err := imp.finalizeDeferredRecovery(sourceID, evt, sum); err != nil {
			return err
		}
	}
	rs.Backfilled = true
	rs.DeferredRelations = nil
	sum.RoomsProcessed++
	if opts.Progress != nil {
		opts.Progress(fmt.Sprintf("%s: %d members", roomID, len(members)))
	}
	return checkpointRoom()
}

// recordLegacyCiphertext stores the ciphertext of a pending event that was
// migrated without it, before the placeholder message is replaced.
func (imp *Importer) recordLegacyCiphertext(sourceID int64, evt *event.Event) error {
	if evt == nil || evt.Type != event.EventEncrypted {
		return nil
	}
	pending, exists, err := imp.store.MatrixUndecryptableEvent(sourceID, evt.ID.String())
	if err != nil || !exists || len(pending.RawEvent) > 0 {
		return err
	}
	raw, err := json.Marshal(evt, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode legacy pending Matrix event %s: %w", evt.ID, err)
	}
	if pending.RoomID == "" {
		pending.RoomID = evt.RoomID.String()
	}
	pending.RawEvent = raw
	return imp.store.PutMatrixUndecryptableEvents(sourceID, []store.MatrixUndecryptableEvent{pending})
}

func relationReplayPriority(evt *event.Event) int {
	if evt != nil && evt.Type == event.EventRedaction {
		return 1
	}
	return 0
}

func (imp *Importer) replayDeferredRelation(ctx context.Context, sourceID, convID int64, evt *event.Event, sum *ImportSummary) error {
	if evt != nil && (evt.Type == event.EventMessage || evt.Type == event.EventSticker) {
		if !parseContent(evt) {
			sum.EventsSkipped++
			return nil
		}
		content := evt.Content.AsMessage()
		if content.RelatesTo.GetReplaceID() == "" && content.RelatesTo.GetReplyTo() != "" {
			return imp.resolveDeferredReply(ctx, sourceID, convID, evt, content.RelatesTo.GetReplyTo())
		}
	}
	return imp.persistEvent(ctx, sourceID, convID, evt, sum)
}

func (imp *Importer) deferredRelationAfterPersist(sourceID int64, evt *event.Event) (*event.Event, error) {
	if evt == nil || evt.Type != event.EventEncrypted {
		return evt, nil
	}
	found, err := imp.store.MessageExistsBatch(sourceID, []string{evt.ID.String()})
	if err != nil {
		return nil, err
	}
	messageID := found[evt.ID.String()]
	if messageID == 0 {
		return evt, nil
	}
	raw, err := imp.store.GetMessageRaw(messageID)
	if err != nil {
		return nil, err
	}
	var retained event.Event
	if err := json.Unmarshal(raw, &retained); err != nil {
		return nil, fmt.Errorf("decode retained Matrix event %s: %w", evt.ID, err)
	}
	if retained.Type == event.EventEncrypted {
		return evt, nil
	}
	retained.RoomID = evt.RoomID
	return &retained, nil
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

func (imp *Importer) finalizeDeferredRecovery(sourceID int64, evt *event.Event, sum *ImportSummary) error {
	if evt == nil || eventPersistsOwnMessage(evt) {
		return nil
	}
	eventID := evt.ID.String()
	found, err := imp.store.MessageExistsBatch(sourceID, []string{eventID})
	if err != nil {
		return err
	}
	if found[eventID] == 0 {
		return imp.store.DeleteMatrixUndecryptableEvent(sourceID, eventID)
	}
	changed, err := imp.store.MarkMessageDeletedIfActive(sourceID, eventID)
	if err != nil {
		return err
	}
	if err := imp.store.DeleteMatrixUndecryptableEvent(sourceID, eventID); err != nil {
		return err
	}
	if changed {
		sum.UndecryptableRecovered++
	}
	return nil
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

// retainedCiphertext is the original ciphertext of a decrypted event, kept
// once the decrypted event is durable.
type retainedCiphertext struct {
	eventID, roomID string
	ciphertext      []byte
}

// relationForDeferral decrypts an encrypted event so that a relation can be
// deferred as its plaintext. It returns the original ciphertext when it
// decrypted the event; the caller retains it only after the decrypted event or
// the deferred relation is durable.
func (imp *Importer) relationForDeferral(ctx context.Context, evt *event.Event) (candidate *event.Event, ciphertext []byte, deferRelation bool, err error) {
	if evt == nil || evt.Type != event.EventEncrypted || imp.runtime == nil || !imp.runtime.canDecrypt() {
		return evt, nil, shouldDeferRelation(evt), nil
	}
	if !parseContent(evt) {
		// The normal persistence path counts and skips this event.
		return evt, nil, false, nil
	}
	decrypted, err := imp.runtime.decrypt(ctx, evt)
	if err != nil {
		// The normal persistence path retains this encrypted event as a
		// placeholder and retries it after more keys arrive.
		//nolint:nilerr // A non-decryptable event is not a relation to defer.
		return evt, nil, false, nil
	}
	ciphertext, err = json.Marshal(evt, json.Deterministic(true))
	if err != nil {
		return nil, nil, false, fmt.Errorf("encode encrypted Matrix event %s: %w", evt.ID, err)
	}
	decrypted.RoomID = evt.RoomID
	return decrypted, ciphertext, shouldDeferRelation(decrypted), nil
}

func (imp *Importer) retainEncryptedRaw(sourceID int64, eventID, roomID string, raw []byte) error {
	if err := imp.store.StoreMatrixEncryptedEvent(sourceID, eventID, roomID, raw); err != nil {
		return fmt.Errorf("retain encrypted Matrix event %s: %w", eventID, err)
	}
	return nil
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
		var evt event.Event
		if err := json.Unmarshal([]byte(raw), &evt); err != nil {
			return nil, err
		}
		events = append(events, &evt)
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

func (imp *Importer) persistEvent(ctx context.Context, sourceID, convID int64, evt *event.Event, sum *ImportSummary) error {
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
		_, err := imp.redact(ctx, sourceID, convID, evt.RoomID, target)
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
		return imp.persistEncryptedEvent(ctx, sourceID, convID, evt, raw, sum)
	}
	return imp.persistPlainEvent(ctx, sourceID, convID, evt, raw, sum)
}

// persistEncryptedEvent archives the decrypted event, or a searchable
// placeholder that later syncs retry once a key arrives.
func (imp *Importer) persistEncryptedEvent(ctx context.Context, sourceID, convID int64, evt *event.Event, raw []byte, sum *ImportSummary) error {
	encryptedEventID := evt.ID.String()
	var decrypted *event.Event
	decryptErr := errors.New("matrix crypto is unavailable")
	if imp.runtime.canDecrypt() {
		decrypted, decryptErr = imp.runtime.decrypt(ctx, evt)
	}
	if decryptErr != nil {
		// An event decrypted by an earlier run is already archived, so a
		// replay without its key must not add a placeholder for it.
		decryptedBefore, err := imp.decryptedBefore(sourceID, encryptedEventID)
		if err != nil || decryptedBefore {
			return err
		}
		// A run that stopped after storing the plaintext but before retaining
		// the ciphertext left a decrypted row: finish that run's write.
		plaintextStored, err := imp.plaintextStored(sourceID, encryptedEventID)
		if err != nil {
			return err
		}
		if plaintextStored {
			return imp.retainEncryptedRaw(sourceID, encryptedEventID, evt.RoomID.String(), raw)
		}
		if err := imp.rememberUndecryptable(sourceID, encryptedEventID, evt.RoomID.String(), raw); err != nil {
			return err
		}
		sum.Undecryptable++
		return imp.persistMessage(ctx, sourceID, convID, evt, raw, encryptedPlaceholder, nil, sum)
	}
	ciphertext, roomID := raw, evt.RoomID.String()
	evt = decrypted
	if !parseContent(evt) {
		sum.EventsSkipped++
		if err := imp.retainEncryptedRaw(sourceID, encryptedEventID, roomID, ciphertext); err != nil {
			return err
		}
		return imp.discardRecoveredUnsupported(sourceID, encryptedEventID)
	}
	raw, err := json.Marshal(evt, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode decrypted Matrix event %s: %w", evt.ID, err)
	}
	// The retained ciphertext is the proof that this event was archived, so it
	// is written only once the decrypted event is stored (or deferred as a
	// relation whose target is not archived yet). A failed write then leaves
	// the event to be decrypted again rather than assumed archived.
	persistErr := imp.persistPlainEvent(ctx, sourceID, convID, evt, raw, sum)
	if persistErr != nil && !errors.Is(persistErr, errRelationTargetMissing) {
		return persistErr
	}
	if err := imp.retainEncryptedRaw(sourceID, encryptedEventID, roomID, ciphertext); err != nil {
		return err
	}
	if persistErr != nil {
		return persistErr
	}
	if !eventPersistsOwnMessage(evt) {
		if _, err := imp.store.MarkMessageDeletedIfActive(sourceID, encryptedEventID); err != nil {
			return err
		}
	}
	return imp.store.DeleteMatrixUndecryptableEvent(sourceID, encryptedEventID)
}

// decryptedBefore reports whether this source already retained the
// ciphertext of a decrypted event with this ID.
func (imp *Importer) decryptedBefore(sourceID int64, eventID string) (bool, error) {
	_, _, err := imp.store.MatrixEncryptedEvent(sourceID, eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// plaintextStored reports whether this event's archived row holds its
// decrypted payload rather than an encrypted placeholder.
func (imp *Importer) plaintextStored(sourceID int64, eventID string) (bool, error) {
	found, err := imp.store.MessageExistsBatch(sourceID, []string{eventID})
	if err != nil || found[eventID] == 0 {
		return false, err
	}
	raw, err := imp.store.GetMessageRaw(found[eventID])
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var archived event.Event
	if err := json.Unmarshal(raw, &archived); err != nil {
		return false, fmt.Errorf("decode archived Matrix event %s: %w", eventID, err)
	}
	return archived.Type != event.EventEncrypted, nil
}

func (imp *Importer) persistPlainEvent(ctx context.Context, sourceID, convID int64, evt *event.Event, raw []byte, sum *ImportSummary) error {
	switch evt.Type {
	case event.EventMessage, event.EventSticker:
		content := evt.Content.AsMessage()
		if target := content.RelatesTo.GetReplaceID(); target != "" {
			if content.NewContent == nil {
				return nil
			}
			return imp.persistEdit(sourceID, convID, evt, target, content.NewContent)
		}
		return imp.persistMessage(ctx, sourceID, convID, evt, raw, messageBody(content), content, sum)
	case event.EventRedaction:
		target := evt.Redacts
		if target == "" {
			target = evt.Content.AsRedaction().Redacts
		}
		if target == "" {
			return nil
		}
		handled, err := imp.redact(ctx, sourceID, convID, evt.RoomID, target)
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
func (imp *Importer) redact(ctx context.Context, sourceID, convID int64, roomID id.RoomID, target id.EventID) (bool, error) {
	// A redacted event must not be decrypted and restored by a later retry.
	if err := imp.store.DeleteMatrixUndecryptableEvent(sourceID, target.String()); err != nil {
		return false, err
	}
	deleted, err := imp.store.DeleteReactionBySourceID(sourceID, convID, target.String())
	if err != nil || deleted {
		return deleted, err
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
		return true, imp.store.MarkMessageDeleted(sourceID, target.String())
	}
	messageID, err := imp.store.MessageIDByMetadataValue(convID, editEventKey, target.String())
	if err != nil || messageID == 0 {
		return false, err
	}
	return true, imp.restoreAfterEditRedaction(ctx, roomID, messageID, target)
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

func (imp *Importer) persistMessage(ctx context.Context, sourceID, convID int64, evt *event.Event, raw []byte, body string, content *event.MessageEventContent, sum *ImportSummary) error {
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
	existingID := existing[evt.ID.String()]
	recovering := false
	if existingID != 0 && evt.Type != event.EventEncrypted {
		// A pending event's row is still the encrypted placeholder.
		if _, recovering, err = imp.store.MatrixUndecryptableEvent(sourceID, evt.ID.String()); err != nil {
			return err
		}
	}
	if existingID != 0 && !recovering {
		// Matrix events never change, so only a reply link may still be missing.
		if replyToMessageID != 0 {
			return imp.store.SetMessageReplyContext(ctx, existingID, replyToMessageID)
		}
		if replyTargetMissing {
			return errRelationTargetMissing
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
	messageID, err := imp.store.PersistMessageContext(ctx, &store.MessagePersistData{
		Message: msg, BodyText: sql.NullString{String: body, Valid: body != ""}, RawMIME: raw, RawFormat: rawFormat,
		FTS: &store.FTSDoc{Body: body}, PreserveLabels: true,
	})
	if err != nil {
		return fmt.Errorf("persist Matrix event %s: %w", evt.ID, err)
	}
	if replyToMessageID != 0 {
		if err := imp.store.SetMessageReplyContext(ctx, messageID, replyToMessageID); err != nil {
			return err
		}
	}
	if recovering {
		// Edits that arrived while this was a placeholder could not be
		// applied, so ask the homeserver for the newest one.
		if err := imp.restoreAfterEditRedaction(ctx, evt.RoomID, messageID, ""); err != nil && !relationsUnavailable(err) {
			return err
		}
	}
	sum.MessagesProcessed++
	if existingID == 0 {
		sum.MessagesAdded++
	}
	if replyTargetMissing {
		return errRelationTargetMissing
	}
	return nil
}

// editEventKey names the message metadata field holding the applied edit, so
// a later redaction of that edit can find its message.
const editEventKey = "matrix_edit_event_id"

type appliedEdit struct {
	EventID string `json:"matrix_edit_event_id"`
	TS      int64  `json:"matrix_edit_ts"`
}

// olderThan follows the Matrix spec for the most recent replacement: the later
// origin_server_ts, then the lexicographically largest event ID on a tie.
func (e appliedEdit) olderThan(evt *event.Event) bool {
	return e.EventID == "" || e.TS < evt.Timestamp || (e.TS == evt.Timestamp && e.EventID < evt.ID.String())
}

func (imp *Importer) persistEdit(sourceID, convID int64, evt *event.Event, target id.EventID, content *event.MessageEventContent) error {
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
	// An edit of an encrypted placeholder is picked up from the homeserver's
	// relations once the original is decrypted.
	if original.Type == event.EventEncrypted || !validReplacement(original, evt) {
		return nil
	}
	current, err := imp.appliedEdit(messageID)
	if err != nil || !current.olderThan(evt) {
		return err
	}
	return imp.applyEdit(messageID, editedBody(original, content), appliedEdit{EventID: evt.ID.String(), TS: evt.Timestamp})
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

func (imp *Importer) archivedEvent(messageID int64) (*event.Event, error) {
	raw, err := imp.store.GetMessageRaw(messageID)
	if err != nil {
		return nil, err
	}
	var original event.Event
	if err := json.Unmarshal(raw, &original); err != nil {
		return nil, fmt.Errorf("decode archived Matrix event %d: %w", messageID, err)
	}
	if original.Content.Parsed == nil {
		if err := original.Content.ParseRaw(original.Type); err != nil && !errors.Is(err, event.ErrUnsupportedContentType) {
			return nil, fmt.Errorf("parse archived Matrix event %s: %w", original.ID, err)
		}
	}
	return &original, nil
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

func (imp *Importer) applyEdit(messageID int64, body string, edit appliedEdit) error {
	metadata, err := json.Marshal(edit, json.Deterministic(true))
	if err != nil {
		return err
	}
	if err := imp.setBody(messageID, body); err != nil {
		return err
	}
	if err := imp.store.SetMessageEdited(messageID); err != nil {
		return err
	}
	// The pointer goes last: a resumed run that sees it skips this edit.
	return imp.store.SetMessageMetadata(messageID, sql.NullString{String: string(metadata), Valid: true})
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
func (imp *Importer) restoreAfterEditRedaction(ctx context.Context, roomID id.RoomID, messageID int64, redacted id.EventID) error {
	original, err := imp.archivedEvent(messageID)
	if err != nil {
		return err
	}
	var newest *event.Event
	var newestContent *event.MessageEventContent
	from := ""
	for {
		page, err := imp.runtime.Client.GetRelations(ctx, roomID, original.ID, &mautrix.ReqGetRelations{RelationType: event.RelReplace, From: from})
		if err != nil {
			return fmt.Errorf("list Matrix edits of %s: %w", original.ID, err)
		}
		for _, edit := range page.Chunk {
			if edit == nil || edit.ID == redacted {
				continue
			}
			if edit = imp.decryptRelation(ctx, roomID, edit); edit == nil {
				continue
			}
			if edit.Content.Parsed == nil && edit.Content.ParseRaw(edit.Type) != nil {
				continue
			}
			content := edit.Content.AsMessage()
			if content.NewContent == nil || content.RelatesTo.GetReplaceID() != original.ID || !validReplacement(original, edit) {
				continue
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
		return imp.applyEdit(messageID, editedBody(original, newestContent), appliedEdit{EventID: newest.ID.String(), TS: newest.Timestamp})
	}
	if err := imp.setBody(messageID, messageBody(original.Content.AsMessage())); err != nil {
		return err
	}
	if err := imp.store.SetMessageEditedState(messageID, false); err != nil {
		return err
	}
	return imp.store.SetMessageMetadata(messageID, sql.NullString{})
}

// relationsUnavailable reports a homeserver that cannot list an event's
// relations, so a recovered message keeps its original text.
func relationsUnavailable(err error) bool {
	var httpErr mautrix.HTTPError
	return errors.As(err, &httpErr) && (httpErr.IsStatus(http.StatusNotFound) || errors.Is(err, mautrix.MUnrecognized))
}

// decryptRelation returns a related event in plaintext, or nil when it is
// encrypted with a key this device does not hold.
func (imp *Importer) decryptRelation(ctx context.Context, roomID id.RoomID, evt *event.Event) *event.Event {
	if evt.Type != event.EventEncrypted {
		return evt
	}
	if !imp.runtime.canDecrypt() || !parseContent(evt) {
		return nil
	}
	evt.RoomID = roomID
	decrypted, err := imp.runtime.decrypt(ctx, evt)
	if err != nil {
		return nil
	}
	decrypted.RoomID = roomID
	return decrypted
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

// undecryptableBatchSize bounds how many pending events are loaded at once.
const undecryptableBatchSize = 500

func (imp *Importer) rememberUndecryptable(sourceID int64, eventID, roomID string, raw []byte) error {
	return imp.store.PutMatrixUndecryptableEvents(sourceID, []store.MatrixUndecryptableEvent{
		{EventID: eventID, RoomID: roomID, RawEvent: raw},
	})
}

// migrateLegacyUndecryptable moves pending events out of checkpoint state
// written before they lived in matrix_undecryptable_events.
func (imp *Importer) migrateLegacyUndecryptable(sourceID int64, state *SyncState) error {
	if len(state.Undecryptable) == 0 {
		state.Undecryptable = nil
		return nil
	}
	eventIDs := make([]string, 0, len(state.Undecryptable))
	for eventID := range state.Undecryptable {
		eventIDs = append(eventIDs, eventID)
	}
	slices.Sort(eventIDs)
	for batch := range slices.Chunk(eventIDs, undecryptableBatchSize) {
		events := make([]store.MatrixUndecryptableEvent, 0, len(batch))
		for _, eventID := range batch {
			pending := state.Undecryptable[eventID]
			events = append(events, store.MatrixUndecryptableEvent{
				EventID: eventID, RoomID: pending.RoomID, RawEvent: []byte(pending.EncryptedEvent),
			})
		}
		if err := imp.store.PutMatrixUndecryptableEvents(sourceID, events); err != nil {
			return fmt.Errorf("migrate pending Matrix events: %w", err)
		}
	}
	state.Undecryptable = nil
	return nil
}

// recoveryPasses is the number of walks over the pending events: originals,
// then other relations, then redactions.
const recoveryPasses = 3

// recoveryPass is the pass that recovers evt.
func recoveryPass(evt *event.Event) int {
	if eventPersistsOwnMessage(evt) {
		return 0
	}
	return 1 + relationReplayPriority(evt)
}

type recoveredRelation struct {
	eventID, roomID string
	ciphertext      []byte
	ownMessage      bool
}

// retryUndecryptable decrypts pending events with the keys now available. A
// recovered relation whose target is not archived is carried in state, which
// is durable only once the caller stores the cursor, so its placeholder and
// pending row stay in place until the returned function runs after that.
func (imp *Importer) retryUndecryptable(ctx context.Context, sourceID int64, state *SyncState, opts ImportOptions, sum *ImportSummary) (settleDeferred func() error, err error) {
	var awaitingCursor []recoveredRelation
	settleDeferred = func() error {
		for _, item := range awaitingCursor {
			if err := imp.retainEncryptedRaw(sourceID, item.eventID, item.roomID, item.ciphertext); err != nil {
				return err
			}
			if !item.ownMessage {
				if err := imp.store.MarkMessageDeleted(sourceID, item.eventID); err != nil {
					return err
				}
			}
			if err := imp.store.DeleteMatrixUndecryptableEvent(sourceID, item.eventID); err != nil {
				return err
			}
		}
		return nil
	}
	if !imp.runtime.canDecrypt() {
		return settleDeferred, nil
	}
	type recoveredEvent struct {
		eventID        string
		roomID         string
		messageID      int64
		conversationID int64
		decrypted      *event.Event
		raw            []byte
		ciphertext     []byte
	}
	var backupErr error
	backupFailures := 0
	defer func() {
		if backupErr == nil {
			return
		}
		// Scheduled syncs pass no Progress, so the daemon log is the only
		// signal that placeholders stay unrecovered.
		slog.Warn("matrix key backup unavailable", "source_id", sourceID, "events", backupFailures, "error", backupErr)
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("key backup unavailable for %d encrypted events: %v", backupFailures, backupErr))
		}
	}()
	for pass := range recoveryPasses {
		afterEventID := ""
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			batch, err := imp.store.MatrixUndecryptableEventsPage(sourceID, afterEventID, undecryptableBatchSize)
			if err != nil {
				return nil, err
			}
			if len(batch) == 0 {
				break
			}
			afterEventID = batch[len(batch)-1].EventID
			// Recovered events of this page are persisted before the next page is
			// read, so memory stays bounded by one page. Persisted rows leave the
			// pending table, which is the resumable progress. An edit of a
			// placeholder is dropped, so originals are recovered in the first pass,
			// other relations in the second and redactions in the third.
			var recovered []recoveredEvent
			eventIDs := make([]string, 0, len(batch))
			for _, pending := range batch {
				eventIDs = append(eventIDs, pending.EventID)
			}
			found, err := imp.store.MessageExistsBatch(sourceID, eventIDs)
			if err != nil {
				return nil, err
			}
			for _, pending := range batch {
				eventID := pending.EventID
				if !roomIncluded(pending.RoomID, opts.Rooms, opts.ExcludeRooms) {
					continue
				}
				messageID := found[eventID]
				if messageID == 0 {
					if err := imp.store.DeleteMatrixUndecryptableEvent(sourceID, eventID); err != nil {
						return nil, err
					}
					continue
				}
				raw := pending.RawEvent
				if len(raw) == 0 {
					raw, err = imp.store.GetMessageRaw(messageID)
					if err != nil {
						return nil, err
					}
				}
				var encrypted event.Event
				if err := json.Unmarshal(raw, &encrypted); err != nil || encrypted.Type != event.EventEncrypted {
					continue
				}
				if len(pending.RawEvent) == 0 {
					if err := imp.rememberUndecryptable(sourceID, eventID, pending.RoomID, raw); err != nil {
						return nil, err
					}
				}
				if encrypted.Content.Parsed == nil {
					if err := encrypted.Content.ParseRaw(encrypted.Type); err != nil {
						return nil, fmt.Errorf("parse archived Matrix event %d: %w", messageID, err)
					}
				}
				if encrypted.RoomID == "" {
					encrypted.RoomID = id.RoomID(pending.RoomID)
				}
				// Other devices keep adding sessions to the server-side backup, so
				// fetch a missing session before retrying. A failure leaves the
				// placeholder for a later sync instead of failing this one.
				if pass == 0 {
					if err := imp.runtime.fetchBackupSession(ctx, &encrypted); err != nil {
						if ctxErr := ctx.Err(); ctxErr != nil {
							return nil, ctxErr
						}
						backupFailures++
						if backupErr == nil {
							backupErr = err
						}
					}
				}
				decrypted, err := imp.runtime.decrypt(ctx, &encrypted)
				if err != nil {
					continue
				}
				decrypted.RoomID = id.RoomID(pending.RoomID)
				if !parseContent(decrypted) {
					if pass != 0 {
						continue
					}
					sum.EventsSkipped++
					if err := imp.retainEncryptedRaw(sourceID, eventID, pending.RoomID, raw); err != nil {
						return nil, err
					}
					if err := imp.discardRecoveredUnsupported(sourceID, eventID); err != nil {
						return nil, err
					}
					continue
				}
				if recoveryPass(decrypted) != pass {
					continue
				}
				decryptedRaw, err := json.Marshal(decrypted, json.Deterministic(true))
				if err != nil {
					return nil, fmt.Errorf("encode recovered Matrix event %s: %w", decrypted.ID, err)
				}
				target, err := imp.store.GetMessageRelationTarget(messageID)
				if err != nil {
					return nil, err
				}
				recovered = append(recovered, recoveredEvent{
					eventID: eventID, roomID: pending.RoomID, messageID: messageID, conversationID: target.ConversationID,
					decrypted: decrypted, raw: decryptedRaw, ciphertext: raw,
				})
			}
			slices.SortFunc(recovered, func(a, b recoveredEvent) int {
				if a.decrypted.Timestamp != b.decrypted.Timestamp {
					return cmp.Compare(a.decrypted.Timestamp, b.decrypted.Timestamp)
				}
				return strings.Compare(a.eventID, b.eventID)
			})
			for _, item := range recovered {
				if err := imp.persistPlainEvent(ctx, sourceID, item.conversationID, item.decrypted, item.raw, sum); err != nil {
					if errors.Is(err, errRelationTargetMissing) {
						if err := rememberRecoveredRelation(state, item.roomID, item.decrypted); err != nil {
							return nil, err
						}
						awaitingCursor = append(awaitingCursor, recoveredRelation{
							eventID: item.eventID, roomID: item.roomID, ciphertext: item.ciphertext,
							ownMessage: eventPersistsOwnMessage(item.decrypted),
						})
						sum.UndecryptableRecovered++
						continue
					}
					return nil, err
				}
				if err := imp.retainEncryptedRaw(sourceID, item.eventID, item.roomID, item.ciphertext); err != nil {
					return nil, err
				}
				if !eventPersistsOwnMessage(item.decrypted) {
					if err := imp.store.MarkMessageDeleted(sourceID, item.eventID); err != nil {
						return nil, err
					}
				}
				if err := imp.store.DeleteMatrixUndecryptableEvent(sourceID, item.eventID); err != nil {
					return nil, err
				}
				sum.UndecryptableRecovered++
			}
		}
	}
	return settleDeferred, nil
}

func (imp *Importer) seedLegacyUndecryptable(ctx context.Context, sourceID int64) error {
	const pageSize = 500
	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := imp.store.ScanArchivedRawMessages(sourceID, rawFormat, afterID, pageSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		seeded := make([]store.MatrixUndecryptableEvent, 0, len(rows))
		for _, row := range rows {
			afterID = row.MessageID
			if row.Deleted || row.BodyText != encryptedPlaceholder {
				continue
			}
			var evt event.Event
			if err := json.Unmarshal(row.RawData, &evt); err != nil || evt.Type != event.EventEncrypted || evt.ID == "" {
				continue
			}
			seeded = append(seeded, store.MatrixUndecryptableEvent{
				EventID: evt.ID.String(), RoomID: evt.RoomID.String(), RawEvent: row.RawData,
			})
		}
		if err := imp.store.PutMatrixUndecryptableEvents(sourceID, seeded); err != nil {
			return err
		}
		if len(rows) < pageSize {
			return nil
		}
	}
}

func rememberRecoveredRelation(state *SyncState, roomID string, evt *event.Event) error {
	if evt == nil || evt.ID == "" {
		return nil
	}
	rs := state.Rooms[roomID]
	if rs == nil {
		rs = &RoomState{}
		state.Rooms[roomID] = rs
	}
	for _, encoded := range rs.DeferredRelations {
		var existing event.Event
		if err := json.Unmarshal([]byte(encoded), &existing); err != nil {
			return fmt.Errorf("decode deferred Matrix relation: %w", err)
		}
		if existing.ID == evt.ID {
			return nil
		}
	}
	raw, err := json.Marshal(evt, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode recovered Matrix relation %s: %w", evt.ID, err)
	}
	rs.DeferredRelations = append(rs.DeferredRelations, string(raw))
	return nil
}

func (imp *Importer) discardRecoveredUnsupported(sourceID int64, eventID string) error {
	if err := imp.store.MarkMessageDeleted(sourceID, eventID); err != nil {
		return err
	}
	return imp.store.DeleteMatrixUndecryptableEvent(sourceID, eventID)
}

func eventPersistsOwnMessage(evt *event.Event) bool {
	if evt.Type != event.EventMessage && evt.Type != event.EventSticker {
		return false
	}
	content := evt.Content.AsMessage()
	// A replacement without m.new_content is never archived, so its
	// placeholder is retired like any other relation's.
	return content.RelatesTo.GetReplaceID() == ""
}

func snippet(body string) string {
	runes := []rune(body)
	if len(runes) > 100 {
		return string(runes[:100])
	}
	return body
}
