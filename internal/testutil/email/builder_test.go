package email

import (
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestPlainMessage(t *testing.T) {
	got := string(NewMessage().Body("Hello world.").Bytes())

	want := strings.Join([]string{
		"From: sender@example.com",
		"To: recipient@example.com",
		"Subject: Test Message",
		"Date: Mon, 01 Jan 2024 12:00:00 +0000",
		`Content-Type: text/plain; charset="utf-8"`,
		"",
		"Hello world.",
		"",
	}, "\n")

	assertpkg.Equal(t, want, got, "plain message mismatch")
}

func TestNoSubject(t *testing.T) {
	got := string(NewMessage().NoSubject().Bytes())
	assertpkg.NotContains(t, got, "Subject:", "expected no Subject header, but found one")
}

func TestMultipartMessage(t *testing.T) {
	got := string(NewMessage().
		Body("See attached.").
		Boundary("BOUND").
		WithAttachment("test.txt", "text/plain", []byte("file data")).
		Bytes())

	// Check structural elements are present and in order.
	checks := []string{
		"Content-Type: multipart/mixed; boundary=\"BOUND\"",
		"--BOUND\n",
		"See attached.",
		"--BOUND\n",
		`Content-Disposition: attachment; filename="test.txt"`,
		"Content-Transfer-Encoding: base64",
		"--BOUND--",
	}
	for _, c := range checks {
		assertpkg.Contains(t, got, c, "multipart message missing %q", c)
	}
}

func TestHeaderOrder(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	got := string(NewMessage().
		Header("X-First", "1").
		Header("X-Second", "2").
		Header("X-Third", "3").
		Bytes())

	i1 := strings.Index(got, "X-First: 1")
	i2 := strings.Index(got, "X-Second: 2")
	i3 := strings.Index(got, "X-Third: 3")

	require.GreaterOrEqual(i1, 0, "missing X-First header in output:\n%s", got)
	require.GreaterOrEqual(i2, 0, "missing X-Second header in output:\n%s", got)
	require.GreaterOrEqual(i3, 0, "missing X-Third header in output:\n%s", got)
	assert.Less(i1, i2, "headers not in insertion order: positions %d, %d, %d", i1, i2, i3)
	assert.Less(i2, i3, "headers not in insertion order: positions %d, %d, %d", i1, i2, i3)
}

func TestCRLF(t *testing.T) {
	got := NewMessage().CRLF().Bytes()
	for i, b := range got {
		if b == '\n' && (i == 0 || got[i-1] != '\r') {
			requirepkg.Failf(t, "bare LF found", "bare \\n at byte %d; expected all line endings to be \\r\\n", i)
		}
	}
	assertpkg.Contains(t, string(got), "\r\n", "expected at least one CRLF line ending")
}

func TestHeaderOverwrite(t *testing.T) {
	got := string(NewMessage().
		Header("X-Custom", "first").
		Header("X-Custom", "second").
		Bytes())

	assertpkg.Equal(t, 1, strings.Count(got, "X-Custom:"), "expected exactly one X-Custom header, got:\n%s", got)
	assertpkg.Contains(t, got, "X-Custom: second", "expected overwritten value 'second', got:\n%s", got)
}

func TestHeaderCaseInsensitiveOverwrite(t *testing.T) {
	got := string(NewMessage().
		Header("X-Custom", "first").
		Header("x-custom", "second").
		Bytes())

	// Case-insensitive dedup should produce exactly one header line.
	count := 0
	for line := range strings.SplitSeq(got, "\n") {
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "x-custom:") {
			count++
		}
	}
	assertpkg.Equal(t, 1, count, "expected exactly one x-custom header (case-insensitive), got %d:\n%s", count, got)
	assertpkg.Contains(t, got, "x-custom: second", "expected latest value with latest casing, got:\n%s", got)
}

func TestHeaderAppendAllowsDuplicates(t *testing.T) {
	got := string(NewMessage().
		HeaderAppend("Received", "from server1").
		HeaderAppend("Received", "from server2").
		Bytes())

	assertpkg.Equal(t, 2, strings.Count(got, "Received:"), "expected two Received headers, got:\n%s", got)
}
