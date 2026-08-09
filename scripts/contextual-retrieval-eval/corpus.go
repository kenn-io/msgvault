//go:build sqlite_vec

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

const (
	distractorSeed   int64 = 55020260808
	familyChat             = "chat"
	familyTranscript       = "transcript"
	familyEmail            = "email"
)

var scenarioIDPattern = regexp.MustCompile(`^(chat|transcript|email)-[0-9]{3}$`)

// Scenario is one frozen information need. The source documents are derived
// from its stable synthetic identity so no user or provider data enters the
// public evaluator corpus.
type Scenario struct {
	ID             string `json:"id"`
	Family         string `json:"family"`
	Query          string `json:"query"`
	ContextOnly    bool   `json:"context_only"`
	PositiveID     string `json:"positive_id"`
	HardNegativeID string `json:"hard_negative_id"`
}

// Judgment freezes grades independently from the generated document text.
// EvidenceIDs marks transcript chunks that contain the cited answer span.
type Judgment struct {
	ScenarioID   string         `json:"scenario_id"`
	Grades       map[string]int `json:"grades"`
	EvidenceIDs  []string       `json:"evidence_ids,omitempty"`
	EvidenceRefs []EvidenceRef  `json:"evidence_refs,omitempty"`
}

type EvidenceRef struct {
	SourceID string `json:"source_id"`
	RawStart int    `json:"raw_start"`
	RawEnd   int    `json:"raw_end"`
}

// SourceDocument is the evaluator's synthetic source record. OldText is the
// current production source. StructuredChunks are byte-identical in S-c4 and
// N-c4; DocumentID controls only the N-c4 grouping boundary.
type SourceDocument struct {
	ID                    string
	MessageID             int64
	Family                string
	DocumentID            string
	Subject               string
	Body                  string
	OldChunks             []string
	StructuredChunks      []string
	StructuredChunkIDs    []string
	StructuredChunkStarts []int
	StructuredChunkEnds   []int
	StructuredChunkBases  []vector.SourceBasis
}

func (d SourceDocument) StructuredChunkText(id string) string {
	for i, chunkID := range d.StructuredChunkIDs {
		if chunkID == id && i < len(d.StructuredChunks) {
			return d.StructuredChunks[i]
		}
	}
	return ""
}

// Corpus is the complete deterministic evaluation input.
type Corpus struct {
	Scenarios   []Scenario
	Judgments   map[string]Judgment
	Distractors []SourceDocument
}

func loadCorpus(scenarioPath, judgmentPath string, distractorCount int) (Corpus, error) {
	var scenarios []Scenario
	if err := readJSONL(scenarioPath, func(line []byte) error {
		var scenario Scenario
		if err := json.Unmarshal(line, &scenario); err != nil {
			return err
		}
		scenarios = append(scenarios, scenario)
		return nil
	}); err != nil {
		return Corpus{}, fmt.Errorf("load scenarios: %w", err)
	}

	judgments := make(map[string]Judgment)
	if err := readJSONL(judgmentPath, func(line []byte) error {
		var judgment Judgment
		if err := json.Unmarshal(line, &judgment); err != nil {
			return err
		}
		if _, exists := judgments[judgment.ScenarioID]; exists {
			return fmt.Errorf("duplicate judgment %q", judgment.ScenarioID)
		}
		judgments[judgment.ScenarioID] = judgment
		return nil
	}); err != nil {
		return Corpus{}, fmt.Errorf("load judgments: %w", err)
	}
	if distractorCount < 0 {
		return Corpus{}, errors.New("distractor count must be non-negative")
	}
	corpus := Corpus{Scenarios: scenarios, Judgments: judgments, Distractors: generateDistractors(distractorCount)}
	if err := corpus.Validate(); err != nil {
		return Corpus{}, err
	}
	return corpus, nil
}

func readJSONL(path string, consume func([]byte) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		if err := consume(scanner.Bytes()); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
	}
	return scanner.Err()
}

func (c Corpus) CountFamily(family string) int {
	count := 0
	for _, scenario := range c.Scenarios {
		if scenario.Family == family {
			count++
		}
	}
	return count
}

func (c Corpus) ContextOnlyCount(family string) int {
	count := 0
	for _, scenario := range c.Scenarios {
		if scenario.Family == family && scenario.ContextOnly {
			count++
		}
	}
	return count
}

func (c Corpus) DistractorCount() int { return len(c.Distractors) }

func (c Corpus) Validate() error {
	if len(c.Scenarios) == 0 {
		return errors.New("corpus has no scenarios")
	}
	seen := make(map[string]struct{}, len(c.Scenarios))
	for _, scenario := range c.Scenarios {
		if !scenarioIDPattern.MatchString(scenario.ID) {
			return fmt.Errorf("invalid synthetic scenario ID %q", scenario.ID)
		}
		if _, exists := seen[scenario.ID]; exists {
			return fmt.Errorf("duplicate scenario ID %q", scenario.ID)
		}
		seen[scenario.ID] = struct{}{}
		if strings.TrimSpace(scenario.Query) == "" {
			return fmt.Errorf("scenario %q has blank query", scenario.ID)
		}
		if strings.Contains(scenario.Query, "@") || strings.Contains(strings.ToLower(scenario.Query), "gmail") {
			return fmt.Errorf("scenario %q contains email-like PII", scenario.ID)
		}
		judgment, ok := c.Judgments[scenario.ID]
		if !ok {
			return fmt.Errorf("scenario %q has no judgment", scenario.ID)
		}
		if judgment.Grades[scenario.PositiveID] < 1 {
			return fmt.Errorf("scenario %q positive is not graded relevant", scenario.ID)
		}
		if _, ok := judgment.Grades[scenario.HardNegativeID]; !ok {
			return fmt.Errorf("scenario %q has no matched hard-negative grade", scenario.ID)
		}
	}
	if len(c.Judgments) != len(c.Scenarios) {
		return fmt.Errorf("judgment count %d does not match scenario count %d", len(c.Judgments), len(c.Scenarios))
	}
	return nil
}

func (c Corpus) Hash() string {
	hash := sha256.New()
	for _, scenario := range c.Scenarios {
		payload := mustMarshalHashInput(scenario)
		hash.Write(payload)
		hash.Write([]byte{'\n'})
		judgment := c.Judgments[scenario.ID]
		payload = mustMarshalHashInput(judgment)
		hash.Write(payload)
		hash.Write([]byte{'\n'})
	}
	for _, distractor := range c.Distractors {
		hash.Write([]byte(distractor.ID))
		hash.Write([]byte{0})
		hash.Write([]byte(distractor.Body))
		hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (c Corpus) QueryHash() string {
	hash := sha256.New()
	for _, scenario := range c.Scenarios {
		hash.Write([]byte(scenario.ID))
		hash.Write([]byte{0})
		hash.Write([]byte(scenario.Query))
		hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func materializedCorpusHash(corpus Corpus, sources []SourceDocument, policyFingerprint string) string {
	hash := sha256.New()
	hash.Write([]byte("contextual-eval-materialized-v1\n"))
	hash.Write([]byte(corpus.Hash()))
	hash.Write([]byte{'\n'})
	hash.Write([]byte(policyFingerprint))
	hash.Write([]byte{'\n'})
	ordered := append([]SourceDocument(nil), sources...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ID == ordered[j].ID {
			return ordered[i].MessageID < ordered[j].MessageID
		}
		return ordered[i].ID < ordered[j].ID
	})
	for _, source := range ordered {
		payload := mustMarshalHashInput(source)
		hash.Write(payload)
		hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func mustMarshalHashInput(value any) []byte {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("marshal contextual evaluation hash input: %w", err))
	}
	return payload
}

func generateDistractors(count int) []SourceDocument {
	// #nosec G404 -- the fixed-seed generator is intentionally deterministic for a frozen evaluation corpus.
	rng := rand.New(rand.NewSource(distractorSeed))
	nouns := []string{"harbor", "meadow", "lantern", "orbit", "ridge", "canvas", "willow", "delta"}
	verbs := []string{"reviewed", "sorted", "measured", "archived", "mapped", "scheduled", "tested", "counted"}
	documents := make([]SourceDocument, 0, count)
	for i := range count {
		id := fmt.Sprintf("distractor-%05d", i)
		body := fmt.Sprintf("Synthetic noise record %05d: team %s %s item %04d.", i,
			nouns[rng.Intn(len(nouns))], verbs[rng.Intn(len(verbs))], rng.Intn(10000))
		old, _ := embed.Preprocess("Synthetic archive note", body, 0, productionPreprocessConfig())
		documents = append(documents, SourceDocument{
			ID: id, MessageID: 1_000_000 + int64(i), Family: "distractor", DocumentID: id,
			Subject: "Synthetic archive note", Body: body, OldChunks: []string{old}, StructuredChunks: []string{old},
			StructuredChunkIDs: []string{id},
		})
	}
	return documents
}

func productionPreprocessConfig() embed.PreprocessConfig {
	return embed.PreprocessConfig{
		StripQuotes: true, StripSignatures: true, StripHTML: true,
		StripBase64: true, StripURLTracking: true, CollapseWhitespace: true,
	}
}
