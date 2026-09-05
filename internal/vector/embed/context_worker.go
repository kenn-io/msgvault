package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// ContextWorkStore is the source-side surface required by ContextWorker.
// *store.Store implements it.
type ContextWorkStore interface {
	ScanEmbeddingChanges(ctx context.Context, after int64, limit int) ([]store.EmbeddingChange, error)
	LatestEmbeddingChangeSequence(ctx context.Context) (int64, error)
	ScanForEmbeddingScoped(ctx context.Context, target, afterID int64, limit int, messageTypes []string, sourceIDs []int64) ([]int64, error)
	SetEmbedGenGroupIfUnchanged(ctx context.Context, stamps []store.EmbedGenStamp, metadata store.EmbedGenMetadataVersion, target int64) (bool, error)
	ResetEmbedGen(ctx context.Context, ids []int64) error
}

var _ ContextWorkStore = (*store.Store)(nil)

// defaultContextRunUTF8Bytes caps one worker tick at roughly ten maximum-size
// Voyage requests. Durable journal and reconciliation cursors resume remaining
// work on the next tick without repeating completed publications.
const defaultContextRunUTF8Bytes = 1_000_000

// ContextWorkerHooks are deterministic crash-boundary seams. Production leaves
// every field nil.
type ContextWorkerHooks struct {
	AfterSnapshot              func(SourceSnapshot) error
	AfterSourcePage            func(int) error
	AfterOrdinaryDiscoveryPage func(int) error
	BeforePublish              func() error
	AfterPublish               func() error
	AfterCoverage              func() error
}

// ContextWorkerDeps bundles contextual source, assembly, embedding, and
// publication collaborators.
type ContextWorkerDeps struct {
	Backend    vector.Backend
	Publisher  vector.DocumentPublisher
	Store      ContextWorkStore
	Assembler  Assembler
	Client     SemanticClient
	BuildScope vector.BuildScope

	ChangeBatchSize         int
	ReconcileBatchSize      int
	MaxRunUTF8Bytes         int
	DocumentPrefixUTF8Bytes int
	Hooks                   ContextWorkerHooks
	Recorder                operations.Recorder
	Log                     *slog.Logger
}

// ContextConvergence is the durable state Task 10 can use for activation and
// scheduler decisions.
type ContextConvergence struct {
	SourceSequence    int64
	ConsumedSequence  int64
	ReconcileCursor   string
	ReconcileComplete bool
	CoverageMissing   bool
	Converged         bool
}

// ContextWorker drains contextual mutations and reconciles all source scopes.
type ContextWorker struct {
	deps   ContextWorkerDeps
	source *store.Store
}

// NewContextWorker applies bounded defaults. Configuration errors are returned
// by RunOnce so construction stays compatible with the ordinary worker.
func NewContextWorker(d ContextWorkerDeps) *ContextWorker {
	if d.ChangeBatchSize <= 0 {
		d.ChangeBatchSize = 64
	}
	if d.ReconcileBatchSize <= 0 {
		d.ReconcileBatchSize = 128
	}
	if d.MaxRunUTF8Bytes <= 0 {
		d.MaxRunUTF8Bytes = defaultContextRunUTF8Bytes
	}
	d.BuildScope = vector.NewBuildScope(d.BuildScope.MessageTypes, d.BuildScope.SourceIDs)
	if d.Log == nil {
		d.Log = slog.Default()
	}
	source, _ := d.Store.(*store.Store)
	return &ContextWorker{deps: d, source: source}
}

// ReclaimStale matches the ordinary worker contract. Contextual work has no
// leases; vector and source cursors make every boundary replayable.
func (w *ContextWorker) ReclaimStale(context.Context) (int, error) { return 0, nil }

// RunOnce drains the journal, preserves ordinary missing-row discovery, and
// completes or resumes reconciliation.
func (w *ContextWorker) RunOnce(
	ctx context.Context, gen vector.GenerationID, scope operations.PassScope,
) (RunResult, error) {
	return w.runOperationPass(ctx, gen, scope, false)
}

// RunBackstop restarts only reconciliation. It never rewinds the consumed
// mutation sequence.
func (w *ContextWorker) RunBackstop(
	ctx context.Context, gen vector.GenerationID, scope operations.PassScope,
) (RunResult, error) {
	return w.runOperationPass(ctx, gen, scope, true)
}

func (w *ContextWorker) runOperationPass(
	ctx context.Context, gen vector.GenerationID, scope operations.PassScope, backstop bool,
) (result RunResult, retErr error) {
	pass, terminal, err := beginOperationPass(
		ctx, w.deps.Recorder, operations.KindMessageEmbedding, scope, w.deps.Log,
	)
	if err != nil {
		return result, err
	}
	if terminal != nil {
		return runResultFromOperationRun(terminal)
	}
	defer func() {
		counters := finalRunCounters(result)
		pass.checkpoint(ctx, counters)
		pass.finish(ctx, counters, retErr)
	}()
	runCtx := contextWithOperationPass(ctx, pass)
	if !backstop {
		return w.run(runCtx, gen)
	}
	return w.runBackstop(runCtx, gen)
}

func (w *ContextWorker) runBackstop(ctx context.Context, gen vector.GenerationID) (RunResult, error) {
	if err := w.validate(); err != nil {
		return RunResult{Contextual: &ContextConvergence{}}, err
	}
	progress, err := w.deps.Publisher.GetDocumentProgress(ctx, gen)
	if err != nil {
		if errors.Is(err, vector.ErrGenerationRetired) {
			return RunResult{Contextual: &ContextConvergence{}}, nil
		}
		return RunResult{Contextual: &ContextConvergence{}}, fmt.Errorf("read contextual reconcile cursor: %w", err)
	}
	if !strings.HasPrefix(progress.ReconcileCursor, "done:") {
		return w.run(ctx, gen)
	}
	if err := w.deps.Publisher.ResetDocumentReconcileCursor(ctx, gen); err != nil {
		if errors.Is(err, vector.ErrGenerationRetired) {
			return RunResult{Contextual: &ContextConvergence{}}, nil
		}
		return RunResult{Contextual: &ContextConvergence{}}, fmt.Errorf("reset contextual reconcile cursor: %w", err)
	}
	return w.run(ctx, gen)
}

func (w *ContextWorker) validate() error {
	if w.deps.Backend == nil || w.deps.Publisher == nil || w.deps.Store == nil ||
		w.deps.Assembler == nil || w.deps.Client == nil || w.source == nil {
		return errors.New("context worker: backend, publisher, concrete store, assembler, and client are required")
	}
	return nil
}

var (
	errContextGenerationRetired  = errors.New("context worker: generation retired")
	errContextRunBudgetExhausted = errors.New("context worker: per-run input budget exhausted")
)

type contextRunBudgetKey struct{}

type contextRunBudget struct {
	limit int
	used  int
}

func (b *contextRunBudget) reserve(inputBytes int) bool {
	if inputBytes <= b.limit-b.used {
		b.used += inputBytes
		return true
	}
	// A publication scope is atomic. Allow one scope larger than the normal
	// run cap so a large but provider-valid day cannot deadlock forever.
	if b.used == 0 {
		b.used = inputBytes
		return true
	}
	return false
}

func (w *ContextWorker) run(ctx context.Context, gen vector.GenerationID) (res RunResult, retErr error) {
	res.Contextual = &ContextConvergence{}
	if err := w.validate(); err != nil {
		return res, err
	}
	// Publish durable contextual ownership before enabling source capture.
	// Journal cleanup takes the source lock and rechecks this vector-side row,
	// so the two operations cannot disable a worker that is starting up.
	if err := w.deps.Publisher.AdvanceDocumentChangeWatermark(ctx, gen, 0); err != nil {
		return res, fmt.Errorf("initialize contextual journal ownership: %w", err)
	}
	if err := w.source.EnableEmbeddingChangeJournal(ctx); err != nil {
		return res, err
	}
	defer func() {
		if err := pruneEmbeddingJournal(ctx, w.deps.Backend, w.deps.Store); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	ctx = context.WithValue(ctx, contextRunBudgetKey{}, &contextRunBudget{limit: w.deps.MaxRunUTF8Bytes})
	fresh, err := w.initializeFreshGeneration(ctx, gen)
	if err != nil {
		if errors.Is(err, errContextGenerationRetired) {
			return res, nil
		}
		return res, err
	}
	if !fresh {
		if err := w.drainJournal(ctx, gen, &res); err != nil {
			if errors.Is(err, errContextRunBudgetExhausted) {
				return res, nil
			}
			if errors.Is(err, errContextGenerationRetired) {
				return res, nil
			}
			return res, err
		}
	}
	needsOrdinaryDiscovery := fresh
	if !needsOrdinaryDiscovery {
		progress, err := w.deps.Publisher.GetDocumentProgress(ctx, gen)
		if err != nil {
			if errors.Is(err, vector.ErrGenerationRetired) {
				return res, nil
			}
			return res, fmt.Errorf("read contextual discovery progress: %w", err)
		}
		needsOrdinaryDiscovery = progress.ReconcileCursor == "" ||
			strings.HasPrefix(progress.ReconcileCursor, "discovery:")
	}
	// The mutation journal covers normal work after reconciliation. A durable
	// discovery cursor resumes a fresh build or explicit backstop without
	// letting a permanently rejected low-ID document starve later messages.
	// Once source or orphan reconciliation begins, resume that phase directly.
	if needsOrdinaryDiscovery {
		if err := w.drainOrdinaryDiscovery(ctx, gen, &res); err != nil {
			if errors.Is(err, errContextRunBudgetExhausted) {
				return res, nil
			}
			if errors.Is(err, errContextGenerationRetired) {
				return res, nil
			}
			return res, err
		}
	}
	if err := w.reconcile(ctx, gen, &res); err != nil {
		if errors.Is(err, errContextRunBudgetExhausted) {
			return res, nil
		}
		if errors.Is(err, errContextGenerationRetired) {
			return res, nil
		}
		return res, err
	}
	// Catch mutations committed while the bounded reconciliation pages ran.
	if err := w.drainJournal(ctx, gen, &res); err != nil {
		if errors.Is(err, errContextRunBudgetExhausted) {
			return res, nil
		}
		if errors.Is(err, errContextGenerationRetired) {
			return res, nil
		}
		return res, err
	}
	convergence, err := w.convergence(ctx, gen)
	if err != nil {
		return res, err
	}
	if res.Failed > 0 {
		// Coverage reset makes activation fail closed. Mark this pass the same
		// way without adding another archive-wide count to every idle worker
		// tick. A later idle pass can stop the CLI loop; the activation checker
		// still performs the authoritative coverage count.
		convergence.CoverageMissing = true
		convergence.Converged = false
	}
	res.Contextual = &convergence
	return res, nil
}

type embeddingJournalStore interface {
	LatestEmbeddingChangeSequence(ctx context.Context) (int64, error)
	PruneEmbeddingChangesThrough(ctx context.Context, sequence int64) (int64, error)
}

func pruneEmbeddingJournal(ctx context.Context, backend vector.Backend, source any) error {
	journal, ok := source.(embeddingJournalStore)
	if !ok {
		return nil
	}
	retention, ok := backend.(vector.DocumentJournalRetention)
	if !ok {
		return nil
	}
	sequence, tracked, err := retention.MinimumDocumentChangeWatermark(ctx)
	if err != nil {
		return fmt.Errorf("read contextual journal retention watermark: %w", err)
	}
	if !tracked {
		if lifecycle, ok := backend.(vector.DocumentJournalLifecycle); ok {
			if err := lifecycle.CleanupDocumentJournalIfUnused(ctx); err != nil {
				return fmt.Errorf("clean up unused contextual journal: %w", err)
			}
			return nil
		}
		sequence, err = journal.LatestEmbeddingChangeSequence(ctx)
		if err != nil {
			return fmt.Errorf("read journal sequence for unused contextual mode: %w", err)
		}
	}
	if _, err := journal.PruneEmbeddingChangesThrough(ctx, sequence); err != nil {
		return fmt.Errorf("prune contextual journal: %w", err)
	}
	return nil
}

// initializeFreshGeneration pins the source clock before the first
// reconciliation. The complete current snapshot is the rebuild input; older
// journal entries describe superseded states and must not be replayed first.
// Mutations committed after this pin have larger sequences and are drained by
// the normal post-reconciliation pass.
func (w *ContextWorker) initializeFreshGeneration(ctx context.Context, gen vector.GenerationID) (bool, error) {
	progress, err := w.deps.Publisher.GetDocumentProgress(ctx, gen)
	if err != nil {
		if errors.Is(err, vector.ErrGenerationRetired) {
			return false, errContextGenerationRetired
		}
		return false, fmt.Errorf("read fresh contextual progress: %w", err)
	}
	if progress.ChangeSequence != 0 || progress.ReconcileCursor != "" {
		return false, nil
	}
	latest, err := w.deps.Store.LatestEmbeddingChangeSequence(ctx)
	if err != nil {
		return false, fmt.Errorf("pin fresh contextual source sequence: %w", err)
	}
	if err := w.deps.Publisher.AdvanceDocumentChangeWatermark(ctx, gen, latest); err != nil {
		if errors.Is(err, vector.ErrGenerationRetired) {
			return false, errContextGenerationRetired
		}
		return false, fmt.Errorf("initialize contextual change watermark to %d: %w", latest, err)
	}
	return true, nil
}

type contextScope struct {
	key      string
	selector AffectedScope
}

type preparedScope struct {
	key          string
	sequence     int64
	docs         []Document
	liveVersions []SourceVersion
}

type preparedScopePublication struct {
	scope      preparedScope
	existing   []vector.DocumentRecord
	changed    bool
	fence      bool
	failedDocs []bool
	preserved  []bool
	resolved   []bool
	vectors    [][][]float32
}

type preparedDocumentTarget struct {
	scopeIndex int
	docIndex   int
}

func (w *ContextWorker) drainJournal(ctx context.Context, gen vector.GenerationID, res *RunResult) error {
	for {
		progress, err := w.deps.Publisher.GetDocumentProgress(ctx, gen)
		if err != nil {
			return fmt.Errorf("read contextual progress: %w", err)
		}
		snapshot, err := BeginSourceSnapshot(ctx, w.source)
		if err != nil {
			return err
		}
		changes, err := snapshot.embeddingChanges(ctx, progress.ChangeSequence, w.deps.ChangeBatchSize)
		if err != nil {
			_ = snapshot.Close()
			return err
		}
		if len(changes) == 0 {
			if err := w.closeSnapshot(snapshot); err != nil {
				return err
			}
			if progress.JournalCursor != "" {
				if err := w.deps.Publisher.SetDocumentJournalCursor(ctx, gen, ""); err != nil {
					return fmt.Errorf("clear stale contextual journal subcursor: %w", err)
				}
			}
			return nil
		}
		change := changes[0]
		if progress.JournalCursor == "" && isMetadataFanoutChange(change) {
			for _, candidate := range changes[1:] {
				if metadataFanoutIdentity(candidate) != metadataFanoutIdentity(change) {
					break
				}
				change.Sequence = candidate.Sequence
			}
		}
		if isMetadataFanoutChange(change) {
			after := journalCursorForSequence(progress.JournalCursor, change.Sequence)
			scopes, more, pageErr := snapshot.metadataScopesAfter(
				ctx, change, w.deps.BuildScope, after, w.deps.ChangeBatchSize,
			)
			if pageErr != nil {
				_ = snapshot.Close()
				return pageErr
			}
			if err := snapshot.Close(); err != nil {
				return err
			}
			if len(scopes) != 0 {
				if budget, ok := ctx.Value(contextRunBudgetKey{}).(*contextRunBudget); ok && budget.used >= budget.limit {
					return errContextRunBudgetExhausted
				}
				pageSnapshot, err := BeginSourceSnapshot(ctx, w.source)
				if err != nil {
					return err
				}
				prepared, err := w.prepareScopes(ctx, pageSnapshot, scopes)
				if err != nil {
					_ = pageSnapshot.Close()
					return err
				}
				if err := w.closeSnapshot(pageSnapshot); err != nil {
					return err
				}
				processed, publishErr := w.publishPreparedScopes(ctx, gen, prepared, res)
				if processed > 0 {
					cursor := encodeJournalCursor(change.Sequence, prepared[processed-1].key)
					if err := w.deps.Publisher.SetDocumentJournalCursor(ctx, gen, cursor); err != nil {
						return fmt.Errorf("persist contextual journal subcursor: %w", err)
					}
				}
				if publishErr != nil {
					return fmt.Errorf("publish metadata journal event %d: %w", change.Sequence, publishErr)
				}
				if processed < len(prepared) {
					return errContextRunBudgetExhausted
				}
			}
			if more {
				continue
			}
			if err := w.deps.Publisher.AdvanceDocumentChangeWatermark(ctx, gen, change.Sequence); err != nil {
				return fmt.Errorf("advance contextual change watermark to %d: %w", change.Sequence, err)
			}
			if err := w.deps.Publisher.SetDocumentJournalCursor(ctx, gen, ""); err != nil {
				return fmt.Errorf("clear contextual journal subcursor: %w", err)
			}
			continue
		}
		normalChanges := changes
		for i, candidate := range changes {
			if isMetadataFanoutChange(candidate) {
				normalChanges = changes[:i]
				break
			}
		}
		scopes, err := snapshot.scopesForChanges(ctx, normalChanges, w.deps.BuildScope)
		if err != nil {
			_ = snapshot.Close()
			return err
		}
		if err := snapshot.Close(); err != nil {
			return err
		}
		last := normalChanges[len(normalChanges)-1].Sequence
		afterKey := journalCursorForSequence(progress.JournalCursor, last)
		if afterKey != "" {
			first := sort.Search(len(scopes), func(i int) bool { return scopes[i].key > afterKey })
			scopes = scopes[first:]
		}
		for start := 0; start < len(scopes); start += w.deps.ChangeBatchSize {
			end := min(start+w.deps.ChangeBatchSize, len(scopes))
			pageSnapshot, err := BeginSourceSnapshot(ctx, w.source)
			if err != nil {
				return err
			}
			prepared, err := w.prepareScopes(ctx, pageSnapshot, scopes[start:end])
			if err != nil {
				_ = pageSnapshot.Close()
				return err
			}
			if err := w.closeSnapshot(pageSnapshot); err != nil {
				return err
			}
			processed, publishErr := w.publishPreparedScopes(ctx, gen, prepared, res)
			if processed > 0 {
				cursor := encodeJournalCursor(last, prepared[processed-1].key)
				if err := w.deps.Publisher.SetDocumentJournalCursor(ctx, gen, cursor); err != nil {
					return fmt.Errorf("persist contextual journal scope cursor: %w", err)
				}
			}
			if publishErr != nil {
				return fmt.Errorf("publish journal scope page through sequence %d: %w", last, publishErr)
			}
		}
		if err := w.deps.Publisher.AdvanceDocumentChangeWatermark(ctx, gen, last); err != nil {
			return fmt.Errorf("advance contextual change watermark to %d: %w", last, err)
		}
		if err := w.deps.Publisher.SetDocumentJournalCursor(ctx, gen, ""); err != nil {
			return fmt.Errorf("clear contextual journal scope cursor: %w", err)
		}
	}
}

func (w *ContextWorker) drainOrdinaryDiscovery(ctx context.Context, gen vector.GenerationID, res *RunResult) error {
	var after int64
	var resumeScope string
	progress, err := w.deps.Publisher.GetDocumentProgress(ctx, gen)
	if err != nil {
		return fmt.Errorf("read contextual ordinary discovery cursor: %w", err)
	}
	if cursor, ok := strings.CutPrefix(progress.ReconcileCursor, "discovery:"); ok {
		cursor, resumeScope, _ = strings.Cut(cursor, "|")
		if cursor != "" {
			after, err = strconv.ParseInt(cursor, 10, 64)
			if err != nil {
				return fmt.Errorf("parse contextual ordinary discovery cursor %q: %w", progress.ReconcileCursor, err)
			}
		}
	}
	for {
		pageAfter := after
		ids, err := w.deps.Store.ScanForEmbeddingScoped(
			ctx, int64(gen), after, w.deps.ChangeBatchSize,
			w.deps.BuildScope.MessageTypes, w.deps.BuildScope.SourceIDs,
		)
		if err != nil {
			return fmt.Errorf("scan contextual ordinary discovery: %w", err)
		}
		if w.deps.Hooks.AfterOrdinaryDiscoveryPage != nil {
			if err := w.deps.Hooks.AfterOrdinaryDiscoveryPage(len(ids)); err != nil {
				return fmt.Errorf("after contextual ordinary discovery page: %w", err)
			}
		}
		if len(ids) == 0 {
			// Discovery has considered the complete missing set. Source
			// reconciliation must still start at zero so it can repair any
			// stamped row whose document ledger drifted independently.
			if err := w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen, "source:0"); err != nil {
				return fmt.Errorf("complete contextual ordinary discovery: %w", err)
			}
			return nil
		}
		res.Claimed += len(ids)
		snapshot, err := BeginSourceSnapshot(ctx, w.source)
		if err != nil {
			return err
		}
		scopes := make([]contextScope, 0, len(ids))
		for _, id := range ids {
			row, found, readErr := snapshot.MessageMeta(ctx, id)
			if readErr != nil {
				_ = snapshot.Close()
				return readErr
			}
			if !found {
				continue
			}
			scopes = append(scopes, liveMessageScope(row))
		}
		prepared, err := w.prepareScopes(ctx, snapshot, scopes)
		if err != nil {
			_ = snapshot.Close()
			return err
		}
		if err := w.closeSnapshot(snapshot); err != nil {
			return err
		}
		if resumeScope != "" {
			first := sort.Search(len(prepared), func(i int) bool { return prepared[i].key > resumeScope })
			prepared = prepared[first:]
		}
		processed, publishErr := w.publishPreparedScopes(ctx, gen, prepared, res)
		if processed > 0 {
			cursor := "discovery:" + strconv.FormatInt(pageAfter, 10) + "|" + prepared[processed-1].key
			if err := w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen, cursor); err != nil {
				return fmt.Errorf("persist contextual ordinary discovery scope cursor: %w", err)
			}
		}
		if publishErr != nil {
			return fmt.Errorf("publish discovered scopes: %w", publishErr)
		}
		resumeScope = ""
		after = ids[len(ids)-1]
		if err := w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen,
			"discovery:"+strconv.FormatInt(after, 10)); err != nil {
			return fmt.Errorf("advance contextual ordinary discovery cursor: %w", err)
		}
	}
}

func (w *ContextWorker) closeSnapshot(snapshot SourceSnapshot) error {
	if err := snapshot.Close(); err != nil {
		return err
	}
	if w.deps.Hooks.AfterSnapshot != nil {
		if err := w.deps.Hooks.AfterSnapshot(snapshot); err != nil {
			return fmt.Errorf("after source snapshot: %w", err)
		}
	}
	return nil
}

func (w *ContextWorker) prepareScopes(ctx context.Context, snapshot SourceSnapshot, scopes []contextScope) ([]preparedScope, error) {
	byKey := make(map[string]contextScope)
	liveVersionsByKey := make(map[string][]SourceVersion)
	selectors := make([]AffectedScope, 0, len(scopes))
	for _, scope := range scopes {
		if scope.key == "" {
			continue
		}
		byKey[scope.key] = scope
		allowed, err := w.selectorInBuildScope(ctx, snapshot, scope.selector)
		if err != nil {
			return nil, err
		}
		if allowed {
			selectors = append(selectors, scope.selector)
			versions, err := snapshot.MessageVersions(ctx, scope.selector)
			if err != nil {
				return nil, fmt.Errorf("read live contextual scope %q: %w", scope.key, err)
			}
			sort.Slice(versions, func(i, j int) bool { return versions[i].MessageID < versions[j].MessageID })
			liveVersionsByKey[scope.key] = versions
		}
	}
	docs, err := w.deps.Assembler.AssembleScopes(ctx, snapshot, selectors)
	if err != nil {
		return nil, fmt.Errorf("assemble contextual scopes: %w", err)
	}
	for _, doc := range docs {
		if _, exists := byKey[doc.ScopeKey]; !exists {
			byKey[doc.ScopeKey] = contextScope{key: doc.ScopeKey, selector: selectorForScopeKey(doc.ScopeKey)}
		}
	}
	docsByScope := make(map[string][]Document)
	for _, doc := range docs {
		docsByScope[doc.ScopeKey] = append(docsByScope[doc.ScopeKey], doc)
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]preparedScope, 0, len(keys))
	for _, key := range keys {
		scopeDocs := docsByScope[key]
		sort.Slice(scopeDocs, func(i, j int) bool { return scopeDocs[i].Key < scopeDocs[j].Key })
		out = append(out, preparedScope{key: key, sequence: snapshot.SourceSequence(), docs: scopeDocs,
			liveVersions: liveVersionsByKey[key]})
	}
	return out, nil
}

// messageTypeInScope enforces only the message-type dimension of a build
// scope. Each dimension must be checked independently: ContainsMessageType
// on a source-only scope (empty MessageTypes) returns false for every type
// and would wrongly reject the whole archive.
func messageTypeInScope(scope vector.BuildScope, messageType string) bool {
	return len(scope.MessageTypes) == 0 || scope.ContainsMessageType(messageType)
}

// selectorInBuildScope is the authoritative scope gate: every publication
// path funnels its selectors through prepareScopes, which uses this to
// decide whether a scope's text may be assembled and sent to the embedding
// provider (a disallowed scope is still published as a tombstone so
// moved-out content is removed). Both scope dimensions are enforced —
// message type from the selector or row, and the account dimension from
// the message's or conversation's source.
func (w *ContextWorker) selectorInBuildScope(
	ctx context.Context, snapshot SourceSnapshot, selector AffectedScope,
) (bool, error) {
	scope := w.deps.BuildScope
	if scope.IsEmpty() {
		return true, nil
	}
	if selector.MessageID != 0 {
		row, found, err := snapshot.MessageMeta(ctx, selector.MessageID)
		if err != nil {
			return false, err
		}
		return found && messageTypeInScope(scope, row.MessageType) &&
			(len(scope.SourceIDs) == 0 || scope.ContainsSource(row.SourceID)), nil
	}
	if !messageTypeInScope(scope, selector.Kind) {
		return false, nil
	}
	if len(scope.SourceIDs) == 0 {
		return true, nil
	}
	if selector.ConversationID == 0 {
		// No source identity to prove membership against an account-scoped
		// build; fail closed so unattributable text cannot leak to the
		// embedding provider.
		return false, nil
	}
	sourceID, found, err := snapshot.ConversationSourceID(ctx, selector.ConversationID)
	if err != nil {
		return false, err
	}
	return found && scope.ContainsSource(sourceID), nil
}

func (w *ContextWorker) publishPreparedScopes(ctx context.Context, gen vector.GenerationID, scopes []preparedScope, res *RunResult) (int, error) {
	plans := make([]preparedScopePublication, 0, len(scopes))
	inputs := make([]DocumentInput, 0)
	targets := make([]preparedDocumentTarget, 0)
	budget, _ := ctx.Value(contextRunBudgetKey{}).(*contextRunBudget)
	budgetExhausted := false
	for _, scope := range scopes {
		plan := preparedScopePublication{scope: scope}
		scopeBytes := 0
		for _, doc := range scope.docs {
			input := DocumentInput{Chunks: make([]string, len(doc.Chunks))}
			for chunkIndex, chunk := range doc.Chunks {
				input.Chunks[chunkIndex] = chunk.Text
			}
			scopeBytes += documentInputUTF8Bytes(input, w.deps.DocumentPrefixUTF8Bytes)
		}
		// Assembly and source reads consume bounded work even when durable
		// vectors can be preserved or a scope now needs only a tombstone.
		scopeBytes = max(scopeBytes, 1)
		if budget != nil && !budget.reserve(scopeBytes) {
			budgetExhausted = true
			break
		}
		existing, err := w.deps.Publisher.ListDocumentsForScope(ctx, gen, scope.key)
		if err != nil {
			return 0, fmt.Errorf("scope %q list published documents: %w", scope.key, err)
		}
		plan.changed = !sameDocumentRevisions(existing, scope.docs)
		plan.existing = existing
		if !plan.changed {
			plan.fence = documentFenceBehind(existing, scope.sequence)
			plans = append(plans, plan)
			continue
		}
		existingByKey := make(map[string]vector.DocumentRecord, len(existing))
		for _, record := range existing {
			existingByKey[record.Key] = record
		}
		plan.vectors = make([][][]float32, len(scope.docs))
		plan.failedDocs = make([]bool, len(scope.docs))
		plan.preserved = make([]bool, len(scope.docs))
		plan.resolved = make([]bool, len(scope.docs))
		scopeInputs := make([]DocumentInput, 0, len(scope.docs))
		scopeTargets := make([]preparedDocumentTarget, 0, len(scope.docs))
		for docIndex, doc := range scope.docs {
			if record, ok := existingByKey[doc.Key]; ok && record.Kind == doc.Kind &&
				record.PublishedRevision == doc.Revision &&
				slices.Equal(record.Members, documentMembers(doc)) {
				plan.preserved[docIndex] = true
				continue
			}
			input := DocumentInput{Chunks: make([]string, len(doc.Chunks))}
			for chunkIndex, chunk := range doc.Chunks {
				input.Chunks[chunkIndex] = chunk.Text
			}
			scopeInputs = append(scopeInputs, input)
			scopeTargets = append(scopeTargets, preparedDocumentTarget{docIndex: docIndex})
		}
		scopeIndex := len(plans)
		for i := range scopeTargets {
			scopeTargets[i].scopeIndex = scopeIndex
		}
		plans = append(plans, plan)
		inputs = append(inputs, scopeInputs...)
		targets = append(targets, scopeTargets...)
	}
	results, embedErr := w.embedDocuments(ctx, inputs)
	if len(results) > len(targets) || (embedErr == nil && len(results) != len(targets)) {
		return 0, fmt.Errorf("contextual batched vector document count mismatch: got %d, expected %d", len(results), len(targets))
	}
	for i, target := range targets {
		if i >= len(results) {
			break
		}
		plans[target.scopeIndex].resolved[target.docIndex] = true
		if results[i].tooLarge {
			plans[target.scopeIndex].failedDocs[target.docIndex] = true
			continue
		}
		if results[i].embeddedChunks != nil {
			applyTruncatedEmbedding(&plans[target.scopeIndex].scope.docs[target.docIndex], results[i].embeddedChunks)
			res.Truncated++
		}
		plans[target.scopeIndex].vectors[target.docIndex] = results[i].vectors
	}
	completePlans := len(plans)
	partialPlan := -1
	if embedErr != nil {
		completePlans = 0
		for completePlans < len(plans) && preparedScopeResolved(plans[completePlans]) {
			completePlans++
		}
		if completePlans < len(plans) && preparedScopeHasNewVectors(plans[completePlans]) {
			partialPlan = completePlans
		}
	}
	publicationPlans := completePlans
	if partialPlan >= 0 {
		publicationPlans++
	}
	publications := make([]vector.DocumentScopePublication, 0, publicationPlans)
	partialPublished := false
	for planIndex := range publicationPlans {
		plan := plans[planIndex]
		if !plan.changed {
			if plan.fence {
				publications = append(publications, vector.DocumentScopePublication{
					ScopeKey: plan.scope.key, SourceSequence: plan.scope.sequence,
					Documents: documentPublications(plan.scope.docs), FenceOnly: true,
				})
			}
			continue
		}
		documents, vectors, preserved := successfulPreparedDocuments(plan)
		docs, chunks, err := buildDocumentPublication(documents, vectors, preserved)
		if err != nil {
			return 0, fmt.Errorf("scope %q: %w", plan.scope.key, err)
		}
		if planIndex == partialPlan {
			var safe bool
			docs, safe = preserveUnresolvedDocumentPublications(plan.scope.sequence, docs, plan.existing)
			if !safe {
				continue
			}
			partialPublished = true
		}
		if w.deps.Hooks.BeforePublish != nil {
			if err := w.deps.Hooks.BeforePublish(); err != nil {
				return 0, fmt.Errorf("scope %q: before index commit: %w", plan.scope.key, err)
			}
		}
		publications = append(publications, vector.DocumentScopePublication{
			ScopeKey: plan.scope.key, SourceSequence: plan.scope.sequence, Documents: docs, Chunks: chunks,
		})
	}
	if len(publications) > 0 {
		if err := w.deps.Publisher.PublishScopes(ctx, gen, publications); err != nil {
			if errors.Is(err, vector.ErrGenerationRetired) {
				return 0, errContextGenerationRetired
			}
			return 0, fmt.Errorf("publish prepared scope page: %w", err)
		}
	}
	processed := 0
	for planIndex := range completePlans {
		plan := plans[planIndex]
		if plan.changed {
			if w.deps.Hooks.AfterPublish != nil {
				if err := w.deps.Hooks.AfterPublish(); err != nil {
					return processed, fmt.Errorf("scope %q: after index commit: %w", plan.scope.key, err)
				}
			}
		}
		if err := w.coverPreparedScopePlan(ctx, gen, plan, res); err != nil {
			return processed, fmt.Errorf("scope %q: %w", plan.scope.key, err)
		}
		processed++
	}
	if partialPublished && w.deps.Hooks.AfterPublish != nil {
		if err := w.deps.Hooks.AfterPublish(); err != nil {
			return processed, fmt.Errorf("scope %q: after partial index commit: %w", plans[partialPlan].scope.key, err)
		}
	}
	if embedErr != nil {
		return processed, embedErr
	}
	if budgetExhausted {
		return processed, errContextRunBudgetExhausted
	}
	return processed, nil
}

func preserveUnresolvedDocumentPublications(
	sourceSequence int64,
	resolved []vector.DocumentPublication,
	existing []vector.DocumentRecord,
) ([]vector.DocumentPublication, bool) {
	keys := make(map[string]struct{}, len(resolved))
	members := make(map[int64]struct{})
	for _, document := range resolved {
		keys[document.Key] = struct{}{}
		for _, messageID := range document.Members {
			members[messageID] = struct{}{}
		}
	}
	for _, record := range existing {
		if _, replaced := keys[record.Key]; replaced {
			continue
		}
		for _, messageID := range record.Members {
			if _, reassigned := members[messageID]; reassigned {
				return resolved, false
			}
		}
		resolved = append(resolved, vector.DocumentPublication{
			Key: record.Key, Kind: record.Kind, Revision: record.PublishedRevision,
			SourceSequence: sourceSequence, Members: slices.Clone(record.Members), PreserveVectors: true,
		})
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].Key < resolved[j].Key })
	return resolved, true
}

func preparedScopeResolved(plan preparedScopePublication) bool {
	if !plan.changed {
		return true
	}
	for i := range plan.scope.docs {
		if !plan.preserved[i] && !plan.resolved[i] {
			return false
		}
	}
	return true
}

func preparedScopeHasNewVectors(plan preparedScopePublication) bool {
	for i := range plan.scope.docs {
		if plan.resolved[i] && !plan.failedDocs[i] {
			return true
		}
	}
	return false
}

func (w *ContextWorker) coverPreparedScopePlan(ctx context.Context, gen vector.GenerationID, plan preparedScopePublication, res *RunResult) error {
	scope := plan.scope
	failedIDs := failedPreparedDocumentIDs(plan)
	if len(failedIDs) != 0 {
		if err := w.deps.Store.ResetEmbedGen(ctx, failedIDs); err != nil {
			return fmt.Errorf("reset rejected document coverage: %w", err)
		}
	}
	documents, _, _ := successfulPreparedDocuments(plan)
	versions, metadata, err := scopeCASTokens(documents)
	if err != nil {
		return err
	}
	blankVersions := terminalBlankVersions(plan.scope)
	versions = append(versions, blankVersions...)
	sort.Slice(versions, func(i, j int) bool { return versions[i].ID < versions[j].ID })
	if len(versions) != 0 {
		stamped, err := w.deps.Store.SetEmbedGenGroupIfUnchanged(ctx, versions, metadata, int64(gen))
		if err != nil {
			return fmt.Errorf("coverage CAS: %w", err)
		}
		if !stamped {
			return errors.New("coverage CAS missed; source scope changed after assembly and remains uncovered")
		}
		if w.deps.Hooks.AfterCoverage != nil {
			if err := w.deps.Hooks.AfterCoverage(); err != nil {
				return fmt.Errorf("after coverage CAS: %w", err)
			}
		}
	}
	res.Succeeded += len(blankVersions)
	for docIndex, doc := range scope.docs {
		if docIndex < len(plan.failedDocs) && plan.failedDocs[docIndex] {
			res.Failed += len(doc.Versions)
			continue
		}
		res.Succeeded += len(doc.Versions)
	}
	checkpointContextOperationPass(ctx, *res)
	return nil
}

func failedPreparedDocumentIDs(plan preparedScopePublication) []int64 {
	seen := make(map[int64]struct{})
	ids := make([]int64, 0)
	for docIndex, doc := range plan.scope.docs {
		if docIndex >= len(plan.failedDocs) || !plan.failedDocs[docIndex] {
			continue
		}
		for _, version := range doc.Versions {
			if _, duplicate := seen[version.MessageID]; duplicate {
				continue
			}
			seen[version.MessageID] = struct{}{}
			ids = append(ids, version.MessageID)
		}
	}
	slices.Sort(ids)
	return ids
}

func terminalBlankVersions(scope preparedScope) []store.EmbedGenStamp {
	assembled := make(map[int64]struct{})
	for _, doc := range scope.docs {
		for _, version := range doc.Versions {
			assembled[version.MessageID] = struct{}{}
		}
	}
	blank := make([]store.EmbedGenStamp, 0)
	for _, version := range scope.liveVersions {
		if _, ok := assembled[version.MessageID]; ok {
			continue
		}
		blank = append(blank, store.EmbedGenStamp{ID: version.MessageID, LastModified: version.LastModified})
	}
	return blank
}

func successfulPreparedDocuments(plan preparedScopePublication) ([]Document, [][][]float32, []bool) {
	documents := make([]Document, 0, len(plan.scope.docs))
	vectors := make([][][]float32, 0, len(plan.vectors))
	preserved := make([]bool, 0, len(plan.preserved))
	for i, doc := range plan.scope.docs {
		if i < len(plan.failedDocs) && plan.failedDocs[i] {
			continue
		}
		if plan.changed && !plan.preserved[i] && !plan.resolved[i] {
			continue
		}
		documents = append(documents, doc)
		if i < len(plan.vectors) {
			vectors = append(vectors, plan.vectors[i])
		}
		preserved = append(preserved, i < len(plan.preserved) && plan.preserved[i])
	}
	return documents, vectors, preserved
}

func documentInputUTF8Bytes(input DocumentInput, documentPrefixBytes int) int {
	total := 0
	for _, chunk := range input.Chunks {
		total += len(chunk) + voyagePromptReserveUTF8BytesPerChunk + max(0, documentPrefixBytes)
	}
	return total
}

type contextualDocumentEmbedding struct {
	vectors  [][]float32
	tooLarge bool
	// embeddedChunks holds the truncated chunk texts actually sent to the
	// provider when the truncation fallback ran; nil for ordinary results.
	embeddedChunks []string
}

// truncatedContextualDocumentFloorUTF8Bytes is the smallest chunk-text budget
// the singleton truncation fallback will try before declaring a document
// permanently too large. Any real document embeds well before this floor.
const truncatedContextualDocumentFloorUTF8Bytes = 1024

func (w *ContextWorker) embedDocuments(ctx context.Context, inputs []DocumentInput) ([]contextualDocumentEmbedding, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	vectors, err := w.deps.Client.EmbedDocuments(ctx, inputs)
	if len(vectors) > len(inputs) || (err == nil && len(vectors) != len(inputs)) {
		return nil, fmt.Errorf("contextual batched vector document count mismatch: got %d, expected %d", len(vectors), len(inputs))
	}
	out := make([]contextualDocumentEmbedding, len(vectors))
	for i := range vectors {
		out[i].vectors = vectors[i]
	}
	if err == nil {
		return out, nil
	}
	var sizeErr *voyageSizeError
	if !errors.Is(err, ErrDocumentTooLarge) && !errors.As(err, &sizeErr) {
		return out, fmt.Errorf("embed contextual documents: %w", err)
	}
	if len(out) != 0 {
		tail, tailErr := w.embedDocuments(ctx, inputs[len(out):])
		return slices.Concat(out, tail), tailErr
	}
	if len(inputs) == 1 {
		return w.embedTruncatedDocument(ctx, inputs[0])
	}
	middle := len(inputs) / 2
	left, err := w.embedDocuments(ctx, inputs[:middle])
	if err != nil {
		return nil, err
	}
	right, err := w.embedDocuments(ctx, inputs[middle:])
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

// embedTruncatedDocument retries one size-rejected document with progressively
// halved chunk text. The provider limit is token-based while local packing is
// byte-based, so a token-dense document can pass packing yet still be rejected;
// without this fallback its members would stay uncovered and block activation
// forever. Truncation preserves the chunk count, so vectors still align with
// the assembled chunk rows and the published revision (derived from source
// content) converges normally.
func (w *ContextWorker) embedTruncatedDocument(ctx context.Context, input DocumentInput) ([]contextualDocumentEmbedding, error) {
	budget := 0
	for _, chunk := range input.Chunks {
		budget += len(chunk)
	}
	for budget/2 >= truncatedContextualDocumentFloorUTF8Bytes {
		budget /= 2
		truncated := truncateDocumentInput(input, budget)
		vectors, err := w.deps.Client.EmbedDocuments(ctx, []DocumentInput{truncated})
		if err == nil {
			if len(vectors) != 1 || len(vectors[0]) != len(input.Chunks) {
				return nil, fmt.Errorf("contextual truncated document chunk count mismatch: got %d, expected %d",
					len(vectors), len(input.Chunks))
			}
			return []contextualDocumentEmbedding{{vectors: vectors[0], embeddedChunks: truncated.Chunks}}, nil
		}
		var sizeErr *voyageSizeError
		if !errors.Is(err, ErrDocumentTooLarge) && !errors.As(err, &sizeErr) {
			return nil, fmt.Errorf("embed truncated contextual document: %w", err)
		}
	}
	return []contextualDocumentEmbedding{{tooLarge: true}}, nil
}

// truncateDocumentInput trims each chunk's text to its proportional share of
// budget bytes, rune-safe, keeping every non-empty chunk non-empty so the
// provider never sees an empty chunk.
func truncateDocumentInput(input DocumentInput, budget int) DocumentInput {
	total := 0
	for _, chunk := range input.Chunks {
		total += len(chunk)
	}
	out := DocumentInput{Chunks: make([]string, len(input.Chunks))}
	for i, chunk := range input.Chunks {
		text := chunk
		if total > 0 {
			text = utf8Prefix(chunk, len(chunk)*budget/total)
		}
		if text == "" && chunk != "" {
			_, size := utf8.DecodeRuneInString(chunk)
			text = chunk[:size]
		}
		out.Chunks[i] = text
	}
	return out
}

// applyTruncatedEmbedding rewrites a document's owned chunks to describe the
// truncated text that was actually embedded, so published offsets never cover
// text the vector never saw. Chunks follow the trimOwnedChunkText layout: the
// trailing SourceCharEnd-SourceCharStart runes of Text are the source span and
// any leading runes are out-of-band context, so a prefix cut removes source
// runes from the span's tail first.
func applyTruncatedEmbedding(doc *Document, embedded []string) {
	for i := range doc.Chunks {
		if i >= len(embedded) || embedded[i] == doc.Chunks[i].Text {
			continue
		}
		chunk := &doc.Chunks[i]
		sourceRunes := max(0, chunk.SourceCharEnd-chunk.SourceCharStart)
		prefixRunes := max(0, utf8.RuneCountInString(chunk.Text)-sourceRunes)
		keptSource := min(sourceRunes, max(0, utf8.RuneCountInString(embedded[i])-prefixRunes))
		chunk.Text = embedded[i]
		chunk.SourceCharEnd = chunk.SourceCharStart + keptSource
		chunk.Truncated = true
	}
}

func sameDocumentRevisions(existing []vector.DocumentRecord, desired []Document) bool {
	if len(existing) != len(desired) {
		return false
	}
	byKey := make(map[string]vector.DocumentRecord, len(existing))
	for _, record := range existing {
		byKey[record.Key] = record
	}
	for _, doc := range desired {
		record, ok := byKey[doc.Key]
		if !ok || record.PublishedRevision != doc.Revision || !slices.Equal(record.Members, documentMembers(doc)) {
			return false
		}
	}
	return true
}

func documentFenceBehind(existing []vector.DocumentRecord, sourceSequence int64) bool {
	if len(existing) == 0 {
		// An empty desired scope still needs a durable fence so a delayed older
		// publication cannot recreate documents after a delete or move.
		return true
	}
	for _, record := range existing {
		if record.SourceSequence < sourceSequence {
			return true
		}
	}
	return false
}

func buildDocumentPublication(docs []Document, vectors [][][]float32, preserved []bool) ([]vector.DocumentPublication, []vector.Chunk, error) {
	if len(vectors) != len(docs) || len(preserved) != len(docs) {
		return nil, nil, fmt.Errorf("contextual vector document count mismatch: got %d, expected %d", len(vectors), len(docs))
	}
	publications := documentPublications(docs)
	chunks := make([]vector.Chunk, 0)
	for i, doc := range docs {
		if preserved[i] {
			publications[i].PreserveVectors = true
			continue
		}
		if len(vectors[i]) != len(doc.Chunks) {
			return nil, nil, fmt.Errorf("contextual vector chunk count mismatch for %q: got %d, expected %d", doc.Key, len(vectors[i]), len(doc.Chunks))
		}
		sourceLengths := make(map[int64]int)
		for _, owned := range doc.Chunks {
			sourceLengths[owned.MessageID] = max(sourceLengths[owned.MessageID], owned.SourceCharEnd)
		}
		for j, owned := range doc.Chunks {
			chunks = append(chunks, vector.Chunk{MessageID: owned.MessageID,
				ChunkIndex: owned.ChunkIndex, Vector: vectors[i][j],
				SourceCharLen: sourceLengths[owned.MessageID], ChunkCharStart: owned.SourceCharStart,
				ChunkCharEnd: owned.SourceCharEnd, SourceBasis: owned.SourceBasis, Truncated: owned.Truncated})
		}
	}
	return publications, chunks, nil
}

func documentPublications(docs []Document) []vector.DocumentPublication {
	publications := make([]vector.DocumentPublication, len(docs))
	for i, doc := range docs {
		publications[i] = vector.DocumentPublication{Key: doc.Key, Kind: doc.Kind,
			Revision: doc.Revision, SourceSequence: doc.SourceSequence, Members: documentMembers(doc)}
	}
	return publications
}

func documentMembers(doc Document) []int64 {
	members := make([]int64, len(doc.Versions))
	for i, version := range doc.Versions {
		members[i] = version.MessageID
	}
	return members
}

func scopeCASTokens(docs []Document) ([]store.EmbedGenStamp, store.EmbedGenMetadataVersion, error) {
	versions := make([]store.EmbedGenStamp, 0)
	seen := make(map[int64]struct{})
	var metadata store.EmbedGenMetadataVersion
	for _, doc := range docs {
		candidate := store.EmbedGenMetadataVersion{ConversationID: doc.MetadataVersion.ConversationID, Digest: doc.MetadataVersion.Digest}
		if candidate.ConversationID != 0 {
			if metadata.ConversationID != 0 && metadata != candidate {
				return nil, metadata, errors.New("contextual scope contains inconsistent metadata versions")
			}
			metadata = candidate
		}
		for _, version := range doc.Versions {
			if _, duplicate := seen[version.MessageID]; duplicate {
				return nil, metadata, fmt.Errorf("contextual scope contains duplicate member %d", version.MessageID)
			}
			seen[version.MessageID] = struct{}{}
			versions = append(versions, store.EmbedGenStamp{ID: version.MessageID, LastModified: version.LastModified})
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].ID < versions[j].ID })
	return versions, metadata, nil
}

func (w *ContextWorker) reconcile(ctx context.Context, gen vector.GenerationID, res *RunResult) error {
	progress, err := w.deps.Publisher.GetDocumentProgress(ctx, gen)
	if err != nil {
		return err
	}
	cursor := progress.ReconcileCursor
	if strings.HasPrefix(cursor, "done:") {
		return nil
	}
	if !strings.HasPrefix(cursor, "orphan:") {
		var after int64
		var resumeScope string
		// A chat-day scope can span many message-ID pages. Assemble it once per
		// reconciliation run; publication remains idempotent across crash resume.
		seenChatScopes := make(map[string]struct{})
		if sourceCursor, ok := strings.CutPrefix(cursor, "source:"); ok {
			sourceCursor, resumeScope, _ = strings.Cut(sourceCursor, "|")
			after, err = strconv.ParseInt(sourceCursor, 10, 64)
			if err != nil {
				return fmt.Errorf("parse contextual source cursor %q: %w", cursor, err)
			}
		}
		for {
			pageAfter := after
			snapshot, err := BeginSourceSnapshot(ctx, w.source)
			if err != nil {
				return err
			}
			scopes, lastID, more, rowCount, err := snapshot.sourceScopesAfter(
				ctx, after, w.deps.ReconcileBatchSize, w.deps.BuildScope,
			)
			if err != nil {
				_ = snapshot.Close()
				return err
			}
			uniqueScopes := scopes[:0]
			for _, scope := range scopes {
				if scope.selector.Kind == contextualChatMessageType {
					if _, seen := seenChatScopes[scope.key]; seen {
						continue
					}
					seenChatScopes[scope.key] = struct{}{}
				}
				uniqueScopes = append(uniqueScopes, scope)
			}
			if resumeScope != "" {
				first := sort.Search(len(uniqueScopes), func(i int) bool { return uniqueScopes[i].key > resumeScope })
				uniqueScopes = uniqueScopes[first:]
			}
			prepared, err := w.prepareScopes(ctx, snapshot, uniqueScopes)
			if err != nil {
				_ = snapshot.Close()
				return err
			}
			if err := w.closeSnapshot(snapshot); err != nil {
				return err
			}
			if w.deps.Hooks.AfterSourcePage != nil {
				if err := w.deps.Hooks.AfterSourcePage(rowCount); err != nil {
					return fmt.Errorf("after contextual source page: %w", err)
				}
			}
			processed, publishErr := w.publishPreparedScopes(ctx, gen, prepared, res)
			if processed > 0 {
				partial := "source:" + strconv.FormatInt(pageAfter, 10) + "|" + prepared[processed-1].key
				if err := w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen, partial); err != nil {
					return err
				}
			}
			if publishErr != nil {
				return fmt.Errorf("reconcile source scopes: %w", publishErr)
			}
			resumeScope = ""
			if lastID != 0 {
				after = lastID
				if err := w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen, "source:"+strconv.FormatInt(after, 10)); err != nil {
					return err
				}
			}
			if !more {
				break
			}
		}
		cursor = "orphan:"
		if err := w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen, cursor); err != nil {
			return err
		}
	}
	afterKey, pageEndKey, resumeScope := decodeOrphanReconcileCursor(cursor)
	for {
		pageAfterKey := afterKey
		records, err := w.deps.Publisher.ListDocumentsAfter(ctx, gen, afterKey, w.deps.ReconcileBatchSize)
		if err != nil {
			return fmt.Errorf("list reconciliation ledger page: %w", err)
		}
		if len(records) == 0 {
			break
		}
		if pageEndKey == "" {
			pageEndKey = records[len(records)-1].Key
		}
		pageRecordCount := sort.Search(len(records), func(i int) bool { return records[i].Key > pageEndKey })
		records = records[:pageRecordCount]
		if len(records) == 0 {
			afterKey = pageEndKey
			pageEndKey = ""
			resumeScope = ""
			if err := w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen, "orphan:"+afterKey); err != nil {
				return err
			}
			continue
		}
		scopeMap := make(map[string]contextScope)
		for _, record := range records {
			scopeMap[record.ScopeKey] = contextScope{key: record.ScopeKey, selector: selectorForScopeKey(record.ScopeKey)}
		}
		scopes := make([]contextScope, 0, len(scopeMap))
		for _, scope := range scopeMap {
			scopes = append(scopes, scope)
		}
		sort.Slice(scopes, func(i, j int) bool { return scopes[i].key < scopes[j].key })
		if resumeScope != "" {
			first := sort.Search(len(scopes), func(i int) bool { return scopes[i].key > resumeScope })
			scopes = scopes[first:]
		}
		snapshot, err := BeginSourceSnapshot(ctx, w.source)
		if err != nil {
			return err
		}
		prepared, err := w.prepareScopes(ctx, snapshot, scopes)
		if err != nil {
			_ = snapshot.Close()
			return err
		}
		if err := w.closeSnapshot(snapshot); err != nil {
			return err
		}
		processed, publishErr := w.publishPreparedScopes(ctx, gen, prepared, res)
		if processed > 0 {
			partial := encodeOrphanReconcileCursor(pageAfterKey, pageEndKey, prepared[processed-1].key)
			if err := w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen, partial); err != nil {
				return err
			}
		}
		if publishErr != nil {
			return fmt.Errorf("reconcile ledger scopes: %w", publishErr)
		}
		afterKey = pageEndKey
		pageEndKey = ""
		resumeScope = ""
		if err := w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen, "orphan:"+afterKey); err != nil {
			return err
		}
	}
	latest, err := w.deps.Store.LatestEmbeddingChangeSequence(ctx)
	if err != nil {
		return err
	}
	return w.deps.Publisher.SetDocumentReconcileCursor(ctx, gen, "done:"+strconv.FormatInt(latest, 10))
}

func (w *ContextWorker) convergence(ctx context.Context, gen vector.GenerationID) (ContextConvergence, error) {
	latest, err := w.deps.Store.LatestEmbeddingChangeSequence(ctx)
	if err != nil {
		return ContextConvergence{}, err
	}
	progress, err := w.deps.Publisher.GetDocumentProgress(ctx, gen)
	if err != nil {
		return ContextConvergence{}, err
	}
	result := ContextConvergence{SourceSequence: latest, ConsumedSequence: progress.ChangeSequence,
		ReconcileCursor: progress.ReconcileCursor, ReconcileComplete: strings.HasPrefix(progress.ReconcileCursor, "done:")}
	// Every publication path reaches coverage through an atomic CAS before its
	// watermark or reconciliation cursor advances. The shared activation checker
	// performs the archive-wide coverage count; repeating it here made every idle
	// worker tick scan the full source table again.
	result.Converged = result.ConsumedSequence == result.SourceSequence && result.ReconcileComplete && !result.CoverageMissing
	return result, nil
}

func (s SourceSnapshot) embeddingChanges(ctx context.Context, after int64, limit int) ([]store.EmbeddingChange, error) {
	if s.state == nil {
		return nil, ErrSourceSnapshotClosed
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if s.state.closed {
		return nil, ErrSourceSnapshotClosed
	}
	rows, err := s.state.tx.QueryContext(ctx, s.state.rebind(`SELECT sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id, new_conversation_id, old_sent_at, new_sent_at, participant_id FROM embedding_changes WHERE sequence > ? ORDER BY sequence LIMIT ?`), after, limit)
	if err != nil {
		return nil, fmt.Errorf("scan embedding changes in source snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]store.EmbeddingChange, 0, limit)
	for rows.Next() {
		var change store.EmbeddingChange
		if err := rows.Scan(&change.Sequence, &change.Kind, &change.MessageID, &change.OldMessageType, &change.NewMessageType, &change.OldConversationID, &change.NewConversationID, &change.OldSentAt, &change.NewSentAt, &change.ParticipantID); err != nil {
			return nil, err
		}
		out = append(out, change)
	}
	return out, rows.Err()
}

func (s SourceSnapshot) scopesForChanges(
	ctx context.Context, changes []store.EmbeddingChange, buildScope vector.BuildScope,
) ([]contextScope, error) {
	byKey := make(map[string]contextScope)
	boundaryConversations := make(map[int64]struct{})
	add := func(scope contextScope) {
		if scope.key != "" {
			byKey[scope.key] = scope
		}
	}
	for _, change := range changes {
		if change.MessageID.Valid {
			id := change.MessageID.Int64
			oldChat := change.OldMessageType.Valid && change.OldMessageType.String == contextualChatMessageType
			newChat := change.NewMessageType.Valid && change.NewMessageType.String == contextualChatMessageType
			var oldChatKey, newChatKey string
			row, found, err := s.MessageMeta(ctx, id)
			if err != nil {
				return nil, err
			}
			if buildScope.IsEmpty() || (change.OldMessageType.Valid && messageTypeInScope(buildScope, change.OldMessageType.String)) {
				oldScope, ok, err := persistedMessageScope(
					id, change.OldMessageType, change.OldConversationID, change.OldSentAt, true,
				)
				if err != nil {
					return nil, fmt.Errorf("embedding change %d old scope: %w", change.Sequence, err)
				}
				if ok {
					add(oldScope)
					if oldChat {
						oldChatKey = oldScope.key
					}
				}
			}
			if buildScope.IsEmpty() || (change.NewMessageType.Valid && messageTypeInScope(buildScope, change.NewMessageType.String)) {
				newScope, ok, err := persistedMessageScope(
					id, change.NewMessageType, change.NewConversationID, change.NewSentAt, true,
				)
				if err != nil {
					return nil, fmt.Errorf("embedding change %d new scope: %w", change.Sequence, err)
				}
				if ok {
					add(newScope)
					if newChat {
						newChatKey = newScope.key
					}
				}
			}
			if found && messageTypeInScope(buildScope, row.MessageType) {
				add(liveMessageScope(row))
			} else if !change.OldMessageType.Valid && !change.NewMessageType.Valid {
				return nil, fmt.Errorf("embedding change %d for missing message %d has no persisted message type", change.Sequence, id)
			}
			if messageTypeInScope(buildScope, contextualChatMessageType) &&
				(oldChat != newChat || oldChatKey != newChatKey) {
				for _, conversation := range []sql.NullInt64{change.OldConversationID, change.NewConversationID} {
					if conversation.Valid {
						boundaryConversations[conversation.Int64] = struct{}{}
					}
				}
			}
		}
	}
	conversationIDs := make([]int64, 0, len(boundaryConversations))
	for conversationID := range boundaryConversations {
		conversationIDs = append(conversationIDs, conversationID)
	}
	slices.Sort(conversationIDs)
	for _, conversationID := range conversationIDs {
		scopes, err := s.latestChatBlockScopes(ctx, conversationID, 2)
		if err != nil {
			return nil, err
		}
		for _, scope := range scopes {
			add(scope)
		}
	}
	out := make([]contextScope, 0, len(byKey))
	for _, scope := range byKey {
		out = append(out, scope)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, nil
}

func (s SourceSnapshot) latestChatBlockScopes(
	ctx context.Context, conversationID int64, blockLimit int,
) ([]contextScope, error) {
	if s.state == nil {
		return nil, ErrSourceSnapshotClosed
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if s.state.closed {
		return nil, ErrSourceSnapshotClosed
	}
	if conversationID == 0 || blockLimit <= 0 {
		return nil, nil
	}
	rows, err := s.state.tx.QueryContext(ctx, s.state.rebind(`
		SELECT m.id, COALESCE(m.sent_at, m.received_at, m.internal_date)
		  FROM messages m
		  JOIN message_bodies mb ON mb.message_id = m.id
		 WHERE m.conversation_id = ?
		   AND m.message_type = 'beeper'
		   AND m.deleted_at IS NULL
		   AND m.deleted_from_source_at IS NULL
		 ORDER BY m.id DESC
		 LIMIT ?`), conversationID, blockLimit*chatScopeMaxMessages)
	if err != nil {
		return nil, fmt.Errorf("read latest chat blocks for conversation %d: %w", conversationID, err)
	}
	defer func() { _ = rows.Close() }()
	blocks := make(map[int64]struct{}, blockLimit)
	byKey := make(map[string]contextScope)
	for rows.Next() {
		var messageID int64
		var rawTimestamp any
		if err := rows.Scan(&messageID, &rawTimestamp); err != nil {
			return nil, fmt.Errorf("scan latest chat block for conversation %d: %w", conversationID, err)
		}
		blockStart := chatBlockStart(messageID)
		if _, known := blocks[blockStart]; !known {
			if len(blocks) == blockLimit {
				break
			}
			blocks[blockStart] = struct{}{}
		}
		timestamp, _, err := contextTimestamp(rawTimestamp)
		if err != nil {
			return nil, err
		}
		scope := chatBlockContextScope(conversationID, timestamp, blockStart)
		byKey[scope.key] = scope
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest chat blocks for conversation %d: %w", conversationID, err)
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]contextScope, len(keys))
	for i, key := range keys {
		out[i] = byKey[key]
	}
	return out, nil
}

func isMetadataFanoutChange(change store.EmbeddingChange) bool {
	switch change.Kind {
	case store.EmbeddingChangeConversationTitle,
		store.EmbeddingChangeConversationParticipant,
		store.EmbeddingChangeParticipantDisplayName:
		return true
	default:
		return false
	}
}

func metadataFanoutIdentity(change store.EmbeddingChange) string {
	switch change.Kind {
	case store.EmbeddingChangeConversationTitle, store.EmbeddingChangeConversationParticipant:
		conversations := make([]int64, 0, 2)
		for _, candidate := range []sql.NullInt64{change.OldConversationID, change.NewConversationID} {
			if candidate.Valid && !slices.Contains(conversations, candidate.Int64) {
				conversations = append(conversations, candidate.Int64)
			}
		}
		slices.Sort(conversations)
		parts := make([]string, len(conversations))
		for i, conversationID := range conversations {
			parts[i] = strconv.FormatInt(conversationID, 10)
		}
		return "conversation:" + strings.Join(parts, ",")
	case store.EmbeddingChangeParticipantDisplayName:
		if change.ParticipantID.Valid {
			return "participant:" + strconv.FormatInt(change.ParticipantID.Int64, 10)
		}
	case store.EmbeddingChangeMessageInsert,
		store.EmbeddingChangeMessageUpdate,
		store.EmbeddingChangeMessageDelete,
		store.EmbeddingChangeMessageBody:
		return ""
	}
	return ""
}

func encodeJournalCursor(sequence int64, scopeKey string) string {
	return strconv.FormatInt(sequence, 10) + "|" + scopeKey
}

func decodeOrphanReconcileCursor(cursor string) (afterKey, pageEndKey, resumeScope string) {
	parts := strings.SplitN(strings.TrimPrefix(cursor, "orphan:"), "|", 3)
	afterKey = parts[0]
	if len(parts) == 3 {
		return afterKey, parts[1], parts[2]
	}
	// The former two-field cursor did not retain the original page boundary.
	// Restart that page so records pulled forward after tombstones cannot be
	// filtered out by a stale scope-key cursor.
	return afterKey, "", ""
}

func encodeOrphanReconcileCursor(afterKey, pageEndKey, resumeScope string) string {
	return "orphan:" + afterKey + "|" + pageEndKey + "|" + resumeScope
}

func journalCursorForSequence(cursor string, sequence int64) string {
	prefix := strconv.FormatInt(sequence, 10) + "|"
	value, ok := strings.CutPrefix(cursor, prefix)
	if !ok {
		return ""
	}
	return value
}

func (s SourceSnapshot) metadataScopesAfter(
	ctx context.Context,
	change store.EmbeddingChange,
	buildScope vector.BuildScope,
	afterKey string,
	limit int,
) ([]contextScope, bool, error) {
	if !messageTypeInScope(buildScope, contextualChatMessageType) {
		return nil, false, nil
	}
	if limit <= 0 {
		limit = 1
	}
	dayExpr := `COALESCE(strftime('%Y-%m-%d', COALESCE(m.sent_at,m.received_at,m.internal_date)), 'undated')`
	blockStartExpr := `(((m.id - 1) / ` + strconv.Itoa(chatScopeMaxMessages) + `) * ` +
		strconv.Itoa(chatScopeMaxMessages) + ` + 1)`
	if s.state != nil && s.state.postgres {
		dayExpr = `COALESCE(TO_CHAR(COALESCE(m.sent_at,m.received_at,m.internal_date) AT TIME ZONE 'UTC', 'YYYY-MM-DD'), 'undated')`
	}
	where := []string{
		`m.message_type = 'beeper'`,
		`m.deleted_at IS NULL`,
		`m.deleted_from_source_at IS NULL`,
	}
	args := make([]any, 0)
	// Narrow metadata fan-out by the account dimension up front: an
	// out-of-scope conversation would only be tombstoned downstream, and a
	// title change on a large excluded archive must not enumerate (and
	// fence) every one of its chat blocks.
	if len(buildScope.SourceIDs) > 0 {
		placeholders := make([]string, len(buildScope.SourceIDs))
		for i, sourceID := range buildScope.SourceIDs {
			placeholders[i] = "?"
			args = append(args, sourceID)
		}
		where = append(where, `m.source_id IN (`+strings.Join(placeholders, ",")+`)`)
	}
	// Mutable remote titles and participant metadata must not invalidate an
	// entire chat archive. Limit metadata-only fanout to the newest stable
	// message-ID block in each affected conversation. Older blocks use the
	// assembler's metadata-independent revision, so metadata-only changes and
	// full backstops preserve their published vectors.
	latestBlockStartExpr := `((((
		SELECT MAX(recent.id)
		  FROM messages recent
		  JOIN message_bodies recent_mb ON recent_mb.message_id = recent.id
		 WHERE recent.conversation_id = m.conversation_id
		   AND recent.message_type = 'beeper'
		   AND recent.deleted_at IS NULL
		   AND recent.deleted_from_source_at IS NULL
	) - 1) / ` + strconv.Itoa(chatScopeMaxMessages) + `) * ` +
		strconv.Itoa(chatScopeMaxMessages) + ` + 1)`
	where = append(where, `m.id >= `+latestBlockStartExpr)
	switch change.Kind {
	case store.EmbeddingChangeConversationTitle, store.EmbeddingChangeConversationParticipant:
		seen := make(map[int64]struct{})
		conversations := make([]int64, 0, 2)
		for _, candidate := range []sql.NullInt64{change.OldConversationID, change.NewConversationID} {
			if !candidate.Valid {
				continue
			}
			if _, ok := seen[candidate.Int64]; ok {
				continue
			}
			seen[candidate.Int64] = struct{}{}
			conversations = append(conversations, candidate.Int64)
		}
		if len(conversations) == 0 {
			return nil, false, nil
		}
		placeholders := make([]string, len(conversations))
		for i, conversation := range conversations {
			placeholders[i] = "?"
			args = append(args, conversation)
		}
		where = append(where, `m.conversation_id IN (`+strings.Join(placeholders, ",")+`)`)
	case store.EmbeddingChangeParticipantDisplayName:
		if !change.ParticipantID.Valid {
			return nil, false, nil
		}
		where = append(where, `(m.sender_id = ? OR EXISTS (
			SELECT 1 FROM conversation_participants cp
			 WHERE cp.conversation_id = m.conversation_id AND cp.participant_id = ?))`)
		args = append(args, change.ParticipantID.Int64, change.ParticipantID.Int64)
	default:
		return nil, false, nil
	}
	query := fmt.Sprintf(`
		SELECT conversation_id, day_key, block_start, scope_key FROM (
			SELECT conversation_id, day_key, block_start,
			       'chat:' || CAST(conversation_id AS TEXT) || ':' || day_key ||
			       CASE WHEN block_start = 1 THEN '' ELSE ':' || CAST(block_start AS TEXT) END AS scope_key
			  FROM (
				SELECT m.conversation_id AS conversation_id, %s AS day_key, %s AS block_start
				  FROM messages m
				  JOIN message_bodies mb ON mb.message_id = m.id
				 WHERE %s
				 GROUP BY m.conversation_id, %s, %s
			  ) grouped
		) keyed
		WHERE scope_key > ?
		ORDER BY scope_key
		LIMIT ?`, dayExpr, blockStartExpr, strings.Join(where, " AND "), dayExpr, blockStartExpr)
	args = append(args, afterKey, limit+1)
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("page contextual metadata scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	scopes := make([]contextScope, 0, limit+1)
	for rows.Next() {
		var conversationID, blockStart int64
		var dayKey, scopeKey string
		if err := rows.Scan(&conversationID, &dayKey, &blockStart, &scopeKey); err != nil {
			return nil, false, err
		}
		if dayKey == "undated" {
			scopes = append(scopes, chatBlockContextScope(conversationID, time.Time{}, blockStart))
			continue
		}
		day, err := time.Parse("2006-01-02", dayKey)
		if err != nil {
			return nil, false, fmt.Errorf("parse contextual metadata day %q for %q: %w", dayKey, scopeKey, err)
		}
		scopes = append(scopes, chatBlockContextScope(conversationID, day, blockStart))
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(scopes) > limit
	if more {
		scopes = scopes[:limit]
	}
	return scopes, more, nil
}

func persistedMessageScope(
	messageID int64,
	messageType sql.NullString,
	conversation sql.NullInt64,
	sentAt sql.NullTime,
	includeOrdinary bool,
) (contextScope, bool, error) {
	if !messageType.Valid {
		return contextScope{}, false, nil
	}
	switch messageType.String {
	case contextualChatMessageType:
		if !conversation.Valid {
			return contextScope{}, false, errors.New("beeper scope has no conversation")
		}
		timestamp := time.Time{}
		if sentAt.Valid {
			timestamp = sentAt.Time
		}
		return chatMessageContextScope(conversation.Int64, timestamp, messageID), true, nil
	case "meeting_transcript":
		key := "meeting:" + strconv.FormatInt(messageID, 10)
		return contextScope{key: key, selector: AffectedScope{Kind: messageType.String, MessageID: messageID}}, true, nil
	default:
		if !includeOrdinary {
			return contextScope{}, false, nil
		}
		key := "message:" + strconv.FormatInt(messageID, 10)
		return contextScope{key: key, selector: AffectedScope{Kind: messageType.String, MessageID: messageID}}, true, nil
	}
}

func liveMessageScope(row AssemblyMessage) contextScope {
	switch row.MessageType {
	case contextualChatMessageType:
		return chatMessageContextScope(row.ConversationID, row.SentAt, row.ID)
	case "meeting_transcript":
		key := "meeting:" + strconv.FormatInt(row.ID, 10)
		return contextScope{key: key, selector: AffectedScope{Kind: row.MessageType, MessageID: row.ID}}
	default:
		key := "message:" + strconv.FormatInt(row.ID, 10)
		return contextScope{key: key, selector: AffectedScope{Kind: row.MessageType, MessageID: row.ID}}
	}
}

func chatDayContextScope(conversationID int64, timestamp time.Time) contextScope {
	return chatBlockContextScope(conversationID, timestamp, 1)
}

// ChatMessageScope returns the production publication selector for one chat
// message. Evaluation code uses this boundary so it cannot drift back to an
// unbounded day selector.
func ChatMessageScope(conversationID int64, timestamp time.Time, messageID int64) AffectedScope {
	return chatMessageContextScope(conversationID, timestamp, messageID).selector
}

func chatMessageContextScope(conversationID int64, timestamp time.Time, messageID int64) contextScope {
	blockStart := chatBlockStart(messageID)
	return chatBlockContextScope(conversationID, timestamp, blockStart)
}

func chatBlockStart(messageID int64) int64 {
	if messageID <= 0 {
		return 1
	}
	return ((messageID - 1) / chatScopeMaxMessages * chatScopeMaxMessages) + 1
}

func chatBlockContextScope(conversationID int64, timestamp time.Time, blockStart int64) contextScope {
	if blockStart <= 0 {
		blockStart = 1
	}
	blockEnd := blockStart + chatScopeMaxMessages
	dayKey := chatDay(timestamp)
	key := "chat:" + strconv.FormatInt(conversationID, 10) + ":" + dayKey
	if blockStart != 1 {
		key += ":" + strconv.FormatInt(blockStart, 10)
	}
	if timestamp.IsZero() {
		return contextScope{
			key: key,
			selector: AffectedScope{
				Kind: contextualChatMessageType, ConversationID: conversationID, Undated: true,
				MessageIDStart: blockStart, MessageIDEnd: blockEnd,
			},
		}
	}
	day := timestamp.UTC()
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	return contextScope{key: key, selector: AffectedScope{
		Kind: contextualChatMessageType, ConversationID: conversationID,
		UTCStart: start, UTCEnd: start.Add(24 * time.Hour),
		MessageIDStart: blockStart, MessageIDEnd: blockEnd,
	}}
}

func scopeKeyForSelector(scope AffectedScope) string {
	return chatBlockContextScope(scope.ConversationID, scope.UTCStart, scope.MessageIDStart).key
}

func (s SourceSnapshot) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s.state == nil {
		return nil, ErrSourceSnapshotClosed
	}
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	if s.state.closed {
		return nil, ErrSourceSnapshotClosed
	}
	return s.state.tx.QueryContext(ctx, s.state.rebind(query), args...) //nolint:sqlclosecheck // The caller owns the returned rows.
}

func (s SourceSnapshot) sourceScopesAfter(
	ctx context.Context, after int64, limit int, buildScope vector.BuildScope,
) ([]contextScope, int64, bool, int, error) {
	query := `SELECT m.id,COALESCE(m.message_type,''),m.conversation_id,COALESCE(m.sent_at,m.received_at,m.internal_date) FROM messages m WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL AND m.id > ?`
	args := []any{after}
	// Each scope dimension narrows independently: gating the type filter on
	// IsEmpty would render `IN ()` for a source-only scope, and vice versa.
	if len(buildScope.MessageTypes) > 0 {
		placeholders := make([]string, len(buildScope.MessageTypes))
		for i, messageType := range buildScope.MessageTypes {
			placeholders[i] = "?"
			args = append(args, messageType)
		}
		query += ` AND m.message_type IN (` + strings.Join(placeholders, ",") + `)`
	}
	if len(buildScope.SourceIDs) > 0 {
		placeholders := make([]string, len(buildScope.SourceIDs))
		for i, sourceID := range buildScope.SourceIDs {
			placeholders[i] = "?"
			args = append(args, sourceID)
		}
		query += ` AND m.source_id IN (` + strings.Join(placeholders, ",") + `)`
	}
	query += ` ORDER BY m.id LIMIT ?`
	args = append(args, limit)
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, 0, false, 0, err
	}
	defer func() { _ = rows.Close() }()
	byKey := make(map[string]contextScope)
	lastID := int64(0)
	rowCount := 0
	for rows.Next() {
		var id, conversation int64
		var kind string
		var rawTimestamp any
		if err := rows.Scan(&id, &kind, &conversation, &rawTimestamp); err != nil {
			return nil, 0, false, rowCount, err
		}
		lastID = id
		rowCount++
		timestamp, timestampValid, err := contextTimestamp(rawTimestamp)
		if err != nil {
			return nil, 0, false, rowCount, err
		}
		switch kind {
		case contextualChatMessageType:
			if !timestampValid {
				timestamp = time.Time{}
			}
			scope := chatMessageContextScope(conversation, timestamp, id)
			byKey[scope.key] = scope
		case "meeting_transcript":
			key := "meeting:" + strconv.FormatInt(id, 10)
			byKey[key] = contextScope{key: key, selector: AffectedScope{Kind: kind, MessageID: id}}
		default:
			key := "message:" + strconv.FormatInt(id, 10)
			byKey[key] = contextScope{key: key, selector: AffectedScope{Kind: kind, MessageID: id}}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, rowCount, err
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]contextScope, len(keys))
	for i, key := range keys {
		out[i] = byKey[key]
	}
	return out, lastID, rowCount == limit, rowCount, nil
}

func contextTimestamp(value any) (time.Time, bool, error) {
	switch typed := value.(type) {
	case nil:
		return time.Time{}, false, nil
	case time.Time:
		return typed.UTC(), true, nil
	case string:
		for _, layout := range []string{
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05.999-07:00",
			"2006-01-02 15:04:05.999",
			"2006-01-02 15:04:05-07:00",
			"2006-01-02 15:04:05",
			time.RFC3339Nano,
		} {
			if parsed, err := time.Parse(layout, typed); err == nil {
				return parsed.UTC(), true, nil
			}
		}
		return time.Time{}, false, fmt.Errorf("parse contextual source timestamp %q", typed)
	case []byte:
		return contextTimestamp(string(typed))
	default:
		return time.Time{}, false, fmt.Errorf("unsupported contextual source timestamp %T", value)
	}
}

func selectorForScopeKey(key string) AffectedScope {
	parts := strings.Split(key, ":")
	if len(parts) == 2 {
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		if parts[0] == "meeting" {
			return AffectedScope{Kind: "meeting_transcript", MessageID: id}
		}
		return AffectedScope{MessageID: id}
	}
	if (len(parts) == 3 || len(parts) == 4) && parts[0] == "chat" {
		conversation, _ := strconv.ParseInt(parts[1], 10, 64)
		blockStart := int64(1)
		if len(parts) == 4 {
			blockStart, _ = strconv.ParseInt(parts[3], 10, 64)
		}
		if blockStart <= 0 {
			return AffectedScope{}
		}
		blockEnd := blockStart + chatScopeMaxMessages
		if parts[2] == "undated" {
			return AffectedScope{
				Kind: contextualChatMessageType, ConversationID: conversation, Undated: true,
				MessageIDStart: blockStart, MessageIDEnd: blockEnd,
			}
		}
		day, err := time.Parse("2006-01-02", parts[2])
		if err == nil {
			return AffectedScope{
				Kind: contextualChatMessageType, ConversationID: conversation,
				UTCStart: day, UTCEnd: day.Add(24 * time.Hour),
				MessageIDStart: blockStart, MessageIDEnd: blockEnd,
			}
		}
	}
	return AffectedScope{}
}
