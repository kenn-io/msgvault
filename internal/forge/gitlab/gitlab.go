// Package gitlab is the GitLab forge: it finds the issues and merge
// requests an account takes part in through its to-dos, its own events,
// the lists of what it authored, is assigned or reviews, and a check of the
// threads already archived, and fetches each thread's posts over REST.
//
// The account's events include every comment it wrote, including replies
// inside discussions, and GitLab keeps those events for three years.
// The check of archived threads finds other people's later replies and edits.
package gitlab

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/forge"
)

const (
	// Name is GitLab's source type, message type and identifier type.
	Name = "gitlab"
	// DefaultURL is the instance an account lives on unless it names another.
	DefaultURL = "https://gitlab.com"
)

// Forge is GitLab for one account's token, for one sync. What it learns
// about projects and users holds for that sync only: projects move.
type Forge struct {
	instance forge.Instance
	client   *client

	mu sync.Mutex
	// accountID is the token's account, learned by Account.
	accountID string
	// projects holds each project's path as this sync's responses showed
	// it, by project ID: enough for discovery. verified holds the paths read
	// from the projects themselves, the only ones Owner trusts.
	projects map[int64]string
	verified map[int64]string
	// authors holds what kind of account each user is, by user ID.
	authors map[int64]authorType
}

// New returns GitLab at baseURL, the instance's web address (DefaultURL
// when empty), for token.
func New(baseURL, token, userAgent string) (*Forge, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultURL
	}
	instance, base, err := forge.ParseInstance(baseURL)
	if err != nil {
		return nil, err
	}
	return &Forge{
		instance: instance,
		client:   newClient(base, token, userAgent, forge.NewHTTPClient(60*time.Second)),
		projects: map[int64]string{},
		verified: map[int64]string{},
		authors:  map[int64]authorType{},
	}, nil
}

func (f *Forge) Name() string             { return Name }
func (f *Forge) Instance() forge.Instance { return f.instance }

// Account returns the token's account and checks its scopes.
func (f *Forge) Account(ctx context.Context) (forge.Account, error) {
	account, err := f.client.account(ctx)
	if err != nil {
		return forge.Account{}, err
	}
	f.mu.Lock()
	f.accountID = account.ID
	f.mu.Unlock()
	return account, nil
}

// Owner returns the top-level namespace, a group or a user, of the
// project the thread is in now. A project can move to another namespace,
// even between discovery and a fetch, so each project is asked itself once
// per sync, whatever discovery's responses showed.
func (f *Forge) Owner(ctx context.Context, ref forge.ThreadRef) (string, error) {
	loc, err := parseLocator(ref)
	if err != nil {
		return "", err
	}
	owner, err := f.projectOwner(ctx, loc.project)
	if err != nil {
		return "", fmt.Errorf("look up the owner of %s: %w", ref.Name(), err)
	}
	return owner, nil
}

// projectOwner is a project's current owner, read from GitLab once per sync.
func (f *Forge) projectOwner(ctx context.Context, id int64) (string, error) {
	f.mu.Lock()
	path, ok := f.verified[id]
	f.mu.Unlock()
	if !ok {
		var err error
		if path, err = f.lookupProject(ctx, id); err != nil {
			return "", err
		}
	}
	return ownerOf(path), nil
}

// projectPath returns a project's path, such as "group/sub/project", as
// this sync last saw it.
func (f *Forge) projectPath(ctx context.Context, id int64) (string, error) {
	f.mu.Lock()
	path, ok := f.projects[id]
	f.mu.Unlock()
	if ok {
		return path, nil
	}
	return f.lookupProject(ctx, id)
}

// lookupProject reads a project's current path from the project itself.
func (f *Forge) lookupProject(ctx context.Context, id int64) (string, error) {
	var project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	}
	if _, err := f.client.get(ctx, "/projects/"+strconv.FormatInt(id, 10), nil, &project); err != nil {
		return "", forbiddenIsUnavailable(err)
	}
	if project.PathWithNamespace == "" {
		return "", fmt.Errorf("GitLab returned no path for project %d", id)
	}
	f.notePath(id, project.PathWithNamespace)
	f.mu.Lock()
	f.verified[id] = project.PathWithNamespace
	f.mu.Unlock()
	return project.PathWithNamespace, nil
}

// notePath keeps a project's path that a response of this sync showed.
func (f *Forge) notePath(id int64, path string) {
	if id <= 0 || path == "" {
		return
	}
	f.mu.Lock()
	f.projects[id] = path
	f.mu.Unlock()
}

// ownerOf is the top-level namespace of a project path.
func ownerOf(path string) string {
	owner, _, _ := strings.Cut(path, "/")
	return owner
}

// Thread keys are GitLab's global IDs, which name the class: an issue's
// and a merge request's numeric IDs are separate sequences.
const (
	issueGID        = "gid://gitlab/Issue/"
	mergeRequestGID = "gid://gitlab/MergeRequest/"
)

// Locator resources.
const (
	resourceIssues        = "issues"
	resourceMergeRequests = "merge_requests"
)

// locator is where a thread lives: its project's ID, which survives moves
// and renames, and its number there. A ref's Locator is its REST path,
// "projects/<id>/issues/<iid>" or "projects/<id>/merge_requests/<iid>".
type locator struct {
	project  int64
	resource string
	iid      int
}

func (l locator) path() string {
	return "projects/" + strconv.FormatInt(l.project, 10) + "/" + l.resource + "/" + strconv.Itoa(l.iid)
}

func parseLocator(ref forge.ThreadRef) (locator, error) {
	if loc, ok := locatorOf(ref); ok {
		return loc, nil
	}
	return locator{}, fmt.Errorf("%w: GitLab thread %s has no project locator", forge.ErrUnavailable, ref.Name())
}

func locatorOf(ref forge.ThreadRef) (locator, bool) {
	parts := strings.Split(ref.Locator, "/")
	if len(parts) != 4 || parts[0] != "projects" || (parts[2] != resourceIssues && parts[2] != resourceMergeRequests) {
		return locator{}, false
	}
	project, err1 := strconv.ParseInt(parts[1], 10, 64)
	iid, err2 := strconv.Atoi(parts[3])
	if err1 != nil || err2 != nil || project <= 0 || iid <= 0 {
		return locator{}, false
	}
	return locator{project: project, resource: parts[2], iid: iid}, true
}

// threadRef names an issue or merge request. path is its project's current
// path, which also names its owner.
func threadRef(resource string, id, project int64, iid int, path string, updated time.Time) forge.ThreadRef {
	ref := forge.ThreadRef{
		Kind: forge.KindIssue, Key: issueGID + strconv.FormatInt(id, 10),
		Owner: ownerOf(path), Repo: path, Number: iid, UpdatedAt: updated,
		Locator: locator{project: project, resource: resource, iid: iid}.path(),
	}
	if resource == resourceMergeRequests {
		ref.Kind, ref.Key = forge.KindMergeRequest, mergeRequestGID+strconv.FormatInt(id, 10)
	}
	return ref
}
