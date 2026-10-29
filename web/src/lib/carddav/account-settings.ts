import type { CardDAVAccountRequest } from '../api/generated/models';

export interface CardDAVAccountValues {
  provider: string;
  oauthApp: string;
  baseURL: string;
  username: string;
  passwordConfigured: boolean;
  enabled: boolean;
  schedule: string;
}

export interface AccountDraft {
  baseline: CardDAVAccountValues;
  values: CardDAVAccountValues;
  password: string;
  testedTuple?: string;
  error?: string;
}

export function accountRequest(values: CardDAVAccountValues, password: string, connection: string | undefined): CardDAVAccountRequest {
  return {
    connection, base_url: values.provider === 'google' ? 'https://www.googleapis.com/.well-known/carddav'
      : values.provider === 'microsoft' ? 'https://graph.microsoft.com/v1.0' : values.baseURL,
    username: values.username, enabled: values.enabled, schedule: values.schedule,
    ...(values.provider === 'google' ? { provider: 'google', oauth_app: values.oauthApp }
      : values.provider === 'microsoft' ? { provider: 'microsoft' }
        : password ? { password } : {}),
  };
}
