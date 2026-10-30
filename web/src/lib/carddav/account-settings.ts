import type { CardDAVStatusResponse } from '../api/generated/models';

export interface CardDAVAccountValues {
  provider: string;
  oauthApp: string;
  baseURL: string;
  username: string;
  passwordConfigured: boolean;
  enabled: boolean;
  schedule: string;
}

export const cardDAVRepairCopy: Record<NonNullable<CardDAVStatusResponse['repair_reason']>, string> = {
  account_missing: 'CardDAV account discovery is missing. Test and save the account again.',
  credential_missing: 'No CardDAV credential is stored. Connect the account in CardDAV settings and save it.',
  credential_mismatch: 'The stored credential belongs to different CardDAV account settings. Connect the account again and save.',
  credential_unavailable: 'The stored credential cannot be read right now. Check that its credential store is available, then try again. For password-based accounts, reconnect the provider account or save its password again in CardDAV settings.',
  google_authorization_required: 'Check the selected OAuth app\'s client settings. Connect Google in CardDAV account settings, or run msgvault carddav authorize-google with your account email and OAuth app, then try again.',
  microsoft_authorization_required: 'Run msgvault carddav authorize-microsoft with your account email, then try again.',
  runtime_unavailable:
    'CardDAV is configured but unavailable in this daemon session. Test and save the account again, or restart the daemon.',
};
