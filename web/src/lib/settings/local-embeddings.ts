import type { SearchByMeaningState, SearchByMeaningLabels } from '@kenn-io/kit-ui';
import type { EmbeddingStatus, LocalEmbeddingsStatus } from '../api/generated/models';

export function localSearchState(status: LocalEmbeddingsStatus, embedding?: EmbeddingStatus, hostname = ''): SearchByMeaningState {
  if (status.phase === 'downloading' || (status.running && status.phase === 'idle')) return { kind: 'downloading', doneBytes: status.done_bytes, totalBytes: status.total_bytes || status.download_bytes };
  if (status.phase === 'indexing') return {
    kind: 'indexing', done: Number(embedding?.current) || 0, total: Number(embedding?.eligible) || 0,
    ...(embedding?.eta_seconds ? { etaMs: Number(embedding.eta_seconds) * 1000 } : {}),
  };
  if (status.enabled) {
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
    meanwhile: 'Keyword search works',
    ...(status.running && status.phase === 'idle' ? { downloading: 'Preparing search by meaning' } : {}),
    ...(!status.enabled && status.ollama_available ? { downloaded: 'Uses Ollama on this computer' } : {}),
    rebuild: ('Switching rebuilds the index for {count} {items}' + (embedding?.eta_seconds ? ', {time}' : '')) + (status.document_rebuild ? '. Document search also rebuilds on this computer.' : '.'),
    ...(status.document_rebuild ? { keepsIndex: 'Switching keeps your message index and rebuilds document search on this computer' } : {}),
  };
}

export function localSearchHintLabels(status: LocalEmbeddingsStatus, hostname: string): Partial<SearchByMeaningLabels> {
  const state = localSearchState(status, undefined, hostname);
  const labels = localSearchLabels(status);
  const disclosure = state.kind === 'off'
    ? `Downloads ${new Intl.NumberFormat(undefined, { style: 'unit', unit: 'megabyte', unitDisplay: 'short', maximumFractionDigits: 0 }).format(state.downloadBytes / 1_000_000)} once to ${state.host || 'this computer'}`
    : labels.downloaded || 'Already on this computer';
  return { ...labels, hint: `Search by meaning is off · ${disclosure}` };
}
