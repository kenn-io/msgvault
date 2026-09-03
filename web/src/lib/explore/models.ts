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
export type PersonFileSearchRequest = components['schemas']['PersonFileSearchHTTPRequest'];
export type PersonFileSearchResponse = components['schemas']['PersonFileSearchHTTPResponse'];
export type PersonFileSearchRow = components['schemas']['PersonFileSearchRow'];
export type PersonFileProvenance = components['schemas']['PersonFileProvenance'];
export type PersonFileDirection = NonNullable<PersonFileSearchRequest['directions']>[number];
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
export type SourceIdentitiesResponse = components['schemas']['SourceIdentitiesResponse'];
export type SourceIdentityResponse = components['schemas']['SourceIdentityResponse'];
export type IdentityDirection = 'any' | 'sender' | 'recipient';
export type PersonSummary = components['schemas']['PersonSummary'];
export type PersonIdentifier = components['schemas']['PersonIdentifier'];
export type PersonCluster = components['schemas']['PersonCluster'];
export type PersonClusterEdge = components['schemas']['PersonClusterEdge'];
export type DomainSummary = components['schemas']['DomainSummary'];
export type PersonContextSummaryResponse = components['schemas']['ParticipantContextSummaryHTTPResponse'];
export type DomainContextSummaryResponse = components['schemas']['DomainContextSummaryHTTPResponse'];
export type IdentitySearchSort = components['schemas']['IdentitySearchSort'];

export type ExploreWorkspace =
  | 'everything'
  | 'directory'
  | 'directory_review'
  | 'files'
  | 'relationships'
  | 'saved_views'
  | 'sources'
  | 'deletions'
  | 'settings';
export type DirectoryReviewKind = 'identity' | 'fact' | 'relationship';
export type IdentityReviewState = 'candidate' | 'conflict' | 'accepted' | 'rejected';
export type RelationshipReviewState = 'pending' | 'accepted' | 'rejected';
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
  directoryQuery: string;
  directoryContactState: string;
  directoryCategory: string;
  directoryOrganization: string;
  directoryPrimaryChannel: string;
  directoryLastContactAfter: string;
  directoryLastContactBefore: string;
  directorySort: 'name' | 'last_contact_desc' | 'last_contact_asc';
  directoryPersonID: number | null;
  reviewKind: DirectoryReviewKind;
  identityState: IdentityReviewState;
  relationshipReviewState: RelationshipReviewState;
  query: string;
  searchMode: ExploreSearchMode;
  filters: ExploreFilter[];
  groupingChain: ExploreGroupDimension[];
  presentation: 'table' | 'timeline' | 'files';
  sort: ExploreSort[];
  fileSort?: FileSearchSort;
  fileFilenameQuery: string;
  fileMIMEFamilies: FileMIMEFamily[];
  personFilePresentation: 'media' | 'files';
  personFileDirections: PersonFileDirection[];
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

/** Rows archived before typed message kinds existed are email records. */
export function isEmailMessageType(messageType: string): boolean {
  return messageType === '' || messageType === 'email';
}

export function isValidSourceID(value: string | undefined): value is string {
  if (value === undefined || !/^\d+$/.test(value)) return false;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed > 0;
}

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
