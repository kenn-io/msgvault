import { describe, expect, it } from 'vitest';
import type { SearchByMeaningState, SearchByMeaningLabels } from '@kenn-io/kit-ui';
import type { LocalEmbeddingsStatus } from '../api/generated/models';
import { embeddingStatus } from '../../test/operations';
import { localSearchHintLabels, localSearchLabels, localSearchState } from './local-embeddings';

const off: LocalEmbeddingsStatus = { host: 'archive.example.test', enabled: false, provider: '', phase: 'idle', done_bytes: 0, total_bytes: 0, download_bytes: 290000000 };

describe('local search row', () => {
  it.each<{ name: string; status: LocalEmbeddingsStatus; state: SearchByMeaningState; labels?: Partial<SearchByMeaningLabels> }>([
    { name: 'off on another machine', status: off, state: { kind: 'off', downloadBytes: 290000000, host: 'archive.example.test' } },
    { name: 'off on this machine', status: { ...off, host: '' }, state: { kind: 'off', downloadBytes: 290000000 } },
    { name: 'downloaded', labels: { downloaded: 'Already on archive.example.test' }, status: { ...off, download_bytes: 0 }, state: { kind: 'downloaded' } },
    { name: 'Ollama not set up', labels: { downloaded: 'Uses Ollama on archive.example.test' }, status: { ...off, provider: 'ollama' }, state: { kind: 'downloaded' } },
    { name: 'Ollama', labels: { keepsIndex: '' }, status: { ...off, enabled: true, provider: 'ollama' }, state: { kind: 'ollama' } },
    { name: 'on', status: { ...off, enabled: true, provider: 'builtin', download_bytes: 0 }, state: { kind: 'on' } },
    { name: 'waiting for setup', labels: { downloading: 'Preparing' }, status: { ...off, running: true }, state: { kind: 'downloading', doneBytes: 0, totalBytes: 290000000 } },
    { name: 'downloading', labels: { downloading: undefined }, status: { ...off, phase: 'downloading', running: true, done_bytes: 1000000, total_bytes: 290000000 }, state: { kind: 'downloading', doneBytes: 1000000, totalBytes: 290000000 } },
    { name: 'indexing', status: { ...off, enabled: true, provider: 'builtin', download_bytes: 0, running: true, phase: 'indexing' }, state: { kind: 'indexing', done: 0, total: 10 } },
  ])('$name', ({ status, state, labels: expectedLabels }) => {
    expect(localSearchState(status, embeddingStatus)).toEqual(state);
    const labels = localSearchLabels(status);
    for (const key of Object.keys(expectedLabels ?? {}) as (keyof SearchByMeaningLabels)[]) expect(labels[key]).toBe(expectedLabels![key]);
  });
});

it.each([
  { name: 'a download to another machine', status: off, disclosure: 'Downloads 290 MB once to archive.example.test' },
  { name: 'a download to this machine', status: { ...off, host: '' }, disclosure: 'Downloads 290 MB once' },
  { name: 'an installed server', status: { ...off, host: '', download_bytes: 0 }, disclosure: 'Already on this computer' },
  { name: 'a running Ollama', status: { ...off, provider: 'ollama' }, disclosure: 'Uses Ollama on archive.example.test' },
])('discloses $name before hint activation', ({ status, disclosure }) => {
  expect(localSearchHintLabels(status).hint).toBe(`Search by meaning is off · ${disclosure}`);
});
