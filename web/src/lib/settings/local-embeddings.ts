import type { SearchByMeaningState, SearchByMeaningLabels } from '@kenn-io/kit-ui';
import type { EmbeddingStatus, LocalEmbeddingsStatus } from '../api/generated/models';

export function localSearchState(status: LocalEmbeddingsStatus, embedding?: EmbeddingStatus, hostname = ''): SearchByMeaningState {
  if (status.phase === 'downloading') return { kind: 'downloading', doneBytes: status.done_bytes, totalBytes: status.total_bytes || status.download_bytes };
  if (status.phase === 'indexing') return {
    kind: 'indexing', done: Number(embedding?.current) || 0, total: Number(embedding?.eligible) || 0,
    ...(embedding?.eta_seconds ? { etaMs: Number(embedding.eta_seconds) * 1000 } : {}),
  };
  if (status.enabled && !status.error) {
    if (status.provider !== 'ollama') return { kind: 'on' };
    if (status.compatible) return { kind: 'ollama' };
    return { kind: 'ollama-other', itemCount: Number(embedding?.eligible) || 0, rebuildMs: Number(embedding?.eta_seconds) * 1000 || 0 };
  }
  if (status.download_bytes === 0 || status.ollama_available) return { kind: 'downloaded' };
  const remote = hostname && !['localhost', '127.0.0.1', '[::1]', '::1'].includes(hostname);
  return { kind: 'off', downloadBytes: status.download_bytes, ...(remote ? { host: hostname } : {}) };
}

export function localSearchLabels(status: LocalEmbeddingsStatus, embedding?: EmbeddingStatus): Partial<SearchByMeaningLabels> {
  return {
    ...(!embedding?.eta_seconds ? { rebuild: 'Switching rebuilds the index for {count} {items}' } : {}),
  };
}
