package meta

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

// WebhookSyncType names the sync runs that apply webhook deliveries.
const WebhookSyncType = store.SyncTypeWebhook

// maxAncestorReads bounds the Graph reads one feed change may make to archive
// a missing post and parent comments.
const maxAncestorReads = 8

// ErrInvalidChange marks a webhook change that can never be applied, such as
// one that does not decode or names an invalid object.
var ErrInvalidChange = errors.New("invalid webhook change")

// ObjectReader reads the comment a webhook names, and its post and parent
// comments when they are not archived yet. CommentHidden reads whether Meta
// currently hides a comment.
type ObjectReader interface {
	Post(ctx context.Context, id string) (Object, error)
	Comment(ctx context.Context, id string) (Object, error)
	CommentHidden(ctx context.Context, id string) (bool, error)
}

// feedChange is one Page feed webhook change value. Comment text, author,
// parent and hidden state are read from Graph, not taken from the change.
type feedChange struct {
	Item      string `json:"item"`
	Verb      string `json:"verb"`
	PostID    string `json:"post_id"`
	CommentID string `json:"comment_id"`
}

func (c feedChange) isPost() bool {
	return slices.Contains([]string{"status", "post", "photo", "video", "share", "link"}, c.Item)
}

// ApplyFeed applies Page feed changes for one Page's comment source. New and
// edited comments are archived through the polling write path, reading a
// missing post or parent comment first. A hide or unhide reads the comment's
// current visibility from Graph and records it in the comment's metadata, and
// removed comments and posts are marked deleted.
// Other changes, such as reactions, are ignored.
func ApplyFeed(ctx context.Context, st *store.Store, reader ObjectReader, app, page string, values []jsontext.Value) error {
	var changes []feedChange
	for _, value := range values {
		var change feedChange
		if err := json.Unmarshal(value, &change); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidChange, err)
		}
		comment := change.Item == "comment" && slices.Contains([]string{"add", "edited", "edit", "hide", "unhide", "remove", "delete"}, change.Verb)
		if comment || (change.isPost() && (change.Verb == "remove" || change.Verb == "delete")) {
			changes = append(changes, change)
		}
	}
	if len(changes) == 0 {
		return nil
	}
	var removed []feedChange
	for _, change := range changes {
		if change.Verb == "remove" || change.Verb == "delete" {
			removed = append(removed, change)
		}
	}
	apply := func(run *importRun) error {
		run.app, run.page = app, page
		for _, change := range changes {
			if err := run.applyFeedChange(ctx, reader, change); err != nil {
				return fmt.Errorf("feed %s %s: %w", change.Item, change.Verb, err)
			}
		}
		return nil
	}
	if len(removed) == 0 {
		return applyWebhook(ctx, st, SourceType, SourceIdentifier(app, page), apply)
	}
	return applyWebhookRemoving(ctx, st, SourceType, SourceIdentifier(app, page), removed, apply)
}

// applyWebhook runs apply under the source's execution lock in a webhook
// sync run, and records the run's outcome and counts.
func applyWebhook(ctx context.Context, st *store.Store, sourceType, identifier string, apply func(*importRun) error) error {
	return applyWebhookRemoving(ctx, st, sourceType, identifier, nil, apply)
}

// applyWebhookRemoving is applyWebhook for a delivery that removes comments
// or posts. Before the webhook run starts, it prunes a paused polling sync's
// saved traversal of the removed objects and the archived replies below a
// removed comment, and publishes that as a new paused polling run. Pruning
// first means `msgvault gc` can never purge the archive rows that link a
// saved reply frame to a removed comment while the prune still needs them: a
// failure after the prune leaves the marks to the retry, and a failure before
// it leaves nothing marked. The prune comes before the webhook run starts
// because a newer run fences the older one's scoped writes. Pruning only
// drops saved traversal, so it is safe even when the same delivery adds the
// comment before removing it.
func applyWebhookRemoving(ctx context.Context, st *store.Store, sourceType, identifier string, removed []feedChange, apply func(*importRun) error) (retErr error) {
	source, execution, err := acquireSource(ctx, st, sourceType, identifier)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	if len(removed) > 0 {
		var ids []string
		for _, change := range removed {
			if change.isPost() {
				ids = append(ids, change.PostID)
				continue
			}
			// A paused sync can hold a saved copy the archive never got, so
			// the removed ID is pruned whether or not it is archived.
			thread, err := st.MessageThreadSourceIDsContext(ctx, source.ID, change.CommentID)
			if err != nil {
				return err
			}
			ids = append(ids, change.CommentID)
			ids = append(ids, thread...)
		}
		if err := dropDeletedTraversal(ctx, st, execution, source, ids); err != nil {
			return err
		}
	}
	syncID, err := execution.StartSyncContext(ctx, WebhookSyncType, "")
	if err != nil {
		return err
	}
	run := &importRun{imp: &Importer{store: st.ScopedToSync(source.ID, syncID)}, source: source, sum: &ImportSummary{}, syncID: syncID}
	err = apply(run)
	// Retrying cannot get past an archive invariant, so the change is invalid.
	if _, ok := errors.AsType[invariantError](err); ok {
		err = fmt.Errorf("%w: %w", ErrInvalidChange, err)
	}
	// Record the outcome even when ctx was cancelled during apply.
	finish := context.WithoutCancel(ctx)
	cp := &store.Checkpoint{MessagesAdded: int64(run.sum.MessagesAdded), MessagesUpdated: int64(run.sum.MessagesUpdated), MessagesProcessed: int64(run.sum.MessagesProcessed)}
	if err = errors.Join(err, st.UpdateSyncCheckpointContext(finish, syncID, cp)); err != nil {
		return errors.Join(err, st.FailSyncContext(finish, syncID, err.Error()))
	}
	return st.CompleteSyncContext(finish, syncID, "")
}

func (r *importRun) applyFeedChange(ctx context.Context, reader ObjectReader, change feedChange) error {
	id, root := change.CommentID, change.PostID
	if change.isPost() {
		id = change.PostID
	}
	if !safeObjectID(id) || !safeObjectID(root) {
		return fmt.Errorf("%w: invalid object identity", ErrInvalidChange)
	}
	archived, err := r.archived(ctx, id)
	if err != nil {
		return err
	}
	switch change.Verb {
	case "remove", "delete":
		// applyWebhookRemoving pruned the paused sync's traversal already.
		if !change.isPost() {
			marked, err := r.imp.store.MarkMessageThreadDeletedContext(ctx, r.source.ID, id)
			r.sum.MessagesProcessed += len(marked)
			r.sum.MessagesUpdated += len(marked)
			return err
		}
		// Only the post is marked; its comments stay archived.
		if !archived {
			return nil
		}
		r.sum.MessagesProcessed++
		r.sum.MessagesUpdated++
		return r.imp.store.MarkMessageDeletedContext(ctx, r.source.ID, id)
	case "hide", "unhide":
		// Polling does not read hidden state, so the state of a comment that
		// is not archived yet is not recorded.
		if !archived {
			return nil
		}
		// Recording Graph's current state instead of the verb means a late or
		// repeated delivery cannot bring back an older state.
		hidden, err := reader.CommentHidden(ctx, id)
		if err != nil {
			return fmt.Errorf("read comment %q: %w", id, err)
		}
		return r.setHidden(ctx, id, hidden)
	}
	// Reading the comment instead of using the change's text means a late or
	// repeated delivery cannot replace newer text with older text.
	obj, err := reader.Comment(ctx, id)
	if err != nil {
		return fmt.Errorf("read comment %q: %w", id, err)
	}
	parent := root
	if obj.Parent != nil && obj.Parent.ID != "" {
		parent = obj.Parent.ID
	}
	if err := r.archiveAncestors(ctx, reader, root, parent); err != nil {
		return err
	}
	return r.persist(ctx, obj, root, parent, "")
}

func (r *importRun) archived(ctx context.Context, id string) (bool, error) {
	rows, err := r.imp.store.MessageExistsBatchContext(ctx, r.source.ID, []string{id})
	return rows[id] != 0, err
}

// archiveAncestors reads and archives the post and parent comments that are
// not archived yet, top down, so persist finds the conversation and parent.
func (r *importRun) archiveAncestors(ctx context.Context, reader ObjectReader, root, parent string) error {
	var chain []Object
	for id := parent; id != root; {
		archived, err := r.archived(ctx, id)
		if err != nil {
			return err
		}
		if archived {
			break
		}
		if len(chain) == maxAncestorReads {
			return invariantError("comment ancestors exceed the read limit")
		}
		obj, err := reader.Comment(ctx, id)
		if err != nil {
			return fmt.Errorf("read parent comment %q: %w", id, err)
		}
		// Meta omits parent on a top-level comment; its parent is the post.
		if obj.Parent == nil || obj.Parent.ID == "" {
			obj.Parent = &Actor{ID: root}
		}
		chain = append(chain, obj)
		id = obj.Parent.ID
	}
	archived, err := r.archived(ctx, root)
	if err != nil {
		return err
	}
	if !archived {
		post, err := reader.Post(ctx, root)
		if err != nil {
			return fmt.Errorf("read post %q: %w", root, err)
		}
		chain = append(chain, post)
	}
	for _, obj := range slices.Backward(chain) {
		parent := ""
		if obj.ID != root {
			parent = obj.Parent.ID
		}
		if err := r.persist(ctx, obj, root, parent, ""); err != nil {
			return err
		}
	}
	return nil
}

func (r *importRun) setHidden(ctx context.Context, id string, hidden bool) error {
	rows, err := r.imp.store.StoredMessagesContext(ctx, r.source.ID, r.objectFormat(), []string{id})
	if err != nil {
		return err
	}
	saved, err := r.imp.store.GetMessageMetadataContext(ctx, rows[id].ID)
	if err != nil {
		return err
	}
	var meta objectMetadata
	if err := json.Unmarshal([]byte(saved.String), &meta); err != nil {
		return fmt.Errorf("%w: %w", invariantError("decode comment metadata"), err)
	}
	if meta.Hidden == hidden {
		return nil
	}
	r.sum.MessagesProcessed++
	r.sum.MessagesUpdated++
	// The same write `comment moderate` uses, so both record one field.
	return setCommentHidden(ctx, r.imp.store, rows[id].ID, hidden)
}
