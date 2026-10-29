import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import SettingsWorkspace from './SettingsWorkspace.svelte';
import { createAPIClient } from '../../api/client';
import { lane, lanes } from '../../../test/operations';
import { chooseSelectOption } from '../../../test/kit-ui';

type Setting = Record<string, unknown>;

const providerDefaults = {
  allowed_identifiers: [], target_keys: [], allow_sensitive_targets: false,
  retention_posture: '', training_posture: '', refresh_interval: '24h', request_timeout: '30s',
  poll_interval: '1s', max_job_age: '1h', max_retries: 2, max_requests_per_run: 10, max_requests_per_day: 100,
  num_results: 1, mode: 'people'
};
const inferenceProfile = {
  name: 'primary', preset_id: 'openrouter', protocol: 'openai-chat', model: 'model-one', endpoint: 'https://provider.example.test/v1',
  credential_source: 'stored', credential_configured: true, credential_revision: '"key-a"', checked: false,
  consent_active: false, fingerprint: 'fingerprint-primary', selected: false, output_mode: 'strict_schema',
  allowed_sources: ['conversation_text'], source_since: '2025-01-01', allow_sensitive: true,
  retention_posture: 'No retention', training_posture: 'No training'
};

function typed(value: unknown): Record<string, unknown> {
  if (typeof value === 'boolean') return { boolean: value };
  if (typeof value === 'number') return Number.isInteger(value) ? { integer: value } : { number: value };
  if (Array.isArray(value)) return { strings: value };
  return { string: value };
}

function setting(key: string, group: string, kind: string, value: unknown, extra: Setting = {}): Setting {
  return {
    key, group, kind, label: key, restart_required: true,
    value: value === undefined ? undefined : typed(value), ...extra
  };
}

function settingsDoc(overrides: Setting = {}) {
  return {
    groups: [{ id: 'browser', label: 'Browser' }, { id: 'server', label: 'Server' }, { id: 'archive', label: 'Archive' }, { id: 'integrations', label: 'Integrations' }, { id: 'enrichment', label: 'People lookups' }, { id: 'sources', label: 'Sources' }, { id: 'attachments', label: 'Attachments' }, { id: 'search', label: 'Search', sections: [
      { id: 'people', label: 'Person embeddings', description: 'Sends consented person fields to the text embedding provider.' },
      { id: 'visual', label: 'Visual attachment search', description: 'Sends images to a hosted provider. Semantic search must also be on.' }
    ] }],
    settings: [
      setting('web.theme', 'browser', 'string', 'system', {
        label: 'Theme', restart_required: false, options: ['system', 'light', 'dark'] }),
      setting('web.density', 'browser', 'string', 'compact', {
        label: 'Density', restart_required: false, options: ['compact', 'comfortable'] }),
      setting('web.default_search_mode', 'browser', 'string', 'full_text', {
        label: 'Default search mode', restart_required: false, options: ['full_text', 'semantic', 'hybrid'] }),
      setting('server.api_port', 'server', 'integer', 0, { label: 'API port', read_only: true, validation: { off: { value: '0', label: 'The daemon picks a free port' } } }),
      setting('server.bind_addr', 'server', 'string', '127.0.0.1', { label: 'Bind address', read_only: true }),
      setting('server.api_key', 'server', 'secret', undefined, {
        label: 'API key', read_only: true, secret: { configured: true, source: 'environment', hint: 'abc…xyz' } }),
      setting('server.daemon_idle_timeout', 'server', 'string', '20m0s', { label: 'Idle timeout' }),
      setting('log.sql_trace', 'server', 'boolean', false, { label: 'Trace every SQL statement' }),
      setting('activity.timezone', 'archive', 'string', 'UTC', { label: 'Timezone' }),
      setting('analytics.builder_threads', 'archive', 'integer', 4, { label: 'Builder threads' }),
      setting('sync.rate_limit_qps', 'sources', 'integer', 5, { label: 'Requests per second' }),
      setting('vector.enabled', 'search', 'boolean', false, { label: 'Semantic search' }),
      setting('vector.embeddings.endpoint', 'search', 'string', '', { label: 'Text embedding endpoint' }),
      setting('vector.embeddings.model', 'search', 'string', '', { label: 'Text embedding model' }),
      setting('vector.embeddings.api_key', 'search', 'secret', undefined, {
        label: 'Text embedding API key', credential_id: 'vector.embeddings', restart_required: true,
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

interface Seen { method: string; path: string; ifMatch: string | null; query?: string; body?: unknown }

function fixture(doc: ReturnType<typeof settingsDoc> = settingsDoc(), extra?: (request: Request) => Response | undefined | Promise<Response | undefined>) {
  const seen: Seen[] = [];
  let etag = 1;
  const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input as Request;
    const path = new URL(request.url).pathname;
    const body = request.method === 'GET' || request.method === 'DELETE' ? undefined : await request.clone().json();
    seen.push({ method: request.method, path, query: new URL(request.url).search, ifMatch: request.headers.get('If-Match'), body });
    const custom = await extra?.(request);
    if (custom) return custom;
    if (path === '/api/v1/settings' && request.method === 'GET') {
      return Response.json(doc, { headers: { ETag: `"s${etag}"`, 'Credential-ETag': '"credential-a"' } });
    }
    if (path === '/api/v1/settings' && request.method === 'PATCH') {
      for (const update of (body as { updates: { key: string; value?: Record<string, unknown> }[] }).updates) {
        const target = (doc.settings as Setting[]).find((candidate) => candidate.key === update.key);
        if (target) {
          if (update.value) target.value = update.value;
          if (target.restart_required) doc.pending_restart = true;
        }
      }
      etag += 1;
      return Response.json(doc, { headers: { ETag: `"s${etag}"` } });
    }
    if (path.startsWith('/api/v1/settings/person-enrichment/providers/') && request.method === 'PUT') {
      doc.person_enrichment_providers = (doc.person_enrichment_providers as Record<string, unknown>[]).map(provider => path.endsWith('/' + provider.name) ? { ...provider, ...body as object } : provider) as never[];
      etag += 1;
      return Response.json(doc, { headers: { ETag: `"s${etag}"` } });
    }
    if (path.startsWith('/api/v1/settings/provider-credentials/')) {
      const credentialID = decodeURIComponent(path.split('/').at(-1)!);
      return Response.json(
        { credential_id: credentialID, state: { configured: true, source: 'stored', hint: '•••' }, pending_restart: credentialID.startsWith('vector.') },
        { headers: { ETag: '"credential-b"' } }
      );
    }
    if (path === '/api/v1/operations/status') return Response.json({ lanes: lanes() });
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

it('shows the restart cue before saving only while a restart-required draft is pending', async () => {
  const doc = settingsDoc();
  doc.settings.push(setting('log.enabled', 'server', 'boolean', false, { restart_required: true }));
  const { client, seen } = fixture(doc);
  render(SettingsWorkspace, { client, category: 'advanced' });
  await chooseSelectOption(await screen.findByLabelText('Theme'), 'Dark');
  const footer = screen.getByText('1 unsaved change').parentElement!;
  expect(within(footer).queryByText('Restart msgvault to apply these changes.')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  await fireEvent.click(await screen.findByRole('switch', { name: 'Keep logs' }));
  expect(within(footer).getByText('Restart msgvault to apply these changes.')).toBeDefined();
  expect(within(footer).getByText('2 unsaved changes.')).toBeDefined();
  expect(seen.some(request => request.method === 'PATCH')).toBe(false);
  await fireEvent.click(screen.getByRole('switch', { name: 'Keep logs' }));
  expect(within(footer).queryByText('Restart msgvault to apply these changes.')).toBeNull();
  expect(within(footer).getByText('1 unsaved change')).toBeDefined();
});

it('warns before leaving with drafts, keeps them on Stay, and discards before continuing', async () => {
  const { client } = fixture();
  let hold: ((leave: () => void) => boolean) | undefined;
  const { unmount } = render(SettingsWorkspace, {
    props: { client, category: 'server' },
    context: new Map([['msgvault:hold-settings-leave', (next: typeof hold) => { hold = next; }]])
  });
  const field = await screen.findByLabelText('Stop when idle after');
  await fireEvent.input(field, { target: { value: '15m' } });
  const firstLeave = vi.fn();
  expect(hold?.(firstLeave)).toBe(true);
  const stay = await screen.findByRole('button', { name: 'Stay' });
  expect(screen.getByText('Leave Settings and discard 1 unsaved change?')).toBeDefined();
  expect(document.activeElement).toBe(stay);
  await fireEvent.click(stay);
  expect(field).toHaveProperty('value', '15m');
  expect(firstLeave).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  expect(hold?.(firstLeave)).toBe(true);
  const latestLeave = vi.fn(() => {
    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  });
  expect(hold?.(latestLeave)).toBe(true);
  await fireEvent.click(await screen.findByRole('button', { name: 'Discard and leave' }));
  expect(firstLeave).not.toHaveBeenCalled();
  expect(latestLeave).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  expect(screen.getByLabelText('Stop when idle after')).toHaveProperty('value', '20m0s');
  expect(hold?.(firstLeave)).toBe(false);
  unmount();
  expect(hold).toBeUndefined();
});

it('cancels a pending leave when the category changes, keeping drafts', async () => {
  const { client } = fixture();
  let hold: ((leave: () => void, stay?: () => void) => boolean) | undefined;
  let cancel: (() => void) | undefined;
  const { rerender } = render(SettingsWorkspace, {
    props: { client, category: 'server' },
    context: new Map([['msgvault:hold-settings-leave', (next: typeof hold, nextCancel: typeof cancel) => { hold = next; cancel = nextCancel; }]])
  });
  const field = await screen.findByLabelText('Stop when idle after');
  await fireEvent.input(field, { target: { value: '15m' } });
  const leave = vi.fn();
  expect(hold?.(leave)).toBe(true);
  await screen.findByRole('button', { name: 'Discard and leave' });
  cancel?.();
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Discard and leave' })).toBeNull());
  expect(leave).not.toHaveBeenCalled();
  expect(field).toHaveProperty('value', '15m');
  const stay = vi.fn();
  expect(hold?.(leave, stay)).toBe(true);
  await screen.findByRole('button', { name: 'Discard and leave' });
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  expect(screen.queryByRole('button', { name: 'Discard and leave' })).toBeNull();
  expect(stay).toHaveBeenCalledOnce();
  expect(leave).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  expect(screen.getByLabelText('Stop when idle after')).toHaveProperty('value', '15m');
  expect(hold?.(leave)).toBe(true);
  await screen.findByRole('button', { name: 'Discard and leave' });
  await rerender({ category: 'carddav' });
  expect(screen.queryByRole('button', { name: 'Discard and leave' })).toBeNull();
  expect(leave).not.toHaveBeenCalled();
});

it('blocks reload only while Settings has unsaved drafts', async () => {
  const { client } = fixture();
  render(SettingsWorkspace, { client, category: 'server' });
  const field = await screen.findByLabelText('Stop when idle after');
  const clean = new Event('beforeunload', { cancelable: true });
  window.dispatchEvent(clean);
  expect(clean.defaultPrevented).toBe(false);
  await fireEvent.input(field, { target: { value: '15m' } });
  const dirty = new Event('beforeunload', { cancelable: true });
  window.dispatchEvent(dirty);
  expect(dirty.defaultPrevented).toBe(true);
  await fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
  const discarded = new Event('beforeunload', { cancelable: true });
  window.dispatchEvent(discarded);
  expect(discarded.defaultPrevented).toBe(false);
});

it('keeps a failed Overview action error on Overview', async () => {
  const { client } = fixture(settingsDoc(), request => {
    if (request.url.endsWith('/operations/status')) return Response.json({ lanes: lanes(lane('carddav_sync', { supported_actions: ['carddav_sync'] })) });
    if (request.url.endsWith('/carddav/sync')) throw new Error('Synthetic sync failure');
  });
  render(SettingsWorkspace, { client });
  await fireEvent.click(await screen.findByRole('button', { name: 'Sync now, Contacts' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Synthetic sync failure');
  await fireEvent.click(screen.getByRole('button', { name: 'Server' }));
  expect(screen.queryByRole('alert')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Overview' }));
  expect((await screen.findByRole('alert')).textContent).toContain('Synthetic sync failure');
});

describe('SettingsWorkspace', () => {
  it('lists the categories in order, Overview first, and opens the one it is given', async () => {
    const { client } = fixture();
    render(SettingsWorkspace, { client, category: 'advanced' });
    await screen.findByRole('heading', { name: 'Advanced', level: 2 });
    const names = [...window.document.querySelectorAll('.kit-settings__nav-item')].map((item) => item.textContent?.trim());
    expect(names).toEqual([
      'Overview', 'Server', 'Search & AI', 'People lookups', 'Tasks', 'Contacts', 'Sources', 'Advanced'
    ]);
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
    expect(within(config).getByText('The daemon picks a free port')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Advanced' }));
    const archiveTuning = screen.getByText('Advanced', { selector: 'summary' }).closest('details')!;
    expect(within(archiveTuning).getByLabelText('Builder threads')).toBeDefined();
  });

  it('reverts and discards edits, then keeps a conflicted draft for a revision-checked save', async () => {
    let conflicts = 1;
    const doc = settingsDoc();
    const { client, seen } = fixture(doc, request => {
      if (request.method === 'GET' && request.url.endsWith('/settings')) return Response.json(doc, { headers: { ETag: conflicts ? '"s1"' : '"s-reloaded"' } });
      if (request.method === 'PATCH' && conflicts-- > 0) return Response.json({ error: 'conflict' }, { status: 412 });
    });
    render(SettingsWorkspace, { client, category: 'server' });
    await fireEvent.click(await screen.findByRole('switch', { name: 'Trace every SQL statement' }));
    await fireEvent.click(screen.getByRole('switch', { name: 'Trace every SQL statement' }));
    expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();
    await fireEvent.click(screen.getByRole('switch', { name: 'Trace every SQL statement' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
    expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();
    await fireEvent.click(screen.getByRole('switch', { name: 'Trace every SQL statement' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    expect((await screen.findByRole('alert')).textContent).toContain('The settings changed on disk');
    expect(seen.filter(request => request.method === 'GET' && request.path === '/api/v1/settings')).toHaveLength(2);
    expect(screen.getByText('1 unsaved change.')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(seen.filter(entry => entry.method === 'PATCH')).toHaveLength(2));
    const patch = seen.filter(entry => entry.method === 'PATCH').at(-1)!;
    expect(patch.ifMatch).toBe('"s-reloaded"');
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
    expect(screen.getByText('1 unsaved change.')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(seen.some((entry) => entry.method === 'PATCH')).toBe(true));
    expect(seen.find((entry) => entry.method === 'PATCH')!.body).toEqual({
      updates: [{ key: 'integrations.kata.api_key', secret: { action: 'set', value: 'synthetic-key' } }]
    });
  });

  it('focuses an incoming Set up link without committing another navigation', async () => {
    const { client } = fixture();
    const onCategoryChange = vi.fn();
    render(SettingsWorkspace, {
      client, category: 'search', onCategoryChange,
      navigationTarget: { authority: 'semantic_search', categoryID: 'search', settingKey: 'vector.enabled' }
    });
    await screen.findByRole('heading', { name: 'Search & AI' });
    await waitFor(() => expect((window.document.activeElement as HTMLElement | null)?.dataset['settingKey'])
      .toBe('vector.enabled'));
    expect(onCategoryChange).not.toHaveBeenCalled();
  });

});

it('keeps attachment off values and cron schedules editable under Sources', async () => {
  const doc = settingsDoc();
  doc.settings.push(setting('discord.max_media_mb', 'attachments', 'integer', 0, {
    label: 'Maximum attachment size', validation: { minimum: 0, off: { value: '0', label: 'Provider default', suggest: '50', on_minimum: 1 } }
  }));
  doc.settings.push(setting('slack.schedule', 'sources', 'string', '0 3 * * *', { label: 'Slack schedule', validation: { format: 'cron' } }));
  const { client, seen } = fixture(doc);
  render(SettingsWorkspace, { client, category: 'sources' });
  await fireEvent.click(await screen.findByRole('switch', { name: 'Set Discord maximum size (MiB)' }));
  expect((screen.getByLabelText('Discord maximum size (MiB)') as HTMLInputElement).value).toBe('50');
  await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Every day at 03:00' }), 'Every 15 minutes');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(seen.find(request => request.method === 'PATCH')?.body).toEqual({ updates: [
    { key: 'discord.max_media_mb', value: { integer: 50 } },
    { key: 'slack.schedule', value: { string: '*/15 * * * *' } }
  ] }));
});

it.each([
  ['bad/name', 'Names can use letters'],
  ['primary', 'already exists'],
  ['exa-primary', null]
] as const)('validates Exa provider %s and retries conflicts with its defaults', async (name, error) => {
  const doc = settingsDoc({ person_enrichment_providers: [{ name: 'primary', kind: 'exa', enabled: false, endpoint: 'https://api.exa.ai/search', credential: { configured: false, source: 'none' }, ...providerDefaults }] });
  let conflict = false;
  const { client, seen } = fixture(doc, async request => {
    if (request.method === 'GET' && request.url.endsWith('/settings')) return Response.json(doc, { headers: { ETag: conflict ? '"s2"' : '"s1"' } });
    if (request.method === 'PUT' && request.url.endsWith('/person-enrichment/providers/exa-primary')) {
      if (!conflict) { conflict = true; return Response.json({ error: 'settings_conflict' }, { status: 412 }); }
      return Response.json({ ...doc, person_enrichment_providers: [{ name: 'exa-primary', ...await request.json(), credential: { configured: false, source: 'none' }, credential_id: 'people.enrichment/exa-primary' }] }, { headers: { ETag: '"s3"' } });
    }
  });
  render(SettingsWorkspace, { client, category: 'enrichment' });
  await fireEvent.click(await screen.findByRole('button', { name: 'Add Exa' }));
  await fireEvent.input(screen.getByLabelText('Exa name'), { target: { value: name } });
  await fireEvent.submit(screen.getByRole('form', { name: 'Add Exa' }));
  if (error) {
    expect((await screen.findByRole('alert')).textContent).toContain(error);
    expect(seen.filter(request => request.method !== 'GET')).toEqual([]);
    return;
  }
  await screen.findByText('The settings changed on disk. The latest are loaded. Try again.');
  await fireEvent.submit(screen.getByRole('form', { name: 'Add Exa' }));
  await screen.findByRole('article', { name: 'exa-primary' });
  const writes = seen.filter(request => request.method === 'PUT');
  expect(writes.map(request => request.ifMatch)).toEqual(['"s1"', '"s2"']);
  expect(writes[1].body).toMatchObject({ kind: 'exa', enabled: false, endpoint: 'https://api.exa.ai/search', mode: 'people', num_results: 1, allowed_identifiers: ['public_profile_url'], target_keys: ['attribute:location'], allow_sensitive_targets: false, retention_posture: '', training_posture: '', refresh_interval: '720h', request_timeout: '1m', max_retries: 5, max_requests_per_run: 10, max_requests_per_day: 100 });
  expect(writes[1].body).not.toHaveProperty('api_key_env');
  expect(writes[1].body).not.toHaveProperty('max_cost_usd_micros_per_day');
});

it('locks fields and newly mounted category controls without holding leave while a save waits', async () => {
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  const doc = settingsDoc();
  const { client, seen } = fixture(doc, request => request.method === 'PATCH' ? pending : undefined);
  let hold: ((leave: () => void) => boolean) | undefined;
  render(SettingsWorkspace, {
    props: { client, category: 'server' },
    context: new Map([['msgvault:hold-settings-leave', (next: typeof hold) => { hold = next; }]])
  });
  await fireEvent.input(await screen.findByLabelText('Stop when idle after'), { target: { value: '15m' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  try {
    await waitFor(() => expect(seen.some(request => request.method === 'PATCH')).toBe(true));
    expect(hold?.(vi.fn())).toBe(false);
    expect(screen.queryByText('Leave Settings and discard 1 unsaved change?')).toBeNull();
    expect(screen.getByLabelText('Stop when idle after').matches(':disabled')).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Advanced' }));
    expect(screen.getByLabelText('Builder threads').matches(':disabled')).toBe(true);
  } finally {
    finish(Response.json(doc, { headers: { ETag: '"s2"' } }));
  }
});

it('keeps credential and provider saves immediate while workspace drafts remain pending', async () => {
  const provider = { ...providerDefaults, name: 'primary', kind: 'exa', enabled: false, endpoint: 'https://provider.example.test', credential_id: 'people.enrichment/primary', credential: { configured: false, source: 'none' } };
  const { client, seen } = fixture(settingsDoc({ person_enrichment_providers: [provider] }));
  const writes = () => seen.filter(request => request.method !== 'GET').map(({ method, path, ifMatch }) => [method, path, ifMatch]);
  render(SettingsWorkspace, { client, category: 'search' });
  await fireEvent.input(await screen.findByLabelText('Endpoint'), { target: { value: 'https://new.example.test' } });
  expect((screen.getByRole('button', { name: 'Add API key' }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByText('Save endpoint settings first before changing this credential.')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Add API key' }));
  expect(screen.getByRole('dialog').textContent).toContain('The daemon uses it after a restart.');
  await fireEvent.input(screen.getByLabelText('New API key'), { target: { value: 'synthetic-key' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save API key' }));
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();

  await fireEvent.click(screen.getByRole('button', { name: 'People lookups' }));
  await fireEvent.input(await screen.findByLabelText('primary endpoint'), { target: { value: 'https://changed.example.test' } });
  expect(screen.getByText('Save provider settings first before changing this credential.')).toBeDefined();
  await fireEvent.click(screen.getByRole('switch', { name: 'Look people up' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Save primary provider' }));
  await waitFor(() => expect(seen.some(request => request.path.endsWith('/providers/primary'))).toBe(true));
  expect(screen.getByText('1 unsaved change.')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Contacts' }));
  expect(screen.getByText('1 unsaved change.')).toBeDefined();
  expect((screen.getByRole('button', { name: 'Save changes' }) as HTMLButtonElement).disabled).toBe(false);
  expect(writes()).toEqual([
    ['PUT', '/api/v1/settings/provider-credentials/vector.embeddings', '"credential-a"'],
    ['PUT', '/api/v1/settings/person-enrichment/providers/primary', '"s1"']
  ]);
  expect(seen.find(request => request.path.endsWith('/providers/primary'))?.body).toMatchObject({ endpoint: 'https://changed.example.test' });
});

it('starts the visual index from Overview and refreshes its row', async () => {
  let built = false;
  const { client } = fixture(settingsDoc(), request => {
    if (request.url.endsWith('/operations/status')) return Response.json({ lanes: lanes(lane('visual_embedding', { supported_actions: built ? [] : ['visual_build'] })) });
    if (request.url.endsWith('/multimodal/build')) {
      built = true;
      return Response.json({});
    }
  });
  render(SettingsWorkspace, { client });
  const name = 'Build visual index, Visual search';
  await fireEvent.click(await screen.findByRole('button', { name }));
  await waitFor(() => expect(screen.queryByRole('button', { name })).toBeNull());
});

it('keeps feature rows during refresh and recovers from a later status failure', async () => {
  let finish!: (response: Response) => void;
  let reads = 0;
  let coverageReads = 0;
  let finishCoverage!: (response: Response) => void;
  const pendingCoverage = new Promise<Response>(resolve => { finishCoverage = resolve; });
  const coverage = { eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1', status: 'unavailable', actions: ['build_index'] };
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  const { client } = fixture(settingsDoc(), request => {
    if (request.url.includes('/search/coverage')) return ++coverageReads === 2 ? pendingCoverage : Response.json(coverage);
    if (request.url.endsWith('/operations/status')) {
      reads += 1;
      if (reads === 3) return Response.json({ message: 'Unavailable' }, { status: 503 });
      if (reads > 3) return Response.json({ lanes: lanes(lane('visual_embedding')) });
      return reads === 1 ? Response.json({ lanes: lanes(lane('visual_embedding', { active: { id: 'run-1', kind: 'visual_embedding', lane: 'visual_attachments', state: 'running', started_at: '2026-01-01T00:00:00Z', counters: [] } })) }) : pending;
    }
  });
  render(SettingsWorkspace, { client });
  await screen.findByText('Running');
  await screen.findByRole('button', { name: 'Build index, Message search' });
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh feature status' }));
  try {
    await waitFor(() => {
      expect(reads).toBe(2);
      expect(coverageReads).toBe(2);
    });
    expect(screen.getByText('Running')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Build index, Message search' })).toBeDefined();
  } finally {
    finish(Response.json({ lanes: lanes(lane('visual_embedding')) }));
    finishCoverage(Response.json(coverage));
  }
  await screen.findByText('On');
  await fireEvent.click(screen.getByRole('button', { name: 'Refresh feature status' }));
  await screen.findByText("Can't load status");
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await screen.findByText('On');
});

it.each(['full_text', 'semantic'])('offers approved search defaults and preserves stored %s until a new choice', async stored => {
  const doc = settingsDoc();
  doc.settings.find(item => item.key === 'web.default_search_mode')!.value = typed(stored);
  const { client, seen } = fixture(doc);
  const onAppearanceSaved = vi.fn();
  render(SettingsWorkspace, { client, category: 'advanced', onAppearanceSaved });
  const trigger = await screen.findByRole('combobox', { name: /^Search opens in:/ });
  expect(trigger.textContent).toContain(stored === 'semantic' ? 'Semantic' : 'Exact words');
  expect(screen.queryByText('1 unsaved change')).toBeNull();
  await fireEvent.click(trigger);
  const semantic = screen.queryByRole('option', { name: 'Semantic' });
  if (stored === 'semantic') {
    expect((semantic as HTMLButtonElement).disabled).toBe(true);
    return;
  }
  expect(semantic).toBeNull();
  await fireEvent.click(screen.getByRole('option', { name: 'Best match' }));
  await chooseSelectOption(screen.getByLabelText('Theme'), 'Dark');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  expect(seen.find(request => request.method === 'PATCH')?.body).toEqual({ updates: [{ key: 'web.default_search_mode', value: { string: 'hybrid' } }, { key: 'web.theme', value: { string: 'dark' } }] });
  expect(onAppearanceSaved).toHaveBeenCalledWith({ defaultSearchMode: 'hybrid', theme: 'dark' });
});

it('keeps edited People search details visible when switching the feature off', async () => {
  const doc = settingsDoc();
  doc.settings.push(setting('vector.people.enabled', 'search', 'boolean', false), setting('vector.people.retention_posture', 'search', 'string', 'standard'));
  const { client, seen } = fixture(doc);
  render(SettingsWorkspace, { client, category: 'search' });
  const toggle = await screen.findByRole('switch', { name: 'Search people' });
  expect(screen.queryByLabelText('How long the provider keeps data')).toBeNull();
  await fireEvent.click(toggle);
  await fireEvent.input(screen.getByLabelText('How long the provider keeps data'), { target: { value: 'zero' } });
  await fireEvent.click(toggle);
  expect((screen.getByLabelText('How long the provider keeps data') as HTMLInputElement).value).toBe('zero');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(screen.queryByText(/^\d+ unsaved changes?/)).toBeNull());
  expect(seen.find(request => request.method === 'PATCH')?.body).toEqual({ updates: [{ key: 'vector.people.retention_posture', value: { string: 'zero' } }] });
  await waitFor(() => expect(screen.queryByLabelText('How long the provider keeps data')).toBeNull());
});

it('refreshes the shared config revision when creating a profile succeeds before its key fails', async () => {
  let revision = '"s1"';
  let created = false;
  const doc = settingsDoc();
  const { client, seen } = fixture(doc, request => {
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/settings' && request.method === 'GET') return Response.json(doc, { headers: { ETag: revision } });
    if (path.includes('/settings/people-inference')) {
      if (request.method === 'PUT' && path.endsWith('/key')) return Response.json({ message: 'Key store unavailable' }, { status: 503 });
      if (request.method === 'PUT') { created = true; revision = '"s2"'; }
      return Response.json({ profiles: created ? [inferenceProfile] : [], configured_enabled: false, running_enabled: false, pending_restart: created }, { headers: { ETag: revision } });
    }
    if (request.method === 'PATCH' && request.headers.get('If-Match') !== revision) return Response.json({ message: 'Config changed' }, { status: 412 });
  });
  render(SettingsWorkspace, { client, category: 'server' });
  await fireEvent.input(await screen.findByLabelText('Stop when idle after'), { target: { value: '15m' } });
  await fireEvent.click(screen.getByRole('button', { name: 'People lookups' }));
  for (const [label, value] of [['Profile name', 'primary'], ['Model ID', 'model-one'], ['API key', 'synthetic-profile-key'], ['Archive data since (YYYY-MM-DD)', '2025-01-01'], ['Retention statement', 'No retention'], ['Training statement', 'No training']]) {
    await fireEvent.input(await screen.findByLabelText(label), { target: { value } });
  }
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Conversation text' }));
  await fireEvent.click(screen.getByRole('radio', { name: 'Exclude sensitive content' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Create profile' }));
  await screen.findByText('Key store unavailable');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(seen.find(request => request.method === 'PATCH')?.ifMatch).toBe('"s2"'));
});

it('clears the settings load error after a successful Retry', async () => {
  let reads = 0;
  const { client } = fixture(settingsDoc(), request => {
    if (new URL(request.url).pathname === '/api/v1/settings' && request.method === 'GET' && ++reads === 1) {
      return Response.json({ message: 'Temporary settings read failure' }, { status: 503 });
    }
  });
  render(SettingsWorkspace, { client, category: 'advanced' });
  await screen.findByText("Can't load settings");
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await screen.findByLabelText('Theme');
  expect(screen.queryByRole('alert')).toBeNull();
});

it('shows unavailable message coverage after a failed initial read', async () => {
  const { client } = fixture(settingsDoc(), request => {
    if (request.url.endsWith('/operations/status')) return Response.json({ lanes: lanes(lane('message_embedding')) });
    if (request.url.includes('/search/coverage')) return Response.json({ message: 'Unavailable' }, { status: 503 });
  });
  render(SettingsWorkspace, { client });
  await screen.findByRole('list', { name: 'Features' });
  const row = screen.getByText('Message search').closest('li')!;
  expect(await within(row).findByText('Unavailable')).toBeDefined();
});

it('refreshes a pending message rebuild and keeps Contacts available', async () => {
  let building = false;
  let reads = 0;
  let finish!: (response: Response) => void;
  let sent!: () => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  // The fake server reads the build request's body before it records the build, so
  // later status reads wait for it the way the daemon registers a build on arrival.
  const buildSent = new Promise<void>(resolve => { sent = resolve; });
  const status = () => Response.json({ lanes: lanes(lane('message_embedding', building ? { active: { id: 'build-1', kind: 'message_embedding', lane: 'messages', state: 'running', started_at: '2026-01-01T00:00:00Z', counters: [] } } : {}), lane('carddav_sync', { supported_actions: ['carddav_sync'] })) });
  const { client, seen } = fixture(settingsDoc(), request => {
    if (request.url.endsWith('/operations/status')) return ++reads === 1 ? status() : buildSent.then(status);
    if (request.url.includes('/search/coverage')) return Response.json({ eligible_count: 10, embedded_count: 0, percentage: 0, cache_revision: 'cache-1', status: 'unavailable', actions: ['build_index'] });
    if (request.url.endsWith('/cli/run')) { building = true; sent(); return pending; }
  });
  render(SettingsWorkspace, { client });
  await fireEvent.click(await screen.findByRole('button', { name: 'Build index, Message search' }));
  try {
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
    await waitFor(() => expect(seen.some(request => request.path === '/api/v1/cli/run')).toBe(true));
    const row = screen.getByText('Message search').closest('li')!;
    await waitFor(() => {
      expect(within(row).getByText('Running')).toBeDefined();
      const button = within(row).getByRole('button', { name: 'Build index, Message search' });
      expect(button).toHaveProperty('disabled', true);
      expect(button.textContent).toContain('Working…');
    });
    expect(seen.filter(request => request.path === '/api/v1/operations/status')).toHaveLength(2);
    expect(screen.getByRole('button', { name: 'Sync now, Contacts' })).toHaveProperty('disabled', false);
  } finally {
    building = false;
    finish(new Response('{"type":"complete"}\n'));
  }
  await waitFor(() => expect(screen.getByRole('button', { name: 'Build index, Message search' })).toHaveProperty('disabled', false));
});

it('keeps the acknowledged named account schedule when refreshing connection summaries fails', async () => {
  let saved = false;
  const account = { configured: true, available: true, credential_configured: true, enabled: true, scheduled: false, schedule: '', account: { base_url: 'https://work.example.test/', username: 'work@example.com' } };
  const { client } = fixture(settingsDoc(), request => {
    const path = new URL(request.url).pathname;
    if (path.endsWith('/carddav/connections')) return saved ? Response.json({ message: 'Unavailable' }, { status: 500 }) : Response.json({ connections: [{ connection: 'work', status: account }] });
    if (path.endsWith('/carddav/status')) return Response.json(account);
    if (path.endsWith('/carddav/account')) {
      saved = true;
      return Response.json({ ...account.account, enabled: true, schedule: '0 3 * * *', books: 1 });
    }
  });
  render(SettingsWorkspace, { client, category: 'carddav' });
  await chooseSelectOption(await screen.findByRole('combobox', { name: /^CardDAV connection/ }), 'work');
  await screen.findByDisplayValue('work@example.com');
  await chooseSelectOption(screen.getByRole('combobox', { name: /^Presets:/ }), 'Every day at 03:00');
  await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
  await screen.findByText('CardDAV account saved. Found 1 address book.');
  await screen.findByText('Unable to load CardDAV connections.');
  expect(screen.getByRole('combobox', { name: /^Presets:/ }).textContent).toContain('Every day at 03:00');
});

it('opens document status from Overview Details without a past run', async () => {
  const { client } = fixture(settingsDoc(), request => {
    if (new URL(request.url).pathname.endsWith('/operations/status')) return Response.json({ lanes: [lane('document_extraction', { lane: 'documents', related_status: 'getDocumentIndexStatus' })] });
  });
  const onOpenStatus = vi.fn();
  render(SettingsWorkspace, { client, onOpenStatus });
  await fireEvent.click(await screen.findByRole('button', { name: 'Details, Document extraction' }));
  expect(onOpenStatus).toHaveBeenCalledWith('getDocumentIndexStatus');
});
