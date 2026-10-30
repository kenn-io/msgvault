import type { CardDAVConnectionResponse } from '../api/generated/models';
import { cardDAVRepairCopy } from '../carddav/account-settings';

type Status = CardDAVConnectionResponse['status'];

/** The run state a contacts connection shows, or undefined when it has never synced. */
export function contactRunState(status: Status): string | undefined {
  if (status.active) return 'running';
  const state = status.latest?.state;
  if (state === 'succeeded' || state === 'partial' || state === 'failed' || state === 'cancelled') return state;
  return undefined;
}

/** Whether Sync now can run: the account is set up and no sync is running. */
export function contactCanSync(status: Status): boolean {
  return status.configured && status.available && status.credential_configured && !status.active;
}

/** The time of the last sync that finished without errors. */
export function contactLastGood(status: Status): string | undefined {
  return status.latest_successful?.finished_at ?? status.latest_successful?.started_at;
}

/** The name a contacts connection goes by: its account, or the connection name. */
export function contactLabel(connection: CardDAVConnectionResponse): string {
  return connection.status.account?.username || (connection.connection === 'default' ? 'Contacts' : connection.connection);
}

/** The repair reason shown before opening Contacts settings. */
export function contactFailure(connection: CardDAVConnectionResponse): string | undefined {
  const { status } = connection;
  if (status.repair_reason) return cardDAVRepairCopy[status.repair_reason];
  if ((status.latest?.state === 'failed' || status.latest?.state === 'partial') && status.latest.error_message) {
    return status.latest.error_message.split('\n')[0]!.trim();
  }
  return undefined;
}
