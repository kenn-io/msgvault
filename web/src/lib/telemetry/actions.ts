import { untrack } from 'svelte';
import { captureTelemetryEvent } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { ExploreSearchMode } from '../explore/models';

export function utcDay(): string {
  return new Date().toISOString().slice(0, 10);
}

export function reportWhenVisible(send: () => unknown, signal?: AbortSignal): () => void {
  const cancel = (): void => {
    document.removeEventListener('visibilitychange', report);
    window.removeEventListener('focus', report);
    signal?.removeEventListener('abort', cancel);
  };
  const report = (): void => {
    if (document.visibilityState === 'hidden') return;
    cancel();
    send();
  };
  if (signal?.aborted) return cancel;
  document.addEventListener('visibilitychange', report);
  window.addEventListener('focus', report);
  signal?.addEventListener('abort', cancel, { once: true });
  report();
  return cancel;
}

export function reportAction(client: APIClient, event: string, property?: string, value?: string, signal?: AbortSignal): Promise<void> {
  return captureTelemetryEvent({ event, properties: { surface: 'web', ...(property ? { [property]: value } : {}) } }, {
    ...client, signal: signal ?? AbortSignal.timeout(3000),
  }).then(() => {}).catch(() => {});
}

export function reportSearch(client: APIClient, query: string, mode: ExploreSearchMode, count?: number): Promise<void> {
  if (!query.trim()) return Promise.resolve();
  const kind = mode === 'full_text' ? 'text' : mode;
  if (count === 0) void reportAction(client, 'search_empty', 'search_type', kind);
  return reportAction(client, 'search_run', 'search_type', kind);
}

export function reportError(client: () => APIClient, kind: () => string, signal?: AbortSignal, onReport?: () => void): () => void {
  return untrack(() => reportWhenVisible(() => {
    onReport?.();
    return reportAction(client(), 'error_shown', 'kind', kind());
  }, signal));
}
