package mbox

import (
	"io"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestReader_Next_SplitsAndUnescapes(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00:00 2024",
		"Subject: One",
		"",
		">From should-unescape",
		">>From keep-one",
		"Normal",
		"",
		"From sender@example.com Mon Jan 1 00:00:01 2024",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")

	r := NewReader(strings.NewReader(mboxData))

	msg1, err := r.Next()
	require.NoError(err, "Next()")
	assert.True(strings.HasPrefix(msg1.FromLine, "From sender@example.com"), "FromLine mismatch: %q", msg1.FromLine)
	raw1 := string(msg1.Raw)
	assert.Contains(raw1, "From should-unescape\n", "expected unescaped From line")
	assert.Contains(raw1, ">From keep-one\n", "expected unescaped >>From -> >From")
	assert.NotContains(raw1, ">>From keep-one\n", "expected mboxrd unescape to remove one '>'")

	msg2, err := r.Next()
	require.NoError(err, "Next() (msg2)")
	raw2 := string(msg2.Raw)
	assert.Contains(raw2, "Subject: Two\n", "msg2 raw")
	assert.Contains(raw2, "\n\nBody2\n", "msg2 raw")

	_, err = r.Next()
	require.ErrorIs(err, io.EOF, "expected EOF")
}

func TestReader_Next_CanDisableUnescape(t *testing.T) {
	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00:00 2024",
		"Subject: One",
		"",
		">From should-stay-escaped",
		"",
	}, "\n")

	r := NewReader(strings.NewReader(mboxData))
	r.SetUnescapeFrom(false)

	msg, err := r.Next()
	requirepkg.NoError(t, err, "Next()")
	raw := string(msg.Raw)
	assertpkg.Contains(t, raw, ">From should-stay-escaped\n", "expected no unescaping")
	assertpkg.NotContains(t, raw, "\n\nFrom should-stay-escaped\n", "expected >From line to remain escaped")
}

func TestReader_Next_AllowsLongLines(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	longValue := strings.Repeat("a", 10_000)
	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00:00 2024",
		"Subject: One",
		"X-Long: " + longValue,
		"",
		"Body1",
		"",
		"From sender@example.com Mon Jan 1 00:00:01 2024",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")

	r := NewReader(strings.NewReader(mboxData))

	msg1, err := r.Next()
	require.NoError(err, "Next() (msg1)")
	assert.Contains(string(msg1.Raw), "X-Long: "+longValue+"\n", "expected full long header line")

	msg2, err := r.Next()
	require.NoError(err, "Next() (msg2)")
	assert.Contains(string(msg2.Raw), "Subject: Two\n", "msg2 raw")

	_, err = r.Next()
	require.ErrorIs(err, io.EOF, "expected EOF")
}

func TestReader_Next_EnforcesMaxMessageBytesAndContinues(t *testing.T) {
	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00:00 2024",
		"Subject: " + strings.Repeat("a", 200),
		"",
		"Body1",
		"",
		"From sender@example.com Mon Jan 1 00:00:01 2024",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")

	r := NewReaderWithMaxMessageBytes(strings.NewReader(mboxData), 64)

	_, err := r.Next()
	requirepkg.ErrorIs(t, err, ErrMessageTooLarge, "expected ErrMessageTooLarge")

	msg2, err := r.Next()
	requirepkg.NoError(t, err, "Next() (msg2)")
	assertpkg.Contains(t, string(msg2.Raw), "Subject: Two\n", "msg2 raw")
}

func TestReader_Next_DoesNotSplitOnUnescapedFromInBody(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00:00 2024",
		"Subject: One",
		"",
		"Body1",
		"From this is not a separator",
		"Body3",
		"",
		"From sender@example.com Mon Jan 1 00:00:01 2024",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")

	r := NewReader(strings.NewReader(mboxData))

	msg1, err := r.Next()
	require.NoError(err, "Next() (msg1)")
	assert.Contains(string(msg1.Raw), "From this is not a separator\n", "expected body to contain unescaped From line")

	msg2, err := r.Next()
	require.NoError(err, "Next() (msg2)")
	assert.Contains(string(msg2.Raw), "Subject: Two\n", "msg2 raw")

	_, err = r.Next()
	require.ErrorIs(err, io.EOF, "expected EOF")
}

func TestReader_Next_AcceptsNamedTimezoneSeparators(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00:00 MST 2024",
		"Subject: One",
		"",
		"Body1",
		"",
		"From sender@example.com Mon Jan 1 00:00:01 MST 2024",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")

	r := NewReader(strings.NewReader(mboxData))

	msg1, err := r.Next()
	require.NoError(err, "Next() (msg1)")
	assert.Contains(string(msg1.Raw), "Subject: One\n", "msg1 raw")

	msg2, err := r.Next()
	require.NoError(err, "Next() (msg2)")
	assert.Contains(string(msg2.Raw), "Subject: Two\n", "msg2 raw")

	_, err = r.Next()
	require.ErrorIs(err, io.EOF, "expected EOF")
}

func TestReader_Next_AcceptsRemoteFromSuffixSeparators(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00:00 2024 remote from mail.example.com",
		"Subject: One",
		"",
		"Body1",
		"",
		"From sender@example.com Mon Jan 1 00:00:01 2024 remote from mail.example.com",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")

	r := NewReader(strings.NewReader(mboxData))

	msg1, err := r.Next()
	require.NoError(err, "Next() (msg1)")
	assert.Contains(string(msg1.Raw), "Subject: One\n", "msg1 raw")

	msg2, err := r.Next()
	require.NoError(err, "Next() (msg2)")
	assert.Contains(string(msg2.Raw), "Subject: Two\n", "msg2 raw")

	_, err = r.Next()
	require.ErrorIs(err, io.EOF, "expected EOF")
}

func TestReader_Next_AcceptsNoSecondsSeparators(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	mboxData := strings.Join([]string{
		"From sender@example.com Mon Jan 1 00:00 2024",
		"Subject: One",
		"",
		"Body1",
		"",
		"From sender@example.com Mon Jan 1 00:01 2024",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")

	r := NewReader(strings.NewReader(mboxData))

	msg1, err := r.Next()
	require.NoError(err, "Next() (msg1)")
	assert.Contains(string(msg1.Raw), "Subject: One\n", "msg1 raw")

	msg2, err := r.Next()
	require.NoError(err, "Next() (msg2)")
	assert.Contains(string(msg2.Raw), "Subject: Two\n", "msg2 raw")

	_, err = r.Next()
	require.ErrorIs(err, io.EOF, "expected EOF")
}

func TestReader_Offset_RespectsSeekPosition(t *testing.T) {
	require := requirepkg.New(t)
	mboxData := strings.Join([]string{
		"From a@example.com Mon Jan 1 00:00:00 2024",
		"Subject: One",
		"",
		"Body1",
		"",
		"From b@example.com Mon Jan 1 00:00:01 2024",
		"Subject: Two",
		"",
		"Body2",
		"",
	}, "\n")

	start := strings.Index(mboxData, "From b@example.com")
	require.GreaterOrEqual(start, 0, "missing second From line")

	sr := strings.NewReader(mboxData)
	_, err := sr.Seek(int64(start), io.SeekStart)
	require.NoError(err, "Seek()")

	r := NewReader(sr)
	require.Equal(int64(start), r.Offset())

	msg, err := r.Next()
	require.NoError(err, "Next()")
	require.True(strings.HasPrefix(msg.FromLine, "From b@example.com"), "unexpected FromLine: %q", msg.FromLine)
}

func TestValidate_FindsSeparator(t *testing.T) {
	data := "not mbox\nFrom a@b Sat Jan 1 00:00:00 2024\nSubject: x\n\nBody\n"
	requirepkg.NoError(t, Validate(strings.NewReader(data), 1024), "Validate()")
}

func TestValidate_FindsSeparator_WithRemoteFromSuffix(t *testing.T) {
	data := "not mbox\nFrom a@b Sat Jan 1 00:00:00 2024 remote from mail.example.com\nSubject: x\n\nBody\n"
	requirepkg.NoError(t, Validate(strings.NewReader(data), 1024), "Validate()")
}
