import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import SettingsWorkspace from './SettingsWorkspace.svelte';
import { createAPIClient } from '../../api/client';
import { chooseSelectOption } from '../../../test/kit-ui';

const initialSettings = {
  settings: [
    setting('web.theme', 'system', { options: ['system', 'light', 'dark'] }),
    setting('server.api_key', undefined, {
      kind: 'secret',
      read_only: true,
      secret: { configured: true, source: 'environment' }
    }),
    setting('vector.embeddings.endpoint', 'http://127.0.0.1:11434'),
    setting('vector.embeddings.api_key_env', 'MSGVAULT_EMBED_API_KEY', { read_only: true }),
    setting('integrations.tasks.api_key', undefined, {
      kind: 'secret',
      secret: { configured: false }
    })
  ],
  pending_restart: false
};

afterEach(() => vi.useRealTimers());

describe('SettingsWorkspace', () => {
  it('groups fields, redacts secrets, labels restart posture and warns on plain HTTP', async () => {
    render(SettingsWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => settingsResponse(initialSettings, '"etag-a"'))),
      plainHTTPWarning: true
    });

    expect(await screen.findByRole('heading', { name: 'Browser experience' })).toBeDefined();
    expect(screen.getByRole('main', { name: 'Settings' })).toBeDefined();
    await openSettingsCategory('Server access');
    expect(screen.getByText('Configured')).toBeDefined();
    await openSettingsCategory('Optional integrations');
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

    await chooseSelectOption(await screen.findByLabelText('Theme'), 'Dark');
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));

    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
    const request = fetchFn.mock.calls[1]?.[0] as Request;
    expect(request.method).toBe('PATCH');
    expect(request.headers.get('If-Match')).toBe('"etag-a"');
    await expect(request.clone().json()).resolves.toEqual({
      updates: [{ key: 'web.theme', value: { string: 'dark' } }]
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
    await chooseSelectOption(theme, 'Dark');
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));

    expect((await screen.findByRole('alert')).textContent).toContain('changed on disk');
    expect(fetchFn).toHaveBeenCalledTimes(3);
    expect(screen.getByRole('combobox', { name: 'Theme: Dark' })).toBeDefined();
  });

  it('keeps daemon authentication host-managed and never publishes an API-key input', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => settingsResponse(initialSettings, '"etag-a"'));
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('Server access');
    expect(await screen.findByText('Configured')).toBeDefined();
    expect(screen.getByText('Set via config.toml on the daemon host.')).toBeDefined();
    expect(screen.queryByLabelText('New daemon API key')).toBeNull();
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it('renders read-only settings without an input and excludes them from saves', async () => {
    const fetchFn = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
      .mockResolvedValueOnce(settingsResponse({ ...initialSettings, pending_restart: true }, '"etag-b"'));
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('Search and vectors');
    expect(await screen.findByText('MSGVAULT_EMBED_API_KEY')).toBeDefined();
    expect(screen.getByText('Set via config.toml on the daemon host.')).toBeDefined();
    expect(screen.queryByLabelText('Embedding key environment variable')).toBeNull();

    await openSettingsCategory('Browser experience');
    await chooseSelectOption(screen.getByLabelText('Theme'), 'Dark');
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
    const request = fetchFn.mock.calls[1]?.[0] as Request;
    await expect(request.clone().json()).resolves.toEqual({
      updates: [{ key: 'web.theme', value: { string: 'dark' } }]
    });
  });

  it('hides the Test connection button when no handler is provided', async () => {
    render(SettingsWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => settingsResponse(initialSettings, '"etag-a"')))
    });

    await screen.findByRole('heading', { name: 'Browser experience' });
    expect(screen.queryByRole('button', { name: 'Test embedding endpoint connection' })).toBeNull();
  });

  it('offers generic secret clearing without publishing fake connection actions', async () => {
    const fetchFn = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
      .mockResolvedValueOnce(settingsResponse({ ...initialSettings, pending_restart: true }, '"etag-b"'));
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('Optional integrations');
    await fireEvent.click(await screen.findByRole('button', { name: 'Clear task integration API key' }));
    await openSettingsCategory('Search and vectors');
    expect(screen.queryByRole('button', { name: /Test .* connection/i })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    const request = fetchFn.mock.calls[1]?.[0] as Request;
    await expect(request.clone().json()).resolves.toEqual({
      updates: [{ key: 'integrations.tasks.api_key', secret: { action: 'clear' } }]
    });
  });

  it('recovers from a rejected save without leaving the form stuck', async () => {
    const fetchFn = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
      .mockRejectedValueOnce(new Error('network unavailable'));
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await chooseSelectOption(await screen.findByLabelText('Theme'), 'Dark');
    await fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));

    expect((await screen.findByRole('alert')).textContent).toContain('network unavailable');
    expect((screen.getByRole('button', { name: 'Save settings' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('uses daemon metadata and writes provider credentials with the independent credential ETag', async () => {
    const document = approvedSettingsDocument();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'GET' && path === '/api/v1/settings') {
        return settingsResponse(document, '"config-a"', '"credential-a"');
      }
      if (request.method === 'PUT' && path === '/api/v1/settings/provider-credentials/vector.embeddings') {
        return Response.json({
          credential_id: 'vector.embeddings',
          state: { configured: true, source: 'stored' },
          pending_restart: true
        }, { headers: { ETag: '"credential-b"' } });
      }
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    expect(await screen.findByRole('heading', { name: 'Web appearance' })).toBeDefined();
    await openSettingsCategory('Search and embeddings');
    expect(screen.getByText('Vector search master switch')).toBeDefined();
    expect(screen.getByText('Environment variable')).toBeDefined();
    expect(screen.queryByRole('button', { name: /Test .* connection/i })).toBeNull();
    await fireEvent.input(screen.getByLabelText('New text embedding API key'), {
      target: { value: 'one-use-browser-secret' }
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Save text embedding API key' }));

    await waitFor(() => expect(requests).toHaveLength(2));
    const write = requests[1] as Request;
    expect(write.method).toBe('PUT');
    expect(write.headers.get('If-Match')).toBe('"credential-a"');
    await expect(write.clone().json()).resolves.toEqual({ value: 'one-use-browser-secret' });
    expect(await screen.findByText('Stored credential')).toBeDefined();
    expect((screen.getByLabelText('New text embedding API key') as HTMLInputElement).value).toBe('');
    expect(JSON.stringify(requests.map((request) => request.url))).not.toContain('one-use-browser-secret');
    expect(requests.some((request) => request.method === 'PATCH')).toBe(false);
  });

  it('requires endpoint settings to be saved before binding a provider credential', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return settingsResponse(approvedSettingsDocument(), '"config-a"', '"credential-a"');
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('Search and embeddings');
    await fireEvent.input(await screen.findByLabelText('Text embedding endpoint'), {
      target: { value: 'https://new-embedding.example.test/v1' }
    });

    const credential = screen.getByLabelText('New text embedding API key') as HTMLInputElement;
    expect(credential.disabled).toBe(true);
    expect(screen.getByText('Save endpoint settings first before changing this credential.')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Save text embedding API key' }) as HTMLButtonElement).disabled).toBe(true);
    expect(requests).toHaveLength(1);
  });

  it('renders attachment validation and future-download semantics from the daemon', async () => {
    render(SettingsWorkspace, {
      client: createAPIClient(vi.fn<typeof fetch>(async () =>
        settingsResponse(approvedSettingsDocument(), '"config-a"', '"credential-a"')))
    });

    await openSettingsCategory('Attachment downloads');
    expect(await screen.findByText('Controls future downloads only; existing files are unchanged.')).toBeDefined();
    const maximum = screen.getByLabelText('Discord maximum attachment size') as HTMLInputElement;
    expect(maximum.min).toBe('0');
    expect(screen.getByText('0 uses the Discord default of 50 MiB.')).toBeDefined();
    expect(screen.getByLabelText('Discord participant limit')).toBeDefined();
    expect(screen.getByRole('combobox', { name: 'Discord attachment scope: All' })).toBeDefined();
  });

  it('updates one stable-name enrichment provider without rewriting same-kind siblings', async () => {
    const initial = approvedSettingsDocument();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'GET' && path === '/api/v1/settings') {
        return settingsResponse(initial, '"config-a"', '"credential-a"');
      }
      if (request.method === 'PUT' && path === '/api/v1/settings/person-enrichment/providers/exa-primary') {
        const body = await request.clone().json();
        const providers = initial.person_enrichment_providers.map((provider) =>
          provider.name === 'exa-primary' ? { ...provider, enabled: body.enabled } : provider
        );
        return settingsResponse({ ...initial, person_enrichment_providers: providers }, '"config-b"', '"credential-a"');
      }
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('Person enrichment');
    expect(await screen.findByRole('heading', { name: 'exa-primary' })).toBeDefined();
    expect(screen.getByRole('heading', { name: 'exa-secondary' })).toBeDefined();
    await fireEvent.click(screen.getByLabelText('Enable exa-primary'));
    await fireEvent.click(screen.getByRole('button', { name: 'Save exa-primary provider' }));

    await waitFor(() => expect(requests).toHaveLength(2));
    const write = requests[1] as Request;
    expect(write.headers.get('If-Match')).toBe('"config-a"');
    const body = await write.clone().json();
    expect(body.kind).toBe('exa');
    expect(body.enabled).toBe(true);
    expect(body).not.toHaveProperty('name');
    expect(body).not.toHaveProperty('api_key_env');
    expect(body).not.toHaveProperty('max_cost_usd_micros_per_day');
    expect(requests.some((request) => request.method === 'PATCH')).toBe(false);
    expect(screen.getByLabelText('Enable exa-secondary')).toBeDefined();
  });

  it('requires a named provider endpoint draft to be saved before its credential', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return settingsResponse(approvedSettingsDocument(), '"config-a"', '"credential-a"');
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('Person enrichment');
    await fireEvent.input(await screen.findByLabelText('exa-primary endpoint'), {
      target: { value: 'https://new-exa.example.test/search' }
    });

    const credential = screen.getByLabelText('New exa API key for exa-primary') as HTMLInputElement;
    expect(credential.disabled).toBe(true);
    expect(screen.getByText('Save provider settings first before changing this credential.')).toBeDefined();
    expect((screen.getByRole('button', { name: 'Save exa API key for exa-primary' }) as HTMLButtonElement).disabled).toBe(true);
    expect(requests).toHaveLength(1);
  });

  it('creates an absent Exa provider by stable name with safe disabled defaults', async () => {
    const initial = { ...approvedSettingsDocument(), person_enrichment_providers: [] };
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'GET') return settingsResponse(initial, '"config-a"', '"credential-a"');
      if (request.method === 'PUT' && path === '/api/v1/settings/person-enrichment/providers/exa-primary') {
        const body = await request.clone().json();
        return settingsResponse({
          ...initial,
          person_enrichment_providers: [{
            name: 'exa-primary',
            ...body,
            credential: { configured: false, source: 'none' },
            credential_id: 'people.enrichment/exa-primary'
          }]
        }, '"config-b"', '"credential-a"');
      }
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('Person enrichment');
    await fireEvent.click(await screen.findByRole('button', { name: 'Add Exa provider' }));
    await fireEvent.input(screen.getByLabelText('Exa stable provider name'), {
      target: { value: 'exa-primary' }
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Create Exa provider' }));

    await waitFor(() => expect(requests).toHaveLength(2));
    const write = requests[1] as Request;
    expect(write.headers.get('If-Match')).toBe('"config-a"');
    const body = await write.clone().json();
    expect(body).toMatchObject({
      kind: 'exa',
      enabled: false,
      endpoint: 'https://api.exa.ai/search',
      mode: 'people',
      num_results: 1,
      allowed_identifiers: ['public_profile_url'],
      target_keys: ['attribute:location'],
      allow_sensitive_targets: false,
      retention_posture: '',
      training_posture: '',
      refresh_interval: '720h',
      request_timeout: '1m',
      max_retries: 5,
      max_requests_per_run: 10,
      max_requests_per_day: 100
    });
    expect(body).not.toHaveProperty('api_key_env');
    expect(body).not.toHaveProperty('max_cost_usd_micros_per_day');
    expect(await screen.findByRole('heading', { name: 'exa-primary' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Add Exa provider' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Add SixtyFour provider' })).toBeDefined();
  });

  it('rejects invalid or duplicate stable provider names before any write', async () => {
    const initial = approvedSettingsDocument();
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return settingsResponse(initial, '"config-a"', '"credential-a"');
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('Person enrichment');
    await fireEvent.click(await screen.findByRole('button', { name: 'Add SixtyFour provider' }));
    const name = screen.getByLabelText('SixtyFour stable provider name');
    await fireEvent.input(name, { target: { value: 'invalid/name' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create SixtyFour provider' }));
    expect(screen.getByRole('alert').textContent).toContain("letters, digits, '.', '_', ':', or '-'");

    await fireEvent.input(name, { target: { value: 'exa-primary' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create SixtyFour provider' }));
    expect(screen.getByRole('alert').textContent).toContain('already exists');
    expect(requests).toHaveLength(1);
  });

  it('tests CardDAV credentials through the dedicated account endpoint', async () => {
    const credentialFacts: Array<{ method: string; path: string; passwordMatched: boolean }> = [];
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(cardDAVSettings(), '"etag-a"');
      if (path === '/api/v1/carddav/account/test') {
        const body = await request.clone().json();
        expect(body).toEqual({
          base_url: 'https://dav.example.test/', username: 'alice', password: 'changed-password',
          enabled: true, schedule: '0 3 * * *'
        });
        credentialFacts.push({ method: request.method, path, passwordMatched: body.password === 'changed-password' });
        return Response.json({
          base_url: 'https://dav.example.test/',
          username: 'alice',
          enabled: true,
          schedule: '0 3 * * *',
          books: 2
        });
      }
      const readResponse = cardDAVOperationsResponse(request);
      if (readResponse) return readResponse;
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    };
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('CardDAV account');
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

    await waitFor(() => expect(credentialFacts).toEqual([
      { method: 'POST', path: '/api/v1/carddav/account/test', passwordMatched: true }
    ]));
    expect(JSON.stringify(credentialFacts)).not.toContain('changed-password');
    expect((await screen.findByRole('status')).textContent).toContain('Found 2 address books');
  });

  it('saves CardDAV credentials through PUT without a generic settings PATCH', async () => {
    const credentialFacts: Array<{ method: string; path: string; passwordPresent: boolean }> = [];
    let genericPatches = 0;
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(cardDAVSettings(), '"etag-a"');
      if (path === '/api/v1/carddav/account') {
        const body = await request.clone().json();
        expect(body).toEqual({
          base_url: 'https://old.example.test/', username: 'alice', enabled: false, schedule: '0 2 * * *'
        });
        credentialFacts.push({ method: request.method, path, passwordPresent: Object.hasOwn(body, 'password') });
        return Response.json({
          base_url: 'https://dav.example.test/',
          username: 'alice',
          enabled: false,
          schedule: '0 2 * * *',
          books: 1
        });
      }
      if (request.method === 'PATCH') genericPatches += 1;
      const readResponse = cardDAVOperationsResponse(request);
      if (readResponse) return readResponse;
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    };
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('CardDAV account');
    await screen.findByLabelText('Base URL');
    expect((screen.getByLabelText('Password') as HTMLInputElement).required).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));

    await waitFor(() => expect(credentialFacts).toEqual([
      { method: 'PUT', path: '/api/v1/carddav/account', passwordPresent: false }
    ]));
    expect(genericPatches).toBe(0);
    expect((await screen.findByRole('status')).textContent).toContain('CardDAV account saved');
  });

  it('requires a password before testing an unconfigured CardDAV account', async () => {
    let credentialRequests = 0;
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(initialSettings, '"etag-a"');
      if (path === '/api/v1/carddav/account/test') credentialRequests += 1;
      const readResponse = cardDAVOperationsResponse(request);
      if (readResponse) return readResponse;
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    };
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('CardDAV account');
    await fireEvent.input(await screen.findByLabelText('Base URL'), {
      target: { value: 'https://dav.example.test/' }
    });
    await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'alice' } });
    const password = screen.getByLabelText('Password') as HTMLInputElement;
    expect(password.required).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));

    expect(credentialRequests).toBe(0);
    expect(screen.getByRole('alert').textContent).toContain('Password is required');
  });

  it('renders unconfigured CardDAV conflict review as optional setup without retry or detail requests', async () => {
    const requests: Array<{ method: string; path: string }> = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      requests.push({ method: request.method, path });
      if (path === '/api/v1/settings') return settingsResponse(initialSettings, '"etag-a"');
      if (path === '/api/v1/carddav/status') return Response.json({
        configured: false, available: false, credential_configured: false,
        enabled: false, scheduled: false, schedule: ''
      });
      if (path === '/api/v1/carddav/books') return Response.json({ books: [] });
      if (path === '/api/v1/carddav/runs') return Response.json({ runs: [] });
      if (path === '/api/v1/carddav/conflicts') {
        return Response.json({ error: 'carddav_unavailable', message: 'synthetic setup detail' }, { status: 503 });
      }
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('CardDAV account');
    expect(await screen.findByText('CardDAV conflict review is unavailable.')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Retry CardDAV conflicts' })).toBeNull();
    expect(screen.queryByText('Unable to load CardDAV conflicts.')).toBeNull();
    expect(document.body.textContent).not.toContain('synthetic setup detail');
    expect(requests.some(({ method, path }) => method !== 'GET' || /^\/api\/v1\/carddav\/conflicts\//.test(path))).toBe(false);
  });

  it('requires a password when a configured CardDAV identity changes', async () => {
    let credentialRequests = 0;
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(cardDAVSettings(), '"etag-a"');
      if (path === '/api/v1/carddav/account/test') credentialRequests += 1;
      const readResponse = cardDAVOperationsResponse(request);
      if (readResponse) return readResponse;
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    };
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('CardDAV account');
    const baseURL = await screen.findByLabelText('Base URL');
    const password = screen.getByLabelText('Password') as HTMLInputElement;
    expect(password.required).toBe(false);

    await fireEvent.input(baseURL, { target: { value: 'https://changed.example.test/' } });
    expect(password.required).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));

    expect(credentialRequests).toBe(0);
    expect(screen.getByRole('alert').textContent).toContain('Password is required');
  });

  it('refreshes the saved account snapshot while retaining unrelated drafts across category navigation', async () => {
    const credentialFacts: Array<{ method: string; path: string; passwordMatched: boolean }> = [];
    let settingsReads = 0;
    let operationsReads = 0;
    let saved = false;
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') {
        settingsReads += 1;
        return settingsResponse(saved
          ? cardDAVSettings({ baseURL: 'https://saved.example.test/', username: 'saved-user', schedule: '0 6 * * *' })
          : cardDAVSettings(), '"etag-a"');
      }
      if (path === '/api/v1/carddav/account') {
        const body = await request.clone().json();
        expect(body).toEqual({
          base_url: 'https://saved.example.test/', username: 'saved-user', password: 'one-use-password',
          enabled: false, schedule: '0 6 * * *'
        });
        credentialFacts.push({ method: request.method, path, passwordMatched: body.password === 'one-use-password' });
        saved = true;
        return Response.json({
          base_url: body.base_url, username: body.username, enabled: body.enabled,
          schedule: body.schedule, books: 1
        });
      }
      const readResponse = cardDAVOperationsResponse(request);
      if (readResponse) {
        operationsReads += 1;
        return readResponse;
      }
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    };
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await chooseSelectOption(await screen.findByLabelText('Theme'), 'Dark');
    await openSettingsCategory('CardDAV account');
    await fireEvent.input(await screen.findByLabelText('Base URL'), { target: { value: 'https://saved.example.test/' } });
    await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'saved-user' } });
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'one-use-password' } });
    await fireEvent.input(screen.getByLabelText('Schedule'), { target: { value: '0 6 * * *' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));

    await waitFor(() => expect(credentialFacts).toEqual([
      { method: 'PUT', path: '/api/v1/carddav/account', passwordMatched: true }
    ]));
    expect(JSON.stringify(credentialFacts)).not.toContain('one-use-password');
    await waitFor(() => expect(settingsReads).toBe(2));
    await waitFor(() => expect(operationsReads).toBe(8));

    await openSettingsCategory('Browser experience');
    expect(screen.getByRole('combobox', { name: 'Theme: Dark' })).toBeDefined();
    await openSettingsCategory('CardDAV account');
    expect((await screen.findByLabelText('Base URL') as HTMLInputElement).value).toBe('https://saved.example.test/');
    expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('saved-user');
    expect((screen.getByLabelText('Schedule') as HTMLInputElement).value).toBe('0 6 * * *');
    expect((screen.getByLabelText('Password') as HTMLInputElement).required).toBe(false);
  });

  it('reuses the persisted CardDAV password after the first successful save', async () => {
    const credentialFacts: Array<{ passwordMatched: boolean; passwordPresent: boolean; schedule: string }> = [];
    let saved = false;
    let savedSchedule = '0 2 * * *';
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') {
        return settingsResponse(saved
          ? cardDAVSettings({ baseURL: 'https://dav.example.test/', username: 'alice', schedule: savedSchedule })
          : initialSettings, '"etag-a"');
      }
      if (path === '/api/v1/carddav/account') {
        const body = await request.clone().json();
        const first = credentialFacts.length === 0;
        if (first) {
          expect(body).toEqual({
            base_url: 'https://dav.example.test/', username: 'alice', password: 'first-password',
            enabled: false, schedule: '0 2 * * *'
          });
        } else {
          expect(body).toEqual({
            base_url: 'https://dav.example.test/', username: 'alice', enabled: false, schedule: '0 4 * * *'
          });
        }
        credentialFacts.push({
          passwordMatched: body.password === 'first-password',
          passwordPresent: Object.hasOwn(body, 'password'),
          schedule: String(body.schedule)
        });
        saved = true;
        savedSchedule = String(body.schedule);
        return Response.json({
          base_url: body.base_url,
          username: body.username,
          enabled: body.enabled,
          schedule: body.schedule,
          books: 1
        });
      }
      const readResponse = cardDAVOperationsResponse(request);
      if (readResponse) return readResponse;
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    };
    render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('CardDAV account');
    await fireEvent.input(await screen.findByLabelText('Base URL'), {
      target: { value: 'https://dav.example.test/' }
    });
    await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'alice' } });
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'first-password' } });
    await fireEvent.input(screen.getByLabelText('Schedule'), { target: { value: '0 2 * * *' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
    await waitFor(() => expect(credentialFacts).toHaveLength(1));
    const password = await screen.findByLabelText('Password') as HTMLInputElement;
    await waitFor(() => expect(password.value).toBe(''));
    expect(password.required).toBe(false);
    await fireEvent.input(screen.getByLabelText('Schedule'), { target: { value: '0 4 * * *' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
    await waitFor(() => expect(credentialFacts).toHaveLength(2));
    expect(credentialFacts).toEqual([
      { passwordMatched: true, passwordPresent: true, schedule: '0 2 * * *' },
      { passwordMatched: false, passwordPresent: false, schedule: '0 4 * * *' }
    ]);
    expect(JSON.stringify(credentialFacts)).not.toContain('first-password');
  });

  it('destroys CardDAV on category exit, clears its password, aborts polling, and remounts fresh reads', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let statusReads = 0;
    let booksReads = 0;
    let runsReads = 0;
    let pollSignal: AbortSignal | undefined;
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return Promise.resolve(settingsResponse(cardDAVSettings(), '"etag-a"'));
      if (path === '/api/v1/carddav/books') {
        booksReads += 1;
        return Promise.resolve(Response.json({ books: [] }));
      }
      if (path === '/api/v1/carddav/runs') {
        runsReads += 1;
        return Promise.resolve(Response.json({ runs: [] }));
      }
      if (path === '/api/v1/carddav/status') {
        statusReads += 1;
        if (statusReads === 1) return Promise.resolve(Response.json({
          configured: true, available: true, credential_configured: true, enabled: true,
          scheduled: true, schedule: '0 2 * * *', active: {
            id: 9, trigger: 'manual', full: false, state: 'running', started_at: '2026-08-28T10:00:00Z',
            books: 1, created: 0, updated: 1, removed: 0
          }
        }));
        if (statusReads === 2) {
          pollSignal = request.signal;
          return new Promise<Response>(() => undefined);
        }
        return Promise.resolve(Response.json({
          configured: true, available: true, credential_configured: true, enabled: true,
          scheduled: true, schedule: '0 2 * * *'
        }));
      }
      throw new Error(`Unexpected request: ${request.method} ${path}`);
    });
    const rendered = render(SettingsWorkspace, { client: createAPIClient(fetchFn) });

    await openSettingsCategory('CardDAV account');
    await screen.findByLabelText('Password');
    await waitFor(() => expect([statusReads, booksReads, runsReads]).toEqual([1, 1, 1]));
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-password' } });
    expect(rendered.container.querySelector('.kit-settings__footer')).toBeNull();
    await vi.advanceTimersByTimeAsync(500);
    await waitFor(() => expect(pollSignal).toBeDefined());

    await openSettingsCategory('Browser experience');
    expect(pollSignal?.aborted).toBe(true);
    expect(screen.queryByLabelText('Password')).toBeNull();

    await openSettingsCategory('CardDAV account');
    await waitFor(() => expect([statusReads, booksReads, runsReads]).toEqual([3, 2, 2]));
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('');
    rendered.unmount();
  });

  it('consumes keyed CardDAV conflict handoffs once and focuses the exact non-dialog detail surface', async () => {
    const detailRequests: number[] = [];
    const onCardDAVRequestConsumed = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/settings') return settingsResponse(cardDAVSettings(), '"etag-a"');
      if (path === '/api/v1/carddav/conflicts/41') {
        detailRequests.push(41);
        return Response.json({
          id: 41,
          address_book: { id: 5, name: 'Synthetic contacts' },
          status: 'unresolved',
          base: { state: 'present', display_name: 'Base person', emails: [], phones: [] },
          local: { state: 'present', display_name: 'Local person', emails: [], phones: [] },
          remote: { state: 'deleted', emails: [], phones: [] },
          allowed_resolutions: ['keep_local', 'keep_remote'],
          created_at: '2026-08-28T10:00:00Z',
          updated_at: '2026-08-28T11:00:00Z'
        });
      }
      const operations = cardDAVOperationsResponse(request);
      if (operations) return operations;
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const rendered = render(SettingsWorkspace, {
      client: createAPIClient(fetchFn),
      cardDAVRequest: { conflictID: 41, key: 1 },
      onCardDAVRequestConsumed
    });

    const heading = await screen.findByRole('heading', { name: 'Conflict comparison' });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(detailRequests).toEqual([41]);
    expect(onCardDAVRequestConsumed.mock.calls).toEqual([[1]]);
    expect(screen.queryByRole('dialog')).toBeNull();

    await rendered.rerender({
      client: createAPIClient(fetchFn),
      cardDAVRequest: { conflictID: 41, key: 1 },
      onCardDAVRequestConsumed
    });
    await Promise.resolve();
    expect(detailRequests).toEqual([41]);
    await rendered.rerender({
      client: createAPIClient(fetchFn),
      cardDAVRequest: { conflictID: 41, key: 2 },
      onCardDAVRequestConsumed
    });
    await waitFor(() => expect(detailRequests).toEqual([41, 41]));
    await waitFor(() => expect(onCardDAVRequestConsumed.mock.calls).toEqual([[1], [2]]));
    rendered.unmount();
  });
});

async function openSettingsCategory(label: string): Promise<void> {
  await fireEvent.click(await screen.findByRole('button', { name: new RegExp(`^${label}`) }));
}

function cardDAVSettings({
  baseURL = 'https://old.example.test/',
  username = 'alice',
  schedule = '0 2 * * *'
}: { baseURL?: string; username?: string; schedule?: string } = {}): object {
  return {
    settings: [
      ...initialSettings.settings,
      setting('carddav.base_url', baseURL),
      setting('carddav.username', username),
      setting('carddav.password', undefined, { kind: 'secret', secret: { configured: true } }),
      setting('carddav.enabled', false, { kind: 'boolean' }),
      setting('carddav.schedule', schedule)
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

function settingsResponse(body: object, etag: string, credentialETag?: string): Response {
  return Response.json(body, { headers: {
    ETag: etag,
    ...(credentialETag ? { 'Credential-ETag': credentialETag } : {})
  } });
}

function approvedSettingsDocument() {
  const provider = (name: string) => ({
    name,
    kind: 'exa',
    enabled: false,
    endpoint: 'https://api.exa.ai/search',
    mode: 'people',
    num_results: 1,
    allowed_identifiers: ['email'],
    target_keys: ['job_title'],
    allow_sensitive_targets: false,
    retention_posture: 'zero_retention',
    training_posture: 'no_training',
    refresh_interval: '720h',
    request_timeout: '1m',
    poll_interval: '30s',
    max_job_age: '15m',
    max_retries: 5,
    max_requests_per_run: 10,
    max_requests_per_day: 100,
    credential: { configured: false, source: 'none' },
    credential_id: `people.enrichment/${name}`
  });
  return {
    groups: [
      { id: 'browser', label: 'Web appearance', description: 'Browser preferences.' },
      { id: 'search', label: 'Search and embeddings', description: 'Vector and provider controls.' },
      { id: 'attachments', label: 'Attachment downloads', description: 'Controls future downloads only; existing files are unchanged.' },
      { id: 'enrichment', label: 'Person enrichment', description: 'Named provider policies.' }
    ],
    settings: [
      daemonSetting('web.theme', 'browser', 'Theme', 'Browser color theme.', 'string', 'system', {
        options: ['system', 'light', 'dark'], restart_required: false
      }),
      daemonSetting('vector.enabled', 'search', 'Vector search master switch', 'Master gate for text search.', 'boolean', false),
      daemonSetting('vector.embeddings.endpoint', 'search', 'Text embedding endpoint', 'Provider API root.', 'string', 'https://old-embedding.example.test/v1'),
      daemonSetting('vector.embeddings.api_key', 'search', 'Text embedding API key', 'Write-only provider credential.', 'secret', undefined, {
        secret: { configured: true, source: 'environment' }, credential_id: 'vector.embeddings'
      }),
      daemonSetting('vector.multimodal.enabled', 'search', 'Visual Voyage embeddings', 'Additional visual lane gate.', 'boolean', false),
      daemonSetting('vector.multimodal.api_key', 'search', 'Voyage API key', 'Write-only provider credential.', 'secret', undefined, {
        secret: { configured: false, source: 'none' }, credential_id: 'vector.multimodal'
      }),
      daemonSetting('discord.media', 'attachments', 'Download Discord attachments', 'Future downloads only.', 'boolean', true),
      daemonSetting('discord.media_scope', 'attachments', 'Discord attachment scope', 'Conversation scope.', 'string', 'all', {
        options: ['all', 'direct', 'none']
      }),
      daemonSetting('discord.media_max_participants', 'attachments', 'Discord participant limit', 'Skip conversations over this size.', 'integer', 0, {
        validation: { minimum: 0, hint: '0 means no participant limit.' }
      }),
      daemonSetting('discord.max_media_mb', 'attachments', 'Discord maximum attachment size', 'Maximum future file size.', 'integer', 0, {
        validation: { minimum: 0, hint: '0 uses the Discord default of 50 MiB.' }
      }),
      daemonSetting('people.enrichment.enabled', 'enrichment', 'Enable person enrichment', 'Global enrichment gate.', 'boolean', false)
    ],
    person_enrichment_providers: [provider('exa-primary'), provider('exa-secondary')],
    credential_etag: '"credential-a"',
    pending_restart: false
  };
}

function daemonSetting(
  key: string,
  group: string,
  label: string,
  description: string,
  kind: string,
  value: unknown,
  overrides: Record<string, unknown> = {}
) {
  return {
    key, group, label, description, kind,
    value: value === undefined ? undefined : typedValue(value),
    restart_required: true,
    ...overrides
  };
}

function cardDAVOperationsResponse(request: Request): Response | undefined {
  if (request.method !== 'GET') return undefined;
  const path = new URL(request.url).pathname;
  if (path === '/api/v1/carddav/status') return Response.json({
    configured: true,
    available: true,
    credential_configured: true,
    enabled: false,
    scheduled: false,
    schedule: ''
  });
  if (path === '/api/v1/carddav/books') return Response.json({ books: [] });
  if (path === '/api/v1/carddav/runs') return Response.json({ runs: [] });
  if (path === '/api/v1/carddav/conflicts') return Response.json({ conflicts: [] });
  return undefined;
}
