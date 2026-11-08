package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/forge"
)

const (
	// Name is GitHub's source type, message type and identifier type.
	Name = "github"
	// Instance is the one GitHub instance: github.com.
	Instance = forge.Instance("github.com")
	// APIURL is github.com's API.
	APIURL = "https://api.github.com"
)

// Forge is GitHub for one account's token, for one sync.
type Forge struct {
	client *client
	// emails reports whether the token may read users' public emails.
	emails bool

	mu      sync.Mutex
	account forge.Account
	owners  map[string]string
}

// New returns GitHub for token, calling the API at apiBase.
func New(apiBase *url.URL, token, userAgent string) *Forge {
	return &Forge{
		client: newClient(apiBase, token, userAgent, &http.Client{Timeout: 60 * time.Second}),
		owners: map[string]string{},
	}
}

func (f *Forge) Name() string             { return Name }
func (f *Forge) Instance() forge.Instance { return Instance }

// Account returns the token's account and checks its scopes.
func (f *Forge) Account(ctx context.Context) (forge.Account, error) {
	account, emails, err := f.client.account(ctx)
	if err != nil {
		return forge.Account{}, err
	}
	f.mu.Lock()
	f.account, f.emails = account, emails
	f.mu.Unlock()
	return account, nil
}

// login is the account's current login, which the involvement search names.
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

// Owner returns the current owner of the thread's repository. GitHub
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
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if _, err := f.client.getJSON(ctx, "/repos/"+ref.Repo, nil, nil, &data); err != nil {
		return "", fmt.Errorf("look up the owner of %s: %w", ref.Repo, err)
	}
	f.mu.Lock()
	f.owners[repo] = data.Owner.Login
	f.mu.Unlock()
	return data.Owner.Login, nil
}
