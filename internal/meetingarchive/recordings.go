package meetingarchive

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

const DefaultRecordingMaxBytes int64 = 4 << 30
const recordingQuietPeriod = 5 * time.Second

var ErrRecordingTooLarge = errors.New("recording exceeds size cap; increase recording_max_bytes and the daemon recording cap, then sync again")
var ErrRecordingValidation = errors.New("recording metadata does not match its media")

var ErrRecordingChanged = errors.New("recording changed or transfer was incomplete; sync again")

// Recording is a private snapshot of completed retained audio. Close removes it.
type Recording struct {
	File     *os.File
	Hash     string
	Size     int64
	MIMEType string
}

func (r *Recording) Close() error { return errors.Join(r.File.Close(), os.Remove(r.File.Name())) }

// PrepareRecording confines savedPath to base. Missing, recent, non-regular, incomplete and
// unreadable files are deferred, without blocking transcript sync. No path
// or source filename leaves the recorder. The snapshot prevents a resumed
// recording from changing the bytes of an in-flight upload.
//
//nolint:nilnil // Optional unavailable or unfinished audio is deferred without failing meeting sync.
func PrepareRecording(ctx context.Context, base, savedPath string, maxBytes int64) (*Recording, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if savedPath == "" {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = DefaultRecordingMaxBytes
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return nil, nil
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return nil, nil
	}
	target := savedPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return nil, nil
	}
	targetDir, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return nil, nil
	}
	target = filepath.Join(targetDir, filepath.Base(target))
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, nil
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = root.Close() }()
	before, err := root.Lstat(rel)
	if err != nil || !before.Mode().IsRegular() || before.Size() == 0 || time.Since(before.ModTime()) < recordingQuietPeriod {
		return nil, nil
	}
	if before.Size() > maxBytes {
		return nil, ErrRecordingTooLarge
	}
	source, err := root.Open(rel)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = source.Close() }()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, nil
	}
	mediaType, err := recordingMediaType(source, before.Size())
	if err != nil {
		return nil, nil
	}
	snapshot, err := os.CreateTemp("", "msgvault-meeting-recording-*")
	if err != nil {
		return nil, fmt.Errorf("create recording snapshot: %w", err)
	}
	r := &Recording{File: snapshot, MIMEType: mediaType}
	keep := false
	defer func() {
		if !keep {
			_ = r.Close()
		}
	}()
	h := sha256.New()
	sourceReader := &export.SourceReader{R: &contextReader{ctx: ctx, r: io.LimitReader(source, maxBytes+1)}}
	size, err := io.Copy(io.MultiWriter(snapshot, h), sourceReader)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if size > maxBytes {
		return nil, ErrRecordingTooLarge
	}
	if err != nil && sourceReader.Err == nil {
		return nil, fmt.Errorf("write recording snapshot: %w", err)
	}
	after, statErr := root.Lstat(rel)
	if err != nil || statErr != nil || !os.SameFile(before, after) || before.Size() != size || after.Size() != size || !before.ModTime().Equal(after.ModTime()) {
		return nil, nil
	}
	r.Hash, r.Size = hex.EncodeToString(h.Sum(nil)), size
	if _, err := snapshot.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind recording snapshot: %w", err)
	}
	keep = true
	return r, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// recordingMediaType checks signatures and complete container lengths without
// decoding or loading audio samples. Muesli writes RIFF/WAVE or M4A-branded
// ISO base media files. A missing moov or a short atom defers a partial M4A.
func recordingMediaType(file *os.File, size int64) (string, error) {
	mediaType, err := recordingSignature(bufio.NewReader(io.NewSectionReader(file, 0, size)), size)
	if err != nil || mediaType == "audio/wav" {
		return mediaType, err
	}
	moov, mdat := false, false
	for pos, count := int64(0), 0; pos < size; count++ {
		if count >= 10000 {
			return "", ErrRecordingChanged
		}
		var atom [16]byte
		if _, err := file.ReadAt(atom[:8], pos); err != nil {
			return "", err
		}
		length, head := int64(binary.BigEndian.Uint32(atom[:4])), int64(8)
		if length == 1 {
			if _, err := file.ReadAt(atom[8:], pos+8); err != nil {
				return "", err
			}
			wide := binary.BigEndian.Uint64(atom[8:])
			if wide > math.MaxInt64 {
				return "", ErrRecordingChanged
			}
			length, head = int64(wide), 16
		}
		if length < head || length > size-pos {
			return "", ErrRecordingChanged
		}
		moov = moov || string(atom[4:8]) == "moov"
		mdat = mdat || string(atom[4:8]) == "mdat"
		pos += length
	}
	if !moov || !mdat {
		return "", ErrRecordingChanged
	}
	return "audio/mp4", nil
}

// recordingSignature accepts an M4A major or compatible brand. Other BMFF
// containers may contain video, so an ftyp box alone does not identify audio.
func recordingSignature(reader *bufio.Reader, size int64) (string, error) {
	header, err := reader.Peek(12)
	if err != nil {
		return "", ErrRecordingChanged
	}
	if string(header[:4]) == "RIFF" && string(header[8:12]) == "WAVE" {
		if int64(binary.LittleEndian.Uint32(header[4:8]))+8 != size {
			return "", ErrRecordingChanged
		}
		return "audio/wav", nil
	}
	length := int64(binary.BigEndian.Uint32(header[:4]))
	if string(header[4:8]) != "ftyp" || length < 16 || length > 4096 || length > size || length%4 != 0 {
		return "", ErrRecordingChanged
	}
	brands, err := reader.Peek(int(length))
	if err != nil {
		return "", ErrRecordingChanged
	}
	if string(brands[8:12]) == "M4A " {
		return "audio/mp4", nil
	}
	for pos := 16; pos < len(brands); pos += 4 {
		if string(brands[pos:pos+4]) == "M4A " {
			return "audio/mp4", nil
		}
	}
	return "", ErrRecordingChanged
}

type recordingReader struct {
	r              io.Reader
	h              hash.Hash
	wantHash       string
	wantSize, size int64
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.size += int64(n)
	_, _ = r.h.Write(p[:n])
	if errors.Is(err, io.EOF) && (r.size != r.wantSize || hex.EncodeToString(r.h.Sum(nil)) != r.wantHash) {
		return n, ErrRecordingChanged
	}
	return n, err
}

func StageRecordingUpload(ctx context.Context, hashValue string, sizeClaim int64, mimeType string, input io.Reader, maxBytes int64) (*Recording, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultRecordingMaxBytes
	}
	if sizeClaim > maxBytes {
		return nil, ErrRecordingTooLarge
	}
	if input == nil {
		return nil, ErrRecordingChanged
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	snapshot, err := os.CreateTemp("", "msgvault-meeting-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create recording upload snapshot: %w", err)
	}
	recording := &Recording{File: snapshot}
	keep := false
	defer func() {
		if !keep {
			_ = recording.Close()
		}
	}()

	limit := maxBytes
	if limit < math.MaxInt64 {
		limit++
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(snapshot, h), &contextReader{ctx: ctx, r: io.LimitReader(input, limit)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if size > maxBytes {
		return nil, ErrRecordingTooLarge
	}
	if err != nil {
		return nil, fmt.Errorf("stage recording upload: %w", err)
	}
	if size != sizeClaim || hex.EncodeToString(h.Sum(nil)) != hashValue {
		return nil, ErrRecordingChanged
	}
	mediaType, err := recordingMediaType(snapshot, size)
	if err != nil {
		return nil, err
	}
	if mediaType != mimeType {
		return nil, ErrRecordingValidation
	}
	if _, err := snapshot.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind recording upload snapshot: %w", err)
	}
	recording.Hash, recording.Size, recording.MIMEType = hashValue, size, mediaType
	keep = true
	return recording, nil
}

func StoreRecording(ctx context.Context, st *store.Store, partKey, filename string, messageID int64, dir string, r *Recording) (bool, error) {
	mediaType, err := recordingMediaType(r.File, r.Size)
	if err != nil {
		return false, err
	}
	if mediaType != r.MIMEType {
		return false, ErrRecordingChanged
	}
	path, gotHash, gotSize, err := export.StoreAttachmentFromPathVerified(ctx, dir, r.File.Name(), r.Hash, r.Size, r.Size)
	if errors.Is(err, export.ErrAttachmentSourceChanged) {
		return false, ErrRecordingChanged
	}
	if err != nil {
		return false, err
	}
	return recordStoredRecording(ctx, st, partKey, filename, messageID, path, gotHash, gotSize, mediaType)
}

func StoreRecordingStream(ctx context.Context, st *store.Store, partKey, filename string, messageID int64, dir, hashValue string, size int64, mediaType string, input io.Reader) (bool, error) {
	// Read a small signature before writing CAS. Hash and byte-count validation
	// at EOF abort publication when the transfer is truncated or changed.
	buffered := bufio.NewReader(input)
	detected, err := recordingSignature(buffered, size)
	if err != nil {
		return false, err
	}
	if detected != mediaType {
		return false, ErrRecordingValidation
	}
	checked := &recordingReader{r: buffered, h: sha256.New(), wantHash: hashValue, wantSize: size}
	path, gotHash, gotSize, err := export.StoreAttachmentStream(ctx, dir, checked, size)
	if errors.Is(err, export.ErrAttachmentTooLarge) {
		return false, ErrRecordingChanged
	}
	if err != nil {
		return false, err
	}
	return recordStoredRecording(ctx, st, partKey, filename, messageID, path, gotHash, gotSize, mediaType)
}

func recordStoredRecording(ctx context.Context, st *store.Store, partKey, filename string, messageID int64, path, gotHash string, gotSize int64, mediaType string) (bool, error) {
	attachments, err := st.MessageAttachmentsByPartKeyContext(ctx, messageID, partKey)
	if err != nil {
		return false, err
	}
	for _, att := range attachments {
		if att.ContentHash == gotHash && att.MimeType == mediaType {
			return false, nil
		}
	}
	write := store.AttachmentWrite{Filename: filename, MIMEType: mediaType,
		StoragePath: path, ContentHash: gotHash, Size: gotSize, MediaType: "audio", SourcePartKey: partKey,
		SourceAttachmentID: partKey, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		State: attachmentpolicy.StateStored}
	if err := st.UpsertAttachmentRecordWithStats(ctx, messageID, write, true); err != nil {
		return false, err
	}
	return true, nil
}
