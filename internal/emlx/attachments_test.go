package emlx

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

// writePartial writes Messages/<num>.partial.emlx with the given MIME body
// and, for each entry in attachments, Attachments/<num>/<partID>/<name>.
// It returns the path of the .partial.emlx file.
func writePartial(t *testing.T, root string, num int, mime string, attachments map[string][]byte) string {
	t.Helper()
	msgDir := filepath.Join(root, "Messages")
	require.NoError(t, os.MkdirAll(msgDir, 0o755))
	path := filepath.Join(msgDir, fmt.Sprintf("%d.partial.emlx", num))
	data := fmt.Sprintf("%d\n%s", len(mime), mime)
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	for rel, content := range attachments {
		p := filepath.Join(root, "Attachments", strconv.Itoa(num), filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, content, 0o600))
	}
	return path
}

// unwrapped returns raw with all line breaks removed, so a base64 payload
// that Raw wraps at 76 characters can be compared against EncodeToString.
func unwrapped(raw []byte) string {
	return strings.NewReplacer("\r\n", "", "\n", "").Replace(string(raw))
}

// placeholderMIME builds a two-part multipart/mixed message the way Apple Mail
// writes a .partial.emlx: a text body followed by an attachment part whose
// content is replaced by an X-Apple-Content-Length header and an empty body.
func placeholderMIME(nl, boundary, filename string, contentLength int) string {
	lines := []string{
		"From: alice@example.com",
		"Subject: Invoice",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="` + boundary + `"`,
		"",
		"--" + boundary,
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Please find the invoice attached.",
		"",
		"--" + boundary,
		"Content-Transfer-Encoding: base64",
		"Content-Disposition: attachment;",
		"\tfilename=\"" + filename + "\"",
		"Content-Type: application/pdf;",
		"\tname=\"" + filename + "\"",
		fmt.Sprintf("X-Apple-Content-Length: %d", contentLength),
		"",
		"",
		"--" + boundary + "--",
		"",
	}
	return strings.Join(lines, nl)
}

func TestParseFile_PartialRestoresAttachmentFromSiblingDir(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	pdf := []byte("%PDF-1.6\n%fake pdf content for testing\n")
	mime := placeholderMIME("\n", "=-boundary42", "report.pdf", 60)
	path := writePartial(t, t.TempDir(), 42, mime, map[string][]byte{
		"2/report.pdf": pdf,
	})

	msg, err := ParseFile(path, 1<<20)
	require.NoError(err)

	raw := string(msg.Raw)
	assert.NotContains(raw, "X-Apple-Content-Length", "placeholder header must be removed")
	assert.Contains(raw, base64.StdEncoding.EncodeToString(pdf), "attachment bytes must be inlined as base64")
	assert.Contains(raw, "Content-Transfer-Encoding: base64")
	assert.Contains(raw, "Please find the invoice attached.", "text part must be untouched")
	assert.Equal(1, msg.RestoredAttachments)
}

func TestParseFile_RestoredAttachmentEncoding(t *testing.T) {
	for _, encoding := range []string{"base64", "quoted-printable", "7bit", ""} {
		t.Run(encoding, func(t *testing.T) {
			require := require.New(t)
			content := []byte("Meeting notes: bring a pen.\n")
			raw := placeholderMIME("\n", "boundary", "notes.txt", len(content))
			header := ""
			if encoding != "" {
				header = "content-transfer-encoding:\n\t" + encoding + "\n"
			}
			raw = strings.Replace(raw, "Content-Transfer-Encoding: base64\n", header, 1)
			raw = strings.Replace(raw, "application/pdf", "text/plain", 1)
			path := writePartial(t, t.TempDir(), 3, raw, map[string][]byte{"2/notes.txt": content})
			msg, err := ParseFile(path, 1<<20)
			require.NoError(err)
			parsed, err := mime.Parse(msg.Raw)
			require.NoError(err)
			require.Len(parsed.Attachments, 1)
			assert.Equal(t, content, parsed.Attachments[0].Content)
		})
	}
}

func TestParseFile_PartialWithoutAttachmentsDirIsUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mime := placeholderMIME("\n", "=-b", "report.pdf", 60)
	path := writePartial(t, t.TempDir(), 7, mime, nil)

	msg, err := ParseFile(path, 1<<20)
	require.NoError(err)
	assert.Equal(mime, string(msg.Raw), "no Attachments/ dir: bytes must be untouched")
	assert.Equal(0, msg.RestoredAttachments)
}

func TestParseFile_PartialMissingFileKeepsPlaceholder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mime := placeholderMIME("\n", "=-b", "report.pdf", 60)
	// Attachments/7 exists but holds a file for a different part.
	path := writePartial(t, t.TempDir(), 7, mime, map[string][]byte{
		"3/other.bin": []byte("x"),
	})

	msg, err := ParseFile(path, 1<<20)
	require.NoError(err)
	assert.Equal(mime, string(msg.Raw), "missing file: part must stay a placeholder")
	assert.Equal(0, msg.RestoredAttachments)
}

func TestParseFile_FullEmlxNextToAttachmentsDirIsNotTouched(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := t.TempDir()
	mime := placeholderMIME("\n", "=-b", "report.pdf", 60)
	writePartial(t, root, 7, mime, map[string][]byte{"2/report.pdf": []byte("pdf")})
	// A full .emlx with the same number must never be rewritten.
	full := filepath.Join(root, "Messages", "7.emlx")
	require.NoError(os.WriteFile(full, []byte(fmt.Sprintf("%d\n%s", len(mime), mime)), 0o600))

	msg, err := ParseFile(full, 1<<20)
	require.NoError(err)
	assert.Equal(mime, string(msg.Raw))
	assert.Equal(0, msg.RestoredAttachments)
}

func TestParseFile_PartIndexCountsTopLevelChildrenNotLeaves(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// mixed -> [ alternative(text/plain, text/html), application/pdf ]
	// Apple names the pdf's directory "2" (second child of the top-level
	// multipart), not "3" (third leaf).
	nl := "\n"
	lines := []string{
		"From: alice@example.com",
		"Subject: Invoice",
		`Content-Type: multipart/mixed; boundary="outer"`,
		"",
		"--outer",
		`Content-Type: multipart/alternative; boundary="inner"`,
		"",
		"--inner",
		"Content-Type: text/plain",
		"",
		"plain body",
		"--inner",
		"Content-Type: text/html",
		"",
		"<p>html body</p>",
		"--inner--",
		"--outer",
		"Content-Transfer-Encoding: base64",
		"Content-Disposition: attachment;",
		"\tfilename=\"invoice.pdf\"",
		"Content-Type: application/pdf",
		"X-Apple-Content-Length: 12",
		"",
		"",
		"--outer--",
		"",
	}
	mime := strings.Join(lines, nl)
	pdf := []byte("%PDF-nested")
	path := writePartial(t, t.TempDir(), 9, mime, map[string][]byte{
		"2/invoice.pdf": pdf,
	})

	msg, err := ParseFile(path, 1<<20)
	require.NoError(err)
	raw := string(msg.Raw)
	assert.Equal(1, msg.RestoredAttachments)
	assert.Contains(raw, base64.StdEncoding.EncodeToString(pdf))
	assert.Contains(raw, "<p>html body</p>", "nested parts must be untouched")
	assert.Contains(raw, "--inner--", "inner boundary must survive")
}

func TestParseFile_RestoresTwoAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	nl := "\n"
	part := func(name string, n int) []string {
		return []string{
			"--b",
			"Content-Transfer-Encoding: base64",
			"Content-Disposition: attachment;",
			"\tfilename=\"" + name + "\"",
			"Content-Type: application/pdf",
			fmt.Sprintf("X-Apple-Content-Length: %d", n),
			"",
			"",
		}
	}
	lines := []string{
		"From: a@example.com",
		`Content-Type: multipart/mixed; boundary="b"`,
		"",
		"--b",
		"Content-Type: text/html",
		"",
		"<p>two invoices</p>",
	}
	lines = append(lines, part("one.pdf", 10)...)
	lines = append(lines, part("two.pdf", 10)...)
	lines = append(lines, "--b--", "")
	mime := strings.Join(lines, nl)
	one, two := []byte("%PDF-one"), []byte("%PDF-two")
	path := writePartial(t, t.TempDir(), 11, mime, map[string][]byte{
		"2/one.pdf": one,
		"3/two.pdf": two,
	})

	msg, err := ParseFile(path, 1<<20)
	require.NoError(err)
	raw := string(msg.Raw)
	assert.Equal(2, msg.RestoredAttachments)
	assert.Contains(raw, base64.StdEncoding.EncodeToString(one))
	assert.Contains(raw, base64.StdEncoding.EncodeToString(two))
	assert.NotContains(raw, "X-Apple-Content-Length")
}

func TestParseFile_PreservesCRLF(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	pdf := []byte("%PDF-crlf")
	mime := placeholderMIME("\r\n", "=-b", "report.pdf", 12)
	path := writePartial(t, t.TempDir(), 13, mime, map[string][]byte{
		"2/report.pdf": pdf,
	})

	msg, err := ParseFile(path, 1<<20)
	require.NoError(err)
	raw := string(msg.Raw)
	assert.Equal(1, msg.RestoredAttachments)
	assert.NotContains(strings.ReplaceAll(raw, "\r\n", ""), "\n", "every line ending must stay CRLF")
	assert.Contains(raw, base64.StdEncoding.EncodeToString(pdf)+"\r\n")
}

func TestParseFile_PicksSingleFileWhenNameDiffers(t *testing.T) {
	// Apple may decode the filename differently than the raw header spells
	// it. The encoded spelling can be invalid on Windows or exceed the
	// filesystem's filename limit even when the decoded name is valid.
	for _, tt := range []struct{ name, encoded, cached string }{
		{"encoded", "=?utf-8?Q?Rechnung=5F1.pdf?=", "Rechnung_1.pdf"},
		{"long_encoded", "=?utf-8?Q?" + strings.Repeat("=61", 100) + ".pdf?=", strings.Repeat("a", 100) + ".pdf"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			pdf := []byte("%PDF-renamed")
			raw := placeholderMIME("\n", "=-b", tt.encoded, 12)
			path := writePartial(t, t.TempDir(), 15, raw, map[string][]byte{
				"2/" + tt.cached: pdf,
			})
			msg, err := ParseFile(path, 1<<20)
			require.NoError(err)
			require.NoError(msg.RestorationError)
			assert.Equal(1, msg.RestoredAttachments)
			parsed, err := mime.Parse(msg.Raw)
			require.NoError(err)
			require.Len(parsed.Attachments, 1)
			assert.Equal(pdf, parsed.Attachments[0].Content)
		})
	}
}

func TestParseFile_RejectsPathTraversalInFilename(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := t.TempDir()
	// A file outside the Attachments/ tree that a hostile sender must never
	// be able to pull into the archive via the filename header.
	secret := []byte("-----BEGIN PRIVATE KEY----- not for you")
	require.NoError(os.WriteFile(filepath.Join(root, "secret.txt"), secret, 0o600))

	mime := placeholderMIME("\n", "=-b", "../../../secret.txt", 12)
	// Attachments/17 exists so the lookup runs, but the part directory holds
	// nothing, so the single-file fallback cannot kick in either.
	path := writePartial(t, root, 17, mime, nil)
	require.NoError(os.MkdirAll(filepath.Join(root, "Attachments", "17", "2"), 0o755))

	msg, err := ParseFile(path, 1<<20)
	require.NoError(err)
	assert.Equal(0, msg.RestoredAttachments)
	assert.NotContains(string(msg.Raw), base64.StdEncoding.EncodeToString(secret), "traversal filename must not read outside the part directory")
	assert.Contains(string(msg.Raw), "X-Apple-Content-Length", "placeholder must survive when nothing is restored")
}

func TestParseFile_SkipsAttachmentThatExceedsBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	big := bytes.Repeat([]byte("x"), 4000)
	mime := placeholderMIME("\n", "=-b", "big.bin", 5334)
	path := writePartial(t, t.TempDir(), 19, mime, map[string][]byte{
		"2/big.bin": big,
	})

	// Budget covers the emlx itself plus a little, but not the 4000-byte file
	// once base64-encoded.
	msg, err := ParseFile(path, int64(len(mime))+1000)
	require.NoError(err)
	assert.Equal(0, msg.RestoredAttachments)
	assert.NotContains(unwrapped(msg.Raw), base64.StdEncoding.EncodeToString(big), "over-budget bytes must not be read")
	assert.Contains(string(msg.Raw), "X-Apple-Content-Length", "over-budget part must stay a placeholder")
	assert.Equal(mime, string(msg.Raw))
}

func TestReadAttachment_FileChangesAfterSizeCheck(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%t", replace), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dir := t.TempDir()
			partDir := filepath.Join(dir, "2")
			require.NoError(os.Mkdir(partDir, 0700))
			path := filepath.Join(partDir, "notes.txt")
			require.NoError(os.WriteFile(path, []byte("note"), 0600))
			resolved, size, err := resolveAttachment(dir, "2", "notes.txt")
			require.NoError(err)
			require.Equal(int64(4), size)

			// A completed size check cannot prevent a later cache update.
			larger := bytes.Repeat([]byte("x"), 4096)
			if replace {
				replacement := filepath.Join(dir, "replacement")
				require.NoError(os.WriteFile(replacement, larger, 0600))
				require.NoError(os.Rename(replacement, path))
			} else {
				require.NoError(os.WriteFile(path, larger, 0600))
			}
			content, err := readAttachment(resolved, 8)
			require.NoError(err)
			require.Len(content, 9, "read only the budget plus one overflow byte")
			assert.Equal(bytes.Repeat([]byte("x"), 9), content, "read only the budget plus one overflow byte")

			require.NoError(os.WriteFile(path, []byte("12345678"), 0600))
			content, err = readAttachment(resolved, 8)
			require.NoError(err)
			assert.Equal([]byte("12345678"), content, "an attachment that fits is read in full")
		})
	}
}

func TestParseFile_RestoresFirstAttachmentAndSkipsSecondWhenBudgetRunsOut(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	nl := "\n"
	part := func(name string, n int) []string {
		return []string{
			"--b",
			"Content-Transfer-Encoding: base64",
			"Content-Disposition: attachment;",
			"\tfilename=\"" + name + "\"",
			"Content-Type: application/octet-stream",
			fmt.Sprintf("X-Apple-Content-Length: %d", n),
			"",
			"",
		}
	}
	lines := []string{
		"From: a@example.com",
		`Content-Type: multipart/mixed; boundary="b"`,
		"",
		"--b",
		"Content-Type: text/plain",
		"",
		"two files",
	}
	lines = append(lines, part("one.bin", 1334)...)
	lines = append(lines, part("two.bin", 1334)...)
	lines = append(lines, "--b--", "")
	mime := strings.Join(lines, nl)
	one := bytes.Repeat([]byte("1"), 1000)
	two := bytes.Repeat([]byte("2"), 1000)
	path := writePartial(t, t.TempDir(), 21, mime, map[string][]byte{
		"2/one.bin": one,
		"3/two.bin": two,
	})

	// Room for one encoded file (~1370 bytes) but not two.
	msg, err := ParseFile(path, int64(len(mime))+2000)
	require.NoError(err)
	assert.Equal(1, msg.RestoredAttachments)
	assert.Contains(unwrapped(msg.Raw), base64.StdEncoding.EncodeToString(one))
	assert.NotContains(unwrapped(msg.Raw), base64.StdEncoding.EncodeToString(two))
	assert.Equal(1, strings.Count(string(msg.Raw), "X-Apple-Content-Length"), "second part keeps its placeholder")
}

func TestFindBoundary_ToleratesWhitespaceAroundEquals(t *testing.T) {
	assert := assert.New(t)
	for _, tc := range []struct{ header, want string }{
		{`Content-Type: multipart/mixed; boundary="=-b"`, "=-b"},
		{`Content-Type: multipart/mixed; boundary=plain`, "plain"},
		{`Content-Type: multipart/mixed; boundary = "spaced"`, "spaced"},
		{`Content-Type: multipart/mixed; boundary= tab`, "tab"},
		{`Content-Type: multipart/mixed; charset=utf-8; BOUNDARY="upper"`, "upper"},
	} {
		assert.Equal(tc.want, findBoundary([]string{tc.header}), tc.header)
	}
}
