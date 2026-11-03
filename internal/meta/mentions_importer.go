package meta

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
)

type MentionAPI interface {
	VerifyPage(ctx context.Context, pageID string) (Page, error)
	LinkedInstagramAccount(ctx context.Context, pageID, accountID string) (IGUser, error)
	Tags(ctx context.Context, ig, after string) (MediaPage, error)
	MentionedMedia(ctx context.Context, ig, mediaID string) (IGMentionMedia, error)
	MentionedComment(ctx context.Context, ig, commentID string) (IGMentionComment, error)
}

type MentionImporter struct {
	store *store.Store
	api   MentionAPI
}

type MentionEvent struct{ MediaID, CommentID string }

func NewMentionImporter(st *store.Store, api MentionAPI) *MentionImporter {
	return &MentionImporter{store: st, api: api}
}

func (imp *MentionImporter) Register(ctx context.Context, app, page, ig string) (*store.Source, error) {
	if !ValidAccountID(app) || !ValidAccountID(page) || !ValidAccountID(ig) {
		return nil, errors.New("invalid Instagram mentions source identity")
	}
	if _, err := imp.api.VerifyPage(ctx, page); err != nil {
		return nil, err
	}
	account, err := imp.api.LinkedInstagramAccount(ctx, page, ig)
	if err != nil {
		return nil, err
	}
	src, err := imp.store.GetOrCreateSourceContext(ctx, MentionsSourceType, InstagramSourceIdentifier(app, page, ig))
	if err != nil {
		return nil, err
	}
	lock, err := imp.store.AcquireSyncExecutionContext(ctx, src.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()
	src, err = imp.store.GetSourceByIDContext(ctx, src.ID)
	if err != nil {
		return nil, err
	}
	if src.HistoryOnly || src.MergedIntoSourceID != 0 {
		return nil, store.ErrSourceRetired
	}
	if err = imp.store.AddAccountIdentityContext(ctx, src.ID, instagramActorIdentifier(app, page, ig, "ig_user", ig), "instagram_page_token"); err != nil {
		return nil, err
	}
	if err = imp.store.UpdateSourceDisplayNameContext(ctx, src.ID, account.Username); err != nil {
		return nil, err
	}
	return imp.store.GetSourceByIDContext(ctx, src.ID)
}

func (imp *MentionImporter) ImportTags(ctx context.Context, app, page, ig string, opts ImportOptions) (*ImportSummary, error) {
	return imp.run(ctx, app, page, ig, opts, nil)
}

func (imp *MentionImporter) Resolve(ctx context.Context, app, page, ig string, ev MentionEvent) (*ImportSummary, error) {
	if !ValidAccountID(ev.MediaID) || (ev.CommentID != "" && !ValidAccountID(ev.CommentID)) {
		return nil, errors.New("invalid Instagram mention event identity")
	}
	return imp.run(ctx, app, page, ig, ImportOptions{}, &ev)
}

func (imp *MentionImporter) run(ctx context.Context, app, page, ig string, opts ImportOptions, ev *MentionEvent) (sum *ImportSummary, retErr error) {
	if !ValidAccountID(app) || !ValidAccountID(page) || !ValidAccountID(ig) || opts.MaxRequests < 0 {
		return nil, errors.New("invalid Instagram mentions import options")
	}
	src, err := imp.store.GetSourceByTypeAndIdentifier(MentionsSourceType, InstagramSourceIdentifier(app, page, ig))
	if err != nil {
		return nil, err
	}
	lock, err := imp.store.AcquireSyncExecutionContext(ctx, src.ID)
	if err != nil {
		return nil, fmt.Errorf("instagram mentions source: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, lock.Release()) }()
	src, err = imp.store.GetSourceByIDContext(ctx, src.ID)
	if err != nil {
		return nil, err
	}
	if src.HistoryOnly || src.MergedIntoSourceID != 0 {
		return nil, store.ErrSourceRetired
	}
	var state *mentionSyncState
	kind := MentionsSourceType
	if ev == nil {
		state, err = loadMentionState(ctx, imp.store, src, opts.Full)
		if err != nil {
			return nil, err
		}
	} else {
		kind = MentionsSourceType + "_webhook"
	}
	id, err := lock.StartSyncContext(ctx, kind, "")
	if err != nil {
		return nil, err
	}
	st := imp.store.ScopedToSync(src.ID, id)
	sum = &ImportSummary{}
	defer func() {
		sum.Partial = retErr != nil || (state != nil && !state.Done)
		cp, err := state.checkpoint(sum)
		if err != nil {
			retErr = errors.Join(retErr, err)
			return
		}
		switch {
		case retErr != nil:
			if jobctx.ErrorAfterYield(ctx, retErr) != nil {
				cp.ErrorsCount = 1
			}
			err = imp.store.InterruptSyncWithCheckpoint(ctx, id, retErr, cp)
		case sum.Partial:
			err = imp.store.PauseSyncWithCheckpoint(id, cp)
		case ev != nil:
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			err = st.UpdateSyncCheckpointContext(cleanup, id, cp)
			if err != nil || ctx.Err() != nil {
				err = errors.Join(err, ctx.Err(), st.FailSyncContext(cleanup, id, "webhook processing interrupted"))
			} else {
				err = st.CompleteSyncContext(cleanup, id, "")
			}
		default:
			err = st.CompleteSyncAndUpdateSourceCursorContext(ctx, id, src.ID, cp.PageToken)
		}
		retErr = errors.Join(retErr, err)
	}()
	cp, err := state.checkpoint(sum)
	if err != nil {
		return sum, err
	}
	if err = st.UpdateSyncCheckpointContext(ctx, id, cp); err != nil {
		return sum, err
	}
	maximum := opts.MaxRequests
	if maximum == 0 {
		maximum = DefaultMaxRequests
	}
	for _, read := range []func() error{
		func() error { _, err := imp.api.VerifyPage(ctx, page); return err },
		func() error { _, err := imp.api.LinkedInstagramAccount(ctx, page, ig); return err },
	} {
		if sum.Requests >= maximum {
			return sum, nil
		}
		sum.Requests++
		if err = read(); err != nil {
			return sum, err
		}
	}
	scope := mentionScope{SourceID: src.ID, App: app, Page: page, IG: ig, Summary: sum}
	if ev != nil {
		sum.Requests++
		var media IGMentionMedia
		var comment *IGMentionComment
		if ev.CommentID != "" {
			c, readErr := imp.api.MentionedComment(ctx, ig, ev.CommentID)
			if readErr != nil {
				return sum, readErr
			}
			if c.Media == nil || c.Media.ID != ev.MediaID {
				return sum, errors.New("instagram mention media identity mismatch")
			}
			media, comment = *c.Media, &c
		} else {
			media, err = imp.api.MentionedMedia(ctx, ig, ev.MediaID)
			if err != nil {
				return sum, err
			}
		}
		return sum, archiveMention(ctx, st, scope, media, comment, surfaceCaptionMention)
	}
	for !state.Done && sum.Requests < maximum {
		sum.Requests++
		p, err := imp.api.Tags(ctx, ig, state.After)
		if err != nil {
			return sum, err
		}
		for _, media := range p.Data {
			if err = archiveMention(ctx, st, scope, media, nil, surfaceTagged); err != nil {
				return sum, err
			}
		}
		state.After, state.Done = p.After, p.After == ""
		cp, err := state.checkpoint(sum)
		if err != nil {
			return sum, err
		}
		if err = st.UpdateSyncCheckpointContext(ctx, id, cp); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

func archiveMention(ctx context.Context, st *store.Store, scope mentionScope, media IGMentionMedia, comment *IGMentionComment, surface mentionSurface) error {
	if media.Owner != nil && media.Owner.ID == scope.IG {
		return nil
	}
	var err error
	if comment != nil {
		_, err = persistMentionComment(ctx, st, scope, *comment)
	} else {
		_, err = persistMentionMedia(ctx, st, scope, media, surface)
	}
	return err
}
