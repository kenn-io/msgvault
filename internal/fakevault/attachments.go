package fakevault

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// attachmentKind describes one class of generated attachment content. The
// compressible flag matters more than the label: it decides whether pack
// trial-compression sees already-compressed media (JPEG, PDF) or text-like
// bytes, which is what shapes backup pack sizes and CPU.
type attachmentKind struct {
	mimeType     string
	mediaType    string
	ext          string
	compressible bool
	thumbnails   bool
}

var attachmentKinds = []attachmentKind{
	{mimeType: "image/jpeg", mediaType: "image", ext: ".jpg", thumbnails: true},
	{mimeType: "application/pdf", mediaType: "document", ext: ".pdf"},
	{mimeType: "text/html", mediaType: "document", ext: ".html", compressible: true},
}

// drawKind picks image 55%, pdf 25%, html 20%.
func drawKind(r *rand.Rand) attachmentKind {
	switch u := r.Float64(); {
	case u < 0.55:
		return attachmentKinds[0]
	case u < 0.80:
		return attachmentKinds[1]
	default:
		return attachmentKinds[2]
	}
}

// drawAttachmentSize draws from a three-class mixture (70% 5–100KB, 25%
// 100KB–1MB, 5% 1–8MB) whose expected value is meanAttachmentBytes; the
// attachment rate calculation depends on that constant staying in sync.
func drawAttachmentSize(r *rand.Rand) int64 {
	switch u := r.Float64(); {
	case u < 0.70:
		return 5<<10 + r.Int64N(95<<10)
	case u < 0.95:
		return 100<<10 + r.Int64N(924<<10)
	default:
		return 1<<20 + r.Int64N(7<<20)
	}
}

// collectAttachments writes zero or more content blobs for one message and
// returns their refs; the caller inserts the rows after the message row
// exists (attachments.message_id is a foreign key). It stops attaching once
// the run's byte budget is spent, so the realized total tracks
// Options.AttachmentBytes.
func (g *generator) collectAttachments(idx int64, r *rand.Rand) ([]blobRef, error) {
	count := int(g.attRate)
	if r.Float64() < g.attRate-float64(count) {
		count++
	}
	var refs []blobRef
	for k := range count {
		if g.attBytes >= g.opts.AttachmentBytes {
			break
		}
		ref, err := g.attachmentBlob(idx<<3|int64(k), r)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// insertAttachmentRows inserts the attachment rows for refs, which
// collectAttachments produced before the message row was written.
func (g *generator) insertAttachmentRows(ctx context.Context, stmts *batchStmts,
	msgID, idx int64, refs []blobRef) error {
	for k, ref := range refs {
		var thumbHash, thumbPath any
		if ref.thumbHash != "" {
			thumbHash, thumbPath = ref.thumbHash, blobRelPath(ref.thumbHash)
		}
		if _, err := stmts.att.ExecContext(ctx, msgID,
			fmt.Sprintf("att-%d-%d%s", idx, k, ref.ext), ref.mimeType, ref.size,
			ref.hash, blobRelPath(ref.hash), ref.mediaType,
			thumbHash, thumbPath); err != nil {
			return fmt.Errorf("fakevault: inserting attachment for message %d: %w", msgID, err)
		}
	}
	return nil
}

// attachmentBlob returns the blob one attachment row references: usually
// fresh content, but 10% of the time a re-reference to an earlier blob so
// generated vaults exercise content-addressed deduplication the way real
// archives (forwarded attachments, repeated logos) do.
func (g *generator) attachmentBlob(contentIdx int64, r *rand.Rand) (blobRef, error) {
	if len(g.blobs) > 32 && r.Float64() < 0.10 {
		return g.blobs[r.IntN(len(g.blobs))], nil
	}
	ar := g.rng(streamAttachment, contentIdx)
	kind := drawKind(ar)
	size := drawAttachmentSize(ar)
	var content []byte
	if kind.compressible {
		content = compressibleBytes(ar, size)
	} else {
		content = incompressibleBytes(g.opts.Seed, streamAttachment, contentIdx, size)
	}
	ref := blobRef{
		hash: hashOf(content), size: size,
		mimeType: kind.mimeType, mediaType: kind.mediaType, ext: kind.ext,
	}
	if err := g.writeBlob(ref.hash, content); err != nil {
		return blobRef{}, err
	}
	if kind.thumbnails && ar.Float64() < 0.5 {
		thumb := incompressibleBytes(g.opts.Seed, streamThumb, contentIdx,
			2<<10+ar.Int64N(6<<10))
		ref.thumbHash = hashOf(thumb)
		if err := g.writeBlob(ref.thumbHash, thumb); err != nil {
			return blobRef{}, err
		}
	}
	g.blobs = append(g.blobs, ref)
	return ref, nil
}

// writeBlob stores content at the canonical <hash[:2]>/<hash> path, skipping
// the write when the content-addressed file already exists (intra-run dedup
// collisions and append runs over earlier output).
func (g *generator) writeBlob(hash string, content []byte) error {
	dir := filepath.Join(g.attDir, hash[:2])
	if !g.dirSeen[dir] {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("fakevault: creating attachment directory: %w", err)
		}
		g.dirSeen[dir] = true
	}
	path := filepath.Join(dir, hash)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("fakevault: checking attachment %s: %w", hash, err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("fakevault: writing attachment %s: %w", hash, err)
	}
	g.blobsNew++
	g.attBytes += int64(len(content))
	return nil
}

func blobRelPath(hash string) string { return hash[:2] + "/" + hash }

func hashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// incompressibleBytes returns size bytes of a deterministic ChaCha8 stream
// keyed by (seed, stream, index): statistically random, so zstd stores the
// frame raw — the behavior real JPEG/PDF/video content produces.
func incompressibleBytes(seed uint64, stream, index, size int64) []byte {
	var key [32]byte
	binary.LittleEndian.PutUint64(key[0:], seed)
	binary.LittleEndian.PutUint64(key[8:], uint64(stream)) //nolint:gosec // deterministic fake data
	binary.LittleEndian.PutUint64(key[16:], uint64(index)) //nolint:gosec // deterministic fake data
	src := rand.NewChaCha8(key)
	buf := make([]byte, size)
	_, _ = src.Read(buf) // ChaCha8.Read never fails
	return buf
}
