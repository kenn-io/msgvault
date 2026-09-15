package imazingcsv

import (
	"bytes"
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHeader = "Chat Session,Message Date,Delivered Date,Read Date,Service,Type,Sender ID,Sender Name,Status,Replying to,Subject,Text,Attachment,Attachment type"

func TestDiscoverExportRootAndCSVDirectory(t *testing.T) {
	t.Run("export root", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		root := t.TempDir()
		csvDir := filepath.Join(root, "csv")
		require.NoError(os.Mkdir(csvDir, 0o700))
		writeTestFile(t, filepath.Join(csvDir, "B.CSV"), testHeader+"\n")
		writeTestFile(t, filepath.Join(csvDir, "a.csv"), testHeader+"\n")
		writeTestFile(t, filepath.Join(csvDir, "ignore.txt"), "ignored")

		layout, err := Discover(root)
		require.NoError(err)
		assert.Equal(root, layout.Root)
		assert.Equal(csvDir, layout.CSVDir)
		assert.Equal(filepath.Join(root, "attachments"), layout.AttachmentsDir)
		require.Len(layout.CSVFiles, 2)
		assert.Equal("a.csv", filepath.Base(layout.CSVFiles[0]))
		assert.Equal("B.CSV", filepath.Base(layout.CSVFiles[1]))
	})

	t.Run("CSV directory", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		root := t.TempDir()
		csvDir := filepath.Join(root, "messages")
		require.NoError(os.Mkdir(csvDir, 0o700))
		writeTestFile(t, filepath.Join(csvDir, "messages.csv"), testHeader+"\n")

		layout, err := Discover(csvDir)
		require.NoError(err)
		assert.Equal(root, layout.Root)
		assert.Equal(filepath.Join(root, "attachments"), layout.AttachmentsDir)
	})
}

func TestDiscoverRejectsInvalidInput(t *testing.T) {
	t.Run("not directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "messages.csv")
		writeTestFile(t, path, testHeader+"\n")
		_, err := Discover(path)
		require.ErrorContains(t, err, "directory")
	})

	t.Run("no CSV", func(t *testing.T) {
		_, err := Discover(t.TempDir())
		require.ErrorContains(t, err, "no CSV files")
	})
}

func TestParseFilesSupportedDialectsAndRawColumns(t *testing.T) {
	tests := []struct {
		name      string
		delimiter string
		bom       string
	}{
		{name: "comma", delimiter: ","},
		{name: "tab", delimiter: "\t"},
		{name: "semicolon", delimiter: ";", bom: "\ufeff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			header := replaceDelimiter(testHeader+",New Column", tt.delimiter)
			row := replaceDelimiter(`Family,2018-12-24 17:42:03,,,SMS,Incoming,+15550000002,Alice,Delivered,,,,,text/plain,future value`, tt.delimiter)
			writeTestFile(t, filepath.Join(dir, "messages.csv"), tt.bom+header+"\r\n"+row+"\r\n")

			layout, err := Discover(dir)
			require.NoError(err)
			rows, err := ParseFiles(context.Background(), layout, "America/New_York")
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal("Family", rows[0].ChatSession)
			assert.Equal(DirectionIncoming, rows[0].Direction)
			assert.Equal("sms", rows[0].Service)
			assert.Equal("future value", rows[0].Raw["New Column"])
			assert.Equal(2, rows[0].Record)
			assert.Equal(time.Date(2018, 12, 24, 22, 42, 3, 0, time.UTC), rows[0].SentAt)
		})
	}
}

func TestParseFilesQuotedNewlineAndMixedConversations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	var content bytes.Buffer
	writer := csv.NewWriter(&content)
	require.NoError(writer.Write([]string{
		"Chat Session", "Message Date", "Delivered Date", "Read Date", "Service", "Type",
		"Sender ID", "Sender Name", "Status", "Replying to", "Subject", "Text", "Attachment", "Attachment type",
	}))
	require.NoError(writer.Write([]string{
		"One", "2024-06-01T12:00:00Z", "", "", "iMessage", "Outgoing",
		"", "", "", "", "", "first\nsecond", "", "",
	}))
	require.NoError(writer.Write([]string{
		"Two", "2024-06-01T13:00:00Z", "", "", "RCS", "Incoming",
		"+15550000002", "Bob", "", "", "", "hello", "", "",
	}))
	writer.Flush()
	require.NoError(writer.Error())
	writeTestBytes(t, filepath.Join(dir, "messages.csv"), content.Bytes())
	layout, err := Discover(dir)
	require.NoError(err)
	rows, err := ParseFiles(context.Background(), layout, "UTC")
	require.NoError(err)
	require.Len(rows, 2)
	assert.Equal("first\nsecond", rows[0].Text)
	assert.Equal("One", rows[0].ChatSession)
	assert.Equal("Two", rows[1].ChatSession)
	assert.Equal("rcs", rows[1].Service)
}

func TestParseFilesPreservesMessageWhitespace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	writeTestCSV(t, filepath.Join(dir, "messages.csv"), [][]string{
		{"Chat", "2024-06-01T12:00:00Z", "", "", "SMS", "Outgoing", "", "", "", "  quoted reply  ", "  subject  ", "  body  ", "", ""},
	})
	layout, err := Discover(dir)
	require.NoError(err)
	rows, err := ParseFiles(context.Background(), layout, "UTC")
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal("  quoted reply  ", rows[0].ReplyingTo)
	assert.Equal("  subject  ", rows[0].Subject)
	assert.Equal("  body  ", rows[0].Text)
}

func TestParseFilesRejectsInvalidRows(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		want    string
	}{
		{name: "invalid UTF-8", content: append([]byte(testHeader+"\nChat,2024-01-01 00:00:00,,,SMS,Incoming,,,,,,"), 0xff), want: "invalid UTF-8"},
		{name: "duplicate header", content: []byte("Chat Session, chat session ,Message Date,Service,Type\nA,A,2024-01-01 00:00:00,SMS,Incoming\n"), want: "duplicate header"},
		{name: "headerless", content: []byte("Family,2024-01-01 00:00:00,SMS,Incoming\n"), want: "required columns"},
		{name: "missing column", content: []byte("Chat Session,Message Date,Service\nFamily,2024-01-01 00:00:00,SMS\n"), want: "required columns"},
		{name: "wrong field count", content: []byte(testHeader + "\nFamily,2024-01-01 00:00:00\n"), want: "record 2"},
		{name: "empty chat", content: []byte(testHeader + "\n,2024-01-01 00:00:00,,,SMS,Incoming,,,,,,,,\n"), want: "chat session"},
		{name: "bad date", content: []byte(testHeader + "\nFamily,yesterday,,,SMS,Incoming,,,,,,,,\n"), want: "message date"},
		{name: "bad direction", content: []byte(testHeader + "\nFamily,2024-01-01 00:00:00,,,SMS,Sideways,,,,,,,,\n"), want: "direction"},
		{name: "empty service", content: []byte(testHeader + "\nFamily,2024-01-01 00:00:00,,,,Incoming,,,,,,,,\n"), want: "service"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			writeTestBytes(t, filepath.Join(dir, "bad.csv"), tt.content)
			layout, err := Discover(dir)
			require.NoError(err)
			_, err = ParseFiles(context.Background(), layout, "UTC")
			require.Error(err)
			require.ErrorContains(err, "bad.csv")
			assert.ErrorContains(err, tt.want)
		})
	}
}

func TestParseFilesCancellation(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "messages.csv"), testHeader+"\n")
	layout, err := Discover(dir)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ParseFiles(ctx, layout, "UTC")
	require.ErrorIs(t, err, context.Canceled)
}

func TestParseWallTimeDSTPolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(err)

	got, err := ParseWallTime("2024-11-03 01:30:00", loc)
	require.NoError(err)
	assert.Equal(time.Date(2024, 11, 3, 5, 30, 0, 0, time.UTC), got)

	_, err = ParseWallTime("2024-03-10 02:30:00", loc)
	require.ErrorContains(err, "nonexistent")
}

func replaceDelimiter(value, delimiter string) string {
	if delimiter == "," {
		return value
	}
	result := make([]byte, len(value))
	copy(result, value)
	for i := range result {
		if result[i] == ',' {
			result[i] = delimiter[0]
		}
	}
	return string(result)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	writeTestBytes(t, path, []byte(content))
}

func writeTestBytes(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, content, 0o600))
}
