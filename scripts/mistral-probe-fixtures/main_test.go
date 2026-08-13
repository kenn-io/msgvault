package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/documentindex/mistral"
)

func TestGeneratedFixturesContainSentinelAndPassProductionDetection(t *testing.T) {
	for _, candidate := range mistral.CandidateFormats() {
		if slices.Contains(nativeSeedFormats, candidate.ID) {
			continue
		}
		t.Run(candidate.ID, func(t *testing.T) {
			require := require.New(t)
			content, generated, err := generatedFixture(candidate.ID)
			require.NoError(err)
			require.True(generated)
			require.NotEmpty(content)
			detected, err := mistral.DetectFormat(bytes.NewReader(content), int64(len(content)), candidate.MediaType)
			require.NoError(err)
			assert.Equal(t, candidate.ID, detected.ID)
			sentinel, err := mistral.ProbeFixtureSentinel(candidate.ID)
			require.NoError(err)
			assert.True(t, fixtureContains(t, content, sentinel), "fixture must contain its native sentinel")
		})
	}
}

func TestGeneratedPackageFixturesAreDeterministicAndPortable(t *testing.T) {
	for _, id := range []string{"odt", "ods", "epub"} {
		t.Run(id, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			first, generated, err := generatedFixture(id)
			require.NoError(err)
			require.True(generated)
			second, generated, err := generatedFixture(id)
			require.NoError(err)
			require.True(generated)
			assert.Equal(first, second)

			archive, err := zip.NewReader(bytes.NewReader(first), int64(len(first)))
			require.NoError(err)
			require.NotEmpty(archive.File)
			mimetype := archive.File[0]
			assert.Equal("mimetype", mimetype.Name)
			assert.Equal(zip.Store, mimetype.Method)
			assert.Empty(mimetype.Extra)
			assert.Zero(mimetype.Flags&0x8, "mimetype entry must not use a data descriptor")
			assert.Equal(zipEpochDate, mimetype.ModifiedDate)
		})
	}
}

func TestBuildFixtureDirectoryResumesWithNativeSeeds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	outputDirectory := filepath.Join(t.TempDir(), "fixtures")
	created, missing, err := buildFixtureDirectory(outputDirectory, "")
	require.NoError(err)
	assert.Equal(len(mistral.CandidateFormats())-len(nativeSeedFormats), created)
	assert.Equal([]string{"doc", "msg", "numbers", "ppt", "xls"}, missing)

	seedDirectory := t.TempDir()
	for _, id := range nativeSeedFormats {
		candidate, ok := mistral.CandidateFormatByID(id)
		require.True(ok)
		sentinel, sentinelErr := mistral.ProbeFixtureSentinel(id)
		require.NoError(sentinelErr)
		var content []byte
		if id == "numbers" {
			content, _, err = zipFixture([]zipEntry{{name: "Index/Tables/Table.iwa", value: sentinel}})
			require.NoError(err)
		} else {
			markers := map[string]string{
				"doc": "WordDocument", "ppt": "PowerPoint Document", "xls": "Workbook", "msg": "__properties_version1.0",
			}
			content = nativeCompoundFixture(t, markers[id], sentinel)
		}
		detected, detectErr := mistral.DetectFormat(bytes.NewReader(content), int64(len(content)), candidate.MediaType)
		require.NoError(detectErr)
		assert.Equal(id, detected.ID)
		require.NoError(os.WriteFile(filepath.Join(seedDirectory, id), content, 0o600))
	}

	created, missing, err = buildFixtureDirectory(outputDirectory, seedDirectory)
	require.NoError(err)
	assert.Equal(len(nativeSeedFormats), created)
	assert.Empty(missing)
	entries, err := os.ReadDir(outputDirectory)
	require.NoError(err)
	assert.Len(entries, len(mistral.CandidateFormats()))
	if runtime.GOOS != "windows" {
		for _, entry := range entries {
			info, infoErr := entry.Info()
			require.NoError(infoErr)
			assert.Equal(os.FileMode(0o600), info.Mode().Perm())
		}
	}

	var output bytes.Buffer
	require.NoError(run([]string{"--output", outputDirectory}, &output))
	assert.Contains(output.String(), "26 Mistral probe fixtures")
	assert.Contains(output.String(), "0 created")
}

func fixtureContains(t *testing.T, content []byte, sentinel string) bool {
	t.Helper()
	require := require.New(t)
	if !bytes.HasPrefix(content, []byte("PK")) {
		return bytes.Contains(content, []byte(sentinel))
	}
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	require.NoError(err)
	for _, entry := range archive.File {
		reader, openErr := entry.Open()
		require.NoError(openErr)
		value, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		require.NoError(readErr)
		require.NoError(closeErr)
		if bytes.Contains(value, []byte(sentinel)) {
			return true
		}
	}
	return false
}

func nativeCompoundFixture(t *testing.T, streamName, sentinel string) []byte {
	t.Helper()
	const (
		freeSector = uint32(0xffffffff)
		endOfChain = uint32(0xfffffffe)
		fatSector  = uint32(0xfffffffd)
		noStream   = uint32(0xffffffff)
	)
	content := make([]byte, 3*512)
	header := content[:512]
	copy(header, []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1})
	binary.LittleEndian.PutUint16(header[26:28], 3)
	binary.LittleEndian.PutUint16(header[28:30], 0xfffe)
	binary.LittleEndian.PutUint16(header[30:32], 9)
	binary.LittleEndian.PutUint16(header[32:34], 6)
	binary.LittleEndian.PutUint32(header[44:48], 1)
	binary.LittleEndian.PutUint32(header[48:52], 1)
	binary.LittleEndian.PutUint32(header[56:60], 4096)
	binary.LittleEndian.PutUint32(header[60:64], endOfChain)
	binary.LittleEndian.PutUint32(header[68:72], endOfChain)
	for offset := 76; offset < 512; offset += 4 {
		binary.LittleEndian.PutUint32(header[offset:offset+4], freeSector)
	}
	binary.LittleEndian.PutUint32(header[76:80], 0)
	fat := content[512:1024]
	for offset := 0; offset < len(fat); offset += 4 {
		binary.LittleEndian.PutUint32(fat[offset:offset+4], freeSector)
	}
	binary.LittleEndian.PutUint32(fat[0:4], fatSector)
	binary.LittleEndian.PutUint32(fat[4:8], endOfChain)
	copy(fat[64:], sentinel)

	directory := content[1024:]
	writeCompoundEntry(t, directory[:128], "Root Entry", 5, noStream)
	binary.LittleEndian.PutUint32(directory[76:80], 1)
	writeCompoundEntry(t, directory[128:256], streamName, 2, noStream)
	return content
}

func writeCompoundEntry(t *testing.T, entry []byte, name string, entryType byte, noStream uint32) {
	t.Helper()
	encoded := make([]byte, 0, (len(name)+1)*2)
	for _, character := range name {
		require.Less(t, character, rune(0x10000))
		encoded = binary.LittleEndian.AppendUint16(encoded, uint16(character))
	}
	encoded = binary.LittleEndian.AppendUint16(encoded, 0)
	require.LessOrEqual(t, len(encoded), 64)
	copy(entry, encoded)
	binary.LittleEndian.PutUint16(entry[64:66], uint16(len(encoded)))
	entry[66] = entryType
	binary.LittleEndian.PutUint32(entry[68:72], noStream)
	binary.LittleEndian.PutUint32(entry[72:76], noStream)
	binary.LittleEndian.PutUint32(entry[76:80], noStream)
}
