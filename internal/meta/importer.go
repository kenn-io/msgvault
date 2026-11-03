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
	Requests          int
	Partial           bool
}

type Importer struct {
	store *store.Store
	api   API
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
	source, err := imp.store.GetOrCreateSourceContext(ctx, SourceType, SourceIdentifier(app, page))
	if err != nil {
		return nil, err
	}
	execution, err := imp.store.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = execution.Release() }()
	current, err := imp.store.GetSourceByIDContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	if current.HistoryOnly || current.MergedIntoSourceID != 0 {
		return nil, store.ErrSourceRetired
	}
	if err = imp.store.AddAccountIdentityContext(ctx, source.ID, ActorIdentifier(app, page, page), "facebook_page_token"); err != nil {
		return nil, err
	}
	if err = imp.store.UpdateSourceDisplayNameContext(ctx, source.ID, identity.Name); err != nil {
		return nil, err
	}
	return imp.store.GetSourceByIDContext(ctx, source.ID)
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
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, SourceIdentifier(app, page))
	if err != nil {
		return nil, err
	}
	execution, err := imp.store.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	source, err = imp.store.GetSourceByIDContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	if source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return nil, store.ErrSourceRetired
	}
	state, err := loadState(ctx, imp.store, source, opts.Full)
	if err != nil {
		return nil, err
	}
	syncID, err := execution.StartSyncContext(ctx, SourceType, "")
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
		if retErr != nil {
			if jobctx.ErrorAfterYield(ctx, retErr) != nil {
				checkpoint.ErrorsCount = 1
			}
			retErr = errors.Join(retErr, imp.store.InterruptSyncWithCheckpoint(ctx, syncID, retErr, checkpoint))
		} else if sum.Partial {
			retErr = imp.store.PauseSyncWithCheckpoint(syncID, checkpoint)
		} else {
			retErr = scoped.store.CompleteSyncAndUpdateSourceCursorContext(ctx, syncID, source.ID, checkpoint.PageToken)
		}
	}()
	// Publish the inherited state before any new request, so a crash cannot
	// conceal a newer failed/paused checkpoint behind this run.
	if err = run.save(ctx); err != nil {
		return sum, err
	}
	sum.Requests++
	if _, err = imp.api.VerifyPage(ctx, page); err != nil {
		return sum, err
	}
	var errs []error
	// Collection attempts update continuation for the next run, not this order.
	start := slices.Index(postEdges[:], state.NextSurface)
	for offset := range postEdges {
		edge := postEdges[(start+offset)%len(postEdges)]
		if err = run.phase(ctx, edge); err != nil {
			if errors.Is(err, errRequestBudget) {
				break
			}
			errs = append(errs, fmt.Errorf("facebook %s: %w", edge, err))
			if ctx.Err() != nil {
				break
			}
		}
	}
	phaseErr := errors.Join(errs...)
	if jobctx.ErrorAfterYield(ctx, phaseErr) != nil {
		for _, edge := range postEdges {
			if state.Phases[edge].Done {
				state.Phases[edge].Rescan = true
			}
		}
	}
	return sum, phaseErr
}

func (r *importRun) checkpoint() (*store.Checkpoint, error) {
	data, err := json.Marshal(r.state)
	if err != nil {
		return nil, err
	}
	return &store.Checkpoint{PageToken: string(data), MessagesAdded: int64(r.sum.MessagesAdded), MessagesProcessed: int64(r.sum.MessagesProcessed)}, nil
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
	r.state.NextSurface = postEdges[(slices.Index(postEdges[:], edge)+1)%len(postEdges)]
	page, err := read()
	var apiErr *APIError
	if after != "" && errors.As(err, &apiErr) && apiErr.Status == 400 {
		return ObjectPage{}, fmt.Errorf("facebook continuation request failed: %w", err)
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
		} else if len(p.Posts) > 0 {
			post := p.Posts[0]
			if err := r.persist(ctx, post, post.ID, "", edge); err != nil {
				return fmt.Errorf("facebook post %q: %w", post.ID, err)
			}
			p.Posts = p.Posts[1:]
			p.Stack = append(p.Stack, commentFrame{Root: post.ID, Parent: post.ID})
		} else if p.Exhausted {
			p.Done = true
		} else {
			page, err := r.request(ctx, edge, p.After, func() (ObjectPage, error) { return r.imp.api.Posts(ctx, r.page, edge, p.After) })
			if err != nil {
				return err
			}
			p.Posts = page.Data
			p.After = page.After
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
	if !f.Loaded {
		page, err := r.request(ctx, edge, f.After, func() (ObjectPage, error) { return r.imp.api.Comments(ctx, f.Parent, f.After) })
		if err != nil {
			return fmt.Errorf("facebook comments for %q: %w", f.Parent, err)
		}
		f.Pending, f.Next, f.Loaded = page.Data, page.After, true
		return nil
	}
	if len(f.Pending) > 0 {
		child := f.Pending[0]
		for _, ancestor := range p.Stack {
			if child.ID == ancestor.Parent {
				return fmt.Errorf("facebook comment %q: cyclic Facebook reply traversal", child.ID)
			}
		}
		if len(p.Stack) >= 128 {
			return fmt.Errorf("facebook comment %q: facebook reply traversal exceeds bounded depth", child.ID)
		}
		if err := r.persist(ctx, child, f.Root, f.Parent, edge); err != nil {
			return fmt.Errorf("facebook comment %q: %w", child.ID, err)
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
