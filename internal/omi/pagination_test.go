package omi

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type mutableOmiProvider struct {
	rows         []Conversation
	discarded    map[string]bool
	hidden       map[string]bool
	invalid      map[string]bool
	beforeRead   func(ListParams)
	locked       map[string]bool
	calls        int
	passRequests int
	flipAt       []int
	discardAt    int
}

func newMutableOmiProvider(t *testing.T, count int, tied bool) *mutableOmiProvider {
	t.Helper()
	provider := &mutableOmiProvider{discarded: map[string]bool{}, locked: map[string]bool{}, hidden: map[string]bool{}, invalid: map[string]bool{}}
	for i := range count {
		created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		if !tied {
			created = created.Add(-time.Duration(i) * time.Second)
		}
		raw := fmt.Sprintf(`{"id":"synthetic-%04d","created_at":"%s","structured":{"title":"Synthetic meeting","overview":"Summary","action_items":[]},"transcript_segments":[]}`, i, created.Format(time.RFC3339Nano))
		provider.rows = append(provider.rows, decodeFixture(t, raw))
	}
	return provider
}

func (p *mutableOmiProvider) serve(w http.ResponseWriter, r *http.Request) {
	p.calls++
	params := omiRequest(r)
	if slices.Contains(p.flipAt, p.calls) {
		slices.Reverse(p.rows)
	}
	if p.calls == p.discardAt {
		p.discarded[p.rows[0].ID] = true
	}
	if p.passRequests > 0 && p.calls > p.passRequests {
		http.Error(w, "synthetic pass interrupted", http.StatusBadRequest)
		return
	}
	if p.beforeRead != nil {
		p.beforeRead(params)
	}
	var rows []Conversation
	for _, row := range p.rows {
		if !p.discarded[row.ID] {
			rows = append(rows, row)
		}
	}
	raw := filterOmiPage(rows, ListParams{Limit: len(rows) + 1, CreatedAfter: params.CreatedAfter, CreatedBefore: params.CreatedBefore})
	window := raw[min(params.Offset, len(raw)):min(params.Offset+params.Limit, len(raw))]
	page := make([]Conversation, 0, params.Limit)
	skipped := 0
	for _, row := range window {
		if p.hidden[row.ID] {
			skipped++
		} else {
			page = append(page, row)
		}
	}
	position := params.Offset + len(window)
	for len(page) < params.Limit && len(window) == params.Limit && skipped <= 64 {
		window = raw[min(position, len(raw)):min(position+params.Limit, len(raw))]
		position += len(window)
		for _, row := range window {
			if p.hidden[row.ID] {
				skipped++
				if skipped > 64 {
					break
				}
				continue
			}
			page = append(page, row)
			if len(page) == params.Limit {
				break
			}
		}
	}
	visible := make([]Conversation, 0, len(page))
	for _, row := range page {
		if !p.locked[row.ID] && !p.invalid[row.ID] {
			visible = append(visible, row)
		}
	}
	writeOmiPage(w, visible)
}

func assertOmiCoverage(t *testing.T, st *store.Store, sourceID int64, p *mutableOmiProvider) {
	t.Helper()
	var ids []string
	for _, row := range p.rows {
		if !p.discarded[row.ID] && !p.locked[row.ID] && !p.invalid[row.ID] && !p.hidden[row.ID] {
			ids = append(ids, row.ID)
		}
	}
	archived, err := st.MessageExistsBatch(sourceID, ids)
	require.NoError(t, err)
	assert.Len(t, archived, len(ids))
}

func TestImportTieBoundaryAcrossPasses(t *testing.T) {
	for _, mutation := range []string{"stable", "earlier deletion", "untied earlier deletion", "during pass", "behind deletion", "cancel replay"} {
		t.Run(mutation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(SourceType, "work")
			require.NoError(err)
			provider := newMutableOmiProvider(t, 17, mutation != "untied earlier deletion")
			provider.passRequests = 3
			if mutation == "during pass" {
				provider.discardAt = 2
			}
			server := httptest.NewServer(http.HandlerFunc(provider.serve))
			defer server.Close()
			imp := NewImporter(st, testOmiClient(t, server.URL, "omi_dev_synthetic"))
			imp.pageSize = 4
			opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
			_, err = imp.Import(t.Context(), opts)
			require.Error(err)
			provider.discardAt = 0
			prior, err := st.GetLatestCheckpointedSyncByType(src.ID, SourceType)
			require.NoError(err)
			var cursor scanCheckpoint
			require.NoError(json.Unmarshal([]byte(prior.CursorBefore.String), &cursor))
			if mutation == "earlier deletion" || mutation == "untied earlier deletion" || mutation == "cancel replay" {
				provider.discarded[provider.rows[0].ID] = true
			}
			if mutation == "behind deletion" {
				require.NotNil(cursor.Behind)
				provider.discarded[(*cursor.Behind)[0]] = true
			}
			if mutation == "cancel replay" {
				provider.calls = 0
				provider.passRequests = 2
				_, err = imp.Import(t.Context(), opts)
				require.Error(err)
			}
			completed := false
			for range 12 {
				provider.calls = 0
				provider.passRequests = 3
				_, err = imp.Import(t.Context(), opts)
				if err == nil {
					completed = true
					break
				}
				_, cursorErr := st.GetLastSuccessfulSync(src.ID)
				require.ErrorIs(cursorErr, store.ErrSyncRunNotFound)
			}
			require.True(completed, "stable ties finish across bounded passes")
			assertOmiCoverage(t, st, src.ID, provider)
			if mutation == "untied earlier deletion" {
				last, err := st.GetLastSuccessfulSync(src.ID)
				require.NoError(err)
				assert.Contains(last.CursorAfter.String, "2026-01-01T12:00:00Z")
			}
		})
	}
}

func TestImportFilteredTimestampDescent(t *testing.T) {
	for _, peer := range []bool{false, true} {
		t.Run(fmt.Sprintf("peer=%t", peer), func(t *testing.T) {
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(SourceType, "work")
			require.NoError(t, err)
			p := newMutableOmiProvider(t, 201, true)
			for _, row := range p.rows[1:200] {
				p.locked[row.ID] = true
			}
			delete(p.locked, p.rows[99].ID)
			p.invalid[p.rows[99].ID] = true
			if !peer {
				p.rows[200] = decodeFixture(t, strings.ReplaceAll(string(p.rows[200].Raw), "2026-01-01T12:00:00Z", "2026-01-01T11:59:59Z"))
			}
			server := httptest.NewServer(http.HandlerFunc(p.serve))
			defer server.Close()
			_, err = NewImporter(st, testOmiClient(t, server.URL, "omi_dev_synthetic")).Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(t, err)
			assertOmiCoverage(t, st, src.ID, p)
			_, err = st.GetLastSuccessfulSync(src.ID)
			require.NoError(t, err)
		})
	}
}

func TestImportCheckpointOptions(t *testing.T) {
	for _, mode := range []string{"inherit limit", "inherit date bound", "replace"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			replacement := mode == "replace"
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(SourceType, "work")
			require.NoError(err)
			provider := newMutableOmiProvider(t, 9, false)
			provider.passRequests = 1
			server := httptest.NewServer(http.HandlerFunc(provider.serve))
			defer server.Close()
			imp := NewImporter(st, testOmiClient(t, server.URL, "omi_dev_synthetic"))
			imp.pageSize = 4
			after := provider.rows[6].CreatedAt
			opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true, Limit: 5, CreatedAfter: after}
			if mode == "inherit date bound" {
				opts.Limit = 0
			}
			_, err = imp.Import(t.Context(), opts)
			require.Error(err)
			prior, err := st.GetLatestCheckpointedSyncByType(src.ID, SourceType)
			require.NoError(err)
			var saved scanCheckpoint
			require.NoError(json.Unmarshal([]byte(prior.CursorBefore.String), &saved))
			assert.Equal(syncStateVersion, saved.Version)
			opts = ImportOptions{Identifier: "work", AccountEmail: "owner@example.com"}
			var notices []string
			opts.Progress = func(line string) { notices = append(notices, line) }
			if replacement {
				opts.Limit = 1
			}
			provider.calls = 0
			provider.passRequests = 0
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			wantProcessed := int64(5)
			if mode == "inherit date bound" {
				wantProcessed = 7
			}
			if replacement {
				wantProcessed = 1
				assert.Contains(strings.Join(notices, "\n"), "replace the unfinished Omi scan")
			}
			assert.Equal(wantProcessed, sum.MeetingsProcessed)
			last, err := st.GetLastSuccessfulSync(src.ID)
			require.NoError(err)
			var finished scanCheckpoint
			require.NoError(json.Unmarshal([]byte(last.CursorBefore.String), &finished))
			if !replacement {
				assert.True(finished.Full)
				assert.Equal(saved.Limit, finished.Limit)
				assert.Equal(saved.After, finished.After)
				assert.Equal(saved.Lower, finished.Lower)
				assert.Equal(saved.Ceiling, finished.Ceiling)
			}
			var state syncState
			require.NoError(json.Unmarshal([]byte(last.CursorAfter.String), &state))
			assert.Equal(syncStateVersion, state.Version)
			assert.Empty(state.CreatedAfter, "inherited limits/date bounds preserve the watermark")
		})
	}
}

func TestImportLimitBoundsRequestsAndUniqueConversations(t *testing.T) {
	for _, mode := range []string{"ordinary", "backfill", "partial final", "delete before confirmation", "delete after confirmation", "one-request passes", "limit one"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(SourceType, "work")
			require.NoError(err)
			p := newMutableOmiProvider(t, 11, true)
			limit := 5
			if mode != "ordinary" && mode != "limit one" {
				p.hidden[p.rows[0].ID] = true
			}
			if mode == "partial final" {
				p.locked[p.rows[1].ID] = true
				limit = 6
			}
			if mode == "limit one" {
				limit = 1
			}
			if mode == "one-request passes" {
				p.passRequests = 1
			}
			reads := 0
			p.beforeRead = func(params ListParams) {
				reads++
				if (mode == "delete before confirmation" && reads == 5) || (mode == "delete after confirmation" && reads == 6) {
					p.discarded[p.rows[1].ID] = true
				}
			}
			server := httptest.NewServer(http.HandlerFunc(p.serve))
			defer server.Close()
			client := testOmiClient(t, server.URL, "omi_dev_synthetic")
			imp := NewImporter(st, sourceFunc(func(ctx context.Context, params ListParams) ([]Conversation, error) {
				processed := int64(0)
				prior, err := st.GetLatestCheckpointedSyncByType(src.ID, SourceType)
				if err == nil {
					var scan scanCheckpoint
					require.NoError(json.Unmarshal([]byte(prior.CursorBefore.String), &scan))
					processed = scan.Processed
				} else {
					require.ErrorIs(err, store.ErrSyncRunNotFound)
				}
				assert.LessOrEqual(params.Limit, limit-int(processed))
				return client.ListConversations(ctx, params)
			}))
			imp.pageSize = 4
			processed, completed := int64(0), false
			for range 30 {
				p.calls = 0
				sum, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", Limit: limit})
				if err == nil {
					processed += sum.MeetingsProcessed
					completed = true
					break
				}
				processed += sum.MeetingsProcessed
			}
			require.True(completed)
			assert.Equal(int64(limit), processed)
			var expected []string
			for _, row := range p.rows {
				if !p.hidden[row.ID] && !p.locked[row.ID] {
					expected = append(expected, row.ID)
					if len(expected) == limit {
						break
					}
				}
			}
			archived, err := st.MessageExistsBatch(src.ID, expected)
			require.NoError(err)
			assert.Len(archived, limit)
			count, err := st.CountMessagesForSourceContext(t.Context(), src.ID)
			require.NoError(err)
			assert.Equal(int64(limit), count)
		})
	}
}

func TestImportChangingTieStaysIncomplete(t *testing.T) {
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(t, err)
	p := newMutableOmiProvider(t, 9, true)
	p.flipAt = []int{4, 8}
	server := httptest.NewServer(http.HandlerFunc(p.serve))
	defer server.Close()
	imp := NewImporter(st, testOmiClient(t, server.URL, "omi_dev_synthetic"))
	imp.pageSize = 4
	_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.ErrorContains(t, err, "timestamp pagination is incomplete")
	_, err = st.GetLastSuccessfulSync(src.ID)
	require.ErrorIs(t, err, store.ErrSyncRunNotFound)
}

func TestImportSmallHistoryCompletion(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.GetOrCreateSource(SourceType, "work")
			require.NoError(err)
			p := newMutableOmiProvider(t, count, false)
			server := httptest.NewServer(http.HandlerFunc(p.serve))
			defer server.Close()
			sum, err := NewImporter(st, testOmiClient(t, server.URL, "omi_dev_synthetic")).Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(count), sum.MeetingsAdded)
			assert.Equal(2, p.calls)
		})
	}
}

func TestImportBackfilledWindowsAcrossPasses(t *testing.T) {
	for _, mode := range []string{"stable", "hidden shift", "visible shift", "empty behind", "pending stable", "pending deletion"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(SourceType, "work")
			require.NoError(err)
			p := newMutableOmiProvider(t, 9, true)
			hidden := 0
			if mode == "hidden shift" {
				hidden = 4
			}
			if strings.HasPrefix(mode, "pending") {
				for i, row := range p.rows {
					if i%4 != 0 {
						p.locked[row.ID] = true
					}
				}
				slices.Reverse(p.rows)
			} else if mode == "empty behind" {
				for _, row := range p.rows[:4] {
					p.locked[row.ID] = true
				}
			} else {
				p.hidden[p.rows[hidden].ID] = true
			}
			p.passRequests = 1
			reads := 0
			p.beforeRead = func(params ListParams) {
				reads++
				if reads == 3 {
					if mode == "hidden shift" {
						p.discarded[p.rows[4].ID] = true
					}
					if mode == "visible shift" {
						p.discarded[p.rows[1].ID] = true
					}
				}
			}
			server := httptest.NewServer(http.HandlerFunc(p.serve))
			defer server.Close()
			imp := NewImporter(st, testOmiClient(t, server.URL, "omi_dev_synthetic"))
			imp.pageSize = 4
			completed, pendingSeen := false, false
			priorOffset, rebases := 0, 0
			for range 20 {
				p.calls = 0
				_, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
				if err == nil {
					completed = true
					break
				}
				require.Error(err)
				prior, checkpointErr := st.GetLatestCheckpointedSyncByType(src.ID, SourceType)
				require.NoError(checkpointErr)
				var scan scanCheckpoint
				require.NoError(json.Unmarshal([]byte(prior.CursorBefore.String), &scan))
				if scan.Ahead != nil && !pendingSeen {
					pendingSeen = true
					if mode == "pending deletion" {
						p.discarded[p.rows[0].ID] = true
					}
				}
				if scan.Offset < priorOffset {
					rebases++
				}
				priorOffset = scan.Offset
				assert.NotContains(err.Error(), "pagination is incomplete")
			}
			require.True(completed)
			wantRebases := 0
			if mode == "visible shift" {
				wantRebases = 1
			}
			if strings.HasPrefix(mode, "pending") {
				require.True(pendingSeen)
			} else {
				assert.Equal(wantRebases, rebases)
			}
			assertOmiCoverage(t, st, src.ID, p)
		})
	}
}
