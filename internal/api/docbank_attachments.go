package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

// DocbankAttachmentStatus reports mirror outcomes and scan progress.
type DocbankAttachmentStatus struct {
	State         string                          `json:"state" enum:"running,docbank_disabled,consent_off,worker_unavailable"`
	UploadConsent bool                            `json:"upload_consent"`
	Folder        string                          `json:"folder"`
	Counts        *store.DocbankAttachmentSummary `json:"counts,omitempty"`
}

type DocbankAttachmentBackfillResponse struct {
	Status string `json:"status"`
}

const DocbankAttachmentMirrorJob = "docbank-attachment-mirror"

type DocbankAttachmentReader struct {
	Store       *store.Store
	Destination string
	Folder      string
}

func (s *Server) registerDocbankAttachmentRoutes(api huma.API) {
	statusOp := rawAPIV1Operation("getDocbankAttachmentStatus", http.MethodGet,
		"/integrations/docbank/attachments", "Read attachment mirror status")
	statusOp.Errors = append(statusOp.Errors, http.StatusServiceUnavailable)
	huma.Register(api, statusOp, func(ctx context.Context, _ *struct{}) (*struct{ Body DocbankAttachmentStatus }, error) {
		reader := s.docbankAttachments
		if s.cfg != nil && (!s.cfg.Integrations.Docbank.Enabled || !s.cfg.Integrations.Docbank.AttachmentUploadConsent) {
			state := "consent_off"
			if !s.cfg.Integrations.Docbank.Enabled {
				state = "docbank_disabled"
			}
			return &struct{ Body DocbankAttachmentStatus }{Body: DocbankAttachmentStatus{State: state, UploadConsent: s.cfg.Integrations.Docbank.AttachmentUploadConsent, Folder: s.cfg.Integrations.Docbank.MirrorFolder()}}, nil
		}
		if reader == nil || reader.Store == nil {
			return nil, huma.Error503ServiceUnavailable("attachment mirror store unavailable")
		}
		if s.scheduler == nil || !s.scheduler.IsJobScheduled(DocbankAttachmentMirrorJob) {
			// Consent stands, but nothing is being mirrored until the worker starts.
			return &struct{ Body DocbankAttachmentStatus }{Body: DocbankAttachmentStatus{State: "worker_unavailable", UploadConsent: true, Folder: reader.Folder}}, nil
		}
		counts, err := reader.Store.DocbankAttachmentStatus(ctx, reader.Destination)
		if err != nil {
			return nil, huma.Error500InternalServerError("read attachment mirror status")
		}
		return &struct{ Body DocbankAttachmentStatus }{Body: DocbankAttachmentStatus{
			State: "running", UploadConsent: true,
			Folder: reader.Folder, Counts: &counts,
		}}, nil
	})
	backfillOp := rawAPIV1Operation("backfillDocbankAttachments", http.MethodPost,
		"/integrations/docbank/attachments/backfill", "Retry failures and restart attachment mirror discovery")
	backfillOp.DefaultStatus = http.StatusAccepted
	backfillOp.Errors = append(backfillOp.Errors, http.StatusConflict, http.StatusServiceUnavailable)
	huma.Register(api, backfillOp, func(ctx context.Context, _ *struct{}) (*struct {
		Body DocbankAttachmentBackfillResponse
	}, error) {
		if s.cfg != nil && (!s.cfg.Integrations.Docbank.Enabled || !s.cfg.Integrations.Docbank.AttachmentUploadConsent) {
			return nil, huma.Error409Conflict("enable the Docbank integration and attachment_upload_consent first")
		}
		if s.docbankAttachments == nil {
			return nil, huma.Error503ServiceUnavailable("attachment mirror store unavailable")
		}
		if s.scheduler == nil || !s.scheduler.IsJobScheduled(DocbankAttachmentMirrorJob) {
			return nil, huma.Error503ServiceUnavailable("attachment mirror scheduler unavailable")
		}
		reader := s.docbankAttachments
		if reader.Store == nil {
			return nil, huma.Error503ServiceUnavailable("attachment mirror store unavailable")
		}
		if err := reader.Store.RetryDocbankAttachments(ctx, reader.Destination); err != nil {
			return nil, huma.Error500InternalServerError("restart attachment mirror")
		}
		if _, err := s.scheduler.StartJob(DocbankAttachmentMirrorJob, false); err != nil {
			return nil, huma.Error503ServiceUnavailable("start attachment mirror job")
		}
		return &struct {
			Body DocbankAttachmentBackfillResponse
		}{Body: DocbankAttachmentBackfillResponse{Status: "scheduled"}}, nil
	})
}
