package tiktok

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
)

type CommentsAPI interface {
	Account(ctx context.Context, account string) (Account, error)
	Videos(ctx context.Context, account, cursor string) (VideoPage, error)
	Comments(ctx context.Context, account, video, parent, cursor string) (CommentPage, error)
}
type Importer struct {
	store *store.Store
	api   CommentsAPI
}

func NewImporter(st *store.Store, api CommentsAPI) *Importer { return &Importer{store: st, api: api} }

// SourceIdentifier binds archive identity to the app and selected account;
// the local profile name can change without splitting history.
func SourceIdentifier(app, account string) string {
	return beeperidentity.EncodePart(app) + ":" + beeperidentity.EncodePart(account)
}
func ActorIdentifier(app, account, actor string) string {
	return "tiktok:" + SourceIdentifier(app, account) + ":" + beeperidentity.EncodePart(actor)
}

func (imp *Importer) Register(ctx context.Context, app, account string) (*store.Source, error) {
	return imp.register(ctx, app, account, CommentsSourceType)
}
func (imp *Importer) register(ctx context.Context, app, account, kind string) (*store.Source, error) {
	return registerTikTokAccount(ctx, imp.store, imp.api, app, account, kind)
}

func registerTikTokAccount(ctx context.Context, st *store.Store, api interface {
	Account(ctx context.Context, account string) (Account, error)
}, app, account, kind string) (*store.Source, error) {
	if !ValidID(app) || !ValidID(account) {
		return nil, errors.New("invalid TikTok app or account selector")
	}
	profile, err := api.Account(ctx, account)
	if err != nil {
		return nil, err
	}
	source, err := st.GetOrCreateSourceContext(ctx, kind, SourceIdentifier(app, account))
	if err != nil {
		return nil, err
	}
	execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = execution.Release() }()
	source, err = st.GetSourceByIDContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	if source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return nil, store.ErrSourceRetired
	}
	if err := st.AddAccountIdentityContext(ctx, source.ID, ActorIdentifier(app, account, account), "tiktok_account_token"); err != nil {
		return nil, err
	}
	name := profile.DisplayName
	if name == "" {
		name = profile.Username
	}
	if err := st.UpdateSourceDisplayNameContext(ctx, source.ID, name); err != nil {
		return nil, err
	}
	return st.GetSourceByIDContext(ctx, source.ID)
}

type ImportOptions struct {
	MaxRequests int
	Full        bool
}
type ImportSummary struct {
	MessagesProcessed int  `json:"messages_processed"`
	MessagesAdded     int  `json:"messages_added"`
	Requests          int  `json:"requests"`
	Partial           bool `json:"partial"`
}
type syncState struct {
	Version   int         `json:"version"`
	Source    string      `json:"source"`
	Cursor    string      `json:"cursor,omitempty"`
	Exhausted bool        `json:"exhausted,omitempty"`
	Videos    []Video     `json:"videos,omitempty"`
	Active    *videoState `json:"active,omitempty"`
}
type videoState struct {
	Video     Video       `json:"video"`
	RootSaved bool        `json:"root_saved,omitempty"`
	Cursor    string      `json:"cursor,omitempty"`
	Exhausted bool        `json:"exhausted,omitempty"`
	Comments  []Comment   `json:"comments,omitempty"`
	Replies   *replyState `json:"replies,omitempty"`
}
type replyState struct {
	Parent    string    `json:"parent"`
	Cursor    string    `json:"cursor,omitempty"`
	Exhausted bool      `json:"exhausted,omitempty"`
	Comments  []Comment `json:"comments,omitempty"`
}

func (s *syncState) complete() bool       { return s.Exhausted && len(s.Videos) == 0 && s.Active == nil }
func freshState(source string) *syncState { return &syncState{Version: 1, Source: source} }

func loadState(ctx context.Context, st *store.Store, source *store.Source, full bool) (*syncState, error) {
	if full {
		return freshState(source.Identifier), nil
	}
	prior, err := st.GetLatestCheckpointedSyncByType(ctx, source.ID, CommentsSourceType)
	if errors.Is(err, store.ErrSyncRunNotFound) {
		return freshState(source.Identifier), nil
	}
	if err != nil {
		return nil, err
	}
	var state syncState
	if json.Unmarshal([]byte(prior.CursorBefore.String), &state) != nil || !state.valid(source.Identifier) {
		return nil, errors.New("invalid TikTok comment checkpoint\nhint: rerun source tiktok-comments sync with --full")
	}
	if state.complete() {
		return freshState(source.Identifier), nil
	}
	return &state, nil
}
func validCursor(value string) bool {
	return len(value) <= 4096 && !strings.ContainsAny(value, "\x00\r\n")
}
func (s *syncState) valid(source string) bool {
	if s.Version != 1 || s.Source != source || !validCursor(s.Cursor) || len(s.Videos) > 20 {
		return false
	}
	for _, v := range s.Videos {
		if !ValidID(string(v.ID)) {
			return false
		}
	}
	if a := s.Active; a != nil {
		if !ValidID(string(a.Video.ID)) || !validCursor(a.Cursor) || len(a.Comments) > 30 {
			return false
		}
		for _, c := range a.Comments {
			if !ValidID(string(c.ID)) {
				return false
			}
		}
		if r := a.Replies; r != nil {
			if !ValidID(r.Parent) || !validCursor(r.Cursor) || len(r.Comments) > 30 {
				return false
			}
			for _, c := range r.Comments {
				if !ValidID(string(c.ID)) {
					return false
				}
			}
		}
	}
	return true
}

var errBudget = errors.New("TikTok request budget exhausted")

type commentRun struct {
	imp          *Importer
	source       *store.Source
	app, account string
	state        *syncState
	sum          *ImportSummary
	maximum      int
	syncID       int64
}

func (imp *Importer) Import(ctx context.Context, app, account string, opts ImportOptions) (sum *ImportSummary, retErr error) {
	if !ValidID(app) || !ValidID(account) || opts.MaxRequests < 0 {
		return nil, errors.New("invalid TikTok import options")
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(ctx, CommentsSourceType, SourceIdentifier(app, account))
	if err != nil {
		return nil, fmt.Errorf("TikTok source is not registered: %w", err)
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
	syncID, err := execution.StartSyncContext(ctx, CommentsSourceType, "")
	if err != nil {
		return nil, err
	}
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	maximum := opts.MaxRequests
	if maximum == 0 {
		maximum = DefaultMaxRequests
	}
	sum = &ImportSummary{}
	r := &commentRun{imp: &scoped, source: source, app: app, account: account, state: state, sum: sum, maximum: maximum, syncID: syncID}
	ctx = withDispatchHook(ctx, r.consumeRequest)
	defer func() {
		sum.Partial = retErr != nil || !state.complete()
		cp, err := r.checkpoint()
		if err != nil {
			retErr = errors.Join(retErr, err)
			return
		}
		if retErr != nil {
			retErr = errors.Join(retErr, imp.store.InterruptSyncWithCheckpoint(ctx, syncID, retErr, cp))
		} else if sum.Partial {
			retErr = imp.store.PauseSyncWithCheckpoint(ctx, syncID, cp)
		} else {
			retErr = scoped.store.CompleteSyncAndUpdateSourceCursorContext(ctx, syncID, source.ID, cp.PageToken)
		}
	}()
	if err := r.save(ctx); err != nil {
		return sum, err
	}
	if _, err := imp.api.Account(ctx, account); err != nil {
		if errors.Is(err, errBudget) {
			return sum, nil
		}
		return sum, err
	}
	for !state.complete() {
		if err := r.step(ctx); err != nil {
			if errors.Is(err, errBudget) {
				return sum, nil
			}
			return sum, err
		}
		if err := r.save(ctx); err != nil {
			return sum, err
		}
	}
	return sum, nil
}
func (r *commentRun) checkpoint() (*store.Checkpoint, error) {
	data, err := json.Marshal(r.state)
	return &store.Checkpoint{PageToken: string(data), MessagesAdded: int64(r.sum.MessagesAdded), MessagesProcessed: int64(r.sum.MessagesProcessed)}, err
}
func (r *commentRun) save(ctx context.Context) error {
	cp, err := r.checkpoint()
	if err != nil {
		return err
	}
	return r.imp.store.UpdateSyncCheckpointContext(ctx, r.syncID, cp)
}
func (r *commentRun) consumeRequest() error {
	if r.sum.Requests >= r.maximum {
		return errBudget
	}
	r.sum.Requests++
	return nil
}

func nextCursor(current string, next ID, more bool) (string, error) {
	if !more {
		return "", nil
	}
	if !validCursor(string(next)) || next == "" || string(next) == initialCursor(current) {
		return "", errors.New("TikTok pagination cursor did not advance")
	}
	return string(next), nil
}
func (r *commentRun) step(ctx context.Context) error {
	s := r.state
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Active == nil {
		if len(s.Videos) > 0 {
			s.Active = &videoState{Video: s.Videos[0]}
			s.Videos = s.Videos[1:]
			return nil
		}
		page, err := r.imp.api.Videos(ctx, r.account, s.Cursor)
		if err != nil {
			return err
		}
		cursor, err := nextCursor(s.Cursor, page.Cursor, page.HasMore)
		if err != nil {
			return err
		}
		for _, video := range page.Videos {
			if !ValidID(string(video.ID)) {
				return errors.New("TikTok video identity is missing")
			}
		}
		s.Videos, s.Cursor, s.Exhausted = page.Videos, cursor, !page.HasMore
		return nil
	}
	a := s.Active
	if !a.RootSaved {
		if err := r.persistVideo(ctx, a.Video); err != nil {
			return err
		}
		a.RootSaved = true
		return nil
	}
	if reply := a.Replies; reply != nil {
		if len(reply.Comments) > 0 {
			if err := r.persistComment(ctx, reply.Comments[0], string(a.Video.ID), reply.Parent); err != nil {
				return err
			}
			reply.Comments = reply.Comments[1:]
			return nil
		}
		if reply.Exhausted {
			a.Replies = nil
			return nil
		}
		return r.fetchComments(ctx, string(a.Video.ID), reply.Parent, &reply.Cursor, &reply.Exhausted, &reply.Comments)
	}
	if len(a.Comments) > 0 {
		comment := a.Comments[0]
		if err := r.persistComment(ctx, comment, string(a.Video.ID), ""); err != nil {
			return err
		}
		a.Comments = a.Comments[1:]
		if comment.Replies > 0 {
			a.Replies = &replyState{Parent: string(comment.ID)}
		}
		return nil
	}
	if a.Exhausted {
		s.Active = nil
		return nil
	}
	return r.fetchComments(ctx, string(a.Video.ID), "", &a.Cursor, &a.Exhausted, &a.Comments)
}
func (r *commentRun) fetchComments(ctx context.Context, video, parent string, cursor *string, exhausted *bool, pending *[]Comment) error {
	page, err := r.imp.api.Comments(ctx, r.account, video, parent, *cursor)
	if err != nil {
		return err
	}
	next, err := nextCursor(*cursor, page.Cursor, page.HasMore)
	if err != nil {
		return err
	}
	*cursor, *exhausted, *pending = next, !page.HasMore, page.Comments
	return nil
}
