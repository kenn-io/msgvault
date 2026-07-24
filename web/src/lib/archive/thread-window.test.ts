import { describe, expect, it } from 'vitest';

import type { ArchiveMessageSummary } from './types';
import { selectInclusiveThreadWindow } from './thread-window';

function message(id: number, sentAt: string): ArchiveMessageSummary {
  return {
    id,
    conversationId: 1,
    subject: `Message ${id}`,
    sender: 'alice@example.com',
    recipients: ['bob@example.com'],
    sentAt,
    snippet: ''
  };
}

function buildSorted(count: number): ArchiveMessageSummary[] {
  const base = Date.UTC(2026, 0, 1);
  return Array.from({ length: count }, (_, index) =>
    message(index + 1, new Date(base + index * 60_000).toISOString())
  );
}

describe('selectInclusiveThreadWindow', () => {
  it('returns the full thread at or below the cap', () => {
    const sorted = buildSorted(10);

    expect(selectInclusiveThreadWindow(sorted, 5, 50)).toEqual({
      messages: sorted,
      truncated: false
    });
  });

  it('returns the most recent cap when the selected message is in the tail', () => {
    const result = selectInclusiveThreadWindow(buildSorted(60), 60, 50);

    expect(result.truncated).toBe(true);
    expect(result.messages).toHaveLength(50);
    expect(result.messages.at(0)?.id).toBe(11);
    expect(result.messages.at(-1)?.id).toBe(60);
  });

  it('keeps an older selected message in a bounded chronological window', () => {
    const result = selectInclusiveThreadWindow(buildSorted(60), 5, 50);

    expect(result.messages).toHaveLength(50);
    expect(result.messages.some((item) => item.id === 5)).toBe(true);
    expect(result.messages.some((item) => item.id === 11)).toBe(false);
    expect(result.messages.every((item, index, items) =>
      index === 0 || items[index - 1]!.sentAt <= item.sentAt
    )).toBe(true);
  });

  it('falls back to the recent window when the selection is absent', () => {
    const result = selectInclusiveThreadWindow(buildSorted(60), 999, 50);

    expect(result.messages.at(0)?.id).toBe(11);
    expect(result.messages.at(-1)?.id).toBe(60);
  });
});
