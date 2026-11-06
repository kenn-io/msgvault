import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import SettingsOverview from '../components/settings/SettingsOverview.svelte';
import type { LocalEmbeddingsStatus } from '../api/generated/models';
import { embeddingStatus } from '../../test/operations';
import { localSearchHintLabels, localSearchLabels, localSearchState } from './local-embeddings';

const off: LocalEmbeddingsStatus = { host: 'archive.example.test', enabled: false, provider: '', compatible: false, ollama_available: false, phase: 'idle', done_bytes: 0, total_bytes: 0, download_bytes: 290000000 };
const row = { id: 'message_embedding', name: 'Message search', chip: { text: 'Off', tone: 'muted' as const } };

describe('local search row', () => {
  it.each<{ name: string; status: LocalEmbeddingsStatus; embedding?: typeof embeddingStatus; chip: string; action?: string; description?: string; absent?: RegExp; progress?: { now: string; max: string } }>([
    { name: 'off', status: off, chip: 'Off', action: 'Turn on', description: 'Downloads 290 MB once to archive.example.test' },
    { name: 'downloaded', status: { ...off, download_bytes: 0 }, chip: 'Off', action: 'Turn on', description: 'Already on this computer' },
    { name: 'available Ollama', status: { ...off, ollama_available: true }, chip: 'Off', action: 'Turn on', description: 'Uses Ollama on this computer' },
    { name: 'matching Ollama', status: { ...off, enabled: true, provider: 'ollama', compatible: true }, chip: 'On · Ollama', action: 'Use built-in instead', description: 'Switching keeps your index', absent: /Switching rebuilds/ },
    { name: 'other Ollama', status: { ...off, enabled: true, provider: 'ollama' }, chip: 'On · Ollama', action: 'Use built-in instead', description: 'Switching rebuilds the index for 10 messages.' },
    { name: 'other Ollama with observed throughput', status: { ...off, enabled: true, provider: 'ollama' }, embedding: { ...embeddingStatus, eligible: 48000, current: 47999, pending: 1, messages_per_minute: 2400, eta_seconds: 0.025, job: { ...embeddingStatus.job, state: 'running' } }, chip: 'On · Ollama', action: 'Use built-in instead', description: 'Switching rebuilds the index for 48,000 messages, about 20 minutes.' },
    { name: 'matching Ollama with document search', status: { ...off, enabled: true, provider: 'ollama', compatible: true, document_rebuild: true }, chip: 'On · Ollama', action: 'Use built-in instead', description: 'Switching keeps your message index. Document search requires consent for the built-in provider.' },
    { name: 'other Ollama with document search', status: { ...off, enabled: true, provider: 'ollama', document_rebuild: true }, chip: 'On · Ollama', action: 'Use built-in instead', description: 'Switching rebuilds the index for 10 messages. Document search requires consent for the built-in provider.' },
    { name: 'on', status: { ...off, enabled: true, provider: 'builtin' }, chip: 'On' },
    { name: 'waiting for setup', status: { ...off, running: true }, chip: 'Turning on', description: 'Preparing search by meaning · Keyword search works', progress: { now: '0', max: '290000000' } },
    { name: 'downloading', status: { ...off, phase: 'downloading', done_bytes: 1000000, total_bytes: 290000000 }, chip: 'Turning on', progress: { now: '1000000', max: '290000000' } },
    { name: 'indexing', status: { ...off, phase: 'indexing' }, chip: 'Turning on', progress: { now: '0', max: '10' } },
  ])('$name', ({ status, embedding = embeddingStatus, chip, action, description, absent, progress }) => {
    render(SettingsOverview, { rows: [row], localSearch: localSearchState(status, embedding), localLabels: localSearchLabels(status, embedding), onSetUp: () => {}, onSources: () => {}, onStart: () => {} });
    expect(screen.getByText(chip)).toBeDefined();
    if (action) expect(screen.getByRole('button', { name: action })).toBeDefined();
    else expect(screen.queryByRole('button')).toBeNull();
    if (description) expect(screen.getByText(description)).toBeDefined();
    if (absent) expect(screen.queryByText(absent)).toBeNull();
    if (progress) {
      expect(screen.getByRole('progressbar').getAttribute('aria-valuenow')).toBe(progress.now);
      expect(screen.getByRole('progressbar').getAttribute('aria-valuemax')).toBe(progress.max);
    }
  });
});

it.each([{ provider: 'builtin', switchAction: false }, { provider: 'ollama', switchAction: true }])('keeps rebuild for a stale $provider index', ({ provider, switchAction }) => {
  const status = { ...off, enabled: true, provider, compatible: true, error: 'Build interrupted' };
  render(SettingsOverview, { rows: [{ ...row, chip: { text: 'Stale', tone: 'muted' as const }, action: { kind: 'start', id: 'messages', label: 'Build index' } }], localSearch: localSearchState(status, embeddingStatus), localLabels: localSearchLabels(status, embeddingStatus), localError: 'Build interrupted', onSetUp: () => {}, onSources: () => {}, onStart: () => {} });
  expect(screen.getByText('Stale')).toBeDefined();
  expect(screen.getByRole('alert').textContent).toBe('Build interrupted');
  expect(Boolean(screen.queryByRole('button', { name: 'Use built-in instead' }))).toBe(switchAction);
  expect(screen.getByRole('button', { name: 'Build index, Message search' })).toBeDefined();
});

it('discloses the download size and remote host before hint activation', () => {
 expect(localSearchHintLabels(off).hint).toBe('Search by meaning is off · Downloads 290 MB once to archive.example.test');
 expect(localSearchHintLabels({...off,ollama_available:true}).hint).toBe('Search by meaning is off · Uses Ollama on this computer');
});
