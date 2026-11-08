package github

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Kind is the kind of a thread: what one archived conversation holds.
type Kind string

const (
	KindIssue       Kind = "issue"
	KindPullRequest Kind = "pull_request"
	KindDiscussion  Kind = "discussion"
	KindRelease     Kind = "release"
)

// Thread is one issue, pull request, discussion or release a sync has
// found changed and still has to archive. Issues, pull requests and
// discussions share one number sequence per repository.
type Thread struct {
	Kind   Kind   `json:"kind"`
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number,omitzero"`
	// ReleaseURL is a release's REST URL, as its notification names it.
	ReleaseURL string    `json:"release_url,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Reason is why GitHub notified the account, when a notification
	// found the thread.
	Reason string `json:"reason,omitempty"`
}

// Key identifies the thread among those a sync has found.
func (t Thread) Key() string {
	repo := strings.ToLower(t.Owner + "/" + t.Repo)
	if t.Kind == KindRelease {
		return repo + "@" + t.ReleaseURL
	}
	return fmt.Sprintf("%s#%d", repo, t.Number)
}

// Name is how errors and logs name the thread: owner/repo#number, or the
// release URL.
func (t Thread) Name() string {
	if t.Kind == KindRelease {
		return t.ReleaseURL
	}
	return fmt.Sprintf("%s/%s#%d", t.Owner, t.Repo, t.Number)
}

// mergeThreads adds found to queue. A thread found twice keeps its latest
// update and any notification reason. The result is newest first, so
// recent activity is archived before an old backlog.
func mergeThreads(queue, found []Thread) []Thread {
	byKey := make(map[string]int, len(queue)+len(found))
	merged := make([]Thread, 0, len(queue)+len(found))
	for _, thread := range slices.Concat(queue, found) {
		index, seen := byKey[thread.Key()]
		if !seen {
			byKey[thread.Key()] = len(merged)
			merged = append(merged, thread)
			continue
		}
		existing := &merged[index]
		if thread.UpdatedAt.After(existing.UpdatedAt) {
			existing.UpdatedAt = thread.UpdatedAt
		}
		if thread.Reason != "" {
			existing.Reason = thread.Reason
		}
	}
	slices.SortStableFunc(merged, func(a, b Thread) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return merged
}

// OwnerFilter keeps or drops threads by the owner of their repository, an
// organization or a user, before any of their content is fetched.
type OwnerFilter struct {
	include []string
	exclude []string
}

// NewOwnerFilter keeps only include's owners when include is non-empty,
// and never exclude's. Owners compare without case, as GitHub's do.
func NewOwnerFilter(include, exclude []string) OwnerFilter {
	return OwnerFilter{include: normalizeOwners(include), exclude: normalizeOwners(exclude)}
}

func normalizeOwners(owners []string) []string {
	out := make([]string, 0, len(owners))
	for _, owner := range owners {
		owner = strings.ToLower(strings.TrimSpace(owner))
		if owner != "" && !slices.Contains(out, owner) {
			out = append(out, owner)
		}
	}
	slices.Sort(out)
	return out
}

// Allows reports whether threads of owner's repositories are archived.
func (f OwnerFilter) Allows(owner string) bool {
	owner = strings.ToLower(owner)
	if slices.Contains(f.exclude, owner) {
		return false
	}
	return len(f.include) == 0 || slices.Contains(f.include, owner)
}

// searchQualifiers restricts a GitHub search to the allowed owners, so an
// excluded owner's threads never reach msgvault. GitHub ORs repeated user:
// qualifiers.
func (f OwnerFilter) searchQualifiers() string {
	var b strings.Builder
	for _, owner := range f.include {
		b.WriteString(" user:" + owner)
	}
	for _, owner := range f.exclude {
		b.WriteString(" -user:" + owner)
	}
	return b.String()
}

// fingerprint names the filter in the sync state: discovery that reached a
// point under one filter starts over under another, so a newly allowed
// owner's history is found.
func (f OwnerFilter) fingerprint() string {
	return "include=" + strings.Join(f.include, ",") + ";exclude=" + strings.Join(f.exclude, ",")
}
