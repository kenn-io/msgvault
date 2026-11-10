package meta

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// ApplyPageMentions archives posts and comments that mention the Page, named
// by Page mention webhook changes, into the Page's comment source. Each post
// or comment is read from Graph, with any missing post or parent comment, so
// it keeps its conversation and reply links.
func ApplyPageMentions(ctx context.Context, st *store.Store, reader ObjectReader, app, page string, values []jsontext.Value) error {
	var changes []feedChange
	for _, value := range values {
		var change feedChange
		if err := json.Unmarshal(value, &change); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidChange, err)
		}
		if change.Verb != "add" || (change.Item != "comment" && !change.isPost()) {
			continue
		}
		if !safeObjectID(change.PostID) || (change.Item == "comment" && !safeObjectID(change.CommentID)) {
			return fmt.Errorf("%w: invalid object identity", ErrInvalidChange)
		}
		changes = append(changes, change)
	}
	if len(changes) == 0 {
		return nil
	}
	ancestors := facebookAncestors(reader)
	return applyWebhook(ctx, st, SourceType, SourceIdentifier(app, page), func(st *store.Store, source *store.Source, sum *ImportSummary) error {
		run := &importRun{imp: &Importer{store: st}, app: app, page: page, source: source, sum: sum}
		for _, change := range changes {
			if change.Item != "comment" {
				if err := run.archiveAncestors(ctx, ancestors, change.PostID, change.PostID); err != nil {
					return err
				}
				continue
			}
			comment, err := reader.Comment(ctx, change.CommentID)
			if err != nil {
				return fmt.Errorf("read comment %q: %w", change.CommentID, err)
			}
			parent := change.PostID
			if comment.Parent != nil && comment.Parent.ID != "" {
				parent = comment.Parent.ID
			}
			if err := run.archiveAncestors(ctx, ancestors, change.PostID, parent); err != nil {
				return err
			}
			if err := run.persist(ctx, comment, change.PostID, parent, ""); err != nil {
				return err
			}
		}
		return nil
	})
}
