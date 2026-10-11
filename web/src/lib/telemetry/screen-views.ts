import { captureTelemetryEvent } from '../api/generated/api/api';
import type { APIClient } from '../api/client';
import type { ExploreWorkspace } from '../explore/models';

export function startScreenViewReporting(client: APIClient, screen: ExploreWorkspace | 'message'): () => void {
  const pending = new Map<AbortController, number>();
  const report = (): void => {
    if (document.visibilityState === 'hidden') return;
    const controller = new AbortController();
    const timer = window.setTimeout(() => controller.abort(), 3000);
    pending.set(controller, timer);
    void captureTelemetryEvent(
      { event: 'screen_viewed', properties: { screen, surface: 'web' } },
      { ...client, signal: controller.signal },
    )
      .catch(() => undefined)
      .finally(() => {
        window.clearTimeout(timer);
        pending.delete(controller);
      });
  };
  report();
  window.addEventListener('focus', report);
  return () => {
    window.removeEventListener('focus', report);
    for (const [controller, timer] of pending) {
      window.clearTimeout(timer);
      controller.abort();
    }
  };
}
