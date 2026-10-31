package daemonclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIStreamExitCompatibility(t *testing.T) {
	for _, code := range []int{0, 2, 3, 4, 5, 6, 7, 130} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, "{\"type\":\"error\",\"error\":\"msgvault: cli subprocess exited non-zero\",\"exit_code\":%d}\n", code)
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			defer func() { _ = client.Close() }()
			err = client.RunCLICommand(t.Context(), CLIRunRequest{Args: []string{"show-message", "1"}}, nil)
			var got *CLIRunError
			require.ErrorAs(t, err, &got)
			want := code
			if want == 0 {
				want = 1
			}
			assert.Equal(t, want, got.ExitCode())
		})
	}
}
