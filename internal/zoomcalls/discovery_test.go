package zoomcalls

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryFiltersAndStages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	day := monthStart.AddDate(0, 0, -1).Format(time.DateOnly)
	to := monthStart.AddDate(0, 0, 1).Format(time.DateOnly)
	reportCalls := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/report/") {
			reportCalls++
			assert.Equal("past", r.URL.Query().Get("type"))
			if r.URL.Query().Get("from") <= day && day <= r.URL.Query().Get("to") {
				_, _ = fmt.Fprintf(w, `{"meetings":[{"uuid":"instance","type":2,"start_time":"%sT10:00:00Z"},{"uuid":"webinar","type":5,"start_time":"%sT10:00:00Z"}]}`, day, day)
			} else {
				_, _ = fmt.Fprint(w, `{"meetings":[]}`)
			}
		} else {
			assert.Contains(r.URL.Path, "instance")
			_, _ = fmt.Fprint(w, roster)
		}
	})
	items, skipped, err := c.Meetings(t.Context(), "owner", day, to)
	require.NoError(err)
	assert.Equal(2, reportCalls)
	require.Len(items, 1)
	assert.Equal(1, skipped)
	assert.Equal("instance", items[0].UUID)
}

func TestDiscoveryIncludesPMIMeetings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	day := time.Now().UTC().AddDate(0, 0, -2).Format(time.DateOnly)
	fetched := false
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/report/") {
			_, _ = fmt.Fprintf(w, `{"meetings":[{"uuid":"pmi-instance","type":4,"start_time":"%sT10:00:00Z"}]}`, day)
		} else {
			assert.Equal("/past_meetings/pmi-instance/participants", r.URL.Path)
			fetched = true
			_, _ = fmt.Fprint(w, roster)
		}
	})
	items, skipped, err := c.Meetings(t.Context(), "owner", day, time.Now().UTC().Format(time.DateOnly))
	require.NoError(err)
	require.Len(items, 1)
	assert.True(fetched)
	assert.Equal("pmi-instance", items[0].UUID)
	assert.Zero(skipped)
}

func TestDateWindowsRejectInvalidSelections(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for _, window := range [][2]string{{"2026-10-02", "2026-10-01"}, {"2026-10-23", "2026-10-24"}, {"2026-09-01", "2026-10-03"}, {"2026-04-20", "2026-04-21"}, {"2026-02-29", "2026-03-01"}, {"bad", ""}} {
		_, err := dateWindows(window[0], window[1], now)
		require.Error(t, err, window)
	}
	_, err := dateWindows("2026-04-21", "2026-04-22", now)
	require.NoError(t, err)
}

func TestLeapMonthWindowsRemainContiguous(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	windows, err := dateWindows("2024-02-28", "2024-03-02", time.Date(2024, 3, 21, 0, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.Len(windows, 2)
	assert.Equal(windows[0][1], windows[1][0])
	assert.Equal(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), windows[0][1])
}

func FuzzDateWindows(f *testing.F) {
	f.Add("2026-09-25", "2026-10-02")
	f.Add("invalid", "")
	f.Fuzz(func(t *testing.T, from, to string) {
		windows, err := dateWindows(from, to, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
		if err != nil {
			return
		}
		a, err := time.Parse(time.DateOnly, from)
		require.NoError(t, err)
		b, err := time.Parse(time.DateOnly, to)
		require.NoError(t, err)
		require.NotEmpty(t, windows)
		assert.Equal(t, a, windows[0][0])
		assert.Equal(t, b, windows[len(windows)-1][1])
		for i, w := range windows {
			assert.True(t, w[0].Before(w[1]))
			assert.Equal(t, w[0].Month(), w[1].Add(-time.Nanosecond).Month())
			if i > 0 {
				assert.Equal(t, windows[i-1][1], w[0])
			}
		}
	})
}
