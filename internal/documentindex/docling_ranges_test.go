package documentindex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func doclingPDFPages(pages int) []byte {
	objects := make([]string, 2, pages+2)
	objects[0] = "<< /Type /Catalog /Pages 2 0 R >>"
	var kids strings.Builder
	for page := range pages {
		fmt.Fprintf(&kids, "%d 0 R ", page+3)
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 16 16] >>")
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), pages)
	return mediatest.PDFObjects("synthetic document range fixture", objects)
}

func TestDoclingWorkerPageRanges(t *testing.T) {
	for _, pages := range []int{50, 51, 101} {
		t.Run(strconv.Itoa(pages), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			content := doclingPDFPages(pages)
			count, err := media.CountPDFPages(content)
			requirements.NoError(err)
			assertions.Equal(int64(pages), count)
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
			provider, err := NewDoclingClient(&c)
			requirements.NoError(err)
			catalog := &workerCatalog{}
			worker, err := NewDoclingWorker(catalog, &workerOpener{content: content, closed: &atomic.Int32{}}, provider, DoclingWorkerConfig{Documents: c, ProfileID: "ranges", LeaseOwner: "worker", LeaseDuration: time.Minute, RetryDelay: time.Minute})
			requirements.NoError(err)
			digest := sha256.Sum256(content)
			result, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", MessageType: "email", Size: int64(len(content))})
			requirements.NoError(err)
			switch pages {
			case 50:
				assertions.Equal([]string{""}, selections)
			case 51:
				assertions.Equal([]string{"1,50", "51,51"}, selections)
			default:
				assertions.Equal([]string{"1,50", "51,100", "101,101"}, selections)
			}
			requirements.NotNil(catalog.publication)
			assertions.Equal(pages, result.Units)
			assertions.Empty(catalog.publication.Units[1].Text)
			assertions.Equal(fmt.Sprintf("page %d sentinel", pages), catalog.publication.Units[pages-1].Text)
		})
	}
}

func TestDoclingRangesResumeAfterReopen(t *testing.T) {
	for _, failure := range []string{"timeout", "partial"} {
		t.Run(failure, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			content := doclingPDFPages(101)
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
					status := "success"
					if failMiddle && first == 51 && failure == "timeout" {
						status = "pending"
					}
					assertions.NoError(json.NewEncoder(w).Encode(map[string]any{"task_id": "resume-job", "task_type": "convert", "task_status": status}))
				case "/v1/status/poll/resume-job":
					_, err := io.WriteString(w, `{"task_id":"resume-job","task_type":"convert","task_status":"pending"}`)
					assertions.NoError(err)
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
			worker, err := NewDoclingWorker(f.Store, &workerOpener{content: content}, provider, config)
			requirements.NoError(err)
			_, err = worker.ProcessCandidate(t.Context(), candidate)
			requirements.Error(err)
			var saved, heads int
			requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&saved))
			assertions.Equal(1, saved)
			requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_heads").Scan(&heads))
			assertions.Zero(heads)
			search, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "sentinel"})
			requirements.NoError(err)
			assertions.Empty(search.Results)
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
			changed, err := f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
			requirements.NoError(err)
			requirements.True(changed)
			failMiddle = false
			config.LeaseOwner = "after-restart"
			worker, err = NewDoclingWorker(f.Store, &workerOpener{content: content}, provider, config)
			requirements.NoError(err)
			result, err := worker.ProcessCandidate(t.Context(), candidate)
			requirements.NoError(err)
			assertions.Equal(101, result.Units)
			assertions.Equal([]string{"1,50", "51,100", "51,100", "101,101"}, selections)
			requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_heads").Scan(&heads))
			assertions.Equal(1, heads)
			var requests int
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT request_count FROM document_extractions WHERE id = ?"), result.ExtractionID).Scan(&requests))
			assertions.Equal(4, requests)
			search, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "sentinel"})
			requirements.NoError(err)
			assertions.NotEmpty(search.Results)
		})
	}
}

func TestDoclingRangesFallbackAndCoverage(t *testing.T) {
	for _, test := range []struct {
		name                                                 string
		degrade, missing, rebase, truncated, cap, blankFirst bool
	}{
		{name: "Markdown from all ranges", degrade: true},
		{name: "blank range before degraded range", degrade: true, blankFirst: true},
		{name: "missing page", missing: true},
		{name: "rebased page", rebase: true},
		{name: "truncate after complete coverage", truncated: true},
		{name: "whole document page cap", cap: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			content := doclingPDFPages(51)
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
			if test.cap {
				c.MaxPagesPerDocument = 50
			}
			provider, err := NewDoclingClient(&c)
			requirements.NoError(err)
			catalog := &workerCatalog{}
			worker, err := NewDoclingWorker(catalog, &workerOpener{content: content}, provider, DoclingWorkerConfig{Documents: c, ProfileID: "coverage", LeaseOwner: "worker", LeaseDuration: time.Minute, RetryDelay: time.Minute})
			requirements.NoError(err)
			digest := sha256.Sum256(content)
			result, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(digest[:]), MIMEType: "application/pdf", MessageType: "email", Size: int64(len(content))})
			if test.missing || test.rebase || test.cap {
				requirements.Error(err)
				assertions.Nil(catalog.publication)
				if test.missing || test.cap {
					assertions.Empty(catalog.ranges)
				}
				if test.cap {
					assertions.Empty(selections)
				}
				return
			}
			requirements.NoError(err)
			requirements.NotNil(catalog.publication)
			assertions.Equal([]string{"1,50", "51,51"}, selections)
			if test.degrade {
				assertions.Equal("document", catalog.publication.UnitKind)
				requirements.Len(catalog.publication.Units, 1)
				if test.blankFirst {
					assertions.Equal("range 51..51 Markdown sentinel", catalog.publication.Units[0].Text)
				} else {
					assertions.Equal("range 1..50 Markdown sentinel\nrange 51..51 Markdown sentinel", catalog.publication.Units[0].Text)
				}
			}
			if test.truncated {
				assertions.True(result.Truncated)
				assertions.Equal(51, result.ProviderUnits)
			}
		})
	}
}
