import type { components } from '../api/generated/schema';

export type EntryRow = components['schemas']['EntryRow'];
export type ExploreCacheUnavailable = components['schemas']['ExploreCacheUnavailableResponse'];
export type ExploreFilter = components['schemas']['ExploreFilter'];
export type ExploreFileFact = components['schemas']['ExploreFileFact'];
export type ExploreFilesResponse = components['schemas']['ExploreFilesHTTPResponse'];
export type FileMetadata = components['schemas']['FileMetadataResponse'];
export type FileSearchRequest = components['schemas']['FileSearchHTTPRequest'];
export type FileSearchResponse = components['schemas']['FileSearchHTTPResponse'];
export type FileSearchRow = components['schemas']['FileSearchRow'];
export interface FileViewerTarget {
  id: FileSearchRow['id'];
  key?: FileSearchRow['key'];
  entry_key?: FileSearchRow['entry_key'];
  message_id?: FileSearchRow['message_id'];
  conversation_id?: FileSearchRow['conversation_id'];
  filename?: FileSearchRow['filename'];
  mime_type?: FileSearchRow['mime_type'];
  size_bytes?: FileSearchRow['size_bytes'];
}
export type FileGroupsResponse = components['schemas']['FileGroupsHTTPResponse'];
export type FileSearchSort = {
  field: 'occurred_at' | 'filename' | 'size';
  direction: 'asc' | 'desc';
};
export type FileMIMEFamily = 'image' | 'pdf' | 'audio' | 'video' | 'text' | 'document' | 'archive' | 'other';
export type ExploreGroupDimension = components['schemas']['ExploreGroupDimension'];
export type ExploreGroupRow = components['schemas']['ExploreGroupRow'];
export type ExploreGroupsResponse = components['schemas']['ExploreGroupsHTTPResponse'];
export type ExplorePredicate = components['schemas']['ExploreHTTPRequest'];
/**
 * Predicate for the groups listing: the shared explore predicate plus the
 * groups-only exact-key filter (see ExploreGroupsHTTPRequest.group_key).
 */
export type ExploreGroupsPredicate = ExplorePredicate &
  Pick<components['schemas']['ExploreGroupsHTTPRequest'], 'group_key'>;
export type ExploreResponse = components['schemas']['ExploreHTTPResponse'];
export type ExploreSearchMode = NonNullable<ExplorePredicate['search_mode']>;
export type ExploreSort = components['schemas']['ExploreSort'];
export type SearchProvenance = components['schemas']['SearchProvenance'];
export type PersonSummary = components['schemas']['PersonSummary'];
export type PersonIdentifier = components['schemas']['PersonIdentifier'];
export type PersonCluster = components['schemas']['PersonCluster'];
export type PersonClusterEdge = components['schemas']['PersonClusterEdge'];
export type DomainSummary = components['schemas']['DomainSummary'];
export type PersonContextSummaryResponse = components['schemas']['PersonContextSummaryHTTPResponse'];
export type DomainContextSummaryResponse = components['schemas']['DomainContextSummaryHTTPResponse'];
export type IdentitySearchSort = components['schemas']['IdentitySearchSort'];

export type ExploreWorkspace =
  | 'everything'
  | 'files'
  | 'relationships'
  | 'saved_views'
  | 'sources'
  | 'deletions'
  | 'settings';
export type RelationshipFacet = 'people' | 'domains';
export type ExploreColumn =
  | 'kind'
  | 'people'
  | 'title'
  | 'excerpt'
  | 'time'
  | 'attachments'
  | 'size';

export const DEFAULT_EXPLORE_COLUMNS: ExploreColumn[] = [
  'kind',
  'people',
  'title',
  'excerpt',
  'time',
  'attachments'
];

export interface ExploreScrollAnchor {
  key: string;
  offset: number;
}

/** Browser-restorable exploration context. Bulk selection is session-only. */
export interface ExploreURLState {
  schemaVersion: number;
  workspace: ExploreWorkspace;
  query: string;
  searchMode: ExploreSearchMode;
  filters: ExploreFilter[];
  groupingChain: ExploreGroupDimension[];
  presentation: 'table' | 'timeline' | 'files';
  sort: ExploreSort[];
  fileSort?: FileSearchSort;
  fileFilenameQuery: string;
  fileMIMEFamilies: FileMIMEFamily[];
  identityQuery?: string;
  identitySort?: IdentitySearchSort;
  analysisTarget?: string | null;
  selectedIdentifier?: string | null;
  relationshipFacet: RelationshipFacet;
  relationshipTarget: string | null;
  relationshipShowAll: boolean;
  relationshipFiles: boolean;
  columns: ExploreColumn[];
  columnWidths: Partial<Record<ExploreColumn, number>>;
  activeRow: string | null;
  selectedRow: string | null;
  inspectorPinned: boolean;
  conversationAnchor: string | null;
  scrollAnchor: ExploreScrollAnchor | null;
  selection?: never;
  [futureField: string]: unknown;
}

export interface ExplicitExploreSelection {
  mode: 'explicit';
  rowKeys: string[];
}

export interface AllMatchingExploreSelection {
  mode: 'all_matching';
  predicate: ExplorePredicate;
  exclusions: string[];
  cacheRevision: string;
  searchProvenance: SearchProvenance;
  candidateSnapshotId?: string;
  /** Session-only identity proving which exact predicate produced this selection. */
  predicateFingerprint: string;
  /** Session-only monotonically increasing query generation. */
  resultGeneration: number;
}

export type ExploreSelection = ExplicitExploreSelection | AllMatchingExploreSelection;

export interface ExploreResult {
  rows: EntryRow[];
  totalCount?: number;
  cacheRevision: string;
  searchProvenance: SearchProvenance;
  candidateSnapshotId?: string;
  candidatePoolSaturated: boolean;
  /** "active" when the backend narrowed a semantic or hybrid search to
   * active (non-deleted) messages; absent when no narrowing was declared. */
  searchDeletionScope?: string;
  nextCursor?: string;
}

export interface ExploreGroupResult {
  rows: ExploreGroupRow[];
  totalCount: number;
  cacheRevision: string;
  searchProvenance: SearchProvenance;
  candidateSnapshotId?: string;
  /** "active" when the backend narrowed a semantic or hybrid search to
   * active (non-deleted) messages; absent when no narrowing was declared. */
  searchDeletionScope?: string;
  nextCursor?: string;
}

export interface ExploreFilesResult {
  files: ExploreFileFact[];
  totalCount: number;
  cacheRevision: string;
  searchProvenance: SearchProvenance;
  candidateSnapshotId?: string;
  nextCursor?: string;
}

export type ExploreLoadResult =
  | { status: 'ready'; result: ExploreResult }
  | { status: 'unavailable'; unavailable: ExploreCacheUnavailable };

export type ExploreGroupLoadResult =
  | { status: 'ready'; result: ExploreGroupResult }
  | { status: 'unavailable'; unavailable: ExploreCacheUnavailable };

export type ExploreFilesLoadResult =
  | { status: 'ready'; result: ExploreFilesResult }
  | { status: 'unavailable'; unavailable: ExploreCacheUnavailable };
