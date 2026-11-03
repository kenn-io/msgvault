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
	if err := c.get(ctx, "me", url.Values{"fields": {"id,name,category"}}, &page); err != nil {
		return Page{}, err
	}
	// Category is Page-only evidence in the pinned SDK, not a token-type
	// attestation. Missing evidence is unavailable, never successful validation.
	if page.ID != id || strings.TrimSpace(page.Category) == "" {
		return Page{}, errors.New("page identity mismatch or Page-only identity evidence unavailable")
	}
	return page, nil
}

func (c *Client) Posts(ctx context.Context, page string, edge PostEdge, after string) (ObjectPage, error) {
	if !ValidAccountID(page) || !edge.valid() {
		return ObjectPage{}, errors.New("invalid selected Page or discovery edge")
	}
	q := url.Values{"fields": {"id,message,created_time,from"}, "limit": {"100"}}
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
		"fields": {"id,message,created_time,from,parent"}, "order": {"chronological"}, "filter": {"toplevel"}, "limit": {"100"},
	})
}

func (c *Client) objects(ctx context.Context, path, after string, q url.Values) (ObjectPage, error) {
	if !c.safeCursor(after) {
		return ObjectPage{}, errors.New("invalid Graph cursor")
	}
	if after != "" {
		q.Set("after", after)
	}
	var wire struct {
		Data   []Object `json:"data"`
		Paging struct {
			Cursors struct {
				After string `json:"after"`
			} `json:"cursors"`
			Next string `json:"next"`
		} `json:"paging"`
	}
	if err := c.get(ctx, path, q, &wire); err != nil {
		return ObjectPage{}, err
	}
	if wire.Data == nil || len(wire.Data) > pageLimit {
		return ObjectPage{}, errors.New("graph response missing or oversized data page")
	}
	next := ""
	if wire.Paging.Next != "" {
		next = wire.Paging.Cursors.After
		if next == "" || next == after || !c.safeCursor(next) {
			return ObjectPage{}, errors.New("invalid Graph pagination progress")
		}
	}
	for _, object := range wire.Data {
		if !safeObjectID(object.ID) {
			return ObjectPage{}, errors.New("invalid Graph object identity")
		}
	}
	return ObjectPage{Data: wire.Data, After: next}, nil
}

// Cursors are opaque request parameters. The finite bound protects checkpoint
// and request size; no paging.next URL is retained or followed.
func (c *Client) safeCursor(cursor string) bool {
	return len(cursor) <= 4096 && strings.IndexFunc(cursor, func(r rune) bool { return r < 32 || r == 127 }) < 0 && !strings.Contains(cursor, c.token)
}
