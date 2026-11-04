package meta

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

func (c *Client) VerifyPage(ctx context.Context, id string) (Page, error) {
	var page Page
	if !ValidAccountID(id) {
		return page, errors.New("invalid selected Page ID")
	}
	if err := c.get(ctx, "me", url.Values{graphFieldsParam: {"id,name,category"}}, &page); err != nil {
		return Page{}, err
	}
	// Category is Page-only evidence in the pinned SDK, not a token-type
	// attestation. Missing evidence is unavailable, never successful validation.
	if page.ID != id || strings.TrimSpace(page.Category) == "" {
		//nolint:revive,staticcheck // error-strings: this user-facing sentence follows the review's exact wording.
		return Page{}, errors.New("Could not verify this token for the configured Page.")
	}
	return page, nil
}

func (c *Client) Posts(ctx context.Context, page string, edge PostEdge, after string) (ObjectPage, error) {
	if !ValidAccountID(page) || !edge.valid() {
		return ObjectPage{}, errors.New("invalid selected Page or discovery edge")
	}
	q := url.Values{graphFieldsParam: {"id,message,created_time,from"}, "limit": {"100"}}
	if edge == AdsPosts {
		q.Set("include_inline_create", "true")
	}
	return c.objects(ctx, page+"/"+string(edge), after, q)
}

func (c *Client) Comments(ctx context.Context, object, after string) (ObjectPage, error) {
	if !safeObjectID(object) {
		return ObjectPage{}, errors.New("invalid Graph object ID")
	}
	return c.objects(ctx, url.PathEscape(object)+"/comments", after, url.Values{
		graphFieldsParam: {"id,message,created_time,from,parent"}, "order": {"chronological"}, "filter": {"toplevel"}, "limit": {"100"},
	})
}

func (c *Client) objects(ctx context.Context, path, after string, q url.Values) (ObjectPage, error) {
	page, err := readObjectPage(ctx, c, path, after, q, func(object Object) string { return object.ID })
	return ObjectPage(page), err
}

// Cursors are opaque request parameters. The finite bound protects checkpoint
// and request size; no paging.next URL is retained or followed.
func (c *Client) safeCursor(cursor string) bool {
	return len(cursor) <= 4096 && strings.IndexFunc(cursor, func(r rune) bool { return r < 32 || r == 127 }) < 0 && !strings.Contains(cursor, c.token)
}

// Post reads a single root with the same wire fields polling uses.
func (c *Client) Post(ctx context.Context, id string) (Object, error) {
	return c.readNode(ctx, id, "id,message,created_time,from")
}

// Comment reads a comment and its provider-declared parent.
func (c *Client) Comment(ctx context.Context, id string) (Object, error) {
	return c.readNode(ctx, id, "id,message,created_time,from,parent")
}

func (c *Client) readNode(ctx context.Context, id, fields string) (Object, error) {
	if !safeObjectID(id) {
		return Object{}, errors.New("invalid Graph object ID")
	}
	var obj Object
	if err := c.get(ctx, url.PathEscape(id), url.Values{graphFieldsParam: {fields}}, &obj); err != nil {
		return Object{}, err
	}
	if obj.ID != id {
		return Object{}, errors.New("graph object identity mismatch")
	}
	return obj, nil
}
