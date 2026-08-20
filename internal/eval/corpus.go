package eval

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Qrels maps a query id to its document-id → relevance-grade judgments.
type Qrels map[string]map[string]int

// HasJudgments reports whether qid was judged at all in this qrels file.
//
// It exists because "no relevant documents" and "no judgments" are different
// facts that RelevantSet cannot tell apart: a topic whose every line grades
// rel=0 produces an empty relevant set, exactly like a qid the file never
// mentions. Conflating them drops the all-non-relevant topic from the run,
// which silently raises every macro average — the topic can only score zero,
// so excluding it removes a zero from the mean. TREC semantics are to score
// such a topic normally; only an unjudged qid has nothing to score against.
func (q Qrels) HasJudgments(qid string) bool {
	return len(q[qid]) > 0
}

// RelevantSet returns the set of doc ids judged relevant (grade >= 1) for qid.
// An empty result is ambiguous on its own — see HasJudgments.
func (q Qrels) RelevantSet(qid string) map[string]struct{} {
	out := make(map[string]struct{})
	for d, r := range q[qid] {
		if r >= 1 {
			out[d] = struct{}{}
		}
	}
	return out
}

// LoadStats records how a corpus file parsed. Both loaders skip lines they
// cannot understand, which is the right behaviour for real-world TREC files
// (comments, trailing junk) but makes a whole-file format mismatch silent: a
// three-column qrels file — no iteration column — parses to an empty-but-valid
// Qrels, and the only downstream symptom is "none of the topics had relevance
// judgments", which reads like an id mismatch rather than a format problem.
// Returning the counts lets a caller tell those two apart and say so.
//
// Blank lines are not counted at all, so Lines == Parsed + Skipped always
// holds.
type LoadStats struct {
	Path    string `json:"path,omitempty"`
	Lines   int    `json:"lines"`   // non-blank lines read
	Parsed  int    `json:"parsed"`  // lines that produced a record
	Skipped int    `json:"skipped"` // lines the format check rejected
}

// String renders the counts for a warning or error message.
func (s LoadStats) String() string {
	return fmt.Sprintf("%d lines, %d parsed, %d skipped", s.Lines, s.Parsed, s.Skipped)
}

// Suspect reports whether the file parsed badly enough to be worth telling the
// user about: nothing usable came out, or a skipped line outnumbered a parsed
// one. Either shape usually means the file is not in the format the loader
// expects, rather than merely containing a stray line.
func (s LoadStats) Suspect() bool {
	return s.Lines > 0 && (s.Parsed == 0 || s.Skipped > s.Parsed)
}

// LoadQrels reads TREC-format relevance judgments: whitespace-separated
// "<qid> <iter> <docid> <rel>" per line (the iter column is ignored). Lines
// with fewer than four fields, or a non-integer relevance, are skipped and
// counted in the returned LoadStats — see that type for why the count matters.
func LoadQrels(path string) (Qrels, LoadStats, error) {
	stats := LoadStats{Path: path}
	f, err := os.Open(path)
	if err != nil {
		return nil, stats, fmt.Errorf("open qrels: %w", err)
	}
	defer func() { _ = f.Close() }()

	q := Qrels{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		stats.Lines++
		fields := strings.Fields(line)
		if len(fields) < 4 {
			stats.Skipped++
			continue
		}
		rel, err := strconv.Atoi(fields[3])
		if err != nil {
			stats.Skipped++
			continue
		}
		qid, docid := fields[0], fields[2]
		if q[qid] == nil {
			q[qid] = make(map[string]int)
		}
		q[qid][docid] = rel
		stats.Parsed++
	}
	if err := sc.Err(); err != nil {
		return nil, stats, fmt.Errorf("read qrels: %w", err)
	}
	return q, stats, nil
}

// Topic is a query whose ID matches a qrels query id.
//
// Category is an optional free-form label for the question's shape — e.g.
// "pointed" (answerable from one message) versus "spanning" (requires
// synthesizing across several). The distinction matters because it decides
// which retrieval levers a benchmark can even see: a topic set made entirely
// of pointed questions is structurally blind to thread-level improvements.
// Empty for topics files that don't carry the column.
type Topic struct {
	ID       string
	Query    string
	Category string
}

// LoadTopics reads a tab-separated topics file:
// "<qid>\t<query text>[\t<category>]" per line. The third column is an
// optional query-category label (see Topic.Category); two-column files —
// the original format — load exactly as before, with Category empty.
// Blank lines are ignored; lines without a tab (or with an empty id or query)
// are skipped and counted in the returned LoadStats, so a space-separated file
// that would otherwise load as zero topics is diagnosable.
//
// A repeated query id is rejected outright, unlike a malformed line. The two
// are not the same kind of problem: a malformed line contributes nothing, so
// dropping and counting it leaves the run intact, whereas a repeated qid
// contributes twice. Every judgment for that qid would be applied once per
// occurrence, weighting the query two or more times in a macro average that is
// meant to be per-query — and when the repeated lines carry different query
// text, whichever policy picked a winner would silently answer one question
// and report it against the other. The qid is the join key to the qrels file,
// so it has to identify exactly one query; a caller cannot repair that
// ambiguity, only the file can.
func LoadTopics(path string) ([]Topic, LoadStats, error) {
	stats := LoadStats{Path: path}
	f, err := os.Open(path)
	if err != nil {
		return nil, stats, fmt.Errorf("open topics: %w", err)
	}
	defer func() { _ = f.Close() }()

	var topics []Topic
	seen := make(map[string]struct{})
	var duplicates []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		stats.Lines++
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			stats.Skipped++
			continue
		}
		id := strings.TrimSpace(parts[0])
		query := strings.TrimSpace(parts[1])
		if id == "" || query == "" {
			stats.Skipped++
			continue
		}
		category := ""
		if len(parts) == 3 {
			category = strings.TrimSpace(parts[2])
		}
		// Read the whole file before complaining, so one message names every
		// offending qid instead of sending the user round the loop once per
		// duplicate. Each is named once however many times it repeats.
		if _, repeat := seen[id]; repeat {
			if !slices.Contains(duplicates, id) {
				duplicates = append(duplicates, id)
			}
		}
		seen[id] = struct{}{}
		topics = append(topics, Topic{ID: id, Query: query, Category: category})
		stats.Parsed++
	}
	if err := sc.Err(); err != nil {
		return nil, stats, fmt.Errorf("read topics: %w", err)
	}
	if len(duplicates) > 0 {
		return nil, stats, fmt.Errorf("topics file %s repeats query id %s: a qid is the join key to "+
			"the qrels file, so a repeat scores the same judgments more than once and weights that "+
			"query several times over in every macro average — give each topic a unique id",
			path, FormatIDList(duplicates, 10))
	}
	return topics, stats, nil
}
