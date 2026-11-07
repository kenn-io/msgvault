package cmd

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const feedCommandsAtom = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example Project releases</title>
  <link href="https://example.com/"/>
  <id>urn:example:releases</id>
  <updated>2026-11-07T15:00:00Z</updated>
  <entry>
    <title>Example Project 1.2.0</title>
    <link href="https://example.com/releases/1.2.0"/>
    <id>urn:example:release:1.2.0</id>
    <updated>2026-11-07T15:00:00Z</updated>
    <content type="html">Faster widgetsync.</content>
  </entry>
</feed>`

// runRootForTest runs one msgvault command line in this process, as the
// daemon's CLI child would, and returns its stdout.
func runRootForTest(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()
	command, _, err := rootCmd.Find(args)
	require.NoError(t, err)
	homeFlag := rootCmd.PersistentFlags().Lookup("home")
	previousHome, previousHomeChanged := homeFlag.Value.String(), homeFlag.Changed
	prevOut, prevErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	defer func() {
		command.Flags().VisitAll(func(flag *pflag.Flag) {
			_ = flag.Value.Set(flag.DefValue)
			flag.Changed = false
		})
		_ = homeFlag.Value.Set(previousHome)
		homeFlag.Changed = previousHomeChanged
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	}()
	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs(append([]string{"--home", home}, args...))
	err = rootCmd.ExecuteContext(t.Context())
	return stdout.String(), err
}

func TestFeedCommandsEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases.atom" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(feedCommandsAtom))
	}))
	t.Cleanup(server.Close)
	feedURL := server.URL + "/releases.atom"

	out, err := runRootForTest(t, home, "add-feed", feedURL)
	require.NoError(err)
	assert.Contains(out, "Example Project releases: 1 new of 1 items")

	out, err = runRootForTest(t, home, "list-feeds")
	require.NoError(err)
	assert.Contains(out, "Example Project releases")
	assert.Contains(out, feedURL)

	out, err = runRootForTest(t, home, "sync-feeds")
	require.NoError(err)
	assert.Contains(out, "Example Project releases: unchanged")

	out, err = runRootForTest(t, home, "add-feed", server.URL+"/missing.atom")
	require.Error(err, "a URL that is not a feed fails the command")
	assert.Contains(out, "failed")

	out, err = runRootForTest(t, home, "remove-feed", feedURL)
	require.NoError(err)
	assert.Contains(out, "Stopped following")
	out, err = runRootForTest(t, home, "list-feeds")
	require.NoError(err)
	assert.Contains(out, "No feeds followed")
	out, err = runRootForTest(t, home, "list-feeds", "--all", "--json")
	require.NoError(err)
	assert.Contains(out, `"followed":false`)

	opml := filepath.Join(t.TempDir(), "subscriptions.opml")
	require.NoError(os.WriteFile(opml, []byte(`<opml version="2.0"><body>
  <outline type="rss" text="Releases" xmlUrl="`+feedURL+`"/>
  <outline type="rss" text="Other" xmlUrl="https://other.example.com/feed.xml"/>
</body></opml>`), 0o600))
	out, err = runRootForTest(t, home, "add-feed", "--opml", opml)
	require.NoError(err)
	assert.Contains(out, "Following 2 new feeds (0 already followed)")
}
