export type GeneratedRequestOptions = RequestInit & {
  fetch?: typeof fetch;
  onResponse?: (response: Response) => void;
};

type GeneratedRequest = {
  url: string;
  method: string;
  headers?: HeadersInit;
  params?: object;
  data?: unknown;
  responseType?: 'blob' | 'text' | 'json';
};

export type APIErrorBody = {
  error?: string;
  message?: string;
  [key: string]: unknown;
};

export type APIResponse<T> = {
  data?: T;
  error?: APIErrorBody;
  response: Response;
};

async function request<T>(
  config: GeneratedRequest,
  options: GeneratedRequestOptions,
  responseType: 'blob' | 'json' | 'stream' | 'text' | undefined
): Promise<APIResponse<T>> {
  const { fetch: fetchFn = fetch, onResponse, ...requestInit } = options;
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(config.params ?? {})) {
    if (value !== undefined) params.append(key, value === null ? 'null' : String(value));
  }
  const query = params.toString();
  const headers = new Headers(config.headers);
  new Headers(requestInit.headers).forEach((value, key) => headers.set(key, value));
  const body = headers.get('Content-Type')?.includes('application/json')
    ? JSON.stringify(config.data)
    : (config.data as BodyInit | undefined);
  // Fetch supplies the boundary for generated multipart bodies.
  if (body instanceof FormData) headers.delete('Content-Type');
  const response = await fetchFn(query ? `${config.url}?${query}` : config.url, {
    ...requestInit,
    method: config.method,
    headers,
    body
  });
  onResponse?.(response);
  let data: unknown;
  if (![204, 205, 304].includes(response.status)) {
    if (response.ok && responseType === 'blob') data = await response.blob();
    else if (response.ok && responseType === 'stream') data = response.body;
    else {
      const text = await response.text();
      if (response.ok && responseType === 'text') data = text;
      else {
        try {
          data = text ? JSON.parse(text) : undefined;
        } catch {
          data = text;
        }
      }
    }
  }
  if (!response.ok) return { error: data as APIErrorBody, response };
  return { data: data as T, response };
}

export function orvalFetch<T>(config: GeneratedRequest, options: GeneratedRequestOptions = {}) {
  return request<T>(config, options, config.responseType);
}

// Preview limits apply to the unread byte stream before buffering. Orval infers
// this return type instead of declaring a Blob for streaming operations.
export function orvalStream<_T>(config: GeneratedRequest, options: GeneratedRequestOptions = {}) {
  return request<ReadableStream<Uint8Array>>(config, options, 'stream');
}

export function orvalText<_T>(config: GeneratedRequest, options: GeneratedRequestOptions = {}) {
  return request<string>(config, options, 'text');
}
