package documentindex

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/docbank/document/mistral"
)

func TestDocumentFailureDetailFiltersContentAndPaths(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"parser", fmt.Errorf("%w: count local Mistral OCR units: missing relationship private-target", errDocumentPreparation), "invalid document structure while counting local units"},
		{"conversion parser", fmt.Errorf("%w: verify generated PDF: private-trailer", errDocumentPreparation), "invalid generated PDF structure"},
		{"PDF parser", fmt.Errorf("%w: read PDF object graph: private-token", errDocumentPreparation), "invalid PDF structure"},
		{"PDF end marker", fmt.Errorf("%w: PDF end marker is missing or not final near synthetic-parser-token at /tmp/synthetic-document/doc.pdf", errDocumentPreparation), "PDF end marker is missing or not final"},
		{"unknown preparation", fmt.Errorf("%w: parser reported token synthetic-parser-token at /tmp/synthetic-document/doc.docx", errDocumentPreparation), "invalid local document source"},
		{"provider", errors.New("private provider response sentinel"), ""},
		{"opener", fmt.Errorf("%w: open document attachment: private-opener-sentinel", errDocumentPreparation), "open document attachment failed"},
		{"path", fmt.Errorf("%w: %w", errDocumentPreparation, &os.PathError{Op: "open", Path: "/private/path-sentinel", Err: syscall.ENOSPC}), "local document I/O (open): no space left on device"},
		{"missing path", fmt.Errorf("%w: %w", errDocumentPreparation, &os.PathError{Op: "open", Path: "/private/path-sentinel", Err: syscall.ENOENT}), "local document I/O: file does not exist"},
		{"permission denied", fmt.Errorf("%w: %w", errDocumentPreparation, &os.PathError{Op: "open", Path: "/private/path-sentinel", Err: syscall.EACCES}), "local document I/O: permission denied"},
		{"spool", fmt.Errorf("%w: %w", errDocumentPreparation, mistral.ErrSpoolCapacity), "document spool capacity unavailable: quota or free-space reserve exhausted"},
		{"spool copy capacity", fmt.Errorf("%w: %w: copy Mistral OCR spool: private-spool-path", errDocumentPreparation, mistral.ErrSpoolCapacity), "document spool capacity unavailable: quota or free-space reserve exhausted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, documentFailureDetail(tc.err)) })
	}
}
