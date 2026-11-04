package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

// ErrDMSourceUnrouted means no enabled, registered inbox matches the delivery.
var ErrDMSourceUnrouted = errors.New("inbox source not routed")

type DMHandler struct {
	store *store.Store
	route func(context.Context, string, string, string) (DMSource, *store.Source, error)
}

func NewDMHandler(st *store.Store, route func(context.Context, string, string, string) (DMSource, *store.Source, error)) *DMHandler {
	return &DMHandler{store: st, route: route}
}

type dmReaction struct {
	Mid      string `json:"mid"`
	Action   string `json:"action"`
	Reaction string `json:"reaction"`
	Emoji    string `json:"emoji"`
}
type dmEdit struct {
	Mid   string `json:"mid"`
	Text  string `json:"text"`
	Count int    `json:"num_edit"`
}
type dmWebhookEvent struct {
	messagingEvent

	channel  string
	Reaction *dmReaction `json:"reaction,omitempty"`
	Edit     *dmEdit     `json:"message_edit,omitempty"`
}
type dmEventGroup struct {
	src     DMSource
	source  *store.Source
	indices []int
	events  []dmWebhookEvent
}

func dmOutcome(class metawebhook.Class, reason string) metawebhook.ItemOutcome {
	return metawebhook.ItemOutcome{Class: class, ErrorClass: reason, CountAttempt: class == metawebhook.Retry}
}

// dmArchiveRetry keeps cancellation separate from an independent archive failure.
func dmArchiveRetry(err error) metawebhook.ItemOutcome {
	if Classify(err, false) == ClassCancelled {
		return metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: string(ClassCancelled)}
	}
	return dmOutcome(metawebhook.Retry, "archive_unavailable")
}

// Handle keeps each source batch ordered under the same execution lock as
// backfill. Its sync runs never replace the typed polling checkpoint.
func (h *DMHandler) Handle(ctx context.Context, app string, items []metawebhook.Item) []metawebhook.ItemOutcome {
	out := make([]metawebhook.ItemOutcome, len(items))
	groups := map[int64]*dmEventGroup{}
	order := []int64{}
	for i, item := range items {
		if item.AppID != "" && item.AppID != app {
			out[i] = dmOutcome(metawebhook.Unrouted, "app_mismatch")
			continue
		}
		src, source, err := h.route(ctx, app, item.Object, item.EntryID)
		switch {
		case errors.Is(err, ErrDMSourceUnrouted), errors.Is(err, store.ErrSourceNotFound), errors.Is(err, store.ErrSourceRetired):
			out[i] = dmOutcome(metawebhook.Unrouted, "source_missing")
			continue
		case err != nil:
			out[i] = dmArchiveRetry(err)
			continue
		case source == nil || source.HistoryOnly || source.MergedIntoSourceID != 0:
			out[i] = dmOutcome(metawebhook.Unrouted, "source_retired")
			continue
		}
		object := "page"
		if src.Platform == PlatformInstagram {
			object = "instagram"
		}
		parsed, valid := ParseDMSourceIdentifier(src.Platform, src.Identifier())
		if !valid || parsed != src || src.AppID != app || object != item.Object || dmOwner(src) != item.EntryID || source.SourceType != src.Platform.SourceType() || source.Identifier != src.Identifier() {
			out[i] = dmOutcome(metawebhook.Unrouted, "source_mismatch")
			continue
		}
		event, result := decodeDMEvent(src, item)
		if result.Class != "" {
			out[i] = result
			continue
		}
		group := groups[source.ID]
		if group == nil {
			group = &dmEventGroup{src: src, source: source}
			groups[source.ID] = group
			order = append(order, source.ID)
		}
		group.indices = append(group.indices, i)
		group.events = append(group.events, event)
	}
	for _, id := range order {
		h.applyGroup(ctx, groups[id], out)
	}
	return out
}
func decodeDMEvent(src DMSource, item metawebhook.Item) (dmWebhookEvent, metawebhook.ItemOutcome) {
	var e dmWebhookEvent
	if len(item.Value) > 256<<10 || json.Unmarshal(item.Value, &e) != nil {
		return e, dmOutcome(metawebhook.Invalid, "invalid_event")
	}
	e.Raw = append([]byte(nil), item.Value...)
	e.channel = item.Field
	count := 0
	if e.Message != nil {
		count++
	}
	if e.Reaction != nil {
		count++
	}
	if e.Edit != nil {
		count++
	}
	if count == 0 {
		return e, dmOutcome(metawebhook.Unhandled, "unsupported_event")
	}
	if count > 1 {
		return e, dmOutcome(metawebhook.Invalid, "ambiguous_event")
	}
	switch {
	case e.Message != nil:
		if item.Field != "messages" && item.Field != "message_echoes" && item.Field != "standby" {
			return e, dmOutcome(metawebhook.Unhandled, "unsupported_event")
		}
		if (item.Field == "message_echoes" && !e.Message.IsEcho) || (src.Platform == PlatformInstagram && item.Field == "message_echoes") {
			return e, dmOutcome(metawebhook.Invalid, "invalid_echo")
		}
	case e.Reaction != nil:
		if item.Field != "message_reactions" {
			return e, dmOutcome(metawebhook.Unhandled, "unsupported_event")
		}
		r := e.Reaction
		if !safeObjectID(r.Mid) || (r.Action != "react" && r.Action != "unreact") || (r.Action == "react" && r.Emoji == "" && r.Reaction == "") {
			return e, dmOutcome(metawebhook.Invalid, "invalid_reaction")
		}
	case e.Edit != nil:
		if src.Platform != PlatformMessenger || item.Field != "message_edits" {
			return e, dmOutcome(metawebhook.Unhandled, "unsupported_event")
		}
		if !safeObjectID(e.Edit.Mid) || e.Edit.Count <= 0 {
			return e, dmOutcome(metawebhook.Invalid, "invalid_edit")
		}
	}
	echo := e.Message != nil && e.Message.IsEcho
	if (!echo && e.Recipient.ID != dmOwner(src)) || (echo && e.Sender.ID != dmOwner(src)) {
		return e, dmOutcome(metawebhook.Invalid, "misrouted")
	}
	if !ValidAccountID(e.Sender.ID) || !ValidAccountID(e.Recipient.ID) || e.Sender.ID == e.Recipient.ID || e.Timestamp <= 0 {
		return e, dmOutcome(metawebhook.Invalid, "invalid_identity")
	}
	if e.Message != nil && !safeObjectID(e.Message.Mid) {
		return e, dmOutcome(metawebhook.Invalid, "invalid_identity")
	}
	return e, metawebhook.ItemOutcome{}
}
func (h *DMHandler) applyGroup(ctx context.Context, g *dmEventGroup, out []metawebhook.ItemOutcome) {
	set := func(result metawebhook.ItemOutcome) {
		for _, i := range g.indices {
			out[i] = result
		}
	}
	execution, err := h.store.AcquireSyncExecutionContext(ctx, g.source.ID)
	if err != nil {
		result := dmArchiveRetry(err)
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			result = dmOutcome(metawebhook.Retry, "source_busy")
			result.CountAttempt = false
		}
		if errors.Is(err, store.ErrSourceRetired) {
			result = dmOutcome(metawebhook.Unrouted, "source_retired")
		}
		set(result)
		return
	}
	defer func() {
		if execution.Release() != nil {
			for _, i := range g.indices {
				if out[i].Class == metawebhook.Applied {
					out[i].Class = metawebhook.Retry
					out[i].ErrorClass = "archive_unavailable"
					out[i].CountAttempt = true
				}
			}
		}
	}()
	current, err := h.store.GetSourceByIDContext(ctx, g.source.ID)
	if err != nil {
		set(dmArchiveRetry(err))
		return
	}
	if current.HistoryOnly || current.MergedIntoSourceID != 0 {
		set(dmOutcome(metawebhook.Unrouted, "source_retired"))
		return
	}
	run, err := execution.StartSyncContext(ctx, g.src.Platform.SourceType()+"_webhook", "")
	if err != nil {
		set(dmArchiveRetry(err))
		return
	}
	writer := &dmWriter{store: h.store.ScopedToSync(g.source.ID, run), sourceID: g.source.ID, src: g.src}
	failed := false
	var counters store.Checkpoint
	for n, event := range g.events {
		i := g.indices[n]
		if ctx.Err() != nil {
			out[i] = dmArchiveRetry(ctx.Err())
		} else {
			out[i] = writer.applyEvent(ctx, event, &counters)
		}
		if out[i].Class == metawebhook.Applied || out[i].Class == metawebhook.Ignored || out[i].ArchiveChanged {
			counters.MessagesProcessed++
		}
		if out[i].Class == metawebhook.Retry {
			counters.ErrorsCount++
		}
		failed = failed || out[i].Class == metawebhook.Retry
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	checkpointErr := h.store.UpdateSyncCheckpointContext(cleanup, run, &counters)
	if failed || ctx.Err() != nil || checkpointErr != nil {
		err = h.store.FailSyncContext(cleanup, run, "inbox webhook processing incomplete")
	} else {
		err = h.store.CompleteSyncContext(cleanup, run, "")
	}
	err = errors.Join(checkpointErr, err)
	if err != nil {
		for _, i := range g.indices {
			if out[i].Class == metawebhook.Applied {
				out[i].Class = metawebhook.Retry
				out[i].ErrorClass = "archive_unavailable"
				out[i].CountAttempt = true
			}
		}
	}
}
func (w *dmWriter) applyEvent(ctx context.Context, e dmWebhookEvent, counters *store.Checkpoint) metawebhook.ItemOutcome {
	if e.Message != nil && (w.src.Platform != PlatformInstagram || !e.Message.IsDeleted) {
		rec, err := recordFromEvent(w.src, e.messagingEvent)
		if err != nil {
			return dmOutcome(metawebhook.Invalid, "invalid_message")
		}
		if e.channel == "standby" {
			rec.Meta.Channel = "standby"
		}
		var persistStats dmPersistStats
		id, _, err := w.persistWithStats(ctx, rec, &persistStats)
		if id != 0 && counters != nil {
			if persistStats.added {
				counters.MessagesAdded++
			} else if persistStats.updated {
				counters.MessagesUpdated++
			}
		}
		result := dmOutcome(metawebhook.Applied, "")
		if err != nil {
			result = dmArchiveRetry(err)
		}
		result.ArchiveChanged = id != 0
		return result
	}
	mid, missing := "", ""
	switch {
	case e.Reaction != nil:
		mid, missing = e.Reaction.Mid, "reaction_target_missing"
	case e.Edit != nil:
		mid, missing = e.Edit.Mid, "edit_target_missing"
	case e.Message != nil:
		mid = e.Message.Mid
	}
	rows, err := w.store.StoredMessagesContext(ctx, w.sourceID, dmRawFormat, []string{mid})
	if err != nil {
		return dmArchiveRetry(err)
	}
	row, exists := rows[mid]
	if !exists {
		if missing != "" {
			return dmOutcome(metawebhook.Retry, missing)
		}
		return dmOutcome(metawebhook.Ignored, "")
	}
	ref, err := w.store.MessageConversation(row.ID)
	if err != nil {
		return dmArchiveRetry(err)
	}
	customer := e.Sender.ID
	if e.Message != nil && e.Message.IsEcho {
		customer = e.Recipient.ID
	}
	key := "psid:" + customer
	if w.src.Platform == PlatformInstagram {
		key = "igsid:" + customer
	}
	if ref.Type != "direct_chat" || ref.SourceConversationID != key {
		return dmOutcome(metawebhook.Invalid, "misrouted")
	}
	saved, err := w.store.GetMessageMetadataContext(ctx, row.ID)
	if err != nil {
		return dmArchiveRetry(err)
	}
	var metadata dmMetadata
	if !saved.Valid || json.Unmarshal([]byte(saved.String), &metadata) != nil || metadata.Version != dmMetadataVersion || metadata.AppID != w.src.AppID || metadata.PageID != w.src.PageID || metadata.IGID != w.src.IGID {
		return dmOutcome(metawebhook.Invalid, "metadata_mismatch")
	}
	legacyGraphObservedAt := metadata.LegacyGraphObservedAt
	graphObservedAt, markerFound, err := w.loadGraphObservedAt(ctx, mid, legacyGraphObservedAt)
	if err != nil {
		return dmArchiveRetry(err)
	}
	markerNeedsUpdate := !markerFound
	if metadata.LegacyGraphObservedAt > graphObservedAt {
		graphObservedAt = metadata.LegacyGraphObservedAt
		markerNeedsUpdate = true
	}
	if graphObservedAt > 0 && markerNeedsUpdate {
		if err = w.saveGraphObservedAt(ctx, mid, graphObservedAt); err != nil {
			return dmArchiveRetry(err)
		}
	}
	metadata.LegacyGraphObservedAt = 0
	switch {
	case e.Reaction != nil:
		r := e.Reaction
		actor := DMActorIdentifier(w.src, e.Sender.ID)
		if e.Timestamp < metadata.ReactionTimes[actor] {
			return dmOutcome(metawebhook.Ignored, "")
		}
		if metadata.ReactionTimes == nil {
			metadata.ReactionTimes = map[string]int64{}
		}
		metadata.ReactionTimes[actor] = e.Timestamp
		reaction := &store.MetaMessageReaction{SourceReactionID: mid + "|" + actor, CreatedAt: time.UnixMilli(e.Timestamp).UTC()}
		if r.Action == "react" {
			reaction.ParticipantID, err = ResolveDMActor(ctx, w.store, w.sourceID, w.src, e.Sender.ID, e.Sender.Name)
			if err != nil {
				return dmArchiveRetry(err)
			}
			reaction.Value = r.Emoji
			if reaction.Value == "" {
				reaction.Value = r.Reaction
			}
		}
		var blob []byte
		blob, err = json.Marshal(metadata)
		if err == nil {
			err = w.store.ApplyMetaMessageUpdateContext(ctx, row.ID, store.MetaMessageUpdate{PreviousMetadata: saved, Metadata: sql.NullString{String: string(blob), Valid: true}, Reaction: reaction})
		}
	case e.Edit != nil:
		if e.Edit.Count <= metadata.EditCount || (graphObservedAt > 0 && e.Timestamp <= graphObservedAt) {
			return dmOutcome(metawebhook.Ignored, "")
		}
		metadata.EditCount = e.Edit.Count
		var blob []byte
		blob, err = json.Marshal(metadata)
		if err == nil {
			watermark := &store.ArchiveWatermark{
				Key:          dmGraphObservedKey(w.sourceID, mid),
				ObservedAt:   e.Timestamp,
				Floor:        legacyGraphObservedAt,
				RequireNewer: true,
			}
			err = w.store.ApplyMetaMessageUpdateContext(ctx, row.ID, store.MetaMessageUpdate{PreviousMetadata: saved, Metadata: sql.NullString{String: string(blob), Valid: true}, Text: &e.Edit.Text, Watermark: watermark})
		}
	case e.Message != nil:
		err = w.store.MarkMessageDeletedContext(ctx, w.sourceID, mid)
	}
	if errors.Is(err, store.ErrStaleArchiveWatermark) {
		return dmOutcome(metawebhook.Ignored, "")
	}
	if err != nil {
		return dmArchiveRetry(err)
	}
	if counters != nil {
		counters.MessagesUpdated++
	}
	return metawebhook.ItemOutcome{Class: metawebhook.Applied, ArchiveChanged: true}
}
