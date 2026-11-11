package mcp

import "go.kenn.io/msgvault/pkg/client/generated"

// DocumentMirrorStatus omits operator paths, diagnostics and unbounded labels.
type DocumentMirrorStatus struct {
	State         string                    `json:"state"`
	UploadConsent bool                      `json:"upload_consent"`
	Counts        *DocumentMirrorCounts     `json:"counts,omitempty"`
	Processing    *DocumentProcessingCounts `json:"processing,omitempty"`
}
type DocumentMirrorCounts struct {
	Sending   int64 `json:"sending"`
	Delivered int64 `json:"delivered"`
	Remaining int64 `json:"remaining"`
	Failed    int64 `json:"failed"`
}
type DocumentProcessingCounts struct {
	Waiting   int64 `json:"waiting"`
	Requested int64 `json:"requested"`
	Extracted int64 `json:"extracted"`
	Other     int64 `json:"other"`
}

func documentOperationalDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("get_docbank_attachment_status", "Read the configured Docbank attachment mirror state and aggregate counts.", OperationFamilyDocuments, closedObject(nil), outputSchemaFor[DocumentMirrorStatus](), false),
		newOperationalDefinition("retry_docbank_attachment_mirror", "Retry failed external attachment uploads, re-request recoverable document processing, and restart full discovery even with no failed uploads, under existing native upload and processing consent. Requires approval; scheduling is not completion.", OperationFamilyDocuments, closedObject(nil), outputSchemaFor[generated.DocbankAttachmentBackfillResponse](), true),
	}
}
