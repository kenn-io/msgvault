package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
)

// ControllerTestEnv encapsulates common setup for ActionController tests.
type ControllerTestEnv struct {
	t    *testing.T
	Ctrl *ActionController
	Dir  string
	Mgr  *deletion.Manager
}

// NewControllerTestEnv creates a ControllerTestEnv with a temporary directory
// and deletion manager wired to the given engine.
func NewControllerTestEnv(t *testing.T, engine *querytest.MockEngine) *ControllerTestEnv {
	t.Helper()
	dir := t.TempDir()
	mgr, err := deletion.NewManager(filepath.Join(dir, "deletions"))
	requirepkg.NoError(t, err, "NewManager")
	return &ControllerTestEnv{
		t:    t,
		Ctrl: NewActionController(engine, dir, mgr),
		Dir:  dir,
		Mgr:  mgr,
	}
}

func newTestEnv(t *testing.T, gmailIDs ...string) *ControllerTestEnv {
	t.Helper()
	return NewControllerTestEnv(t, &querytest.MockEngine{GmailIDs: gmailIDs})
}

type stageArgs struct {
	aggregates      map[string]bool
	selection       map[int64]bool
	view            query.ViewType
	accountID       *int64
	accounts        []query.AccountInfo
	timeGranularity query.TimeGranularity
	messages        []query.MessageSummary
	drillFilter     *query.MessageFilter
}

// StageForDeletion is a test helper that calls the controller's StageForDeletion
// method with sensible defaults, failing the test on error.
func (e *ControllerTestEnv) StageForDeletion(args stageArgs) *deletion.Manifest {
	e.t.Helper()
	granularity := args.timeGranularity
	if granularity == 0 {
		granularity = query.TimeYear
	}
	manifest, err := e.Ctrl.StageForDeletion(DeletionContext{
		AggregateSelection: args.aggregates,
		MessageSelection:   args.selection,
		AggregateViewType:  args.view,
		AccountFilter:      args.accountID,
		Accounts:           args.accounts,
		TimeGranularity:    granularity,
		Messages:           args.messages,
		DrillFilter:        args.drillFilter,
	})
	requirepkg.NoError(e.t, err)
	return manifest
}

func msgSummary(id int64, sourceID string) query.MessageSummary {
	return query.MessageSummary{ID: id, SourceMessageID: sourceID}
}

func TestStageForDeletion_FromAggregateSelection(t *testing.T) {
	env := newTestEnv(t, "gid1", "gid2", "gid3")

	manifest := env.StageForDeletion(stageArgs{
		aggregates: testutil.MakeSet("alice@example.com"),
		view:       query.ViewSenders,
	})

	assertpkg.Len(t, manifest.GmailIDs, 3)
	assertpkg.Equal(t, []string{"alice@example.com"}, manifest.Filters.Senders)
	assertpkg.Equal(t, "tui", manifest.CreatedBy)
}

func TestStageForDeletion_FromMessageSelection(t *testing.T) {
	env := newTestEnv(t)

	messages := []query.MessageSummary{
		msgSummary(10, "gid_a"),
		msgSummary(20, "gid_b"),
		msgSummary(30, "gid_c"),
	}

	manifest := env.StageForDeletion(stageArgs{
		selection: testutil.MakeSet[int64](10, 30),
		view:      query.ViewSenders,
		messages:  messages,
	})

	assertpkg.ElementsMatch(t, []string{"gid_a", "gid_c"}, manifest.GmailIDs)
}

func TestStageForDeletion_NoSelection(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.Ctrl.StageForDeletion(DeletionContext{
		AggregateViewType: query.ViewSenders,
		TimeGranularity:   query.TimeYear,
	})
	requirepkg.Error(t, err)
}

func TestStageForDeletion_MultipleAggregates_DeterministicFilter(t *testing.T) {
	env := newTestEnv(t, "gid1")

	agg := testutil.MakeSet("charlie@example.com", "alice@example.com", "bob@example.com")

	for range 10 {
		manifest := env.StageForDeletion(stageArgs{aggregates: agg, view: query.ViewSenders})
		assertpkg.Equal(t, []string{"alice@example.com", "bob@example.com", "charlie@example.com"}, manifest.Filters.Senders)
	}
}

func TestStageForDeletion_ViewTypes(t *testing.T) {
	tests := []struct {
		name     string
		viewType query.ViewType
		key      string
		check    func(t *testing.T, f deletion.Filters)
	}{
		{"senders", query.ViewSenders, "a@b.com", func(t *testing.T, f deletion.Filters) {
			assertpkg.Equal(t, []string{"a@b.com"}, f.Senders)
		}},
		{"recipients", query.ViewRecipients, "c@d.com", func(t *testing.T, f deletion.Filters) {
			assertpkg.Equal(t, []string{"c@d.com"}, f.Recipients)
		}},
		{"domains", query.ViewDomains, "example.com", func(t *testing.T, f deletion.Filters) {
			assertpkg.Equal(t, []string{"example.com"}, f.SenderDomains)
		}},
		{"labels", query.ViewLabels, "INBOX", func(t *testing.T, f deletion.Filters) {
			assertpkg.Equal(t, []string{"INBOX"}, f.Labels)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, "gid1")

			manifest := env.StageForDeletion(stageArgs{
				aggregates: testutil.MakeSet(tt.key),
				view:       tt.viewType,
			})
			tt.check(t, manifest.Filters)
		})
	}
}

func TestStageForDeletion_AccountFilter(t *testing.T) {
	env := newTestEnv(t, "gid1")

	accountID := int64(42)
	accounts := []query.AccountInfo{
		{ID: 42, Identifier: "test@gmail.com"},
	}

	manifest := env.StageForDeletion(stageArgs{
		aggregates: testutil.MakeSet("sender@x.com"),
		view:       query.ViewSenders,
		accountID:  &accountID,
		accounts:   accounts,
	})
	assertpkg.Equal(t, "test@gmail.com", manifest.Filters.Account)
}

func TestStageForDeletion_DrillFilterApplied(t *testing.T) {
	// Simulate: drill into sender "alice@example.com", switch to time view,
	// select "2024-01". The filter should include both sender AND time period.
	var capturedFilter query.MessageFilter
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, f query.MessageFilter) ([]string, error) {
			capturedFilter = f
			return []string{"gid1", "gid2"}, nil
		},
	}
	env := NewControllerTestEnv(t, engine)

	drillFilter := &query.MessageFilter{
		Sender: "alice@example.com",
	}

	manifest := env.StageForDeletion(stageArgs{
		aggregates:  testutil.MakeSet("2024-01"),
		view:        query.ViewTime,
		drillFilter: drillFilter,
	})

	// Verify the filter passed to the engine includes both drill context and selection
	assertpkg.Equal(t, "alice@example.com", capturedFilter.Sender)
	assertpkg.Equal(t, "2024-01", capturedFilter.TimeRange.Period)
	assertpkg.Len(t, manifest.GmailIDs, 2)
}

func TestStageForDeletion_NoDrillFilter(t *testing.T) {
	// Without drill filter, only the aggregate selection filter is applied.
	var capturedFilter query.MessageFilter
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, f query.MessageFilter) ([]string, error) {
			capturedFilter = f
			return []string{"gid1"}, nil
		},
	}
	env := NewControllerTestEnv(t, engine)

	env.StageForDeletion(stageArgs{
		aggregates: testutil.MakeSet("2024-01"),
		view:       query.ViewTime,
	})

	assertpkg.Empty(t, capturedFilter.Sender)
	assertpkg.Equal(t, "2024-01", capturedFilter.TimeRange.Period)
}

func TestExportAttachments_NilDetail(t *testing.T) {
	env := newTestEnv(t)
	cmd := env.Ctrl.ExportAttachments(nil, nil)
	assertpkg.Nil(t, cmd, "expected nil cmd for nil detail")
}

func TestExportAttachments_NoSelection(t *testing.T) {
	env := newTestEnv(t)
	detail := &query.MessageDetail{
		Attachments: []query.AttachmentInfo{
			{ID: 1, Filename: "file.pdf", ContentHash: "abc123"},
		},
	}
	cmd := env.Ctrl.ExportAttachments(detail, map[int]bool{})
	assertpkg.Nil(t, cmd, "expected nil cmd for empty selection")
}

func TestExportAttachments_ErrBehavior(t *testing.T) {
	tests := []struct {
		name        string
		attachments []query.AttachmentInfo
		wantErr     bool
	}{
		{
			name: "invalid content hash sets Err",
			attachments: []query.AttachmentInfo{
				{ID: 1, Filename: "file.pdf", ContentHash: ""},
			},
			wantErr: true,
		},
		{
			name: "missing file sets Err",
			attachments: []query.AttachmentInfo{
				{ID: 1, Filename: "file.pdf", ContentHash: "abc123def456"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := requirepkg.New(t)
			assert := assertpkg.New(t)
			env := newTestEnv(t)
			detail := &query.MessageDetail{
				ID:          1,
				Subject:     "Test",
				Attachments: tt.attachments,
			}
			selection := make(map[int]bool)
			for i := range tt.attachments {
				selection[i] = true
			}

			cmd := env.Ctrl.ExportAttachments(detail, selection)
			require.NotNil(cmd)

			msg := cmd()
			result, ok := msg.(ExportResultMsg)
			require.True(ok, "expected ExportResultMsg, got %T", msg)

			if tt.wantErr {
				assert.Error(result.Err)
			} else {
				assert.NoError(result.Err)
			}
		})
	}
}

func TestExportAttachments_PartialSuccess(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Partial success: one valid file exports, one missing file fails.
	// Err should be nil because stats.Count > 0 (some files succeeded).
	env := newTestEnv(t)

	// Clean up the zip file that gets created in current directory.
	// TODO: ExportAttachments should write to a configurable output directory.
	t.Cleanup(func() { _ = os.Remove("Test_1.zip") })

	// Create a valid attachment file (must be valid 64-char hex SHA-256 hash)
	validHash := "abc123def456abc123def456abc123def456abc123def456abc123def456abc1"
	missingHash := "def456abc123def456abc123def456abc123def456abc123def456abc123def4"
	attachmentsDir := filepath.Join(env.Dir, "attachments")
	hashDir := filepath.Join(attachmentsDir, validHash[:2])
	require.NoError(os.MkdirAll(hashDir, 0o755), "failed to create hash dir")
	require.NoError(os.WriteFile(filepath.Join(hashDir, validHash), []byte("test content"), 0o644), "failed to write attachment")

	detail := &query.MessageDetail{
		ID:      1,
		Subject: "Test",
		Attachments: []query.AttachmentInfo{
			{ID: 1, Filename: "valid.pdf", ContentHash: validHash},
			{ID: 2, Filename: "missing.pdf", ContentHash: missingHash},
		},
	}
	selection := map[int]bool{0: true, 1: true}

	cmd := env.Ctrl.ExportAttachments(detail, selection)
	require.NotNil(cmd)

	msg := cmd()
	result, ok := msg.(ExportResultMsg)
	require.True(ok, "expected ExportResultMsg, got %T", msg)

	// Partial success should NOT set Err
	assert.NoError(result.Err, "expected Err to be nil for partial success")

	// Result should contain both success info and error details
	assert.NotEmpty(result.Result)
}

func TestExportAttachments_FullSuccess(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Full success: all attachments export without errors.
	env := newTestEnv(t)

	// Clean up the zip file that gets created in current directory.
	// TODO: ExportAttachments should write to a configurable output directory.
	t.Cleanup(func() { _ = os.Remove("Test_1.zip") })

	// Create a valid attachment file (must be valid 64-char hex SHA-256 hash)
	validHash := "abc123def456abc123def456abc123def456abc123def456abc123def456abc1"
	attachmentsDir := filepath.Join(env.Dir, "attachments")
	hashDir := filepath.Join(attachmentsDir, validHash[:2])
	require.NoError(os.MkdirAll(hashDir, 0o755), "failed to create hash dir")
	require.NoError(os.WriteFile(filepath.Join(hashDir, validHash), []byte("test content"), 0o644), "failed to write attachment")

	detail := &query.MessageDetail{
		ID:      1,
		Subject: "Test",
		Attachments: []query.AttachmentInfo{
			{ID: 1, Filename: "valid.pdf", ContentHash: validHash},
		},
	}
	selection := map[int]bool{0: true}

	cmd := env.Ctrl.ExportAttachments(detail, selection)
	require.NotNil(cmd)

	msg := cmd()
	result, ok := msg.(ExportResultMsg)
	require.True(ok, "expected ExportResultMsg, got %T", msg)

	assert.NoError(result.Err, "expected Err to be nil for full success")
	assert.NotEmpty(result.Result)
}
