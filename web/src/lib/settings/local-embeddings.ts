import type { SearchByMeaningState, SearchByMeaningLabels } from '@kenn-io/kit-ui';
import type { EmbeddingStatus, LocalEmbeddingsStatus } from '../api/generated/models';

function rebuildDuration(embedding?: EmbeddingStatus): number {
  const rate = Number(embedding?.messages_per_minute);
  const count = Number(embedding?.eligible);
  return rate > 0 && count > 0 ? count / rate * 60_000 : 0;
}

export function localSearchState(status: LocalEmbeddingsStatus, embedding?: EmbeddingStatus): SearchByMeaningState {
  if (status.phase === 'downloading' || (status.running && status.phase === 'idle')) return { kind: 'downloading', doneBytes: status.done_bytes, totalBytes: status.total_bytes || status.download_bytes };
  if (status.phase === 'indexing') return {
    kind: 'indexing', done: Number(embedding?.current) || 0, total: Number(embedding?.eligible) || 0,
    ...(embedding?.eta_seconds ? { etaMs: Number(embedding.eta_seconds) * 1000 } : {}),
  };
  if (status.enabled) {
    if (status.provider !== 'ollama') return { kind: 'on' };
    if (status.compatible) return { kind: 'ollama' };
    return { kind: 'ollama-other', itemCount: Number(embedding?.eligible) || 0, rebuildMs: rebuildDuration(embedding) };
  }
  if (status.download_bytes === 0 || status.ollama_available) return { kind: 'downloaded' };
  return { kind: 'off', downloadBytes: status.download_bytes, host: status.host };
}

export function localSearchLabels(status: LocalEmbeddingsStatus, embedding?: EmbeddingStatus): Partial<SearchByMeaningLabels> {
  return {
    meanwhile: 'Keyword search works',
    ...(status.running && status.phase === 'idle' ? { downloading: 'Preparing search by meaning' } : {}),
    ...(!status.enabled && status.ollama_available ? { downloaded: 'Uses Ollama on this computer' } : {}),
    rebuild: ('Switching rebuilds the index for {count} {items}' + (rebuildDuration(embedding) ? ', {time}' : '')) + (status.document_rebuild ? '. Document search requires consent for the built-in provider.' : '.'),
    ...(status.document_rebuild ? { keepsIndex: 'Switching keeps your message index. Document search requires consent for the built-in provider.' } : {}),
  };
}

export function localSearchHintLabels(status: LocalEmbeddingsStatus): Partial<SearchByMeaningLabels> {
  const state = localSearchState(status);
  const labels = localSearchLabels(status);
  const disclosure = state.kind === 'off'
    ? `Downloads ${new Intl.NumberFormat(undefined, { style: 'unit', unit: 'megabyte', unitDisplay: 'short', maximumFractionDigits: 0 }).format(state.downloadBytes / 1_000_000)} once to ${state.host || 'this computer'}`
    : labels.downloaded || 'Already on this computer';
  return { ...labels, hint: `Search by meaning is off · ${disclosure}` };
}
