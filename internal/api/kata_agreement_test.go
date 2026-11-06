package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataissues"
)

type agreementOperationsFake struct {
	fakeKataIssueOperations

	ref   string
	input kataissues.AgreementInput
}

func (f *agreementOperationsFake) Agreement(_ context.Context, ref string) (kataissues.AgreementView, error) {
	f.ref = ref
	return kataissues.AgreementView{Pending: []string{}}, f.err
}

func (f *agreementOperationsFake) RecordAgreement(_ context.Context, ref string, input kataissues.AgreementInput) (kataissues.AgreementView, error) {
	f.ref, f.input = ref, input
	return kataissues.AgreementView{AcceptedEventID: input.ID}, f.err
}

func TestKataAgreementHTTP(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	operations := &agreementOperationsFake{}
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}, Store: &mockStore{}, Logger: testLogger(), KataIssueOperations: operations})
	path := "/api/v1/integrations/kata/issues/example%23abcd/agreement"
	read := httptest.NewRecorder()
	server.router.ServeHTTP(read, httptest.NewRequest(http.MethodGet, path, nil))
	must.Equal(http.StatusOK, read.Code, read.Body.String())
	check.Equal("example#abcd", operations.ref)
	check.Equal("no-store", read.Header().Get("Cache-Control"))
	write := serveKataIssue(server, path, `{"id":"a","action":"accept","actor":"obligor","target":"r","expected_events":1,"reviewed":true,"evidence":[]}`, nil)
	must.Equal(http.StatusOK, write.Code, write.Body.String())
	check.Equal("a", operations.input.ID)
	check.True(operations.input.Reviewed)
	unknown := serveKataIssue(server, path, `{"unknown":true}`, nil)
	check.Equal(http.StatusBadRequest, unknown.Code)
	for _, operation := range []string{"getKataAgreement", "recordKataAgreement"} {
		check.False(delegatedOperationAllowed(operation), "agreement history is owner-only")
		check.False(remoteClientOperationAllowed(operation, false), "read-only remote keys cannot read agreement history")
	}
	operations.err = kataissues.ErrAgreementConflict
	conflict := serveKataIssue(server, path, `{"evidence":[]}`, nil)
	check.Equal(http.StatusConflict, conflict.Code)
	check.Contains(conflict.Body.String(), "agreement_conflict")
	operations.err = kataissues.ErrAgreementEvidenceUnlinked
	unlinked := serveKataIssue(server, path, `{"evidence":[]}`, nil)
	check.Equal(http.StatusUnprocessableEntity, unlinked.Code)
	check.Contains(unlinked.Body.String(), "agreement_evidence_unlinked")
	oversize := serveKataIssue(server, path, strings.Repeat(" ", (512<<10)+1), nil)
	check.Equal(http.StatusBadRequest, oversize.Code)
}
