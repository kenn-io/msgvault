package meta

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// InstagramReader reads the comment and media nodes that an Instagram
// comment webhook names.
type InstagramReader interface {
	InstagramComment(ctx context.Context, id string) (InstagramCommentNode, error)
	InstagramMediaNode(ctx context.Context, id string) (IGMedia, error)
}

// InstagramCommentNode is a comment read by ID, with the media it is on.
type InstagramCommentNode struct {
	IGComment

	Media *IGMedia `json:"media,omitempty"`
}

func (c *Client) InstagramComment(ctx context.Context, id string) (InstagramCommentNode, error) {
	return readNode(ctx, c, id, instagramCommentFields+",media{id}", func(n InstagramCommentNode) string { return n.ID })
}

func (c *Client) InstagramMediaNode(ctx context.Context, id string) (IGMedia, error) {
	return readNode(ctx, c, id, "id,caption,timestamp,permalink,owner{id}", func(m IGMedia) string { return m.ID })
}

// ApplyInstagramComments archives the Instagram comments and live comments
// that webhook changes name. Each comment is read again from Meta instead of
// trusting the delivery. Media that is not archived yet is read as well, and
// must belong to account before anything is saved.
func ApplyInstagramComments(ctx context.Context, st *store.Store, reader InstagramReader, app, page, account string, values []jsontext.Value) error {
	type target struct{ comment, media string }
	targets := make([]target, 0, len(values))
	for _, value := range values {
		var change struct {
			ID    string `json:"id"`
			Media struct {
				ID string `json:"id"`
			} `json:"media"`
		}
		if err := json.Unmarshal(value, &change); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidChange, err)
		}
		if !safeObjectID(change.ID) || !safeObjectID(change.Media.ID) {
			return fmt.Errorf("%w: invalid object identity", ErrInvalidChange)
		}
		targets = append(targets, target{change.ID, change.Media.ID})
	}
	ancestors := ancestorReader{
		root: func(ctx context.Context, id string) (Object, error) {
			media, err := reader.InstagramMediaNode(ctx, id)
			if err != nil {
				return Object{}, err
			}
			if media.Owner == nil || media.Owner.ID != account {
				return Object{}, fmt.Errorf("%w: media %q does not belong to the account", ErrInvalidChange, id)
			}
			return instagramMediaObject(&media), nil
		},
		comment: func(ctx context.Context, id string) (Object, error) {
			node, err := reader.InstagramComment(ctx, id)
			return instagramCommentObject(&node.IGComment), err
		},
	}
	return applyWebhook(ctx, st, InstagramSourceType, InstagramSourceIdentifier(app, page, account), func(st *store.Store, source *store.Source, sum *ImportSummary) error {
		run := &importRun{imp: &Importer{store: st, instagram: account}, app: app, page: page, source: source, sum: sum}
		for _, t := range targets {
			node, err := reader.InstagramComment(ctx, t.comment)
			if err != nil {
				return fmt.Errorf("read comment %q: %w", t.comment, err)
			}
			if node.Media == nil || node.Media.ID != t.media {
				return fmt.Errorf("%w: comment %q is not on media %q", ErrInvalidChange, t.comment, t.media)
			}
			obj := instagramCommentObject(&node.IGComment)
			parent := t.media
			if obj.Parent != nil {
				parent = obj.Parent.ID
			}
			if err := run.archiveAncestors(ctx, ancestors, t.media, parent); err != nil {
				return err
			}
			if err := run.persist(ctx, obj, t.media, parent, ""); err != nil {
				return err
			}
		}
		return nil
	})
}
