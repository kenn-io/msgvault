export type ArchiveSearchMode = 'fts' | 'vector' | 'hybrid';

export type ArchiveFilterValue = string | number | boolean | string[];

export interface ArchiveFilter {
  field: string;
  operator: string;
  value: ArchiveFilterValue;
}

export interface ArchiveSort {
  field: string;
  direction: 'asc' | 'desc';
}

export type ArchivePresentation = 'table' | 'timeline' | 'files';

/**
 * Browser-restorable analytical context. Bulk selection is deliberately
 * excluded: selection can be large and is scoped to one live archive revision.
 */
export interface ArchiveURLState {
  schemaVersion: number;
  query: string;
  searchMode: ArchiveSearchMode;
  filters: ArchiveFilter[];
  groupingChain: string[];
  presentation: ArchivePresentation;
  sort: ArchiveSort[];
  columns: string[];
  selectedRow: string | null;
  inspectorPinned: boolean;
  conversationAnchor: string | null;
  scrollKey: string | null;
  bulkSelection?: never;
  [futureField: string]: unknown;
}

export interface ArchiveMessageSummary {
  id: number;
  conversationId: number;
  subject: string;
  sender: string;
  recipients: string[];
  sentAt: string;
  snippet: string;
}

export interface ArchiveAttachment {
  filename: string;
  mimeType: string;
  sizeBytes: number;
}

export interface ArchiveMessageDetail extends ArchiveMessageSummary {
  body: string;
  bodyHtml?: string;
  attachments: ArchiveAttachment[];
}

export type MessageViewMode = 'html' | 'text';
