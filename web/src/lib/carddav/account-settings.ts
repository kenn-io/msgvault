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
  values: CardDAVAccountValues;
  password: string;
  request: import('../api/generated/models').CardDAVAccountRequest;
  error?: string;
  busy?: boolean;
}
