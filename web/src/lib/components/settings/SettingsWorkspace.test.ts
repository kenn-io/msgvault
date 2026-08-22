import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import SettingsWorkspace from './SettingsWorkspace.svelte';
import { createAPIClient } from '../../api/client';

const initialSettings = {
  settings: [
    setting('web.theme', 'system', { options: ['system', 'light', 'dark'] }),
    setting('server.api_key', undefined, { kind: 'secret', secret: { configured: true } }),
    setting('vector.embeddings.endpoint', 'http://127.0.0.1:11434', { testable: true }),
    setting('vector.embeddings.api_key_env', 'MSGVAULT_EMBED_API_KEY', { read_only: true }),
    setting('integrations.tasks.api_key', undefined, {
      kind: 'secret',
      secret: { configured: false }
    })
  ],
  pending_restart: false
};

describe('SettingsWorkspace', () => {
  it('groups fields, redacts secrets, labels restart posture and warns on plain HTTP', async () => {
    render(SettingsWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => settingsResponse(initialSettings, '"etag-a"'))),
      plainHTTPWarning: true
    });

    expect(await screen.findByRole('heading', { name: 'Browser experience' })).toBeDefined();
    expect(screen.getByRole('main', { name: 'Settings' })).toBeDefined();
    expect(screen.getByText('Set')).toBeDefined();
    expect(screen.getByText('Not set')).toBeDefined();
    expect(screen.getAllByText('Restart required').length).toBeGreaterThan(0);
    expect(screen.getByRole('alert').textContent).toContain('plain HTTP');
  });

  it('patches only changed values with If-Match and shows pending restart', async () => {
    const fetchFn = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
      .mockResolvedValueOnce(
        settingsResponse(
          {
            ...initialSettings,
            settings: initialSettings.settings.map((item) =>
              item.key === 'web.theme' ? { ...item, value: { string: 'dark' } } : item
            ),
            pending_restart: true
          },
          '"etag-b"'
        )
      );
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.change(await screen.findByLabelText('Theme'), { target: { value: 'dark' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
    const request = fetchFn.mock.calls[1]?.[0] as Request;
    expect(request.method).toBe('PATCH');
    expect(request.headers.get('If-Match')).toBe('"etag-a"');
    await expect(request.clone().json()).resolves.toEqual({
      updates: [{ key: 'web.theme', value: { string: 'dark' } }],
      confirm_api_key_restart: false
    });
    expect(await screen.findByText('Changes are pending restart.')).toBeDefined();
  });

  it('reloads the latest ETag after a conflict while retaining the local draft', async () => {
    const latest = {
      ...initialSettings,
      settings: initialSettings.settings.map((item) =>
        item.key === 'web.theme' ? { ...item, value: { string: 'light' } } : item
      )
    };
    const fetchFn = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
      .mockResolvedValueOnce(Response.json({ error: 'settings_conflict' }, { status: 412 }))
      .mockResolvedValueOnce(settingsResponse(latest, '"etag-latest"'));
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    const theme = await screen.findByLabelText('Theme');
    await fireEvent.change(theme, { target: { value: 'dark' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));

    expect((await screen.findByRole('alert')).textContent).toContain('changed on disk');
    expect(fetchFn).toHaveBeenCalledTimes(3);
    expect((screen.getByLabelText('Theme') as HTMLSelectElement).value).toBe('dark');
  });

  it('requires explicit API-key restart confirmation before sending the secret', async () => {
    const fetchFn = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
      .mockResolvedValueOnce(settingsResponse({ ...initialSettings, pending_restart: true }, '"etag-b"'));
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.input(await screen.findByLabelText('New daemon API key'), {
      target: { value: 'replacement-key' }
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(fetchFn).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('alert').textContent).toContain('confirm');

    await fireEvent.click(screen.getByLabelText('I understand the API key changes after restart'));
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
    const request = fetchFn.mock.calls[1]?.[0] as Request;
    await expect(request.clone().json()).resolves.toEqual({
      updates: [
        {
          key: 'server.api_key',
          secret: { action: 'set', value: 'replacement-key' }
        }
      ],
      confirm_api_key_restart: true
    });
  });

  it('renders read-only settings without an input and excludes them from saves', async () => {
    const fetchFn = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
      .mockResolvedValueOnce(settingsResponse({ ...initialSettings, pending_restart: true }, '"etag-b"'));
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByText('MSGVAULT_EMBED_API_KEY')).toBeDefined();
    expect(screen.getByText('Set via config.toml on the daemon host.')).toBeDefined();
    expect(screen.queryByLabelText('Embedding key environment variable')).toBeNull();

    await fireEvent.change(screen.getByLabelText('Theme'), { target: { value: 'dark' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
    const request = fetchFn.mock.calls[1]?.[0] as Request;
    await expect(request.clone().json()).resolves.toEqual({
      updates: [{ key: 'web.theme', value: { string: 'dark' } }],
      confirm_api_key_restart: false
    });
  });

  it('hides the Test connection button when no handler is provided', async () => {
    render(SettingsWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => settingsResponse(initialSettings, '"etag-a"')))
    });

    await screen.findByRole('heading', { name: 'Browser experience' });
    expect(screen.queryByRole('button', { name: 'Test embedding endpoint connection' })).toBeNull();
  });

  it('offers secret clearing and test-connection hooks without exposing values', async () => {
    const onTestConnection = vi.fn(async () => undefined);
    const fetchFn = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
      .mockResolvedValueOnce(settingsResponse({ ...initialSettings, pending_restart: true }, '"etag-b"'));
    render(SettingsWorkspace, { client: createAPIClient(fetchFn), onTestConnection });

    await fireEvent.click(await screen.findByRole('button', { name: 'Clear task integration API key' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Test embedding endpoint connection' }));
    expect(onTestConnection).toHaveBeenCalledWith('vector.embeddings.endpoint');
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    const request = fetchFn.mock.calls[1]?.[0] as Request;
    await expect(request.clone().json()).resolves.toEqual({
      updates: [{ key: 'integrations.tasks.api_key', secret: { action: 'clear' } }],
      confirm_api_key_restart: false
    });
  });

  it('recovers from a rejected save without leaving the form stuck', async () => {
    const fetchFn = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
      .mockRejectedValueOnce(new Error('network unavailable'));
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.change(await screen.findByLabelText('Theme'), { target: { value: 'dark' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));

    expect((await screen.findByRole('alert')).textContent).toContain('network unavailable');
    expect((screen.getByRole('button', { name: 'Save settings' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('tests CardDAV credentials through the dedicated account endpoint', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(cardDAVSettings(), '"etag-a"');
      if (path === '/api/v1/carddav/account/test') {
        return Response.json({
          base_url: 'https://dav.example.test/',
          username: 'alice',
          enabled: true,
          schedule: '0 3 * * *',
          books: 2
        });
      }
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByRole('heading', { name: 'CardDAV account' })).toBeDefined();
    expect(screen.getByLabelText('Base URL')).toBeDefined();
    expect(screen.getByLabelText('Username')).toBeDefined();
    expect(screen.getByLabelText('Password')).toBeDefined();
    expect(screen.getByLabelText('Enabled')).toBeDefined();
    expect(screen.getByLabelText('Schedule')).toBeDefined();
    expect(screen.queryByText('CardDAV server')).toBeNull();

    await fireEvent.input(screen.getByLabelText('Base URL'), {
      target: { value: 'https://dav.example.test/' }
    });
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'changed-password' } });
    await fireEvent.click(screen.getByLabelText('Enabled'));
    await fireEvent.input(screen.getByLabelText('Schedule'), { target: { value: '0 3 * * *' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
    const request = fetchFn.mock.calls[1]?.[0] as Request;
    expect(request.method).toBe('POST');
    expect(new URL(request.url).pathname).toBe('/api/v1/carddav/account/test');
    await expect(request.clone().json()).resolves.toEqual({
      base_url: 'https://dav.example.test/',
      username: 'alice',
      password: 'changed-password',
      enabled: true,
      schedule: '0 3 * * *'
    });
    expect((await screen.findByRole('status')).textContent).toContain('Found 2 address books');
  });

  it('saves CardDAV credentials through PUT without a generic settings PATCH', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(cardDAVSettings(), '"etag-a"');
      if (path === '/api/v1/carddav/account') {
        return Response.json({
          base_url: 'https://dav.example.test/',
          username: 'alice',
          enabled: false,
          schedule: '0 2 * * *',
          books: 1
        });
      }
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await screen.findByLabelText('Base URL');
    expect((screen.getByLabelText('Password') as HTMLInputElement).required).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
    const request = fetchFn.mock.calls[1]?.[0] as Request;
    expect(request.method).toBe('PUT');
    expect(new URL(request.url).pathname).toBe('/api/v1/carddav/account');
    await expect(request.clone().json()).resolves.toEqual({
      base_url: 'https://old.example.test/',
      username: 'alice',
      enabled: false,
      schedule: '0 2 * * *'
    });
    expect(
      fetchFn.mock.calls.some(([candidate]) => candidate instanceof Request && candidate.method === 'PATCH')
    ).toBe(false);
    expect((await screen.findByRole('status')).textContent).toContain('CardDAV account saved');
  });

  it('requires a password before testing an unconfigured CardDAV account', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(initialSettings, '"etag-a"');
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.input(await screen.findByLabelText('Base URL'), {
      target: { value: 'https://dav.example.test/' }
    });
    await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'alice' } });
    const password = screen.getByLabelText('Password') as HTMLInputElement;
    expect(password.required).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));

    expect(fetchFn).toHaveBeenCalledOnce();
    expect(screen.getByRole('alert').textContent).toContain('Password is required');
  });

  it('requires a password when a configured CardDAV identity changes', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(cardDAVSettings(), '"etag-a"');
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    const baseURL = await screen.findByLabelText('Base URL');
    const password = screen.getByLabelText('Password') as HTMLInputElement;
    expect(password.required).toBe(false);

    await fireEvent.input(baseURL, { target: { value: 'https://changed.example.test/' } });
    expect(password.required).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));

    expect(fetchFn).toHaveBeenCalledOnce();
    expect(screen.getByRole('alert').textContent).toContain('Password is required');
  });

  it('reuses the persisted CardDAV password after the first successful save', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(initialSettings, '"etag-a"');
      if (path === '/api/v1/carddav/account') {
        requests.push(request);
        const body = (await request.clone().json()) as Record<string, unknown>;
        return Response.json({
          base_url: body.base_url,
          username: body.username,
          enabled: body.enabled,
          schedule: body.schedule,
          books: 1
        });
      }
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await fireEvent.input(await screen.findByLabelText('Base URL'), {
      target: { value: 'https://dav.example.test/' }
    });
    await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'alice' } });
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'first-password' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    await screen.findByRole('status');

    const password = screen.getByLabelText('Password') as HTMLInputElement;
    expect(password.value).toBe('');
    expect(password.required).toBe(false);
    await fireEvent.input(screen.getByLabelText('Schedule'), { target: { value: '0 4 * * *' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
    await waitFor(() => expect(requests).toHaveLength(2));

    await expect(requests[0].clone().json()).resolves.toMatchObject({ password: 'first-password' });
    await expect(requests[1].clone().json()).resolves.toEqual({
      base_url: 'https://dav.example.test/',
      username: 'alice',
      enabled: false,
      schedule: '0 4 * * *'
    });
  });
});

function cardDAVSettings(): object {
  return {
    settings: [
      ...initialSettings.settings,
      setting('carddav.base_url', 'https://old.example.test/'),
      setting('carddav.username', 'alice'),
      setting('carddav.password', undefined, { kind: 'secret', secret: { configured: true } }),
      setting('carddav.enabled', false, { kind: 'boolean' }),
      setting('carddav.schedule', '0 2 * * *')
    ],
    pending_restart: false
  };
}

function setting(
  key: string,
  value: unknown,
  overrides: Record<string, unknown> = {}
): Record<string, unknown> {
  return {
    key,
    group: 'ignored',
    kind: 'string',
    value: value === undefined ? undefined : typedValue(value),
    restart_required: true,
    ...overrides
  };
}

function typedValue(value: unknown): Record<string, unknown> {
  if (typeof value === 'boolean') return { boolean: value };
  if (typeof value === 'number') return Number.isInteger(value) ? { integer: value } : { number: value };
  if (Array.isArray(value)) return { strings: value };
  return { string: value };
}

function settingsResponse(body: object, etag: string): Response {
  return Response.json(body, { headers: { ETag: etag } });
}
