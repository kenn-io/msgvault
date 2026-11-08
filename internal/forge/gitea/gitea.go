// Package gitea is the Gitea forge, which also serves Forgejo: it finds the
// threads an account takes part in through its notifications, issue searches
// for its involvement, its own comment activity, and a rotating check of the
// threads already archived, and fetches each thread's posts over REST.
package gitea

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/forge"
)

const (
	// Name is Gitea's source type, message type and identifier type.
	Name = "gitea"
	// apiPath is where a Gitea or Forgejo server serves its API, under its
	// base URL.
	apiPath = "api/v1"
	// reconcilePerSync bounds how many archived threads one sync checks for
	// changes the other streams can't see.
	reconcilePerSync = 50
)

// ErrMissingScope means the token lacks a scope a sync needs.
var ErrMissingScope = errors.New("the Gitea token lacks a scope msgvault needs")

// Forge is a Gitea or Forgejo server for one account's token, for one sync.
type Forge struct {
	api      *forge.API
	instance forge.Instance
	now      func() time.Time
	// reconcile is how many archived threads each sync checks.
	reconcile int

	mu      sync.Mutex
	account forge.Account
	// limit is the effective page size after reading the server's settings.
	limit int
	// owners is each repository's current owner, by lowercase full name.
	owners map[string]string
}

// New returns the Gitea or Forgejo server at base, for token. base is the
// server's web address, with any path the server is served under.
func New(base *url.URL, token, userAgent string) (*Forge, error) {
	instance, base, err := forge.ParseInstance(base.String())
	if err != nil {
		return nil, err
	}
	f := &Forge{
		instance:  instance,
		now:       time.Now,
		reconcile: reconcilePerSync,
		owners:    map[string]string{},
	}
	f.api = &forge.API{
		Base:   base.JoinPath(apiPath),
		Client: forge.NewHTTPClient(60 * time.Second),
		Authorize: func(req *http.Request) {
			req.Header.Set("Authorization", "token "+token)
			req.Header.Set("Accept", "application/json")
		},
		UserAgent: userAgent,
		Limited:   f.limited,
	}
	return f, nil
}

func (f *Forge) Name() string             { return Name }
func (f *Forge) Instance() forge.Instance { return f.instance }

// scopeProbes are the scopes a sync needs beyond read:user, each with a
// request that only a token holding it may make.
var scopeProbes = []struct{ scope, path string }{
	{"read:notification", "/notifications"},
	{"read:issue", "/repos/issues/search"},
	{"read:repository", "/repos/search"},
}

// Account returns the token's account and checks that the token holds every
// scope a sync needs. These probes are supported by both Gitea and Forgejo;
// Gitea's /token introspection endpoint is not available on both servers.
func (f *Forge) Account(ctx context.Context) (forge.Account, error) {
	var me user
	if _, err := f.api.GetJSON(ctx, "/user", nil, nil, &me); err != nil {
		return forge.Account{}, scopeError(err, "read:user")
	}
	if me.ID <= 0 || me.Login == "" {
		return forge.Account{}, errors.New("gitea returned no account for the token")
	}
	for _, probe := range scopeProbes {
		var ignored jsontext.Value
		if _, err := f.api.GetJSON(ctx, probe.path, url.Values{"limit": {"1"}}, nil, &ignored); err != nil {
			return forge.Account{}, scopeError(err, probe.scope)
		}
	}
	account := forge.Account{Kind: forge.AuthorAccount, ID: strconv.FormatInt(me.ID, 10), Login: me.Login, URL: me.HTMLURL}
	f.mu.Lock()
	f.account = account
	f.mu.Unlock()
	return account, nil
}

// scopeError reads a refused scope check: a 403 means the token lacks the
// scope, or, for notifications, is limited to public resources.
func scopeError(err error, scope string) error {
	if refused, ok := errors.AsType[*forge.HTTPError](err); ok && refused.Status == http.StatusForbidden {
		return fmt.Errorf("%w (%s): %w", ErrMissingScope, scope, err)
	}
	return err
}

// login is the account's current login, which its activity feed is named by.
func (f *Forge) login(ctx context.Context) (string, error) {
	f.mu.Lock()
	login := f.account.Login
	f.mu.Unlock()
	if login != "" {
		return login, nil
	}
	account, err := f.Account(ctx)
	return account.Login, err
}

// Owner returns the current owner of the thread's repository. Gitea
// redirects a renamed or transferred repository, so the answer follows a
// move. Each repository is asked once per sync.
func (f *Forge) Owner(ctx context.Context, ref forge.ThreadRef) (string, error) {
	repo := strings.ToLower(ref.Repo)
	f.mu.Lock()
	owner, ok := f.owners[repo]
	f.mu.Unlock()
	if ok {
		return owner, nil
	}
	var data struct {
		Owner *user `json:"owner"`
	}
	if _, err := f.get(ctx, repoPath(ref.Repo), nil, &data); err != nil {
		return "", fmt.Errorf("look up the owner of %s: %w", ref.Repo, err)
	}
	if data.Owner == nil || data.Owner.Login == "" {
		return "", fmt.Errorf("gitea named no owner of %s", ref.Repo)
	}
	f.mu.Lock()
	f.owners[repo] = data.Owner.Login
	f.mu.Unlock()
	return data.Owner.Login, nil
}

// repoPath is a repository's API path, from its "owner/name".
func repoPath(repo string) string {
	return "/repos/" + repo
}
