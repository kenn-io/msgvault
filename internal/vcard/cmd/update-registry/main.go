// Command update-registry checks or updates the vendored IANA vCard registry.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vcard/registry"
)

const (
	defaultBaseURL = "https://www.iana.org/assignments/vcard-elements"
	defaultDestDir = "internal/vcard/registry/data"
	maxResponse    = 8 << 20
)

var snapshotNames = []string{
	"metadata.json",
	"properties.csv",
	"parameters.csv",
	"value-data-types.csv",
	"property-values.csv",
	"parameter-values.csv",
}

type options struct {
	baseURL string
	destDir string
	write   bool
}

type registryXML struct {
	Updated string `xml:"updated"`
}

type registryMetadata struct {
	Source  string `json:"source"`
	Updated string `json:"updated"`
}

func main() {
	var write bool
	flag.BoolVar(&write, "write", false, "write a validated IANA registry snapshot")
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}
	err := run(context.Background(), client, os.Stdout, options{
		baseURL: defaultBaseURL,
		destDir: defaultDestDir,
		write:   write,
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	client *http.Client,
	stdout io.Writer,
	opts options,
) error {
	if client == nil {
		return errors.New("HTTP client is required")
	}
	if stdout == nil {
		return errors.New("stdout writer is required")
	}
	opts.baseURL = strings.TrimRight(opts.baseURL, "/")
	if opts.baseURL == "" {
		return errors.New("base URL is required")
	}
	if opts.destDir == "" {
		return errors.New("destination directory is required")
	}

	xmlData, err := fetch(ctx, client, opts.baseURL+"/vcard-elements.xml", "vcard-elements.xml")
	if err != nil {
		return err
	}
	updated, err := parseUpdated(xmlData)
	if err != nil {
		return err
	}

	remote := make(map[string][]byte, len(snapshotNames))
	for _, name := range snapshotNames[1:] {
		data, fetchErr := fetch(ctx, client, opts.baseURL+"/"+name, name)
		if fetchErr != nil {
			return fetchErr
		}
		remote[name] = data
	}
	metadataBytes, err := json.Marshal(registryMetadata{
		Source:  defaultBaseURL + "/vcard-elements.xhtml",
		Updated: updated,
	})
	if err != nil {
		return fmt.Errorf("encode metadata: %w", err)
	}
	remote["metadata.json"] = append(metadataBytes, '\n')

	if _, err := registry.Parse(filesFromMap(remote)); err != nil {
		return fmt.Errorf("validate fetched registry: %w", err)
	}

	changed, err := changedFiles(opts.destDir, remote)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return nil
	}
	for _, name := range changed {
		if _, err := fmt.Fprintln(stdout, name); err != nil {
			return fmt.Errorf("report changed registry files: %w", err)
		}
	}
	if !opts.write {
		return fmt.Errorf("IANA vCard registry drift: %s", strings.Join(changed, ", "))
	}

	if err := os.MkdirAll(opts.destDir, 0o755); err != nil {
		return fmt.Errorf("create registry destination: %w", err)
	}
	for _, name := range changed {
		if err := writeAtomic(filepath.Join(opts.destDir, name), remote[name]); err != nil {
			return err
		}
	}
	return nil
}

func fetch(
	ctx context.Context,
	client *http.Client,
	url string,
	name string,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request for %s: %w", name, err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", name, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch %s: %s", name, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(data) > maxResponse {
		return nil, fmt.Errorf("%s response exceeds %d bytes", name, maxResponse)
	}
	return data, nil
}

func parseUpdated(data []byte) (string, error) {
	var document registryXML
	if err := xml.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("parse registry XML: %w", err)
	}
	updated := strings.TrimSpace(document.Updated)
	parsed, err := time.Parse("2006-01-02", updated)
	if err != nil || parsed.Format("2006-01-02") != updated {
		return "", fmt.Errorf("registry update date %q is not YYYY-MM-DD", updated)
	}
	return updated, nil
}

func filesFromMap(files map[string][]byte) registry.Files {
	return registry.Files{
		Metadata:        files["metadata.json"],
		Properties:      files["properties.csv"],
		Parameters:      files["parameters.csv"],
		ValueDataTypes:  files["value-data-types.csv"],
		PropertyValues:  files["property-values.csv"],
		ParameterValues: files["parameter-values.csv"],
	}
}

func changedFiles(destDir string, remote map[string][]byte) ([]string, error) {
	var changed []string
	for _, name := range snapshotNames {
		local, err := os.ReadFile(filepath.Join(destDir, name))
		switch {
		case err == nil:
			if !bytes.Equal(local, remote[name]) {
				changed = append(changed, name)
			}
		case errors.Is(err, os.ErrNotExist):
			changed = append(changed, name)
		default:
			return nil, fmt.Errorf("read local %s: %w", name, err)
		}
	}
	return changed, nil
}

func writeAtomic(dest string, data []byte) (retErr error) {
	dir := filepath.Dir(dest)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", filepath.Base(dest), err)
	}
	tempName := temp.Name()
	defer func() {
		if retErr != nil {
			_ = temp.Close()
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0o644); err != nil {
		return fmt.Errorf("set mode on temporary %s: %w", filepath.Base(dest), err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write temporary %s: %w", filepath.Base(dest), err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary %s: %w", filepath.Base(dest), err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary %s: %w", filepath.Base(dest), err)
	}
	if err := os.Rename(tempName, dest); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(dest), err)
	}
	return nil
}
