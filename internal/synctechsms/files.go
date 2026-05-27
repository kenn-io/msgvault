package synctechsms

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrEncryptedBackup = errors.New("encrypted SMS Backup & Restore backups are not supported; disable encryption in the Android app and export again")

type BackupFile struct {
	Name   string
	Kind   BackupKind
	Size   int64
	Opener func() (io.ReadCloser, error)
}

func DiscoverBackupFiles(path string) ([]BackupFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat backup path: %w", err)
	}
	if info.IsDir() {
		return discoverDir(path)
	}
	if strings.EqualFold(filepath.Ext(path), ".zip") {
		return discoverZip(path)
	}
	return classifyPath(path)
}

func discoverDir(dir string) ([]BackupFile, error) {
	var out []BackupFile
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".xml") {
			return nil
		}
		files, err := classifyPath(path)
		if err != nil {
			return nil //nolint:nilerr // unclassifiable files are skipped, not fatal to the walk
		}
		out = append(out, files...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk backup directory: %w", err)
	}
	sortBackupFiles(out)
	return out, nil
}

func classifyPath(path string) ([]BackupFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open backup file: %w", err)
	}
	defer func() { _ = f.Close() }()
	kind, ok := classifyXML(f)
	if !ok {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return []BackupFile{{
		Name: path,
		Kind: kind,
		Size: info.Size(),
		Opener: func() (io.ReadCloser, error) {
			return os.Open(path)
		},
	}}, nil
}

func discoverZip(path string) ([]BackupFile, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open zip backup: %w", err)
	}
	defer func() { _ = zr.Close() }()
	var out []BackupFile
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.EqualFold(filepath.Ext(f.Name), ".xml") {
			continue
		}
		if f.Flags&0x1 == 0x1 {
			return nil, ErrEncryptedBackup
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry %s: %w", f.Name, err)
		}
		kind, ok := classifyXML(rc)
		_ = rc.Close()
		if !ok {
			continue
		}
		name := f.Name
		out = append(out, BackupFile{
			Name: name,
			Kind: kind,
			Size: int64(f.UncompressedSize64), //nolint:gosec // zip entry size; can't realistically exceed int64
			Opener: func() (io.ReadCloser, error) {
				zr, err := zip.OpenReader(path)
				if err != nil {
					return nil, err
				}
				for _, entry := range zr.File {
					if entry.Name == name {
						rc, err := entry.Open()
						if err != nil {
							_ = zr.Close()
							return nil, err
						}
						return zipEntryReadCloser{ReadCloser: rc, closeZip: zr.Close}, nil
					}
				}
				_ = zr.Close()
				return nil, fmt.Errorf("zip entry %s not found", name)
			},
		})
	}
	sortBackupFiles(out)
	return out, nil
}

type zipEntryReadCloser struct {
	io.ReadCloser

	closeZip func() error
}

func (z zipEntryReadCloser) Close() error {
	err1 := z.ReadCloser.Close()
	err2 := z.closeZip()
	if err1 != nil {
		return err1
	}
	return err2
}

func classifyXML(r io.Reader) (BackupKind, bool) {
	dec := xml.NewDecoder(r)
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "smses":
			return KindMessages, true
		case "calls":
			return KindCalls, true
		default:
			return "", false
		}
	}
}

func sortBackupFiles(files []BackupFile) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].Kind != files[j].Kind {
			return files[i].Kind < files[j].Kind
		}
		return files[i].Name < files[j].Name
	})
}
