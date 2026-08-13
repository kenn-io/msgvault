package mistral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
)

// LoadProbeFixtures validates and privately spools the complete capability
// matrix. Fixture files are named by candidate ID without an extension (for
// example, "pdf", "docx", and "xlsx"). Paths never enter the returned
// documents or capability manifest.
func LoadProbeFixtures(
	ctx context.Context,
	fixtureDirectory string,
	maxBytes int64,
) (documents map[string]Document, cleanup func() error, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if fixtureDirectory == "" || maxBytes <= 0 {
		return nil, nil, errors.New("mistral capability fixtures require a directory and positive byte limit")
	}
	if maxBytes > math.MaxInt64/int64(len(candidateFormats)) {
		return nil, nil, errors.New("mistral capability fixture spool quota would overflow")
	}
	maxSpoolBytes := maxBytes * int64(len(candidateFormats))
	directoryInfo, err := os.Lstat(fixtureDirectory)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect mistral capability fixture directory: %w", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("mistral capability fixture path must be a real directory")
	}

	spoolDirectory, err := os.MkdirTemp("", "msgvault-mistral-probe-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create mistral capability spool directory: %w", err)
	}
	if err := os.Chmod(spoolDirectory, 0o700); err != nil { // #nosec G302 -- this is a private directory, not a file.
		_ = os.Remove(spoolDirectory)
		return nil, nil, fmt.Errorf("secure mistral capability spool directory: %w", err)
	}

	cleanups := make([]func() error, 0, len(candidateFormats))
	cleanup = func() error {
		var cleanupErrors []error
		for _, cleanupFixture := range slices.Backward(cleanups) {
			if cleanupErr := cleanupFixture(); cleanupErr != nil {
				cleanupErrors = append(cleanupErrors, cleanupErr)
			}
		}
		if removeErr := os.Remove(spoolDirectory); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove mistral capability spool directory: %w", removeErr))
		}
		return errors.Join(cleanupErrors...)
	}
	fail := func(loadErr error) error {
		return errors.Join(loadErr, cleanup())
	}

	documents = make(map[string]Document, len(candidateFormats))
	for _, candidate := range candidateFormats {
		if err := ctx.Err(); err != nil {
			return nil, nil, fail(err)
		}
		fixturePath := filepath.Join(fixtureDirectory, candidate.ID)
		document, documentCleanup, loadErr := loadProbeFixture(
			ctx, fixturePath, spoolDirectory, candidate, maxBytes, maxSpoolBytes,
		)
		if loadErr != nil {
			return nil, nil, fail(fmt.Errorf("load mistral capability fixture %q: %w", candidate.ID, loadErr))
		}
		cleanups = append(cleanups, documentCleanup)
		documents[candidate.ID] = document
	}
	return documents, cleanup, nil
}

func loadProbeFixture(
	ctx context.Context,
	fixturePath string,
	spoolDirectory string,
	candidate CandidateFormat,
	maxBytes int64,
	maxSpoolBytes int64,
) (Document, func() error, error) {
	pathInfo, err := os.Lstat(fixturePath)
	if err != nil {
		return Document{}, nil, fmt.Errorf("inspect fixture: %w", err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 {
		return Document{}, nil, errors.New("fixture must be a regular non-symlink file")
	}
	if pathInfo.Size() <= 0 || pathInfo.Size() > maxBytes {
		return Document{}, nil, errors.New("fixture size is outside configured bounds")
	}

	file, err := os.Open(fixturePath)
	if err != nil {
		return Document{}, nil, fmt.Errorf("open fixture: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return Document{}, nil, fmt.Errorf("inspect opened fixture: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		_ = file.Close()
		return Document{}, nil, errors.New("fixture changed while opening")
	}

	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(&contextReader{ctx: ctx, reader: file}, maxBytes+1))
	if err != nil {
		_ = file.Close()
		return Document{}, nil, fmt.Errorf("hash fixture: %w", err)
	}
	if written != openedInfo.Size() || written > maxBytes {
		_ = file.Close()
		return Document{}, nil, errors.New("fixture changed or exceeded bounds while hashing")
	}
	detected, err := DetectFormat(file, written, candidate.MediaType)
	if err != nil {
		_ = file.Close()
		return Document{}, nil, fmt.Errorf("validate fixture container: %w", err)
	}
	if detected.ID != candidate.ID {
		_ = file.Close()
		return Document{}, nil, fmt.Errorf(
			"validate fixture container: detected %q, want %q", detected.ID, candidate.ID,
		)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return Document{}, nil, fmt.Errorf("rewind fixture: %w", err)
	}

	document, cleanup, err := SpoolVerifiedSource(ctx, file, SpoolOptions{
		Directory:      spoolDirectory,
		MediaType:      candidate.MediaType,
		ExpectedSize:   written,
		ExpectedSHA256: hex.EncodeToString(hash.Sum(nil)),
		MaxBytes:       maxBytes,
		MaxSpoolBytes:  maxSpoolBytes,
		MinFreeBytes:   maxBytes,
	})
	if err != nil {
		return Document{}, nil, err
	}
	return document, cleanup, nil
}
