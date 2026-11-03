package metawebhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Counters struct {
	Accepted, Duplicates, SignatureFailures, TooLarge, Unavailable uint64
	LastAccepted                                                   time.Time
}
type ReceiverSnapshot struct {
	Listener, Addr, DisabledReason string
	Counters                       Counters
	WrittenAt                      time.Time
}

func WriteReceiverSnapshot(root string, snap ReceiverSnapshot) error {
	return writeJSON(filepath.Join(root, "receiver.json"), snap)
}
func ReadReceiverSnapshot(root string) (ReceiverSnapshot, error) {
	var snap ReceiverSnapshot
	err := readJSON(filepath.Join(root, "receiver.json"), &snap)
	return snap, err
}

type requeueRequest struct {
	Filter RequeueFilter
	DryRun bool
}
type requeueResult struct {
	Count  int
	Failed bool
}

func requestIDOK(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func SubmitRequeue(root string, f RequeueFilter, dryRun bool) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate replay request ID: %w", err)
	}
	id := hex.EncodeToString(random[:])
	err := writeJSON(filepath.Join(root, "requests", id+".json"), requeueRequest{f, dryRun})
	return id, err
}
func AwaitRequeueResult(ctx context.Context, root, id string) (int, error) {
	if !requestIDOK(id) {
		return 0, errors.New("invalid replay request ID")
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		path := filepath.Join(root, "requests", id+".result.json")
		var result requeueResult
		err := readJSON(path, &result)
		if err == nil {
			if result.Failed {
				return 0, errors.New("webhook replay request failed")
			}
			return result.Count, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Spool) ConsumeRequests() error {
	folder := filepath.Join(s.dir, "requests")
	files, err := os.ReadDir(folder)
	if err != nil {
		return err
	}
	for _, file := range files {
		id := strings.TrimSuffix(file.Name(), ".json")
		if !requestIDOK(id) {
			continue
		}
		path := filepath.Join(folder, file.Name())
		var request requeueRequest
		if err = readJSON(path, &request); err != nil {
			return err
		}
		n, requeueErr := s.Requeue(request.Filter, request.DryRun)
		if err = writeJSON(filepath.Join(folder, id+".result.json"), requeueResult{n, requeueErr != nil}); err != nil {
			return err
		}
		if err = os.Remove(path); err != nil {
			return err
		}
		if err = syncDir(folder); err != nil {
			return err
		}
	}
	return nil
}
