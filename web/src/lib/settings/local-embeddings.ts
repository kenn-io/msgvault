import type { SearchByMeaningState, SearchByMeaningLabels } from '@kenn-io/kit-ui';
import type { EmbeddingStatus, LocalEmbeddingsStatus } from '../api/generated/models';

// kit-ui formats the row's size itself but doesn't export the formatter the hint needs.
function megabytes(bytes: number): string {
  return new Intl.NumberFormat(undefined, { style: 'unit', unit: 'megabyte', unitDisplay: 'short', maximumFractionDigits: 0 }).format(bytes / 1_000_000);
}

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
  return {
    ...(status.running && status.phase === 'idle' ? { downloading: 'Preparing' } : {}),
    ...(!status.enabled ? { downloaded: `${status.provider === 'ollama' ? 'Uses Ollama' : 'Already'} on ${status.host || 'this computer'}` } : {}),
    keepsIndex: '',
  };
}

export function localSearchHintLabels(status: LocalEmbeddingsStatus): Partial<SearchByMeaningLabels> {
  const labels = localSearchLabels(status);
  const disclosure = status.provider !== 'ollama' && status.download_bytes
    ? `Downloads ${megabytes(status.download_bytes)} once${status.host ? ` to ${status.host}` : ''}`
    : labels.downloaded;
  return { ...labels, hint: `Search by meaning is off · ${disclosure}` };
}
