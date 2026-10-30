package query

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

func TestDuckDBLivePolicyRevisionRejectsUnchangedCache(t *testing.T) {
	requirements := require.New(t)

	s, openErr := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	requirements.NoError(openErr)
	requirements.NoError(s.InitSchema())
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	source, err := s.GetOrCreateSource("beeper", "cache-policy-account")
	requirements.NoError(err)
	_, err = s.EnsureConversation(source.ID, "room", "Synthetic room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Full))
	dir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)
	state, err := ReadCacheSyncState(dir)
	requirements.NoError(err)
	state.ConversationPolicyRevision, err = s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	writePolicyCacheState(t, dir, state)
	opts := DuckDBOptions{PolicyStore: s, ConversationPolicyRevision: s.ConversationPolicyRevision}
	for _, restricted := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "restricted"}[restricted], func(t *testing.T) {
			requirements := require.New(t)

			var e *DuckDBEngine
			if restricted {
				e, err = NewArchiveDuckDBEngine(t.Context(), dir, opts)
			} else {
				e, err = NewDuckDBEngine(dir, "", nil, opts)
			}
			requirements.NoError(err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			_, err = e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Cold))
			for range 2 {
				_, err = e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
				requirements.ErrorIs(err, ErrCacheUnavailable)
				var unavailable *CacheUnavailableError
				requirements.ErrorAs(err, &unavailable)
				assert.Equal(t, CacheDrifted, unavailable.Readiness)
			}
			readers := map[string]func() error{
				"aggregate":     func() error { _, err := e.Aggregate(t.Context(), ViewSenders, DefaultAggregateOptions()); return err },
				"explore":       func() error { _, err := e.Explore(t.Context(), ExploreRequest{}); return err },
				"files":         func() error { _, err := e.SearchFiles(t.Context(), FileSearchRequest{}); return err },
				"relationships": func() error { _, err := e.Relationships(t.Context(), RelationshipsRequest{}); return err },
				"people":        func() error { _, err := e.SearchPeople(t.Context(), PersonSearchRequest{}); return err },
				"domains":       func() error { _, err := e.SearchDomains(t.Context(), DomainSearchRequest{}); return err },
				"pagination": func() error {
					_, err := e.SearchFastWithStats(t.Context(), search.Parse("hello"), "hello", MessageFilter{}, ViewSenders, 10, 1)
					return err
				},
			}
			for name, read := range readers {
				t.Run(name, func(t *testing.T) { require.ErrorIs(t, read(), ErrCacheUnavailable) })
			}
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Full))
			state.ConversationPolicyRevision, err = s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			writePolicyCacheState(t, dir, state)
		})
	}
}

func TestDuckDBLivePolicyReadFailureClosesFence(t *testing.T) {
	requirements := require.New(t)

	s, openErr := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	requirements.NoError(openErr)
	requirements.NoError(s.InitSchema())
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	dir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)
	e, err := NewDuckDBEngine(dir, "", nil)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	e.policyStore = s
	sentinel := errors.New("policy authority unavailable")
	e.conversationPolicyRevision = func(context.Context) (int64, error) { return 0, sentinel }
	_, err = e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
	requirements.ErrorIs(err, sentinel)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	release, err := s.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
	requirements.NoError(err)
	release()
}

func TestDuckDBCacheReadAcquiresPolicyFenceBeforeCacheLock(t *testing.T) {
	requirements := require.New(t)

	s, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	requirements.NoError(err)
	requirements.NoError(s.InitSchema())
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	source, err := s.GetOrCreateSource("beeper", "cache-lock-order")
	requirements.NoError(err)
	_, err = s.EnsureConversation(source.ID, "room", "Synthetic room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Full))

	dir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)
	state, err := ReadCacheSyncState(dir)
	requirements.NoError(err)
	state.ConversationPolicyRevision, err = s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	writePolicyCacheState(t, dir, state)
	e, err := NewDuckDBEngine(dir, "", nil, DuckDBOptions{
		PolicyStore: s, ConversationPolicyRevision: s.ConversationPolicyRevision,
	})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(e.Close()) })

	cacheWriter := flock.New(CacheBuildLockPath(dir))
	requirements.NoError(cacheWriter.Lock())
	defer func() { requirements.NoError(cacheWriter.Close()) }()
	cacheLockEntered := make(chan struct{})
	e.cacheReadLock = func(ctx context.Context, analyticsDir string) (func(), error) {
		close(cacheLockEntered)
		return AcquireCacheReadLock(ctx, analyticsDir)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	readerDone := make(chan error, 1)
	go func() {
		release, err := e.acquireCacheRead(ctx)
		if release != nil {
			release()
		}
		readerDone <- err
	}()
	select {
	case <-cacheLockEntered:
	case <-ctx.Done():
		requirements.NoError(ctx.Err())
	}

	writerCtx, writerCancel := context.WithTimeout(ctx, time.Second)
	err = s.SetConversationPolicyOverride(writerCtx, source.ID, "room", conversationpolicy.Cold)
	writerCancel()
	requirements.ErrorIs(err, context.DeadlineExceeded,
		"the reader holds the policy fence while blocked on the cache lock")
	requirements.NoError(cacheWriter.Unlock())
	select {
	case err := <-readerDone:
		requirements.NoError(err)
	case <-ctx.Done():
		requirements.NoError(ctx.Err())
	}
	requirements.NoError(s.SetConversationPolicyOverride(ctx, source.ID, "room", conversationpolicy.Cold))
}

func writePolicyCacheState(t *testing.T, dir string, state CacheSyncState) {
	t.Helper()
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(CacheStatePath(dir), data, 0600))
}

func TestDuckDBPolicyFenceAcrossProcess(t *testing.T) {
	requirements := require.New(t)

	if path := os.Getenv("MSGVAULT_POLICY_CACHE_CHILD"); path != "" {
		s, err := store.OpenForTest(path)
		requirements.NoError(err)
		defer func() { require.NoError(t, s.Close()) }()
		requirements.NoError(s.SetConversationPolicyOverride(t.Context(), 1, "room", conversationpolicy.Cold))
		return
	}
	path := filepath.Join(t.TempDir(), "archive.db")
	s, err := store.OpenForTest(path)
	requirements.NoError(err)
	requirements.NoError(s.InitSchema())
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	source, err := s.GetOrCreateSource("beeper", "process-cache-policy")
	requirements.NoError(err)
	_, err = s.EnsureConversation(source.ID, "room", "Synthetic room")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Full))
	dir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)
	state, err := ReadCacheSyncState(dir)
	requirements.NoError(err)
	state.ConversationPolicyRevision, err = s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	writePolicyCacheState(t, dir, state)
	e, err := NewDuckDBEngine(dir, "", nil, DuckDBOptions{PolicyStore: s})
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
	requirements.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	executable, err := os.Executable()
	requirements.NoError(err)
	child := exec.CommandContext(ctx, executable, "-test.run=^TestDuckDBPolicyFenceAcrossProcess$")
	child.Env = append(os.Environ(), "MSGVAULT_POLICY_CACHE_CHILD="+path)
	output, err := child.CombinedOutput()
	requirements.NoError(err, string(output))
	_, err = e.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
	requirements.ErrorIs(err, ErrCacheUnavailable)
}

func TestDuckDBLivePolicyFenceHeldUntilReadRelease(t *testing.T) {
	requirements := require.New(t)

	s, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	requirements.NoError(err)
	requirements.NoError(s.InitSchema())
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	dir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)
	e, err := NewDuckDBEngine(dir, "", nil, DuckDBOptions{PolicyStore: s})
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	release, err := e.acquireCacheRead(t.Context())
	requirements.NoError(err)
	t.Cleanup(func() {
		if release != nil {
			release()
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; an admitted analytical read holds the real policy fence
	unlock, err := s.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
	cancel()
	if unlock != nil {
		unlock()
	}
	requirements.ErrorIs(err, context.DeadlineExceeded, "policy writer cannot pass an admitted analytical read")
	release()
	release = nil
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	unlock, err = s.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
	requirements.NoError(err)
	unlock()
}
