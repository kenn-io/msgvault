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
});

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
