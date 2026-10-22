package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
)

type EmailFamilyExportInput struct {
	ExpectedFamilyRevision string  `json:"expected_family_revision"`
	SourceIDs              []int64 `json:"source_ids,omitzero"`
}

type EmailFamilyExportStore interface {
	ExportEmailFamilyContext(ctx context.Context, messageID int64, revision string, options store.EmailFamilyOptions, output io.Writer) (*store.EmailFamilyExportManifest, error)
}

func (s *Server) registerEmailFamilyExportRoutes(api huma.API) {
	op := rawAPIV1Operation("exportEmailFamily", http.MethodPost, "/messages/{id}/family/export", "Download archived email copies and provenance from one snapshot")
	op.Parameters = []*huma.Param{pathIntegerParam("Live archived email message ID")}
	op.RequestBody = jsonRequestBodyFor[EmailFamilyExportInput](api)
	op.Responses = binaryResponsesFor(api, "application/zip", 400, 401, 403, 404, 409, 503)
	registerRawHumaRoute(api, op, s.handleExportEmailFamily)
}

func (s *Server) handleExportEmailFamily(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	backend, ok := s.store.(EmailFamilyExportStore)
	if !ok {
		writeError(w, 503, "email_family_unavailable", "Email family export is unavailable")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, 400, "invalid_request", "Message ID must be positive")
		return
	}
	var input EmailFamilyExportInput
	if !decodeEntityRequest(w, r, &input, "email family export") {
		return
	}
	if len(input.SourceIDs) > 100 {
		writeError(w, 400, "invalid_source_ids", "At most 100 source IDs are allowed")
		return
	}
	// Stage on disk: failures and revision conflicts must be reported before ZIP headers.
	staged, err := os.CreateTemp("", "msgvault-family-*.zip")
	if err != nil {
		s.writeEmailFamilyDecisionError(w, err)
		return
	}
	defer func() { _ = staged.Close(); _ = os.Remove(staged.Name()) }()
	if err := fileutil.SecureChmod(staged.Name(), 0o600); err != nil {
		s.writeEmailFamilyDecisionError(w, err)
		return
	}
	_, err = backend.ExportEmailFamilyContext(r.Context(), id, input.ExpectedFamilyRevision, store.EmailFamilyOptions{SourceIDs: input.SourceIDs}, staged)
	if err != nil {
		s.writeEmailFamilyDecisionError(w, err)
		return
	}
	size, err := staged.Seek(0, io.SeekCurrent)
	if err != nil {
		s.writeEmailFamilyDecisionError(w, err)
		return
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		s.writeEmailFamilyDecisionError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="email-family-%d.zip"`, id))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, staged); err != nil {
		s.logger.Debug("email family download interrupted", "error", err)
	}
}
