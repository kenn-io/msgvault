package deletion

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testManager creates a Manager in a temp directory for testing.
func testManager(t *testing.T) *Manager {
	t.Helper()
	mgr, err := NewManager(filepath.Join(t.TempDir(), "deletions"))
	require.NoError(t, err, "NewManager()")
	return mgr
}

// newTestManifest creates a Manifest directly with the given description and IDs.
func newTestManifest(t *testing.T, desc string, ids ...string) *Manifest {
	t.Helper()
	return NewManifest(desc, ids)
}

// createTestManifest creates a manifest via the manager with default IDs.
func createTestManifest(t *testing.T, mgr *Manager, desc string) *Manifest {
	t.Helper()
	m, err := mgr.CreateManifest(desc, []string{"a", "b"}, Filters{})
	require.NoError(t, err, "CreateManifest(%q)", desc)
	return m
}

// ManifestBuilder provides a fluent API for constructing Manifest test fixtures.
type ManifestBuilder struct {
	t *testing.T
	m *Manifest
}

// BuildManifest starts building a Manifest with sensible defaults.
// If no IDs are provided, defaults to {"id1", "id2"}.
func BuildManifest(t *testing.T, desc string, ids ...string) *ManifestBuilder {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{"id1", "id2"}
	}
	return &ManifestBuilder{t: t, m: NewManifest(desc, ids)}
}

// WithFilters sets sender and after-date filters.
func (b *ManifestBuilder) WithFilters(senders []string, after string) *ManifestBuilder {
	b.m.Filters = Filters{Senders: senders, After: after}
	return b
}

// WithSummary sets the manifest summary.
func (b *ManifestBuilder) WithSummary(count int, sizeBytes int64, dateRange [2]string, topSenders []SenderCount) *ManifestBuilder {
	b.m.Summary = &Summary{
		MessageCount:   count,
		TotalSizeBytes: sizeBytes,
		DateRange:      dateRange,
		TopSenders:     topSenders,
	}
	return b
}

// WithExecution sets the manifest execution details.
func (b *ManifestBuilder) WithExecution(method Method, succeeded, failed int, completedAt *time.Time) *ManifestBuilder {
	b.m.Execution = &Execution{
		StartedAt:   time.Now().Add(-time.Hour),
		CompletedAt: completedAt,
		Method:      method,
		Succeeded:   succeeded,
		Failed:      failed,
	}
	return b
}

// WithoutSummary explicitly sets the manifest summary to nil.
func (b *ManifestBuilder) WithoutSummary() *ManifestBuilder {
	b.m.Summary = nil
	return b
}

// WithStatus sets the manifest status.
func (b *ManifestBuilder) WithStatus(s Status) *ManifestBuilder {
	b.m.Status = s
	return b
}

// Build returns the constructed Manifest.
func (b *ManifestBuilder) Build() *Manifest {
	return b.m
}

// AssertManifestEqual compares two manifests structurally, ignoring timestamps.
func AssertManifestEqual(t *testing.T, got, want *Manifest) {
	t.Helper()
	opts := cmp.Options{
		cmpopts.IgnoreFields(Manifest{}, "CreatedAt"),
		cmpopts.IgnoreFields(Execution{}, "StartedAt", "CompletedAt"),
	}
	diff := cmp.Diff(want, got, opts...)
	assert.Empty(t, diff, "Manifest mismatch (-want +got):\n%s", diff)
}

// AssertManifestInState verifies that a manifest file exists in the expected state directory.
func AssertManifestInState(t *testing.T, mgr *Manager, id string, state Status) {
	t.Helper()
	var dir string
	switch state {
	case StatusPending:
		dir = mgr.PendingDir()
	case StatusInProgress:
		dir = mgr.InProgressDir()
	case StatusCompleted:
		dir = mgr.CompletedDir()
	case StatusFailed:
		dir = mgr.FailedDir()
	case StatusCancelled:
		dir = mgr.dirForStatus(StatusCancelled)
	default:
		require.Failf(t, "unknown state", "%q", state)
	}
	path := filepath.Join(dir, id+".json")
	_, err := os.Stat(path)
	assert.NoError(t, err, "manifest %q not found in %s directory", id, state)
}

// assertSummaryContains checks that summary contains all specified substrings.
func assertSummaryContains(t *testing.T, summary string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		assert.Contains(t, summary, part, "summary missing %q", part)
	}
}

// assertSummaryNotContains checks that summary does not contain any of the specified substrings.
func assertSummaryNotContains(t *testing.T, summary string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		assert.NotContains(t, summary, part, "summary should not contain %q", part)
	}
}

// assertListCount calls a list function and asserts the returned slice length.
func assertListCount(t *testing.T, listFn func() ([]*Manifest, error), want int) {
	t.Helper()
	got, err := listFn()
	require.NoError(t, err, "list error")
	require.Len(t, got, want, "list returned %d manifests, want %d", len(got), want)
}

// writeFile is a test helper that writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0644), "WriteFile(%s)", path)
}

func TestSanitizeForFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"with spaces", "with-spaces"},
		{"with.dots", "with-dots"},
		{"with-dash_underscore", "with-dash_underscore"},
		{"Special@#$Characters!", "SpecialCharacters"},
		{"MixedCase123", "MixedCase123"},
		{"", ""},
		{"a b c d e", "a-b-c-d-e"},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, sanitizeForFilename(tc.input), "sanitizeForFilename(%q)", tc.input)
	}
}

func TestGenerateID(t *testing.T) {
	tests := []struct {
		name     string
		desc     string
		validate func(*testing.T, string)
	}{
		{
			name: "basic ID generation",
			desc: "test batch",
			validate: func(t *testing.T, id string) {
				t.Helper()
				assert.NotEmpty(t, id, "generateID returned empty string")
				parts := strings.SplitN(id, "-", 3)
				assert.GreaterOrEqual(t, len(parts), 2, "expected timestamp-description format, got %q", id)
			},
		},
		{
			name: "long description truncated",
			desc: "this is a very long description that exceeds twenty characters",
			validate: func(t *testing.T, id string) {
				t.Helper()
				assert.LessOrEqual(t, len(id), 53, "ID too long: %d chars (%q)", len(id), id)
			},
		},
		{
			name: "empty description uses batch",
			desc: "",
			validate: func(t *testing.T, id string) {
				t.Helper()
				assert.Contains(t, id, "-batch-", "expected -batch- segment, got %q", id)
			},
		},
		{
			name: "same description yields distinct IDs",
			desc: "duplicate",
			validate: func(t *testing.T, id string) {
				t.Helper()
				assert.NotEqual(t, generateID("duplicate"), id,
					"IDs generated in the same second must not collide")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := generateID(tc.desc)
			tc.validate(t, id)
		})
	}
}

func TestNewManifest(t *testing.T) {
	assert := assert.New(t)
	gmailIDs := []string{"msg1", "msg2", "msg3"}
	m := NewManifest("test deletion", gmailIDs)

	assert.Equal(1, m.Version)
	assert.NotEmpty(m.ID, "ID is empty")
	assert.Equal("test deletion", m.Description)
	assert.Len(m.GmailIDs, 3)
	assert.Equal(StatusPending, m.Status)
	assert.Equal("cli", m.CreatedBy)
	assert.False(m.CreatedAt.IsZero(), "CreatedAt is zero")
}

func TestNewManifestForSourceRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	path := filepath.Join(t.TempDir(), "source-bound.json")
	wantSource := SourceReference{ID: 42, Type: "gmail", Identifier: "account@example.invalid"}
	manifest := NewManifestForSource("source-bound", []string{"gm-1"}, wantSource)

	require.NoError(manifest.Save(path))
	loaded, err := LoadManifest(path)
	require.NoError(err)
	assert.Equal(2, loaded.Version)
	require.NotNil(loaded.Source)
	assert.Equal(wantSource, *loaded.Source)
}

func TestManifestVersionSourceContract(t *testing.T) {
	validSource := &SourceReference{ID: 42, Type: "gmail", Identifier: "account@example.invalid"}
	tests := []struct {
		name     string
		version  int
		source   *SourceReference
		wantPart string
	}{
		{name: "v1 with source", version: 1, source: validSource, wantPart: "version 1"},
		{name: "v2 missing source", version: 2, wantPart: "complete source"},
		{name: "v2 zero id", version: 2, source: &SourceReference{Type: "gmail", Identifier: "account@example.invalid"}, wantPart: "complete source"},
		{name: "unsupported", version: 3, wantPart: "unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := NewManifest("invalid", []string{"gm-1"})
			manifest.Version = tt.version
			manifest.Source = tt.source
			err := manifest.Save(filepath.Join(t.TempDir(), "manifest.json"))
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantPart)
		})
	}
}

func TestLoadLiteralVersionOneManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	path := filepath.Join(t.TempDir(), "legacy.json")
	require.NoError(os.WriteFile(path, []byte(`{
		"version":1,"id":"legacy-batch","created_at":"2026-08-19T00:00:00Z",
		"created_by":"cli","description":"legacy","filters":{"account":"legacy@example.invalid"},
		"gmail_ids":["gm-1"],"status":"pending"
	}`), 0o600))

	manifest, err := LoadManifest(path)
	require.NoError(err)
	assert.Equal(1, manifest.Version)
	assert.Nil(manifest.Source)
}

func TestClaimManifestWithDigestRejectsReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mgr := testManager(t)
	manifest := NewManifestForSource("confirmed", []string{"gm-1"}, SourceReference{
		ID: 42, Type: "gmail", Identifier: "account@example.invalid",
	})
	require.NoError(mgr.SaveManifest(manifest))
	digest, err := manifest.Digest()
	require.NoError(err)

	replacement := *manifest
	replacement.Filters.Account = "changed@example.invalid"
	require.NoError(replacement.Save(filepath.Join(mgr.PendingDir(), manifest.ID+".json")))

	_, err = mgr.ClaimManifestWithDigest(manifest.ID, MethodTrash, digest)
	require.ErrorContains(err, "changed since confirmation")
	assert.FileExists(filepath.Join(mgr.PendingDir(), manifest.ID+".json"))
	assert.NoFileExists(filepath.Join(mgr.InProgressDir(), manifest.ID+".json"))
}

func TestManifest_SaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test-manifest.json")

	// Create and save manifest
	m := BuildManifest(t, "save test").
		WithFilters([]string{"test@example.com"}, "2024-01-01").
		WithSummary(2, 1024, [2]string{"2024-01-01", "2024-01-15"}, []SenderCount{
			{Sender: "test@example.com", Count: 2},
		}).
		Build()

	require.NoError(t, m.Save(path), "Save()")

	// Load and verify
	loaded, err := LoadManifest(path)
	require.NoError(t, err, "LoadManifest()")

	AssertManifestEqual(t, loaded, m)
}

func TestManifest_Save_CreatesDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "subdir", "nested", "manifest.json")

	m := BuildManifest(t, "nested test", "id1").Build()
	require.NoError(t, m.Save(path), "Save() with nested path")

	// Verify file exists
	_, err := os.Stat(path)
	assert.False(t, os.IsNotExist(err), "File was not created")
}

func TestLoadManifest_NotFound(t *testing.T) {
	_, err := LoadManifest("/nonexistent/path/manifest.json")
	assert.Error(t, err, "LoadManifest() should error for nonexistent file")
}

func TestLoadManifest_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "invalid.json")
	writeFile(t, path, "not valid json")

	_, err := LoadManifest(path)
	assert.Error(t, err, "LoadManifest() should error for invalid JSON")
}

func TestManifest_FormatSummary(t *testing.T) {
	tests := []struct {
		name            string
		setupManifest   func(*testing.T) *Manifest
		wantContains    []string
		wantNotContains []string
	}{
		{
			name: "basic summary",
			setupManifest: func(t *testing.T) *Manifest {
				t.Helper()
				return BuildManifest(t, "format test", "id1", "id2", "id3").
					WithSummary(3, 5*1024*1024, [2]string{"2024-01-01", "2024-01-31"}, []SenderCount{
						{Sender: "alice@example.com", Count: 2},
						{Sender: "bob@example.com", Count: 1},
					}).Build()
			},
			wantContains: []string{"pending", "Messages: 3", "5.00 MB", "2024-01-01", "alice@example.com"},
		},
		{
			name: "with execution",
			setupManifest: func(t *testing.T) *Manifest {
				t.Helper()
				now := time.Now()
				return BuildManifest(t, "exec test", "id1").
					WithExecution(MethodTrash, 10, 2, &now).Build()
			},
			wantContains: []string{"Execution:", "Method: trash", "Succeeded: 10", "Failed: 2", "Completed:"},
		},
		{
			name: "empty date range",
			setupManifest: func(t *testing.T) *Manifest {
				t.Helper()
				return BuildManifest(t, "empty date test", "id1").
					WithSummary(1, 1024, [2]string{"", ""}, nil).Build()
			},
			wantNotContains: []string{"Date Range:"},
		},
		{
			name: "nil summary",
			setupManifest: func(t *testing.T) *Manifest {
				t.Helper()
				return BuildManifest(t, "no summary test").WithoutSummary().Build()
			},
			wantContains:    []string{"Messages: 2"},
			wantNotContains: []string{"Total Size:"},
		},
		{
			name: "many top senders truncated to 10",
			setupManifest: func(t *testing.T) *Manifest {
				t.Helper()
				topSenders := make([]SenderCount, 15)
				for i := range 15 {
					topSenders[i] = SenderCount{
						Sender: "sender" + string(rune('a'+i)) + "@example.com",
						Count:  100 - i,
					}
				}
				return BuildManifest(t, "many senders test", "id1").
					WithSummary(15, 1024, [2]string{"2024-01-01", "2024-01-31"}, topSenders).Build()
			},
			wantContains:    []string{"sendera@example.com", "senderj@example.com"},
			wantNotContains: []string{"senderk@example.com"},
		},
		{
			name: "execution without completed time",
			setupManifest: func(t *testing.T) *Manifest {
				t.Helper()
				return BuildManifest(t, "no completed test", "id1").
					WithExecution(MethodDelete, 5, 0, nil).Build()
			},
			wantContains:    []string{"Execution:", "Method: delete"},
			wantNotContains: []string{"Completed:"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.setupManifest(t)
			got := m.FormatSummary()
			assertSummaryContains(t, got, tc.wantContains...)
			assertSummaryNotContains(t, got, tc.wantNotContains...)
			if tc.name == "basic summary" {
				assertSummaryContains(t, got, m.ID)
			}
		})
	}
}

func TestNewManager(t *testing.T) {
	assert := assert.New(t)
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "deletions")

	mgr, err := NewManager(baseDir)
	require.NoError(t, err, "NewManager()")

	// Verify all directories were created
	expectedDirs := []string{"pending", "in_progress", "completed", "failed", "cancelled"}
	for _, d := range expectedDirs {
		path := filepath.Join(baseDir, d)
		info, err := os.Stat(path)
		assert.True(err == nil && info.IsDir(), "Directory %s was not created", d)
	}

	// Verify directory getters
	assert.Equal(filepath.Join(baseDir, "pending"), mgr.PendingDir())
	assert.Equal(filepath.Join(baseDir, "in_progress"), mgr.InProgressDir())
	assert.Equal(filepath.Join(baseDir, "completed"), mgr.CompletedDir())
	assert.Equal(filepath.Join(baseDir, "failed"), mgr.FailedDir())
}

func TestManager_CreateAndListManifests(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)

	// Create manifests
	m1, err := mgr.CreateManifest("first batch", []string{"a", "b"}, Filters{Senders: []string{"alice@example.com"}})
	require.NoError(err, "CreateManifest()")

	m2 := createTestManifest(t, mgr, "second batch")

	// List pending should return both
	assertListCount(t, mgr.ListPending, 2)
	pending, err := mgr.ListPending()
	require.NoError(err, "ListPending()")

	// Verify both manifests are present
	ids := make(map[string]bool)
	for _, m := range pending {
		ids[m.ID] = true
	}
	assert.True(ids[m1.ID], "ListPending() missing manifest %q", m1.ID)
	assert.True(ids[m2.ID], "ListPending() missing manifest %q", m2.ID)

	// Verify ordering: list should be sorted by CreatedAt (newest first)
	assert.True(slices.IsSortedFunc(pending, func(a, b *Manifest) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	}), "ListPending() not sorted newest-first")
}

func TestManager_GetManifest(t *testing.T) {
	mgr := testManager(t)

	// Create a manifest
	m := createTestManifest(t, mgr, "get test")

	// Get it back
	loaded, path, err := mgr.GetManifest(m.ID)
	require.NoError(t, err, "GetManifest()")
	AssertManifestEqual(t, loaded, m)
	assert.NotEmpty(t, path, "GetManifest() returned empty path")
}

func TestManager_GetManifest_NotFound(t *testing.T) {
	mgr := testManager(t)

	_, _, err := mgr.GetManifest("nonexistent-id")
	require.Error(t, err, "GetManifest() should error for nonexistent manifest")
	assert.Contains(t, err.Error(), "not found")
}

func TestManager_GetManifest_EmptyID(t *testing.T) {
	mgr := testManager(t)

	for _, id := range []string{"", "   "} {
		_, _, err := mgr.GetManifest(id)
		require.Error(t, err, "GetManifest(%q) should error", id)
		assert.Contains(t, err.Error(), "batch ID is required", "clear error for %q", id)
		assert.NotContains(t, err.Error(), "  ", "no double space for %q", id)
	}
}

func TestManager_Transitions(t *testing.T) {
	tests := []struct {
		name string
		// Chain of transitions to apply; last one is the transition under test.
		chain   [][2]Status
		wantErr bool
		// Expected list counts after successful transitions: [pending, inProgress, completed, failed]
		wantCounts [4]int
	}{
		{"pending->in_progress", [][2]Status{{StatusPending, StatusInProgress}}, false, [4]int{0, 1, 0, 0}},
		{"in_progress->completed", [][2]Status{{StatusPending, StatusInProgress}, {StatusInProgress, StatusCompleted}}, false, [4]int{0, 0, 1, 0}},
		{"in_progress->failed", [][2]Status{{StatusPending, StatusInProgress}, {StatusInProgress, StatusFailed}}, false, [4]int{0, 0, 0, 1}},
		{"completed->pending (invalid)", [][2]Status{{StatusPending, StatusInProgress}, {StatusInProgress, StatusCompleted}, {StatusCompleted, StatusPending}}, true, [4]int{}},
		{"pending->pending (invalid)", [][2]Status{{StatusPending, StatusPending}}, true, [4]int{}},
		{"pending->cancelled", [][2]Status{{StatusPending, StatusCancelled}}, false, [4]int{0, 0, 0, 0}},
		{"in_progress->cancelled", [][2]Status{{StatusPending, StatusInProgress}, {StatusInProgress, StatusCancelled}}, false, [4]int{0, 0, 0, 0}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := testManager(t)
			m := createTestManifest(t, mgr, "transition test")

			var err error
			for _, step := range tc.chain {
				err = mgr.MoveManifest(m.ID, step[0], step[1])
				// Only the last step may error; earlier steps must succeed.
				if err != nil {
					break
				}
			}

			assert.Equal(t, tc.wantErr, err != nil, "MoveManifest() error = %v, wantErr %v", err, tc.wantErr)
			if !tc.wantErr {
				last := tc.chain[len(tc.chain)-1]
				AssertManifestInState(t, mgr, m.ID, last[1])

				// Verify list counts to ensure proper bookkeeping
				assertListCount(t, mgr.ListPending, tc.wantCounts[0])
				assertListCount(t, mgr.ListInProgress, tc.wantCounts[1])
				assertListCount(t, mgr.ListCompleted, tc.wantCounts[2])
				assertListCount(t, mgr.ListFailed, tc.wantCounts[3])
			}
		})
	}
}

func TestManager_CancelManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)

	manifest := createTestManifest(t, mgr, "cancel test")

	// Cancel it
	require.NoError(mgr.CancelManifest(manifest.ID), "CancelManifest()")

	baseDir := filepath.Dir(mgr.PendingDir())

	// File should now exist at cancelled/<id>.json with Status=cancelled.
	cancelledPath := filepath.Join(baseDir, "cancelled", manifest.ID+".json")
	_, err := os.Stat(cancelledPath)
	require.NoError(err, "expected cancelled manifest at %s", cancelledPath)
	loaded, err := LoadManifest(cancelledPath)
	require.NoError(err, "load cancelled manifest")
	assert.Equal(StatusCancelled, loaded.Status)
	// File should be absent from pending/.
	pendingPath := filepath.Join(baseDir, "pending", manifest.ID+".json")
	_, err = os.Stat(pendingPath)
	assert.True(os.IsNotExist(err), "expected pending manifest gone, got err=%v", err)
}

func TestManager_CancelManifest_InProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	manifest := createTestManifest(t, mgr, "cancel in-progress")

	// Move to in_progress
	require.NoError(mgr.MoveManifest(manifest.ID, StatusPending, StatusInProgress), "MoveManifest()")

	// Cancel it
	require.NoError(mgr.CancelManifest(manifest.ID), "CancelManifest()")

	baseDir := filepath.Dir(mgr.PendingDir())

	// File should now exist at cancelled/<id>.json with Status=cancelled.
	cancelledPath := filepath.Join(baseDir, "cancelled", manifest.ID+".json")
	_, err := os.Stat(cancelledPath)
	require.NoError(err, "expected cancelled manifest at %s", cancelledPath)
	loaded, err := LoadManifest(cancelledPath)
	require.NoError(err, "load cancelled manifest")
	assert.Equal(StatusCancelled, loaded.Status)
	// File should be absent from in_progress/.
	inProgressPath := filepath.Join(baseDir, "in_progress", manifest.ID+".json")
	_, err = os.Stat(inProgressPath)
	assert.True(os.IsNotExist(err), "expected in_progress manifest gone, got err=%v", err)
}

func TestManager_CancelManifest_NotFound(t *testing.T) {
	mgr := testManager(t)

	err := mgr.CancelManifest("nonexistent-id")
	require.Error(t, err, "CancelManifest() should error for nonexistent manifest")
	assert.Contains(t, err.Error(), "not found")
}

func TestManager_SaveManifest(t *testing.T) {
	mgr := testManager(t)

	// Create manifest with each status and verify placement
	statuses := []Status{StatusPending, StatusInProgress, StatusCompleted, StatusFailed}
	for _, status := range statuses {
		m := NewManifest("status-"+string(status), []string{"a"})
		m.Status = status

		require.NoError(t, mgr.SaveManifest(m), "SaveManifest(%s)", status)

		// Verify it can be found
		loaded, _, err := mgr.GetManifest(m.ID)
		require.NoError(t, err, "GetManifest(%s) after SaveManifest", status)
		assert.Equal(t, status, loaded.Status)
	}
}

func TestManager_ListManifests_SkipsInvalidFiles(t *testing.T) {
	mgr := testManager(t)

	// Create a valid manifest
	createTestManifest(t, mgr, "valid")

	// Add an invalid JSON file
	writeFile(t, filepath.Join(mgr.PendingDir(), "invalid.json"), "not json")

	// Add a non-JSON file
	writeFile(t, filepath.Join(mgr.PendingDir(), "readme.txt"), "readme")

	// Add a directory
	dirPath := filepath.Join(mgr.PendingDir(), "subdir.json")
	require.NoError(t, os.MkdirAll(dirPath, 0755), "MkdirAll(subdir.json)")

	// ListPending should only return the valid manifest
	assertListCount(t, mgr.ListPending, 1)
}

func TestStatus_Values(t *testing.T) {
	assert := assert.New(t)
	// Verify status constants
	assert.Equal(StatusPending, Status("pending"))
	assert.Equal(StatusInProgress, Status("in_progress"))
	assert.Equal(StatusCompleted, Status("completed"))
	assert.Equal(StatusFailed, Status("failed"))
	assert.Equal(StatusCancelled, Status("cancelled"))
}

func TestMethod_Values(t *testing.T) {
	assert.Equal(t, MethodTrash, Method("trash"))
	assert.Equal(t, MethodDelete, Method("delete"))
}

// TestStatusDirMap verifies that statusDirMap contains all persisted statuses
// and maps them to the expected directory names.
func TestStatusDirMap(t *testing.T) {
	assert := assert.New(t)
	// Verify all persistedStatuses have entries in statusDirMap
	for _, status := range persistedStatuses {
		dirName, ok := statusDirMap[status]
		if !ok {
			assert.Fail("missing from statusDirMap", "persistedStatus %q missing", status)
			continue
		}
		assert.NotEmpty(dirName, "statusDirMap[%q] is empty", status)
	}

	// Verify expected mappings
	expectedMappings := map[Status]string{
		StatusPending:    "pending",
		StatusInProgress: "in_progress",
		StatusCompleted:  "completed",
		StatusFailed:     "failed",
		StatusCancelled:  "cancelled",
	}
	for status, wantDir := range expectedMappings {
		gotDir, ok := statusDirMap[status]
		if !ok {
			assert.Fail("missing from statusDirMap", "statusDirMap missing entry for %q", status)
			continue
		}
		assert.Equal(wantDir, gotDir, "statusDirMap[%q]", status)
	}
}

// TestDirForStatus verifies that dirForStatus returns the correct path for each status.
func TestDirForStatus(t *testing.T) {
	mgr := testManager(t)

	tests := []struct {
		status  Status
		wantDir string
	}{
		{StatusPending, "pending"},
		{StatusInProgress, "in_progress"},
		{StatusCompleted, "completed"},
		{StatusFailed, "failed"},
		{StatusCancelled, "cancelled"},
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			got := mgr.dirForStatus(tc.status)
			wantSuffix := string(filepath.Separator) + tc.wantDir
			assert.True(t, strings.HasSuffix(got, wantSuffix), "dirForStatus(%q) = %q, want suffix %q", tc.status, got, wantSuffix)
		})
	}
}

// TestPersistedStatusesComplete verifies that persistedStatuses includes all
// statuses that should be persisted to disk.
func TestPersistedStatusesComplete(t *testing.T) {
	// All these statuses should be in persistedStatuses
	requiredStatuses := []Status{
		StatusPending,
		StatusInProgress,
		StatusCompleted,
		StatusFailed,
	}

	for _, status := range requiredStatuses {
		assert.Contains(t, persistedStatuses, status, "Status %q should be in persistedStatuses but is not", status)
	}

	// StatusCancelled MUST be in persistedStatuses; cancelled manifests
	// are persisted on disk for audit (per spec § Manifest format).
	assert.Contains(t, persistedStatuses, StatusCancelled, "StatusCancelled missing from persistedStatuses; cancelled manifests must persist on disk")
}

func TestManager_ListCancelled(t *testing.T) {
	require := require.New(t)
	mgr := testManager(t)

	manifest := createTestManifest(t, mgr, "test cancel")
	require.NoError(mgr.CancelManifest(manifest.ID), "CancelManifest")
	got, err := mgr.ListCancelled()
	require.NoError(err, "ListCancelled")
	require.Len(got, 1)
	assert.Equal(t, manifest.ID, got[0].ID)
}

// TestManager_SaveManifest_UnknownStatus tests saving with an unknown status.
func TestManager_SaveManifest_UnknownStatus(t *testing.T) {
	mgr := testManager(t)

	m := NewManifest("unknown status", []string{"a"})
	m.Status = "invalid_status" // Unknown status

	// Should still save (to pending dir by default)
	require.NoError(t, mgr.SaveManifest(m), "SaveManifest() with unknown status")

	// Should be findable (saved to pending by default)
	loaded, _, err := mgr.GetManifest(m.ID)
	require.NoError(t, err, "GetManifest()")
	assert.Equal(t, Status("invalid_status"), loaded.Status)
}

// TestManager_ListManifests_NonexistentDir tests listing from a nonexistent directory.
func TestManager_ListManifests_NonexistentDir(t *testing.T) {
	mgr := testManager(t)

	// Remove the pending directory
	require.NoError(t, os.RemoveAll(mgr.PendingDir()), "RemoveAll()")

	// ListPending should return empty (not error) for nonexistent dir
	assertListCount(t, mgr.ListPending, 0)
}

// TestManifest_Save_FilePermissions verifies that manifest files are saved with
// restrictive permissions (0600) to protect Gmail message IDs.
func TestManifest_Save_FilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "permission-test.json")

	m := newTestManifest(t, "permission test", "msg1", "msg2")
	require.NoError(t, m.Save(path), "Save()")

	info, err := os.Stat(path)
	require.NoError(t, err, "Stat()")

	// File should have 0600 permissions (owner read/write only)
	// Windows does not support Unix permissions.
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "file permissions")
	}
}

func TestValidateManifestID(t *testing.T) {
	require := require.New(t)
	valid := []string{
		"20260208-202335-batch",
		"20260208-202335-Sender_Name-1",
		"abc",
		"A1_b2-c3",
	}
	for _, id := range valid {
		require.NoError(ValidateManifestID(id), "id %q should be valid", id)
	}
	invalid := []string{
		"",
		"   ",
		"..",
		"../escape",
		"../../tokens/foo",
		"a/b",
		`a\b`,
		"/etc/passwd",
		"has space",
		"dot.name",
		"emoji😀",
	}
	for _, id := range invalid {
		require.Error(ValidateManifestID(id), "id %q should be rejected", id)
	}
}

func TestSaveManifestRejectsTraversalID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)

	m := newTestManifest(t, "traversal", "gmail-1")
	m.ID = "../../escape"

	err := mgr.SaveManifest(m)
	require.Error(err, "SaveManifest should reject a traversal ID")

	// Nothing may be written outside the deletions base directory.
	escaped := filepath.Join(mgr.baseDir, "..", "..", "escape.json")
	_, statErr := os.Stat(escaped)
	assert.True(os.IsNotExist(statErr), "no file should be written outside baseDir")
}

func TestGetMoveCancelRejectTraversalID(t *testing.T) {
	require := require.New(t)
	mgr := testManager(t)

	_, _, getErr := mgr.GetManifest("../../etc/passwd")
	require.Error(getErr, "GetManifest should reject a traversal ID")

	moveErr := mgr.MoveManifest("../../escape", StatusPending, StatusInProgress)
	require.Error(moveErr, "MoveManifest should reject a traversal ID")

	cancelErr := mgr.CancelManifest("../../escape")
	require.Error(cancelErr, "CancelManifest should reject a traversal ID")
}

func TestManifestRawFilterRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mgr, err := NewManager(t.TempDir())
	require.NoError(err, "NewManager")

	m := NewManifest("raw filter test", []string{"gm-1"})
	m.RawFilter = json.RawMessage(`{"filter":{"sender":"alice@example.com"},"dry_run":false}`)
	require.NoError(mgr.SaveManifest(m), "save")

	loaded, _, err := mgr.GetManifest(m.ID)
	require.NoError(err, "reload")
	assert.JSONEq(string(m.RawFilter), string(loaded.RawFilter), "raw filter survives round-trip")

	// Old manifests without the field load with a nil RawFilter.
	old := NewManifest("no raw filter", []string{"gm-2"})
	require.NoError(mgr.SaveManifest(old), "save old-style")
	loadedOld, _, err := mgr.GetManifest(old.ID)
	require.NoError(err, "reload old-style")
	assert.Nil(loadedOld.RawFilter, "absent field stays nil")
}

// TestManifestListIDsRoundTripAndOmitWhenEmpty verifies list IDs remain
// durable deletion provenance without changing older manifests' JSON shape.
func TestManifestListIDsRoundTripAndOmitWhenEmpty(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mgr, err := NewManager(t.TempDir())
	require.NoError(err)
	manifest := NewManifest("list filter", []string{"gm-1"})
	manifest.Filters.ListIDs = []string{"<announce.example.test>"}
	require.NoError(mgr.SaveManifest(manifest))

	claimed, err := mgr.ClaimManifest(manifest.ID, MethodTrash)
	require.NoError(err)
	require.NotNil(claimed.Execution)
	claimed.Execution.LastProcessedIndex = 1
	require.NoError(mgr.WriteInProgressCheckpoint(claimed, manifest.ID))

	loaded, status, err := mgr.GetManifestWithStatus(manifest.ID)
	require.NoError(err)
	assert.Equal(StatusInProgress, status)
	assert.Equal([]string{"<announce.example.test>"}, loaded.Filters.ListIDs)

	data, err := json.Marshal(NewManifest("no list filter", []string{"gm-2"}))
	require.NoError(err)
	assert.NotContains(string(data), "list_ids")
}

func TestGetManifestWithStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mgr, err := NewManager(t.TempDir())
	require.NoError(err, "NewManager")

	m := NewManifest("status test", []string{"gm-1"})
	require.NoError(mgr.SaveManifest(m), "save pending")

	got, status, err := mgr.GetManifestWithStatus(m.ID)
	require.NoError(err, "lookup pending")
	assert.Equal(StatusPending, status)
	assert.Equal(m.ID, got.ID)

	// Directory is authoritative even when the inline field is stale:
	// move the file to cancelled/ without rewriting the inline status.
	require.NoError(mgr.MoveManifest(m.ID, StatusPending, StatusCancelled), "move")
	_, status, err = mgr.GetManifestWithStatus(m.ID)
	require.NoError(err, "lookup cancelled")
	assert.Equal(StatusCancelled, status, "dir-derived status wins over stale inline field")

	_, _, err = mgr.GetManifestWithStatus("does-not-exist")
	require.Error(err, "missing manifest")
	assert.ErrorIs(err, ErrManifestNotFound)
}

func TestGetManifestWithStatusCorruptFile(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mgr, err := NewManager(t.TempDir())
	require.NoError(err, "NewManager")

	require.NoError(
		os.WriteFile(filepath.Join(mgr.PendingDir(), "corrupt-batch.json"), []byte("{not json"), 0o644),
		"write corrupt manifest")

	_, _, err = mgr.GetManifestWithStatus("corrupt-batch")
	require.Error(err, "corrupt manifest must error")
	assert.NotErrorIs(err, ErrManifestNotFound,
		"a corrupt existing manifest must not be reported as missing (would map to HTTP 404)")
}

func TestSaveManifestRapidDuplicateDescriptions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mgr, err := NewManager(t.TempDir())
	require.NoError(err, "NewManager")

	first := NewManifest("same description", []string{"gm-1"})
	second := NewManifest("same description", []string{"gm-2"})
	require.NotEqual(first.ID, second.ID, "same-second IDs must differ")

	require.NoError(mgr.SaveManifest(first), "save first")
	require.NoError(mgr.SaveManifest(second), "save second")

	pending, err := mgr.ListPending()
	require.NoError(err, "list pending")
	assert.Len(pending, 2, "both same-description manifests must persist")
}

func TestListPendingSkipsInvalidManifestFilename(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mgr, err := NewManager(t.TempDir())
	require.NoError(err, "NewManager")

	valid := NewManifest("valid batch", []string{"gm-1"})
	require.NoError(mgr.SaveManifest(valid), "save valid manifest")
	require.NoError(
		os.WriteFile(filepath.Join(mgr.PendingDir(), "bad.name.json"), []byte(`{"id":"bad.name"}`), 0o600),
		"write invalid manifest filename",
	)

	pending, err := mgr.ListPending()
	require.NoError(err, "list pending")
	require.Len(pending, 1)
	assert.Equal(valid.ID, pending[0].ID)
}

func TestListByStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mgr, err := NewManager(t.TempDir())
	require.NoError(err, "NewManager")

	a := NewManifest("batch a", []string{"gm-1"})
	require.NoError(mgr.SaveManifest(a), "save a")
	b := NewManifest("batch b", []string{"gm-2", "gm-3"})
	require.NoError(mgr.SaveManifest(b), "save b")
	require.NoError(mgr.CancelManifest(b.ID), "cancel b")

	pending, err := mgr.ListByStatus(StatusPending)
	require.NoError(err, "list pending")
	require.Len(pending, 1)
	assert.Equal(a.ID, pending[0].ID)
	assert.Equal(StatusPending, pending[0].Status, "normalized status")

	cancelled, err := mgr.ListByStatus(StatusCancelled)
	require.NoError(err, "list cancelled")
	require.Len(cancelled, 1)
	assert.Equal(b.ID, cancelled[0].ID)

	_, err = mgr.ListByStatus(Status("bogus"))
	assert.Error(err, "invalid status rejected")
}

func TestIsValidStatus(t *testing.T) {
	for _, s := range PersistedStatuses() {
		assert.True(t, IsValidStatus(s), "status %q", s)
	}
	assert.False(t, IsValidStatus(Status("bogus")))
	assert.False(t, IsValidStatus(Status("")))
}

// setupInProgress creates a manifest and claims it into in_progress/, returning
// the in-memory manifest with an initialized Execution.
func setupInProgress(t *testing.T, mgr *Manager, desc string) *Manifest {
	t.Helper()
	m := createTestManifest(t, mgr, desc)
	require.NoError(t, mgr.MoveManifest(m.ID, StatusPending, StatusInProgress), "MoveManifest")
	m.Status = StatusInProgress
	m.Execution = &Execution{StartedAt: time.Now(), Method: MethodTrash}
	return m
}

// assertSingleValidManifest verifies that id resolves to exactly one on-disk
// manifest, that its file is non-empty, and that it parses as a full Manifest.
// This is the invariant a torn checkpoint write would violate.
func assertSingleValidManifest(t *testing.T, mgr *Manager, id string) {
	t.Helper()
	require := require.New(t)
	found := 0
	for _, status := range persistedStatuses {
		path := filepath.Join(mgr.dirForStatus(status), id+".json")
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		found++
		require.Positive(info.Size(), "manifest %s in %s must not be empty", id, status)
		loaded, err := LoadManifest(path)
		require.NoError(err, "manifest %s in %s must be fully parseable", id, status)
		require.Equal(id, loaded.ID, "parsed manifest %s in %s has wrong ID", id, status)
	}
	require.Equal(1, found, "manifest %s must exist in exactly one status directory", id)
}

// TestManager_CheckpointCancelSerialize deterministically drives the
// rename-vs-write critical section: an in-flight checkpoint holds the
// per-manifest lock while a cancel attempts to run. The cancel must block until
// the checkpoint's atomic write completes and the lock is released, after which
// the whole valid record moves to cancelled/ — never a torn or empty file, and
// never a resurrected in_progress file.
func TestManager_CheckpointCancelSerialize(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := setupInProgress(t, mgr, "serialize")
	inProgressPath := filepath.Join(mgr.InProgressDir(), m.ID+".json")

	// Simulate an in-flight checkpoint write by holding the per-manifest lock.
	held, err := mgr.acquireManifestLock(m.ID)
	require.NoError(err, "acquireManifestLock")

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- mgr.CancelManifest(m.ID) }()

	// The cancel rename must not proceed while the checkpoint holds the lock.
	select {
	case <-cancelDone:
		require.FailNow("cancel ran while checkpoint held the lock")
	case <-time.After(150 * time.Millisecond):
	}

	// Complete the checkpoint's atomic write, then release the lock.
	m.Execution.LastProcessedIndex = 5
	require.NoError(writeManifestAtomic(m, inProgressPath), "writeManifestAtomic")
	require.NoError(held.Unlock(), "Unlock")

	require.NoError(<-cancelDone, "CancelManifest")

	// The whole valid record moved to cancelled/; in_progress/ is empty.
	_, statErr := os.Stat(inProgressPath)
	assert.True(os.IsNotExist(statErr), "in_progress file must be gone, got err=%v", statErr)
	loaded, err := LoadManifest(filepath.Join(mgr.dirForStatus(StatusCancelled), m.ID+".json"))
	require.NoError(err, "cancelled manifest must be fully parseable")
	assert.Equal(StatusCancelled, loaded.Status)
	require.NotNil(loaded.Execution, "checkpoint content preserved")
	assert.Equal(5, loaded.Execution.LastProcessedIndex, "checkpoint content preserved in cancelled record")
	assertSingleValidManifest(t, mgr, m.ID)
}

// TestManager_WriteInProgressCheckpoint_Writes verifies the happy path: a
// checkpoint on a live in_progress manifest publishes its content atomically.
func TestManager_WriteInProgressCheckpoint_Writes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := setupInProgress(t, mgr, "checkpoint happy")
	m.Execution.LastProcessedIndex = 7

	require.NoError(mgr.WriteInProgressCheckpoint(m, m.ID), "WriteInProgressCheckpoint")

	loaded, err := LoadManifest(filepath.Join(mgr.InProgressDir(), m.ID+".json"))
	require.NoError(err, "reload checkpoint")
	require.NotNil(loaded.Execution)
	assert.Equal(7, loaded.Execution.LastProcessedIndex)
	// No stray temp files left in the status directory.
	entries, err := os.ReadDir(mgr.InProgressDir())
	require.NoError(err, "ReadDir")
	for _, e := range entries {
		assert.False(strings.HasSuffix(e.Name(), ".tmp"), "leftover temp file %s", e.Name())
	}
}

// TestManager_WriteInProgressCheckpoint_Cancelled verifies that a checkpoint on
// a manifest a cancel already moved out of in_progress/ returns
// ErrManifestCancelled and writes nothing (no resurrection).
func TestManager_WriteInProgressCheckpoint_Cancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := setupInProgress(t, mgr, "checkpoint cancelled")
	require.NoError(mgr.CancelManifest(m.ID), "CancelManifest")

	err := mgr.WriteInProgressCheckpoint(m, m.ID)
	require.ErrorIs(err, ErrManifestCancelled, "checkpoint after cancel")

	_, statErr := os.Stat(filepath.Join(mgr.InProgressDir(), m.ID+".json"))
	assert.True(os.IsNotExist(statErr), "in_progress must not be resurrected, got err=%v", statErr)
	loaded, err := LoadManifest(filepath.Join(mgr.dirForStatus(StatusCancelled), m.ID+".json"))
	require.NoError(err, "cancelled manifest parseable")
	assert.Equal(StatusCancelled, loaded.Status)
	assertSingleValidManifest(t, mgr, m.ID)
}

// TestManager_FinalizeInProgress_Cancelled verifies that a finalize losing to a
// concurrent cancel returns ErrManifestCancelled rather than creating a second
// copy in a terminal directory.
func TestManager_FinalizeInProgress_Cancelled(t *testing.T) {
	mgr := testManager(t)
	m := setupInProgress(t, mgr, "finalize cancelled")
	require.NoError(t, mgr.CancelManifest(m.ID), "CancelManifest")

	err := mgr.FinalizeInProgress(m.ID, StatusCompleted)
	require.ErrorIs(t, err, ErrManifestCancelled, "finalize after cancel")
	assertSingleValidManifest(t, mgr, m.ID)
}

// TestManager_CheckpointCancelStress races repeated checkpoint writes against a
// cancel on the same manifest ID. Regardless of interleaving, the manifest must
// always end up as exactly one fully valid, non-empty on-disk record — never
// torn, empty, resurrected, or duplicated. Run with -race to surface data races
// in the locked critical sections.
func TestManager_CheckpointCancelStress(t *testing.T) {
	for range 50 {
		mgr := testManager(t)
		m := setupInProgress(t, mgr, "stress")
		id := m.ID

		// The checkpoint goroutine mutates its own copy of the manifest.
		cp := *m
		exec := *m.Execution
		cp.Execution = &exec

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 20 {
				cp.Execution.LastProcessedIndex = i
				if err := mgr.WriteInProgressCheckpoint(&cp, id); err != nil {
					// assert (not require) inside a goroutine: require's FailNow
					// is illegal off the test's own goroutine.
					assert.ErrorIs(t, err, ErrManifestCancelled, "checkpoint error")
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			assert.NoError(t, mgr.CancelManifest(id), "CancelManifest")
		}()
		wg.Wait()

		assertSingleValidManifest(t, mgr, id)
	}
}

// TestManager_ClaimManifest_PersistsInProgressState verifies that claiming a
// pending manifest publishes both Status=InProgress and a non-nil Execution
// to in_progress/<id>.json on disk — not just to the returned in-memory
// struct. This closes the crash-before-checkpoint gap: reloading the file
// from disk must never still say "pending" once ClaimManifest has returned.
func TestManager_ClaimManifest_PersistsInProgressState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := createTestManifest(t, mgr, "claim persists")

	claimed, err := mgr.ClaimManifest(m.ID, MethodTrash)
	require.NoError(err, "ClaimManifest")
	assert.Equal(StatusInProgress, claimed.Status)
	require.NotNil(claimed.Execution, "in-memory Execution")

	loaded, err := LoadManifest(filepath.Join(mgr.InProgressDir(), m.ID+".json"))
	require.NoError(err, "reload claimed manifest from disk")
	assert.Equal(StatusInProgress, loaded.Status, "on-disk status must not still say pending")
	require.NotNil(loaded.Execution, "on-disk Execution")
	assert.Equal(MethodTrash, loaded.Execution.Method)

	_, statErr := os.Stat(filepath.Join(mgr.PendingDir(), m.ID+".json"))
	assert.True(os.IsNotExist(statErr), "pending file must be removed after claim")
	assertSingleValidManifest(t, mgr, m.ID)
}

// TestManager_ClaimManifest_CrashThenResume simulates a crash immediately
// after ClaimManifest claims a pending manifest, before any checkpoint is
// written, by discarding the returned in-memory result and reloading purely
// from what is durable on disk. A subsequent claim (as a restarted CLI
// process would issue) must resume successfully using only the in_progress
// file, without depending on the now-removed pending file.
func TestManager_ClaimManifest_CrashThenResume(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := createTestManifest(t, mgr, "claim crash resume")

	_, err := mgr.ClaimManifest(m.ID, MethodTrash)
	require.NoError(err, "initial ClaimManifest")

	// Simulate a crash: reload strictly from what is durable on disk.
	manifest, status, err := mgr.GetManifestWithStatus(m.ID)
	require.NoError(err, "GetManifestWithStatus after simulated crash")
	assert.Equal(StatusInProgress, status)
	require.NotNil(manifest.Execution, "Execution survives a crash before the first checkpoint")

	resumed, err := mgr.ClaimManifest(m.ID, MethodTrash)
	require.NoError(err, "resume ClaimManifest")
	assert.Equal(StatusInProgress, resumed.Status)
	require.NotNil(resumed.Execution)
	assertSingleValidManifest(t, mgr, m.ID)
}

// TestManager_ClaimManifest_ResumePreservesProgress verifies that claiming an
// already in_progress manifest with existing checkpoint progress is a resume:
// LastProcessedIndex/Succeeded/Failed and the original Method are preserved,
// rather than Execution being reset to a fresh StartedAt with zeroed counters.
func TestManager_ClaimManifest_ResumePreservesProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := createTestManifest(t, mgr, "claim resume progress")
	require.NoError(mgr.MoveManifest(m.ID, StatusPending, StatusInProgress), "MoveManifest")
	original := &Execution{
		StartedAt:          time.Now().Add(-time.Hour).Truncate(time.Second),
		Method:             MethodTrash,
		Succeeded:          7,
		Failed:             1,
		FailedIDs:          []string{"msg-x"},
		LastProcessedIndex: 8,
	}
	m.Status = StatusInProgress
	m.Execution = original
	require.NoError(
		writeManifestAtomic(m, filepath.Join(mgr.InProgressDir(), m.ID+".json")),
		"seed in_progress state",
	)

	// Resume with a different method argument: it must be ignored because an
	// Execution already exists.
	resumed, err := mgr.ClaimManifest(m.ID, MethodDelete)
	require.NoError(err, "ClaimManifest resume")
	assert.Equal(StatusInProgress, resumed.Status)
	require.NotNil(resumed.Execution)
	assert.Equal(7, resumed.Execution.Succeeded, "Succeeded preserved")
	assert.Equal(1, resumed.Execution.Failed, "Failed preserved")
	assert.Equal(8, resumed.Execution.LastProcessedIndex, "LastProcessedIndex preserved")
	assert.Equal(MethodTrash, resumed.Execution.Method, "Method preserved, not overwritten by resume arg")
	assert.True(
		original.StartedAt.Equal(resumed.Execution.StartedAt),
		"StartedAt not reset: want %v, got %v", original.StartedAt, resumed.Execution.StartedAt,
	)
}

// TestManager_ClaimManifest_ResumeIgnoresStaleInlineStatus verifies that a
// manifest file living in in_progress/ is claimed as a resume based on its
// directory location, even when its serialized Status field still says
// "pending" — exactly the state a crash before the first checkpoint used to
// leave behind. The directory is authoritative, consistent with
// FinalizeInProgress and GetManifestWithStatus.
func TestManager_ClaimManifest_ResumeIgnoresStaleInlineStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := createTestManifest(t, mgr, "stale inline status")
	require.NoError(mgr.MoveManifest(m.ID, StatusPending, StatusInProgress), "MoveManifest")

	onDiskBefore, err := LoadManifest(filepath.Join(mgr.InProgressDir(), m.ID+".json"))
	require.NoError(err, "reload before claim")
	require.Equal(StatusPending, onDiskBefore.Status, "precondition: inline status is stale")

	claimed, err := mgr.ClaimManifest(m.ID, MethodTrash)
	require.NoError(err, "ClaimManifest on directory-in_progress/inline-pending manifest")
	assert.Equal(StatusInProgress, claimed.Status)
	require.NotNil(claimed.Execution)

	loaded, err := LoadManifest(filepath.Join(mgr.InProgressDir(), m.ID+".json"))
	require.NoError(err, "reload after claim")
	assert.Equal(StatusInProgress, loaded.Status, "inline status corrected on disk")
}

// TestManager_ClaimManifest_CancelledMarker verifies that a durable
// cancelled/ marker wins: claiming returns ErrManifestCancelled and does not
// create or resurrect an in_progress file.
func TestManager_ClaimManifest_CancelledMarker(t *testing.T) {
	mgr := testManager(t)
	m := createTestManifest(t, mgr, "claim vs cancel marker")
	require.NoError(t, mgr.CancelManifest(m.ID), "CancelManifest")

	_, err := mgr.ClaimManifest(m.ID, MethodTrash)
	require.ErrorIs(t, err, ErrManifestCancelled, "ClaimManifest after cancel")

	_, statErr := os.Stat(filepath.Join(mgr.InProgressDir(), m.ID+".json"))
	assert.True(t, os.IsNotExist(statErr), "in_progress file must not be created")
	assertSingleValidManifest(t, mgr, m.ID)
}

// TestManager_ClaimManifest_TerminalOrAbsent verifies that claiming a
// completed, failed, or nonexistent manifest returns an error distinct from
// ErrManifestCancelled.
func TestManager_ClaimManifest_TerminalOrAbsent(t *testing.T) {
	require := require.New(t)
	mgr := testManager(t)

	completed := createTestManifest(t, mgr, "claim completed")
	require.NoError(mgr.MoveManifest(completed.ID, StatusPending, StatusInProgress), "move to in_progress")
	require.NoError(mgr.FinalizeInProgress(completed.ID, StatusCompleted), "finalize completed")
	_, err := mgr.ClaimManifest(completed.ID, MethodTrash)
	require.Error(err, "ClaimManifest on completed manifest")
	require.NotErrorIs(err, ErrManifestCancelled, "completed is not the same as cancelled")

	failed := createTestManifest(t, mgr, "claim failed")
	require.NoError(mgr.MoveManifest(failed.ID, StatusPending, StatusInProgress), "move to in_progress")
	require.NoError(mgr.FinalizeInProgress(failed.ID, StatusFailed), "finalize failed")
	_, err = mgr.ClaimManifest(failed.ID, MethodTrash)
	require.Error(err, "ClaimManifest on failed manifest")

	_, err = mgr.ClaimManifest("does-not-exist", MethodTrash)
	require.Error(err, "ClaimManifest on absent manifest")
}

// TestManager_FinalizeInProgress_RemovesStalePending simulates a crash in
// claimPendingManifest's publish/remove window (in_progress/<id>.json written,
// pending/<id>.json not yet removed) followed by a successful execution.
// FinalizeInProgress must remove the stale pending copy so a later
// ClaimManifest cannot reclaim and re-execute the completed deletion.
func TestManager_FinalizeInProgress_RemovesStalePending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := createTestManifest(t, mgr, "stale pending finalize")

	// Simulate the crash window: publish the initialized in_progress copy
	// exactly as claimPendingManifest does, but leave the pending file behind.
	m.Status = StatusInProgress
	m.Execution = &Execution{StartedAt: time.Now(), Method: MethodTrash}
	require.NoError(
		writeManifestAtomic(m, filepath.Join(mgr.InProgressDir(), m.ID+".json")),
		"publish in_progress copy",
	)
	_, err := os.Stat(filepath.Join(mgr.PendingDir(), m.ID+".json"))
	require.NoError(err, "precondition: stale pending copy present")

	require.NoError(mgr.FinalizeInProgress(m.ID, StatusCompleted), "FinalizeInProgress")

	_, statErr := os.Stat(filepath.Join(mgr.PendingDir(), m.ID+".json"))
	assert.True(os.IsNotExist(statErr), "stale pending copy removed by finalize, got err=%v", statErr)

	_, err = mgr.ClaimManifest(m.ID, MethodTrash)
	require.ErrorContains(err, "completed", "completed manifest must not be reclaimable")
	assertSingleValidManifest(t, mgr, m.ID)
}

// TestManager_GetManifest_PrefersInProgressOverStalePending verifies that
// by-ID lookups return the authoritative in_progress record when a claim
// crash left a stale pending copy beside it: the in_progress copy carries the
// real execution state (method, checkpoint progress) that planning relies on.
func TestManager_GetManifest_PrefersInProgressOverStalePending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := createTestManifest(t, mgr, "lookup stale pending")

	// Simulate the crash window: publish the initialized in_progress copy
	// exactly as claimPendingManifest does, but leave the pending file behind.
	m.Status = StatusInProgress
	m.Execution = &Execution{StartedAt: time.Now(), Method: MethodTrash, LastProcessedIndex: 3}
	require.NoError(
		writeManifestAtomic(m, filepath.Join(mgr.InProgressDir(), m.ID+".json")),
		"publish in_progress copy",
	)

	loaded, path, err := mgr.GetManifest(m.ID)
	require.NoError(err, "GetManifest")
	assert.Equal(filepath.Join(mgr.InProgressDir(), m.ID+".json"), path, "in_progress path wins")
	require.NotNil(loaded.Execution, "execution state from the in_progress record")
	assert.Equal(MethodTrash, loaded.Execution.Method)

	withStatus, status, err := mgr.GetManifestWithStatus(m.ID)
	require.NoError(err, "GetManifestWithStatus")
	assert.Equal(StatusInProgress, status, "directory-derived status prefers in_progress")
	require.NotNil(withStatus.Execution)
	assert.Equal(3, withStatus.Execution.LastProcessedIndex)
}

// TestManager_CancelManifest_PrefersInProgressOverStalePending simulates a
// crash in claimPendingManifest's publish/remove window (in_progress written,
// pending not yet removed) followed by a cancel. The cancel must move the
// authoritative in_progress record to cancelled/ and remove the stale pending
// copy — cancelling the pending copy instead would orphan the in_progress
// file, so lookups would keep reporting the batch as in progress.
func TestManager_CancelManifest_PrefersInProgressOverStalePending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := createTestManifest(t, mgr, "cancel stale pending")

	// Simulate the crash window: publish the initialized in_progress copy
	// exactly as claimPendingManifest does, but leave the pending file behind.
	m.Status = StatusInProgress
	m.Execution = &Execution{StartedAt: time.Now(), Method: MethodTrash}
	require.NoError(
		writeManifestAtomic(m, filepath.Join(mgr.InProgressDir(), m.ID+".json")),
		"publish in_progress copy",
	)
	_, err := os.Stat(filepath.Join(mgr.PendingDir(), m.ID+".json"))
	require.NoError(err, "precondition: stale pending copy present")

	require.NoError(mgr.CancelManifest(m.ID), "CancelManifest")

	_, statErr := os.Stat(filepath.Join(mgr.InProgressDir(), m.ID+".json"))
	assert.True(os.IsNotExist(statErr), "in_progress record moved to cancelled, got err=%v", statErr)
	_, statErr = os.Stat(filepath.Join(mgr.PendingDir(), m.ID+".json"))
	assert.True(os.IsNotExist(statErr), "stale pending copy removed, got err=%v", statErr)

	loaded, status, err := mgr.GetManifestWithStatus(m.ID)
	require.NoError(err, "GetManifestWithStatus after cancel")
	assert.Equal(StatusCancelled, status, "lookups must not report the batch as in progress")
	require.NotNil(loaded.Execution, "cancelled record keeps the initialized execution state")
	assertSingleValidManifest(t, mgr, m.ID)
}

// TestManager_ClaimManifest_TerminalWinsOverStalePending covers the guard for
// a stale pending copy that survives even finalization (a second crash between
// FinalizeInProgress's rename and its pending cleanup): with both
// pending/<id>.json and completed/<id>.json present, ClaimManifest must refuse
// to re-execute the deletion and remove the stale pending copy.
func TestManager_ClaimManifest_TerminalWinsOverStalePending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mgr := testManager(t)
	m := createTestManifest(t, mgr, "terminal wins stale pending")

	completed := *m
	completed.Status = StatusCompleted
	require.NoError(
		completed.Save(filepath.Join(mgr.CompletedDir(), m.ID+".json")),
		"seed completed record alongside stale pending copy",
	)

	_, err := mgr.ClaimManifest(m.ID, MethodTrash)
	require.ErrorContains(err, "completed", "ClaimManifest with terminal record present")

	_, statErr := os.Stat(filepath.Join(mgr.PendingDir(), m.ID+".json"))
	assert.True(os.IsNotExist(statErr), "stale pending copy removed by claim guard, got err=%v", statErr)
	_, statErr = os.Stat(filepath.Join(mgr.InProgressDir(), m.ID+".json"))
	assert.True(os.IsNotExist(statErr), "no in_progress file created")
	assertSingleValidManifest(t, mgr, m.ID)
}

// TestManager_ClaimCancelStress races repeated claim attempts against a
// concurrent cancel on the same manifest ID. Regardless of interleaving, the
// manifest must end up in exactly one status directory: either the claim wins
// the lock first (landing the file in in_progress/, which the cancel then
// moves on to cancelled/) or the cancel wins first (the claim then observes
// the durable cancelled/ marker and returns ErrManifestCancelled without
// creating anything). Run with -race to surface data races in the locked
// critical sections.
func TestManager_ClaimCancelStress(t *testing.T) {
	for range 50 {
		mgr := testManager(t)
		m := createTestManifest(t, mgr, "claim cancel stress")
		id := m.ID

		var wg sync.WaitGroup
		wg.Add(2)
		claimErrs := make(chan error, 1)
		go func() {
			defer wg.Done()
			_, err := mgr.ClaimManifest(id, MethodTrash)
			claimErrs <- err
		}()
		go func() {
			defer wg.Done()
			// assert (not require) inside a goroutine: require's FailNow is
			// illegal off the test's own goroutine.
			assert.NoError(t, mgr.CancelManifest(id), "CancelManifest")
		}()
		wg.Wait()

		if claimErr := <-claimErrs; claimErr != nil {
			require.ErrorIs(t, claimErr, ErrManifestCancelled, "claim error")
		}
		assertSingleValidManifest(t, mgr, id)
	}
}
