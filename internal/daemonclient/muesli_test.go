package daemonclient

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/muesli"
)

func TestImportMuesliRemoteValidationIsFatal(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusUnprocessableEntity, Body: io.NopCloser(strings.NewReader(`{"error":"validation_failed"}`))}
	_, err := decodeMuesliResponse(response)
	require.Error(t, err)
	assert.NotErrorIs(t, err, muesli.ErrRemoteValidation, "server rejection can be a source mismatch and must abort")
}

func TestImportMuesliRemoteRecordValidationCanContinue(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusUnprocessableEntity, Body: io.NopCloser(strings.NewReader(`{"error":"record_validation_failed","message":"attendee@example.com"}`))}
	_, err := decodeMuesliResponse(response)
	require.ErrorIs(t, err, muesli.ErrRemoteValidation)
	assert.NotContains(t, err.Error(), "attendee@example.com")
}
