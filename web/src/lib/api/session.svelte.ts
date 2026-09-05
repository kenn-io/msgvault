import {
  getSession as generatedGetSession,
  loginSession as generatedLoginSession,
  logoutSession as generatedLogoutSession,
} from './generated/session/session';
import type { SessionStatus as GeneratedSessionStatus } from './generated/models';
import { createSessionAwareAPIClient, type APIClient } from './client';
type SessionStatus = GeneratedSessionStatus;
function errorMessage(error: unknown, fallback: string): string {
  if (typeof error === 'object' && error !== null && 'message' in error) {
    const message = (
      error as {
        message?: unknown;
      }
    ).message;
    if (typeof message === 'string' && message) return message;
  }
  return fallback;
}
export class SessionController {
  status = $state<SessionStatus>();
  loading = $state(false);
  error = $state<string>();
  readonly client: APIClient;
  constructor(fetchFn: typeof fetch = fetch) {
    this.client = createSessionAwareAPIClient(
      fetchFn,
      () => this.csrfToken,
      () => this.requireLogin(),
    );
  }
  get authMode(): SessionStatus['auth_mode'] | undefined {
    return this.status?.auth_mode;
  }
  get csrfToken(): string | undefined {
    return this.status?.csrf_token;
  }
  async bootstrap(): Promise<void> {
    this.loading = true;
    this.error = undefined;
    try {
      const { data, error } = await generatedGetSession(this.client);
      if (data) {
        this.status = data;
      } else {
        this.error = errorMessage(error, 'Could not determine browser session status');
      }
    } catch (error) {
      this.error = errorMessage(error, 'Could not determine browser session status');
    } finally {
      this.loading = false;
    }
  }
  async login(apiKey: string): Promise<boolean> {
    this.loading = true;
    this.error = undefined;
    try {
      const { data, error } = await generatedLoginSession({ api_key: apiKey }, this.client);
      if (!data) {
        this.requireLogin();
        this.error = errorMessage(error, 'Login failed');
        return false;
      }
      this.status = data;
      return true;
    } catch (error) {
      this.requireLogin();
      this.error = errorMessage(error, 'Login failed');
      return false;
    } finally {
      this.loading = false;
    }
  }
  async logout(): Promise<boolean> {
    this.loading = true;
    this.error = undefined;
    try {
      const { response, error } = await generatedLogoutSession(this.client);
      if (!response.ok) {
        this.error = errorMessage(error, 'Logout failed');
        return false;
      }
      this.requireLogin();
      return true;
    } catch (error) {
      this.error = errorMessage(error, 'Logout failed');
      return false;
    } finally {
      this.loading = false;
    }
  }
  private requireLogin(): void {
    this.status = {
      auth_mode: 'required',
      https: this.status?.https ?? false,
      plain_http_warning: this.status?.plain_http_warning ?? true,
    };
  }
}
export function createSessionController(fetchFn: typeof fetch = fetch): SessionController {
  return new SessionController(fetchFn);
}
