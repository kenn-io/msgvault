import { expect, it } from 'vitest';
import type { CardDAVConnectionResponse } from '../api/generated/models';
import { contactFailure } from './contacts';
import { syncCommand } from './labels';

it('preserves a named Google OAuth app and quotes apostrophes in repair commands', () => {
  const connection: CardDAVConnectionResponse = { connection: 'work', orphaned: false, oauth_app: "Team's app", status: { configured: true, available: true, credential_configured: true, enabled: false, scheduled: false, schedule: '', repair_reason: 'google_authorization_required', account: { base_url: 'https://contacts.example.com', username: "o'brien@example.com" } } };
  expect(contactFailure(connection)?.command).toBe("msgvault carddav authorize-google 'o'\\''brien@example.com' --oauth-app 'Team'\\''s app'");
  connection.status.repair_reason = 'microsoft_authorization_required';
  expect(contactFailure(connection)?.command).toBe("msgvault carddav authorize-microsoft 'o'\\''brien@example.com'");
  expect(syncCommand({ identifier: "o'brien@example.com", sync_unavailable_reason: 'sync_not_configured' })).toBe("msgvault sync 'o'\\''brien@example.com'");
});
