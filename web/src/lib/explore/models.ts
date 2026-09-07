import type {
  DomainContextSummaryHTTPResponse as GeneratedDomainContextSummaryHTTPResponse,
  DomainSummary as GeneratedDomainSummary,
  EntryRow as GeneratedEntryRow,
  ExploreCacheUnavailableResponse as GeneratedExploreCacheUnavailableResponse,
  ExploreFileFact as GeneratedExploreFileFact,
  ExploreFilesHTTPResponse as GeneratedExploreFilesHTTPResponse,
  ExploreFilter as GeneratedExploreFilter,
  ExploreGroupDimension as GeneratedExploreGroupDimension,
  ExploreGroupRow as GeneratedExploreGroupRow,
  ExploreGroupsHTTPRequest as GeneratedExploreGroupsHTTPRequest,
  ExploreGroupsHTTPResponse as GeneratedExploreGroupsHTTPResponse,
  ExploreHTTPRequest as GeneratedExploreHTTPRequest,
  ExploreHTTPResponse as GeneratedExploreHTTPResponse,
  ExploreSort as GeneratedExploreSort,
  FileGroupsHTTPResponse as GeneratedFileGroupsHTTPResponse,
  FileMetadataResponse as GeneratedFileMetadataResponse,
  FileSearchHTTPRequest as GeneratedFileSearchHTTPRequest,
  FileSearchHTTPResponse as GeneratedFileSearchHTTPResponse,
  FileSearchRow as GeneratedFileSearchRow,
  IdentitySearchSort as GeneratedIdentitySearchSort,
  OperationRunDetailRelatedStatus as GeneratedOperationRunDetailRelatedStatus,
  OperationRunSummaryKind as GeneratedOperationKind,
  OperationRunSummaryLane as GeneratedOperationLane,
  OperationRunSummaryState as GeneratedOperationState,
  ParticipantContextSummaryHTTPResponse as GeneratedParticipantContextSummaryHTTPResponse,
  PersonCluster as GeneratedPersonCluster,
  PersonClusterEdge as GeneratedPersonClusterEdge,
  PersonFileProvenance as GeneratedPersonFileProvenance,
  PersonFileSearchHTTPRequest as GeneratedPersonFileSearchHTTPRequest,
  PersonFileSearchHTTPResponse as GeneratedPersonFileSearchHTTPResponse,
  PersonFileSearchRow as GeneratedPersonFileSearchRow,
  PersonIdentifier as GeneratedPersonIdentifier,
  PersonSummary as GeneratedPersonSummary,
  SearchProvenance as GeneratedSearchProvenance,
  SourceIdentitiesResponse as GeneratedSourceIdentitiesResponse,
  SourceIdentityResponse as GeneratedSourceIdentityResponse,
} from '../api/generated/models';
import type { SettingsNavigationAuthority } from '../carddav/navigation';

export type EntryRow = GeneratedEntryRow;
export type ExploreCacheUnavailable = GeneratedExploreCacheUnavailableResponse;
export type ExploreFilter = GeneratedExploreFilter;
export type ExploreFileFact = GeneratedExploreFileFact;
export type ExploreFilesResponse = GeneratedExploreFilesHTTPResponse;
export type FileMetadata = GeneratedFileMetadataResponse;
export type FileSearchRequest = GeneratedFileSearchHTTPRequest;
export type FileSearchResponse = GeneratedFileSearchHTTPResponse;
export type FileSearchRow = GeneratedFileSearchRow;
export type PersonFileSearchRequest = GeneratedPersonFileSearchHTTPRequest;
export type PersonFileSearchResponse = GeneratedPersonFileSearchHTTPResponse;
export type PersonFileSearchRow = GeneratedPersonFileSearchRow;
export type PersonFileProvenance = GeneratedPersonFileProvenance;
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
export type FileGroupsResponse = GeneratedFileGroupsHTTPResponse;
export type FileSearchSort = {
  field: 'occurred_at' | 'filename' | 'size';
  direction: 'asc' | 'desc';
};
export type FileMIMEFamily = 'image' | 'pdf' | 'audio' | 'video' | 'text' | 'document' | 'archive' | 'other';
export type ExploreGroupDimension = GeneratedExploreGroupDimension;
export type ExploreGroupRow = GeneratedExploreGroupRow;
export type ExploreGroupsResponse = GeneratedExploreGroupsHTTPResponse;
export type ExplorePredicate = GeneratedExploreHTTPRequest;
/**
 * Predicate for the groups listing: the shared explore predicate plus the
 * groups-only exact-key filter (see ExploreGroupsHTTPRequest.group_key).
 */
export type ExploreGroupsPredicate = ExplorePredicate & Pick<GeneratedExploreGroupsHTTPRequest, 'group_key'>;
export type ExploreResponse = GeneratedExploreHTTPResponse;
export type ExploreSearchMode = NonNullable<ExplorePredicate['search_mode']>;
export type OperationStatusAuthority = Exclude<
  GeneratedOperationRunDetailRelatedStatus,
  'listSourceStatus' | 'getCardDAVStatus'
>;
export type ExploreSort = GeneratedExploreSort;
export type SearchProvenance = GeneratedSearchProvenance;
export type SourceIdentitiesResponse = GeneratedSourceIdentitiesResponse;
export type SourceIdentityResponse = GeneratedSourceIdentityResponse;
export type IdentityDirection = 'any' | 'sender' | 'recipient';
export type PersonSummary = GeneratedPersonSummary;
export type PersonIdentifier = GeneratedPersonIdentifier;
export type PersonCluster = GeneratedPersonCluster;
export type PersonClusterEdge = GeneratedPersonClusterEdge;
export type DomainSummary = GeneratedDomainSummary;
export type PersonContextSummaryResponse = GeneratedParticipantContextSummaryHTTPResponse;
export type DomainContextSummaryResponse = GeneratedDomainContextSummaryHTTPResponse;
export type IdentitySearchSort = GeneratedIdentitySearchSort;

export type ExploreWorkspace =
  | 'everything'
  | 'directory'
  | 'directory_review'
  | 'files'
  | 'operations'
  | 'relationships'
  | 'saved_views'
  | 'sources'
  | 'deletions'
  | 'settings';
export type OperationKind = GeneratedOperationKind;
export type OperationLane = GeneratedOperationLane;
export type OperationState = GeneratedOperationState;
export type DirectoryReviewKind = 'identity' | 'fact' | 'relationship';
export type IdentityReviewState = 'candidate' | 'conflict' | 'accepted' | 'rejected';
export type RelationshipReviewState = 'pending' | 'accepted' | 'rejected';
export type RelationshipFacet = 'people' | 'domains';
export type ExploreColumn = 'kind' | 'people' | 'title' | 'excerpt' | 'time' | 'attachments' | 'size';

export const DEFAULT_EXPLORE_COLUMNS: ExploreColumn[] = ['kind', 'people', 'title', 'excerpt', 'time', 'attachments'];

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
  operationLane: '' | OperationLane;
  operationKind: '' | OperationKind;
  operationState: '' | OperationState;
  operationStartedFrom: string;
  operationStartedBefore: string;
  operationRunID: string | null;
  operationStatus: '' | OperationStatusAuthority;
  settingsAuthority: '' | SettingsNavigationAuthority;
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
  { status: 'ready'; result: ExploreResult } | { status: 'unavailable'; unavailable: ExploreCacheUnavailable };

export type ExploreGroupLoadResult =
  { status: 'ready'; result: ExploreGroupResult } | { status: 'unavailable'; unavailable: ExploreCacheUnavailable };

export type ExploreFilesLoadResult =
  { status: 'ready'; result: ExploreFilesResult } | { status: 'unavailable'; unavailable: ExploreCacheUnavailable };
