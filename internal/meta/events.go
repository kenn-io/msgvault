package meta

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// FeedEvent carries provider evidence from one Page feed or mention change.
// Time comes from entry.time and orders updates; it never supplies sent time.
type FeedEvent struct {
	ReceiptOrder uint64 `json:"-"`
	Time         int64  `json:"-"`
	PostID       string `json:"post_id"`
	CommentID    string `json:"comment_id"`
	ParentID     string `json:"parent_id"`
	Item         string `json:"item"`
	Verb         string `json:"verb"`
	Message      string `json:"message"`
	CreatedTime  string `json:"created_time"`
	From         *Actor `json:"from,omitempty"`
}

func (e *FeedEvent) UnmarshalJSON(data []byte) error {
	type plain FeedEvent
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("invalid feed event")
	}
	created := fields["created_time"]
	if len(created) > 0 && created[0] != '"' && string(created) != "null" {
		var stamp int64
		if json.Unmarshal(created, &stamp) != nil {
			return errors.New("invalid feed creation time")
		}
		fields["created_time"], _ = json.Marshal(time.Unix(stamp, 0).UTC().Format(time.RFC3339))
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return errors.New("invalid feed event")
	}
	var decoded plain
	if err = json.Unmarshal(normalized, &decoded); err != nil {
		return errors.New("invalid feed event")
	}
	*e = FeedEvent(decoded)
	return nil
}

type EventOptions struct {
	MaxRequests int
	// Mention accepts new post/comment mentions and resolves sparse evidence.
	Mention bool
}
type EventOutcome struct {
	Class, ErrorClass string
	CountAttempt      bool
	ArchiveChanged    bool
}
type EventImporter struct {
	store *store.Store
	api   ObjectReader
}

func NewEventImporter(st *store.Store, api ObjectReader) *EventImporter {
	return &EventImporter{store: st, api: api}
}

// ApplyFeedEvents retains the polling source lock for the whole batch, but
// starts a separate webhook run only when an event needs an archive write.
func (e *EventImporter) ApplyFeedEvents(ctx context.Context, app, page string, events []FeedEvent, opts EventOptions) (out []EventOutcome, retErr error) {
	out = make([]EventOutcome, len(events))
	all := func(class, reason string, count bool) []EventOutcome {
		for i := range out {
			out[i] = EventOutcome{Class: class, ErrorClass: reason, CountAttempt: count}
		}
		return out
	}
	if err := ctx.Err(); err != nil {
		return all("retry", "cancelled", true), err
	}
	if !ValidAccountID(app) || !ValidAccountID(page) || opts.MaxRequests < 0 {
		return all("invalid", "invalid_options", false), nil
	}
	src, err := e.store.GetSourceByTypeAndIdentifier(SourceType, SourceIdentifier(app, page))
	if errors.Is(err, store.ErrSourceNotFound) {
		return all("unrouted", "source_missing", false), nil
	}
	if err != nil {
		return all("retry", "archive_unavailable", true), err
	}
	exec, err := e.store.AcquireSyncExecutionContext(ctx, src.ID)
	if errors.Is(err, store.ErrSourceRetired) {
		return all("unrouted", "source_retired", false), nil
	}
	if errors.Is(err, store.ErrSyncAlreadyActive) {
		return all("retry", "source_busy", false), nil
	}
	if err != nil {
		return all("retry", "archive_unavailable", true), err
	}
	defer func() { retErr = errors.Join(retErr, exec.Release()) }()
	src, err = e.store.GetSourceByIDContext(ctx, src.ID)
	if err != nil {
		return all("retry", "archive_unavailable", true), err
	}
	if src.HistoryOnly || src.MergedIntoSourceID != 0 {
		return all("unrouted", "source_retired", false), nil
	}
	maximum := opts.MaxRequests
	if maximum == 0 {
		maximum = 50
	}
	imp := &Importer{store: e.store}
	run := &importRun{imp: imp, app: app, page: page, source: src, sum: &ImportSummary{}}
	batch := &feedBatch{run: run, reader: e.api, remaining: maximum, mention: opts.Mention}
	batch.start = func() error {
		if run.syncID != 0 {
			return nil
		}
		id, err := exec.StartSyncContext(ctx, WebhookSyncType, "")
		if err == nil {
			run.syncID, imp.store = id, e.store.ScopedToSync(src.ID, id)
		} else {
			retErr = errors.Join(retErr, err)
		}
		return err
	}
	for i, ev := range events {
		before := run.sum.MessagesAdded + run.sum.MessagesUpdated
		batch.changed = false
		out[i] = batch.apply(ctx, ev)
		out[i].ArchiveChanged = batch.changed || run.sum.MessagesAdded+run.sum.MessagesUpdated > before
	}
	if run.syncID != 0 {
		retErr = run.finishWebhook(ctx, retErr)
	}
	return out, retErr
}

// Finalization retains counters for already committed writes if the batch was
// interrupted after persistence. Cache refreshes also inspect failed runs.
func (r *importRun) finishWebhook(ctx context.Context, cause error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	cause = errors.Join(cause, r.imp.store.UpdateSyncCheckpointContext(cleanup, r.syncID, r.counters()))
	if ctx.Err() != nil || cause != nil {
		return errors.Join(cause, r.imp.store.FailSyncContext(cleanup, r.syncID, "webhook processing interrupted"))
	}
	return r.imp.store.CompleteSyncContext(cleanup, r.syncID, "")
}

type feedBatch struct {
	mention   bool
	run       *importRun
	reader    ObjectReader
	remaining int
	changed   bool
	start     func() error
}

func (b *feedBatch) stored(ctx context.Context, id string) (store.StoredMessage, objectMetadata, bool, error) {
	rows, err := b.run.imp.store.StoredMessagesContext(ctx, b.run.source.ID, rawFormat, []string{id})
	row, exists := rows[id]
	var metadata objectMetadata
	if err == nil && exists {
		var saved sql.NullString
		saved, err = b.run.imp.store.GetMessageMetadataContext(ctx, row.ID)
		if err == nil && (!saved.Valid || json.Unmarshal([]byte(saved.String), &metadata) != nil) {
			err = errors.New("invalid archived comment metadata")
		}
	}
	return row, metadata, exists, err
}

func (b *feedBatch) apply(ctx context.Context, ev FeedEvent) EventOutcome {
	state := ev.Verb == "hide" || ev.Verb == "unhide" || ev.Verb == "remove" || ev.Verb == "delete"
	body := ev.Verb == "add" || ev.Verb == "edited" || ev.Verb == "edit"
	post := ev.Item == "status" || ev.Item == "post" || ev.Item == "photo" || ev.Item == "video" || ev.Item == "share" || ev.Item == "link"
	postRemoval := post && (ev.Verb == "remove" || ev.Verb == "delete")
	if b.mention {
		state, body = false, ev.Verb == "add"
	}
	if (ev.Item != "comment" && !postRemoval && !(b.mention && post)) || (!state && !body) {
		return EventOutcome{Class: "ignored"}
	}
	id, parent := ev.CommentID, ev.ParentID
	if post {
		id, parent = ev.PostID, ""
	} else if parent == "" && !b.mention {
		parent = ev.PostID
	}
	if !safeObjectID(id) || !safeObjectID(ev.PostID) || (parent != "" && !safeObjectID(parent)) || id == parent || (!post && id == ev.PostID) {
		return EventOutcome{Class: "invalid", ErrorClass: "invalid_identity"}
	}
	row, metadata, exists, err := b.stored(ctx, id)
	if err != nil {
		return feedRetry(err)
	}
	if b.mention && !post && parent == "" && exists {
		parent = metadata.ParentID
	}
	if exists && (!metadata.matches(b.run.app, b.run.page, "", ev.PostID) || metadata.ParentID != parent || metadata.Version != objectMetadataVersion) {
		return EventOutcome{Class: "invalid", ErrorClass: "conflicting_parent"}
	}
	// A completed body receipt needs no target read on an automatic retry.
	if body && (olderEvent(metadata.BodyTime, metadata.BodyOrder, ev) ||
		(metadata.BodyTime != 0 && metadata.BodyOrder != 0 && metadata.BodyTime == ev.Time && metadata.BodyOrder == ev.ReceiptOrder)) {
		return b.fillMissingEvidence(ctx, ev, row, id, parent)
	}
	if state && olderEvent(metadata.StateTime, metadata.StateOrder, ev) {
		return EventOutcome{Class: "ignored"}
	}
	if b.mention && (ev.Message == "" || (!post && parent == "")) {
		target, err := b.read(ctx, id, post)
		if err != nil {
			return feedRetry(err)
		}
		if ev.Message == "" {
			ev.Message = target.Message
		}
		if !post && parent == "" {
			parent = ev.PostID
			if target.Parent != nil {
				parent = target.Parent.ID
			}
		}
		if ev.From == nil {
			ev.From = target.From
		}
		if ev.CreatedTime == "" {
			ev.CreatedTime = target.CreatedTime
		}
	}
	if post && !exists && !b.mention {
		return EventOutcome{Class: "ignored"}
	}
	b.run.bodyTime, b.run.stateTime = 0, 0
	b.run.bodyOrder, b.run.stateOrder = 0, 0
	if body {
		b.run.bodyTime, b.run.bodyOrder = ev.Time, ev.ReceiptOrder
	} else {
		b.run.stateTime, b.run.stateOrder = ev.Time, ev.ReceiptOrder
	}
	if (!post || b.mention) && (!exists || body) {
		var ancestors []Object
		if !post {
			ancestors, err = b.ancestors(ctx, ev.PostID, parent)
		}
		if err != nil {
			var api *APIError
			if state && errors.As(err, &api) && api.NotFound() {
				return EventOutcome{Class: "ignored", ErrorClass: "ancestor_unavailable"}
			}
			return feedRetry(err)
		}
		if err = b.start(); err != nil {
			return feedRetry(err)
		}
		// Ancestors are node reads, not webhook body/state updates.
		bodyTime, stateTime := b.run.bodyTime, b.run.stateTime
		bodyOrder, stateOrder := b.run.bodyOrder, b.run.stateOrder
		b.run.bodyTime, b.run.stateTime = 0, 0
		b.run.bodyOrder, b.run.stateOrder = 0, 0
		for _, ancestor := range ancestors {
			p := ""
			if ancestor.Parent != nil {
				p = ancestor.Parent.ID
			}
			if err = b.run.persistFreshBody(ctx, ancestor, ev.PostID, p, ""); err != nil {
				return feedPersistOutcome(ctx, err)
			}
		}
		b.run.bodyTime, b.run.stateTime = bodyTime, stateTime
		b.run.bodyOrder, b.run.stateOrder = bodyOrder, stateOrder
		obj := Object{ID: id, Message: ev.Message, CreatedTime: ev.CreatedTime, From: ev.From, Parent: &Actor{ID: parent}}
		if err = b.run.persist(ctx, obj, ev.PostID, parent, ""); err != nil {
			return feedPersistOutcome(ctx, err)
		}
		if state {
			row, metadata, _, err = b.stored(ctx, id)
			if err != nil {
				return feedRetry(err)
			}
		}
	}
	if state {
		if err = b.start(); err != nil {
			return feedRetry(err)
		}
		metadata.StateTime, metadata.StateOrder = ev.Time, ev.ReceiptOrder
		if ev.Verb == "hide" || ev.Verb == "unhide" {
			hidden := ev.Verb == "hide"
			metadata.Hidden = &hidden
		}
		data, err := json.Marshal(metadata)
		if err == nil {
			err = b.run.imp.store.SetMessageMetadataContext(ctx, row.ID, sql.NullString{String: string(data), Valid: true})
		}
		if err == nil {
			b.changed = true
		}
		if err == nil && (ev.Verb == "remove" || ev.Verb == "delete") {
			err = b.run.imp.store.MarkMessageDeletedContext(ctx, b.run.source.ID, id)
		}
		if err != nil {
			return feedRetry(err)
		}
	}
	return EventOutcome{Class: "applied"}
}

// Provider seconds order different timestamps; durable local receipt order
// breaks ties for automatic retries. Zero preserves legacy event behavior.
func olderEvent(stamp int64, order uint64, ev FeedEvent) bool {
	return stamp > ev.Time || (stamp == ev.Time && order != 0 && ev.ReceiptOrder != 0 && order > ev.ReceiptOrder)
}

// Older bodies remain ignored, but an add can supply evidence omitted by an edit.
func (b *feedBatch) fillMissingEvidence(ctx context.Context, ev FeedEvent, row store.StoredMessage, id, parent string) EventOutcome {
	from, created := ev.From, ev.CreatedTime
	if row.SenderID.Valid {
		from = nil
	}
	if row.SentAt.Valid {
		created = ""
	}
	if (from == nil || from.ID == "") && created == "" {
		return EventOutcome{Class: "ignored"}
	}
	body, err := b.run.imp.store.GetMessageBodyText(row.ID)
	if err != nil {
		return feedRetry(err)
	}
	if err := b.start(); err != nil {
		return feedRetry(err)
	}
	b.run.bodyTime, b.run.stateTime = 0, 0
	b.run.bodyOrder, b.run.stateOrder = 0, 0
	obj := Object{ID: id, Message: body, CreatedTime: created, From: from, Parent: &Actor{ID: parent}}
	if err := b.run.persist(ctx, obj, ev.PostID, parent, ""); err != nil {
		return feedPersistOutcome(ctx, err)
	}
	return EventOutcome{Class: "ignored"}
}

// ancestors reads the whole missing chain before writing, then returns it
// top-down. Archived ancestors are still validated by persist's provenance checks.
func (b *feedBatch) ancestors(ctx context.Context, root, parent string) ([]Object, error) {
	objects := []Object{}
	if _, _, exists, err := b.stored(ctx, root); err != nil {
		return nil, err
	} else if !exists {
		obj, err := b.read(ctx, root, true)
		if err != nil {
			return nil, err
		}
		if obj.ID != root || obj.Parent != nil {
			return nil, errors.New("invalid root evidence")
		}
		objects = append(objects, obj)
	}
	chain := []Object{}
	seen := map[string]bool{}
	for parent != root {
		if !safeObjectID(parent) || seen[parent] || len(chain) >= 8 {
			return nil, errors.New("ancestor depth or identity invalid")
		}
		seen[parent] = true
		if _, _, exists, err := b.stored(ctx, parent); err != nil {
			return nil, err
		} else if exists {
			break
		}
		obj, err := b.read(ctx, parent, false)
		if err != nil {
			return nil, err
		}
		if obj.Parent == nil {
			obj.Parent = &Actor{ID: root}
		}
		if obj.ID != parent || !safeObjectID(obj.Parent.ID) {
			return nil, errors.New("invalid ancestor evidence")
		}
		chain = append(chain, obj)
		parent = obj.Parent.ID
	}
	for _, object := range slices.Backward(chain) {
		objects = append(objects, object)
	}
	return objects, nil
}

// Target and ancestor reads share one batch request budget.
func (b *feedBatch) read(ctx context.Context, id string, post bool) (Object, error) {
	if b.remaining == 0 {
		return Object{}, errRequestBudget
	}
	b.remaining--
	if post {
		return b.reader.Post(ctx, id)
	}
	return b.reader.Comment(ctx, id)
}

func feedRetry(err error) EventOutcome {
	class := string(Classify(err, false))
	if errors.Is(err, errRequestBudget) {
		return EventOutcome{Class: "retry", ErrorClass: "request_budget"}
	}
	return EventOutcome{Class: "retry", ErrorClass: class, CountAttempt: true}
}
func feedPersistOutcome(ctx context.Context, err error) EventOutcome {
	if ctx.Err() != nil {
		return feedRetry(ctx.Err())
	}
	if errors.Is(err, errInvalidObject) {
		return EventOutcome{Class: "invalid", ErrorClass: "invalid_object"}
	}
	return feedRetry(err)
}
