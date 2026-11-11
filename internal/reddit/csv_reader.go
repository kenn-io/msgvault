package reddit

import (
	"bufio"
	"encoding/csv"
	"errors"
	"io"
)

const maxCSVRecordBytes = 8 << 20

type csvRecordReader struct {
	*csv.Reader
	input      *bufio.Reader
	limit      *io.LimitedReader
	blankLines int
}

func newCSVRecordReader(input io.Reader) *csvRecordReader {
	limit := &io.LimitedReader{R: input}
	buffer := bufio.NewReader(limit)
	return &csvRecordReader{Reader: csv.NewReader(buffer), input: buffer, limit: limit}
}

func (r *csvRecordReader) Read() ([]string, error) {
	// Skip the same leading blank lines as encoding/csv outside the record budget.
	r.limit.N = maxCSVRecordBytes + 1
	for {
		prefix, err := r.input.Peek(1)
		if err != nil {
			return nil, err
		}
		if prefix[0] == '\r' {
			prefix, err = r.input.Peek(2)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
		}
		if prefix[len(prefix)-1] != '\n' {
			break
		}
		_, _ = r.input.Discard(len(prefix))
		r.blankLines++
		// Refills used for leading blank lines do not spend the record budget.
		r.limit.N = maxCSVRecordBytes + 1
	}
	// encoding/csv shares input and may already have bytes from the next record.
	// Allow one extra byte to distinguish a truncated record from a valid EOF.
	r.limit.N = maxCSVRecordBytes + 1 - int64(r.input.Buffered())
	start := r.InputOffset()
	fields, err := r.Reader.Read()
	if r.InputOffset()-start > maxCSVRecordBytes {
		return nil, errors.New("CSV record exceeds 8 MiB")
	}
	if parseErr, ok := errors.AsType[*csv.ParseError](err); ok {
		parseErr.StartLine += r.blankLines
		parseErr.Line += r.blankLines
	}
	return fields, err
}
