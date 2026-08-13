package mistral

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadProbeFixturesSpoolsCompletePrivateMatrix(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixtureDirectory := t.TempDir()
	for _, candidate := range CandidateFormats() {
		require.NoError(os.WriteFile(
			filepath.Join(fixtureDirectory, candidate.ID),
			probeFixtureContent(t, candidate.ID),
			0o600,
		))
	}

	documents, cleanup, err := LoadProbeFixtures(t.Context(), fixtureDirectory, 1<<20)
	require.NoError(err)
	require.Len(documents, len(CandidateFormats()))
	spoolDirectory := ""
	for _, candidate := range CandidateFormats() {
		document, ok := documents[candidate.ID]
		require.True(ok)
		assert.Equal(candidate.MediaType, document.MediaType)
		info, statErr := os.Lstat(document.Path)
		require.NoError(statErr)
		assert.True(info.Mode().IsRegular())
		if runtime.GOOS != "windows" {
			assert.Equal(os.FileMode(0o600), info.Mode().Perm())
		}
		if spoolDirectory == "" {
			spoolDirectory = filepath.Dir(document.Path)
		}
		assert.Equal(spoolDirectory, filepath.Dir(document.Path))
	}

	require.NoError(cleanup())
	assert.NoDirExists(spoolDirectory)
	require.NoError(cleanup())
}

func TestLoadProbeFixturesFailsClosedOnMissingOrSymlinkFixture(t *testing.T) {
	require := require.New(t)
	fixtureDirectory := t.TempDir()
	_, _, err := LoadProbeFixtures(t.Context(), fixtureDirectory, 1<<20)
	require.ErrorContains(err, `fixture "pdf"`)

	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(fixtureDirectory, "target")
	require.NoError(os.WriteFile(target, []byte("%PDF-1.7\nsynthetic"), 0o600))
	require.NoError(os.Symlink(target, filepath.Join(fixtureDirectory, "pdf")))
	_, _, err = LoadProbeFixtures(t.Context(), fixtureDirectory, 1<<20)
	require.ErrorContains(err, "regular non-symlink")
}

func TestLoadProbeFixturesRejectsMislabeledContainer(t *testing.T) {
	require := require.New(t)
	fixtureDirectory := t.TempDir()
	for _, candidate := range CandidateFormats() {
		require.NoError(os.WriteFile(
			filepath.Join(fixtureDirectory, candidate.ID),
			probeFixtureContent(t, candidate.ID),
			0o600,
		))
	}
	require.NoError(os.WriteFile(
		filepath.Join(fixtureDirectory, "docx"), probeFixtureContent(t, "pdf"), 0o600,
	))

	documents, cleanup, err := LoadProbeFixtures(t.Context(), fixtureDirectory, 1<<20)
	require.ErrorContains(err, `fixture "docx"`)
	require.ErrorContains(err, "not declared")
	assert.Nil(t, documents)
	assert.Nil(t, cleanup)
}

func probeFixtureContent(t *testing.T, id string) []byte {
	t.Helper()
	switch id {
	case "pdf":
		return []byte("%PDF-1.7\nsynthetic")
	case "docx":
		return documentZIP(t, map[string]string{
			ooxmlContentTypesName: docxContentTypes("application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"),
			"word/document.xml":   "<document/>",
		})
	case "doc":
		return compoundDocument(t, "WordDocument")
	case "odt":
		return documentZIP(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.text", "META-INF/manifest.xml": "<manifest/>"})
	case "rtf":
		return []byte(`{\rtf1 synthetic}`)
	case "pptx":
		return documentZIP(t, map[string]string{
			ooxmlContentTypesName:  `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/></Types>`,
			"ppt/presentation.xml": "<presentation/>",
		})
	case "ppt":
		return compoundDocument(t, "PowerPoint Document")
	case "xlsx":
		return documentZIP(t, map[string]string{
			ooxmlContentTypesName: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/></Types>`,
			"xl/workbook.xml":     "<workbook/>",
		})
	case "xls":
		return compoundDocument(t, "Workbook")
	case "ods":
		return documentZIP(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.spreadsheet", "META-INF/manifest.xml": "<manifest/>"})
	case "numbers":
		return documentZIP(t, map[string]string{"Index/Tables/Table.iwa": "synthetic"})
	case "csv":
		return []byte("name,value\nsynthetic,42\n")
	case "epub":
		return documentZIP(t, map[string]string{"mimetype": "application/epub+zip", "META-INF/container.xml": "<container/>"})
	case "txt":
		return []byte("synthetic text")
	case "markdown":
		return []byte("# Synthetic\n")
	case "rst":
		return []byte("Synthetic\n=========\n")
	case "latex":
		return []byte(`\documentclass{article}\begin{document}synthetic\end{document}`)
	case "json":
		return []byte(`{"synthetic":true}`)
	case "jsonl":
		return []byte("{\"synthetic\":1}\n{\"synthetic\":2}\n")
	case "xml":
		return []byte("<synthetic>42</synthetic>")
	case "yaml":
		return []byte("---\nsynthetic: true\n")
	case "go":
		return []byte("package synthetic\n")
	case "python":
		return []byte("synthetic = True\n")
	case "javascript":
		return []byte("const synthetic = true;\n")
	case "eml":
		return []byte("From: sender@example.test\r\nDate: Thu, 13 Aug 2026 00:00:00 +0000\r\nSubject: Synthetic\r\n\r\nBody")
	case "msg":
		return compoundDocument(t, "__properties_version1.0")
	default:
		require.FailNow(t, "missing probe fixture builder", id)
		return nil
	}
}
