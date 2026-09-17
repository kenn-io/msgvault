import type { Page } from '@playwright/test';

// The full exploration state, including keyboard focus and scroll position,
// lives in the browser history entry; the URL carries only the shareable part.
export function exploreHistoryState(page: Page): Promise<Record<string, unknown>> {
  return page.evaluate(() => {
    const entry = window.history.state as { exploreState?: Record<string, unknown> } | null;
    return entry?.exploreState ?? {};
  });
}
