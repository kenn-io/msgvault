import createClient from 'openapi-fetch';

import type { paths } from './generated/schema';

class SameOriginRequest extends Request {
  constructor(input: RequestInfo | URL, init?: RequestInit) {
    const requestInput =
      typeof input === 'string' && input.startsWith('/') && typeof document !== 'undefined'
        ? new URL(input, document.baseURI)
        : input;

    super(requestInput, init);
  }
}

export function createAPIClient(fetchFn: typeof fetch = fetch) {
  return createSessionAwareAPIClient(fetchFn);
}

type CSRFTokenProvider = () => string | undefined;
type UnauthorizedHandler = () => void;

export function createSessionAwareAPIClient(
  fetchFn: typeof fetch = fetch,
  csrfToken: CSRFTokenProvider = () => undefined,
  onUnauthorized: UnauthorizedHandler = () => undefined
) {
  const sessionFetch: typeof fetch = async (input, init) => {
    const original = input instanceof Request ? input : new SameOriginRequest(input, init);
    let request = original;
    const method = request.method.toUpperCase();
    const unsafe = !['GET', 'HEAD', 'OPTIONS', 'TRACE'].includes(method);
    const sameOrigin =
      typeof window === 'undefined' || new URL(request.url).origin === window.location.origin;
    const token = csrfToken();
    if (unsafe && sameOrigin && token) {
      const headers = new Headers(request.headers);
      headers.set('X-CSRF-Token', token);
      request = new Request(request, { headers });
    }

    const response = await fetchFn(request);
    if (response.status === 401) onUnauthorized();
    return response;
  };

  return createClient<paths>({
    baseUrl: '',
    fetch: sessionFetch,
    Request: SameOriginRequest
  });
}

export type APIClient = ReturnType<typeof createSessionAwareAPIClient>;
