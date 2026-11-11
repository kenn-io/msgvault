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
	beforeAnalyticsRetire                   func() error
	analyticsReopen                         bool
	analyticsGeneration                     uint64
}

// NewXContentCleanup constructs the local archive cleanup coordinator. Provider
// reads, credentials and spending are not involved in content removal.
func NewXContentCleanup(st *store.Store, cfg *config.Config, backend vector.Backend) *XContentCleanup {
	return &XContentCleanup{store: st, cfg: cfg, backend: backend}
}
func (c *XContentCleanup) SetServer(server *api.Server) { c.server = server }
func (c *XContentCleanup) lock(ctx context.Context, write bool) (func(), error) {
	if c == nil || c.store == nil || c.cfg == nil {
		return nil, errors.New("x cleanup unavailable")
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
		return nil, fmt.Errorf("acquire X content read lock: %w", err)
	}
	if !ok {
		return nil, errors.New("x content fence not acquired")
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
			err = errors.New("x cleanup pending")
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
func (c *XContentCleanup) removeXContent(ctx context.Context, input store.XRemovalWork) error {
	targets, err := c.store.XRemovalTargetsContext(ctx, input.SourceID, input.CanonicalID)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if target.SourceID != input.SourceID || target.CanonicalID != input.CanonicalID {
			return store.ErrXRemovalNeedsOwner
		}
	}
	_, err = c.removeXContentsBatch(ctx, []store.XRemovalGroup{{Root: input, Targets: targets}})
	return err
}

// removeXContentBatch finishes several source-local removals under one
// analytics/vector publication fence. A sync callback may use it only when
// every connected target belongs to its already-owned source.
func (c *XContentCleanup) removeXContentBatch(ctx context.Context, inputs []store.XRemovalWork) error {
	groups := make([]store.XRemovalGroup, 0, len(inputs))
	for _, input := range inputs {
		targets, err := c.store.XRemovalTargetsContext(ctx, input.SourceID, input.CanonicalID)
		if err != nil {
			return err
		}
		for _, target := range targets {
			if target.SourceID != input.SourceID || target.CanonicalID != input.CanonicalID {
				return store.ErrXRemovalNeedsOwner
			}
		}
		groups = append(groups, store.XRemovalGroup{Root: input, Targets: targets})
	}
	_, err := c.removeXContentsBatch(ctx, groups)
	return err
}

// removeXContents requires ownership of EVERY target source. The plan is
// rechecked after suppression commits; an expansion remains pending instead
// of finishing while an unlocked copy could survive.
func (c *XContentCleanup) removeXContents(ctx context.Context, input store.XRemovalWork, targets []store.XRemovalWork) (store.XRemovalWork, error) {
	return c.removeXContentsBatch(ctx, []store.XRemovalGroup{{Root: input, Targets: targets}})
}

// removeXContentsBatch owns the content/cache publication fence once for all
// pending groups. It does not acquire source execution; its caller must hold
// every source named by every group's target set.
func (c *XContentCleanup) removeXContentsBatch(ctx context.Context, groups []store.XRemovalGroup, profiles ...store.XAuthorProfileRemovalWork) (work store.XRemovalWork, retErr error) {
	if len(groups) == 0 && len(profiles) == 0 {
		return work, errors.New("x removal batch is empty")
	}
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
	cacheLockHeld := true
	defer func() {
		if cacheLockHeld {
			retErr = errors.Join(retErr, unlock())
		}
	}()
	// A builder that was already running may have published while we waited.
	// Observe its committed cache only after builders and readers are drained.
	_, statErr := os.Stat(c.cfg.AnalyticsDir())
	hadCache := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return work, statErr
	}
	var root store.XRemovalWork
	if len(groups) > 0 {
		root = groups[0].Root
	} else {
		root.SourceID, root.Reason = profiles[0].SourceID, "protected"
	}
	rootReason := root.Reason
	if rootReason == "" {
		rootReason = "removed"
	}
	work = store.XRemovalWork{SourceID: root.SourceID, CanonicalID: root.CanonicalID, Reason: rootReason, Pending: true}
	type targetKey struct {
		source    int64
		canonical string
	}
	owned := make(map[targetKey]bool)
	targetsByKey := make(map[targetKey]store.XRemovalWork)
	defaultReason := make(map[targetKey]string)
	explicitReason := make(map[targetKey]string)
	inputByKey := make(map[targetKey]store.XRemovalWork)
	for _, group := range groups {
		reason := group.Root.Reason
		if reason == "" {
			reason = "removed"
		}
		rootKey := targetKey{group.Root.SourceID, group.Root.CanonicalID}
		explicitReason[rootKey] = reason
		if group.Root.CacheRevision != 0 {
			inputByKey[rootKey] = group.Root
		}
		for _, target := range group.Targets {
			key := targetKey{target.SourceID, target.CanonicalID}
			owned[key] = true
			targetsByKey[key] = target
			if defaultReason[key] == "" {
				defaultReason[key] = reason
			}
		}
	}
	keys := make([]targetKey, 0, len(targetsByKey))
	for key := range targetsByKey {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b targetKey) int {
		if a.source < b.source {
			return -1
		}
		if a.source > b.source {
			return 1
		}
		return strings.Compare(a.canonical, b.canonical)
	})
	completed := make(map[targetKey]store.XRemovalWork, len(keys))
	for _, key := range keys {
		local, ok := inputByKey[key]
		if !ok || local.CacheRevision == 0 {
			reason := explicitReason[key]
			if reason == "" {
				reason = defaultReason[key]
			}
			if reason == "" {
				reason = rootReason
			}
			local, err = c.store.RemoveXPostContentContext(ctx, key.source, key.canonical, reason)
			if err != nil {
				return work, err
			}
		}
		completed[key] = local
		for _, id := range local.MessageIDs {
			if !slices.Contains(work.MessageIDs, id) {
				work.MessageIDs = append(work.MessageIDs, id)
			}
		}
		for _, id := range local.EvidenceIDs {
			if !slices.Contains(work.EvidenceIDs, id) {
				work.EvidenceIDs = append(work.EvidenceIDs, id)
			}
		}
		work.CacheRevision = max(work.CacheRevision, local.CacheRevision)
	}
	for _, profile := range profiles {
		local, err := c.store.RemoveXAuthorProfileContext(ctx, profile)
		if err != nil {
			return work, err
		}
		work.EvidenceIDs = append(work.EvidenceIDs, local.EvidenceIDs...)
		work.CacheRevision = max(work.CacheRevision, local.CacheRevision)
	}
	checkTargets := func() error {
		for _, group := range groups {
			current, err := c.store.XRemovalTargetsContext(ctx, group.Root.SourceID, group.Root.CanonicalID)
			if err != nil {
				return err
			}
			for _, target := range current {
				if !owned[targetKey{target.SourceID, target.CanonicalID}] {
					return store.ErrXRemovalNeedsOwner
				}
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
	if len(work.MessageIDs) > 0 {
		backend, closeBackend, err := c.cleanupBackend(ctx)
		if err != nil {
			return work, err
		}
		if closeBackend != nil {
			defer closeBackend()
		}
		if backend != nil {
			lister, ok := backend.(interface {
				GenerationIDs(ctx context.Context) ([]vector.GenerationID, error)
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
	}
	if c.afterVectors != nil {
		if err := c.afterVectors(); err != nil {
			return work, err
		}
	}
	if c.server != nil {
		modeBefore := c.server.AnalyticsMode()
		if modeBefore == api.AnalyticsModeDuckDB || modeBefore == api.AnalyticsModeInitializing || c.server.AnalyticsInitializationActive() {
			c.analyticsReopen = true
		}
		if c.beforeAnalyticsRetire != nil {
			if err := c.beforeAnalyticsRetire(); err != nil {
				return work, err
			}
		}
		c.server.InvalidateMessageContent(work.MessageIDs)
		mode := api.AnalyticsModeSQLFallback
		if c.store.IsPostgreSQL() {
			mode = api.AnalyticsModePostgres
		}
		generation, initializationActive, err := c.server.RetireContentAnalytics(
			query.NewEngine(c.store.DB(), c.store.IsPostgreSQL()), mode,
		)
		c.analyticsGeneration = generation
		if initializationActive {
			c.analyticsReopen = true
		}
		if err != nil {
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
		if _, err := buildCacheLocked(ctx, c.cfg.DatabaseDSN(), c.cfg.AnalyticsDir(), true, false, analyticsBuilderOverrides(c.cfg.Analytics)); err != nil {
			return work, err
		}
	}
	if err := checkTargets(); err != nil {
		return work, err
	}
	if c.server != nil && c.analyticsReopen {
		// openDaemonAnalyticsEngine may build a missing cache through its normal
		// subprocess path. Release the destructive-maintenance locks first so
		// that builder can acquire them; the content write fence remains held.
		unlockErr := unlock()
		cacheLockHeld = false
		if unlockErr != nil {
			return work, unlockErr
		}
		engine, mode, _, err := openDaemonAnalyticsEngine(ctx, c.cfg, c.store, startupCacheBuildIntentNone)
		if err != nil {
			return work, fmt.Errorf("reopen analytics after X cleanup: %w", err)
		}
		if mode == api.AnalyticsModeDuckDB {
			if !c.server.SetAnalyticsEngineForGeneration(c.analyticsGeneration, engine, mode) {
				return work, errors.New("analytics cleanup generation changed before replacement engine install")
			}
		} else if engine != nil {
			if err := engine.Close(); err != nil {
				return work, fmt.Errorf("close non-DuckDB analytics after X cleanup: %w", err)
			}
		}
		c.analyticsReopen = false
		c.analyticsGeneration = 0
	}
	for _, key := range keys {
		local := completed[key]
		local.DerivedComplete = true
		if err := c.store.FinishXRemovalContext(ctx, local.SourceID, local); err != nil {
			return work, err
		}
	}
	for _, profile := range profiles {
		profile.DerivedComplete = true
		if err := c.store.FinishXAuthorProfileRemovalContext(ctx, profile); err != nil {
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
	var exists bool
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
			profiles, err := c.store.PendingXAuthorProfileRemovalsContext(ctx)
			if err != nil || len(profiles) == 0 {
				return err
			}
			// Profile facts share the attribution lock across sources. Only the
			// observing source owns the durable obligation and its completion.
			sourceID := profiles[0].SourceID
			profiles = slices.DeleteFunc(profiles, func(work store.XAuthorProfileRemovalWork) bool { return work.SourceID != sourceID })
			execution, err := c.store.AcquireSyncExecutionContext(ctx, sourceID)
			if err != nil {
				return err
			}
			_, cleanupErr := c.removeXContentsBatch(ctx, nil, profiles...)
			if err := errors.Join(cleanupErr, execution.Release()); err != nil {
				return err
			}
			continue
		}
		execution, err := c.store.AcquireXRemovalExecutionBatchContext(ctx, pending)
		if err != nil {
			return err
		}
		_, cleanupErr := c.removeXContentsBatch(ctx, execution.Groups)
		releaseErr := execution.Release()
		if err := errors.Join(cleanupErr, releaseErr); err != nil {
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
	return c.removeXContent(ctx, work)
}

// DuringSyncBatch finishes all removals from one persisted page under a single
// content/cache publication fence while retaining the importer's source lock.
func (c *XContentCleanup) DuringSyncBatch(ctx context.Context, work []store.XRemovalWork) error {
	return c.removeXContentBatch(ctx, work)
}
