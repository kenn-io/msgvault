package deletion

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// testManager creates a Manager in a temp directory for testing.
func testManager(t *testing.T) *Manager {
	t.Helper()
	mgr, err := NewManager(filepath.Join(t.TempDir(), "deletions"))
	requirepkg.NoError(t, err, "NewManager()")
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
	requirepkg.NoError(t, err, "CreateManifest(%q)", desc)
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
	assertpkg.Empty(t, diff, "Manifest mismatch (-want +got):\n%s", diff)
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
		requirepkg.Failf(t, "unknown state", "%q", state)
	}
	path := filepath.Join(dir, id+".json")
	_, err := os.Stat(path)
	assertpkg.NoError(t, err, "manifest %q not found in %s directory", id, state)
}

// assertSummaryContains checks that summary contains all specified substrings.
func assertSummaryContains(t *testing.T, summary string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		assertpkg.Contains(t, summary, part, "summary missing %q", part)
	}
}

// assertSummaryNotContains checks that summary does not contain any of the specified substrings.
func assertSummaryNotContains(t *testing.T, summary string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		assertpkg.NotContains(t, summary, part, "summary should not contain %q", part)
	}
}

// assertListCount calls a list function and asserts the returned slice length.
func assertListCount(t *testing.T, listFn func() ([]*Manifest, error), want int) {
	t.Helper()
	got, err := listFn()
	requirepkg.NoError(t, err, "list error")
	requirepkg.Len(t, got, want, "list returned %d manifests, want %d", len(got), want)
}

// writeFile is a test helper that writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	requirepkg.NoError(t, os.WriteFile(path, []byte(content), 0644), "WriteFile(%s)", path)
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
		assertpkg.Equal(t, tc.want, sanitizeForFilename(tc.input), "sanitizeForFilename(%q)", tc.input)
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
				assertpkg.NotEmpty(t, id, "generateID returned empty string")
				parts := strings.SplitN(id, "-", 3)
				assertpkg.GreaterOrEqual(t, len(parts), 2, "expected timestamp-description format, got %q", id)
			},
		},
		{
			name: "long description truncated",
			desc: "this is a very long description that exceeds twenty characters",
			validate: func(t *testing.T, id string) {
				assertpkg.LessOrEqual(t, len(id), 40, "ID too long: %d chars (%q)", len(id), id)
			},
		},
		{
			name: "empty description uses batch",
			desc: "",
			validate: func(t *testing.T, id string) {
				assertpkg.True(t, strings.HasSuffix(id, "-batch"), "expected -batch suffix, got %q", id)
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
	assert := assertpkg.New(t)
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

	requirepkg.NoError(t, m.Save(path), "Save()")

	// Load and verify
	loaded, err := LoadManifest(path)
	requirepkg.NoError(t, err, "LoadManifest()")

	AssertManifestEqual(t, loaded, m)
}

func TestManifest_Save_CreatesDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "subdir", "nested", "manifest.json")

	m := BuildManifest(t, "nested test", "id1").Build()
	requirepkg.NoError(t, m.Save(path), "Save() with nested path")

	// Verify file exists
	_, err := os.Stat(path)
	assertpkg.False(t, os.IsNotExist(err), "File was not created")
}

func TestLoadManifest_NotFound(t *testing.T) {
	_, err := LoadManifest("/nonexistent/path/manifest.json")
	assertpkg.Error(t, err, "LoadManifest() should error for nonexistent file")
}

func TestLoadManifest_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "invalid.json")
	writeFile(t, path, "not valid json")

	_, err := LoadManifest(path)
	assertpkg.Error(t, err, "LoadManifest() should error for invalid JSON")
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
				now := time.Now()
				return BuildManifest(t, "exec test", "id1").
					WithExecution(MethodTrash, 10, 2, &now).Build()
			},
			wantContains: []string{"Execution:", "Method: trash", "Succeeded: 10", "Failed: 2", "Completed:"},
		},
		{
			name: "empty date range",
			setupManifest: func(t *testing.T) *Manifest {
				return BuildManifest(t, "empty date test", "id1").
					WithSummary(1, 1024, [2]string{"", ""}, nil).Build()
			},
			wantNotContains: []string{"Date Range:"},
		},
		{
			name: "nil summary",
			setupManifest: func(t *testing.T) *Manifest {
				return BuildManifest(t, "no summary test").WithoutSummary().Build()
			},
			wantContains:    []string{"Messages: 2"},
			wantNotContains: []string{"Total Size:"},
		},
		{
			name: "many top senders truncated to 10",
			setupManifest: func(t *testing.T) *Manifest {
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
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "deletions")

	mgr, err := NewManager(baseDir)
	requirepkg.NoError(t, err, "NewManager()")

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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	requirepkg.NoError(t, err, "GetManifest()")
	AssertManifestEqual(t, loaded, m)
	assertpkg.NotEmpty(t, path, "GetManifest() returned empty path")
}

func TestManager_GetManifest_NotFound(t *testing.T) {
	mgr := testManager(t)

	_, _, err := mgr.GetManifest("nonexistent-id")
	requirepkg.Error(t, err, "GetManifest() should error for nonexistent manifest")
	assertpkg.Contains(t, err.Error(), "not found")
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

			assertpkg.Equal(t, tc.wantErr, err != nil, "MoveManifest() error = %v, wantErr %v", err, tc.wantErr)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	requirepkg.Error(t, err, "CancelManifest() should error for nonexistent manifest")
	assertpkg.Contains(t, err.Error(), "not found")
}

func TestManager_SaveManifest(t *testing.T) {
	mgr := testManager(t)

	// Create manifest with each status and verify placement
	statuses := []Status{StatusPending, StatusInProgress, StatusCompleted, StatusFailed}
	for _, status := range statuses {
		m := NewManifest("status-"+string(status), []string{"a"})
		m.Status = status

		requirepkg.NoError(t, mgr.SaveManifest(m), "SaveManifest(%s)", status)

		// Verify it can be found
		loaded, _, err := mgr.GetManifest(m.ID)
		requirepkg.NoError(t, err, "GetManifest(%s) after SaveManifest", status)
		assertpkg.Equal(t, status, loaded.Status)
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
	requirepkg.NoError(t, os.MkdirAll(dirPath, 0755), "MkdirAll(subdir.json)")

	// ListPending should only return the valid manifest
	assertListCount(t, mgr.ListPending, 1)
}

func TestStatus_Values(t *testing.T) {
	assert := assertpkg.New(t)
	// Verify status constants
	assert.Equal(Status("pending"), StatusPending)
	assert.Equal(Status("in_progress"), StatusInProgress)
	assert.Equal(Status("completed"), StatusCompleted)
	assert.Equal(Status("failed"), StatusFailed)
	assert.Equal(Status("cancelled"), StatusCancelled)
}

func TestMethod_Values(t *testing.T) {
	assertpkg.Equal(t, Method("trash"), MethodTrash)
	assertpkg.Equal(t, Method("delete"), MethodDelete)
}

// TestStatusDirMap verifies that statusDirMap contains all persisted statuses
// and maps them to the expected directory names.
func TestStatusDirMap(t *testing.T) {
	assert := assertpkg.New(t)
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
			assertpkg.True(t, strings.HasSuffix(got, wantSuffix), "dirForStatus(%q) = %q, want suffix %q", tc.status, got, wantSuffix)
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
		assertpkg.Contains(t, persistedStatuses, status, "Status %q should be in persistedStatuses but is not", status)
	}

	// StatusCancelled MUST be in persistedStatuses; cancelled manifests
	// are persisted on disk for audit (per spec § Manifest format).
	assertpkg.Contains(t, persistedStatuses, StatusCancelled, "StatusCancelled missing from persistedStatuses; cancelled manifests must persist on disk")
}

func TestManager_ListCancelled(t *testing.T) {
	require := requirepkg.New(t)
	mgr := testManager(t)

	manifest := createTestManifest(t, mgr, "test cancel")
	require.NoError(mgr.CancelManifest(manifest.ID), "CancelManifest")
	got, err := mgr.ListCancelled()
	require.NoError(err, "ListCancelled")
	require.Len(got, 1)
	assertpkg.Equal(t, manifest.ID, got[0].ID)
}

// TestManager_SaveManifest_UnknownStatus tests saving with an unknown status.
func TestManager_SaveManifest_UnknownStatus(t *testing.T) {
	mgr := testManager(t)

	m := NewManifest("unknown status", []string{"a"})
	m.Status = "invalid_status" // Unknown status

	// Should still save (to pending dir by default)
	requirepkg.NoError(t, mgr.SaveManifest(m), "SaveManifest() with unknown status")

	// Should be findable (saved to pending by default)
	loaded, _, err := mgr.GetManifest(m.ID)
	requirepkg.NoError(t, err, "GetManifest()")
	assertpkg.Equal(t, Status("invalid_status"), loaded.Status)
}

// TestManager_ListManifests_NonexistentDir tests listing from a nonexistent directory.
func TestManager_ListManifests_NonexistentDir(t *testing.T) {
	mgr := testManager(t)

	// Remove the pending directory
	requirepkg.NoError(t, os.RemoveAll(mgr.PendingDir()), "RemoveAll()")

	// ListPending should return empty (not error) for nonexistent dir
	assertListCount(t, mgr.ListPending, 0)
}

// TestManifest_Save_FilePermissions verifies that manifest files are saved with
// restrictive permissions (0600) to protect Gmail message IDs.
func TestManifest_Save_FilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "permission-test.json")

	m := newTestManifest(t, "permission test", "msg1", "msg2")
	requirepkg.NoError(t, m.Save(path), "Save()")

	info, err := os.Stat(path)
	requirepkg.NoError(t, err, "Stat()")

	// File should have 0600 permissions (owner read/write only)
	// Windows does not support Unix permissions.
	if runtime.GOOS != "windows" {
		assertpkg.Equal(t, os.FileMode(0600), info.Mode().Perm(), "file permissions")
	}
}
