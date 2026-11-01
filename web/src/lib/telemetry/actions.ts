import { untrack } from 'svelte';
import { startBrowserTelemetry, type BrowserTelemetryReporter } from '@kenn-io/kit-ui/utils/browser-telemetry';
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

const reporters = new WeakMap<APIClient, BrowserTelemetryReporter>();

// App registers each reporter with its transport; screens, sessions, and actions only look it up.
export function startTelemetryReporting(client: APIClient, transport: APIClient = client): BrowserTelemetryReporter {
  const existing = reporters.get(client);
  if (existing) return existing;
  const reporter = startBrowserTelemetry({
    route: '/api/v1/telemetry/events', surface: 'web', storageKey: 'msgvault.app-opened.web',
    post: async (route, event, options) => {
      return transport.fetch(route, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(event), signal: AbortSignal.timeout(3000), ...options,
      });
    },
  });
  reporters.set(client, reporter);
  return reporter;
}

export function telemetryReporter(client: APIClient): BrowserTelemetryReporter | undefined {
  return reporters.get(client);
}

export function stopTelemetryReporting(client: APIClient): void {
  reporters.get(client)?.stop();
  reporters.delete(client);
}

// error_shown counts every occurrence; the other actions count once per UTC day.
export function reportAction(client: APIClient, event: 'message_opened' | 'search_run' | 'search_empty' | 'error_shown', property?: string, value?: string): Promise<void> {
  const report = { event, properties: { surface: 'web', ...(property ? { [property]: value } : {}) } };
  const reporter = reporters.get(client);
  if (event === 'error_shown') reporter?.reportVisit(report);
  else reporter?.reportDaily(event === 'message_opened' ? event : `${event}:${value}`, report);
  return Promise.resolve();
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
