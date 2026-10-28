import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import SettingsWorkspace from './SettingsWorkspace.svelte';
import { createAPIClient } from '../../api/client';
import { chooseSelectOption } from '../../../test/kit-ui';

type Setting = Record<string, unknown>;

function typed(value: unknown): Record<string, unknown> {
  if (typeof value === 'boolean') return { boolean: value };
  if (typeof value === 'number') return Number.isInteger(value) ? { integer: value } : { number: value };
  if (Array.isArray(value)) return { strings: value };
  return { string: value };
}

function setting(key: string, group: string, kind: string, value: unknown, extra: Setting = {}): Setting {
  return {
    key, group, kind, label: key, restart_required: false,
    value: value === undefined ? undefined : typed(value), ...extra
  };
}

function settingsDoc(overrides: Setting = {}) {
  return {
    groups: [],
    settings: [
      setting('web.theme', 'browser', 'string', 'system', {
        label: 'Theme', options: ['system', 'light', 'dark'] }),
      setting('web.density', 'browser', 'string', 'compact', {
        label: 'Density', options: ['compact', 'comfortable'] }),
      setting('web.default_search_mode', 'browser', 'string', 'full_text', {
        label: 'Default search mode', options: ['full_text', 'semantic', 'hybrid'] }),
      setting('server.bind_addr', 'server', 'string', '127.0.0.1', { label: 'Bind address', read_only: true }),
      setting('server.api_key', 'server', 'secret', undefined, {
        label: 'API key', read_only: true, secret: { configured: true, source: 'environment', hint: 'abc…xyz' } }),
      setting('server.daemon_idle_timeout', 'server', 'string', '20m0s', { label: 'Idle timeout' }),
      setting('log.sql_trace', 'server', 'boolean', false, { label: 'Trace every SQL statement' }),
      setting('activity.timezone', 'archive', 'string', 'UTC', { label: 'Timezone' }),
      setting('analytics.builder_threads', 'archive', 'integer', 4, { label: 'Builder threads' }),
      setting('sync.rate_limit_qps', 'archive', 'number', 5, { label: 'Requests per second' }),
      setting('vector.enabled', 'search', 'boolean', false, { label: 'Semantic search' }),
      setting('vector.embeddings.endpoint', 'search', 'string', '', { label: 'Text embedding endpoint' }),
      setting('vector.embeddings.model', 'search', 'string', '', { label: 'Text embedding model' }),
      setting('vector.embeddings.api_key', 'search', 'secret', undefined, {
        label: 'Text embedding API key', credential_id: 'vector.embeddings',
        secret: { configured: false, source: 'none' } }),
      setting('vector.search.rrf_k', 'search', 'integer', 60, { label: 'RRF constant' }),
      setting('integrations.kata.enabled', 'integrations', 'boolean', false, { label: 'Kata person agendas' }),
      setting('integrations.kata.api_key', 'integrations', 'secret', undefined, {
        label: 'Kata API key', secret: { configured: false, source: 'none' } }),
      setting('integrations.tasks.enabled', 'integrations', 'boolean', true, { label: 'Task integration' }),
      setting('beeper.schedule', 'sources', 'string', '', { label: 'Beeper schedule' }),
      setting('discord.media', 'attachments', 'boolean', true, { label: 'Download Discord attachments' }),
      setting('people.enrichment.enabled', 'enrichment', 'boolean', false, { label: 'Enable person enrichment' }),
    ],
    person_enrichment_providers: [],
    credential_etag: '"credential-a"',
    pending_restart: false,
    ...overrides
  };
}

interface Seen { method: string; path: string; ifMatch: string | null; body?: unknown }

function fixture(doc: ReturnType<typeof settingsDoc> = settingsDoc(), extra?: (request: Request) => Response | undefined) {
  const seen: Seen[] = [];
  let etag = 1;
  const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input as Request;
    const path = new URL(request.url).pathname;
    const body = request.method === 'GET' || request.method === 'DELETE' ? undefined : await request.clone().json();
    seen.push({ method: request.method, path, ifMatch: request.headers.get('If-Match'), body });
    const custom = extra?.(request);
    if (custom) return custom;
    if (path === '/api/v1/settings' && request.method === 'GET') {
      return Response.json(doc, { headers: { ETag: `"s${etag}"`, 'Credential-ETag': '"credential-a"' } });
    }
    if (path === '/api/v1/settings' && request.method === 'PATCH') {
      for (const update of (body as { updates: { key: string; value?: Record<string, unknown> }[] }).updates) {
        const target = (doc.settings as Setting[]).find((candidate) => candidate.key === update.key);
        if (target && update.value) target.value = update.value;
      }
      etag += 1;
      return Response.json({ ...doc, pending_restart: true }, { headers: { ETag: `"s${etag}"` } });
    }
    if (path.startsWith('/api/v1/settings/person-enrichment/providers/') && request.method === 'PUT') {
      doc.person_enrichment_providers = (doc.person_enrichment_providers as Record<string, unknown>[]).map(provider => path.endsWith('/' + provider.name) ? { ...provider, ...body as object } : provider) as never[];
      etag += 1;
      return Response.json(doc, { headers: { ETag: `"s${etag}"` } });
    }
    if (path.startsWith('/api/v1/settings/provider-credentials/')) {
      return Response.json(
        { credential_id: decodeURIComponent(path.split('/').at(-1)!), state: { configured: true, source: 'stored', hint: '•••' }, pending_restart: false },
        { headers: { ETag: '"credential-b"' } }
      );
    }
    if (path === '/api/v1/relationship-types') return Response.json({ relationship_types: [] });
    if (path === '/api/v1/attribute-definitions') return Response.json({ definitions: [] });
    if (path.startsWith('/api/v1/carddav')) {
      if (path.endsWith('/connections')) return Response.json({ connections: [] });
      if (path.endsWith('/status')) {
        return Response.json({ configured: false, available: false, credential_configured: false, enabled: false,
          scheduled: false, schedule: '' });
      }
      if (path.endsWith('/books')) return Response.json({ books: [] });
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      return Response.json({ conflicts: [] });
    }
    if (path.includes('/embeddings/status')) return Response.json({}, { status: 503 });
    if (path.includes('/multimodal/status')) return Response.json({}, { status: 503 });
    if (path.includes('/settings/people-inference')) {
      return Response.json({ profiles: [], configured_enabled: false, running_enabled: false, pending_restart: false },
        { headers: { ETag: '"p1"' } });
    }
    return Response.json({}, { status: 404 });
  }));
  return { client, seen };
}

afterEach(() => vi.restoreAllMocks());

describe('SettingsWorkspace', () => {
  it('lists the categories in order, Overview first, and opens the one it is given', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'advanced' });
    await screen.findByRole('heading', { name: 'Advanced', level: 2 });
    const names = [...window.document.querySelectorAll('.kit-settings__nav-item')].map((item) => item.textContent?.trim());
    expect(names).toEqual([
      'Overview', 'Server', 'Search & AI', 'People lookups', 'Tasks', 'Contacts', 'Sources', 'Advanced'
    ]);
    expect(screen.queryByText('Attachments')).toBeNull();
    expect(screen.queryByText('Beeper schedule')).toBeNull();
  });

  it('shows the plain-HTTP warning on Server only', async () => {
    const { client } = fixture();
    const view = render(SettingsWorkspace, { client, category: 'advanced', plainHTTPWarning: true });
    await screen.findByRole('heading', { name: 'Advanced', level: 2 });
    expect(screen.queryByText(/plain HTTP/)).toBeNull();
    await view.rerender({ client, category: 'server', plainHTTPWarning: true });
    expect(await screen.findByText(/plain HTTP/)).toBeDefined();
  });

  it('keeps tuning behind Advanced and host values behind Set in config.toml', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'server' });
    await screen.findByRole('heading', { name: 'Server' });
    expect(screen.getByLabelText('Stop when idle after')).toBeDefined();
    const advanced = screen.getByText('Advanced', { selector: 'summary' }).closest('details')!;
    expect(within(advanced).getByRole('switch', { name: 'Trace every SQL statement' })).toBeDefined();
    const config = screen.getByText('Set in config.toml').closest('details')!;
    expect(within(config).getByText('Bind address')).toBeDefined();
    expect(within(config).getByText('127.0.0.1')).toBeDefined();
    expect(screen.queryByText('Host-managed')).toBeNull();
    expect(screen.queryByText(/Saved changes apply/)).toBeNull();
  });

  it('keeps archive tuning and source rate limits reachable', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'advanced' });
    await screen.findByRole('heading', { name: 'Advanced', level: 2 });
    const advanced = screen.getByText('Advanced', { selector: 'summary' }).closest('details')!;

    expect(within(advanced).getByLabelText('Builder threads')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Sources' }));
    expect(screen.getByLabelText('Requests per second')).toBeDefined();
    expect(screen.getByRole('switch', { name: 'Download Discord attachments' })).toBeDefined();
  });

  it('saves every change through one Save changes with If-Match and then asks for a restart', async () => {
    const { client, seen } = fixture();
    render(SettingsWorkspace, { client, category: 'server' });
    await fireEvent.click(await screen.findByRole('switch', { name: 'Trace every SQL statement' }));
    expect(screen.getByText('1 unsaved change')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(seen.some((entry) => entry.method === 'PATCH')).toBe(true));
    const patch = seen.find((entry) => entry.method === 'PATCH')!;
    expect(patch.ifMatch).toBe('"s1"');
    expect(await screen.findByText('Saved. Restart msgvault to apply these changes.')).toBeDefined();
  });

  it('drops a change that is put back to its saved value, and Discard clears the rest', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'server' });
    const toggle = await screen.findByRole('switch', { name: 'Trace every SQL statement' });
    await fireEvent.click(toggle);
    await fireEvent.click(toggle);
    expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();
    await fireEvent.click(toggle);
    await fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
    expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();
  });

  it('never treats an emptied number as a value and blocks saving it', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'sources' });
    await screen.findByRole('heading', { name: 'Sources', level: 2 });
    const field = screen.getByLabelText('Requests per second') as HTMLInputElement;
    await fireEvent.input(field, { target: { value: '' } });
    expect(screen.getByText(/Enter a number to save/)).toBeDefined();
    expect((screen.getByRole('button', { name: 'Save changes' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('holds a typed secret until Save changes, then sends it with the settings', async () => {
    const { client, seen } = fixture();
    render(SettingsWorkspace, { client, category: 'tasks' });
    await fireEvent.click(await screen.findByRole('button', { name: 'Add API key' }));
    await fireEvent.input(screen.getByLabelText('New API key'), { target: { value: 'synthetic-key' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save API key' }));
    expect(seen.some((entry) => entry.method === 'PATCH')).toBe(false);
    expect(screen.getByText('1 unsaved change')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(seen.some((entry) => entry.method === 'PATCH')).toBe(true));
    expect(seen.find((entry) => entry.method === 'PATCH')!.body).toEqual({
      updates: [{ key: 'integrations.kata.api_key', secret: { action: 'set', value: 'synthetic-key' } }]
    });
    expect(screen.queryByText('Task integration')).toBeNull();
  });

  it('saves the endpoint first and then the provider key it binds to', async () => {
    const { client, seen } = fixture();
    render(SettingsWorkspace, { client, category: 'search' });
    const endpoint = await screen.findByLabelText('Endpoint');
    await fireEvent.input(endpoint, { target: { value: 'https://embed.example.test/v1' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add API key' }));
    await fireEvent.input(screen.getByLabelText('New API key'), { target: { value: 'provider-secret' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save API key' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(seen.some((entry) => entry.method === 'PUT')).toBe(true));
    const writes = seen.filter((entry) => entry.method !== 'GET').map((entry) => `${entry.method} ${entry.path}`);
    expect(writes).toEqual([
      'PATCH /api/v1/settings',
      'PUT /api/v1/settings/provider-credentials/vector.embeddings'
    ]);
    expect(seen.find((entry) => entry.method === 'PUT')!.ifMatch).toBe('"credential-a"');
  });

  it('lands a Set up link on the first field the switch still needs', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, {
      client, category: 'search',
      navigationTarget: { authority: 'semantic_search', categoryID: 'search', settingKey: 'vector.enabled' }
    });
    await screen.findByRole('heading', { name: 'Search & AI' });
    await waitFor(() => expect((window.document.activeElement as HTMLElement | null)?.dataset['settingKey'])
      .toBe('vector.embeddings.endpoint'));
  });

  it('reports a saved Appearance value, and hides search-mode choices until vector search is ready', async () => {
    const onAppearanceSaved = vi.fn();
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'advanced', onAppearanceSaved });
    await screen.findByRole('heading', { name: 'Advanced', level: 2 });
    expect(screen.getByRole('combobox', { name: /^Search opens in/ })).toBeDefined();
    await chooseSelectOption(screen.getByLabelText('Theme'), 'Dark');
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(onAppearanceSaved).toHaveBeenCalledWith({ theme: 'dark' }));
    expect(screen.queryByText(/Use daemon theme/)).toBeNull();
  });



  it('reloads the latest settings after a conflict and keeps what still differs', async () => {
    let conflicts = 1;
    const { client } = fixture(settingsDoc(), (request) => {
      if (request.method === 'PATCH' && conflicts-- > 0) return Response.json({ error: 'conflict' }, { status: 412 });
      return undefined;
    });
    render(SettingsWorkspace, { client, category: 'server' });
    await fireEvent.click(await screen.findByRole('switch', { name: 'Trace every SQL statement' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    expect((await screen.findByRole('alert')).textContent).toContain('The settings changed on disk');
    expect(screen.getByText('1 unsaved change')).toBeDefined();
  });

  it('shows Contacts with its heading first and without sync status, Sync now, or Full sync', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'carddav' });
    expect(await screen.findByRole('heading', { name: 'Contacts' })).toBeDefined();
    expect(await screen.findByRole('button', { name: 'Add connection' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Sync now' })).toBeDefined();
    expect(screen.queryByText(/These save immediately/)).toBeNull();
  });


  it('offers providers and facts under People lookups, with the add form behind a click', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'enrichment' });
    expect(await screen.findByRole('heading', { name: 'People lookups' })).toBeDefined();
    expect(await screen.findByRole('button', { name: 'Add Exa' })).toBeDefined();
    expect(await screen.findByLabelText('Profile name')).toBeDefined();
  });

  it('offers to build the message index when search is on and no index is active', async () => {
    const doc = settingsDoc();
    (doc.settings as Setting[]).find((candidate) => candidate.key === 'vector.enabled')!.value = typed(true);
    const { client, seen } = fixture(doc, (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/embeddings/status') return Response.json({ active_generation: null });
      if (path === '/api/v1/cli/run') return new Response('{"type":"complete"}\n');
      return undefined;
    });
    render(SettingsWorkspace, { client, category: 'search' });
    expect(await screen.findByText('Index not built')).toBeDefined();

    await fireEvent.click(await screen.findByRole('button', { name: 'Build index' }));
    await waitFor(() => expect(seen.find((entry) => entry.path === '/api/v1/cli/run')?.body).toEqual({
      args: ['embeddings', 'build', '--full-rebuild', '--yes']
    }));
  });

  it('shows the visual index step the server advertises, with its consent line', async () => {
    const doc = settingsDoc();
    (doc.settings as Setting[]).push(setting('vector.multimodal.enabled', 'search', 'boolean', true, { label: 'Visual search' }));
    const lane = (actions: string[]) => ({ lanes: [{ kind: 'visual_embedding', lane: 'visual_attachments', configured: true, supported_actions: actions }] });
    let actions = ['visual_build'];
    const { client, seen } = fixture(doc, (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/operations/status') return Response.json(lane(actions));
      if (path === '/api/v1/multimodal/build') {
        actions = ['visual_resume'];
        return Response.json({});
      }
      return undefined;
    });
    render(SettingsWorkspace, { client, category: 'search' });
    expect(await screen.findByText('Sends attachments to your search provider.')).toBeDefined();

    await fireEvent.click(await screen.findByRole('button', { name: 'Build visual index' }));
    await waitFor(() => expect(seen.find((entry) => entry.path === '/api/v1/multimodal/build')?.body).toEqual({ consent: true }));
    expect(await screen.findByRole('button', { name: 'Resume visual index' })).toBeDefined();
    expect(screen.queryByText('Sends attachments to your search provider.')).toBeNull();
  });
});


it('keeps provider endpoints and keys across categories, saves endpoints before keys, and retains only a failed key', async () => {
  let failKey = true;
  const provider = {
    name: 'primary', kind: 'exa', enabled: false, endpoint: 'https://old.example.test',
    credential_id: 'people.enrichment.primary', credential: { configured: false, source: 'none' },
    allowed_identifiers: [], target_keys: [], allow_sensitive_targets: false,
    retention_posture: '', training_posture: '', refresh_interval: '24h', request_timeout: '30s',
    poll_interval: '1s', max_job_age: '1h', max_retries: 2, max_requests_per_run: 10, max_requests_per_day: 100
  };
  const { client, seen } = fixture(settingsDoc({ person_enrichment_providers: [provider] }), request => {
    if (request.url.endsWith('/provider-credentials/people.enrichment.primary') && failKey)
      return Response.json({ message: 'Key store unavailable' }, { status: 503 });
  });
  render(SettingsWorkspace, { client, category: 'enrichment' });
  await fireEvent.input(await screen.findByLabelText('primary endpoint'), { target: { value: 'https://new.example.test' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Add API key for primary' }));
  await fireEvent.input(screen.getByLabelText('New API key for primary'), { target: { value: 'synthetic-provider-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save API key for primary' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  await fireEvent.click(screen.getByRole('switch', { name: 'Trace every SQL statement' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  expect((await screen.findByRole('alert')).textContent).toBe('Key store unavailable');
  expect(screen.getByText('1 unsaved change')).toBeDefined();
  const writes = seen.filter(request => request.method !== 'GET');
  expect(writes.map(({ method, path, ifMatch }) => [method, path, ifMatch])).toEqual([
    ['PATCH', '/api/v1/settings', '"s1"'],
    ['PUT', '/api/v1/settings/person-enrichment/providers/primary', '"s2"'],
    ['PUT', '/api/v1/settings/provider-credentials/people.enrichment.primary', '"credential-a"']
  ]);
  expect(writes[1].body).toMatchObject({ endpoint: 'https://new.example.test' });
  failKey = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull());
  expect(seen.filter(request => request.method === 'PATCH')).toHaveLength(1);
  expect(seen.filter(request => request.path.endsWith('/providers/primary'))).toHaveLength(1);
});

it('saves account drafts from Overview and drops their passwords on Discard', async () => {
  const { client, seen } = fixture(settingsDoc(), request => {
    if (new URL(request.url).pathname === '/api/v1/carddav/account')
      return Response.json({ base_url: 'https://dav.example.test/', username: 'account@example.com', enabled: false, schedule: '', books: 1 });
  });
  render(SettingsWorkspace, { client, category: 'carddav' });
  await fireEvent.input(await screen.findByLabelText('Base URL'), { target: { value: 'https://dav.example.test/' } });
  await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'account@example.com' } });
  await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-account-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('synthetic-account-key');
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull());
  expect(seen.filter(request => request.method === 'PUT').map(request => request.body)).toEqual([
    { connection: 'default', base_url: 'https://dav.example.test/', username: 'account@example.com', password: 'synthetic-account-key', enabled: false, schedule: '' }
  ]);
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'discarded-account-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
  expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('');
});

it('retains inference replacement keys across navigation and saves them from the footer', async () => {
  const inference = { profiles: [{ name: 'primary', protocol: 'openai', model: 'example-model', credential_source: 'stored', credential_configured: true, credential_revision: '"key-a"' }], configured_enabled: false, running_enabled: false, pending_restart: false };
  const { client, seen } = fixture(settingsDoc(), request => {
    if (request.url.includes('/settings/people-inference'))
      return Response.json(inference, { headers: { ETag: '"p1"' } });
  });
  render(SettingsWorkspace, { client, category: 'enrichment' });
  await fireEvent.input(await screen.findByLabelText('Replacement API key'), { target: { value: 'synthetic-inference-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  await fireEvent.click(screen.getByRole('button', { name: 'People lookups' }));
  expect((await screen.findByLabelText('Replacement API key') as HTMLInputElement).value).toBe('synthetic-inference-key');
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull());
  expect(seen.find(request => request.method === 'PUT')).toMatchObject({ path: '/api/v1/settings/people-inference/providers/primary/key', ifMatch: '"key-a"', body: { value: 'synthetic-inference-key' } });
});

it('keeps a host-owned primary field read-only', async () => {
  const doc = settingsDoc();
  doc.settings.push(setting('server.daemon_auto_start', 'server', 'boolean', true, { read_only: true, label: 'Automatic start' }));
  const { client } = fixture(doc);
  render(SettingsWorkspace, { client, category: 'server' });
  await screen.findByRole('heading', { name: 'Server' });
  expect(screen.queryByRole('switch', { name: 'Start automatically' })).toBeNull();
  expect(screen.getByText('Start automatically')).toBeDefined();
});

it('keeps attachment off values and cron schedules editable under Sources', async () => {
  const doc = settingsDoc();
  doc.settings.push(setting('discord.max_media_mb', 'attachments', 'integer', 0, {
    label: 'Maximum attachment size', validation: { minimum: 0, off: { value: '0', label: 'Provider default', suggest: '50', on_minimum: 1 } }
  }));
  doc.settings.push(setting('slack.schedule', 'sources', 'string', '0 3 * * *', { label: 'Slack schedule', validation: { format: 'cron' } }));
  const { client, seen } = fixture(doc);
  render(SettingsWorkspace, { client, category: 'sources' });
  await fireEvent.click(await screen.findByRole('switch', { name: 'Set Maximum attachment size' }));
  expect((screen.getByLabelText('Maximum attachment size') as HTMLInputElement).value).toBe('50');
  await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Every day at 03:00' }), 'Every 15 minutes');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(seen.find(request => request.method === 'PATCH')?.body).toEqual({ updates: [
    { key: 'discord.max_media_mb', value: { integer: 50 } },
    { key: 'slack.schedule', value: { string: '*/15 * * * *' } }
  ] }));
});
