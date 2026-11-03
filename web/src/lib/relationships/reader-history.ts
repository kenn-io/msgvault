import type { ReadingPaneSelection } from '../components/reader/ReadingPane.svelte';

export const RELATIONSHIP_READER_HISTORY_KEY = 'relationshipReader';
export const RELATIONSHIP_DAY_HISTORY_KEY = 'relationshipDay';

export interface RelationshipReaderHistory {
  target: string;
  selection: ReadingPaneSelection;
  anchor?: number;
  bounds?: { start: string; end: string };
}

export function relationshipReaderHistory(history: Record<string, unknown> | null, workspace: string, target: string | null): RelationshipReaderHistory | undefined {
  const snapshot = history?.[RELATIONSHIP_READER_HISTORY_KEY] as RelationshipReaderHistory | undefined;
  return workspace === 'relationships' && target !== null && snapshot?.target === target ? snapshot : undefined;
}

export function relationshipDayHistory(history: Record<string, unknown> | null, workspace: string, target: string | null): string | null {
  const day = history?.[RELATIONSHIP_DAY_HISTORY_KEY] as { target: string; date: string } | undefined;
  return workspace === 'relationships' && target !== null && day?.target === target ? day.date : null;
}

/** The relationship keys an Explore history rewrite keeps, when they belong to the entry's target. */
export function relationshipHistoryState(history: Record<string, unknown> | null, workspace: string, target: string | null): Record<string, unknown> | null {
  const reader = relationshipReaderHistory(history, workspace, target);
  const date = relationshipDayHistory(history, workspace, target);
  return reader || date ? { [RELATIONSHIP_READER_HISTORY_KEY]: reader, [RELATIONSHIP_DAY_HISTORY_KEY]: date ? { target, date } : undefined } : null;
}
