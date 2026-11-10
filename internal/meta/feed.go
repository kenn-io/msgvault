package meta

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

// WebhookSyncType names the sync runs that apply webhook deliveries.
const WebhookSyncType = "webhook"

// maxAncestorReads bounds the Graph reads one feed change may make to archive
// a missing post and parent comments.
const maxAncestorReads = 8

// ErrInvalidChange marks a webhook change that can never be applied, such as
// one that does not decode or names an invalid object.
var ErrInvalidChange = errors.New("invalid webhook change")

// ObjectReader reads the comment a webhook names, and its post and parent
// comments when they are not archived yet.
type ObjectReader interface {
	Post(ctx context.Context, id string) (Object, error)
	Comment(ctx context.Context, id string) (Object, error)
}

// feedChange is one Page feed webhook change value. Comment text, author and
// parent are read from Graph, not taken from the change.
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
// missing post or parent comment first. Hides and unhides are recorded in the
// comment's metadata, and removed comments and posts are marked deleted.
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
	return applyWebhook(ctx, st, SourceType, SourceIdentifier(app, page), func(run *importRun) error {
		run.app, run.page = app, page
		for _, change := range changes {
			if err := run.applyFeedChange(ctx, reader, change); err != nil {
				return fmt.Errorf("feed %s %s: %w", change.Item, change.Verb, err)
			}
		}
		return nil
	})
}

// applyWebhook runs apply under the source's execution lock in a webhook
// sync run, and records the run's outcome and counts.
func applyWebhook(ctx context.Context, st *store.Store, sourceType, identifier string, apply func(*importRun) error) (retErr error) {
	source, execution, err := acquireSource(ctx, st, sourceType, identifier)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	syncID, err := execution.StartSyncContext(ctx, WebhookSyncType, "")
	if err != nil {
		return err
	}
	run := &importRun{imp: &Importer{store: st.ScopedToSync(source.ID, syncID)}, source: source, sum: &ImportSummary{}, syncID: syncID}
	err = apply(run)
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
		if !archived {
			return nil
		}
		r.sum.MessagesProcessed++
		r.sum.MessagesUpdated++
		return r.imp.store.MarkMessageDeletedContext(ctx, r.source.ID, id)
	case "hide", "unhide":
		// A comment polling has not archived yet arrives with its state later.
		if !archived {
			return nil
		}
		return r.setHidden(ctx, id, change.Verb == "hide")
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
			return errors.New("comment ancestors exceed the read limit")
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
		return fmt.Errorf("decode comment metadata: %w", err)
	}
	if meta.Hidden != nil && *meta.Hidden == hidden {
		return nil
	}
	meta.Hidden = &hidden
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	r.sum.MessagesProcessed++
	r.sum.MessagesUpdated++
	return r.imp.store.SetMessageMetadataContext(ctx, rows[id].ID, sql.NullString{String: string(data), Valid: true})
}
