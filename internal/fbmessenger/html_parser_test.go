package fbmessenger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHTMLThread_Simple(t *testing.T) {
	root := "testdata/html_simple"
	th, err := ParseHTMLThread(root, threadDir(t, root, "inbox", "alice_ABC123"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Participants) != 2 {
		t.Errorf("participants=%d want 2", len(th.Participants))
	}
	if th.ConvType != "direct_chat" {
		t.Errorf("conv_type=%q want direct_chat", th.ConvType)
	}
	if len(th.Messages) != 3 {
		t.Fatalf("messages=%d want 3", len(th.Messages))
	}
	// HTML exports do not expose reaction metadata, so the HTML parser
	// must not fabricate a "[reacted: ...]" suffix. Reaction coverage
	// lives in the JSON parser tests + TestImportDYI_ReactionsDualPath.
	wantBodies := []string{
		"Hello",
		"café time?",
		"See you soon",
	}
	for i, w := range wantBodies {
		if th.Messages[i].Body != w {
			t.Errorf("messages[%d].Body=%q want %q", i, th.Messages[i].Body, w)
		}
	}
	if th.Title != "Alice Example" {
		t.Errorf("title=%q want Alice Example", th.Title)
	}
}

func TestParseHTMLThread_WithMedia(t *testing.T) {
	root := "testdata/html_with_media"
	th, err := ParseHTMLThread(root, threadDir(t, root, "inbox", "bob_XYZ789"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Messages) != 1 {
		t.Fatalf("messages=%d want 1: %+v", len(th.Messages), th.Messages)
	}
	m := th.Messages[0]
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments=%d want 1", len(m.Attachments))
	}
	if _, err := os.Stat(m.Attachments[0].AbsPath); err != nil {
		t.Errorf("attachment should exist on disk: %v", err)
	}
}

func TestParseHTMLThread_TimestampLayouts(t *testing.T) {
	want := time.Date(2019, 10, 19, 14, 37, 0, 0, time.UTC)
	for _, name := range []string{"layout1.html", "layout2.html", "layout3.html"} {
		data, err := os.ReadFile(filepath.Join("testdata/html_timestamps", name))
		if err != nil {
			t.Fatal(err)
		}
		// Use parseHTMLLines indirectly through the main parse path by
		// writing into a temp thread dir and calling ParseHTMLThread.
		tmp := t.TempDir()
		threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "ts_TEST")
		if err := os.MkdirAll(threadPath, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(threadPath, "message_1.html"), data, 0644); err != nil {
			t.Fatal(err)
		}
		th, err := ParseHTMLThread(tmp, threadPath)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if len(th.Messages) != 1 {
			t.Fatalf("%s: messages=%d want 1", name, len(th.Messages))
		}
		if !th.Messages[0].SentAt.Equal(want) {
			t.Errorf("%s: SentAt=%v want %v", name, th.Messages[0].SentAt, want)
		}
		if th.Messages[0].SentAt.Location() != time.UTC {
			t.Errorf("%s: location=%v want UTC", name, th.Messages[0].SentAt.Location())
		}
	}
}

// TestParseHTMLThread_ImagePositioning verifies that images are attached to
// the message block where they appear in the DOM, not to the first empty or
// attachment-less message.
func TestParseHTMLThread_ImagePositioning(t *testing.T) {
	root := "testdata/html_multi_media"
	th, err := ParseHTMLThread(root, threadDir(t, root, "inbox", "carol_IMG456"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Messages) != 3 {
		t.Fatalf("messages=%d want 3", len(th.Messages))
	}
	// Message 0: "Hello Carol" — no image, no attachments.
	if len(th.Messages[0].Attachments) != 0 {
		t.Errorf("messages[0].Attachments=%d want 0 (image should NOT land here)", len(th.Messages[0].Attachments))
	}
	// Message 1: "Check out this photo" — the image belongs here.
	if len(th.Messages[1].Attachments) != 1 {
		t.Errorf("messages[1].Attachments=%d want 1", len(th.Messages[1].Attachments))
	}
	// Message 2: "Nice picture" — no attachments.
	if len(th.Messages[2].Attachments) != 0 {
		t.Errorf("messages[2].Attachments=%d want 0", len(th.Messages[2].Attachments))
	}
}

func TestParseHTMLThread_StructuralParsing(t *testing.T) {
	// Replace known class names with random strings; the parser must
	// still find participants, bodies, and timestamps.
	data, err := os.ReadFile("testdata/html_simple/your_activity_across_facebook/messages/inbox/alice_ABC123/message_1.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, cls := range []string{"_a706", "_a70e", "_3b0d", "_a6-g", "_a6-p", "_2ph_", "_a6-h", "_a6-i", "_a72d"} {
		body = strings.ReplaceAll(body, cls, "zzq"+cls[1:])
	}
	tmp := t.TempDir()
	threadPath := filepath.Join(tmp, "your_activity_across_facebook", "messages", "inbox", "alice_ABC123")
	if err := os.MkdirAll(threadPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(threadPath, "message_1.html"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	th, err := ParseHTMLThread(tmp, threadPath)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(th.Messages) != 3 {
		t.Fatalf("messages=%d want 3", len(th.Messages))
	}
	if len(th.Participants) != 2 {
		t.Errorf("participants=%d want 2", len(th.Participants))
	}
}
