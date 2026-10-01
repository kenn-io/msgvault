package documentindex

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/docbank/document/ocr"
)

func TestDocumentFailureDetailFiltersContentAndPaths(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"parser", fmt.Errorf("%w: count local Mistral OCR units: missing relationship private-target", errDocumentPreparation), "invalid local document source"},
		{"conversion parser", fmt.Errorf("%w: verify generated PDF: private-trailer", errDocumentPreparation), "invalid local document source"},
		{"PDF parser", fmt.Errorf("%w: read PDF object graph: private-token", errDocumentPreparation), "invalid local document source"},
		{"PDF end marker", fmt.Errorf("%w: PDF end marker is missing or not final near synthetic-parser-token at /tmp/synthetic-document/doc.pdf", errDocumentPreparation), "invalid local document source"},
		{"unknown preparation", fmt.Errorf("%w: parser reported token synthetic-parser-token at /tmp/synthetic-document/doc.docx", errDocumentPreparation), "invalid local document source"},
		{"provider", errors.New("private provider response sentinel"), ""},
		{"opener", fmt.Errorf("%w: %w: private-opener-sentinel", errDocumentPreparation, errDocumentAttachmentOpen), "open document attachment failed"},
		{"path", fmt.Errorf("%w: %w", errDocumentPreparation, &os.PathError{Op: "open", Path: "/private/path-sentinel", Err: syscall.ENOSPC}), "local document I/O (open): no space left on device"},
		{"missing path", fmt.Errorf("%w: %w", errDocumentPreparation, &os.PathError{Op: "open", Path: "/private/path-sentinel", Err: syscall.ENOENT}), "local document I/O: file does not exist"},
		{"permission denied", fmt.Errorf("%w: %w", errDocumentPreparation, &os.PathError{Op: "open", Path: "/private/path-sentinel", Err: syscall.EACCES}), "local document I/O: permission denied"},
		{"shared preparation", fmt.Errorf("%w: %w", errDocumentPreparation, ocr.NewPreparationError("source size mismatch", errors.New("synthetic-private-parser-token"))), "source size mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, documentFailureDetail(tc.err)) })
	}
}
