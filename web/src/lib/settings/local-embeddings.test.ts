import { describe, expect, it } from 'vitest';
import type { SearchByMeaningState, SearchByMeaningLabels } from '@kenn-io/kit-ui';
import { render, screen } from '@testing-library/svelte';
import SettingsOverview from '../components/settings/SettingsOverview.svelte';
import type { LocalEmbeddingsStatus } from '../api/generated/models';
import { embeddingStatus } from '../../test/operations';
import { localSearchHintLabels, localSearchLabels, localSearchState } from './local-embeddings';

const off: LocalEmbeddingsStatus = { host: 'archive.example.test', enabled: false, provider: '', compatible: false, ollama_available: false, phase: 'idle', done_bytes: 0, total_bytes: 0, download_bytes: 290000000 };
const row = { id: 'message_embedding', name: 'Message search', chip: { text: 'Off', tone: 'muted' as const } };

describe('local search row', () => {
  it.each<{ name: string; status: LocalEmbeddingsStatus; embedding?: typeof embeddingStatus; state: SearchByMeaningState; action?: string; labels?: Partial<SearchByMeaningLabels>; progress?: { now: string; max: string } }>([
    { name: 'off', status: off, state: { kind: 'off', downloadBytes: 290000000, host: 'archive.example.test' }, action: 'Turn on' },
    { name: 'downloaded', status: { ...off, download_bytes: 0 }, state: { kind: 'downloaded' }, action: 'Turn on' },
    { name: 'available Ollama', labels: { downloaded: 'Uses Ollama on this computer' }, status: { ...off, ollama_available: true }, state: { kind: 'downloaded' }, action: 'Turn on' },
    { name: 'installed built-in with Ollama available', labels: { downloaded: undefined }, status: { ...off, provider: 'builtin', compatible: true, ollama_available: true, download_bytes: 0 }, state: { kind: 'downloaded' }, action: 'Turn on' },
    { name: 'failed built-in startup with Ollama available', labels: { downloaded: undefined }, status: { ...off, provider: 'builtin', compatible: true, ollama_available: true, error: 'Local model files need setup. Turn on to retry.' }, state: { kind: 'off', downloadBytes: 290000000, host: 'archive.example.test' }, action: 'Turn on' },
    { name: 'matching Ollama', status: { ...off, enabled: true, provider: 'ollama', compatible: true }, state: { kind: 'ollama' }, action: 'Use built-in instead' },
    { name: 'other Ollama', labels: { rebuild: 'Switching rebuilds the index for {count} {items}.' }, status: { ...off, enabled: true, provider: 'ollama' }, state: { kind: 'ollama-other', itemCount: 10, rebuildMs: 0 }, action: 'Use built-in instead' },
    { name: 'other Ollama with observed throughput', labels: { rebuild: 'Switching rebuilds the index for {count} {items}, {time}.' }, status: { ...off, enabled: true, provider: 'ollama' }, embedding: { ...embeddingStatus, eligible: 48000, current: 47999, pending: 1, messages_per_minute: 2400, eta_seconds: 0.025, job: { ...embeddingStatus.job, state: 'running' } }, state: { kind: 'ollama-other', itemCount: 48000, rebuildMs: 1200000 }, action: 'Use built-in instead' },
    { name: 'matching Ollama with document search', labels: { keepsIndex: 'Switching keeps your message index. Document\u00a0search requires consent for the built-in provider.' }, status: { ...off, enabled: true, provider: 'ollama', compatible: true, document_rebuild: true }, state: { kind: 'ollama' }, action: 'Use built-in instead' },
    { name: 'other Ollama with document search', labels: { rebuild: 'Switching rebuilds the index for {count} {items}. Document\u00a0search requires consent for the built-in provider.' }, status: { ...off, enabled: true, provider: 'ollama', document_rebuild: true }, state: { kind: 'ollama-other', itemCount: 10, rebuildMs: 0 }, action: 'Use built-in instead' },
    { name: 'on', status: { ...off, enabled: true, provider: 'builtin' }, state: { kind: 'on' } },
    { name: 'waiting for setup', labels: { downloading: 'Preparing search by meaning' }, status: { ...off, running: true }, state: { kind: 'downloading', doneBytes: 0, totalBytes: 290000000 }, progress: { now: '0', max: '290000000' } },
    { name: 'downloading', status: { ...off, phase: 'downloading', running: true, done_bytes: 1000000, total_bytes: 290000000 }, state: { kind: 'downloading', doneBytes: 1000000, totalBytes: 290000000 }, progress: { now: '1000000', max: '290000000' } },
    { name: 'indexing', status: { ...off, enabled: true, provider: 'builtin', running: true, phase: 'indexing' }, state: { kind: 'indexing', done: 0, total: 10 }, progress: { now: '0', max: '10' } },
  ])('$name', ({ status, embedding = embeddingStatus, state, action, labels: expectedLabels, progress }) => {
    expect(localSearchState(status, embedding)).toEqual(state);
    const labels = localSearchLabels(status, embedding);
    expect(labels.meanwhile).toBe('Keyword search works');
    for (const key of Object.keys(expectedLabels ?? {}) as (keyof SearchByMeaningLabels)[]) expect(labels[key]).toBe(expectedLabels![key]);
    const chip = status.enabled ? { text: 'Incomplete', tone: 'warning' as const } : row.chip;
    render(SettingsOverview, { rows: [{ ...row, chip }], localSearch: state, localLabels: labels, localError: status.error, onSetUp: () => {}, onSources: () => {}, onStart: () => {} });
    if (action) expect(screen.getByRole('button', { name: action })).toBeDefined();
    else expect(screen.queryByRole('button')).toBeNull();
    if (status.error) expect(screen.getByRole('alert').textContent).toBe(status.error);
    if (progress) {
      expect(screen.getByRole('progressbar').getAttribute('aria-valuenow')).toBe(progress.now);
      expect(screen.getByRole('progressbar').getAttribute('aria-valuemax')).toBe(progress.max);
    }
  });
});

it.each([{ provider: 'builtin', switchAction: false }, { provider: 'ollama', switchAction: true }])('keeps rebuild for a stale $provider index', ({ provider, switchAction }) => {
  const status = { ...off, enabled: true, provider, compatible: true, error: 'Build interrupted' };
  render(SettingsOverview, { rows: [{ ...row, chip: { text: 'Stale', tone: 'muted' as const }, action: { kind: 'start', id: 'messages', label: 'Build index' } }], localSearch: localSearchState(status, embeddingStatus), localLabels: localSearchLabels(status, embeddingStatus), localError: 'Build interrupted', onSetUp: () => {}, onSources: () => {}, onStart: () => {} });
  expect(Boolean(screen.queryByRole('button', { name: 'Use built-in instead' }))).toBe(switchAction);
});

it.each([
  { name: 'available Ollama', status: { ...off, ollama_available: true }, disclosure: 'Uses Ollama on this computer' },
  { name: 'paused built-in despite available Ollama', status: { ...off, provider: 'builtin', compatible: true, ollama_available: true }, disclosure: 'Downloads 290 MB once to archive.example.test' },
  { name: 'installed built-in despite available Ollama', status: { ...off, provider: 'builtin', compatible: true, ollama_available: true, download_bytes: 0 }, disclosure: 'Already on this computer' },
])('discloses $name before hint activation', ({ status, disclosure }) => {
  expect(localSearchHintLabels(status).hint).toBe(`Search by meaning is off · ${disclosure}`);
});
