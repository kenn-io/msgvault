// Package rederive re-computes stored message columns from the verbatim
// provider payloads archived alongside them.
//
// Importers derive columns — body text, snippets, the search index, attachment
// classification — from a provider's payload at import time, then archive that
// payload as-is (message_raw.raw_format). Improving how a column is derived
// therefore leaves every already-archived message stale, and re-syncing repairs
// it only where the provider still holds the message and only at network speed.
//
// A registered pass reads the archive instead: offline, bounded by disk rather
// than an API, and able to repair messages the provider has since dropped.
// Registration lives here rather than in each importer so one command and one
// upgrade path serve all of them.
package rederive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Summary reports what a re-derivation pass changed.
type Summary struct {
	Duration                 time.Duration
	MessagesScanned          int64
	MessageMetadataRewritten int64
	BodiesRewritten          int64
	AttachmentsTagged        int64
	// Undecodable counts archived payloads that could not be parsed; they are
	// left untouched.
	Undecodable int64
	Errors      int64
}

// Add accumulates other into s, except Duration, which the caller owns.
func (s *Summary) Add(other *Summary) {
	if other == nil {
		return
	}
	s.MessagesScanned += other.MessagesScanned
	s.MessageMetadataRewritten += other.MessageMetadataRewritten
	s.BodiesRewritten += other.BodiesRewritten
	s.AttachmentsTagged += other.AttachmentsTagged
	s.Undecodable += other.Undecodable
	s.Errors += other.Errors
}

// Func re-derives every archived message of one source. progress may be nil.
type Func func(ctx context.Context, s *store.Store, sourceID int64, progress func(string)) (*Summary, error)

type entry struct {
	fn      Func
	version string
}

var registry = map[string]entry{}

type crossTypeEntry struct {
	name    string
	version string
	fn      Func
}

// crossTypeRegistry holds passes that apply to every source type, in
// registration order.
var crossTypeRegistry []crossTypeEntry

// Register associates a source type with its re-derivation pass.
//
// version identifies the derivation logic, not the schema: bump it whenever a
// change would produce different output for the same payload, so archives heal
// on their next sync. Registering a source type twice is a programming error
// and panics, since the second pass would silently shadow the first.
func Register(sourceType, version string, fn Func) {
	if _, dup := registry[sourceType]; dup {
		panic(fmt.Sprintf("rederive: source type %q registered twice", sourceType))
	}
	registry[sourceType] = entry{fn: fn, version: version}
}

// RegisterAllSourceTypes adds a pass that runs for every source type, after
// the type's own pass. Each source records it under its own ledger key, so
// version follows the same rules as Register. A duplicate name panics.
func RegisterAllSourceTypes(name, version string, fn Func) {
	for _, e := range crossTypeRegistry {
		if e.name == name {
			panic(fmt.Sprintf("rederive: cross-type pass %q registered twice", name))
		}
	}
	crossTypeRegistry = append(crossTypeRegistry, crossTypeEntry{name: name, version: version, fn: fn})
}

// HasPass reports whether any pass, typed or cross-type, applies to a source
// type.
func HasPass(sourceType string) bool {
	_, ok := registry[sourceType]
	return ok || len(crossTypeRegistry) > 0
}

// Lookup returns the pass registered for a source type.
func Lookup(sourceType string) (Func, string, bool) {
	e, ok := registry[sourceType]
	return e.fn, e.version, ok
}

// SourceTypes lists every registered source type, sorted for stable output.
func SourceTypes() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LedgerKey names the applied_migrations entry recording that a source has
// been re-derived at a given version. Keying per source (rather than per source
// type) lets one account heal without blocking or repeating the others.
func LedgerKey(sourceType, identifier, version string) string {
	return fmt.Sprintf("rederive:%s:%s:%s", sourceType, identifier, version)
}

// passLedgerKey names the ledger entry of a cross-type pass for one source.
func passLedgerKey(name, sourceType, identifier, version string) string {
	return fmt.Sprintf("rederive:%s:%s:%s:%s", name, sourceType, identifier, version)
}

type pass struct {
	fn        Func
	ledgerKey string
}

// passesFor lists the typed pass, if any, then every cross-type pass.
func passesFor(sourceType, identifier string) []pass {
	var out []pass
	if e, ok := registry[sourceType]; ok {
		out = append(out, pass{fn: e.fn, ledgerKey: LedgerKey(sourceType, identifier, e.version)})
	}
	for _, e := range crossTypeRegistry {
		out = append(out, pass{fn: e.fn, ledgerKey: passLedgerKey(e.name, sourceType, identifier, e.version)})
	}
	return out
}

// Run executes every pass of a source and records each in the ledger, whether
// or not the ledger already held it. This is the on-demand path; recording
// still matters here, or the next sync would repeat a full scan of an archive
// that is already current.
//
// A pass is recorded only on success, so a failed attempt is retried later
// rather than being silently marked done.
func Run(
	ctx context.Context, s *store.Store, sourceType, identifier string, sourceID int64, progress func(string),
) (*Summary, error) {
	if !HasPass(sourceType) {
		return nil, fmt.Errorf("no re-derivation pass registered for source type %q", sourceType)
	}
	total := &Summary{}
	for _, p := range passesFor(sourceType, identifier) {
		sum, err := runOne(ctx, s, p, sourceID, progress)
		addSummary(total, sum)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func addSummary(total, sum *Summary) {
	if sum != nil {
		total.Add(sum)
		total.Duration += sum.Duration
	}
}

func runOne(ctx context.Context, s *store.Store, p pass, sourceID int64, progress func(string)) (*Summary, error) {
	sum, err := p.fn(ctx, s, sourceID, progress)
	if err != nil {
		// A pass can fail after earlier message transactions committed. Make
		// those partial authoritative writes visible to cache maintenance even
		// though the repair remains retryable.
		if sum != nil && (sum.MessagesScanned > 0 || sum.Errors > 0) {
			return sum, errors.Join(err, s.AdvanceDerivedDataRevision())
		}
		return sum, err
	}
	if sum != nil && sum.Errors > 0 {
		return sum, s.AdvanceDerivedDataRevision()
	}
	ledgerKey := p.ledgerKey
	if sum == nil || sum.MessagesScanned == 0 {
		// New and empty sources have no existing derived rows to invalidate.
		// Record the pass so sync does not repeat it, but keep a current cache
		// valid after the source's first import.
		return sum, s.MarkMigrationApplied(ledgerKey)
	}
	if err := s.MarkMigrationAppliedWithDerivedDataRevision(ledgerKey); err != nil {
		return sum, err
	}
	return sum, nil
}

// RunIfStale runs each pass of a source that the ledger does not already
// record at its current version. ran reports whether any pass executed: a
// source type with no pass, or one whose passes are all recorded, is a no-op.
// This is the upgrade path, called from a sync.
func RunIfStale(
	ctx context.Context, s *store.Store, sourceType, identifier string, sourceID int64, progress func(string),
) (*Summary, bool, error) {
	total := &Summary{}
	ran := false
	for _, p := range passesFor(sourceType, identifier) {
		applied, err := s.IsMigrationApplied(p.ledgerKey)
		if err != nil {
			return nil, ran, err
		}
		if applied {
			continue
		}
		ran = true
		sum, err := runOne(ctx, s, p, sourceID, progress)
		addSummary(total, sum)
		if err != nil {
			return total, true, err
		}
	}
	if !ran {
		return nil, false, nil
	}
	return total, true, nil
}

// Heal runs RunIfStale before a sync or import adds more mail and logs a
// failure instead of returning it. A failed pass records no ledger entry, so
// it retries on the next sync while this one carries on.
func Heal(ctx context.Context, logger *slog.Logger, s *store.Store, src *store.Source) {
	sum, ran, err := RunIfStale(ctx, s, src.SourceType, src.Identifier, src.ID, nil)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("re-derive archived messages failed; retrying on the next sync",
				"source_id", src.ID, "error", err)
		}
		return
	}
	if ran && sum != nil && sum.MessagesScanned > 0 {
		logger.Info("re-derived archived messages",
			"source_id", src.ID, "messages", sum.MessagesScanned, "undecodable", sum.Undecodable)
	}
}
