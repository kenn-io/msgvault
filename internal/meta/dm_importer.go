package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
)

type DMImporter struct {
	store *store.Store
	api   DMAPI
	now   func() time.Time
}

func NewDMImporter(st *store.Store, api DMAPI) *DMImporter {
	return &DMImporter{store: st, api: api, now: time.Now}
}

// Register checks existing retirement under the execution lock before any
// provider read. A new source is created only after identity verification.
func (imp *DMImporter) Register(ctx context.Context, src DMSource) (out *store.Source, retErr error) {
	parsed, ok := ParseDMSourceIdentifier(src.Platform, src.Identifier())
	if !ok || parsed != src {
		return nil, errors.New("invalid inbox source identity")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	if err != nil && !errors.Is(err, store.ErrSourceNotFound) {
		return nil, err
	}
	var execution *store.SyncExecution
	lock := func() error {
		var err error
		execution, err = imp.store.AcquireSyncExecutionContext(ctx, source.ID)
		if err != nil {
			return err
		}
		current, err := imp.store.GetSourceByIDContext(ctx, source.ID)
		if err != nil {
			return err
		}
		if current.HistoryOnly || current.MergedIntoSourceID != 0 {
			return store.ErrSourceRetired
		}
		return nil
	}
	defer func() {
		if execution != nil {
			retErr = errors.Join(retErr, execution.Release())
		}
	}()
	if source != nil {
		if err := lock(); err != nil {
			return nil, err
		}
	}
	page, err := imp.api.VerifyPage(ctx, src.PageID)
	if err != nil {
		return nil, err
	}
	name, owner := page.Name, src.PageID
	if src.Platform == PlatformInstagram {
		ig, err := imp.api.LinkedInstagramAccount(ctx, src.PageID, src.IGID)
		if err != nil {
			return nil, err
		}
		name, owner = ig.Username, src.IGID
	}
	if source == nil {
		source, err = imp.store.GetOrCreateSourceContext(ctx, src.Platform.SourceType(), src.Identifier())
		if err != nil {
			return nil, err
		}
		if err = lock(); err != nil {
			return nil, err
		}
	}
	if err = imp.store.AddAccountIdentityContext(ctx, source.ID, DMActorIdentifier(src, owner), "facebook_page_token"); err != nil {
		return nil, err
	}
	if err = imp.store.UpdateSourceDisplayNameContext(ctx, source.ID, name); err != nil {
		return nil, err
	}
	return imp.store.GetSourceByIDContext(ctx, source.ID)
}

// Import backfills one bounded conversation page at a time. Pending preserves
// the rest of that page when the request budget expires inside a thread.
func (imp *DMImporter) Import(ctx context.Context, src DMSource, opts ImportOptions) (sum *DMImportSummary, retErr error) {
	parsed, ok := ParseDMSourceIdentifier(src.Platform, src.Identifier())
	if !ok || parsed != src || opts.MaxRequests < 0 {
		return nil, errors.New("invalid inbox import options")
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
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
	state, err := loadDMState(ctx, imp.store, source, opts.Full, imp.now())
	if err != nil {
		return nil, err
	}
	syncID, err := execution.StartSyncContext(ctx, src.Platform.SourceType(), "")
	if err != nil {
		return nil, err
	}
	scoped := imp.store.ScopedToSync(source.ID, syncID)
	writer := &dmWriter{store: scoped, sourceID: source.ID, src: src}
	sum = &DMImportSummary{Note: "backfill covers at most the 20 most recent messages per thread"}
	defer func() {
		sum.Partial = retErr != nil || !state.complete()
		cp, err := dmCheckpoint(state, sum)
		if err != nil {
			retErr = errors.Join(retErr, err)
			return
		}
		if retErr != nil {
			if jobctx.ErrorAfterYield(ctx, retErr) != nil {
				cp.ErrorsCount = 1
			}
			err = imp.store.InterruptSyncWithCheckpoint(ctx, syncID, retErr, cp)
		} else if sum.Partial {
			err = imp.store.PauseSyncWithCheckpoint(syncID, cp)
		} else {
			err = scoped.CompleteSyncAndUpdateSourceCursorContext(ctx, syncID, source.ID, cp.PageToken)
		}
		retErr = errors.Join(retErr, err)
	}()
	maximum := opts.MaxRequests
	if maximum == 0 {
		maximum = DefaultMaxRequests
	}
	read := func(f func() error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sum.Requests >= maximum {
			return errRequestBudget
		}
		sum.Requests++
		err := f()
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.Permission() || apiErr.Status == 403) {
			set := "pages_messaging, pages_manage_metadata, pages_read_engagement"
			if src.Platform == PlatformInstagram {
				set = "instagram_basic, instagram_manage_messages, pages_manage_metadata"
			}
			return fmt.Errorf("permission_missing: needs %s: %w", set, err)
		}
		return err
	}
	save := func() error { return saveDMState(ctx, scoped, syncID, state, sum) }
	if err = save(); err != nil {
		return sum, err
	}
	err = read(func() error { _, e := imp.api.VerifyPage(ctx, src.PageID); return e })
	if errors.Is(err, errRequestBudget) {
		return sum, nil
	}
	if err != nil {
		return sum, err
	}
	if src.Platform == PlatformInstagram {
		err = read(func() error { _, e := imp.api.LinkedInstagramAccount(ctx, src.PageID, src.IGID); return e })
		if errors.Is(err, errRequestBudget) {
			return sum, nil
		}
		if err != nil {
			return sum, err
		}
	}
	for !state.complete() {
		if state.Frame == nil {
			if len(state.Pending) == 0 {
				var page ConversationPage
				after := state.After
				err = read(func() error {
					var e error
					page, e = imp.api.Conversations(ctx, src.PageID, src.Platform.GraphPlatform(), after)
					return e
				})
				if errors.Is(err, errRequestBudget) {
					return sum, nil
				}
				if err != nil {
					var apiErr *APIError
					if after != "" && errors.As(err, &apiErr) && apiErr.Status == 400 && !apiErr.Permission() {
						err = fmt.Errorf("facebook continuation request failed: %w", err)
					}
					return sum, err
				}
				state.Pending, state.After, state.Exhausted = page.Data, page.After, page.After == ""
				if !state.valid(source) {
					return sum, errors.New("invalid inbox conversation page")
				}
				if err = save(); err != nil {
					return sum, err
				}
				if len(state.Pending) == 0 {
					continue
				}
			}
			conv := state.Pending[0]
			updated, _ := parseDMTime(conv.UpdatedTime)
			high, _ := parseDMTime(state.HighWater)
			// Revisit the boundary: separate arrivals can share timestamp precision.
			if !updated.IsZero() && !high.IsZero() && updated.Before(high) {
				state.Pending = state.Pending[1:]
				if err = save(); err != nil {
					return sum, err
				}
				continue
			}
			f := &dmFrame{ThreadID: conv.ID}
			err = read(func() error { var e error; f.IDs, f.Capped, e = imp.api.ConversationMessageIDs(ctx, conv.ID); return e })
			if errors.Is(err, errRequestBudget) {
				return sum, nil
			}
			if err != nil {
				return sum, err
			}
			state.Frame = f
			sum.Threads++
			if !state.valid(source) {
				return sum, errors.New("invalid inbox message frame")
			}
			if err = save(); err != nil {
				return sum, err
			}
		}
		frame, conv := state.Frame, state.Pending[0]
		for frame.Next < len(frame.IDs) {
			mid := frame.IDs[frame.Next]
			var id int64
			added := false
			var node DMMessage
			graphObservedAt := imp.now().UTC().UnixMilli()
			err = read(func() error { var e error; node, e = imp.api.Message(ctx, mid); return e })
			if errors.Is(err, errRequestBudget) {
				return sum, nil
			}
			if err != nil {
				var apiErr *APIError
				// Only Messenger's explicit deleted-message error permits a skip.
				// https://developers.facebook.com/docs/messenger-platform/error-codes/
				if !errors.As(err, &apiErr) || apiErr.Status != 400 || apiErr.Code != 9000001 {
					return sum, err
				}
				sum.MessagesSkipped++
			} else {
				rec, e := recordFromNode(src, node)
				if e != nil {
					return sum, e
				}
				rec.GraphObservedAt = graphObservedAt
				rec.ThreadID = frame.ThreadID
				for _, p := range conv.Participants {
					if p.ID == dmOwner(src) {
						continue
					}
					if rec.FromMe && rec.CustomerID == "" {
						rec.CustomerID = p.ID
					}
					if rec.CustomerID == p.ID && rec.CustomerName == "" {
						rec.CustomerName = p.Name
						if rec.CustomerName == "" {
							rec.CustomerName = p.Username
						}
					}
				}
				id, added, err = writer.persist(ctx, rec)
				if err != nil {
					return sum, err
				}
			}
			if frame.Capped && id != 0 {
				if err = markDMBackfillCapped(ctx, scoped, id); err != nil {
					return sum, err
				}
			}
			if added {
				sum.MessagesAdded++
			}
			sum.MessagesProcessed++
			frame.Next++
			if err = save(); err != nil {
				return sum, err
			}
		}
		updated, err := parseDMTime(conv.UpdatedTime)
		if err != nil {
			return sum, errors.New("invalid conversation update time")
		}
		// Later pages may be fetched after a budget pause. Keep an inclusive
		// second-precision boundary so arrivals in earlier threads during the
		// scan's starting second remain eligible on the next scan.
		boundary, _ := parseDMTime(state.ScanStarted)
		boundary = boundary.Truncate(time.Second)
		if updated.After(boundary) {
			updated = boundary
		}
		newHigh, _ := parseDMTime(state.NewHigh)
		if updated.After(newHigh) {
			state.NewHigh = updated.UTC().Format(time.RFC3339Nano)
		}
		state.Frame = nil
		state.Pending = state.Pending[1:]
		if err = save(); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

func markDMBackfillCapped(ctx context.Context, st *store.Store, messageID int64) error {
	ref, err := st.MessageConversation(messageID)
	if err != nil {
		return err
	}
	blob, err := st.GetConversationMetadataContext(ctx, ref.ConversationID)
	if err != nil {
		return err
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(blob.String), &metadata) != nil {
		return errors.New("invalid inbox conversation metadata")
	}
	metadata["backfill_capped"] = true
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return st.SetConversationMetadataContext(ctx, ref.ConversationID, sql.NullString{Valid: true, String: string(data)})
}
