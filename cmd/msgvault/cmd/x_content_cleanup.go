package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// XContentCleanup coordinates durable X removal across local content readers.
type XContentCleanup struct {
	store                                   *store.Store
	cfg                                     *config.Config
	backend                                 vector.Backend
	server                                  *api.Server
	afterStore, afterVectors, beforePublish func() error
	beforeCacheLock                         func() error
}

// NewXContentCleanup constructs the local archive cleanup coordinator. Provider
// reads, credentials and spending are not involved in content removal.
func NewXContentCleanup(st *store.Store, cfg *config.Config, backend vector.Backend) *XContentCleanup {
	return &XContentCleanup{store: st, cfg: cfg, backend: backend}
}
func (c *XContentCleanup) SetServer(server *api.Server) { c.server = server }
func (c *XContentCleanup) lock(ctx context.Context, write bool) (func(), error) {
	if c == nil || c.store == nil || c.cfg == nil {
		return nil, errors.New("X cleanup unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(c.cfg.AnalyticsDir()), 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Clean(c.cfg.AnalyticsDir())+".x-content.lock", flock.SetPermissions(0600))
	var ok bool
	var err error
	if write {
		ok, err = lock.TryLockContext(ctx, 50*time.Millisecond)
	} else {
		ok, err = lock.TryRLockContext(ctx, 50*time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("X content fence not acquired")
	}
	return func() { _ = lock.Close() }, nil
}
func (c *XContentCleanup) AcquireRead(ctx context.Context) (func(), error) {
	release, err := c.lock(ctx, false)
	if err != nil {
		return nil, err
	}
	pending, err := c.store.XCleanupPendingContext(ctx)
	if err != nil || pending {
		release()
		if err == nil {
			err = errors.New("X cleanup pending")
		}
		return nil, err
	}
	return release, nil
}

// RemovePost owns the source fence for the complete local operation. It does
// not load credentials, consult a spending policy or construct a provider API.
func (c *XContentCleanup) RemovePost(ctx context.Context, sourceID int64, id, reason string) (work store.XRemovalWork, retErr error) {
	execution, err := c.store.AcquireXRemovalExecutionContext(ctx, sourceID, id)
	if err != nil {
		return work, err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	return c.removeXContents(ctx, store.XRemovalWork{SourceID: sourceID, CanonicalID: id, Reason: reason, Pending: true}, execution.Targets)
}

// removeXContent requires existing single-source ownership. A sync callback
// passes the full work returned by its ScopedToSync Store purge, retaining the
// generation fence. Duplicate copies defer to the owner coordinator AFTER the
// importer releases ownership; this path never takes nested source locks.
func (c *XContentCleanup) removeXContent(ctx context.Context, input store.XRemovalWork) (store.XRemovalWork, error) {
	targets, err := c.store.XRemovalTargetsContext(ctx, input.SourceID, input.CanonicalID)
	if err != nil {
		return input, err
	}
	for _, target := range targets {
		if target.SourceID != input.SourceID || target.CanonicalID != input.CanonicalID {
			return input, store.ErrXRemovalNeedsOwner
		}
	}
	return c.removeXContents(ctx, input, targets)
}

// removeXContents requires ownership of EVERY target source. The plan is
// rechecked after suppression commits; an expansion remains pending instead
// of finishing while an unlocked copy could survive.
func (c *XContentCleanup) removeXContents(ctx context.Context, input store.XRemovalWork, targets []store.XRemovalWork) (work store.XRemovalWork, retErr error) {
	release, err := c.lock(ctx, true)
	if err != nil {
		return work, err
	}
	defer release()
	if c.beforeCacheLock != nil {
		if err := c.beforeCacheLock(); err != nil {
			return work, err
		}
	}
	unlock, err := lockCacheAndInvalidateSyncState(c.cfg.AnalyticsDir())
	if err != nil {
		return work, err
	}
	defer func() { retErr = errors.Join(retErr, unlock()) }()
	// A builder that was already running may have published while we waited.
	// Observe its committed cache only after builders and readers are drained.
	_, statErr := os.Stat(c.cfg.AnalyticsDir())
	hadCache := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return work, statErr
	}
	reason := input.Reason
	if reason == "" {
		reason = "removed"
	}
	work = store.XRemovalWork{SourceID: input.SourceID, Reason: reason, Pending: true}
	var completed []store.XRemovalWork
	type targetKey struct {
		source    int64
		canonical string
	}
	owned := map[targetKey]bool{}
	for _, target := range targets {
		owned[targetKey{target.SourceID, target.CanonicalID}] = true
		local := input
		if input.CacheRevision == 0 || input.SourceID != target.SourceID || input.CanonicalID != target.CanonicalID {
			local, err = c.store.RemoveXPostContentContext(ctx, target.SourceID, target.CanonicalID, reason)
			if err != nil {
				return work, err
			}
		}
		completed = append(completed, local)
		if local.SourceID == input.SourceID && (work.CanonicalID == "" || local.CanonicalID == input.CanonicalID) {
			work.CanonicalID = local.CanonicalID
		}
		for _, id := range local.MessageIDs {
			if !slices.Contains(work.MessageIDs, id) {
				work.MessageIDs = append(work.MessageIDs, id)
			}
		}
		work.EvidenceIDs = append(work.EvidenceIDs, local.EvidenceIDs...)
		work.CacheRevision = max(work.CacheRevision, local.CacheRevision)
	}
	checkTargets := func() error {
		current, err := c.store.XRemovalTargetsContext(ctx, input.SourceID, input.CanonicalID)
		if err != nil {
			return err
		}
		for _, target := range current {
			if !owned[targetKey{target.SourceID, target.CanonicalID}] {
				return store.ErrXRemovalNeedsOwner
			}
		}
		return nil
	}
	if err := checkTargets(); err != nil {
		return work, err
	}
	slices.Sort(work.MessageIDs)
	if c.afterStore != nil {
		if err := c.afterStore(); err != nil {
			return work, err
		}
	}
	backend, closeBackend, err := c.cleanupBackend(ctx)
	if err != nil {
		return work, err
	}
	if closeBackend != nil {
		defer closeBackend()
	}
	if backend != nil {
		lister, ok := backend.(interface {
			GenerationIDs(context.Context) ([]vector.GenerationID, error)
		})
		if !ok {
			return work, errors.New("vector backend cannot enumerate cleanup generations")
		}
		generations, err := lister.GenerationIDs(ctx)
		if err != nil {
			return work, err
		}
		for _, gen := range generations {
			if err := backend.Delete(ctx, gen, work.MessageIDs); err != nil {
				return work, fmt.Errorf("delete X vectors generation %d: %w", gen, err)
			}
		}
	}
	if c.afterVectors != nil {
		if err := c.afterVectors(); err != nil {
			return work, err
		}
	}
	if c.server != nil {
		c.server.InvalidateMessageContent(work.MessageIDs)
		mode := api.AnalyticsModeSQLFallback
		if c.store.IsPostgreSQL() {
			mode = api.AnalyticsModePostgres
		}
		if err := c.server.RetireContentAnalytics(query.NewEngine(c.store.DB(), c.store.IsPostgreSQL()), mode); err != nil {
			return work, err
		}
	}
	// Builder ownership has drained real builders, so stale staging is removable
	// even when its PID happens to remain alive in this daemon process.
	parent := filepath.Dir(c.cfg.AnalyticsDir())
	entries, err := os.ReadDir(parent)
	if err != nil {
		return work, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), cacheStagingPrefix(c.cfg.AnalyticsDir())) {
			if !entry.IsDir() {
				return work, errors.New("unsafe analytics staging artifact")
			}
			if err := os.RemoveAll(filepath.Join(parent, entry.Name())); err != nil {
				return work, err
			}
		}
	}
	if err := os.RemoveAll(c.cfg.AnalyticsDir()); err != nil {
		return work, err
	}
	if c.beforePublish != nil {
		if err := c.beforePublish(); err != nil {
			return work, err
		}
	}
	if hadCache && !c.store.IsPostgreSQL() {
		if _, err := buildCacheLocked(c.cfg.DatabaseDSN(), c.cfg.AnalyticsDir(), true, false, publishLockHeld); err != nil {
			return work, err
		}
	}
	// A scoped importer can learn a new edit link during derived I/O. Leave
	// pending work intact and retry with the expanded source group. Links
	// learned after this check queue their own durable pending obligation.
	if err := checkTargets(); err != nil {
		return work, err
	}
	for _, local := range completed {
		local.DerivedComplete = true
		if err := c.store.FinishXRemovalContext(ctx, local.SourceID, local); err != nil {
			return work, err
		}
	}
	work.DerivedComplete = true
	work.Pending = false
	return work, nil
}

func (c *XContentCleanup) cleanupBackend(ctx context.Context) (vector.Backend, func(), error) {
	if c.backend != nil {
		return c.backend, nil, nil
	}
	exists := false
	if c.store.IsPostgreSQL() {
		var err error
		exists, err = c.store.XVectorIndexExistsContext(ctx)
		if err != nil {
			return nil, nil, err
		}
	} else {
		path := c.cfg.Vector.DBPath
		if path == "" {
			path = filepath.Join(c.cfg.Data.DataDir, "vectors.db")
		}
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		exists = true
	}
	if !exists {
		return nil, nil, nil
	}
	inv := newInvocation()
	inv.cfg = c.cfg
	return openEmbeddingsBackendWithMode(withInvocation(ctx, inv), false)
}

// Resume finishes pending suppression independently of schedules/budget/auth.
// Call before any daemon readers, vector workers or cache publishers start.
func (c *XContentCleanup) Resume(ctx context.Context) error {
	for {
		pending, err := c.store.PendingXRemovalsContext(ctx, 0)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		work := pending[0]
		if _, err := c.RemovePost(ctx, work.SourceID, work.CanonicalID, work.Reason); err != nil {
			return err
		}
	}
}

// ResumeAtStartup requires daemon profile ownership and no exposed readers or
// opened query engines. It drains crash-left spill files before completing
// durable work, including a stale directory whose PID was reused. Generic
// Resume (for imports or isolated restore) does not touch the host spill tree.
func (c *XContentCleanup) ResumeAtStartup(ctx context.Context) error {
	pending, err := c.store.XCleanupPendingContext(ctx)
	if err != nil {
		return err
	}
	if pending {
		owned, err := query.PrepareDaemonSpillDir(c.cfg.HomeDir)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(owned); err != nil {
			return fmt.Errorf("remove interrupted X query spills: %w", err)
		}
	}
	return c.Resume(ctx)
}

// DuringSync finishes queued work under the importer's existing source
// execution. Cross-source work stays pending until Resume after that execution
// is released; this method never nests source execution locks.
func (c *XContentCleanup) DuringSync(ctx context.Context, work store.XRemovalWork) error {
	_, err := c.removeXContent(ctx, work)
	return err
}
