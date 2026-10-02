package pocket

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

type summaryPackage struct {
	Status    string `json:"processingStatus"`
	UpdatedAt string `json:"updatedAt"`
	CreatedAt string `json:"createdAt"`
	V2        *struct {
		Summary *struct {
			Markdown *string  `json:"markdown"`
			Bullets  []string `json:"bulletPoints"`
		} `json:"summary"`
		Actions *struct {
			Items jsontext.Value `json:"actionItems"`
		} `json:"actionItems"`
	} `json:"v2"`
}

func Normalize(rec Recording) (meetingcontent.Content, error) {
	content := meetingcontent.Content{
		Summary:    meetingcontent.Section{State: meetingcontent.StateUnavailable, Reason: "provider_processing"},
		Notes:      meetingcontent.Section{State: meetingcontent.StateUnsupported},
		Transcript: meetingcontent.Transcript{State: meetingcontent.StateUnavailable, Reason: "provider_processing"},
		Actions:    []meetingcontent.Action{}, ActionCoverage: meetingcontent.CoverageUnavailable, ActionReason: "provider_processing",
		DurationSeconds: rec.Duration, DurationBasis: meetingcontent.DurationProvider,
	}
	var wire struct {
		Transcript      jsontext.Value `json:"transcript"`
		TranscriptError string         `json:"transcript_error"`
		Summarizations  jsontext.Value `json:"summarizations"`
		SummaryErrors   []string       `json:"summarizations_errors"`
	}
	if json.Unmarshal(rec.Raw, &wire) != nil {
		return content, sectionError("recording")
	}
	if present(wire.Transcript) && strings.TrimSpace(wire.TranscriptError) == "" {
		var segments []struct {
			Speaker string   `json:"speaker"`
			Text    string   `json:"text"`
			Start   *float64 `json:"start"`
			End     *float64 `json:"end"`
		}
		if json.Unmarshal(wire.Transcript, &segments) != nil || segments == nil {
			return content, sectionError("transcript")
		}
		transcript := meetingcontent.Transcript{State: meetingcontent.StateEmpty, Segments: []meetingcontent.Segment{}}
		seen := map[string]bool{}
		for _, item := range segments {
			if strings.TrimSpace(item.Text) == "" || invalidSeconds(item.Start) || invalidSeconds(item.End) || item.Start != nil && item.End != nil && *item.End < *item.Start {
				return content, sectionError("transcript segment")
			}
			transcript.Segments = append(transcript.Segments, meetingcontent.Segment{Speaker: strings.TrimSpace(item.Speaker), Text: item.Text, OffsetSeconds: item.Start})
			if name := strings.TrimSpace(item.Speaker); name != "" && !seen[name] {
				content.SourceParticipants = append(content.SourceParticipants, meetingcontent.Participant{Name: name, Role: "speaker"})
				seen[name] = true
			}
		}
		if len(segments) > 0 {
			transcript.State = meetingcontent.StateAvailable
		}
		content.Transcript = transcript
	}
	if present(wire.Summarizations) {
		var packages map[string]jsontext.Value
		if json.Unmarshal(wire.Summarizations, &packages) != nil || packages == nil {
			return content, sectionError("summarizations")
		}
		keys := make([]string, 0, len(packages))
		for key := range packages {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var selected *summaryPackage
		var selectedUpdated, selectedCreated time.Time
		for _, key := range keys {
			var item summaryPackage
			if json.Unmarshal(packages[key], &item) != nil || item.Status == "" {
				return content, sectionError("summarization package")
			}
			updated, err := summaryTime(item.UpdatedAt)
			if err != nil {
				return content, err
			}
			created, err := summaryTime(item.CreatedAt)
			if err != nil {
				return content, err
			}
			if item.Status != "completed" {
				continue
			}
			if selected == nil || updated.After(selectedUpdated) || updated.Equal(selectedUpdated) && !created.Before(selectedCreated) {
				selected = &item
				selectedUpdated = updated
				selectedCreated = created
			}
		}
		if selected != nil {
			if selected.V2 == nil {
				return content, sectionError("completed summarization")
			}
			if selected.V2.Summary != nil {
				value := selected.V2.Summary
				text := ""
				if value.Markdown != nil {
					text = strings.TrimSpace(*value.Markdown)
				}
				if text == "" && len(value.Bullets) > 0 {
					var bullets strings.Builder
					for _, bullet := range value.Bullets {
						if strings.TrimSpace(bullet) != "" {
							bullets.WriteString("- " + strings.TrimSpace(bullet) + "\n")
						}
					}
					text = strings.TrimSpace(bullets.String())
				}
				if value.Markdown != nil || value.Bullets != nil {
					content.Summary = meetingcontent.Section{State: meetingcontent.StateEmpty, Text: text}
					if text != "" {
						content.Summary.State = meetingcontent.StateAvailable
					}
				}
			}
			if selected.V2.Actions != nil && present(selected.V2.Actions.Items) {
				actions, err := normalizeActions(selected.V2.Actions.Items)
				if err != nil {
					return content, err
				}
				content.Actions = actions
				content.ActionCoverage = meetingcontent.CoverageAvailable
				content.ActionReason = ""
			}
		}
	}
	if len(wire.SummaryErrors) > 0 {
		content.Summary = meetingcontent.Section{State: meetingcontent.StateUnavailable, Reason: "provider_processing"}
		content.Actions = []meetingcontent.Action{}
		content.ActionCoverage = meetingcontent.CoverageUnavailable
		content.ActionReason = "provider_processing"
	}
	if content.DurationSeconds == nil {
		content.DurationBasis = ""
	}
	return content, nil
}

func present(raw jsontext.Value) bool { return len(raw) > 0 && string(raw) != "null" }
func invalidSeconds(value *float64) bool {
	return value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0))
}
func sectionError(section string) error {
	return fmt.Errorf("%w: incompatible %s", ErrContract, section)
}
func summaryTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	out, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return out, sectionError("summarization timestamp")
	}
	return out, nil
}

func normalizeActions(raw jsontext.Value) ([]meetingcontent.Action, error) {
	var items []struct {
		ID           string `json:"id"`
		GlobalID     string `json:"globalActionItemId"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Status       string `json:"status"`
		Due          string `json:"dueDate"`
		Completed    *bool  `json:"isCompleted"`
		CompletedAlt *bool  `json:"is_completed"`
	}
	if json.Unmarshal(raw, &items) != nil || items == nil {
		return nil, sectionError("action items")
	}
	out := make([]meetingcontent.Action, 0, len(items))
	for i, item := range items {
		if strings.TrimSpace(item.Title) == "" {
			return nil, sectionError("action title")
		}
		completed := item.Completed
		if completed == nil {
			completed = item.CompletedAlt
		}
		if item.Completed != nil && item.CompletedAlt != nil && *item.Completed != *item.CompletedAlt {
			return nil, sectionError("conflicting action completion")
		}
		status := meetingcontent.StatusUnknown
		switch strings.ToUpper(item.Status) {
		case "TODO", "IN_PROGRESS":
			status = meetingcontent.StatusPending
		case "COMPLETED":
			status = meetingcontent.StatusCompleted
		case "CANCELLED", "CANCELED":
			status = meetingcontent.StatusCancelled
		}
		if completed != nil {
			if *completed && (status == meetingcontent.StatusPending || status == meetingcontent.StatusCancelled) || !*completed && status == meetingcontent.StatusCompleted {
				return nil, sectionError("conflicting action status")
			}
			if *completed {
				status = meetingcontent.StatusCompleted
			} else if status == meetingcontent.StatusUnknown && item.Status == "" {
				status = meetingcontent.StatusPending
			}
		}
		id := item.GlobalID
		if id == "" {
			id = item.ID
		}
		out = append(out, meetingcontent.Action{Ordinal: i, SourceID: id, Title: strings.TrimSpace(item.Title), Description: item.Description, Status: status, SourceStatus: item.Status, DueDate: item.Due, Origin: SourceType, Locator: fmt.Sprintf("action_items[%d]", i)})
	}
	return out, nil
}
