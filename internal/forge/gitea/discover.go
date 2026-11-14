package gitea

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/forge"
)

const (
	// overlap is how far each scan reads back before the newest update the
	// last complete scan read; what it reads again is skipped. The issue
	// search reads Gitea's issue indexer, which sees an update a moment after
	// the database does.
	overlap = 5 * time.Minute
	// reconcileSlack reads plain comments from before the first page was
	// fetched, which this machine's clock timed, not the server's. A fetch
	// may finish much later after a pause.
	reconcileSlack = 10 * time.Minute
)

// searchFlags are the issue search's filters for the account's involvement.
// Set together they must all hold (routers/api/v1/repo/issue.go), so each
// is searched on its own.
var searchFlags = []string{"created", "assigned", "mentioned", "review_requested", "reviewed"}

// cursor is how far each discovery stream came.
type cursor struct {
	// Streams are the listed streams' positions, by stream name.
	Streams map[string]*position `json:"streams,omitempty"`
	// Reconciled is the archived thread the rotation of checks last reached.
	Reconciled *mark `json:"reconciled,omitempty"`
}

// position is how far one stream came: through its last complete scan, and
// through a scan a sync left unfinished.
type position struct {
	// Since is the newest update a complete scan read.
	Since time.Time `json:"since,omitzero"`
	// Seen holds what was read within overlap of Since, by entry, at the
	// update read: the next scan, which reads back by overlap, skips those
	// unless they changed again.
	Seen map[string]time.Time `json:"seen,omitempty"`
	// Scan is the scan in progress.
	Scan *scan `json:"scan,omitempty"`
}

// scan is a scan in progress: the page it reads next, and the newest
// update it read with what it read within overlap of it.
type scan struct {
	Page   int                  `json:"page"`
	Newest time.Time            `json:"newest,omitzero"`
	Recent map[string]time.Time `json:"recent,omitempty"`
}

func (c *cursor) stream(name string) *position {
	if c.Streams == nil {
		c.Streams = map[string]*position{}
	}
	p, ok := c.Streams[name]
	if !ok {
		p = &position{}
		c.Streams[name] = p
	}
	return p
}

func (c *cursor) encode() []byte {
	data, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		panic(fmt.Sprintf("encode Gitea cursor: %v", err)) // maps of times and ints always encode
	}
	return data
}

// from is the update a scan reads back to; zero reads the whole list.
func (p *position) from() time.Time {
	if p.Since.IsZero() {
		return time.Time{}
	}
	return p.Since.Add(-overlap)
}

// fresh reports whether an entry read at update at is new to the stream.
func (p *position) fresh(key string, at time.Time) bool {
	seen, ok := p.Seen[key]
	return !ok || !seen.Equal(at)
}

// read notes an entry the scan read.
func (s *scan) read(key string, at time.Time) {
	if at.After(s.Newest) {
		s.Newest = at
		maps.DeleteFunc(s.Recent, func(_ string, read time.Time) bool { return read.Before(at.Add(-overlap)) })
	}
	if at.Before(s.Newest.Add(-overlap)) {
		return
	}
	if s.Recent == nil {
		s.Recent = map[string]time.Time{}
	}
	s.Recent[key] = at
}

// finish completes the scan: the stream now reaches as far as it read.
func (p *position) finish() {
	done := p.Scan
	p.Scan = nil
	if done.Newest.IsZero() {
		return
	}
	if done.Newest.After(p.Since) {
		p.Since = done.Newest
	}
	seen := maps.Clone(p.Seen)
	if seen == nil {
		seen = map[string]time.Time{}
	}
	maps.Copy(seen, done.Recent)
	maps.DeleteFunc(seen, func(_ string, read time.Time) bool { return read.Before(p.Since.Add(-overlap)) })
	p.Seen = seen
}

// entry is one item of a stream's list: its own key and update, and the
// thread it points at when that is a thread the sync archives.
type entry struct {
	key     string
	at      time.Time
	ref     forge.ThreadRef
	archive bool
}

// archivedThreads is the archive's threads, read once per discovery when a
// stream first asks about one.
type archivedThreads struct {
	known   forge.KnownThreads
	threads map[string]forge.KnownThread
}

// holds reports whether the archive holds ref's update, as the core decides
// it. A thread the archive lacks is queued or was dropped, so a stream
// reading it again unchanged has nothing to add.
func (a *archivedThreads) holds(ctx context.Context, ref forge.ThreadRef) (bool, error) {
	if a.known == nil {
		return true, nil
	}
	if a.threads == nil {
		threads := map[string]forge.KnownThread{}
		if err := a.known.Each(ctx, func(k forge.KnownThread) error {
			threads[k.Ref.Key] = k
			return nil
		}); err != nil {
			return false, fmt.Errorf("read archived Gitea threads: %w", err)
		}
		a.threads = threads
	}
	k, ok := a.threads[ref.Key]
	return !ok || k.Holds(ref.UpdatedAt), nil
}

// lister reads one page of a stream's list, back to since (zero for all of
// it), and reports whether more pages follow.
type lister func(ctx context.Context, page int, since time.Time) ([]entry, bool, error)

// Discover reads the account's notifications, searches the issues and pull
// requests it is involved in, reads its own comment activity, then checks a
// share of the archived threads again. Each stream keeps its own position.
func (f *Forge) Discover(ctx context.Context, raw []byte, owners forge.OwnerFilter, known forge.KnownThreads,
	page func([]forge.ThreadRef, []byte) error,
) error {
	var state cursor
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return fmt.Errorf("decode Gitea discovery cursor: %w", err)
		}
	}
	login, err := f.login(ctx)
	if err != nil {
		return err
	}
	archive := &archivedThreads{known: known}
	if err := f.scanStream(ctx, &state, "notifications", f.notifications(owners), archive, page); err != nil {
		return err
	}
	for _, flag := range searchFlags {
		// The search takes one owner per request, so a limited filter is
		// searched owner by owner and other owners' threads never arrive.
		include := owners.Include()
		if len(include) == 0 {
			include = []string{""}
		}
		for _, owner := range include {
			name := flag
			if owner != "" {
				name += "@" + owner
			}
			if err := f.scanStream(ctx, &state, name, f.search(flag, owner, owners), archive, page); err != nil {
				return err
			}
		}
	}
	if err := f.scanStream(ctx, &state, "activity", f.activity(login, owners), archive, page); err != nil {
		return err
	}
	return f.reconcileKnown(ctx, &state, owners, known, page)
}

// scanStream reads a stream's list page by page and reports each page's new
// threads as soon as it is read. The lists are ordered by update or
// creation, newest first: an entry added or changed since a page was read
// moves above it, which only repeats entries on later pages. A scan a sync
// left unfinished resumes a page early, which covers entries deleted since.
// An entry read again at the update it was read at before is skipped, unless
// the archive doesn't hold that update: the thread may have changed again
// in the same second, after its fetch read it.
func (f *Forge) scanStream(ctx context.Context, state *cursor, name string, list lister, archive *archivedThreads,
	page func([]forge.ThreadRef, []byte) error,
) error {
	pos := state.stream(name)
	first := 1
	if pos.Scan != nil {
		first = max(1, pos.Scan.Page-1)
	} else {
		pos.Scan = &scan{Page: 1}
	}
	since := pos.from()
	for n := first; ; n++ {
		entries, more, err := list(ctx, n, since)
		if err != nil {
			return err
		}
		var refs []forge.ThreadRef
		for _, e := range entries {
			if !since.IsZero() && e.at.Before(since) {
				continue
			}
			if e.archive {
				report := pos.fresh(e.key, e.at)
				if !report {
					held, err := archive.holds(ctx, e.ref)
					if err != nil {
						return err
					}
					report = !held
				}
				if report {
					refs = append(refs, e.ref)
				}
			}
			pos.Scan.read(e.key, e.at)
		}
		pos.Scan.Page = n + 1
		if !more {
			pos.finish()
		}
		if err := page(refs, state.encode()); err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// repository is a repository as notifications and activity show it.
type repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Owner    *user  `json:"owner"`
}

// notification is one thread of GET /notifications. Gitea gives no reason.
type notification struct {
	ID         int64       `json:"id"`
	UpdatedAt  time.Time   `json:"updated_at"`
	Repository *repository `json:"repository"`
	Subject    *struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"subject"`
}

// notifications lists every notification thread, read or not. The list is
// one row per thread, newest update first; since keeps those updated at or
// after it.
func (f *Forge) notifications(owners forge.OwnerFilter) lister {
	return func(ctx context.Context, page int, since time.Time) ([]entry, bool, error) {
		limit, err := f.pageLimit(ctx)
		if err != nil {
			return nil, false, err
		}
		query := pageQuery(page, limit)
		query.Set("all", "true")
		if !since.IsZero() {
			query.Set("since", since.UTC().Format(time.RFC3339))
		}
		var batch []notification
		header, err := f.get(ctx, "/notifications", query, &batch)
		if err != nil {
			return nil, false, fmt.Errorf("list Gitea notifications: %w", err)
		}
		entries := make([]entry, 0, len(batch))
		for _, n := range batch {
			e := entry{key: strconv.FormatInt(n.ID, 10), at: n.UpdatedAt}
			e.ref, e.archive = notificationRef(n)
			e.archive = e.archive && owners.Allows(e.ref.Owner)
			entries = append(entries, e)
		}
		return entries, hasMore(header, page, len(batch), limit), nil
	}
}

// notificationRef is the issue or pull request a notification points at.
// Gitea leaves out the repository and subject when the account lost access,
// and its commit and repository notifications are no thread.
func notificationRef(n notification) (forge.ThreadRef, bool) {
	if n.Repository == nil || n.Repository.Owner == nil || n.Subject == nil {
		return forge.ThreadRef{}, false
	}
	var kind forge.Kind
	switch n.Subject.Type {
	case "Issue":
		kind = forge.KindIssue
	case "Pull":
		kind = forge.KindPullRequest
	default:
		return forge.ThreadRef{}, false
	}
	number, ok := subjectNumber(n.Subject.URL)
	if !ok {
		return forge.ThreadRef{}, false
	}
	return threadRef(kind, n.Repository.ID, n.Repository.Owner.Login, n.Repository.FullName, number, n.UpdatedAt)
}

// subjectNumber reads the number from a subject's API URL,
// .../repos/{owner}/{repo}/issues/{index}, which Gitea uses for pull
// requests too.
func subjectNumber(subjectURL string) (int, bool) {
	parsed, err := url.Parse(subjectURL)
	if err != nil {
		return 0, false
	}
	dir, last := path.Split(strings.TrimRight(parsed.Path, "/"))
	if kind := path.Base(dir); kind != "issues" && kind != "pulls" {
		return 0, false
	}
	number, err := strconv.Atoi(last)
	return number, err == nil && number > 0
}

// threadRef names an issue or pull request; ok is false for one without a
// repository or number.
func threadRef(kind forge.Kind, repoID int64, owner, repo string, number int, updated time.Time) (forge.ThreadRef, bool) {
	if repoID <= 0 || owner == "" || repo == "" || number <= 0 {
		return forge.ThreadRef{}, false
	}
	return forge.ThreadRef{
		Kind: kind, Key: threadKey(repoID, number), Owner: owner, Repo: repo, Number: number, UpdatedAt: updated,
	}, true
}

// threadKey is an issue's or pull request's key in the sync's queue: they
// share one number sequence per repository. The conversation is keyed by
// the issue's ID, which a move doesn't change.
func threadKey(repoID int64, number int) string {
	return strconv.FormatInt(repoID, 10) + "#" + strconv.Itoa(number)
}

// issueHit is one result of GET /repos/issues/search.
type issueHit struct {
	ID          int64     `json:"id"`
	Number      int       `json:"number"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request"`
	Repository  *struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Owner    string `json:"owner"`
	} `json:"repository"`
}

// search lists the issues and pull requests one involvement flag finds,
// open or closed, in owner's repositories when owner is set. Gitea orders
// them by creation, newest first; since keeps those updated at or after it.
func (f *Forge) search(flag, owner string, owners forge.OwnerFilter) lister {
	return func(ctx context.Context, page int, since time.Time) ([]entry, bool, error) {
		limit, err := f.pageLimit(ctx)
		if err != nil {
			return nil, false, err
		}
		query := pageQuery(page, limit)
		query.Set("state", "all")
		query.Set(flag, "true")
		if owner != "" {
			query.Set("owner", owner)
		}
		if !since.IsZero() {
			query.Set("since", since.UTC().Format(time.RFC3339))
		}
		var batch []issueHit
		header, err := f.get(ctx, "/repos/issues/search", query, &batch)
		if err != nil {
			if owner != "" {
				return nil, false, fmt.Errorf("search Gitea threads (%s) of %s, which include_owners names: %w", flag, owner, err)
			}
			return nil, false, fmt.Errorf("search Gitea threads (%s): %w", flag, err)
		}
		entries := make([]entry, 0, len(batch))
		for _, hit := range batch {
			e := entry{key: strconv.FormatInt(hit.ID, 10), at: hit.UpdatedAt}
			if hit.Repository != nil {
				kind := forge.KindIssue
				if hit.PullRequest != nil {
					kind = forge.KindPullRequest
				}
				e.ref, e.archive = threadRef(kind, hit.Repository.ID, hit.Repository.Owner, hit.Repository.FullName, hit.Number, hit.UpdatedAt)
				e.archive = e.archive && owners.Allows(e.ref.Owner)
			}
			entries = append(entries, e)
		}
		return entries, hasMore(header, page, len(batch), limit), nil
	}
}

// activity is one entry of a user's activity feed.
type activity struct {
	ID      int64       `json:"id"`
	OpType  string      `json:"op_type"`
	Created time.Time   `json:"created"`
	Content string      `json:"content"`
	Repo    *repository `json:"repo"`
}

// activity lists the comments the account wrote, from its own activity
// feed, newest first. Gitea keeps a feed's history unless its admin turns
// on deleting old actions. The feed takes no since, so reading stops at the
// first entry older than it.
func (f *Forge) activity(login string, owners forge.OwnerFilter) lister {
	return func(ctx context.Context, page int, since time.Time) ([]entry, bool, error) {
		limit, err := f.pageLimit(ctx)
		if err != nil {
			return nil, false, err
		}
		query := pageQuery(page, limit)
		query.Set("only-performed-by", "true")
		var batch []activity
		header, err := f.get(ctx, "/users/"+url.PathEscape(login)+"/activities/feeds", query, &batch)
		if err != nil {
			return nil, false, fmt.Errorf("list Gitea activity of %s: %w", login, err)
		}
		more := hasMore(header, page, len(batch), limit)
		entries := make([]entry, 0, len(batch))
		for _, a := range batch {
			if !since.IsZero() && a.Created.Before(since) {
				more = false
			}
			e := entry{key: strconv.FormatInt(a.ID, 10), at: a.Created}
			e.ref, e.archive = activityRef(a)
			e.archive = e.archive && owners.Allows(e.ref.Owner)
			entries = append(entries, e)
		}
		return entries, more, nil
	}
}

// activityRef is the thread a comment activity points at. Gitea writes a
// comment's content as "<index>|<excerpt>" (services/feed/notifier.go); the
// excerpt is never kept.
func activityRef(a activity) (forge.ThreadRef, bool) {
	var kind forge.Kind
	switch a.OpType {
	case "comment_issue":
		kind = forge.KindIssue
	case "comment_pull":
		kind = forge.KindPullRequest
	default:
		return forge.ThreadRef{}, false
	}
	if a.Repo == nil || a.Repo.Owner == nil {
		return forge.ThreadRef{}, false
	}
	index, _, ok := strings.Cut(a.Content, "|")
	number, err := strconv.Atoi(index)
	if !ok || err != nil {
		return forge.ThreadRef{}, false
	}
	return threadRef(kind, a.Repo.ID, a.Repo.Owner.Login, a.Repo.FullName, number, a.Created)
}

// mark is an archived thread's place in the rotation of checks, which goes
// through archived threads least recently fetched first.
type mark struct {
	CheckedAt time.Time `json:"checked_at"`
	Key       string    `json:"key"`
}

func compareKnown(a, b forge.KnownThread) int {
	return cmp.Or(a.CheckedAt.Compare(b.CheckedAt), strings.Compare(a.Ref.Key, b.Ref.Key))
}

// reconcileKnown checks the next share of the archived threads for updates
// since each was last archived, continuing a rotation through all of them.
// Notifications stop for an unwatched thread, and the account's activity
// shows only what it wrote. A thread that changed is reported, and so is
// one whose check fails, so that its fetch records why. The rotation keeps
// its own place: a thread's fetch time moves only when it is fetched, so the
// least recently fetched threads stay the same while none of them changes.
func (f *Forge) reconcileKnown(ctx context.Context, state *cursor, owners forge.OwnerFilter, known forge.KnownThreads,
	page func([]forge.ThreadRef, []byte) error,
) error {
	if known == nil || f.reconcile <= 0 {
		return nil
	}
	var threads []forge.KnownThread
	if err := known.Each(ctx, func(k forge.KnownThread) error {
		threads = append(threads, k)
		return nil
	}); err != nil {
		return err
	}
	slices.SortFunc(threads, compareKnown)
	start := 0
	if state.Reconciled != nil {
		reached := forge.KnownThread{CheckedAt: state.Reconciled.CheckedAt, Ref: forge.ThreadRef{Key: state.Reconciled.Key}}
		start, _ = slices.BinarySearchFunc(threads, reached, compareKnown)
		if start < len(threads) && compareKnown(threads[start], reached) == 0 {
			start++
		}
	}
	rotation := slices.Concat(threads[start:], threads[:start])
	for _, k := range rotation[:min(f.reconcile, len(rotation))] {
		ref, changed, err := f.recheck(ctx, k, owners)
		if err != nil {
			return err
		}
		state.Reconciled = &mark{CheckedAt: k.CheckedAt, Key: k.Ref.Key}
		var refs []forge.ThreadRef
		if changed {
			refs = []forge.ThreadRef{ref}
		}
		if err := page(refs, state.encode()); err != nil {
			return err
		}
	}
	return nil
}

// recheck compares the thread's own update time with its archived update.
// New reviews, code comments and edits to the opening post all move it.
// If it hasn't moved, recent plain comments are checked for edits after the
// archived update. An update in the archived update's own second counts
// unless the archived fetch read the thread well after it. Edits to existing
// code comments cannot be detected without fetching the thread: neither the
// plain comment list nor the timeline includes them.
// Its repository's current owner is checked first, so an excluded owner's
// thread is never read.
func (f *Forge) recheck(ctx context.Context, k forge.KnownThread, owners forge.OwnerFilter) (forge.ThreadRef, bool, error) {
	ref := k.Ref
	if !owners.Allows(ref.Owner) || ref.Number <= 0 {
		return ref, false, nil
	}
	owner, err := f.Owner(ctx, ref)
	if err == nil {
		if !owners.Allows(owner) {
			return ref, false, nil
		}
		ref.Owner = owner
		var issue struct {
			UpdatedAt time.Time `json:"updated_at"`
		}
		if _, err = f.get(ctx, issuePath(ref.Repo, ref.Number), nil, &issue); err == nil {
			if issue.UpdatedAt.After(ref.UpdatedAt) {
				ref.UpdatedAt = issue.UpdatedAt
				return ref, true, nil
			}
			var comments []struct {
				UpdatedAt time.Time `json:"updated_at"`
			}
			query := url.Values{"since": {k.ReadAt.Add(-reconcileSlack).UTC().Format(time.RFC3339)}}
			if _, err = f.get(ctx, issuePath(ref.Repo, ref.Number)+"/comments", query, &comments); err == nil {
				for _, c := range comments {
					if c.UpdatedAt.After(ref.UpdatedAt) {
						ref.UpdatedAt = c.UpdatedAt
					}
				}
				// An update in the same second as the archived one is
				// reported unless the archived fetch read the thread well
				// after it.
				return ref, !k.Holds(ref.UpdatedAt), nil
			}
		}
	}
	if forge.IsRateLimit(err) || ctx.Err() != nil {
		return ref, false, err
	}
	// The check failed: the thread may have changed up to now, so it is
	// reported as updated now, which the sync doesn't take for unchanged,
	// and its fetch records why.
	if now := f.now(); now.After(ref.UpdatedAt) {
		ref.UpdatedAt = now
	}
	return ref, true, nil
}

// issuePath is an issue's or pull request's API path.
func issuePath(repo string, number int) string {
	return repoPath(repo) + "/issues/" + strconv.Itoa(number)
}
