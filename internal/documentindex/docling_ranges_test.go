package documentindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func doclingPDFPages(t *testing.T, pages int) []byte {
	t.Helper()
	var imageBytes bytes.Buffer
	require.NoError(t, jpeg.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil))
	objects := make([]string, 2, pages+2)
	objects[0] = "<< /Type /Catalog /Pages 2 0 R >>"
	var kids strings.Builder
	for page := range pages {
		fmt.Fprintf(&kids, "%d 0 R ", page+3)
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 16 16] /Resources << /XObject << /Im %d 0 R >> >> /Contents %d 0 R >>", pages+3, pages+4))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), pages)
	objects = append(objects,
		fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", imageBytes.Len(), imageBytes.String()),
		"<< /Length 10 >>\nstream\nq /Im Do Q\nendstream")
	return mediatest.PDFObjects("synthetic document range fixture", objects)
}

func TestDoclingWorkerPageRanges(t *testing.T) {
	for _, test := range []struct {
		name                     string
		pages, cap               int
		loadFailure, saveFailure bool
		corruption               string
	}{
		{name: "50", pages: 50},
		{name: "51", pages: 51},
		{name: "corrupt checkpoint", pages: 51, corruption: "evidence"},
		{name: "corrupt checksum", pages: 51, corruption: "checksum"},
		{name: "checkpoint load failure", pages: 51, loadFailure: true},
		{name: "checkpoint save failure", pages: 51, saveFailure: true},
		{name: "whole document page cap", pages: 51, cap: 50},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			pages := test.pages
			content := doclingPDFPages(t, pages)
			var selections []string
			var first, last int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/convert/file/async":
					r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
					if !assertions.NoError(r.ParseMultipartForm(2 << 20)) { // #nosec G120 -- MaxBytesReader bounds this synthetic upload.
						return
					}
					defer func() { assertions.NoError(r.MultipartForm.RemoveAll()) }()
					selected := r.MultipartForm.Value["page_range"]
					selections = append(selections, strings.Join(selected, ","))
					first, last = 1, pages
					if len(selected) == 2 {
						var err error
						first, err = strconv.Atoi(selected[0])
						if !assertions.NoError(err) {
							return
						}
						last, err = strconv.Atoi(selected[1])
						if !assertions.NoError(err) {
							return
						}
					}
					_, err := io.WriteString(w, `{"task_id":"range-job","task_type":"convert","task_status":"success"}`)
					assertions.NoError(err)
				case "/v1/result/range-job":
					response := doclingWorkerResponse(false)
					wire, ok := response["document"].(map[string]any)
					if !assertions.True(ok) {
						return
					}
					doc, ok := wire["json_content"].(map[string]any)
					if !assertions.True(ok) {
						return
					}
					registry := map[string]any{}
					texts := []any{}
					for page := first; page <= last; page++ {
						registry[strconv.Itoa(page)] = map[string]any{}
						if page != 2 {
							texts = append(texts, map[string]any{"text": fmt.Sprintf("page %d sentinel", page), "prov": []any{map[string]any{"page_no": page}}})
						}
					}
					doc["pages"], doc["texts"] = registry, texts
					assertions.NoError(json.NewEncoder(w).Encode(response))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := doclingTestConfig()
			c.Endpoint = server.URL
			if test.cap > 0 {
				c.MaxPagesPerDocument = test.cap
			}
			provider, err := NewDoclingClient(&c)
			requirements.NoError(err)
			catalog := &workerCatalog{}
			if test.loadFailure {
				catalog.loadRangesErr = errors.New("database is locked")
			}
			if test.saveFailure {
				catalog.saveRangeErr = errors.New("database is locked")
			}
			worker, err := NewDoclingWorker(catalog, &workerOpener{content: content, closed: &atomic.Int32{}}, provider, DoclingWorkerConfig{Documents: c, ProfileID: "ranges", LeaseOwner: "worker", LeaseDuration: time.Minute, RetryDelay: time.Minute})
			requirements.NoError(err)
			digest := sha256.Sum256(content)
			candidate := store.DocumentExtractionCandidate{AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", MessageType: "email", Size: int64(len(content))}
			if test.corruption != "" {
				_, err = worker.ProcessCandidate(t.Context(), candidate)
				requirements.NoError(err)
				requirements.Len(catalog.ranges, 2)
				catalog.ranges = catalog.ranges[:1]
				if test.corruption == "checksum" {
					catalog.ranges[0].EvidenceChecksum = "invalid"
				} else {
					catalog.ranges[0].Evidence = append(catalog.ranges[0].Evidence, ' ')
				}
				catalog.publication = nil
				selections = nil
			}
			result, err := worker.ProcessCandidate(t.Context(), candidate)
			if test.corruption != "" {
				requirements.ErrorContains(err, "invalid canonical document range evidence")
				assertions.Nil(catalog.publication)
				assertions.Empty(selections)
				return
			}
			if test.loadFailure || test.saveFailure {
				requirements.Error(err)
				assertions.Nil(catalog.publication)
				assertions.Equal("range_checkpoint_failed", result.FailureReasonCode)
				requirements.NotNil(catalog.failure)
				assertions.False(catalog.failure.Terminal)
				assertions.False(catalog.failure.RetryAt.IsZero())
				assertions.Empty(catalog.failure.Detail)
				return
			}
			if test.cap > 0 {
				requirements.Error(err)
				assertions.Nil(catalog.publication)
				assertions.Empty(catalog.ranges)
				assertions.Empty(selections)
				assertions.Equal("invalid_local_source", result.FailureReasonCode)
				assertions.Equal("Docling PDF has 51 pages; max_pages_per_document is 50", result.FailureDetail)
				requirements.NotNil(catalog.failure)
				assertions.Equal(result.FailureDetail, catalog.failure.Detail)
				return
			}
			requirements.NoError(err)
			switch pages {
			case 50:
				assertions.Equal([]string{""}, selections)
			}
			requirements.NotNil(catalog.publication)
			assertions.Equal(pages, result.Units)
			assertions.Empty(catalog.publication.Units[1].Text)
			assertions.Equal(fmt.Sprintf("page %d sentinel", pages), catalog.publication.Units[pages-1].Text)
		})
	}
}

func TestDoclingRangesResumeAfterReopen(t *testing.T) {
	for _, failure := range []string{"partial", "checkpoint save failure", "checkpoint limit"} {
		t.Run(failure, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			content := doclingPDFPages(t, 600)
			digest := sha256.Sum256(content)
			hash := hex.EncodeToString(digest[:])
			var selections []string
			first, last := 0, 0
			failMiddle := true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/convert/file/async":
					r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
					if !assertions.NoError(r.ParseMultipartForm(2 << 20)) { // #nosec G120 -- MaxBytesReader bounds this synthetic upload.
						return
					}
					defer func() { assertions.NoError(r.MultipartForm.RemoveAll()) }()
					selected := r.MultipartForm.Value["page_range"]
					if !assertions.Len(selected, 2) {
						return
					}
					var err error
					first, err = strconv.Atoi(selected[0])
					if !assertions.NoError(err) {
						return
					}
					last, err = strconv.Atoi(selected[1])
					if !assertions.NoError(err) {
						return
					}
					selections = append(selections, strings.Join(selected, ","))
					assertions.NoError(json.NewEncoder(w).Encode(map[string]any{"task_id": "resume-job", "task_type": "convert", "task_status": "success"}))
				case "/v1/result/resume-job":
					response := doclingWorkerResponse(false)
					wire, ok := response["document"].(map[string]any)
					if !assertions.True(ok) {
						return
					}
					doc, ok := wire["json_content"].(map[string]any)
					if !assertions.True(ok) {
						return
					}
					registry := map[string]any{}
					texts := []any{}
					for page := first; page <= last; page++ {
						registry[strconv.Itoa(page)] = map[string]any{}
						texts = append(texts, map[string]any{"text": fmt.Sprintf("range sentinel page %d", page), "prov": []any{map[string]any{"page_no": page}}})
					}
					doc["pages"], doc["texts"] = registry, texts
					if failMiddle && first == 51 && failure == "partial" {
						response["status"] = "partial_success"
						response["errors"] = []any{map[string]any{"page_no": 51}}
					}
					assertions.NoError(json.NewEncoder(w).Encode(response))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := doclingTestConfig()
			c.Endpoint = server.URL
			c.MaxPollAttempts = 1
			c.PollInterval = time.Millisecond
			c.MaxPagesPerDocument = 600
			if failure == "checkpoint limit" {
				c.MaxResponseBytes = 64 << 10
			}
			descriptor, err := c.DoclingDescriptor()
			requirements.NoError(err)
			input, err := c.doclingInputPolicy()
			requirements.NoError(err)
			policy, err := c.doclingPolicyJSON(input.AllowedMediaTypes)
			requirements.NoError(err)
			profile := store.DocumentExtractionProfile{ID: "profile-ranges", Fingerprint: descriptor.PolicyFingerprint, Provider: ProviderDocling, Endpoint: c.Endpoint, Region: c.Region, Model: c.Model, RetentionPosture: c.RetentionPosture, TrainingPosture: c.TrainingPosture, AllowedMediaTypes: input.AllowedMediaTypes, PolicyJSON: policy}
			_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
			requirements.NoError(err)
			requirements.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint, RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture}))
			messageID := f.CreateMessage("range-resume")
			requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{Filename: "manual.pdf", MIMEType: "application/pdf", Size: int64(len(content)), StoragePath: hash[:2] + "/" + hash, ContentHash: hash, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1"}))
			var attachmentID int64
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT id FROM attachments WHERE message_id = ?"), messageID).Scan(&attachmentID))
			_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 1)
			requirements.NoError(err)
			requirements.True(eligible)
			candidate := store.DocumentExtractionCandidate{AttachmentID: attachmentID, CanonicalBlobHash: hash, MIMEType: "application/pdf", Size: int64(len(content)), MessageType: "email", SourceSequence: 1}
			provider, err := NewDoclingClient(&c)
			requirements.NoError(err)
			config := DoclingWorkerConfig{Documents: c, ProfileID: profile.ID, LeaseOwner: "before-restart", LeaseDuration: time.Minute, RetryDelay: time.Minute}
			var catalog DocumentExtractionCatalog = f.Store
			if failure == "checkpoint save failure" {
				catalog = &failSecondRangeSave{Store: f.Store}
				config.RetryDelay = 100 * time.Millisecond
			}
			worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, config)
			requirements.NoError(err)
			failed, err := worker.ProcessCandidate(t.Context(), candidate)
			requirements.Error(err)
			var saved int
			requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&saved))
			if failure == "checkpoint limit" {
				assertions.Positive(saved, "each range fits; the aggregate exceeds the cap")
				assertions.Equal("response_too_large", failed.FailureReasonCode)
				assertions.Equal("Docling range checkpoints exceed max_response_bytes (65536 bytes)", failed.FailureDetail)
				var detail string
				requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT failure_detail FROM document_extractions WHERE profile_id = ? AND canonical_blob_hash = ?"), profile.ID, hash).Scan(&detail))
				assertions.Equal(failed.FailureDetail, detail)
				return
			}
			assertions.Equal(1, saved)
			if !f.Store.IsPostgreSQL() {
				var sequence int
				var name, path string
				requirements.NoError(f.Store.DB().QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path))
				requirements.NoError(f.Store.Close())
				reopened, err := store.OpenForTest(path)
				requirements.NoError(err)
				f.Store = reopened
				t.Cleanup(func() { requirements.NoError(reopened.Close()) })
			}
			if failure != "checkpoint save failure" {
				changed, err := f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
				requirements.NoError(err)
				requirements.True(changed)
			}
			failMiddle = false
			config.LeaseOwner = "after-restart"
			worker, err = NewDoclingWorker(f.Store, &workerOpener{content: content}, provider, config)
			requirements.NoError(err)
			var result DocumentExtractionResult
			if failure == "checkpoint save failure" {
				var retryAt time.Time
				requirements.NoError(f.Store.DB().QueryRow("SELECT next_retry_at FROM document_extractions WHERE state = 'tombstoned'").Scan(&retryAt))
				// Wait for the persisted retry deadline and SQLite's whole-second clock.
				requirements.Eventually(func() bool {
					if time.Now().UTC().Before(retryAt) {
						return false
					}
					result, err = worker.ProcessCandidate(t.Context(), candidate)
					return err == nil
				}, 5*time.Second, 20*time.Millisecond)
			} else {
				result, err = worker.ProcessCandidate(t.Context(), candidate)
				requirements.NoError(err)
			}
			assertions.Equal(600, result.Units)
			assertions.Equal([]string{"1,50", "51,100", "51,100", "101,150", "151,200", "201,250", "251,300", "301,350", "351,400", "401,450", "451,500", "501,550", "551,600"}, selections)
			var heads int
			requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_heads").Scan(&heads))
			assertions.Equal(1, heads)
			var requests int
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT request_count FROM document_extractions WHERE id = ?"), result.ExtractionID).Scan(&requests))
			assertions.Equal(22, requests)
			search, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "sentinel"})
			requirements.NoError(err)
			assertions.NotEmpty(search.Results)
		})
	}
}

func TestDoclingRangesFallbackAndCoverage(t *testing.T) {
	for _, test := range []struct {
		name                                                                              string
		degrade, missing, rebase, truncated, blankFirst, structuredFirst, missingFallback bool
	}{
		{name: "Markdown from all ranges", degrade: true},
		{name: "blank range before degraded range", degrade: true, blankFirst: true},
		{name: "structured text before degraded range", degrade: true, structuredFirst: true},
		{name: "degraded range without Markdown", degrade: true, missingFallback: true},
		{name: "missing page", missing: true},
		{name: "rebased page", rebase: true},
		{name: "truncate after complete coverage", truncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			content := doclingPDFPages(t, 51)
			first, last := 0, 0
			var selections []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/convert/file/async":
					r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
					if !assertions.NoError(r.ParseMultipartForm(2 << 20)) { // #nosec G120 -- MaxBytesReader bounds this synthetic upload.
						return
					}
					defer func() { assertions.NoError(r.MultipartForm.RemoveAll()) }()
					selected := r.MultipartForm.Value["page_range"]
					if !assertions.Len(selected, 2) {
						return
					}
					var err error
					first, err = strconv.Atoi(selected[0])
					if !assertions.NoError(err) {
						return
					}
					last, err = strconv.Atoi(selected[1])
					if !assertions.NoError(err) {
						return
					}
					selections = append(selections, strings.Join(selected, ","))
					_, err = io.WriteString(w, `{"task_id":"coverage-job","task_type":"convert","task_status":"success"}`)
					assertions.NoError(err)
				case "/v1/result/coverage-job":
					response := doclingWorkerResponse(false)
					wire, ok := response["document"].(map[string]any)
					if !assertions.True(ok) {
						return
					}
					wire["md_content"] = fmt.Sprintf("range %d..%d Markdown sentinel", first, last)
					doc, ok := wire["json_content"].(map[string]any)
					if !assertions.True(ok) {
						return
					}
					registry := map[string]any{}
					texts := []any{}
					for page := first; page <= last; page++ {
						if test.missing && page == 2 {
							continue
						}
						number := page
						if test.rebase && first == 51 {
							number = 1
						}
						registry[strconv.Itoa(number)] = map[string]any{}
						texts = append(texts, map[string]any{"text": fmt.Sprintf("page %d %s", page, strings.Repeat("sentinel ", 5)), "prov": []any{map[string]any{"page_no": number}}})
					}
					doc["pages"], doc["texts"] = registry, texts
					if test.blankFirst && first == 1 {
						doc["texts"] = []any{}
						wire["md_content"] = ""
					}
					if (test.structuredFirst && first == 1) || (test.missingFallback && first == 51) {
						wire["md_content"] = ""
					}
					if test.degrade && first == 51 {
						doc["tables"] = []any{map[string]any{"data": map[string]any{"table_cells": []any{}}}}
					}
					assertions.NoError(json.NewEncoder(w).Encode(response))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := doclingTestConfig()
			c.Endpoint = server.URL
			if test.truncated {
				c.MaxNormalizedChars = 100
			}
			provider, err := NewDoclingClient(&c)
			requirements.NoError(err)
			catalog := &workerCatalog{}
			worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, DoclingWorkerConfig{Documents: c, ProfileID: "coverage", LeaseOwner: "worker", LeaseDuration: time.Minute, RetryDelay: time.Minute})
			requirements.NoError(err)
			digest := sha256.Sum256(content)
			result, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", MessageType: "email", Size: int64(len(content))})
			if test.missing || test.rebase || test.missingFallback {
				requirements.Error(err)
				assertions.Nil(catalog.publication)
				if test.missingFallback {
					assertions.Len(catalog.ranges, 1, "retry must request the degraded range again")
				}
				if test.missing {
					assertions.Empty(catalog.ranges)
				}
				return
			}
			requirements.NoError(err)
			requirements.NotNil(catalog.publication)
			assertions.Equal([]string{"1,50", "51,51"}, selections)
			requirements.Len(catalog.ranges, 2)
			assertions.Empty(catalog.ranges[0].Markdown)
			if test.blankFirst {
				assertions.Zero(catalog.ranges[0].CharCount)
			} else {
				assertions.Equal(2641, catalog.ranges[0].CharCount)
			}
			if test.degrade {
				assertions.Equal("range 51..51 Markdown sentinel", catalog.ranges[1].Markdown)
				assertions.Equal(83, catalog.ranges[1].CharCount)
			} else {
				assertions.Empty(catalog.ranges[1].Markdown)
				assertions.Equal(53, catalog.ranges[1].CharCount)
			}
			if test.degrade {
				assertions.Equal("document", catalog.publication.UnitKind)
				requirements.Len(catalog.publication.Units, 1)
				if test.blankFirst {
					assertions.Equal("range 51..51 Markdown sentinel", catalog.publication.Units[0].Text)
				} else {
					assertions.Contains(catalog.publication.Units[0].Text, "page 1 sentinel")
					assertions.Contains(catalog.publication.Units[0].Text, "page 50 sentinel")
					assertions.Contains(catalog.publication.Units[0].Text, "range 51..51 Markdown sentinel")
				}
			}
			if test.truncated {
				assertions.True(result.Truncated)
				assertions.Equal(51, result.ProviderUnits)
			}
		})
	}
}

// The real Store still owns claims, failure recording, and checkpoint transfer.
// Only one save fails to reproduce transient storage trouble after a completed job.
type failSecondRangeSave struct {
	*store.Store

	failed bool
}

func (s *failSecondRangeSave) SaveDocumentExtractionRange(ctx context.Context, claim store.DocumentExtractionClaim, checkpoint store.DocumentExtractionRange, maxBytes int64) error {
	if checkpoint.FirstPage == 51 && !s.failed {
		s.failed = true
		return errors.New("database is locked")
	}
	return s.Store.SaveDocumentExtractionRange(ctx, claim, checkpoint, maxBytes)
}
