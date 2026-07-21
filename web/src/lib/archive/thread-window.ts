import type { ArchiveMessageSummary } from './types';

export interface ThreadWindow {
  messages: ArchiveMessageSummary[];
  truncated: boolean;
}

export function selectInclusiveThreadWindow(
  sorted: ArchiveMessageSummary[],
  selectedId: number,
  cap = 50
): ThreadWindow {
  if (sorted.length <= cap) return { messages: sorted, truncated: false };

  const recent = sorted.slice(-cap);
  if (recent.some((message) => message.id === selectedId)) {
    return { messages: recent, truncated: true };
  }

  const selected = sorted.find((message) => message.id === selectedId);
  if (!selected) return { messages: recent, truncated: true };

  const messages = [...recent.slice(1), selected].sort((left, right) =>
    left.sentAt.localeCompare(right.sentAt)
  );
  return { messages, truncated: true };
}
