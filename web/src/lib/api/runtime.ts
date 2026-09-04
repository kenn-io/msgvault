export type GeneratedRequestOptions = RequestInit & {
  fetch?: typeof fetch;
  onResponse?: (response: Response) => void;
  parseAs?: 'blob' | 'json' | 'stream' | 'text';
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

export async function orvalFetch<T>(
  url: string,
  options: GeneratedRequestOptions = {}
): Promise<APIResponse<T>> {
  const { fetch: fetchFn = fetch, onResponse, parseAs, ...requestInit } = options;
  const response = await fetchFn(url, requestInit);
  onResponse?.(response);
  const contentType = response.headers.get('Content-Type') ?? '';
  let body: unknown;
  if (![204, 205, 304].includes(response.status)) {
    const responseParseAs = response.ok ? parseAs : undefined;
    if (responseParseAs === 'blob') body = await response.blob();
    else if (responseParseAs === 'stream') body = response.body;
    else {
      const text = await response.text();
      if (responseParseAs === 'text' || contentType.includes('ndjson')) body = text;
      else {
        try {
          body = text ? JSON.parse(text) : undefined;
        } catch {
          body = text;
        }
      }
    }
  }
  if (!response.ok) return { error: body as APIErrorBody, response };
  return { data: body as T, response };
}
