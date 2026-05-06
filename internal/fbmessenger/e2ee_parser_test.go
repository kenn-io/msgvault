package fbmessenger

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseE2EEJSONFile_Simple(t *testing.T) {
	root := "testdata/e2ee_simple"
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(absRoot, "your_activity_across_facebook", "messages", "alice_1.json")

	th, err := ParseE2EEJSONFile(absRoot, filePath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if th.ConvType != "direct_chat" {
		t.Errorf("conv_type=%q want direct_chat", th.ConvType)
	}
	if len(th.Participants) != 2 {
		t.Errorf("participants=%d want 2", len(th.Participants))
	}
	if th.Format != "e2ee_json" {
		t.Errorf("format=%q want e2ee_json", th.Format)
	}
	if th.DirName != "alice_1" {
		t.Errorf("dir_name=%q want alice_1", th.DirName)
	}
	// Unsent message should be filtered out.
	if len(th.Messages) != 3 {
		t.Fatalf("messages=%d want 3", len(th.Messages))
	}
	// Messages must be chronological ascending.
	for i := 1; i < len(th.Messages); i++ {
		if th.Messages[i-1].SentAt.After(th.Messages[i].SentAt) {
			t.Errorf("messages out of order at %d", i)
		}
	}
	// Mojibake repair: message 1 body must contain "café".
	if !strings.Contains(th.Messages[1].Body, "café") {
		t.Errorf("mojibake not repaired: body=%q", th.Messages[1].Body)
	}
	// Reactions appended to body.
	if !strings.Contains(th.Messages[1].Body, "[reacted:") {
		t.Errorf("reactions not appended: body=%q", th.Messages[1].Body)
	}
	if len(th.Messages[1].Reactions) != 1 {
		t.Errorf("reactions=%d want 1", len(th.Messages[1].Reactions))
	}
	// Index monotonic.
	for i, m := range th.Messages {
		if m.Index != i {
			t.Errorf("index[%d]=%d want %d", i, m.Index, i)
		}
	}
}

func TestParseE2EEJSONFile_Group(t *testing.T) {
	root := "testdata/e2ee_simple"
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(absRoot, "your_activity_across_facebook", "messages", "group_2.json")

	th, err := ParseE2EEJSONFile(absRoot, filePath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if th.ConvType != "group_chat" {
		t.Errorf("conv_type=%q want group_chat", th.ConvType)
	}
	if len(th.Participants) != 3 {
		t.Errorf("participants=%d want 3", len(th.Participants))
	}
}

func TestParseE2EEJSONFile_MediaResolution(t *testing.T) {
	root := "testdata/e2ee_simple"
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(absRoot, "your_activity_across_facebook", "messages", "group_2.json")

	th, err := ParseE2EEJSONFile(absRoot, filePath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Messages) != 2 {
		t.Fatalf("messages=%d want 2", len(th.Messages))
	}
	// Second message has a media attachment.
	m := th.Messages[1]
	if m.Body != "[media]" {
		t.Errorf("body=%q want [media]", m.Body)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments=%d want 1", len(m.Attachments))
	}
	att := m.Attachments[0]
	if att.Kind != "photo" {
		t.Errorf("kind=%q want photo", att.Kind)
	}
	if att.Filename != "photo.jpg" {
		t.Errorf("filename=%q want photo.jpg", att.Filename)
	}
	if _, err := os.Stat(att.AbsPath); err != nil {
		t.Errorf("attachment file should exist on disk: %v", err)
	}
}

func TestParseE2EEJSONFile_NotAThread(t *testing.T) {
	tmp := t.TempDir()
	cases := map[string]string{
		"array.json":   `[{"any": "list"}]`,
		"scalar.json":  `"a string"`,
		"no_keys.json": `{"setting": true, "version": 2}`,
	}
	for name, body := range cases {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ParseE2EEJSONFile(tmp, p)
		if !errors.Is(err, ErrNotE2EEThread) {
			t.Errorf("%s: expected ErrNotE2EEThread, got %v", name, err)
		}
	}
}

// TestParseE2EEJSONFile_PartialObjectCorrupt verifies that an object
// with exactly one of "participants"/"messages" is classified as corrupt
// rather than silently skipped — a partial export with missing
// messages must not vanish silently.
func TestParseE2EEJSONFile_PartialObjectCorrupt(t *testing.T) {
	tmp := t.TempDir()
	cases := map[string]string{
		"only_p.json":   `{"participants": ["A", "B"]}`,
		"only_msg.json": `{"messages": [{"senderName":"A","text":"x","timestamp":1}]}`,
	}
	for name, body := range cases {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ParseE2EEJSONFile(tmp, p)
		if !errors.Is(err, ErrCorruptJSON) {
			t.Errorf("%s: expected ErrCorruptJSON, got %v", name, err)
		}
	}
}

func TestParseE2EEJSONFile_CorruptJSON(t *testing.T) {
	tmp := t.TempDir()
	badFile := filepath.Join(tmp, "bad.json")
	if err := os.WriteFile(badFile, []byte(`{not valid json`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ParseE2EEJSONFile(tmp, badFile)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrCorruptJSON) {
		t.Errorf("expected ErrCorruptJSON, got %v", err)
	}
}

func TestParseE2EEJSONFile_PathEscapeRejected(t *testing.T) {
	tmp := t.TempDir()
	body := `{
		"participants": ["A", "B"],
		"threadName": "test",
		"messages": [{
			"senderName": "A",
			"text": "",
			"timestamp": 1600000000000,
			"type": "Generic",
			"media": [{"uri": "../../etc/passwd"}]
		}]
	}`
	filePath := filepath.Join(tmp, "evil.json")
	if err := os.WriteFile(filePath, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	th, err := ParseE2EEJSONFile(tmp, filePath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(th.Messages))
	}
	// Path escape should be rejected — no attachments resolved.
	if len(th.Messages[0].Attachments) != 0 {
		t.Errorf("path escape not rejected: attachments=%+v", th.Messages[0].Attachments)
	}
}

func TestDiscover_E2EEFlat(t *testing.T) {
	dirs, err := Discover("testdata/e2ee_simple")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// e2ee_simple has two JSON files at the messages root: alice_1.json
	// and group_2.json. Both should be discovered as e2ee_json threads.
	if len(dirs) != 2 {
		t.Fatalf("discovered %d threads, want 2: %+v", len(dirs), dirs)
	}
	for _, d := range dirs {
		if d.Format != "e2ee_json" {
			t.Errorf("format=%q want e2ee_json for %q", d.Format, d.Name)
		}
		if d.Section != "e2ee_cutover" {
			t.Errorf("section=%q want e2ee_cutover for %q", d.Section, d.Name)
		}
		if d.FilePath == "" {
			t.Errorf("FilePath should be set for E2EE thread %q", d.Name)
		}
		if !filepath.IsAbs(d.Path) {
			t.Errorf("path not absolute: %q", d.Path)
		}
	}
	// Sorted by name.
	if dirs[0].Name != "alice_1" {
		t.Errorf("dirs[0].Name=%q want alice_1", dirs[0].Name)
	}
	if dirs[1].Name != "group_2" {
		t.Errorf("dirs[1].Name=%q want group_2", dirs[1].Name)
	}
}

// TestDiscover_E2EEFlatRejectsNonThreadJSON verifies that a directory
// containing both real thread files and unknown non-thread JSON blobs
// (e.g. a new DYI metadata file Facebook may add) discovers only the
// thread files. Keeping the indexed list stable across runs is required
// for checkpoint-by-thread-index resume.
func TestDiscover_E2EEFlatRejectsNonThreadJSON(t *testing.T) {
	tmp := t.TempDir()
	thread := `{"participants":["A","B"],"threadName":"t","messages":[]}`
	if err := os.WriteFile(filepath.Join(tmp, "real_1.json"), []byte(thread), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "metadata.json"), []byte(`{"setting":true,"version":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "list.json"), []byte(`[1,2,3]`), 0o644); err != nil {
		t.Fatal(err)
	}

	dirs, err := Discover(tmp)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("discovered %d files, want 1: %+v", len(dirs), dirs)
	}
	if dirs[0].Name != "real_1" {
		t.Errorf("dirs[0].Name=%q want real_1", dirs[0].Name)
	}
}
