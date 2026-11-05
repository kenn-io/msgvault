package daemonclient

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/hex"
)

func TestImportHexRemoteValidationIsFatal(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusUnprocessableEntity, Body: io.NopCloser(strings.NewReader(`{"error":"validation_failed"}`))}
	_, err := decodeRecorderResponse[hex.RemoteResult](response, hexRecorderErrors)
	require.Error(t, err)
	assert.NotErrorIs(t, err, hex.ErrRemoteValidation, "server rejection can be a source mismatch and must abort")
}

func TestImportHexRemoteRecordValidationCanContinue(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusUnprocessableEntity, Body: io.NopCloser(strings.NewReader(`{"error":"record_validation_failed","message":"attendee@example.com"}`))}
	_, err := decodeRecorderResponse[hex.RemoteResult](response, hexRecorderErrors)
	require.ErrorIs(t, err, hex.ErrRemoteValidation)
	assert.NotContains(t, err.Error(), "attendee@example.com")
}
