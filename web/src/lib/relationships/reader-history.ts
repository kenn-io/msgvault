import type { ReadingPaneSelection } from '../components/reader/ReadingPane.svelte';

export const RELATIONSHIP_READER_HISTORY_KEY = 'relationshipReader';

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
