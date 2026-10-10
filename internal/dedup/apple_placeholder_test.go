package dedup_test

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/dedup"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// appleCopyMIME builds a partial or restored copy without using archive data.
func appleCopyMIME(placeholderHeader, newline string, nested bool, content []byte) []byte {
	part := "Content-Type: application/octet-stream\n" +
		"Content-Disposition: attachment; filename=notes.bin\n" +
		"Content-Transfer-Encoding: base64\n"
	if placeholderHeader != "" {
		part += placeholderHeader + ": 12\n\n"
	} else {
		part += "\n" + base64.StdEncoding.EncodeToString(content) + "\n"
	}
	if nested {
		part = "Content-Type: multipart/mixed; boundary=inner\n\n--inner\n" +
			part + "--inner--\n"
	}
	raw := "Message-ID: <apple-copy@example.test>\n" +
		"From: sender@example.test\nTo: recipient@example.test\n" +
		"Subject: Attachment restoration\nMIME-Version: 1.0\n" +
		"Content-Type: multipart/mixed; boundary=outer\n\n" +
		"--outer\nContent-Type: text/plain\n\nPlease read the attachment.\n" +
		"--outer\n" + part + "--outer--\n"
	return []byte(strings.ReplaceAll(raw, "\n", newline))
}

func TestEngine_ApplePlaceholderRestoredCopy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newer := older.Add(24 * time.Hour)
	partial := appleCopyMIME("X-Apple-Content-Length", "\r\n", false, nil)
	restored := appleCopyMIME("", "\r\n", false, []byte("restored synthetic attachment"))
	partialID := ingestRawMessage(t, f.Store, f.Source, "partial", partial, older)
	restoredID := ingestRawMessage(t, f.Store, f.Source, "restored", restored, newer)
	setArchivedAt(t, f.Store, partialID, older)
	setArchivedAt(t, f.Store, restoredID, newer)
	linkLabel(t, f.Store, f.Source.ID, partialID, "old-label", "Old label", "user")
	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs:           []int64{f.Source.ID},
		DeleteDupsFromSourceServer: true,
		DeletionsDir:               filepath.Join(t.TempDir(), "deletions"),
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)
	require.Len(report.Groups, 1)
	group := report.Groups[0]
	require.Equal(restoredID, group.Messages[group.Survivor].ID,
		"restored MIME must beat an older, label-rich Apple placeholder")

	summary, err := engine.Execute(t.Context(), report, "apple-restored")
	require.NoError(err)
	assert.Empty(summary.StagedManifests, "differing MIME must not stage remote deletion")
	assertSoftDeleted(t, f.Store, partialID, true)
	assertSoftDeleted(t, f.Store, restoredID, false)
	count, _, err := engine.Undo("apple-restored")
	require.NoError(err)
	assert.Equal(int64(1), count)
	assertSoftDeleted(t, f.Store, partialID, false)
	assertSoftDeleted(t, f.Store, restoredID, false)
}

func TestEngine_ApplePlaceholderUnknownMIME(t *testing.T) {
	for _, kind := range []string{"missing", "fatal parse", "corrupt zlib", "unknown received"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			partialID := ingestRawMessage(t, f.Store, f.Source, "partial",
				appleCopyMIME("X-Apple-Content-Length", "\n", false, nil), older)
			restoredID := ingestRawMessage(t, f.Store, f.Source, "restored",
				appleCopyMIME("", "\n", false, []byte("restored")), older.Add(time.Hour))
			linkLabel(t, f.Store, f.Source.ID, partialID, "old-label", "Old label", "user")
			unknownID := addMessage(t, f.Store, f.Source, "unknown", "apple-copy@example.test", false)
			wantID, wantCopies := partialID, 3
			switch kind {
			case "fatal parse":
				raw := []byte("Message-ID: <apple-copy@example.test>\n" +
					"Content-Type: multipart mixed; boundary=outer\n\n--outer--\n")
				parsed, parseErr := msgmime.ParseWithRecovery(raw, "")
				require.Error(parseErr)
				require.Equal("<apple-copy@example.test>", parsed.MessageID)
				require.NoError(f.Store.UpsertMessageRaw(unknownID, raw))
			case "corrupt zlib":
				require.NoError(f.Store.UpsertMessageRaw(unknownID, []byte("unused")))
				_, err := f.Store.DB().Exec(f.Store.Rebind(
					"UPDATE message_raw SET raw_data = ? WHERE message_id = ?"), []byte("invalid zlib"), unknownID)
				require.NoError(err)
				wantID, wantCopies = restoredID, 2
			case "unknown received":
				_, err := f.Store.DB().Exec(f.Store.Rebind(
					"UPDATE messages SET is_from_me = TRUE WHERE id IN (?, ?)"), partialID, restoredID)
				require.NoError(err)
				wantID = restoredID
			}
			engine := dedup.NewEngine(f.Store, dedup.Config{
				AccountSourceIDs: []int64{f.Source.ID},
			}, nil)
			report, err := engine.Scan(t.Context())
			require.NoError(err)
			require.Len(report.Groups, 1)
			group := report.Groups[0]
			assert.Len(group.Messages, wantCopies)
			assert.Equal(wantID, group.Messages[group.Survivor].ID)
		})
	}
}

func TestEngine_ApplePlaceholderUnrelatedCollisionKeepsFallback(t *testing.T) {
	for _, tc := range []struct {
		name, old, replacement string
	}{
		{"subject", "Subject: Attachment restoration", "Subject: Another synthetic message"},
		{"sender", "From: sender@example.test", "From: other@example.test"},
		{"body", "Please read the attachment.", "An unrelated synthetic body."},
		{"attachment name", "filename=notes.bin", "filename=other.bin"},
		{"unrelated encoding", "Content-Type: text/plain\n\nPlease read the attachment.",
			"Content-Type: text/plain\nContent-Transfer-Encoding: base64\n\nUGxlYXNlIHJlYWQgdGhlIGF0dGFjaG1lbnQu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			f := storetest.New(t)
			older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			partialID := ingestRawMessage(t, f.Store, f.Source, "partial",
				appleCopyMIME("X-Apple-Content-Length", "\n", true, nil), older)
			restored := appleCopyMIME("", "\n", true, []byte("synthetic attachment"))
			unrelated := []byte(strings.Replace(string(restored), tc.old, tc.replacement, 1))
			require.NotEqual(restored, unrelated)
			otherID := ingestRawMessage(t, f.Store, f.Source, "unrelated", unrelated, older.Add(time.Hour))
			setArchivedAt(t, f.Store, partialID, older)
			setArchivedAt(t, f.Store, otherID, older.Add(time.Hour))
			engine := dedup.NewEngine(f.Store, dedup.Config{AccountSourceIDs: []int64{f.Source.ID}}, nil)
			report, err := engine.Scan(t.Context())
			require.NoError(err)
			require.Len(report.Groups, 1)
			group := report.Groups[0]
			assert.Equal(t, partialID, group.Messages[group.Survivor].ID,
				"unrelated content must keep the earlier archived-copy fallback")
		})
	}
}

func FuzzEngine_ApplePlaceholderUnrelatedCollision(f *testing.F) {
	f.Add([]byte("unrelated"), false)
	f.Add([]byte{0, 255}, true)
	f.Fuzz(func(t *testing.T, content []byte, nested bool) {
		content = content[:min(len(content), 4096)]
		fixture := storetest.New(t)
		older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		partialID := ingestRawMessage(t, fixture.Store, fixture.Source, "partial",
			appleCopyMIME("X-Apple-Content-Length", "\n", nested, nil), older)
		restored := appleCopyMIME("", "\n", nested, []byte("synthetic attachment"))
		unrelated := []byte(strings.Replace(string(restored), "Please read the attachment.",
			"Other synthetic body: "+base64.StdEncoding.EncodeToString(content), 1))
		otherID := ingestRawMessage(t, fixture.Store, fixture.Source, "unrelated", unrelated, older.Add(time.Hour))
		setArchivedAt(t, fixture.Store, partialID, older)
		setArchivedAt(t, fixture.Store, otherID, older.Add(time.Hour))
		engine := dedup.NewEngine(fixture.Store, dedup.Config{AccountSourceIDs: []int64{fixture.Source.ID}}, nil)
		report, err := engine.Scan(t.Context())
		require.NoError(t, err)
		require.Len(t, report.Groups, 1)
		group := report.Groups[0]
		assert.Equal(t, partialID, group.Messages[group.Survivor].ID)
	})
}

// The oracle is the restored row, independent of attachment counts or hashes.
// A casing-sensitive byte search or a missing completeness tier breaks it.
func FuzzEngine_ApplePlaceholderRestoredCopy(f *testing.F) {
	f.Add(uint32(0), false, false, []byte("restored"))
	f.Add(^uint32(0), true, true, []byte{0, 1, 255})
	f.Fuzz(func(t *testing.T, mask uint32, crlf, nested bool, content []byte) {
		header := []byte("x-apple-content-length")
		for i, ch := range header {
			if mask&(1<<i) != 0 && ch >= 'a' && ch <= 'z' {
				header[i] = ch - ('a' - 'A')
			}
		}
		newline := "\n"
		if crlf {
			newline = "\r\n"
		}
		// Bound materialization, while keeping the byte generator unrestricted.
		content = content[:min(len(content), 4096)]
		fixture := storetest.New(t)
		older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		partialID := ingestRawMessage(t, fixture.Store, fixture.Source, "partial",
			appleCopyMIME(string(header), newline, nested, nil), older)
		restoredID := ingestRawMessage(t, fixture.Store, fixture.Source, "restored",
			appleCopyMIME("", newline, nested, content), older.Add(time.Hour))
		setArchivedAt(t, fixture.Store, partialID, older)
		setArchivedAt(t, fixture.Store, restoredID, older.Add(time.Hour))
		engine := dedup.NewEngine(fixture.Store, dedup.Config{
			AccountSourceIDs: []int64{fixture.Source.ID},
		}, nil)
		report, err := engine.Scan(t.Context())
		require.NoError(t, err)
		require.Len(t, report.Groups, 1)
		group := report.Groups[0]
		assert.Equalf(t, restoredID, group.Messages[group.Survivor].ID,
			"header=%s crlf=%t nested=%t", header, crlf, nested)
	})
}

func TestEngine_ApplePlaceholderUnrepresentedMIMEKeepsFallback(t *testing.T) {
	for _, kind := range []string{"preamble", "nested preamble", "corrupt body", "matching preambles"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			f := storetest.New(t)
			older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			partial := string(appleCopyMIME("X-Apple-Content-Length", "\n", true, nil))
			other := string(appleCopyMIME("", "\n", true, []byte("restored")))
			switch kind {
			case "preamble":
				partial = strings.Replace(partial, "boundary=outer\n\n", "boundary=outer\n\nOriginal preamble.\n", 1)
				other = strings.Replace(other, "boundary=outer\n\n", "boundary=outer\n\nUnrelated preamble.\n", 1)
			case "nested preamble":
				partial = strings.Replace(partial, "boundary=inner\n\n", "boundary=inner\n\nOriginal nested preamble.\n", 1)
				other = strings.Replace(other, "boundary=inner\n\n", "boundary=inner\n\nUnrelated nested preamble.\n", 1)
			case "matching preambles":
				for _, boundary := range []string{"outer", "inner"} {
					before := "boundary=" + boundary + "\n\n"
					after := before + "Shared synthetic MIME preamble.\n\n"
					partial = strings.Replace(partial, before, after, 1)
					other = strings.Replace(other, before, after, 1)
				}
			case "corrupt body":
				original := "Content-Type: text/plain\n\nPlease read the attachment."
				partial = strings.Replace(partial, original, "Content-Type: text/plain\nContent-Transfer-Encoding: base64\n\nAAAAA", 1)
				other = strings.Replace(other, original, "Content-Type: text/plain\nContent-Transfer-Encoding: base64\n\nBBBBB", 1)
			}
			partialParsed, _, err := msgmime.ParseWithRecoveryAndPartFingerprints([]byte(partial), "")
			require.NoError(err)
			otherParsed, _, err := msgmime.ParseWithRecoveryAndPartFingerprints([]byte(other), "")
			require.NoError(err)
			if kind == "corrupt body" {
				require.NotEmpty(partialParsed.Errors)
				require.NotEmpty(otherParsed.Errors)
			}
			partialID := ingestRawMessage(t, f.Store, f.Source, "partial", []byte(partial), older)
			otherID := ingestRawMessage(t, f.Store, f.Source, "unrelated", []byte(other), older.Add(time.Hour))
			setArchivedAt(t, f.Store, partialID, older)
			setArchivedAt(t, f.Store, otherID, older.Add(time.Hour))
			engine := dedup.NewEngine(f.Store, dedup.Config{AccountSourceIDs: []int64{f.Source.ID}}, nil)
			report, err := engine.Scan(t.Context())
			require.NoError(err)
			require.Len(report.Groups, 1)
			group := report.Groups[0]
			wantID := partialID
			if kind == "matching preambles" {
				wantID = otherID
			}
			assert.Equal(t, wantID, group.Messages[group.Survivor].ID, "only placeholder differences may enable the preference")
		})
	}
}

func TestEngine_ApplePlaceholderConflictingRestorationsKeepFallback(t *testing.T) {
	f := storetest.New(t)
	older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	partialID := ingestRawMessage(t, f.Store, f.Source, "partial",
		appleCopyMIME("X-Apple-Content-Length", "\n", true, nil), older)
	firstID := ingestRawMessage(t, f.Store, f.Source, "restored-a",
		appleCopyMIME("", "\n", true, []byte("first restored attachment")), older.Add(time.Hour))
	secondID := ingestRawMessage(t, f.Store, f.Source, "restored-b",
		appleCopyMIME("", "\n", true, []byte("contradictory attachment")), older.Add(2*time.Hour))
	setArchivedAt(t, f.Store, partialID, older)
	setArchivedAt(t, f.Store, firstID, older.Add(time.Hour))
	setArchivedAt(t, f.Store, secondID, older.Add(2*time.Hour))
	linkLabel(t, f.Store, f.Source.ID, partialID, "old-label", "Old label", "user")
	engine := dedup.NewEngine(f.Store, dedup.Config{AccountSourceIDs: []int64{f.Source.ID}}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(t, err)
	require.Len(t, report.Groups, 1)
	group := report.Groups[0]
	assert.Equal(t, partialID, group.Messages[group.Survivor].ID, "conflicting restored attachment bytes do not establish one restoration")
}
