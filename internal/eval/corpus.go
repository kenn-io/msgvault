package eval

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Qrels maps a query id to its document-id → relevance-grade judgments.
type Qrels map[string]map[string]int

// RelevantSet returns the set of doc ids judged relevant (grade >= 1) for qid.
func (q Qrels) RelevantSet(qid string) map[string]struct{} {
	out := make(map[string]struct{})
	for d, r := range q[qid] {
		if r >= 1 {
			out[d] = struct{}{}
		}
	}
	return out
}

// LoadQrels reads TREC-format relevance judgments: whitespace-separated
// "<qid> <iter> <docid> <rel>" per line (the iter column is ignored). Lines
// with fewer than four fields, or a non-integer relevance, are skipped.
func LoadQrels(path string) (Qrels, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open qrels: %w", err)
	}
	defer func() { _ = f.Close() }()

	q := Qrels{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		rel, err := strconv.Atoi(fields[3])
		if err != nil {
			continue
		}
		qid, docid := fields[0], fields[2]
		if q[qid] == nil {
			q[qid] = make(map[string]int)
		}
		q[qid][docid] = rel
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read qrels: %w", err)
	}
	return q, nil
}

// Topic is a query whose ID matches a qrels query id.
type Topic struct {
	ID    string
	Query string
}

// LoadTopics reads a tab-separated topics file: "<qid>\t<query text>" per
// line. Blank lines and lines without a tab are skipped.
func LoadTopics(path string) ([]Topic, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open topics: %w", err)
	}
	defer func() { _ = f.Close() }()

	var topics []Topic
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) < 2 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		query := strings.TrimSpace(parts[1])
		if id == "" || query == "" {
			continue
		}
		topics = append(topics, Topic{ID: id, Query: query})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read topics: %w", err)
	}
	return topics, nil
}
