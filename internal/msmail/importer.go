package msmail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/sync/errgroup"
)

// SourceType is the sources.source_type value for a Graph mail account.
const SourceType = "msmail"

// fetchWorkers is the number of parallel $value downloads. Microsoft documents
// four concurrent requests per mailbox as the limit.
const fetchWorkers = 4

// systemFolders maps Graph well-known folder names to the label system role
// they carry. Each one is labelled "system"; only Sent Items has a role.
var systemFolders = map[string]string{
	"inbox":        "",
	"sentitems":    store.LabelSystemRoleSent,
	"drafts":       "",
	"deleteditems": "",
	"junkemail":    "",
	"archive":      "",
}

// Options configures one sync of one mailbox.
type Options struct {
	Email          string
	AttachmentsDir string
	Progress       func(string)
}

// Summary reports what one sync did.
type Summary struct {
	SourceID int64
	Folders  int
	Added    int
	Moved    int
	Deleted  int
	Errors   int
	Duration time.Duration
}

// Import syncs every folder of the mailbox. A folder with no saved cursor is
// walked from the start. A folder with a cursor fetches only the changes since
// the last sync. Messages already in the vault are never downloaded again.
func Import(ctx context.Context, st *store.Store, c *Client, opts Options, log *slog.Logger) (sum *Summary, err error) {
	start := time.Now()
	src, err := st.GetOrCreateSource(SourceType, opts.Email)
	if err != nil {
		return nil, err
	}
	sum = &Summary{SourceID: src.ID}

	// Cursors: the last completed run, then any checkpoint of an interrupted
	// run after it. A delta link is opaque, so the newer checkpoint wins.
	cursors := map[string]string{}
	if prev, perr := st.GetLastSuccessfulSync(src.ID); perr == nil && prev != nil && prev.CursorAfter.Valid {
		mergeCursors(cursors, prev.CursorAfter.String)
	}
	if cp, cerr := st.GetLatestCheckpointedSync(src.ID); cerr == nil && cp != nil && cp.CursorBefore.Valid {
		mergeCursors(cursors, cp.CursorBefore.String)
	}

	syncID, err := st.StartSync(src.ID, SourceType)
	if err != nil {
		return nil, err
	}
	st = st.ScopedToSync(src.ID, syncID)
	checkpoint := func() *store.Checkpoint {
		blob, _ := json.Marshal(cursors, json.Deterministic(true))
		return &store.Checkpoint{
			PageToken:         string(blob),
			MessagesProcessed: int64(sum.Added + sum.Moved + sum.Deleted),
			MessagesAdded:     int64(sum.Added),
			ErrorsCount:       int64(sum.Errors),
		}
	}
	defer func() {
		if err != nil {
			_ = st.FailSyncWithCheckpoint(syncID, err.Error(), checkpoint())
		}
	}()

	s := &syncer{st: st, c: c, opts: opts, log: log, sourceID: src.ID, sum: sum}
	folders, err := c.ListFolders(ctx)
	if err != nil {
		return sum, fmt.Errorf("list mail folders: %w", err)
	}
	if s.labels, err = s.ensureLabels(ctx, folders); err != nil {
		return sum, err
	}

	for _, f := range folders {
		sum.Folders++
		s.progressf("Folder %s", f.Path)
		link := cursors[f.ID]
		if link == "" {
			link = DeltaStartURL(f.ID)
		}
		for {
			page, perr := c.DeltaPage(ctx, link)
			if errors.Is(perr, msgraph.ErrGone) {
				// The token expired. Walk the folder again; messages already
				// in the vault are not downloaded again.
				log.Info("delta token expired, walking folder again", "folder", f.Path)
				link = DeltaStartURL(f.ID)
				continue
			}
			if perr != nil {
				return sum, fmt.Errorf("folder %s: %w", f.Path, perr)
			}
			if err = s.applyPage(ctx, s.labels[f.ID], page.Value); err != nil {
				return sum, fmt.Errorf("folder %s: %w", f.Path, err)
			}
			if page.NextLink != "" {
				link = page.NextLink
			} else {
				link = page.DeltaLink
			}
			cursors[f.ID] = link
			if err = st.UpdateSyncCheckpoint(syncID, checkpoint()); err != nil {
				return sum, err
			}
			if page.NextLink == "" {
				break
			}
		}
	}

	if err = st.RecomputeConversationStats(src.ID); err != nil {
		return sum, err
	}
	cp := checkpoint()
	if err = st.CompleteSync(syncID, cp.PageToken); err != nil {
		return sum, err
	}
	sum.Duration = time.Since(start)
	return sum, nil
}

func mergeCursors(dst map[string]string, blob string) {
	var m map[string]string
	if json.Unmarshal([]byte(blob), &m) != nil {
		return
	}
	for k, v := range m {
		if v != "" {
			dst[k] = v
		}
	}
}

type syncer struct {
	st       *store.Store
	c        *Client
	opts     Options
	log      *slog.Logger
	sourceID int64
	sum      *Summary
	labels   map[string]int64 // Graph folder ID -> label ID

	// deletions is the hidden Recoverable Items folder. A permanent delete
	// (Shift+Delete, or emptying Deleted Items) moves a message there.
	deletions string
}

func (s *syncer) progressf(format string, args ...any) {
	if s.opts.Progress != nil {
		s.opts.Progress(fmt.Sprintf(format, args...))
	}
}

// ensureLabels makes one label per folder. The label's source ID is the Graph
// folder ID, so a folder rename changes only the label name.
func (s *syncer) ensureLabels(ctx context.Context, folders []Folder) (map[string]int64, error) {
	system := map[string]string{} // folder ID -> system role
	for name, role := range systemFolders {
		id, err := s.c.WellKnownFolderID(ctx, name)
		if errors.Is(err, msgraph.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("look up folder %s: %w", name, err)
		}
		system[id] = role
	}
	id, err := s.c.WellKnownFolderID(ctx, "recoverableitemsdeletions")
	if err != nil && !errors.Is(err, msgraph.ErrNotFound) {
		return nil, fmt.Errorf("look up folder recoverableitemsdeletions: %w", err)
	}
	s.deletions = id
	infos := make(map[string]store.LabelInfo, len(folders))
	for _, f := range folders {
		info := store.LabelInfo{Name: f.Path, Type: "user"}
		if role, ok := system[f.ID]; ok {
			info.Type, info.SystemRole = "system", role
		}
		infos[f.ID] = info
	}
	return s.st.EnsureLabelsBatch(s.sourceID, infos)
}

// applyPage stores one delta page for the folder that has label folderLabel.
// New messages are downloaded. Known messages get the folder as their only
// label, because a mail item is in exactly one folder. Removed messages are
// looked up: a message that Graph still finds in a mail folder moved, and one
// it cannot find, or finds in Recoverable Items, is marked deleted.
func (s *syncer) applyPage(ctx context.Context, folderLabel int64, items []DeltaMessage) error {
	var live []DeltaMessage
	var liveIDs, removedIDs []string
	for _, m := range items {
		if m.Removed != nil {
			removedIDs = append(removedIDs, m.ID)
			continue
		}
		live = append(live, m)
		liveIDs = append(liveIDs, m.ID)
	}

	known, err := s.st.MessageExistsBatch(s.sourceID, append(liveIDs, removedIDs...))
	if err != nil {
		return err
	}
	var todo []DeltaMessage
	for _, m := range live {
		if id, ok := known[m.ID]; ok {
			if err := s.setFolder(id, folderLabel); err != nil {
				return err
			}
			continue
		}
		todo = append(todo, m)
	}
	if err := s.download(ctx, folderLabel, todo); err != nil {
		return err
	}

	var gone []string
	for _, id := range removedIDs {
		msgID, ok := known[id]
		if !ok {
			continue
		}
		parent, err := s.c.ParentFolderID(ctx, id)
		if errors.Is(err, msgraph.ErrNotFound) || (err == nil && parent == s.deletions) {
			gone = append(gone, id)
			continue
		}
		if err != nil {
			return fmt.Errorf("look up removed message: %w", err)
		}
		// A folder this run did not list is picked up on the next sync.
		if label, ok := s.labels[parent]; ok {
			if err := s.setFolder(msgID, label); err != nil {
				return err
			}
		}
	}
	if len(gone) > 0 {
		if err := s.st.MarkMessagesDeletedBatch(s.sourceID, gone); err != nil {
			return err
		}
		s.sum.Deleted += len(gone)
	}
	return nil
}

func (s *syncer) setFolder(messageID, label int64) error {
	changed, err := s.st.ReconcileMessageLabels(messageID, []int64{label}, true)
	if changed {
		s.sum.Moved++
	}
	return err
}

type fetched struct {
	msg DeltaMessage
	raw []byte
}

// download fetches messages with fetchWorkers parallel requests and stores
// them one at a time. A message that disappears before its download is
// skipped. Any other download failure stops the sync, so the cursor does not
// move past a message that is not in the vault.
func (s *syncer) download(ctx context.Context, folderLabel int64, msgs []DeltaMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	jobs := make(chan DeltaMessage)
	results := make(chan fetched, fetchWorkers)
	g.Go(func() error {
		defer close(jobs)
		for _, m := range msgs {
			select {
			case jobs <- m:
			case <-gctx.Done():
				return gctx.Err()
			}
		}
		return nil
	})
	for range fetchWorkers {
		g.Go(func() error {
			for m := range jobs {
				raw, err := s.c.GetMIME(gctx, m.ID)
				if errors.Is(err, msgraph.ErrNotFound) {
					continue
				}
				if err != nil {
					return fmt.Errorf("download message: %w", err)
				}
				select {
				case results <- fetched{m, raw}:
				case <-gctx.Done():
					return gctx.Err()
				}
			}
			return nil
		})
	}
	var fetchErr error
	go func() {
		fetchErr = g.Wait()
		close(results)
	}()

	for r := range results {
		sum := sha256.Sum256(r.raw)
		if err := importer.IngestRawMessage(ctx, s.st, s.sourceID, s.opts.Email, s.opts.AttachmentsDir,
			[]int64{folderLabel}, r.msg.ID, hex.EncodeToString(sum[:]), r.raw, r.msg.ReceivedDateTime, s.log); err != nil {
			// ponytail: a message that fails to parse or store is counted and
			// skipped; it is retried only when its folder is walked again.
			s.log.Warn("store message", "id", r.msg.ID, "error", err)
			s.sum.Errors++
			continue
		}
		s.sum.Added++
	}
	if fetchErr != nil {
		return fmt.Errorf("download messages: %w", fetchErr)
	}
	return nil
}
