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
		`window.YTD.direct_messages.part0 = [{}]`,
		`window.YTD.direct_messages.part0 = [{"dmConversation":{}}]`,
		`window.YTD.direct_messages.part0 = [{"dmConversation":{"conversationId":"1"}}]`,
	} {
		t.Run(body[:min(len(body), 60)], func(t *testing.T) {
			require := require.New(t)
			root := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(root, "direct-messages.js"), []byte(body), 0600))
			r, err := openArchive(root)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(r.Close()) })
			require.Error(r.events(t.Context(), func(record) error { return nil }))
		})
	}
}

func TestReaderStreamsLongConversationAndParts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	event := `{"messageCreate":{"id":"1","text":"` + strings.Repeat("x", 8192) + `"}}`
	// Create names in reverse lexical order.
	synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"1-2","messages":[`+event+`]}}]`)
	require.NoError(os.WriteFile(filepath.Join(root, "direct-messages-part1.js"), []byte(`window.YTD.direct_messages.part1 = [{"dmConversation":{"conversationId":"1-2","messages":[`+strings.TrimSuffix(strings.Repeat(event+",", 300), ",")+`]}}]`), 0600))
	r, err := openArchive(root)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(r.Close()) })
	n := 0
	var filenames []string
	require.NoError(r.events(t.Context(), func(rec record) error {
		if rec.Raw == nil {
			filenames = append(filenames, rec.Filename)
			return nil
		}
		n++
		assert.Equal("direct:1-2", rec.Peer)
		return nil
	}))
	assert.Equal(301, n)
	assert.Equal([]string{"direct-messages-part1.js", "direct-messages.js"}, filenames)
}

// A bound must stop the underlying reader before JSON allocates a huge value,
// including decoder read-ahead; checking len(raw) afterward would fail this.
func TestReaderEnforcesAbsoluteReadCeiling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := &countedReader{Reader: io.MultiReader(strings.NewReader(`[{"dmConversation":{"conversationId":"1","messages":[{"messageCreate":{"text":"`), io.LimitReader(repeatReader{}, int64(maxEventBytes)*4))}
	err := readEvents(context.WithoutCancel(t.Context()), source, "direct-messages.js", false, func(record) error { return nil })
	require.Error(err)
	assert.LessOrEqual(source.n, int64(maxEventBytes+512))
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
			require := require.New(t)
			root := t.TempDir()
			file := filepath.Join(root, "direct-messages.js")
			if kind == "directory" {
				require.NoError(os.Mkdir(file, 0700))
			} else {
				outside := filepath.Join(t.TempDir(), "outside.js")
				require.NoError(os.WriteFile(outside, []byte("window.YTD.direct_messages.part0 = []"), 0600))
				require.NoError(os.Symlink(outside, file))
			}
			r, err := openArchive(root)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(r.Close()) })
			require.Error(r.events(t.Context(), func(record) error { return nil }))
		})
	}
}
