package forge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHTTPErrorNamesTheStatusOnce(t *testing.T) {
	for _, tc := range []struct {
		body, want string
	}{
		{`{"message":"Not Found"}`, "GET /repos/acme/widgets: 404 Not Found"},
		{`{"message":"404 Not Found"}`, "GET /repos/acme/widgets: 404 Not Found"},
		{``, "GET /repos/acme/widgets: 404 Not Found"},
		{`{"message":"404 Project Not Found"}`, "GET /repos/acme/widgets: 404 Not Found: 404 Project Not Found"},
		{`repository is archived`, "GET /repos/acme/widgets: 404 Not Found: repository is archived"},
	} {
		recorder := httptest.NewRecorder()
		recorder.WriteHeader(http.StatusNotFound)
		_, _ = recorder.WriteString(tc.body)
		resp := recorder.Result()
		resp.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://forge.example.com/repos/acme/widgets?token=secret", strings.NewReader(""))
		assert.Equal(t, tc.want, StatusError(resp).Error(), tc.body)
	}
}
