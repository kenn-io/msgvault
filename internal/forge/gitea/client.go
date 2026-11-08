package gitea

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/httpretry"
)

const (
	// pageSize is the most items a sync requests per page. The server's
	// max_response_items may lower this limit.
	pageSize = 50
	// unknownLimitWait is how long a rate limit lasts when the response that
	// reports it names no time.
	unknownLimitWait = time.Minute
)

// limited recognizes a rate limit. Gitea and Forgejo limit no API requests
// themselves, but a proxy in front of a hosted server may refuse one with 429
// Too Many Requests and say in Retry-After when to come back.
func (f *Forge) limited(resp *http.Response) (time.Time, bool) {
	if resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	now := f.now()
	if delay, ok := httpretry.ParseRetryAfter(resp.Header.Get("Retry-After"), 0, now); ok {
		return now.Add(delay), true
	}
	return now.Add(unknownLimitWait), true
}

// get sends a GET, decodes the response into out and returns its headers.
// A 403 means the token may not read the resource.
func (f *Forge) get(ctx context.Context, path string, query url.Values, out any) (http.Header, error) {
	resp, err := f.api.GetJSON(ctx, path, query, nil, out)
	if refused, ok := errors.AsType[*forge.HTTPError](err); ok && refused.Status == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %w", forge.ErrUnavailable, err)
	}
	if err != nil {
		return nil, err
	}
	return resp.Header, nil
}

// pageLimit reads the server's public API settings once per Forge. A failed
// read is retried on the next attempt, so rate limits can pause and resume.
func (f *Forge) pageLimit(ctx context.Context) (int, error) {
	f.mu.Lock()
	limit := f.limit
	f.mu.Unlock()
	if limit > 0 {
		return limit, nil
	}
	var settings struct {
		MaxResponseItems int `json:"max_response_items"`
	}
	if _, err := f.get(ctx, "/settings/api", nil, &settings); err != nil {
		return 0, fmt.Errorf("read Gitea API settings: %w", err)
	}
	if settings.MaxResponseItems <= 0 {
		return 0, errors.New("gitea returned no positive max_response_items")
	}
	limit = min(pageSize, settings.MaxResponseItems)
	f.mu.Lock()
	f.limit = limit
	f.mu.Unlock()
	return limit, nil
}

// hasMore uses the total before considering links or the returned count:
// Gitea pages reviews before hiding other users' drafts, so even an empty
// page may precede submitted reviews.
func hasMore(header http.Header, page, n, limit int) bool {
	if total, err := strconv.Atoi(header.Get("X-Total-Count")); err == nil {
		return page*limit < total
	}
	if links := header.Get("Link"); links != "" {
		return forge.NextLink(links) != ""
	}
	return n == limit
}

// pageQuery is a list request's page.
func pageQuery(page, limit int) url.Values {
	return url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
}

// user is an account as Gitea's User shows it (modules/structs/user.go).
// Forgejo's has no type.
type user struct {
	ID      int64  `json:"id"`
	Login   string `json:"login"`
	HTMLURL string `json:"html_url"`
	Type    string `json:"type"`
}

const (
	// ghostID and ghostLogin are Gitea's stand-in for a deleted account
	// (models/user/user_system.go). Gitea also shows a migrated author under
	// the ghost's ID, with the author's original name as its login.
	ghostID    = -1
	ghostLogin = "Ghost"
	// botType is the type Gitea gives a bot account, its own Actions user
	// among them.
	botType = "Bot"
)

// author is the account a post's author is. originalAuthor is the name a
// migration carried over, which Forgejo still reports beside the ghost.
// Emails are never taken: Gitea shows most accounts' addresses as
// placeholders that can't be told from real ones (services/convert/user.go).
func author(u *user, originalAuthor string) forge.Account {
	switch {
	case originalAuthor != "":
		return forge.Account{Kind: forge.AuthorMigrated, Login: originalAuthor}
	case u == nil || u.ID == 0 || u.Login == "":
		return forge.Account{Kind: forge.AuthorUnavailable, Login: ghostLogin}
	case u.ID == ghostID && u.Login != ghostLogin:
		return forge.Account{Kind: forge.AuthorMigrated, Login: u.Login}
	case u.ID == ghostID:
		return forge.Account{Kind: forge.AuthorUnavailable, Login: u.Login, URL: u.HTMLURL}
	default:
		// Gitea's other negative IDs are its own system accounts, such as
		// the Actions user: bots.
		return forge.Account{
			Kind: forge.AuthorAccount, ID: strconv.FormatInt(u.ID, 10), Login: u.Login,
			URL: u.HTMLURL, Bot: u.Type == botType || u.ID < 0,
		}
	}
}

// evidence is a post's JSON as Gitea sent it, keys sorted, without what
// changes when the post doesn't, so unchanged evidence is byte-identical
// across syncs: its lists (labels, assignees, attachments), the drop
// fields, and of each account or milestone it names, all but what names
// it.
func evidence(value jsontext.Value, drop ...string) []byte {
	var object map[string]jsontext.Value
	if json.Unmarshal(value, &object) != nil {
		return value
	}
	for key, nested := range object {
		if nested.Kind() == '[' {
			delete(object, key)
		}
	}
	for _, key := range drop {
		delete(object, key)
	}
	for key, keep := range named {
		if nested, ok := object[key]; ok && nested.Kind() == '{' {
			object[key] = only(nested, keep)
		}
	}
	data, err := json.Marshal(object, json.Deterministic(true))
	if err != nil {
		return value
	}
	return data
}

// named are the fields of a post that name something of its own, an
// account or a milestone, and what of each the evidence keeps. The rest
// changes without the post: an account's last login and follower counts,
// a milestone's issue counts.
var named = map[string][]string{
	"user":      accountFields,
	"resolver":  accountFields,
	"assignee":  accountFields,
	"milestone": {"id", "title"},
}

// accountFields are what names an account. Its email is left out with the
// rest: Gitea shows most accounts' addresses as placeholders.
var accountFields = []string{"id", "login", "full_name", "html_url", "type"}

// only is an object with just the keep fields.
func only(value jsontext.Value, keep []string) jsontext.Value {
	var object map[string]jsontext.Value
	if json.Unmarshal(value, &object) != nil {
		return value
	}
	for key := range object {
		if !slices.Contains(keep, key) {
			delete(object, key)
		}
	}
	data, err := json.Marshal(object, json.Deterministic(true))
	if err != nil {
		return value
	}
	return data
}
