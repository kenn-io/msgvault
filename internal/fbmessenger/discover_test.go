package fbmessenger

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestDiscover_JSONSimple(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dirs, err := Discover("testdata/json_simple")
	require.NoError(err, "Discover")
	// json_simple has one inbox thread and one archived thread.
	want := []struct {
		section, name, format string
	}{
		{"archived_threads", "zoe_ARCH", "json"},
		{"inbox", "alice_ABC123", "json"},
	}
	require.Len(dirs, len(want), "discovered threads: %+v", dirs)
	for i, w := range want {
		assert.Equal(w.section, dirs[i].Section, "[%d] section", i)
		assert.Equal(w.name, dirs[i].Name, "[%d] name", i)
		assert.Equal(w.format, dirs[i].Format, "[%d] format", i)
		assert.True(filepath.IsAbs(dirs[i].Path), "[%d] path not absolute: %q", i, dirs[i].Path)
	}
}

func TestDiscover_HTMLOnly(t *testing.T) {
	dirs, err := Discover("testdata/html_simple")
	requirepkg.NoError(t, err, "Discover")
	requirepkg.Len(t, dirs, 1)
	assertpkg.Equal(t, "html", dirs[0].Format)
}

func TestDiscover_Both(t *testing.T) {
	dirs, err := Discover("testdata/mixed")
	requirepkg.NoError(t, err, "Discover")
	requirepkg.Len(t, dirs, 1)
	assertpkg.Equal(t, "both", dirs[0].Format)
}

func TestDiscover_AbsoluteAndRelativeInvariance(t *testing.T) {
	require := requirepkg.New(t)
	rel, err := Discover("testdata/json_simple")
	require.NoError(err, "relative Discover")
	absRoot, err := filepath.Abs("testdata/json_simple")
	require.NoError(err)
	abs, err := Discover(absRoot)
	require.NoError(err, "absolute Discover")
	sort.Slice(rel, func(i, j int) bool { return rel[i].Path < rel[j].Path })
	sort.Slice(abs, func(i, j int) bool { return abs[i].Path < abs[j].Path })
	assertpkg.Equal(t, abs, rel, "relative vs absolute differ")
}

func TestDiscover_IgnoresHiddenAndMediaSubdirs(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// json_with_media contains a photos/ subdir with tiny.png; it must
	// not be returned as a thread dir.
	dirs, err := Discover("testdata/json_with_media")
	require.NoError(err, "Discover")
	require.Len(dirs, 1)
	assert.Equal("bob_XYZ789", dirs[0].Name)
	// None of the returned paths should point at photos/, videos/, etc.
	for _, d := range dirs {
		base := filepath.Base(d.Path)
		assert.NotEqual("photos", base, "unexpected media subdir yielded: %q", d.Path)
		assert.NotEqual("videos", base, "unexpected media subdir yielded: %q", d.Path)
	}
}

func TestDiscover_AlternateLayouts(t *testing.T) {
	// Verify all three messagesRootCandidates layouts are discovered.
	layouts := []string{
		filepath.Join("your_activity_across_facebook", "messages"),
		filepath.Join("your_facebook_activity", "messages"),
		"messages",
	}
	for _, layout := range layouts {
		t.Run(layout, func(t *testing.T) {
			require := requirepkg.New(t)
			tmp := t.TempDir()
			threadDir := filepath.Join(tmp, layout, "inbox", "testthread_1")
			require.NoError(os.MkdirAll(threadDir, 0755))
			require.NoError(os.WriteFile(
				filepath.Join(threadDir, "message_1.json"),
				[]byte(`{"participants":[{"name":"A"}],"messages":[]}`),
				0644,
			))
			dirs, err := Discover(tmp)
			require.NoError(err, "Discover")
			require.Len(dirs, 1, "discovered: %+v", dirs)
			assertpkg.Equal(t, "testthread_1", dirs[0].Name)
		})
	}
}

// TestDiscover_UnnumberedJSONSiblingNotMisclassified guards against a thread
// directory that contains valid HTML plus only an unnumbered JSON sibling
// (e.g. message_final.json) being classified as JSON or "both". ParseJSONThread
// only accepts numbered message_<N>.json files, so misclassifying as JSON
// would cause the thread to fail in auto/json mode and the valid HTML to
// be skipped.
func TestDiscover_UnnumberedJSONSiblingNotMisclassified(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "foo_1")
	require.NoError(os.MkdirAll(threadDir, 0755))
	require.NoError(os.WriteFile(
		filepath.Join(threadDir, "message_1.html"),
		[]byte(`<html><body>hi</body></html>`),
		0644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(threadDir, "message_final.json"),
		[]byte(`{"unrelated":true}`),
		0644,
	))

	dirs, err := Discover(tmp)
	require.NoError(err, "Discover")
	require.Len(dirs, 1, "discovered: %+v", dirs)
	assertpkg.Equal(t, "html", dirs[0].Format,
		"unnumbered JSON sibling must not promote thread to json/both")
}

func TestDiscover_IgnoresDSStore(t *testing.T) {
	require := requirepkg.New(t)
	// Create a temp DYI tree with a .DS_Store at the thread level; it
	// must not turn it into a thread dir.
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "foo_1")
	require.NoError(os.MkdirAll(threadDir, 0755))
	require.NoError(os.WriteFile(filepath.Join(threadDir, "message_1.json"), []byte(`{"participants":[{"name":"A"}],"messages":[]}`), 0644))
	// Add a .DS_Store sibling and a .hidden dir at section level.
	section := filepath.Dir(threadDir)
	require.NoError(os.WriteFile(filepath.Join(section, ".DS_Store"), []byte("x"), 0644))
	require.NoError(os.Mkdir(filepath.Join(section, ".hidden"), 0755))

	dirs, err := Discover(tmp)
	require.NoError(err, "Discover")
	require.Len(dirs, 1, "discovered: %+v", dirs)
	assertpkg.Equal(t, "foo_1", dirs[0].Name)
}
