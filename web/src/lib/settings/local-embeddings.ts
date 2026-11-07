import type { SearchByMeaningState, SearchByMeaningLabels } from '@kenn-io/kit-ui';
import type { EmbeddingStatus, LocalEmbeddingsStatus } from '../api/generated/models';

export function localSearchState(status: LocalEmbeddingsStatus, embedding?: EmbeddingStatus): SearchByMeaningState {
  if (status.phase === 'downloading' || (status.running && status.phase === 'idle')) return { kind: 'downloading', doneBytes: status.done_bytes, totalBytes: status.total_bytes || status.download_bytes };
  if (status.phase === 'indexing') return {
    kind: 'indexing', done: Number(embedding?.current) || 0, total: Number(embedding?.eligible) || 0,
    ...(embedding?.eta_seconds ? { etaMs: Number(embedding.eta_seconds) * 1000 } : {}),
  };
  if (status.enabled) return { kind: status.provider === 'ollama' ? 'ollama' : 'on' };
  if (status.provider === 'ollama' || status.download_bytes === 0) return { kind: 'downloaded' };
  return { kind: 'off', downloadBytes: status.download_bytes, ...(status.host ? { host: status.host } : {}) };
}

export function localSearchLabels(status: LocalEmbeddingsStatus): Partial<SearchByMeaningLabels> {
  const where = status.host ? ` on ${status.host}` : '';
  const downloaded = status.provider === 'ollama' ? `Uses Ollama${where}` : where && `Already${where}`;
  return {
    ...(status.running && status.phase === 'idle' ? { downloading: 'Preparing' } : {}),
    ...(!status.enabled && downloaded ? { downloaded } : {}),
    keepsIndex: '',
  };
}

export function sentence(message: string): string {
  return message && !/[.!?]$/.test(message) ? `${message}.` : message;
}
