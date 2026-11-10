package xarchive

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReaderRejectsMalformedAndUnboundedInput(t *testing.T) {
	for _, body := range []string{
		`window.YTD.direct_messages_group.part0 = []`,
		`window.YTD.direct_messages.part0 = []; alert(1)`,
		`[{"dmConversation":{}}]`,
		`window.YTD.direct_messages.part0 = [{"dmConversation":{"messages":[],"conversationId":"1"}}]`,
		`window.YTD.direct_messages.part0 = [{"dmConversation":{"conversationId":"1","messages":[{"messageCreate":{"text":"` + strings.Repeat("x", maxEventBytes) + `"}}]}}]`,
	} {
		t.Run(body[:min(len(body), 60)], func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "direct-messages.js"), []byte(body), 0600))
			r, err := openArchive(root)
			require.NoError(t, err)
			defer r.Close()
			require.Error(t, r.events(t.Context(), func(record) error { return nil }))
		})
	}
}

func TestReaderStreamsLongConversationAndParts(t *testing.T) {
	root := t.TempDir()
	event := `{"messageCreate":{"id":"1","text":"` + strings.Repeat("x", 8192) + `"}}`
	synthetic(t, root, "direct-messages-part1.js", `[{"dmConversation":{"conversationId":"1-2","messages":[`+strings.TrimSuffix(strings.Repeat(event+",", 300), ",")+`]}}]`)
	r, err := openArchive(root)
	require.NoError(t, err)
	defer r.Close()
	n := 0
	require.NoError(t, r.events(t.Context(), func(rec record) error {
		if rec.Raw == nil {
			return nil
		}
		n++
		assert.Equal(t, "direct:1-2", rec.Peer)
		return nil
	}))
	assert.Equal(t, 300, n)
}

// A bound must stop the underlying reader before JSON allocates a huge value,
// including decoder read-ahead; checking len(raw) afterward would fail this.
func TestReaderEnforcesAbsoluteReadCeiling(t *testing.T) {
	source := &countedReader{Reader: io.MultiReader(strings.NewReader(`[{"dmConversation":{"conversationId":"1","messages":[{"messageCreate":{"text":"`), io.LimitReader(repeatReader{}, int64(maxEventBytes)*4))}
	err := readEvents(context.WithoutCancel(t.Context()), source, "direct-messages.js", false, func(record) error { return nil })
	require.Error(t, err)
	assert.LessOrEqual(t, source.n, int64(maxEventBytes+512))
}

type countedReader struct {
	io.Reader
	n int64
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	r.n += int64(n)
	return n, e
}

type repeatReader struct{}

func (repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestReaderRejectsNonregularAndEscapingSources(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "direct-messages.js")
			if kind == "directory" {
				require.NoError(t, os.Mkdir(file, 0700))
			} else {
				outside := filepath.Join(t.TempDir(), "outside.js")
				require.NoError(t, os.WriteFile(outside, []byte("window.YTD.direct_messages.part0 = []"), 0600))
				require.NoError(t, os.Symlink(outside, file))
			}
			r, err := openArchive(root)
			require.NoError(t, err)
			defer r.Close()
			require.Error(t, r.events(t.Context(), func(record) error { return nil }))
		})
	}
}
