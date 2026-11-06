import { describe, expect, it } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/svelte';
import SettingsOverview from '../components/settings/SettingsOverview.svelte';
import type { LocalEmbeddingsStatus } from '../api/generated/models';
import { embeddingStatus } from '../../test/operations';
import { localSearchHintLabels, localSearchLabels, localSearchState } from './local-embeddings';

const off: LocalEmbeddingsStatus = { host: 'archive.example.test', enabled: false, provider: '', compatible: false, ollama_available: false, phase: 'idle', done_bytes: 0, total_bytes: 0, download_bytes: 290000000 };
const row = { id: 'message_embedding', name: 'Message search', chip: { text: 'Off', tone: 'muted' as const } };

describe('local search row', () => {
  it.each([
    ['off', off, 'Off', 'Turn on'],
    ['downloaded', { ...off, download_bytes: 0 }, 'Off', 'Turn on'],
    ['available Ollama', { ...off, ollama_available: true }, 'Off', 'Turn on'],
    ['matching Ollama', { ...off, enabled: true, provider: 'ollama', compatible: true }, 'On · Ollama', 'Use built-in instead'],
    ['other Ollama', { ...off, enabled: true, provider: 'ollama' }, 'On · Ollama', 'Use built-in instead'],
    ['on', { ...off, enabled: true, provider: 'builtin' }, 'On', undefined],
    ['waiting for setup', { ...off, running: true }, 'Turning on', undefined],
    ['downloading', { ...off, phase: 'downloading', done_bytes: 1000000, total_bytes: 290000000 }, 'Turning on', undefined],
    ['indexing', { ...off, phase: 'indexing' }, 'Turning on', undefined],
  ] as const)('%s', (_name, status, chip, action) => {
    render(SettingsOverview, { rows: [{ ...row, chip: { ...row.chip, text: 'On' } }], localSearch: localSearchState(status, embeddingStatus), localLabels: localSearchLabels(status, embeddingStatus), onSetUp: () => {}, onSources: () => {}, onStart: () => {} });
    expect(screen.getByText(chip)).toBeDefined();
    if (action) expect(screen.getByRole('button', { name: action })).toBeDefined();
    else expect(screen.queryByRole('button')).toBeNull();
    if (status === off) expect(screen.getByText('Downloads 290 MB once to archive.example.test')).toBeDefined();
    if (_name === 'downloaded') expect(screen.queryByText(/Downloads /)).toBeNull();
    if (_name === 'available Ollama') expect(screen.getByText('Uses Ollama on this computer')).toBeDefined();
    if (status.phase === 'indexing') expect(screen.getByRole('progressbar').getAttribute('aria-valuemax')).toBe('10');
    if (_name === 'other Ollama') expect(screen.getByText('Switching rebuilds the index for 10 messages.')).toBeDefined();
  });
});

for (const kind of ['on', 'ollama'] as const) {
  it(`keeps rebuild for a stale ${kind} index`, async () => {
    const calls: string[] = [];
    render(SettingsOverview, { rows: [{ ...row, chip: { text: 'Stale', tone: 'muted' as const }, action: { kind: 'start', id: 'messages', label: 'Build index' } }], localSearch: localSearchState({ ...off, enabled: true, provider: kind === 'on' ? 'builtin' : 'ollama', compatible: true, error: 'Build interrupted' }), localError: 'Build interrupted', onSetUp: () => {}, onSources: () => {}, onStart: () => calls.push('build') });
    expect(screen.getByText('Stale')).toBeDefined();
    expect(screen.getByRole('alert').textContent).toBe('Build interrupted');
    await fireEvent.click(screen.getByRole('button', { name: 'Build index, Message search' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm full rebuild' }));
    expect(calls).toEqual(['build']);
    if (kind === 'ollama') expect(screen.getByRole('button', { name: 'Use built-in instead' })).toBeDefined();
  });
}

it('discloses the download size and remote host before hint activation', () => {
 expect(localSearchHintLabels(off).hint).toBe('Search by meaning is off · Downloads 290 MB once to archive.example.test');
 expect(localSearchHintLabels({...off,ollama_available:true}).hint).toBe('Search by meaning is off · Uses Ollama on this computer');
});

it.each(['running', 'idle'] as const)('estimates the whole replacement index from observed throughput when %s', (jobState) => {
  const status = { ...off, enabled: true, provider: 'ollama', compatible: false };
  const embedding = { ...embeddingStatus, eligible: 48000, current: 47999, pending: 1, messages_per_minute: 2400, eta_seconds: jobState === 'running' ? 0.025 : null, job: { ...embeddingStatus.job, state: jobState } };
  render(SettingsOverview, { rows: [row], localSearch: localSearchState(status, embedding), localLabels: localSearchLabels(status, embedding), onSetUp: () => {}, onSources: () => {}, onStart: () => {} });
  expect(screen.getByText('Switching rebuilds the index for 48,000 messages, about 20 minutes.')).toBeDefined();
});
