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
	Updated  int
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
			MessagesProcessed: int64(sum.Added + sum.Updated + sum.Moved + sum.Deleted),
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
		// seen collects the IDs of a walk that starts in this run, so that
		// archived messages it does not return can be looked up at its end.
		var seen map[string]bool
		if link == "" {
			link, seen = DeltaStartURL(f.ID), map[string]bool{}
		}
		restarted := false
		for {
			page, perr := c.DeltaPage(ctx, link)
			if errors.Is(perr, msgraph.ErrGone) && !restarted {
				// The token expired. Walk the folder again; messages already
				// in the vault are not downloaded again.
				log.Info("delta token expired, walking folder again", "folder", f.Path)
				link, seen, restarted = DeltaStartURL(f.ID), map[string]bool{}, true
				continue
			}
			if perr != nil {
				return sum, fmt.Errorf("folder %s: %w", f.Path, perr)
			}
			if err = s.applyPage(ctx, f.ID, page.Value, seen); err != nil {
				return sum, fmt.Errorf("folder %s: %w", f.Path, err)
			}
			if page.NextLink == "" && seen != nil {
				if err = s.reconcileWalk(ctx, s.labels[f.ID], seen); err != nil {
					return sum, fmt.Errorf("folder %s: %w", f.Path, err)
				}
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

	// drafts is the Drafts folder. A draft keeps its ID while it is edited,
	// so a known draft is downloaded again when delta reports it.
	drafts string

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
		if name == "drafts" {
			s.drafts = id
		}
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

// applyPage stores one delta page for a folder. New messages are downloaded.
// Known messages get the folder as their only label, because a mail item is
// in exactly one folder, and lose any deletion mark, because the mailbox has
// them again. In an incremental round (seen is nil), known messages are also
// downloaded again, because delta reports them only when they changed. A walk
// (seen is not nil) returns every message, so it downloads again only drafts,
// whose content can change under the same ID. Removed messages are looked up
// with relocate. A walk collects the IDs of live messages in seen.
func (s *syncer) applyPage(ctx context.Context, folderID string, items []DeltaMessage, seen map[string]bool) error {
	folderLabel := s.labels[folderID]
	var live []DeltaMessage
	var liveIDs, removedIDs []string
	for _, m := range items {
		if m.Removed != nil {
			removedIDs = append(removedIDs, m.ID)
			continue
		}
		live = append(live, m)
		liveIDs = append(liveIDs, m.ID)
		if seen != nil {
			seen[m.ID] = true
		}
	}

	known, err := s.st.MessageExistsBatch(s.sourceID, append(liveIDs, removedIDs...))
	if err != nil {
		return err
	}
	var todo []DeltaMessage
	for _, m := range live {
		id, ok := known[m.ID]
		if ok {
			if err := s.st.ClearMessageDeletedFromSource(s.sourceID, m.ID); err != nil {
				return err
			}
		}
		if !ok {
			todo = append(todo, m)
			continue
		}
		if err := s.setFolder(id, folderLabel); err != nil {
			return err
		}
		if seen == nil || (s.drafts != "" && folderID == s.drafts) {
			m.archiveID = id
			todo = append(todo, m)
		}
	}
	if err := s.download(ctx, folderLabel, todo); err != nil {
		return err
	}

	removed := map[string]int64{}
	for _, id := range removedIDs {
		if msgID, ok := known[id]; ok {
			removed[id] = msgID
		}
	}
	return s.relocate(ctx, removed)
}

// reconcileWalk looks up the archived messages of a folder that a complete
// walk did not return. They left the folder while no delta cursor covered it,
// for example after the cursor expired.
func (s *syncer) reconcileWalk(ctx context.Context, folderLabel int64, seen map[string]bool) error {
	rows, err := s.st.DB().QueryContext(ctx, s.st.Rebind(`
		SELECT m.source_message_id, m.id FROM messages m
		JOIN message_labels ml ON ml.message_id = m.id
		WHERE m.source_id = ? AND ml.label_id = ? AND m.deleted_from_source_at IS NULL`),
		s.sourceID, folderLabel)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	missing := map[string]int64{}
	for rows.Next() {
		var sourceMsgID string
		var id int64
		if err := rows.Scan(&sourceMsgID, &id); err != nil {
			return err
		}
		if !seen[sourceMsgID] {
			missing[sourceMsgID] = id
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return s.relocate(ctx, missing)
}

// relocate finds where known messages went: source message ID -> message ID.
// A message that Graph still finds in a mail folder moved, and one it cannot
// find, or finds in Recoverable Items, is marked deleted.
func (s *syncer) relocate(ctx context.Context, msgs map[string]int64) error {
	var gone []string
	for id, msgID := range msgs {
		parent, err := s.c.ParentFolderID(ctx, id)
		if errors.Is(err, msgraph.ErrNotFound) || (err == nil && s.deletions != "" && parent == s.deletions) {
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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

	// A message that fails to store stops the page, so the cursor does not
	// move past it; the next sync retries the page. Results are drained so
	// the workers can exit.
	var storeErr error
	for r := range results {
		if storeErr != nil {
			continue
		}
		if r.msg.archiveID != 0 {
			// The new MIME replaces the old one, so drop the attachment rows
			// of the old MIME first.
			if err := s.st.DeleteKeyedAttachmentsExceptContext(ctx, r.msg.archiveID, "mime:", ""); err != nil {
				storeErr = fmt.Errorf("clear attachments of message %s: %w", r.msg.ID, err)
				cancel()
				continue
			}
		}
		sum := sha256.Sum256(r.raw)
		if err := importer.IngestRawMessage(ctx, s.st, s.sourceID, s.opts.Email, s.opts.AttachmentsDir,
			[]int64{folderLabel}, r.msg.ID, hex.EncodeToString(sum[:]), r.raw, r.msg.ReceivedDateTime, s.log); err != nil {
			storeErr = fmt.Errorf("store message %s: %w", r.msg.ID, err)
			s.sum.Errors++
			cancel()
			continue
		}
		if r.msg.archiveID == 0 {
			s.sum.Added++
			continue
		}
		if err := s.st.RecomputeMessageAttachmentStats(r.msg.archiveID); err != nil {
			storeErr = fmt.Errorf("update attachment stats of message %s: %w", r.msg.ID, err)
			cancel()
			continue
		}
		s.sum.Updated++
	}
	if storeErr != nil {
		return storeErr
	}
	if fetchErr != nil {
		return fmt.Errorf("download messages: %w", fetchErr)
	}
	return nil
}
