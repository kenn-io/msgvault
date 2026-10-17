import { captureTelemetryEvent } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { ExploreSearchMode } from '../explore/models';

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

export function reportSearch(client: APIClient, query: string, mode: ExploreSearchMode): Promise<void> {
  return query.trim() ? reportAction(client, 'search_run', 'search_type', mode === 'full_text' ? 'text' : mode) : Promise.resolve();
}

export function failureKind(cause: unknown): string {
  const status = typeof cause === 'number' ? cause : typeof cause === 'object' && cause !== null && 'status' in cause ? Number(cause.status) : 0;
  if (status === 401 || status === 403) return 'auth';
  if (status === 404) return 'not_found';
  if (status === 429) return 'rate_limited';
  if (status >= 500 && status <= 599) return 'provider';
  if (cause instanceof TypeError || cause instanceof DOMException && cause.name === 'TimeoutError') return 'network';
  return 'other';
}

// Claims stay in this origin's tab. Only the digest survives a reload.
function outcomeVisible(signal?: AbortSignal): boolean {
  return !signal?.aborted && document.visibilityState !== 'hidden';
}

async function claimOutcome(event: string, identity: string, signal?: AbortSignal): Promise<boolean> {
  try {
    const bytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(identity));
    if (!outcomeVisible(signal)) return false;
    const key = `msgvault.outcome.${event}.${Array.from(new Uint8Array(bytes), (byte) => byte.toString(16).padStart(2, '0')).join('')}`;
    if (sessionStorage.getItem(key)) return false;
    sessionStorage.setItem(key, '1');
    return true;
  } catch { return false; }
}

export async function reportSearchOutcome(client: APIClient, query: string, mode: ExploreSearchMode, empty: boolean, cause?: unknown, signal?: AbortSignal): Promise<void> {
  const normalized = query.trim().replace(/\s+/g, ' ');
  if (!normalized || !outcomeVisible(signal)) return;
  const event = cause === undefined ? 'search_empty' : 'error_shown';
  if (cause === undefined && !empty) return;
  if (!await claimOutcome(event, `${mode}:${normalized}`, signal) || !outcomeVisible(signal)) return;
  await reportAction(client, event, event === 'search_empty' ? 'search_type' : 'kind', event === 'search_empty' ? mode === 'full_text' ? 'text' : mode : failureKind(cause), signal);
}

export async function reportError(client: APIClient, identity: string, cause: unknown, signal?: AbortSignal): Promise<void> {
  if (!outcomeVisible(signal) || cause instanceof DOMException && cause.name === 'AbortError') return;
  if (!await claimOutcome('error_shown', identity, signal) || !outcomeVisible(signal)) return;
  await reportAction(client, 'error_shown', 'kind', failureKind(cause), signal);
}
