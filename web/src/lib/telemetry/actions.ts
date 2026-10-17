import { captureTelemetryEvent } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { ExploreSearchMode } from '../explore/models';

export function reportAction(client: APIClient, event: string, property?: string, value?: string, signal?: AbortSignal): Promise<void> {
  return captureTelemetryEvent({ event, properties: { surface: 'web', ...(property ? { [property]: value } : {}) } }, {
    ...client, signal: signal ?? AbortSignal.timeout(3000),
  }).then(() => {}).catch(() => {});
}

export function reportSearch(client: APIClient, query: string, mode: ExploreSearchMode): Promise<void> {
  return query.trim() ? reportAction(client, 'search_run', 'search_type', mode === 'full_text' ? 'text' : mode) : Promise.resolve();
}
