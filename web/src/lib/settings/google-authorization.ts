import { beginGoogleCardDAVAuthorization, completeGoogleCardDAVAuthorization } from '../api/generated/api/api';
import type { APIClient } from '../api/client';

const channelName = 'msgvault-carddav-oauth';

function responseMessage(error: unknown, fallback: string): string {
  return typeof error === 'object' && error !== null && 'message' in error && typeof error.message === 'string'
    ? error.message : fallback;
}

// A same-origin channel also works when the identity provider severs the
// popup's opener through Cross-Origin-Opener-Policy.
export async function authorizeGoogleContacts(client: APIClient, email: string, oauthApp: string, signal: AbortSignal): Promise<void> {
  const popup = window.open('about:blank', '_blank', 'popup,width=540,height=720');
  if (!popup) throw new Error('Allow pop-up windows for msgvault, then connect Google again.');
  const channel = new BroadcastChannel(channelName);
  let timer: ReturnType<typeof setTimeout> | undefined;
  let abort: (() => void) | undefined;
  try {
    const { data, error } = await beginGoogleCardDAVAuthorization({ email, oauth_app: oauthApp, redirect_uri: `${window.location.origin}/` }, { ...client, signal });
    if (!data) throw new Error(responseMessage(error, 'Unable to start Google sign-in.'));
    const code = await new Promise<string>((resolve, reject) => {
      abort = () => reject(new Error('Google sign-in canceled.'));
      if (signal.aborted) { abort(); return; }
      signal.addEventListener('abort', abort, { once: true });
      timer = setTimeout(() => reject(new Error('Google sign-in expired. Connect again.')), 10 * 60 * 1000);
      channel.onmessage = (event: MessageEvent<unknown>) => {
        const callback = event.data;
        if (typeof callback !== 'object' || callback === null || !('state' in callback) || callback.state !== data.state) return;
        if ('error' in callback && callback.error) { reject(new Error('Google sign-in was declined. Connect again when ready.')); return; }
        if ('code' in callback && typeof callback.code === 'string' && callback.code) resolve(callback.code);
      };
      popup.location.href = data.url;
    });
    const result = await completeGoogleCardDAVAuthorization({ state: data.state, code }, { ...client, signal });
    if (!result.data) throw new Error(responseMessage(result.error, 'Unable to finish Google sign-in.'));
  } finally {
    if (timer !== undefined) clearTimeout(timer);
    if (abort) signal.removeEventListener('abort', abort);
    channel.close();
    popup.close();
  }
}

// Run before application bootstrap so the callback page neither loads archive
// data nor leaves an authorization code in browser history.
export function receiveGoogleContactsCallback(): boolean {
  const params = new URLSearchParams(window.location.search);
  const state = params.get('state');
  if (!state?.startsWith('msgvault-carddav-')) return false;
  window.history.replaceState(null, '', window.location.pathname);
  const channel = new BroadcastChannel(channelName);
  channel.postMessage({ state, code: params.get('code'), error: params.get('error') });
  channel.close();
  window.close();
  return true;
}
