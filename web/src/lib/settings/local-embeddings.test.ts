import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import SettingsOverview from '../components/settings/SettingsOverview.svelte';
import type { LocalEmbeddingsStatus } from '../api/generated/models';
import { embeddingStatus } from '../../test/operations';
import { localSearchLabels, localSearchState } from './local-embeddings';

const off: LocalEmbeddingsStatus = { enabled: false, provider: '', compatible: false, ollama_available: false, phase: 'idle', done_bytes: 0, total_bytes: 0, download_bytes: 290000000 };
const row = { id: 'message_embedding', name: 'Message search', chip: { text: 'Off', tone: 'muted' as const } };

describe('local search row', () => {
  it.each([
    ['off', off, 'Off', 'Turn on'],
    ['downloaded', { ...off, download_bytes: 0 }, 'Off', 'Turn on'],
    ['available Ollama', { ...off, ollama_available: true }, 'Off', 'Turn on'],
    ['matching Ollama', { ...off, enabled: true, provider: 'ollama', compatible: true }, 'On · Ollama', 'Use built-in instead'],
    ['other Ollama', { ...off, enabled: true, provider: 'ollama' }, 'On · Ollama', 'Use built-in instead'],
    ['on', { ...off, enabled: true, provider: 'builtin' }, 'On', undefined],
    ['downloading', { ...off, phase: 'downloading', done_bytes: 1000000, total_bytes: 290000000 }, 'Turning on', undefined],
    ['indexing', { ...off, phase: 'indexing' }, 'Turning on', undefined],
    ['retry', { ...off, enabled: true, error: 'Download interrupted' }, 'Off', 'Turn on'],
  ] as const)('%s', (_name, status, chip, action) => {
    render(SettingsOverview, { rows: [row], localSearch: localSearchState(status, embeddingStatus, 'archive.example.test'), localLabels: localSearchLabels(status, embeddingStatus), onSetUp: () => {}, onSources: () => {}, onStart: () => {} });
    expect(screen.getByText(chip)).toBeDefined();
    if (action) expect(screen.getByRole('button', { name: action })).toBeDefined();
    else expect(screen.queryByRole('button')).toBeNull();
    if (status === off) expect(screen.getByText('Downloads 290 MB once to archive.example.test')).toBeDefined();
    if (status.phase === 'indexing') expect(screen.getByRole('progressbar').getAttribute('aria-valuemax')).toBe('10');
    if (_name === 'other Ollama') expect(screen.getByText('Switching rebuilds the index for 10 messages')).toBeDefined();
  });
});
