package taskclient

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const OutreachDeliveryMetadataKey = "msgvault.outreach_delivery.v1"

var ErrNotificationOccupied = errors.New("kata inbox already has another attention request; clear it before retrying outreach delivery")

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// FindActionTaskComplete requires service-wide read authority. Unlike the
// interactive issue bridge it never narrows an inaccessible search to one
// project: a moved reminder must still answer an automated retry.
func (c *KataClient) FindActionTaskComplete(ctx context.Context, action string) (KataTask, bool, error) {
	if action == "" {
		return KataTask{}, false, ErrRequestRejected
	}
	tasks, err := c.listAllIssues(ctx, url.Values{"meta": {ActionMetadataKey + "=" + action}, "sort": {"oldest"}, "limit": {"1"}}.Encode())
	if err != nil || len(tasks) == 0 {
		return KataTask{}, false, err
	}
	return tasks[0], true, nil
}

// ListPersonTasksComplete reads across projects, with an extra row so callers
// fail closed when open reminders might be outside the bounded response.
func (c *KataClient) ListPersonTasksComplete(ctx context.Context, personUID string) ([]KataTask, error) {
	if personUID == "" {
		return nil, ErrRequestRejected
	}
	return c.listAllIssues(ctx, url.Values{"meta": {"msgvault.person=" + personUID}, "status": {"open"}, "limit": {"101"}, "sort": {"oldest"}}.Encode())
}

// NotifyTaskOnce commits the native kata notify inbox slot and a durable
// receipt in ONE revision-guarded transaction. The receipt survives clearing
// the slot, so even a lost POST response (or follow-up GET) cannot re-notify.
// A closed task is left untouched, including when it closes during the patch.
func (c *KataClient) NotifyTaskOnce(ctx context.Context, task KataTask, cycle, recipient string) (KataTask, error) {
	if cycle == "" || strings.TrimSpace(recipient) != recipient || recipient == "" || len(recipient) > 128 || !utf8.ValidString(recipient) || strings.ContainsFunc(recipient, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return KataTask{}, ErrRequestRejected
	}
	receipt := cycle + ":" + recipient
	key := "notify." + base64.RawURLEncoding.EncodeToString([]byte(recipient))
	for range 3 {
		if task.Metadata[OutreachDeliveryMetadataKey] == receipt || task.Status != "open" {
			return task, nil
		}
		if task.Metadata[OutreachDeliveryMetadataKey] != nil {
			return task, ErrConflict
		}
		if task.Metadata[key] != nil {
			return task, ErrNotificationOccupied
		}
		updated, err := c.MutateMetadata(ctx, task.Project, task.UID, task.Revision, map[string]any{
			key:                         map[string]any{"from": writeActor, "message": "Review the keep-in-touch draft before sending."},
			OutreachDeliveryMetadataKey: receipt,
		})
		if err == nil {
			return updated, nil
		}
		// MutateMetadata can fail after the transaction committed. Always inspect
		// the durable receipt before returning an unknown outcome to the caller.
		current, readErr := c.GetTask(ctx, task.Project, task.UID)
		if readErr != nil {
			return task, err
		}
		if current.Metadata[OutreachDeliveryMetadataKey] == receipt || current.Status != "open" {
			return current, nil
		}
		if !errors.Is(err, ErrConflict) {
			return current, err
		}
		task = current
	}
	return task, ErrConflict
}
