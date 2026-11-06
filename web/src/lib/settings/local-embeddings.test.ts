import { describe, expect, it } from 'vitest';
import type { SearchByMeaningState, SearchByMeaningLabels } from '@kenn-io/kit-ui';
import type { LocalEmbeddingsStatus } from '../api/generated/models';
import { embeddingStatus } from '../../test/operations';
import { localSearchHintLabels, localSearchLabels, localSearchState } from './local-embeddings';

const off: LocalEmbeddingsStatus = { host: 'archive.example.test', enabled: false, provider: '', compatible: false, ollama_available: false, phase: 'idle', done_bytes: 0, total_bytes: 0, download_bytes: 290000000 };

describe('local search row', () => {
  it.each<{ name: string; status: LocalEmbeddingsStatus; embedding?: typeof embeddingStatus; state: SearchByMeaningState; labels?: Partial<SearchByMeaningLabels>; progress?: { now: string; max: string } }>([
    { name: 'off', status: off, state: { kind: 'off', downloadBytes: 290000000, host: 'archive.example.test' } },
    { name: 'downloaded', status: { ...off, download_bytes: 0 }, state: { kind: 'downloaded' } },
    { name: 'available Ollama', labels: { downloaded: 'Uses Ollama on this computer' }, status: { ...off, ollama_available: true }, state: { kind: 'downloaded' } },
    { name: 'installed built-in with Ollama available', labels: { downloaded: undefined }, status: { ...off, provider: 'builtin', compatible: true, ollama_available: true, download_bytes: 0 }, state: { kind: 'downloaded' } },
    { name: 'matching Ollama', status: { ...off, enabled: true, provider: 'ollama', compatible: true }, state: { kind: 'ollama' } },
    { name: 'other Ollama', labels: { rebuild: 'Switching rebuilds the index for {count} {items}. Downloads 290 MB once to archive.example.test.' }, status: { ...off, enabled: true, provider: 'ollama' }, state: { kind: 'ollama-other', itemCount: 10, rebuildMs: 0 } },
    { name: 'other Ollama with observed throughput', labels: { rebuild: 'Switching rebuilds the index for {count} {items}, {time}. Downloads 290 MB once to archive.example.test.' }, status: { ...off, enabled: true, provider: 'ollama' }, embedding: { ...embeddingStatus, eligible: 48000, current: 47999, pending: 1, messages_per_minute: 2400, eta_seconds: 0.025, job: { ...embeddingStatus.job, state: 'running' } }, state: { kind: 'ollama-other', itemCount: 48000, rebuildMs: 1200000 } },
    { name: 'matching Ollama with document search', labels: { keepsIndex: 'Switching keeps your message index. Downloads 290 MB once to archive.example.test. Document\u00a0search requires consent for the built-in provider.' }, status: { ...off, enabled: true, provider: 'ollama', compatible: true, document_rebuild: true }, state: { kind: 'ollama' } },
    { name: 'other Ollama with document search', labels: { rebuild: 'Switching rebuilds the index for {count} {items}. Downloads 290 MB once to archive.example.test. Document\u00a0search requires consent for the built-in provider.' }, status: { ...off, enabled: true, provider: 'ollama', document_rebuild: true }, state: { kind: 'ollama-other', itemCount: 10, rebuildMs: 0 } },
    { name: 'on', status: { ...off, enabled: true, provider: 'builtin' }, state: { kind: 'on' } },
    { name: 'waiting for setup', labels: { downloading: 'Preparing search by meaning' }, status: { ...off, running: true }, state: { kind: 'downloading', doneBytes: 0, totalBytes: 290000000 } },
    { name: 'downloading', status: { ...off, phase: 'downloading', running: true, done_bytes: 1000000, total_bytes: 290000000 }, state: { kind: 'downloading', doneBytes: 1000000, totalBytes: 290000000 } },
    { name: 'indexing', status: { ...off, enabled: true, provider: 'builtin', running: true, phase: 'indexing' }, state: { kind: 'indexing', done: 0, total: 10 } },
  ])('$name', ({ status, embedding = embeddingStatus, state, labels: expectedLabels }) => {
    expect(localSearchState(status, embedding)).toEqual(state);
    const labels = localSearchLabels(status, embedding);
    expect(labels.meanwhile).toBe('Keyword search works');
    for (const key of Object.keys(expectedLabels ?? {}) as (keyof SearchByMeaningLabels)[]) expect(labels[key]).toBe(expectedLabels![key]);
  });
});

it.each([
  { name: 'available Ollama', status: { ...off, ollama_available: true }, disclosure: 'Uses Ollama on this computer' },
  { name: 'paused built-in despite available Ollama', status: { ...off, provider: 'builtin', compatible: true, ollama_available: true }, disclosure: 'Downloads 290 MB once to archive.example.test' },
  { name: 'installed built-in despite available Ollama', status: { ...off, provider: 'builtin', compatible: true, ollama_available: true, download_bytes: 0 }, disclosure: 'Already on this computer' },
])('discloses $name before hint activation', ({ status, disclosure }) => {
  expect(localSearchHintLabels(status).hint).toBe(`Search by meaning is off · ${disclosure}`);
});
