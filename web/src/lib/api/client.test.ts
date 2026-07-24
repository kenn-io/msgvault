import { describe, expect, it, vi } from 'vitest';

import { createSessionAwareAPIClient } from './client';

function client(fetchFn: typeof fetch, onUnauthorized: () => void) {
  return createSessionAwareAPIClient(fetchFn, () => undefined, onUnauthorized);
}

describe('session-aware API client 401 handling', () => {
  it('logs out on the daemon session-expiry 401 (error: "unauthorized")', async () => {
    const onUnauthorized = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async () =>
      Response.json({ error: 'unauthorized', message: 'Invalid or missing API key' }, { status: 401 })
    );

    const { GET } = client(fetchFn, onUnauthorized);
    const { response } = await GET('/api/session');

    expect(response.status).toBe(401);
    expect(onUnauthorized).toHaveBeenCalledOnce();
  });

  it('does not log out on a downstream task 401 (error: "authentication_required")', async () => {
    const onUnauthorized = vi.fn();
    const body = { error: 'authentication_required', message: 'Task service authentication is required' };
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(body, { status: 401 }));

    const { GET } = client(fetchFn, onUnauthorized);
    const { response, error } = await GET('/api/session');

    expect(onUnauthorized).not.toHaveBeenCalled();
    // The response passes through so the caller handles it inline; the wrapper
    // only inspected a clone, leaving the body intact for openapi-fetch to parse.
    expect(response.status).toBe(401);
    expect(error).toEqual(body);
  });

  it('does not log out on an empty or non-JSON 401 body', async () => {
    const onUnauthorized = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async () => new Response('not json', { status: 401 }));

    const { GET } = client(fetchFn, onUnauthorized);
    const { response } = await GET('/api/session');

    expect(response.status).toBe(401);
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it('does not log out on non-401 responses', async () => {
    const onUnauthorized = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async () =>
      Response.json({ error: 'unauthorized' }, { status: 403 })
    );

    const { GET } = client(fetchFn, onUnauthorized);
    await GET('/api/session');

    expect(onUnauthorized).not.toHaveBeenCalled();
  });
});
