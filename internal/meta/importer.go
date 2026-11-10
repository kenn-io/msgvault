package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
)

const DefaultMaxRequests = 1000

var errRequestBudget = errors.New("facebook request budget exhausted")

type ImportOptions struct {
	MaxRequests int
	Full        bool
}

type ImportSummary struct {
	MessagesProcessed int
	MessagesAdded     int
	MessagesUpdated   int
	Requests          int
	Partial           bool
}

type Importer struct {
	store     *store.Store
	api       API
	instagram string
	igAPI     instagramAPI
}

func NewImporter(st *store.Store, api API) *Importer { return &Importer{store: st, api: api} }

func SourceIdentifier(app, page string) string { return "app:" + app + ":page:" + page }

func ParseSourceIdentifier(identifier string) (app, page string, ok bool) {
	parts := strings.Split(identifier, ":")
	if len(parts) != 4 || parts[0] != "app" || parts[2] != "page" || !ValidAccountID(parts[1]) || !ValidAccountID(parts[3]) {
		return "", "", false
	}
	return parts[1], parts[3], true
}

// Register verifies the supplied Page token before any archive write. This
// does not discover other assets or assert the app's permission grants.
func (imp *Importer) Register(ctx context.Context, app, page string) (*store.Source, error) {
	if !ValidAccountID(app) || !ValidAccountID(page) {
		return nil, errors.New("invalid Facebook app or Page ID")
	}
	identity, err := imp.api.VerifyPage(ctx, page)
	if err != nil {
		return nil, err
	}
	anchor, provenance := ActorIdentifier(app, page, page), "facebook_page_token"
	if imp.instagram != "" {
		account, err := imp.igAPI.LinkedInstagramAccount(ctx, page, imp.instagram)
		if err != nil {
			return nil, err
		}
		identity.Name = account.Username
		anchor, provenance = instagramActorIdentifier(app, page, imp.instagram, "ig_user", imp.instagram), "instagram_page_token"
	}
	return registerSource(ctx, imp.store, imp.sourceType(), imp.sourceIdentifier(app, page), anchor, provenance, identity.Name)
}

// registerSource records a verified account as a source and names it.
func registerSource(ctx context.Context, st *store.Store, sourceType, identifier, anchor, provenance, name string) (*store.Source, error) {
	source, err := st.GetOrCreateSourceContext(ctx, sourceType, identifier)
	if err != nil {
		return nil, err
	}
	source, execution, err := acquireSource(ctx, st, source.SourceType, source.Identifier)
	if err != nil {
		return nil, err
	}
	defer func() { _ = execution.Release() }()
	if err = st.AddAccountIdentityContext(ctx, source.ID, anchor, provenance); err != nil {
		return nil, err
	}
	if err = st.UpdateSourceDisplayNameContext(ctx, source.ID, name); err != nil {
		return nil, err
	}
	return st.GetSourceByIDContext(ctx, source.ID)
}

// acquireSource takes the source's execution lock and refuses a retired
// source. The caller releases the returned execution.
func acquireSource(ctx context.Context, st *store.Store, sourceType, identifier string) (*store.Source, *store.SyncExecution, error) {
	source, err := st.GetSourceByTypeAndIdentifier(ctx, sourceType, identifier)
	if err != nil {
		return nil, nil, err
	}
	execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return nil, nil, err
	}
	source, err = st.GetSourceByIDContext(ctx, source.ID)
	if err == nil && (source.HistoryOnly || source.MergedIntoSourceID != 0) {
		err = store.ErrSourceRetired
	}
	if err != nil {
		return nil, nil, errors.Join(err, execution.Release())
	}
	return source, execution, nil
}

// finishSync records how a polling run ended. Only a complete run advances the
// source cursor; a failed or paused run keeps its checkpoint for the next run.
func finishSync(ctx context.Context, st *store.Store, sourceID, syncID int64, cp *store.Checkpoint, partial bool, runErr error) error {
	switch {
	case runErr != nil:
		if jobctx.ErrorAfterYield(ctx, runErr) != nil {
			cp.ErrorsCount = 1
		}
		return errors.Join(runErr, st.InterruptSyncWithCheckpoint(ctx, syncID, runErr, cp))
	case partial:
		return st.PauseSyncWithCheckpoint(ctx, syncID, cp)
	default:
		return st.ScopedToSync(sourceID, syncID).CompleteSyncAndUpdateSourceCursorContext(ctx, syncID, sourceID, cp.PageToken)
	}
}

type importRun struct {
	imp         *Importer
	app, page   string
	source      *store.Source
	state       *syncState
	sum         *ImportSummary
	maxRequests int
	syncID      int64
}

func (imp *Importer) Import(ctx context.Context, app, page string, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if !ValidAccountID(app) || !ValidAccountID(page) || opts.MaxRequests < 0 {
		return nil, errors.New("invalid Facebook import options")
	}
	source, execution, err := acquireSource(ctx, imp.store, imp.sourceType(), imp.sourceIdentifier(app, page))
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	state, err := loadState(ctx, imp.store, source, opts.Full)
	if err != nil {
		return nil, err
	}
	syncID, err := execution.StartSyncContext(ctx, imp.sourceType(), "")
	if err != nil {
		return nil, err
	}
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	sum = &ImportSummary{}
	maximum := opts.MaxRequests
	if maximum == 0 {
		maximum = DefaultMaxRequests
	}
	run := &importRun{imp: &scoped, app: app, page: page, source: source, state: state, sum: sum, maxRequests: maximum, syncID: syncID}
	defer func() {
		sum.Partial = retErr != nil || !state.complete()
		checkpoint, err := run.checkpoint()
		if err != nil {
			retErr = errors.Join(retErr, err)
			return
		}
		retErr = finishSync(ctx, imp.store, source.ID, syncID, checkpoint, sum.Partial, retErr)
	}()
	// Publish the inherited state before any new request, so a crash cannot
	// conceal a newer failed/paused checkpoint behind this run.
	if err = run.save(ctx); err != nil {
		return sum, err
	}
	if err = run.verify(ctx); errors.Is(err, errRequestBudget) {
		return sum, nil
	} else if err != nil {
		return sum, err
	}
	var errs []error
	// Collection attempts update continuation for the next run, not this order.
	edges := state.edges()
	start := slices.Index(edges, state.NextSurface)
	for offset := range edges {
		edge := edges[(start+offset)%len(edges)]
		if err = run.phase(ctx, edge); err != nil {
			if errors.Is(err, errRequestBudget) {
				break
			}
			errs = append(errs, fmt.Errorf("%s %s: %w", run.providerName(), edge, err))
			if ctx.Err() != nil {
				break
			}
		}
	}
	phaseErr := errors.Join(errs...)
	if jobctx.ErrorAfterYield(ctx, phaseErr) != nil {
		for _, edge := range edges {
			if state.Phases[edge].Done {
				state.Phases[edge].Rescan = true
			}
		}
	}
	return sum, phaseErr
}

func (r *importRun) verify(ctx context.Context) error {
	reads := []func() error{func() error {
		_, err := r.imp.api.VerifyPage(ctx, r.page)
		return err
	}}
	if r.imp.instagram != "" {
		reads = append(reads, func() error {
			_, err := r.imp.igAPI.LinkedInstagramAccount(ctx, r.page, r.imp.instagram)
			return err
		})
	}
	for _, read := range reads {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.sum.Requests >= r.maxRequests {
			return errRequestBudget
		}
		r.sum.Requests++
		if err := read(); err != nil {
			return err
		}
	}
	return nil
}

func (r *importRun) checkpoint() (*store.Checkpoint, error) {
	data, err := json.Marshal(r.state)
	if err != nil {
		return nil, err
	}
	return &store.Checkpoint{PageToken: string(data), MessagesAdded: int64(r.sum.MessagesAdded), MessagesUpdated: int64(r.sum.MessagesUpdated), MessagesProcessed: int64(r.sum.MessagesProcessed)}, nil
}

func (r *importRun) save(ctx context.Context) error {
	cp, err := r.checkpoint()
	if err != nil {
		return err
	}
	return r.imp.store.UpdateSyncCheckpointContext(ctx, r.syncID, cp)
}

func (r *importRun) request(ctx context.Context, edge PostEdge, after string, read func() (ObjectPage, error)) (ObjectPage, error) {
	if err := ctx.Err(); err != nil {
		return ObjectPage{}, err
	}
	if r.sum.Requests >= r.maxRequests {
		return ObjectPage{}, errRequestBudget
	}
	r.sum.Requests++
	edges := r.state.edges()
	r.state.NextSurface = edges[(slices.Index(edges, edge)+1)%len(edges)]
	page, err := read()
	var apiErr *APIError
	if after != "" && errors.As(err, &apiErr) && apiErr.Status == 400 {
		return ObjectPage{}, fmt.Errorf("%s continuation request failed: %w", r.providerName(), err)
	}
	return page, err
}

func (r *importRun) phase(ctx context.Context, edge PostEdge) error {
	p := r.state.Phases[edge]
	for !p.Done {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(p.Stack) > 0 {
			if err := r.comments(ctx, p, edge); err != nil {
				return err
			}
		} else if len(p.Posts) > 0 && !p.postsRead {
			// Posts saved by an earlier run may be older than a webhook
			// change since; read their page again before archiving them.
			page, err := r.request(ctx, edge, p.PostsAfter, func() (ObjectPage, error) { return r.readPosts(ctx, edge, p.PostsAfter) })
			if err != nil {
				return err
			}
			p.Posts, p.postsRead = keepSaved(p.Posts, page.Data), true
		} else if len(p.Posts) > 0 {
			post := p.Posts[0]
			if err := r.persist(ctx, post, post.ID, "", edge); err != nil {
				return fmt.Errorf("%s post %q: %w", r.providerName(), post.ID, err)
			}
			p.Posts = p.Posts[1:]
			p.Stack = append(p.Stack, commentFrame{Root: post.ID, Parent: post.ID})
		} else if p.Exhausted {
			p.Done = true
		} else {
			page, err := r.request(ctx, edge, p.After, func() (ObjectPage, error) { return r.readPosts(ctx, edge, p.After) })
			if err != nil {
				return err
			}
			p.Posts, p.postsRead = page.Data, true
			p.PostsAfter, p.After = p.After, page.After
			p.Exhausted = page.After == ""
		}
		if err := r.save(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *importRun) comments(ctx context.Context, p *phaseState, edge PostEdge) error {
	index := len(p.Stack) - 1
	f := &p.Stack[index]
	if !f.Loaded || (len(f.Pending) > 0 && !f.read) {
		page, err := r.request(ctx, edge, f.After, func() (ObjectPage, error) { return r.readComments(ctx, *f) })
		if err != nil {
			return fmt.Errorf("%s comments for %q: %w", r.providerName(), f.Parent, err)
		}
		if f.Loaded {
			// Comments saved by an earlier run may be older than a webhook
			// change since; keep only the ones not archived yet, as read now.
			f.Pending, f.read = keepSaved(f.Pending, page.Data), true
			return nil
		}
		f.Pending, f.Next, f.Loaded, f.read = page.Data, page.After, true, true
		return nil
	}
	if len(f.Pending) > 0 {
		child := f.Pending[0]
		for _, ancestor := range p.Stack {
			if child.ID == ancestor.Parent {
				return fmt.Errorf("%s comment %q: cyclic Facebook reply traversal", r.providerName(), child.ID)
			}
		}
		if len(p.Stack) >= 128 {
			return fmt.Errorf("%s comment %q: facebook reply traversal exceeds bounded depth", r.providerName(), child.ID)
		}
		if err := r.persist(ctx, child, f.Root, f.Parent, edge); err != nil {
			return fmt.Errorf("%s comment %q: %w", r.providerName(), child.ID, err)
		}
		f.Pending = f.Pending[1:]
		p.Stack = append(p.Stack, commentFrame{Root: f.Root, Parent: child.ID})
	} else if f.Next != "" {
		f.After, f.Next, f.Loaded = f.Next, "", false
	} else {
		p.Stack = p.Stack[:index]
	}
	return nil
}

// keepSaved returns the fresh objects whose IDs are in saved, in fresh order.
// An object no longer on the page is left for the next complete scan.
func keepSaved(saved, fresh []Object) []Object {
	ids := make(map[string]bool, len(saved))
	for _, object := range saved {
		ids[object.ID] = true
	}
	return slices.DeleteFunc(fresh, func(object Object) bool { return !ids[object.ID] })
}
