package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

// DocbankAttachmentStatus reports occurrence counts; shared content uploads once.
type DocbankAttachmentStatus struct {
	Enabled       bool                           `json:"enabled"`
	UploadConsent bool                           `json:"upload_consent"`
	Collection    string                         `json:"collection"`
	Counts        store.DocbankAttachmentSummary `json:"counts"`
}

type DocbankAttachmentBackfillResponse struct {
	Status string `json:"status"`
}

type docbankAttachmentStore interface {
	ArchiveUIDContext(ctx context.Context) (string, error)
	DocbankAttachmentStatus(ctx context.Context, destination string) (store.DocbankAttachmentSummary, error)
	RetryDocbankAttachments(ctx context.Context, destination string) error
}

func (s *Server) docbankAttachmentDestination(ctx context.Context) (docbankAttachmentStore, string, error) {
	st, ok := s.store.(docbankAttachmentStore)
	if !ok {
		return nil, "", errors.New("attachment mirror store unavailable")
	}
	uid, err := st.ArchiveUIDContext(ctx)
	if err != nil {
		return nil, "", err
	}
	cfg := s.cfg.Integrations.Docbank
	return st, docbankmedia.AttachmentDestinationKey(cfg.URL, uid, cfg.MirrorCollection()), nil
}

func (s *Server) registerDocbankAttachmentRoutes(api huma.API) {
	statusOp := rawAPIV1Operation("getDocbankAttachmentStatus", http.MethodGet,
		"/integrations/docbank/attachments", "Read attachment mirror status")
	statusOp.Errors = append(statusOp.Errors, http.StatusServiceUnavailable)
	huma.Register(api, statusOp, func(ctx context.Context, _ *struct{}) (*struct{ Body DocbankAttachmentStatus }, error) {
		st, destination, err := s.docbankAttachmentDestination(ctx)
		if err != nil {
			return nil, huma.Error503ServiceUnavailable("attachment mirror store unavailable")
		}
		counts, err := st.DocbankAttachmentStatus(ctx, destination)
		if err != nil {
			return nil, huma.Error500InternalServerError("read attachment mirror status")
		}
		cfg := s.cfg.Integrations.Docbank
		return &struct{ Body DocbankAttachmentStatus }{Body: DocbankAttachmentStatus{
			Enabled: cfg.Enabled && cfg.AttachmentMirror, UploadConsent: cfg.AttachmentUploadConsent,
			Collection: cfg.MirrorCollection(), Counts: counts,
		}}, nil
	})
	backfillOp := rawAPIV1Operation("backfillDocbankAttachments", http.MethodPost,
		"/integrations/docbank/attachments/backfill", "Retry failures and restart attachment mirror discovery")
	backfillOp.DefaultStatus = http.StatusAccepted
	backfillOp.Errors = append(backfillOp.Errors, http.StatusConflict, http.StatusServiceUnavailable)
	huma.Register(api, backfillOp, func(ctx context.Context, _ *struct{}) (*struct {
		Body DocbankAttachmentBackfillResponse
	}, error) {
		cfg := s.cfg.Integrations.Docbank
		if !cfg.Enabled || !cfg.AttachmentMirror || !cfg.AttachmentUploadConsent {
			return nil, huma.Error409Conflict("enable attachment_mirror and attachment_upload_consent first")
		}
		if s.scheduler == nil || !s.scheduler.IsJobScheduled(scheduler.StoredMediaSubmitJob) {
			return nil, huma.Error503ServiceUnavailable("stored media scheduler unavailable")
		}
		st, destination, err := s.docbankAttachmentDestination(ctx)
		if err != nil {
			return nil, huma.Error503ServiceUnavailable("attachment mirror store unavailable")
		}
		if err := st.RetryDocbankAttachments(ctx, destination); err != nil {
			return nil, huma.Error500InternalServerError("restart attachment mirror")
		}
		if _, err := s.scheduler.StartJob(scheduler.StoredMediaSubmitJob); err != nil {
			return nil, huma.Error503ServiceUnavailable("start stored media job")
		}
		return &struct {
			Body DocbankAttachmentBackfillResponse
		}{Body: DocbankAttachmentBackfillResponse{Status: "scheduled"}}, nil
	})
}
