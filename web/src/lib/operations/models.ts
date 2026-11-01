import type {
  OperationLaneStatus as GeneratedOperationLaneStatus,
  OperationLaneStatusSupportedActionsItem as GeneratedOperationAction,
  OperationRunsResponse as GeneratedOperationRunsResponse,
  OperationRunDetail as GeneratedOperationRunDetail,
  OperationRunSummary as GeneratedOperationRunSummary,
  OperationRunSummaryKind as GeneratedOperationKind,
  OperationRunSummaryLane as GeneratedOperationLane,
  OperationRunSummaryState as GeneratedOperationState,
  OperationStatusResponse as GeneratedOperationStatusResponse,
  OperationUnavailableKind as GeneratedOperationUnavailableKind,
} from '../api/generated/models';
import type { ExploreURLState } from '../explore/models';

export type OperationAction = GeneratedOperationAction;
export type OperationActionOutcome = 'succeeded' | 'conflict' | 'failed' | 'discarded';
export type OperationKind = GeneratedOperationKind;
export type OperationLane = GeneratedOperationLane;
export type OperationLaneStatus = GeneratedOperationLaneStatus;
export type OperationRunDetail = GeneratedOperationRunDetail;
export type OperationRunSummary = GeneratedOperationRunSummary;
export type OperationRunsResponse = GeneratedOperationRunsResponse;
export type OperationStatusResponse = GeneratedOperationStatusResponse;
export type OperationState = GeneratedOperationState;
export type OperationUnavailableKind = GeneratedOperationUnavailableKind;

export type OperationsURLState = Pick<ExploreURLState,
  | 'operationLane'
  | 'operationKind'
  | 'operationState'
  | 'operationStartedFrom'
  | 'operationStartedBefore'
  | 'operationRunID'
  | 'operationStatus'>;

export interface OperationStatusLane {
  readonly lane: OperationLane;
  readonly kinds: readonly OperationLaneStatus[];
}

export interface OperationsSnapshot {
  readonly statusLanes: readonly OperationStatusLane[];
  readonly rows: readonly OperationRunSummary[];
  readonly unavailableKinds: readonly OperationUnavailableKind[];
  readonly detail: OperationRunDetail | null;
  readonly membershipRevision: number | null;
  readonly nextCursor: string | null;
  readonly statusReadable: boolean;
  readonly historyReadable: boolean;
  readonly initialLoading: boolean;
  readonly backgroundLoading: boolean;
  readonly paging: boolean;
  readonly detailLoading: boolean;
  readonly statusError: string | null;
  readonly runsError: string | null;
  readonly detailError: string | null;
  readonly conflict: string | null;
  readonly restartRequired: boolean;
  readonly actionPending: OperationAction | null;
  readonly actionConflict: string | null;
  readonly actionError: string | null;
}
