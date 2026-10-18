package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

func deletionTargets(n int) []query.DeletionTarget {
	targets := make([]query.DeletionTarget, n)
	for i := range targets {
		targets[i] = query.DeletionTarget{MessageID: int64(i + 1), SourceID: 7, SourceType: "gmail", SourceIdentifier: "reader@example.test", SourceMessageID: fmt.Sprintf("message-%d", i)}
	}
	return targets
}

func TestStageDeletionExactSearch(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		mode       query.DeletionSearchMode
		size       int
	}{
		{"fast", "subject:newsletter", query.DeletionSearchFast, 1200},
		{"deep OR", "alpha | gamma", query.DeletionSearchDeep, 150},
		{"structured OR", "subject:first | subject:second", query.DeletionSearchFast, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			targets := deletionTargets(tc.size)
			eng := &querytest.MockEngine{
				SearchFastCountFunc: func(context.Context, *search.Query, query.MessageFilter) (int64, error) { return 0, nil }, // Stale zero must still resolve.
				SearchDeepWithStatsFunc: func(_ context.Context, _ *search.Query, _ query.MessageFilter, limit, offset int) (*query.SearchFastResult, error) {
					assert.Equal(1, limit)
					assert.Zero(offset)
					return &query.SearchFastResult{TotalCount: int64(tc.size)}, nil
				},
				GetDeletionTargetsBySearchFunc: func(_ context.Context, _ *search.Query, _ query.MessageFilter, mode query.DeletionSearchMode) ([]query.DeletionTarget, error) {
					assert.Equal(tc.mode, mode)
					return targets, nil
				},
				SearchFastFunc: func(context.Context, *search.Query, query.MessageFilter, int, int) ([]query.MessageSummary, error) {
					assert.Fail("capped fast search used")
					return nil, nil
				},
				SearchFunc: func(context.Context, *search.Query, int, int) ([]query.MessageSummary, error) {
					assert.Fail("capped deep search used")
					return nil, nil
				},
			}
			saver := &captureDeletionManifestSaver{}
			h := &handlers{engine: eng, manifestSaver: saver}
			resp := runTool[stageDeletionResponse](t, "stage_deletion", h.stageDeletion, map[string]any{"query": tc.text})
			require.NotNil(t, saver.manifest)
			want := make([]string, tc.size)
			for i := range targets {
				want[i] = targets[i].SourceMessageID
			}
			assert.Equal(want, saver.manifest.GmailIDs)
			assert.Equal(tc.size, resp.MessageCount)
			assert.Equal(tc.mode == query.DeletionSearchDeep, strings.Contains(resp.NextStep, "body matches"))
		})
	}
}

func TestStageDeletionSizingRefuses(t *testing.T) {
	for _, path := range []string{"fast", "deep", "fallback"} {
		for _, failure := range []string{"error", "negative", "over limit"} {
			t.Run(path+"/"+failure, func(t *testing.T) {
				assert := assert.New(t)
				var logs bytes.Buffer
				previousLogger := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previousLogger) })
				calls := 0
				count := int64(1)
				var countErr error
				switch failure {
				case "error":
					countErr = errors.New("private backend detail")
				case "negative":
					count = -1
				case "over limit":
					count = 100001
				}
				eng := &querytest.MockEngine{
					SearchFastCountFunc: func(context.Context, *search.Query, query.MessageFilter) (int64, error) {
						if path == "fallback" {
							return 0, nil
						}
						return count, countErr
					},
					SearchDeepWithStatsFunc: func(context.Context, *search.Query, query.MessageFilter, int, int) (*query.SearchFastResult, error) {
						return &query.SearchFastResult{TotalCount: count}, countErr
					},
					GetDeletionTargetsBySearchFunc: func(context.Context, *search.Query, query.MessageFilter, query.DeletionSearchMode) ([]query.DeletionTarget, error) {
						calls++
						return nil, nil
					},
				}
				saver := &captureDeletionManifestSaver{}
				h := &handlers{engine: eng, manifestSaver: saver}
				text := "subject:newsletter"
				if path == "deep" {
					text = "alpha | gamma"
				}
				if path == "fallback" {
					text = "invoice"
				}
				result := runToolExpectError(t, "stage_deletion", h.stageDeletion, map[string]any{"query": text})
				if failure == "over limit" {
					assert.Contains(resultText(t, result), "100001")
					assert.Contains(resultText(t, result), "at most 100000")
				} else {
					assert.Contains(resultText(t, result), "could not size")
					assert.Contains(logs.String(), "MCP deletion selection sizing failed")
					if countErr != nil {
						assert.Contains(logs.String(), countErr.Error())
					}
				}
				assert.NotContains(resultText(t, result), "private backend detail")
				wantCalls := 0
				if path == "fallback" {
					wantCalls = 1
				}
				assert.Equal(wantCalls, calls)
				assert.Empty(saver.manifests)
			})
		}
	}
}

func TestStageDeletionTextFallback(t *testing.T) {
	assert := assert.New(t)
	var events []string
	eng := &querytest.MockEngine{
		Accounts: []query.AccountInfo{{ID: 7, SourceType: "gmail", Identifier: "reader@example.test"}},
		SearchFastCountFunc: func(_ context.Context, q *search.Query, f query.MessageFilter) (int64, error) {
			events = append(events, "fast count")
			assert.Equal([]int64{7}, q.AccountIDs)
			require.NotNil(t, f.SourceID)
			assert.Equal(int64(7), *f.SourceID)
			return 1, nil
		},
		SearchDeepWithStatsFunc: func(_ context.Context, q *search.Query, f query.MessageFilter, _, _ int) (*query.SearchFastResult, error) {
			events = append(events, "deep count")
			assert.Equal([]int64{7}, q.AccountIDs)
			require.NotNil(t, f.SourceID)
			assert.Equal(int64(7), *f.SourceID)
			return &query.SearchFastResult{TotalCount: 1}, nil
		},
		GetDeletionTargetsBySearchFunc: func(_ context.Context, _ *search.Query, f query.MessageFilter, m query.DeletionSearchMode) ([]query.DeletionTarget, error) {
			require.NotNil(t, f.SourceID)
			assert.Equal(int64(7), *f.SourceID)
			events = append(events, string(m))
			if m == query.DeletionSearchFast {
				return nil, nil
			}
			return deletionTargets(1), nil
		},
	}
	saver := &captureDeletionManifestSaver{}
	h := &handlers{engine: eng, manifestSaver: saver}
	resp := runTool[stageDeletionResponse](t, "stage_deletion", h.stageDeletion, map[string]any{"query": "invoice", "account": "reader@example.test"})
	assert.Equal([]string{"fast count", "fast", "deep count", "deep"}, events)
	require.NotNil(t, saver.manifest)
	assert.Equal([]string{"message-0"}, saver.manifest.GmailIDs)
	assert.Contains(resp.NextStep, "body matches")
}

type engineWithoutDeletionSearch struct{ query.Engine }

func TestStageDeletionResolutionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		targets     []query.DeletionTarget
		err         error
		unsupported bool
		want        string
	}{
		{name: "unsupported", unsupported: true, want: "cannot resolve every match safely"},
		{name: "resolver failure", targets: deletionTargets(1), err: errors.New("upgrade daemon: private path")},
		{name: "blank source", targets: []query.DeletionTarget{{MessageID: 1, SourceID: 7, SourceType: " ", SourceIdentifier: "reader@example.test"}}, want: "incomplete source metadata"},
		{name: "multiple sources", targets: append(deletionTargets(1), query.DeletionTarget{MessageID: 2, SourceID: 8, SourceType: "gmail", SourceIdentifier: "other@example.test", SourceMessageID: "other"}), want: "multiple sources"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			eng := &querytest.MockEngine{GetDeletionTargetsBySearchFunc: func(context.Context, *search.Query, query.MessageFilter, query.DeletionSearchMode) ([]query.DeletionTarget, error) {
				return tc.targets, tc.err
			}}
			var engine query.Engine = eng
			if tc.unsupported {
				engine = engineWithoutDeletionSearch{eng}
			}
			saver := &captureDeletionManifestSaver{}
			h := &handlers{engine: engine, manifestSaver: saver}
			result, err := h.stageDeletion(t.Context(), toolRequest{arguments: map[string]any{"query": "subject:newsletter"}})
			if tc.err != nil {
				require.Error(err)
				assert.Nil(result)
			} else {
				require.NoError(err)
				require.NotNil(result)
				assert.True(result.isError)
				assert.Contains(resultText(t, result), tc.want)
			}
			assert.Empty(saver.manifests)
		})
	}
}

// Counts admit work; only exact target IDs may enter the saved manifest.
func FuzzStageDeletionWholeSelection(f *testing.F) {
	for _, n := range []uint32{0, 1, 100, 500, 99999, 100000, 100001} {
		for _, mode := range []uint8{0, 1, 2} {
			f.Add(int64(n), n, mode)
		}
	}
	f.Add(int64(-1), uint32(1), uint8(0))
	f.Add(int64(0), uint32(2), uint8(1))
	f.Fuzz(func(t *testing.T, estimate int64, size uint32, variant uint8) {
		// Above the rejection boundary cardinality is equivalent; bound materialization, not the drawn domain.
		n := min(uint64(size), uint64(100001))
		targets := deletionTargets(int(n))
		kind := variant % 3
		eng := &querytest.MockEngine{
			SearchFastCountFunc: func(context.Context, *search.Query, query.MessageFilter) (int64, error) { return estimate, nil },
			SearchDeepWithStatsFunc: func(context.Context, *search.Query, query.MessageFilter, int, int) (*query.SearchFastResult, error) {
				return &query.SearchFastResult{TotalCount: estimate}, nil
			},
			GetDeletionTargetsBySearchFunc: func(context.Context, *search.Query, query.MessageFilter, query.DeletionSearchMode) ([]query.DeletionTarget, error) {
				return targets, nil
			},
			GetDeletionTargetsByFilterFunc: func(context.Context, query.MessageFilter) ([]query.DeletionTarget, error) { return targets, nil },
		}
		args := map[string]any{"query": "subject:newsletter"}
		if kind == 1 {
			args["query"] = "alpha | gamma"
		}
		if kind == 2 {
			args = map[string]any{"from": "newsletter@example.test"}
		}
		saver := &captureDeletionManifestSaver{}
		h := &handlers{engine: eng, manifestSaver: saver}
		result, err := h.stageDeletion(t.Context(), toolRequest{arguments: args})
		require.NoError(t, err)
		admitted := kind == 2 || (estimate >= 0 && estimate <= 100000)
		if !admitted || n == 0 || n > 100000 {
			require.NotNil(t, result)
			assert.True(t, result.isError)
			assert.Empty(t, saver.manifests)
			return
		}
		require.NotNil(t, saver.manifest)
		assert.False(t, result.isError)
		want := make([]string, len(targets))
		for i := range targets {
			want[i] = targets[i].SourceMessageID
		}
		assert.Equal(t, want, saver.manifest.GmailIDs)
	})
}

// Invalid provenance anywhere in a nonempty selection must never save a prefix or retry deep.
func FuzzStageDeletionProvenance(f *testing.F) {
	f.Add(int64(7), "gmail", "reader@example.test", uint8(0))
	f.Add(int64(0), "gmail", "reader@example.test", uint8(2))
	f.Add(int64(7), "\u2003", "reader@example.test", uint8(1))
	f.Add(int64(8), "gmail", "other@example.test", uint8(2))
	f.Fuzz(func(t *testing.T, sourceID int64, sourceType, identifier string, position uint8) {
		targets := deletionTargets(3)
		i := int(position) % len(targets)
		targets[i].SourceID, targets[i].SourceType, targets[i].SourceIdentifier = sourceID, sourceType, identifier
		eng := &querytest.MockEngine{GetDeletionTargetsBySearchFunc: func(_ context.Context, _ *search.Query, _ query.MessageFilter, mode query.DeletionSearchMode) ([]query.DeletionTarget, error) {
			assert.Equal(t, query.DeletionSearchFast, mode)
			return targets, nil
		}}
		saver := &captureDeletionManifestSaver{}
		h := &handlers{engine: eng, manifestSaver: saver}
		result, err := h.stageDeletion(t.Context(), toolRequest{arguments: map[string]any{"query": "invoice"}})
		require.NoError(t, err)
		if sourceID != 7 || sourceType != "gmail" || identifier != "reader@example.test" {
			assert.True(t, result.isError)
			assert.Empty(t, saver.manifests)
			return
		}
		require.NotNil(t, saver.manifest)
		assert.Equal(t, []string{"message-0", "message-1", "message-2"}, saver.manifest.GmailIDs)
	})
}

func TestStageDeletionUndercountRefusesWithResolvedSize(t *testing.T) {
	eng := &querytest.MockEngine{
		SearchFastCountFunc: func(context.Context, *search.Query, query.MessageFilter) (int64, error) { return 99000, nil },
		GetDeletionTargetsBySearchFunc: func(context.Context, *search.Query, query.MessageFilter, query.DeletionSearchMode) ([]query.DeletionTarget, error) {
			return deletionTargets(100001), nil
		},
	}
	saver := &captureDeletionManifestSaver{}
	h := &handlers{engine: eng, manifestSaver: saver}
	result := runToolExpectError(t, "stage_deletion", h.stageDeletion, map[string]any{"query": "subject:newsletter"})
	assert.Contains(t, resultText(t, result), "matches 100001 messages")
	assert.Empty(t, saver.manifests)
}
