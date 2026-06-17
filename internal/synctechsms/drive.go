package synctechsms

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"
)

type DriveClient interface {
	ListBackupFiles(ctx context.Context, folderID string) ([]DriveFile, error)
	DownloadToFile(ctx context.Context, fileID, path string) error
}

type DriveFile struct {
	ID           string
	Name         string
	MimeType     string
	Size         int64
	Checksum     string
	ModifiedTime time.Time
}

type GoogleDriveClient struct {
	service *drive.Service
}

func NewGoogleDriveClient(service *drive.Service) *GoogleDriveClient {
	return &GoogleDriveClient{service: service}
}

func (c *GoogleDriveClient) ListBackupFiles(ctx context.Context, folderID string) ([]DriveFile, error) {
	if !validDriveFolderID(folderID) {
		return nil, errors.New("invalid Drive folder ID")
	}
	q := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
	var out []DriveFile
	pageToken := ""
	for {
		call := c.service.Files.List().
			Context(ctx).
			Q(q).
			Fields("nextPageToken,files(id,name,mimeType,size,md5Checksum,modifiedTime)").
			OrderBy("modifiedTime")
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("list Drive files: %w", err)
		}
		for _, f := range resp.Files {
			if !isDriveBackupCandidate(f.Name) {
				continue
			}
			mod, _ := time.Parse(time.RFC3339, f.ModifiedTime)
			out = append(out, DriveFile{
				ID: f.Id, Name: f.Name, MimeType: f.MimeType, Size: f.Size,
				Checksum: f.Md5Checksum, ModifiedTime: mod,
			})
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModifiedTime.Before(out[j].ModifiedTime) })
	return out, nil
}

func (c *GoogleDriveClient) DownloadToFile(ctx context.Context, fileID, path string) error {
	resp, err := c.service.Files.Get(fileID).Context(ctx).Download()
	if err != nil {
		return fmt.Errorf("download Drive file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write Drive download: %w", err)
	}
	return nil
}

func SelectStableDriveFiles(files []DriveFile, now time.Time, stableAfter time.Duration, imported map[string]string) []DriveFile {
	var out []DriveFile
	for _, f := range files {
		if now.Sub(f.ModifiedTime) < stableAfter {
			continue
		}
		if imported[f.ID] != "" && imported[f.ID] == f.Checksum {
			continue
		}
		out = append(out, f)
	}
	return out
}

var driveFolderIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validDriveFolderID(folderID string) bool {
	return driveFolderIDPattern.MatchString(folderID)
}

func isDriveBackupCandidate(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".zip")
}
