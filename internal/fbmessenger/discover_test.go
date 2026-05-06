package fbmessenger

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestDiscover_JSONSimple(t *testing.T) {
	dirs, err := Discover("testdata/json_simple")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// json_simple has one inbox thread and one archived thread.
	want := []struct {
		section, name, format string
	}{
		{"archived_threads", "zoe_ARCH", "json"},
		{"inbox", "alice_ABC123", "json"},
	}
	if len(dirs) != len(want) {
		t.Fatalf("discovered %d threads, want %d: %+v", len(dirs), len(want), dirs)
	}
	for i, w := range want {
		if dirs[i].Section != w.section {
			t.Errorf("[%d] section=%q want %q", i, dirs[i].Section, w.section)
		}
		if dirs[i].Name != w.name {
			t.Errorf("[%d] name=%q want %q", i, dirs[i].Name, w.name)
		}
		if dirs[i].Format != w.format {
			t.Errorf("[%d] format=%q want %q", i, dirs[i].Format, w.format)
		}
		if !filepath.IsAbs(dirs[i].Path) {
			t.Errorf("[%d] path not absolute: %q", i, dirs[i].Path)
		}
	}
}

func TestDiscover_HTMLOnly(t *testing.T) {
	dirs, err := Discover("testdata/html_simple")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("discovered %d threads, want 1", len(dirs))
	}
	if dirs[0].Format != "html" {
		t.Errorf("format=%q want html", dirs[0].Format)
	}
}

func TestDiscover_Both(t *testing.T) {
	dirs, err := Discover("testdata/mixed")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("discovered %d threads, want 1", len(dirs))
	}
	if dirs[0].Format != "both" {
		t.Errorf("format=%q want both", dirs[0].Format)
	}
}

func TestDiscover_AbsoluteAndRelativeInvariance(t *testing.T) {
	rel, err := Discover("testdata/json_simple")
	if err != nil {
		t.Fatalf("relative Discover: %v", err)
	}
	absRoot, err := filepath.Abs("testdata/json_simple")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := Discover(absRoot)
	if err != nil {
		t.Fatalf("absolute Discover: %v", err)
	}
	sort.Slice(rel, func(i, j int) bool { return rel[i].Path < rel[j].Path })
	sort.Slice(abs, func(i, j int) bool { return abs[i].Path < abs[j].Path })
	if !reflect.DeepEqual(rel, abs) {
		t.Errorf("relative vs absolute differ:\nrel=%+v\nabs=%+v", rel, abs)
	}
}

func TestDiscover_IgnoresHiddenAndMediaSubdirs(t *testing.T) {
	// json_with_media contains a photos/ subdir with tiny.png; it must
	// not be returned as a thread dir.
	dirs, err := Discover("testdata/json_with_media")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("discovered %d threads, want 1", len(dirs))
	}
	if dirs[0].Name != "bob_XYZ789" {
		t.Errorf("name=%q want bob_XYZ789", dirs[0].Name)
	}
	// None of the returned paths should point at photos/, videos/, etc.
	for _, d := range dirs {
		base := filepath.Base(d.Path)
		if base == "photos" || base == "videos" {
			t.Errorf("unexpected media subdir yielded: %q", d.Path)
		}
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
			tmp := t.TempDir()
			threadDir := filepath.Join(tmp, layout, "inbox", "testthread_1")
			if err := os.MkdirAll(threadDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(threadDir, "message_1.json"),
				[]byte(`{"participants":[{"name":"A"}],"messages":[]}`),
				0644,
			); err != nil {
				t.Fatal(err)
			}
			dirs, err := Discover(tmp)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(dirs) != 1 {
				t.Fatalf("discovered %d threads, want 1: %+v", len(dirs), dirs)
			}
			if dirs[0].Name != "testthread_1" {
				t.Errorf("name=%q want testthread_1", dirs[0].Name)
			}
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
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "foo_1")
	if err := os.MkdirAll(threadDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(threadDir, "message_1.html"),
		[]byte(`<html><body>hi</body></html>`),
		0644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(threadDir, "message_final.json"),
		[]byte(`{"unrelated":true}`),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	dirs, err := Discover(tmp)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("discovered %d threads, want 1: %+v", len(dirs), dirs)
	}
	if dirs[0].Format != "html" {
		t.Errorf("format=%q want html (unnumbered JSON sibling must not promote thread to json/both)", dirs[0].Format)
	}
}

func TestDiscover_IgnoresDSStore(t *testing.T) {
	// Create a temp DYI tree with a .DS_Store at the thread level; it
	// must not turn it into a thread dir.
	tmp := t.TempDir()
	threadDir := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "foo_1")
	if err := os.MkdirAll(threadDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(threadDir, "message_1.json"), []byte(`{"participants":[{"name":"A"}],"messages":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	// Add a .DS_Store sibling and a .hidden dir at section level.
	section := filepath.Dir(threadDir)
	if err := os.WriteFile(filepath.Join(section, ".DS_Store"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(section, ".hidden"), 0755); err != nil {
		t.Fatal(err)
	}

	dirs, err := Discover(tmp)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("discovered %d threads, want 1: %+v", len(dirs), dirs)
	}
	if dirs[0].Name != "foo_1" {
		t.Errorf("name=%q want foo_1", dirs[0].Name)
	}
}
