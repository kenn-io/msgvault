package fbmessenger

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func threadDir(t *testing.T, root, section, name string) string {
	t.Helper()
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(abs, "your_activity_across_facebook", "messages", section, name)
}

func TestParseJSONThread_Simple(t *testing.T) {
	root := "testdata/json_simple"
	th, err := ParseJSONThread(root, threadDir(t, root, "inbox", "alice_ABC123"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if th.ConvType != "direct_chat" {
		t.Errorf("conv_type=%q want direct_chat", th.ConvType)
	}
	if len(th.Participants) != 2 {
		t.Errorf("participants=%d want 2", len(th.Participants))
	}
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
	if len(th.Messages[1].Reactions) != 2 {
		t.Errorf("reactions=%d want 2", len(th.Messages[1].Reactions))
	}
	// Index monotonic.
	for i, m := range th.Messages {
		if m.Index != i {
			t.Errorf("index[%d]=%d want %d", i, m.Index, i)
		}
	}
}

func TestParseJSONThread_Group(t *testing.T) {
	root := "testdata/json_group"
	th, err := ParseJSONThread(root, threadDir(t, root, "inbox", "crew_GRP123"))
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

func TestParseJSONThread_Multifile_NumericSort(t *testing.T) {
	root := "testdata/json_multifile"
	th, err := ParseJSONThread(root, threadDir(t, root, "inbox", "dave_MULTI"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Messages) != 4 {
		t.Fatalf("messages=%d want 4", len(th.Messages))
	}
	// Bodies, in chronological order, must be A,B,C,D.
	wantBodies := []string{
		"Message A (from file 1, oldest)",
		"Message B (from file 1, newer)",
		"Message C (from file 2)",
		"Message D (from file 10, newest)",
	}
	for i, w := range wantBodies {
		if th.Messages[i].Body != w {
			t.Errorf("messages[%d].Body=%q want %q", i, th.Messages[i].Body, w)
		}
	}
}

func TestParseJSONThread_Corrupt(t *testing.T) {
	root := "testdata/corrupt"
	_, err := ParseJSONThread(root, threadDir(t, root, "inbox", "broken_BAD"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrCorruptJSON) {
		t.Errorf("expected ErrCorruptJSON, got %v", err)
	}
}

func TestParseJSONThread_Attachments(t *testing.T) {
	root := "testdata/json_with_media"
	th, err := ParseJSONThread(root, threadDir(t, root, "inbox", "bob_XYZ789"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(th.Messages))
	}
	m := th.Messages[0]
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments=%d want 1", len(m.Attachments))
	}
	if m.Attachments[0].Kind != "photo" {
		t.Errorf("kind=%q want photo", m.Attachments[0].Kind)
	}
	if _, err := os.Stat(m.Attachments[0].AbsPath); err != nil {
		t.Errorf("attachment file should exist on disk: %v", err)
	}
	if m.Attachments[0].MimeType != "image/png" {
		t.Errorf("mime=%q want image/png", m.Attachments[0].MimeType)
	}
}

func TestParseJSONThread_Attachments_AltLayout(t *testing.T) {
	root := "testdata/json_with_media_alt"
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	td := filepath.Join(absRoot, "your_facebook_activity", "messages", "inbox", "carol_ALT456")
	th, err := ParseJSONThread(root, td)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(th.Messages))
	}
	m := th.Messages[0]
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments=%d want 1", len(m.Attachments))
	}
	if m.Attachments[0].Kind != "photo" {
		t.Errorf("kind=%q want photo", m.Attachments[0].Kind)
	}
	if _, err := os.Stat(m.Attachments[0].AbsPath); err != nil {
		t.Errorf("attachment file should exist on disk: %v", err)
	}
}

func TestParseJSONThread_NonTextBodies(t *testing.T) {
	root := "testdata/json_nontext"
	th, err := ParseJSONThread(root, threadDir(t, root, "inbox", "sam_NONTXT"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Ordered chronologically ascending: unsubscribe, share, missed call, call, photo, sticker.
	wantBodies := []string{
		"[system] Sam left the chat",
		"[shared link] https://example.com/article\nExample share text",
		"[call: missed, 0s]",
		"[call: 3m 12s]",
		"[photo]",
		"[sticker]",
	}
	if len(th.Messages) != len(wantBodies) {
		t.Fatalf("messages=%d want %d", len(th.Messages), len(wantBodies))
	}
	for i, w := range wantBodies {
		if th.Messages[i].Body != w {
			t.Errorf("messages[%d].Body=%q want %q", i, th.Messages[i].Body, w)
		}
	}
}

func TestParseJSONThread_PathEscapeRejected(t *testing.T) {
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "evil_ESC")
	if err := os.MkdirAll(threadPath, 0755); err != nil {
		t.Fatal(err)
	}
	body := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","photos":[{"uri":"../../etc/passwd"}]}
],"title":"x"}`
	if err := os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	th, err := ParseJSONThread(tmp, threadPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(th.Messages))
	}
	att := th.Messages[0].Attachments
	if len(att) != 1 {
		t.Fatalf("attachments=%d want 1", len(att))
	}
	if att[0].AbsPath != "" {
		t.Errorf("path escape not rejected: AbsPath=%q", att[0].AbsPath)
	}
}

// When a thread dir has no valid numbered message files at all, the
// parser returns an error because there is nothing to import.
func TestParseJSONThread_OnlyUnnumberedFiles(t *testing.T) {
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "bad_NAME")
	if err := os.MkdirAll(threadPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(threadPath, "message_final.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ParseJSONThread(tmp, threadPath)
	if err == nil {
		t.Fatal("expected error when no valid numbered files present")
	}
}

// When a thread dir contains BOTH valid numbered files and a sibling
// whose name doesn't match the `^message_(\d+)\.json$` pattern, the
// parser must import the valid file(s) and report the bad sibling via
// Thread.BadSiblings rather than aborting the entire thread.
func TestParseJSONThread_SkipsUnnumberedSibling(t *testing.T) {
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "mix_MIXED")
	if err := os.MkdirAll(threadPath, 0755); err != nil {
		t.Fatal(err)
	}
	good := `{"participants":[{"name":"A"},{"name":"B"}],"messages":[
{"sender_name":"A","timestamp_ms":1600000000000,"type":"Generic","content":"hi from A"}
],"title":"mix"}`
	if err := os.WriteFile(filepath.Join(threadPath, "message_1.json"), []byte(good), 0644); err != nil {
		t.Fatal(err)
	}
	// Facebook sometimes writes a human-named sibling; content doesn't
	// matter because we skip it by name before attempting to parse it.
	if err := os.WriteFile(filepath.Join(threadPath, "message_final.json"), []byte(`not even valid json`), 0644); err != nil {
		t.Fatal(err)
	}
	th, err := ParseJSONThread(tmp, threadPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(th.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(th.Messages))
	}
	if th.Messages[0].Body != "hi from A" {
		t.Errorf("body=%q want %q", th.Messages[0].Body, "hi from A")
	}
	if len(th.BadSiblings) != 1 || th.BadSiblings[0] != "message_final.json" {
		t.Errorf("BadSiblings=%v want [message_final.json]", th.BadSiblings)
	}
}
