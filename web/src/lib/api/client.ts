import type { GeneratedRequestOptions } from './runtime';

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

// The daemon tags its own expired/invalid browser-session 401s with this error
// code (humaAuthMiddleware, the session login route, and the daemon shutdown
// route all emit it). A downstream integration such as the task service that
// needs its OWN re-auth surfaces a different code (e.g. "authentication_required"),
// which must not tear down the perfectly valid msgvault session.
const SESSION_UNAUTHORIZED_CODE = 'unauthorized';

// Reads the small {error,message} envelope from a 401 without disturbing the
// body the caller still needs. Returns false on any non-JSON/empty/unreadable
// body so a spurious logout never fires; a genuine session-expiry carries the
// session code and is matched here.
async function isSessionUnauthorized(response: Response): Promise<boolean> {
  try {
    const body = (await response.clone().json()) as { error?: unknown };
    return body?.error === SESSION_UNAUTHORIZED_CODE;
  } catch {
    return false;
  }
}

export function createSessionAwareAPIClient(
  fetchFn: typeof fetch = fetch,
  csrfToken: CSRFTokenProvider = () => undefined,
  onUnauthorized: UnauthorizedHandler = () => undefined,
) {
  const sessionFetch: typeof fetch = async (input, init) => {
    const original = input instanceof Request ? input : new SameOriginRequest(input, init);
    let request = original;
    const method = request.method.toUpperCase();
    const unsafe = !['GET', 'HEAD', 'OPTIONS', 'TRACE'].includes(method);
    const sameOrigin = typeof window === 'undefined' || new URL(request.url).origin === window.location.origin;
    const token = csrfToken();
    if (unsafe && sameOrigin && token) {
      const headers = new Headers(request.headers);
      headers.set('X-CSRF-Token', token);
      request = new Request(request, { headers });
    }

    const response = await fetchFn(request);
    if (response.status === 401 && (await isSessionUnauthorized(response))) onUnauthorized();
    return response;
  };

  return { fetch: sessionFetch } satisfies GeneratedRequestOptions;
}

export type APIClient = ReturnType<typeof createSessionAwareAPIClient>;
