import {
  getMeetingContext as generatedGetMeetingContext,
  getMeetingMetrics as generatedGetMeetingMetrics,
  listMeetingActionItems as generatedListMeetingActionItems,
} from '../api/generated/meetings/meetings';
import type {
  ActionsPage,
  ErrorResponse,
  MeetingActionsRequest,
  MeetingContextRequest,
  MeetingMetricsRequest,
  Metrics,
  PacketResult,
} from '../api/generated/models';
import type { APIClient } from '../api/client';

export class MeetingsAPIError extends Error {
  readonly status: number;
  readonly code: string;
  readonly serverError: ErrorResponse;

  constructor(status: number, serverError: ErrorResponse) {
    super(serverError.message || `Meeting request failed (${status})`);
    this.name = 'MeetingsAPIError';
    this.status = status;
    this.code = serverError.error;
    this.serverError = serverError;
  }
}

export interface MeetingsAPI {
  context(request: MeetingContextRequest, signal?: AbortSignal): Promise<PacketResult>;
  actions(request: MeetingActionsRequest, signal?: AbortSignal): Promise<ActionsPage>;
  metrics(request: MeetingMetricsRequest, signal?: AbortSignal): Promise<Metrics>;
}

function failure(error: unknown, status: number): MeetingsAPIError {
  const serverError =
    typeof error === 'object' && error !== null && typeof (error as Partial<ErrorResponse>).error === 'string'
      ? (error as ErrorResponse)
      : {
          error: 'meeting_request_failed',
          message: `Meeting request failed (${status})`,
        };
  return new MeetingsAPIError(status, serverError);
}

export function createMeetingsAPI(client: APIClient): MeetingsAPI {
  return {
    async context(request, signal) {
      const { data, error, response } = await generatedGetMeetingContext(request, { ...client, signal });
      if (data !== undefined) return data;
      throw failure(error, response.status);
    },
    async actions(request, signal) {
      const { data, error, response } = await generatedListMeetingActionItems(request, { ...client, signal });
      if (data !== undefined) return data;
      throw failure(error, response.status);
    },
    async metrics(request, signal) {
      const { data, error, response } = await generatedGetMeetingMetrics(request, { ...client, signal });
      if (data !== undefined) return data;
      throw failure(error, response.status);
    },
  };
}
