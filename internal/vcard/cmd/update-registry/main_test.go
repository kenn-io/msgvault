package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vcard/registry"
)

func TestRunWritesValidatedSnapshotAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := newIANATestServer(t, "2026-01-13")
	defer server.Close()

	dest := t.TempDir()
	err := run(context.Background(), server.Client(), io.Discard, options{
		baseURL: server.URL,
		destDir: dest,
		write:   true,
	})
	require.NoError(err)

	loaded, err := loadFiles(dest)
	require.NoError(err)
	snapshot, err := registry.Parse(loaded)
	require.NoError(err)
	assert.Equal("2026-01-13", snapshot.Updated)
	assert.Equal("SOURCE", snapshot.Properties[0].Name)
}

func TestRunCheckReportsDriftWithoutWriting(t *testing.T) {
	assert := assert.New(t)
	server := newIANATestServer(t, "2026-01-14")
	defer server.Close()

	dest := writeExistingSnapshot(t, "2026-01-13")
	var stdout bytes.Buffer
	err := run(context.Background(), server.Client(), &stdout, options{
		baseURL: server.URL,
		destDir: dest,
		write:   false,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "IANA vCard registry drift")
	assert.Equal("2026-01-13", readMetadataDate(t, dest))
	assert.Contains(stdout.String(), "metadata.json")
}

func TestRunIdenticalSnapshotIsNoOp(t *testing.T) {
	server := newIANATestServer(t, "2026-01-13")
	defer server.Close()

	dest := writeExistingSnapshot(t, "2026-01-13")
	before := readSnapshotBytes(t, dest)

	require.NoError(t, run(context.Background(), server.Client(), io.Discard, options{
		baseURL: server.URL,
		destDir: dest,
		write:   false,
	}))
	require.NoError(t, run(context.Background(), server.Client(), io.Discard, options{
		baseURL: server.URL,
		destDir: dest,
		write:   true,
	}))
	assert.Equal(t, before, readSnapshotBytes(t, dest))
}

func TestRunRejectsMalformedRemoteBeforeChangingDestination(t *testing.T) {
	files := testRegistryFiles("2026-01-14")
	files.Properties = []byte("Namespace,Property,Reference\n,SOURCE\n")
	server := newIANATestServerWithFiles(t, "2026-01-14", files)
	defer server.Close()

	dest := writeExistingSnapshot(t, "2026-01-13")
	before := readSnapshotBytes(t, dest)
	err := run(context.Background(), server.Client(), io.Discard, options{
		baseURL: server.URL,
		destDir: dest,
		write:   true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "properties record 2")
	assert.Equal(t, before, readSnapshotBytes(t, dest))
}

func TestRunRejectsNonSuccessResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := run(context.Background(), server.Client(), io.Discard, options{
		baseURL: server.URL,
		destDir: t.TempDir(),
		write:   true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "503 Service Unavailable")
}

func TestRunRejectsResponseLargerThanLimit(t *testing.T) {
	files := testRegistryFiles("2026-01-13")
	server := newIANATestServerWithFiles(t, "2026-01-13", files)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/properties.csv" {
			_, _ = w.Write(bytes.Repeat([]byte("x"), (8<<20)+1))
			return
		}
		serveRegistryFile(t, w, r, "2026-01-13", files)
	})
	defer server.Close()

	err := run(context.Background(), server.Client(), io.Discard, options{
		baseURL: server.URL,
		destDir: t.TempDir(),
		write:   true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "properties.csv")
	require.ErrorContains(t, err, "exceeds 8388608 bytes")
}

func TestRunRejectsMalformedRegistryUpdateDate(t *testing.T) {
	files := testRegistryFiles("2026-01-13")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/vcard-elements.xml" {
			_, _ = io.WriteString(w, `<registry><updated>not-a-date</updated></registry>`)
			return
		}
		serveRegistryFile(t, w, r, "2026-01-13", files)
	}))
	defer server.Close()

	err := run(context.Background(), server.Client(), io.Discard, options{
		baseURL: server.URL,
		destDir: t.TempDir(),
		write:   true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "registry update date")
}

func newIANATestServer(t *testing.T, updated string) *httptest.Server {
	t.Helper()
	return newIANATestServerWithFiles(t, updated, testRegistryFiles(updated))
}

func newIANATestServerWithFiles(t *testing.T, updated string, files registry.Files) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRegistryFile(t, w, r, updated, files)
	}))
}

func serveRegistryFile(
	t *testing.T,
	w http.ResponseWriter,
	r *http.Request,
	updated string,
	files registry.Files,
) {
	t.Helper()
	switch r.URL.Path {
	case "/vcard-elements.xml":
		_, _ = io.WriteString(w, `<registry><updated>`+updated+`</updated></registry>`)
	case "/properties.csv":
		_, _ = w.Write(files.Properties)
	case "/parameters.csv":
		_, _ = w.Write(files.Parameters)
	case "/value-data-types.csv":
		_, _ = w.Write(files.ValueDataTypes)
	case "/property-values.csv":
		_, _ = w.Write(files.PropertyValues)
	case "/parameter-values.csv":
		_, _ = w.Write(files.ParameterValues)
	default:
		http.NotFound(w, r)
	}
}

func loadFiles(dir string) (registry.Files, error) {
	read := func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, name))
	}
	metadata, err := read("metadata.json")
	if err != nil {
		return registry.Files{}, err
	}
	properties, err := read("properties.csv")
	if err != nil {
		return registry.Files{}, err
	}
	parameters, err := read("parameters.csv")
	if err != nil {
		return registry.Files{}, err
	}
	valueDataTypes, err := read("value-data-types.csv")
	if err != nil {
		return registry.Files{}, err
	}
	propertyValues, err := read("property-values.csv")
	if err != nil {
		return registry.Files{}, err
	}
	parameterValues, err := read("parameter-values.csv")
	if err != nil {
		return registry.Files{}, err
	}
	return registry.Files{
		Metadata:        metadata,
		Properties:      properties,
		Parameters:      parameters,
		ValueDataTypes:  valueDataTypes,
		PropertyValues:  propertyValues,
		ParameterValues: parameterValues,
	}, nil
}

func writeExistingSnapshot(t *testing.T, updated string) string {
	t.Helper()
	dir := t.TempDir()
	files := testRegistryFiles(updated)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), files.Metadata, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "properties.csv"), files.Properties, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "parameters.csv"), files.Parameters, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "value-data-types.csv"), files.ValueDataTypes, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "property-values.csv"), files.PropertyValues, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "parameter-values.csv"), files.ParameterValues, 0o600))
	return dir
}

func readMetadataDate(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	require.NoError(t, err)
	var metadata struct {
		Updated string `json:"updated"`
	}
	require.NoError(t, json.Unmarshal(data, &metadata))
	return metadata.Updated
}

func readSnapshotBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := make(map[string]string)
	for _, name := range snapshotNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		got[name] = string(data)
	}
	return got
}

func testRegistryFiles(updated string) registry.Files {
	metadata, err := json.Marshal(struct {
		Source  string `json:"source"`
		Updated string `json:"updated"`
	}{
		Source:  "https://www.iana.org/assignments/vcard-elements/vcard-elements.xhtml",
		Updated: updated,
	})
	if err != nil {
		panic(err)
	}
	metadata = append(metadata, '\n')
	return registry.Files{
		Metadata:        metadata,
		Properties:      []byte("Namespace,Property,Reference\n,SOURCE,[RFC6350]\n"),
		Parameters:      []byte("Namespace,Parameter,Reference\n,PREF,[RFC6350]\n"),
		ValueDataTypes:  []byte("Value Data Type,Reference\nTEXT,[RFC6350]\n"),
		PropertyValues:  []byte("Property,Value,Reference\nKIND,individual,[RFC6350]\n"),
		ParameterValues: []byte("Property,Parameter,Value,Reference\nTEL,TYPE,voice,[RFC6350]\n"),
	}
}
