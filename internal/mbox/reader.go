// Package mbox implements a streaming reader for MBOX files.
//
// We support typical mboxo/mboxrd exports where each message is preceded by a
// Unix "From " separator line. Body lines that begin with "From " (or with one
// or more leading '>' followed by "From ") are commonly escaped in the file by
// prefixing an additional '>' (mboxrd). When reading, we unescape by removing a
// single leading '>' from any line that matches ^>+From . This can mutate
// literal ">From " lines in pure mboxo exports; call (*Reader).SetUnescapeFrom(false)
// to disable unescaping if needed.
package mbox

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

const maxLineBytes = 32 << 20 // 32 MiB

var ErrMessageTooLarge = errors.New("mbox message exceeds max size")

// Message is a single message from an MBOX file.
type Message struct {
	// FromLine is the separator line (without trailing newline).
	FromLine string

	// Raw is the RFC 5322 message bytes (headers + body). The separator line
	// is not included. Lines may use either LF or CRLF endings depending on
	// the source file.
	Raw []byte
}

type offsetReader struct {
	r io.Reader
	n int64
}

func (o *offsetReader) Read(p []byte) (int, error) {
	n, err := o.r.Read(p)
	o.n += int64(n)
	return n, err
}

// Reader reads messages from an MBOX stream.
// It is safe for large files: it reads one message at a time.
type Reader struct {
	or *offsetReader
	br *bufio.Reader

	// nextFromLine is the already-read separator line for the next message, if any.
	nextFromLine   string
	nextFromOffset int64
	hasNextFrom    bool
	eof            bool

	maxMessageBytes int64
	unescapeFrom    bool
}

// NewReader creates a new MBOX reader.
func NewReader(r io.Reader) *Reader {
	or := &offsetReader{r: r}
	// If the underlying reader is seekable (e.g. *os.File), initialize the counter
	// from the current position so offsets remain absolute after a prior Seek().
	if s, ok := r.(io.Seeker); ok {
		if off, err := s.Seek(0, io.SeekCurrent); err == nil {
			or.n = off
		}
	}
	return &Reader{
		or:           or,
		br:           bufio.NewReader(or),
		unescapeFrom: true,
	}
}

// NewReaderWithMaxMessageBytes creates a new MBOX reader that rejects messages
// larger than maxMessageBytes. If maxMessageBytes <= 0, no limit is enforced.
func NewReaderWithMaxMessageBytes(r io.Reader, maxMessageBytes int64) *Reader {
	rd := NewReader(r)
	rd.maxMessageBytes = maxMessageBytes
	return rd
}

// SetUnescapeFrom controls whether the reader performs mboxrd-style unescaping
// of ^>+From  lines. The default is true.
func (r *Reader) SetUnescapeFrom(enabled bool) {
	r.unescapeFrom = enabled
}

// Offset reports the current logical read offset (bytes consumed) within the
// underlying stream, accounting for buffered data.
func (r *Reader) Offset() int64 {
	return r.or.n - int64(r.br.Buffered())
}

// NextFromOffset reports the stream offset of the next message's "From " line.
// Valid only after a successful Next() call (or Offset() at end-of-file).
func (r *Reader) NextFromOffset() int64 {
	if r.hasNextFrom {
		return r.nextFromOffset
	}
	// If there is no buffered "From " line, the next message would start at the
	// current offset (either EOF or we haven't found the first separator yet).
	return r.Offset()
}

// Next returns the next message from the MBOX stream.
// Returns io.EOF when there are no more messages.
func (r *Reader) Next() (*Message, error) {
	if r.eof {
		return nil, io.EOF
	}

	// Find the separator line for the next message, if we don't already have it.
	if !r.hasNextFrom {
		for {
			lineStart := r.Offset()
			line, err := r.readLineBytes()
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if isFromSeparatorLine(line) {
				r.nextFromLine = string(bytes.TrimRight(line, "\r\n"))
				r.nextFromOffset = lineStart
				r.hasNextFrom = true
				break
			}
			if errors.Is(err, io.EOF) {
				r.eof = true
				return nil, io.EOF
			}
		}
	}

	// Consume the next separator and start collecting this message.
	fromLine := r.nextFromLine
	r.hasNextFrom = false

	var raw bytes.Buffer
	var rawBytes int64
	tooLarge := false

	for {
		lineStart := r.Offset()
		line, err := r.readLineBytes()
		if len(line) > 0 {
			if isFromSeparatorLine(line) {
				// Found the next message separator; stash it for the next call.
				r.nextFromLine = string(bytes.TrimRight(line, "\r\n"))
				r.nextFromOffset = lineStart
				r.hasNextFrom = true
				break
			}

			if !tooLarge {
				b := line
				if r.unescapeFrom {
					b = unescapeFromBytes(line)
				}
				if r.maxMessageBytes > 0 && rawBytes+int64(len(b)) > r.maxMessageBytes {
					tooLarge = true
				} else {
					raw.Write(b)
					rawBytes += int64(len(b))
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				r.eof = true
				break
			}
			return nil, err
		}
	}

	if tooLarge {
		return nil, fmt.Errorf("%w: limit %d bytes", ErrMessageTooLarge, r.maxMessageBytes)
	}

	// Empty message bodies are unusual but possible; return them anyway.
	return &Message{
		FromLine: fromLine,
		Raw:      raw.Bytes(),
	}, nil
}

func (r *Reader) readLineBytes() ([]byte, error) {
	// ReadBytes returns bufio.ErrBufferFull when the buffer fills before finding
	// the delimiter. Treat that as a partial line and keep accumulating.
	var out []byte
	for {
		b, err := r.br.ReadBytes('\n')
		out = append(out, b...)
		if len(out) > maxLineBytes {
			return nil, fmt.Errorf("mbox line exceeds max length (%d bytes)", maxLineBytes)
		}
		if err == nil {
			return out, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err == io.EOF {
			return out, io.EOF
		}
		if len(out) > 0 {
			return out, err
		}
		return nil, err
	}
}

var fromPrefix = []byte("From ")

// isFromSeparatorLine checks whether line (with or without trailing newline)
// looks like an mbox "From " separator. Works for both string and []byte
// callers by accepting []byte and converting only the trimmed portion.
func isFromSeparatorLine(line []byte) bool {
	if !bytes.HasPrefix(line, fromPrefix) {
		return false
	}
	trimmed := string(bytes.TrimRight(line, "\r\n"))
	_, ok := ParseFromSeparatorDate(trimmed)
	return ok
}

// unescapeFromBytes removes a single leading '>' from any line that matches
// ^>+From  (mboxrd unquoting). This also works for mboxo where only ">From "
// appears for originally "From " lines.
func unescapeFromBytes(line []byte) []byte {
	if len(line) == 0 || line[0] != '>' {
		return line
	}

	// Count leading '>' characters.
	i := 0
	for i < len(line) && line[i] == '>' {
		i++
	}
	// Check if the remainder begins with "From ".
	if i < len(line) && bytes.HasPrefix(line[i:], []byte("From ")) {
		return line[1:]
	}
	return line
}

// Validate scans the stream and returns an error if it doesn't look like an MBOX file.
// It reads up to maxBytes from the stream. This is a heuristic.
func Validate(r io.Reader, maxBytes int64) error {
	if maxBytes <= 0 {
		return errors.New("maxBytes must be > 0")
	}
	br := bufio.NewReader(io.LimitReader(r, maxBytes))
	for {
		line, err := br.ReadString('\n')
		if isFromSeparatorLine([]byte(line)) {
			return nil
		}
		if err != nil {
			if err == io.EOF {
				return errors.New("no \"From \" separators found (not an mbox file?)")
			}
			return err
		}
	}
}
