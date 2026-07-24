import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createSessionController } from '../../api/session.svelte';
import Login from './Login.svelte';

function response(status: number) {
  if (status === 200) {
    return Response.json({
      auth_mode: 'session',
      csrf_token: 'csrf-token',
      https: true,
      plain_http_warning: false
    });
  }
  return Response.json({ error: 'unauthorized', message: 'Invalid API key' }, { status });
}

describe('Login', () => {
  it('exchanges the API key and leaves required mode on success', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => response(200));
    const session = createSessionController(fetchFn);
    render(Login, { session });

    await fireEvent.input(screen.getByLabelText('API key'), { target: { value: 'test-key' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Log in' }));

    await waitFor(() => expect(session.authMode).toBe('session'));
    const request = fetchFn.mock.calls[0]?.[0] as Request;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe('/api/session/login');
    await expect(request.clone().json()).resolves.toEqual({ api_key: 'test-key' });
  });

  it('shows the server error after a rejected key', async () => {
    const session = createSessionController(vi.fn<typeof fetch>(async () => response(401)));
    render(Login, { session });

    await fireEvent.input(screen.getByLabelText('API key'), { target: { value: 'wrong-key' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Log in' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Invalid API key');
    expect(session.authMode).toBe('required');
  });
});
