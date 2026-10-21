package zoomcalls

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	c := NewClient([]string{"unused"})
	c.base = s.URL
	c.http.Transport = s.Client().Transport
	c.token = func(context.Context) (string, error) { return "synthetic-bearer", nil }
	return c
}

func TestFetchMeetingPagination(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var paths []string
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		assert.Equal("Bearer synthetic-bearer", r.Header.Get("Authorization"))
		if r.URL.Query().Get("next_page_token") == "" {
			_, _ = fmt.Fprint(w, `{"participants":[],"next_page_token":"page-two"}`)
		} else {
			_, _ = fmt.Fprint(w, roster)
		}
	})
	raw, err := c.Meeting(t.Context(), "/instance//")
	require.NoError(err)
	assert.Len(paths, 2)
	assert.Contains(paths[0], url.PathEscape(url.PathEscape("/instance//")))
	o, err := Meeting("account", "owner", "/instance//", raw, nil)
	require.NoError(err)
	require.NotNil(o)
}

func TestFetchRejectsIncompleteResponses(t *testing.T) {
	oversizedJSON := `{"participants":[],"padding":"` + strings.Repeat("x", (1<<20)+1) + `"}`
	tests := []struct {
		name, body, wantError string
	}{
		{"malformed JSON", `{`, "invalid Zoom response JSON"},
		{"oversized valid JSON", oversizedJSON, "zoom response exceeds 1 MiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, tt.body) })
			_, err := c.Meeting(t.Context(), "instance")
			require.ErrorContains(t, err, tt.wantError)
			assert.NotContains(t, err.Error(), "synthetic-bearer")
		})
	}
}

func TestFetchRefusesRedirect(t *testing.T) {
	destinationRequests := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			destinationRequests++
			_, _ = fmt.Fprint(w, roster)
			return
		}
		http.Redirect(w, r, "/other", http.StatusFound)
	})
	_, err := c.Meeting(t.Context(), "instance")
	require.Error(t, err)
	assert.Zero(t, destinationRequests)
}

func TestFetchRejectsNumericMeetingID(t *testing.T) {
	requests := 0
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, roster)
	})
	_, err := c.Meeting(t.Context(), "1234567890")
	require.ErrorContains(t, err, "exact instance UUID")
	assert.Zero(t, requests)
}

func TestFetchRejectsRepeatedPageToken(t *testing.T) {
	requests := 0
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, `{"participants":[],"next_page_token":"same-token"}`)
	})
	_, err := c.Meeting(t.Context(), "instance")
	require.ErrorContains(t, err, "repeated Zoom page token")
	assert.Equal(t, 2, requests)
}

func TestFetchRejectsUncoveredReportedTotal(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"participants":[],"total_records":2}`)
	})
	_, err := c.Meeting(t.Context(), "instance")
	require.Error(t, err)
}

func TestFetchPreservesCancellation(t *testing.T) {
	c := fixtureClient(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.Meeting(ctx, "instance")
	require.ErrorIs(t, err, context.Canceled)
}

// The subprocess exercises os/exec's real io.Copy dispatch, including its
// ReaderFrom optimization; calling the output buffer directly misses this bug.
func TestTokenCommandBoundsRealProcessOutput(t *testing.T) {
	t.Setenv("MSGVAULT_ZOOM_TOKEN_HELPER", "oversize")
	_, err := commandToken(t.Context(), []string{os.Args[0], "-test.run=^TestZoomTokenHelper$"})
	require.Error(t, err)
}

func TestZoomTokenHelper(t *testing.T) {
	switch os.Getenv("MSGVAULT_ZOOM_TOKEN_HELPER") {
	case "oversize":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 128<<10))
		os.Exit(0)
	case "literal":
		_, _ = fmt.Fprint(os.Stdout, os.Args[len(os.Args)-1])
		os.Exit(0)
	case "failure":
		_, _ = fmt.Fprint(os.Stdout, "synthetic-sensitive-output")
		_, _ = fmt.Fprint(os.Stderr, "synthetic-sensitive-output")
		os.Exit(7)
	}
}

func TestTokenCommandKeepsArgumentsLiteral(t *testing.T) {
	t.Setenv("MSGVAULT_ZOOM_TOKEN_HELPER", "literal")
	want := "literal$(unexecuted);&"
	got, err := commandToken(t.Context(), []string{os.Args[0], "-test.run=^TestZoomTokenHelper$", "--", want})
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestNewClientCopiesCommand(t *testing.T) {
	t.Setenv("MSGVAULT_ZOOM_TOKEN_HELPER", "literal")
	argv := []string{os.Args[0], "-test.run=^TestZoomTokenHelper$", "--", "synthetic-original"}
	c := NewClient(argv)
	argv[len(argv)-1] = "mutated"
	got, err := c.token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "synthetic-original", got)
}

func TestTokenCommandFailureSuppressesOutput(t *testing.T) {
	t.Setenv("MSGVAULT_ZOOM_TOKEN_HELPER", "failure")
	_, err := commandToken(t.Context(), []string{os.Args[0], "-test.run=^TestZoomTokenHelper$"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "synthetic-sensitive-output")
}

func TestFetchRetryBudgetAndSanitizedErrors(t *testing.T) {
	for _, status := range []int{401, 403, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "synthetic-sensitive-body")
			})
			_, err := c.Meeting(t.Context(), "instance")
			require.Error(t, err)
			want := 1
			if status == 503 {
				want = 3
			}
			assert.Equal(t, want, calls)
			assert.NotContains(t, err.Error(), "synthetic-sensitive-body")
		})
	}
}

func TestFetchHasPageLimit(t *testing.T) {
	calls := 0
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = fmt.Fprintf(w, `{"participants":[],"next_page_token":%q}`, strconv.Itoa(calls))
	})
	_, err := c.Meeting(t.Context(), "instance")
	require.ErrorContains(t, err, "page limit")
	assert.Equal(t, 100, calls)
}

func FuzzZoomUUIDSurvivesRequestPath(f *testing.F) {
	f.Add(uint64(0))
	f.Add(uint64(1))
	f.Fuzz(func(t *testing.T, seed uint64) {
		uuid := fmt.Sprintf("id-%x/%%+雪", seed)
		double := seed%2 != 0
		if double {
			uuid = "/" + uuid + "//"
		}
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/past_meetings/"), "/participants")
			if double {
				decoded, err := url.PathUnescape(got)
				if err != nil {
					http.Error(w, "invalid escaped path", http.StatusBadRequest)
					return
				}
				got = decoded
			}
			assert.Equal(t, uuid, got)
			_, _ = fmt.Fprint(w, roster)
		})
		_, err := c.Meeting(t.Context(), uuid)
		require.NoError(t, err)
	})
}
