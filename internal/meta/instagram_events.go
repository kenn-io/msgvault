package meta

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

// InstagramWebhookMedia retains provider observations. AdID is provenance,
// not evidence of ownership or an instruction to classify a comment as an ad.
type InstagramWebhookMedia struct {
	ID               string `json:"id"`
	AdID             string `json:"ad_id,omitempty"`
	AdTitle          string `json:"ad_title,omitempty"`
	OriginalMediaID  string `json:"original_media_id,omitempty"`
	MediaProductType string `json:"media_product_type,omitempty"`
}

// InstagramCommentEvent identifies a comment to read. Time orders webhook
// updates; the node's timestamp supplies the message sent time.
type InstagramCommentEvent struct {
	Time      int64                 `json:"-"`
	CommentID string                `json:"id"`
	ParentID  string                `json:"parent_id,omitempty"`
	Media     InstagramWebhookMedia `json:"media"`
}

type InstagramCommentNode struct {
	IGComment

	Media *IGMedia `json:"media,omitempty"`
}

type InstagramObjectReader interface {
	InstagramComment(ctx context.Context, id string) (InstagramCommentNode, error)
	InstagramMediaNode(ctx context.Context, id string) (IGMedia, error)
}

func (c *Client) InstagramComment(ctx context.Context, id string) (InstagramCommentNode, error) {
	var node InstagramCommentNode
	if !safeObjectID(id) {
		return node, fmt.Errorf("%w: invalid Instagram comment identity", errInvalidObject)
	}
	if err := c.get(ctx, url.PathEscape(id), url.Values{graphFieldsParam: {instagramCommentFields + ",media{id}"}}, &node); err != nil {
		return node, err
	}
	if node.ID != id {
		return node, fmt.Errorf("%w: Instagram comment identity mismatch", errInvalidObject)
	}
	return node, nil
}

func (c *Client) InstagramMediaNode(ctx context.Context, id string) (IGMedia, error) {
	var node IGMedia
	if !safeObjectID(id) {
		return node, fmt.Errorf("%w: invalid Instagram media identity", errInvalidObject)
	}
	if err := c.get(ctx, url.PathEscape(id), url.Values{graphFieldsParam: {"id,caption,timestamp,permalink,owner{id}"}}, &node); err != nil {
		return node, err
	}
	if node.ID != id {
		return node, fmt.Errorf("%w: Instagram media identity mismatch", errInvalidObject)
	}
	return node, nil
}

type InstagramEventImporter struct {
	store *store.Store
	api   InstagramObjectReader
}

func NewInstagramEventImporter(st *store.Store, api InstagramObjectReader) *InstagramEventImporter {
	return &InstagramEventImporter{store: st, api: api}
}

func (e *InstagramEventImporter) ApplyEvents(ctx context.Context, app, page, account string, events []InstagramCommentEvent, opts EventOptions) (out []EventOutcome, retErr error) {
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
	if !ValidAccountID(app) || !ValidAccountID(page) || !ValidAccountID(account) || opts.MaxRequests < 0 {
		return all("invalid", "invalid_options", false), nil
	}
	src, err := e.store.GetSourceByTypeAndIdentifier(InstagramSourceType, InstagramSourceIdentifier(app, page, account))
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
	imp := &Importer{store: e.store, instagram: account}
	run := &importRun{imp: imp, app: app, page: page, source: src, sum: &ImportSummary{}}
	batch := &instagramEventBatch{feedBatch: &feedBatch{run: run, remaining: maximum}, reader: e.api}
	batch.start = func() error {
		if run.syncID != 0 {
			return nil
		}
		id, err := exec.StartSyncContext(ctx, InstagramSourceType+"_webhook", "")
		if err == nil {
			run.syncID, imp.store = id, e.store.ScopedToSync(src.ID, id)
		} else {
			retErr = errors.Join(retErr, err)
		}
		return err
	}
	for i, event := range events {
		before := run.sum.MessagesAdded + run.sum.MessagesUpdated
		out[i] = batch.apply(ctx, event)
		out[i].ArchiveChanged = run.sum.MessagesAdded+run.sum.MessagesUpdated > before
	}
	retErr = errors.Join(retErr, ctx.Err())
	if run.syncID != 0 {
		retErr = run.finishWebhook(ctx, retErr)
	}
	return out, retErr
}

type instagramEventBatch struct {
	*feedBatch

	reader InstagramObjectReader
}

var errInstagramAncestry = errors.New("invalid Instagram comment ancestry")

func instagramEventFailure(ctx context.Context, err error) EventOutcome {
	if errors.Is(err, errRequestBudget) {
		return EventOutcome{Class: "retry", ErrorClass: "request_budget"}
	}
	if errors.Is(err, errInstagramAncestry) {
		return EventOutcome{Class: "invalid", ErrorClass: "invalid_ancestry"}
	}
	return feedPersistOutcome(ctx, err)
}

func (b *instagramEventBatch) comment(ctx context.Context, id, root string) (Object, error) {
	if b.remaining == 0 {
		return Object{}, errRequestBudget
	}
	b.remaining--
	node, err := b.reader.InstagramComment(ctx, id)
	if err != nil {
		return Object{}, err
	}
	if node.ID != id || node.Media == nil || node.Media.ID != root || node.Timestamp == "" {
		return Object{}, fmt.Errorf("%w: incomplete Instagram comment evidence", errInvalidObject)
	}
	parent := node.ParentID
	if parent == "" {
		parent = root
	}
	if !safeObjectID(parent) || parent == id {
		return Object{}, fmt.Errorf("%w: invalid Instagram comment parent", errInvalidObject)
	}
	return Object{ID: id, Message: node.Text, CreatedTime: node.Timestamp, Parent: &Actor{ID: parent}, InstagramComment: &node.IGComment}, nil
}

func (b *instagramEventBatch) ancestors(ctx context.Context, root, parent, comment string) ([]Object, error) {
	var objects []Object
	if _, metadata, exists, err := b.stored(ctx, root); err != nil {
		return nil, err
	} else if exists {
		if !metadata.matches(b.run.app, b.run.page, b.run.imp.instagram, root) || metadata.ParentID != "" || metadata.Version != objectMetadataVersion {
			return nil, errInstagramAncestry
		}
	} else {
		if b.remaining == 0 {
			return nil, errRequestBudget
		}
		b.remaining--
		media, err := b.reader.InstagramMediaNode(ctx, root)
		if err != nil {
			return nil, err
		}
		if media.ID != root || media.Owner == nil || media.Owner.ID != b.run.imp.instagram {
			return nil, fmt.Errorf("%w: Instagram media ownership unavailable", errInvalidObject)
		}
		objects = append(objects, Object{ID: media.ID, Message: media.Caption, CreatedTime: media.Timestamp, InstagramMedia: &media})
	}
	chain := []Object{}
	seen := map[string]bool{comment: true}
	for parent != root {
		if seen[parent] || len(chain) >= 8 {
			return nil, errInstagramAncestry
		}
		seen[parent] = true
		if _, metadata, exists, err := b.stored(ctx, parent); err != nil {
			return nil, err
		} else if exists {
			if !metadata.matches(b.run.app, b.run.page, b.run.imp.instagram, root) || metadata.Version != objectMetadataVersion {
				return nil, errInstagramAncestry
			}
			break
		}
		obj, err := b.comment(ctx, parent, root)
		if err != nil {
			return nil, err
		}
		chain = append(chain, obj)
		parent = obj.Parent.ID
	}
	for _, ancestor := range slices.Backward(chain) {
		objects = append(objects, ancestor)
	}
	return objects, nil
}

func (b *instagramEventBatch) apply(ctx context.Context, event InstagramCommentEvent) EventOutcome {
	if !safeObjectID(event.CommentID) || !safeObjectID(event.Media.ID) || event.CommentID == event.Media.ID || (event.ParentID != "" && !safeObjectID(event.ParentID)) {
		return EventOutcome{Class: "invalid", ErrorClass: "invalid_identity"}
	}
	_, metadata, exists, err := b.stored(ctx, event.CommentID)
	if err != nil {
		return feedRetry(err)
	}
	if exists && (!metadata.matches(b.run.app, b.run.page, b.run.imp.instagram, event.Media.ID) || metadata.Version != objectMetadataVersion) {
		return EventOutcome{Class: "invalid", ErrorClass: "conflicting_context"}
	}
	if metadata.BodyTime > event.Time {
		return EventOutcome{Class: "ignored"}
	}
	obj, err := b.comment(ctx, event.CommentID, event.Media.ID)
	if err != nil {
		return instagramEventFailure(ctx, err)
	}
	parent := obj.Parent.ID
	if (event.ParentID != "" && event.ParentID != parent) || (exists && metadata.ParentID != parent) {
		return EventOutcome{Class: "invalid", ErrorClass: "conflicting_parent"}
	}
	ancestors, err := b.ancestors(ctx, event.Media.ID, parent, event.CommentID)
	if err != nil {
		return instagramEventFailure(ctx, err)
	}
	if err = b.start(); err != nil {
		return feedRetry(err)
	}
	b.run.bodyTime, b.run.webhookMedia = 0, nil
	for _, ancestor := range ancestors {
		ancestorParent := ""
		if ancestor.Parent != nil {
			ancestorParent = ancestor.Parent.ID
		}
		if err = b.run.persistFreshBody(ctx, ancestor, event.Media.ID, ancestorParent, ""); err != nil {
			return instagramEventFailure(ctx, err)
		}
	}
	b.run.bodyTime, b.run.webhookMedia = event.Time, &event.Media
	if err = b.run.persistFreshBody(ctx, obj, event.Media.ID, parent, ""); err != nil {
		return instagramEventFailure(ctx, err)
	}
	return EventOutcome{Class: "applied"}
}
