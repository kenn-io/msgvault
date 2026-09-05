import type { VisualTextSearchRequest } from './generated/models/visualTextSearchRequest';
import type { SearchVisualAttachmentsBodyTwo } from './generated/models/searchVisualAttachmentsBodyTwo';
import { orvalFetch, type GeneratedRequest, type GeneratedRequestOptions } from './runtime';

export function orvalVisualSearch<T>(
  config: GeneratedRequest & { data: VisualTextSearchRequest | SearchVisualAttachmentsBodyTwo },
  options: GeneratedRequestOptions = {}
) {
  const headers = new Headers(config.headers);
  if ('image' in config.data) {
    const { image, ...fields } = config.data;
    const data = new FormData();
    data.append('image', image);
    for (const [key, value] of Object.entries(fields)) {
      if (value === undefined) continue;
      for (const item of Array.isArray(value) ? value : [value]) {
        data.append(key, item);
      }
    }
    // The shared transport leaves multipart boundaries to browser fetch.
    headers.delete('Content-Type');
    return orvalFetch<T>({ ...config, headers, data }, options);
  }
  headers.set('Content-Type', 'application/json');
  return orvalFetch<T>({ ...config, headers }, options);
}
