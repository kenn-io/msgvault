package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDoclingDocumentsConsentBuildAndLifecycle(t *testing.T) {
	testDoclingDocumentsLifecycle(t, mistraltest.MinimalPDF("synthetic Docling document"), "application/pdf", "document.pdf")
}

func TestDoclingDocumentsNativeCSVConsentBuildAndLifecycle(t *testing.T) {
	testDoclingDocumentsLifecycle(t, []byte("name,value\nsynthetic,docling1057sentinel\n"), "text/csv", "document.csv")
}

func TestDoclingStatusDisclosesRangePlaintext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	markDaemonCLISubprocessForTest(t)
	cfg := testConfigValue()
	cfg.Data.DataDir = t.TempDir()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.Provider = documentindex.ProviderDocling
	cfg.Attachments.Documents.Endpoint = "http://127.0.0.1:5001"
	cfg.Attachments.Documents.ApplyConfiguredProviderDefaults(func(string) bool { return false })
	ctx := withTestConfig(t, cfg)
	f := storetest.New(t)
	deps := documentsCommandDeps{
		openStore:            func(context.Context) (*store.Store, func(), error) { return f.Store, func() {}, nil },
		openInitializedStore: func(context.Context) (*store.Store, func(), error) { return f.Store, func() {}, nil },
	}
	run := func(args ...string) string {
		command := newDocumentsCmd(deps)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(args)
		requirements.NoError(command.ExecuteContext(ctx))
		return output.String()
	}
	run("consent-docling", "--yes")
	documentsConfig, _, _, profile, err := configuredDocumentProfile("", invocationFromContext(ctx))
	requirements.NoError(err)
	hash := strings.Repeat("a", 64)
	messageID := f.CreateMessage("range-status")
	requirements.NoError(f.Store.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{
		Filename: "range.pdf", MIMEType: "application/pdf", Size: 128,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1",
	}))
	var attachmentID int64
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind("SELECT id FROM attachments WHERE message_id = ?"), messageID).Scan(&attachmentID))
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(ctx, attachmentID, 1)
	requirements.NoError(err)
	requirements.True(eligible)
	claim, err := f.Store.ClaimDocumentExtraction(ctx, store.DocumentExtractionClaimInput{
		ExtractionID: "range-status-attempt", ProfileID: profile.ID, CanonicalBlobHash: hash, ExtractionInputKey: "original",
		OccurrenceAttachmentID: attachmentID, OccurrenceMIMEType: "application/pdf", OccurrenceMessageType: "email",
		LeaseOwner: "range-status-worker", LeaseUntil: time.Now().UTC().Add(time.Minute), LocalBytes: 128, SourceSequence: 1,
	})
	requirements.NoError(err)
	requirements.NoError(f.Store.SaveDocumentExtractionRange(ctx, claim, storetest.DocumentRangeCheckpoint(t), documentsConfig.MaxResponseBytes, documentsConfig.MaxNormalizedChars))
	var status documentStatusOutput
	requirements.NoError(json.Unmarshal([]byte(run("status", "--json")), &status))
	assertions.True(status.StoresPlaintext)
}

func testDoclingDocumentsLifecycle(t *testing.T, content []byte, mediaType, uploadName string) {
	t.Helper()
	assertions := assert.New(t)
	requirements := require.New(t)

	markDaemonCLISubprocessForTest(t)
	var uploads atomic.Int32
	var reject atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assertions.Empty(r.Header.Get("X-Api-Key"))
		if r.Method == http.MethodPost {
			uploads.Add(1)
			if reject.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, err := w.Write([]byte(`{"task_id":"synthetic-job","task_type":"convert","task_status":"success"}`))
			assertions.NoError(err)
			return
		}
		response := `{"status":"success","errors":[],"document":{"filename":"document.pdf","md_content":"docling1057sentinel","json_content":{"schema_name":"DoclingDocument","version":"1.7.0","origin":{"filename":"document.pdf"},"pages":{"1":{}},"texts":[{"text":"docling1057sentinel","prov":[{"page_no":1}]}]}}}`
		_, err := w.Write([]byte(strings.ReplaceAll(response, "document.pdf", uploadName)))
		assertions.NoError(err)
	}))
	t.Cleanup(server.Close)
	cfg := testConfigValue()
	cfg.Data.DataDir = t.TempDir()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.Provider = documentindex.ProviderDocling
	cfg.Attachments.Documents.Endpoint = server.URL
	cfg.Attachments.Documents.ApplyConfiguredProviderDefaults(func(string) bool { return false })
	cfg.Attachments.Documents.PollInterval = time.Millisecond
	ctx := withTestConfig(t, cfg)
	fixture := storetest.New(t)
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	messageID := fixture.CreateMessage("synthetic-docling-document")
	requirements.NoError(fixture.Store.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{
		Filename: "synthetic-" + uploadName, MIMEType: mediaType, Size: int64(len(content)), ContentHash: hash, StoragePath: hash[:2] + "/" + hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1",
	}))
	deps := documentsCommandDeps{
		newMistralProcessor: func(*documentindex.DocumentsConfig) (documentindex.MistralProcessor, error) {
			assertions.Fail("Docling must not instantiate a Mistral processor")
			return nil, errors.New("unexpected Mistral processor")
		},
		openStore:            func(context.Context) (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
		openInitializedStore: func(context.Context) (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
		openAttachments: func(context.Context, *store.Store) (documentindex.DocumentAttachmentOpener, func() error, error) {
			return commandAttachmentOpener{content: content}, func() error { return nil }, nil
		},
		openReadClient: func(context.Context) (documentReadClient, func(), error) {
			return localDocumentReadClient{store: fixture.Store}, func() {}, nil
		},
	}
	run := func(args ...string) (string, error) {
		command := newDocumentsCmd(deps)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(args)
		err := command.ExecuteContext(ctx)
		return output.String(), err
	}
	disclosure, err := run("consent-docling")
	requirements.ErrorContains(err, "requires --yes")
	assertions.Contains(disclosure, server.URL)
	assertions.Contains(disclosure, "operator-controlled")
	assertions.NotContains(disclosure, "Hosted document extraction")
	_, err = run("build", "--yes")
	requirements.ErrorContains(err, "exact consent")
	assertions.Zero(uploads.Load())
	_, err = run("consent-docling", "--yes")
	requirements.NoError(err)
	_, err = run("build")
	requirements.ErrorContains(err, "requires --yes")
	assertions.Zero(uploads.Load())
	cfg.Attachments.Documents.APIKeyEnv = "SYNTHETIC_DOCLING_BUILD_KEY"
	t.Setenv(cfg.Attachments.Documents.APIKeyEnv, "")
	requirements.NoError(os.Unsetenv(cfg.Attachments.Documents.APIKeyEnv))
	_, err = run("consent-docling", "--yes")
	requirements.NoError(err)
	for _, args := range [][]string{{"build", "--yes"}, {"build", "--full-rebuild", "--yes"}, {"resume", "--yes"}} {
		_, err = run(args...)
		requirements.ErrorContains(err, "requires nonempty environment variable SYNTHETIC_DOCLING_BUILD_KEY")
		var claims, rebuilds int
		requirements.NoError(fixture.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM document_extractions").Scan(&claims))
		assertions.Zero(claims, "missing credentials must not claim or suppress attachments")
		requirements.NoError(fixture.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM document_extraction_rebuilds").Scan(&rebuilds))
		assertions.Zero(rebuilds, "missing credentials must not leave a rebuild behind")
		assertions.Zero(uploads.Load())
	}
	cfg.Attachments.Documents.APIKeyEnv = ""
	_, err = run("consent-docling", "--yes")
	requirements.NoError(err)
	_, err = run("build", "--yes")
	requirements.NoError(err)
	assertions.Equal(int32(1), uploads.Load())
	search, err := run("search", "docling1057sentinel", "--json")
	requirements.NoError(err)
	var response store.DocumentSearchResponse
	requirements.NoError(json.Unmarshal([]byte(search), &response))
	requirements.Len(response.Results, 1)
	assertions.Equal(messageID, response.Results[0].MessageID)
	assertions.Equal(mediaType, response.Results[0].MIMEType)
	normalized, err := fixture.Store.LoadNormalizedDocument(ctx, response.Results[0].ExtractionID)
	requirements.NoError(err)
	wantUnitKind := "page"
	if mediaType == "text/csv" {
		wantUnitKind = "document"
	}
	assertions.Equal(wantUnitKind, normalized.UnitKind)
	var receipts int
	requirements.NoError(fixture.Store.DB().QueryRowContext(t.Context(), fixture.Store.Rebind(
		"SELECT COUNT(*) FROM document_extraction_conversions WHERE extraction_id = ?"),
		response.Results[0].ExtractionID).Scan(&receipts))
	assertions.Zero(receipts, "native Docling uploads must not invent conversion provenance")
	_, err = run("resume", "--yes")
	requirements.NoError(err)
	assertions.Equal(int32(1), uploads.Load())
	statusJSON, err := run("status", "--json")
	requirements.NoError(err)
	assertions.Contains(statusJSON, `"configured_formats":8`)
	humanStatus, err := run("status")
	requirements.NoError(err)
	assertions.Contains(humanStatus, "Formats: 8 configured")
	assertions.NotContains(humanStatus, "Private spool:")
	_, _, inputPolicy, profile, err := configuredDocumentProfile("", invocationFromContext(ctx))
	requirements.NoError(err)
	assertions.Len(inputPolicy.Routes, 8)
	assertions.Equal(documentindex.ProviderDocling, profile.Provider)
	assertions.Equal(server.URL, profile.Endpoint)
	cfg.Attachments.Documents.Scope.IncludeInline = true
	_, err = run("build", "--yes")
	requirements.ErrorContains(err, "exact consent")
	assertions.Equal(int32(1), uploads.Load())
	cfg.Attachments.Documents.Scope.IncludeInline = false
	cfg.Attachments.Documents.Endpoint = "http://127.0.0.1:1"
	_, err = run("build", "--yes")
	requirements.ErrorContains(err, "exact consent")
	assertions.Equal(int32(1), uploads.Load())
	cfg.Attachments.Documents.Endpoint = server.URL
	reject.Store(true)
	_, err = run("build", "--full-rebuild", "--yes")
	requirements.Error(err)
	assertions.Equal(int32(2), uploads.Load())
	search, err = run("search", "docling1057sentinel", "--json")
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal([]byte(search), &response))
	requirements.Len(response.Results, 1)
	_, err = run("retry", "--hash", hash)
	requirements.NoError(err)
	reject.Store(false)
	_, err = run("resume", "--yes")
	requirements.NoError(err)
	assertions.Equal(int32(3), uploads.Load())
	_, err = run("retire", profile.ID, "--yes")
	requirements.NoError(err)
	_, err = run("build", "--yes")
	requirements.ErrorContains(err, "exact consent")
	assertions.Equal(int32(3), uploads.Load())
}

// doclingJobBarrier holds each upload until the expected number of uploads
// are in flight together, or a timeout passes, and records the peak.
type doclingJobBarrier struct {
	mu       sync.Mutex
	want     int
	inFlight int
	peak     int
	release  chan struct{}
}

func (b *doclingJobBarrier) reset(want int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.want, b.peak, b.release = want, 0, make(chan struct{})
}

func (b *doclingJobBarrier) wait() {
	b.mu.Lock()
	b.inFlight++
	b.peak = max(b.peak, b.inFlight)
	release := b.release
	if b.inFlight >= b.want {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	b.mu.Unlock()
	select {
	case <-release:
	case <-time.After(5 * time.Second):
	}
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
}

func (b *doclingJobBarrier) peakInFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

func TestDoclingBuildRunsConfiguredConcurrentJobs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	markDaemonCLISubprocessForTest(t)
	const jobs = 3
	var barrier doclingJobBarrier
	barrier.reset(jobs)
	var mu sync.Mutex
	uploads := make(map[string]int)
	tasks := make(map[string]string)
	var reject string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			if !assertions.NoError(r.ParseMultipartForm(1 << 20)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			files := r.MultipartForm.File["files"]
			if !assertions.Len(files, 1) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			file, err := files[0].Open()
			if !assertions.NoError(err) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			content, err := io.ReadAll(file)
			if !assertions.NoError(err) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			digest := sha256.Sum256(content)
			hash := hex.EncodeToString(digest[:])
			barrier.wait()
			mu.Lock()
			uploads[hash]++
			rejected := hash == reject
			taskID := "synthetic-job-" + hash[:16]
			tasks[taskID] = hash
			mu.Unlock()
			if rejected {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, err = w.Write([]byte(`{"task_id":"` + taskID + `","task_type":"convert","task_status":"success"}`))
			assertions.NoError(err)
		case strings.HasPrefix(r.URL.Path, "/v1/result/"):
			mu.Lock()
			hash := tasks[strings.TrimPrefix(r.URL.Path, "/v1/result/")]
			mu.Unlock()
			sentinel := "doclingjob" + hash[:12]
			response := `{"status":"success","errors":[],"document":{"filename":"document.pdf","md_content":"` + sentinel + `","json_content":{"schema_name":"DoclingDocument","version":"1.7.0","origin":{"filename":"document.pdf"},"pages":{"1":{}},"texts":[{"text":"` + sentinel + `","prov":[{"page_no":1}]}]}}}`
			_, err := w.Write([]byte(response))
			assertions.NoError(err)
		default:
			assertions.Fail("unexpected Docling request", "%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	cfg := testConfigValue()
	cfg.Data.DataDir = t.TempDir()
	c := &cfg.Attachments.Documents
	c.Enabled, c.Provider, c.Endpoint = true, documentindex.ProviderDocling, server.URL
	c.ApplyConfiguredProviderDefaults(func(string) bool { return false })
	c.PollInterval = time.Millisecond
	c.MaxConcurrentJobs = jobs
	// Each document may return one unit, so the run budget admits four.
	c.MaxPagesPerDocument, c.MaxPagesPerRun = 1, 4
	ctx := withTestConfig(t, cfg)
	fixture := storetest.New(t)
	contents := make(map[string][]byte)
	for index := range 7 {
		content := mistraltest.MinimalPDF(fmt.Sprintf("synthetic concurrent document %d", index))
		digest := sha256.Sum256(content)
		hash := hex.EncodeToString(digest[:])
		contents[hash] = content
		messageID := fixture.CreateMessage(fmt.Sprintf("synthetic-docling-concurrency-%d", index))
		requirements.NoError(fixture.Store.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{
			Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: int64(len(content)), ContentHash: hash,
			StoragePath: hash[:2] + "/" + hash, Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1",
		}))
	}
	deps := documentsCommandDeps{
		openStore: func(context.Context) (*store.Store, func(), error) { return fixture.Store, func() {}, nil },
		openAttachments: func(context.Context, *store.Store) (documentindex.DocumentAttachmentOpener, func() error, error) {
			return commandAttachmentMapOpener{contents: contents}, func() error { return nil }, nil
		},
	}
	run := func(args ...string) (string, error) {
		command := newDocumentsCmd(deps)
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(args)
		err := command.ExecuteContext(ctx)
		return output.String(), err
	}
	snapshot := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(uploads)
	}
	_, err := run("consent-docling", "--yes")
	requirements.NoError(err)

	output, err := run("build", "--yes")
	requirements.NoError(err)
	assertions.Contains(output, "at most 4 canonical document(s)")
	assertions.Contains(output, "Up to 3 Docling job(s) run at once.")
	assertions.Contains(output, "indexed 4 document(s), 4 unit(s), skipped 0, failed 0")
	assertions.Equal(jobs, barrier.peakInFlight(), "the build must keep exactly max_concurrent_jobs uploads in flight")
	first := snapshot()
	requirements.Len(first, 4, "the run budget must cap the build at four documents")
	for hash, count := range first {
		assertions.Equal(1, count, "document %s uploaded more than once", hash)
	}

	var remaining []string
	for hash := range contents {
		if first[hash] == 0 {
			remaining = append(remaining, hash)
		}
	}
	requirements.Len(remaining, 3)
	slices.Sort(remaining)
	mu.Lock()
	reject = remaining[0]
	mu.Unlock()
	barrier.reset(jobs)
	output, err = run("resume", "--yes")
	requirements.ErrorContains(err, "1 extraction failure")
	assertions.Contains(err.Error(), reject+": provider_rejected")
	assertions.Contains(output, "indexed 2 document(s), 2 unit(s), skipped 0, failed 1")
	assertions.Equal(jobs, barrier.peakInFlight())

	_, err = run("resume", "--yes")
	requirements.NoError(err)
	all := snapshot()
	assertions.Len(all, len(contents))
	for hash := range contents {
		assertions.Equal(1, all[hash], "document %s must be uploaded exactly once", hash)
	}
	for hash := range contents {
		response, err := fixture.Store.SearchDocuments(ctx, store.DocumentSearchRequest{Query: "doclingjob" + hash[:12]})
		requirements.NoError(err)
		if hash == reject {
			assertions.Empty(response.Results)
			continue
		}
		assertions.Len(response.Results, 1, "document %s must publish its own text", hash)
	}
}

func TestDoclingConsentDisclosesCredentialsAndRangeRetention(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	markDaemonCLISubprocessForTest(t)
	cfg := testConfigValue()
	c := &cfg.Attachments.Documents
	c.Provider, c.Endpoint, c.Enabled = documentindex.ProviderDocling, "http://127.0.0.1:5001", true
	c.ApplyConfiguredProviderDefaults(func(string) bool { return false })
	c.APIKeyEnv = "SYNTHETIC_DOCLING_KEY"
	t.Setenv(c.APIKeyEnv, "synthetic-secret-value")
	command := newDocumentsCmd(documentsCommandDeps{})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"consent-docling"})
	requirements.ErrorContains(command.ExecuteContext(withTestConfig(t, cfg)), "requires --yes")
	assertions.Contains(output.String(), c.APIKeyEnv)
	assertions.Contains(output.String(), "X-Api-Key")
	assertions.Contains(output.String(), "PDFs over 50 pages")
	assertions.Contains(output.String(), "Each job uploads the original PDF again")
	assertions.Contains(output.String(), "daemon restarts")
	assertions.Contains(output.String(), "documents purge-derived")
	assertions.Contains(output.String(), "seven days")
	assertions.NotContains(output.String(), "synthetic-secret-value")
}

func TestDoclingConfigurationRejectsMistralCommands(t *testing.T) {
	requirements := require.New(t)

	markDaemonCLISubprocessForTest(t)
	cfg := testConfigValue()
	c := &cfg.Attachments.Documents
	c.Provider, c.Endpoint, c.Enabled = documentindex.ProviderDocling, "http://127.0.0.1:5001", true
	c.ApplyConfiguredProviderDefaults(func(string) bool { return false })
	for _, args := range [][]string{
		{"probe-mistral", "--fixtures", "synthetic-fixtures", "--validate-only"},
		{"consent-mistral", "--capabilities", "synthetic-manifest", "--yes"},
	} {
		command := newDocumentsCmd(documentsCommandDeps{})
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(args)
		requirements.ErrorContains(command.ExecuteContext(withTestConfig(t, cfg)), "provider=mistral")
	}
}

func TestMistralDocumentOperationsStillRequireManifest(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	cfg := testConfigValue()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.RetentionPosture = documentindex.RetentionZDR
	cfg.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut
	for _, args := range [][]string{{"build", "--yes"}, {"resume", "--yes"}, {"status"}, {"retry", "--hash", "synthetic-hash"}} {
		command := newDocumentsCmd(documentsCommandDeps{})
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs(args)
		require.ErrorContains(t, command.ExecuteContext(withTestConfig(t, cfg)), "requires --capabilities")
	}
}
