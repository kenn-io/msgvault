import { describe, expect, it, vi } from 'vitest';

import { createSessionController } from './session.svelte';

function sessionResponse(
  authMode: 'loopback' | 'api_key' | 'session' | 'required',
  csrfToken?: string
) {
  return Response.json({
    auth_mode: authMode,
    ...(csrfToken ? { csrf_token: csrfToken } : {}),
    https: false,
    plain_http_warning: true
  });
}

describe('browser session controller', () => {
  it('bootstraps authentication from the same-origin session endpoint', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => sessionResponse('session', 'csrf-token'));
    const session = createSessionController(fetchFn);

    await session.bootstrap();

    expect(session.authMode).toBe('session');
    expect(session.csrfToken).toBe('csrf-token');
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(new URL((fetchFn.mock.calls[0]?.[0] as Request).url).pathname).toBe('/api/session');
  });

  it('attaches the session token to same-origin mutations', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      requests.push(request);
      if (new URL(request.url).pathname === '/api/session' && request.method === 'GET') {
        return sessionResponse('session', 'csrf-token');
      }
      return new Response(null, { status: 204 });
    });
    const session = createSessionController(fetchFn);
    await session.bootstrap();

    await session.client.DELETE('/api/session');

    expect(requests[1]?.headers.get('X-CSRF-Token')).toBe('csrf-token');
    expect(requests[1]?.headers.get('Origin')).toBeNull();
  });

  it('does not retry a mutation after an unauthorized response', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input as Request;
      if (request.method === 'GET') return sessionResponse('session', 'csrf-token');
      return Response.json({ error: 'unauthorized', message: 'Session expired' }, { status: 401 });
    });
    const session = createSessionController(fetchFn);
    await session.bootstrap();

    await session.client.DELETE('/api/session');

    expect(fetchFn).toHaveBeenCalledTimes(2);
    expect(session.authMode).toBe('required');
  });
});
