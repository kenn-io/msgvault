---
title: OAuth Setup
description: Create OAuth credentials for Gmail (Google Cloud) or Microsoft 365 (Azure AD) and authorize msgvault.
---

## Google (Gmail and Calendar)

To archive Gmail, create a Google OAuth client for your copy of msgvault and
authorize each account you want to sync. OAuth lets Google grant access without
sharing your Google password.

Before you start, make sure you have [msgvault installed](../setup.md) and a
Google account with Gmail. Use a browser on the computer where you run
`add-account`. If you are setting up a server without a browser, see
[Headless Server Setup](#headless-server-setup).

You work with three pieces during setup:

| Piece | What it does | Where it lives |
| --- | --- | --- |
| Google Cloud project | Holds enabled APIs and your OAuth app settings | Google Cloud Console |
| OAuth client JSON | Identifies your copy of msgvault to Google | A file you download and point msgvault at |
| Account token | Records one account's authorization | `~/.msgvault/tokens/`, created by msgvault |

The client JSON alone does not grant access to a mailbox. Signing in through the
browser in Step 6 creates the token for that account, and you can reuse the same
client across your personal Gmail accounts.

Google updates its console layout periodically. The steps below use **Google
Auth Platform**, which older guides call **OAuth consent screen**. Whenever you
open a console link, check the project picker at the top of the page and make
sure you stay in the same project. For current button names, check Google's
[Gmail quickstart](https://developers.google.com/workspace/gmail/api/quickstart/go)
and [consent setup guide](https://developers.google.com/workspace/guides/configure-oauth-consent).

The examples on this page use file storage. If you prefer to keep credentials in
a secret store, use
[command-backed Google credentials and tokens](/docs/configuration/#command-backed-google-credentials-and-tokens)
instead. That reference covers configuration, migration, and headless export. The
file inspection and removal steps below apply to file storage.

### Step 1: Create a Google Cloud Project

1. Open [Google Cloud Console](https://console.cloud.google.com/) and sign in
   with the account that owns the project.
2. If this is your first visit, complete Google's country and terms prompts.
3. Open the project picker at the top and click **New project**. You can also
   use an existing project if you can manage its APIs and OAuth clients.
4. Enter `msgvault` as the project name. For personal Gmail, leave the
   organization set to **No organization** if that option appears.
5. Click **Create**, wait for the notification to finish, and click **Select
   project**.
6. Check the project picker to confirm it now shows `msgvault`.

This project gives you a place for your API and authentication settings. Setting up Gmail
does not require creating a VM or enabling unrelated paid cloud services.

### Step 2: Enable Google APIs

1. Open the [Gmail API library page](https://console.cloud.google.com/apis/library/gmail.googleapis.com).
2. Confirm the selected project, then click **Enable**. If the button says
   **Manage**, the Gmail API is already enabled for that project.
3. If you also want to archive Calendar, open the
   [Google Calendar API library page](https://console.cloud.google.com/apis/library/calendar-json.googleapis.com)
   in the same project and click **Enable**.
4. If you want Google Contacts over CardDAV, open the API library, search for
   "CardDAV", open **Google Contacts CardDAV API**, and click **Enable**.
   The Gmail and Calendar APIs do not cover contacts, and without this API,
   saving the CardDAV account fails during discovery because Google rejects
   every request with `SERVICE_DISABLED`.

**Checkpoint.** **Gmail API** appears under **APIs & Services > Enabled APIs &
services**.

### Step 3: Configure OAuth Consent Screen

1. Open [Google Auth Platform > Branding](https://console.cloud.google.com/auth/branding).
   If Google says the platform is not configured, click **Get started**.
2. Under **App Information**, enter `msgvault` for **App name**, select your
   email for **User support email**, and click **Next**.
3. Under **Audience**, choose **External** for personal Gmail. Choose **Internal**
   only when the project belongs to a Google Workspace or Cloud Identity
   organization and every account you authorize belongs to that organization.
   Click **Next**.
4. Under **Contact Information**, enter an email address you monitor and click
   **Next**.
5. Review Google's user data policy, select the agreement checkbox if you agree,
   and click **Continue** followed by **Create**.

If the app is already configured, you do not need to create it again. Its
settings are split across **Branding**, **Audience**, and **Data Access**.

#### Add test users

For an External app, open
[Audience](https://console.cloud.google.com/auth/audience) and check **Publishing
status**. While the status is **Testing**:

1. Scroll to **Test users** and click **Add users**.
2. Enter the exact Gmail address you pass to `msgvault add-account`.
3. Click **Save**, and add every other account you plan to authorize.

Google does not add the project owner's address as a test user automatically. An
account can own the project and still fail to authorize until you add it here.

#### Choose Testing or In production

Start in **Testing** to verify your setup. Under Google's
[Audience rules](https://support.google.com/cloud/answer/15549945), Testing
limits the app to 100 listed test users and expires Gmail authorizations,
including refresh tokens, seven days after consent. Automatic token refresh does
not extend that seven-day limit.

For ongoing personal archiving, you can switch an External app to **In
production** under **Audience > Publish app**. Publishing status and
verification are separate, and Google offers a
[personal-use verification exemption](https://support.google.com/cloud/answer/13464323)
for apps used by fewer than 100 users. An unverified personal app still shows
warnings, has a user cap, and must comply with Google's user data policy.
**In production** removes Testing's seven-day expiry, though tokens can still be
revoked or expire for other reasons. See
[Google's token-expiration rules](https://developers.google.com/identity/protocols/oauth2#expiration)
for details.

After you change the status, get a fresh authorization if your current token was
issued in Testing; see [Google setup troubleshooting](#google-setup-troubleshooting).
If your account is managed by an organization or the app is distributed to others,
follow Google's verification requirements and your administrator's policy.

#### Declare the scopes you will use

A scope names a specific permission. Open
[Data Access](https://console.cloud.google.com/auth/scopes) and click **Add or
Remove Scopes**. Under **Manually add scopes**, enter the URLs for your
workflow, one per line, then click **Add to table**, **Update**, and **Save**.

| Workflow | Scopes to declare | msgvault command |
| --- | --- | --- |
| Gmail archiving with later trash-based deletion | `https://www.googleapis.com/auth/gmail.readonly` and `https://www.googleapis.com/auth/gmail.modify` | `msgvault add-account you@gmail.com` |
| Gmail archiving with read access only | `https://www.googleapis.com/auth/gmail.readonly` | `msgvault add-account you@gmail.com --readonly` |
| Optional Calendar archiving | Also add `https://www.googleapis.com/auth/calendar.readonly` | Authorize separately with `msgvault add-calendar you@gmail.com` |

!!! warning "This page does not restrict what gets granted"
    Console scope declarations describe your app for Google's review, while the
    scopes msgvault requests determine the actual grant. Removing `gmail.modify`
    here does not make msgvault read-only. Pass `--readonly` when you add the
    account instead.

!!! note
    By default, msgvault requests `gmail.readonly` and `gmail.modify`. Sync
    reads mail, and `gmail.modify` allows later trash-based deletion. The first
    time you run `delete-staged --permanent`, msgvault prompts for full
    `https://mail.google.com/` access for batch deletion. Read
    [Read-Only Access](#read-only-access) before changing an account that
    already has write access.

### Step 4: Create OAuth Client Credentials

1. Open [Google Auth Platform > Clients](https://console.cloud.google.com/auth/clients).
2. Click **Create client**.
3. For **Application type**, select **Desktop app**. This type supports
   msgvault's browser sign-in and local callback. **TVs and Limited Input
   devices** does not support Gmail scopes.
4. Enter `msgvault` for the client **Name** and click **Create**.
5. In the **OAuth client created** dialog, click **Download JSON** before
   closing the window, and save the file as `client_secret.json`.
6. Check that your browser saved the JSON file, then close the dialog.

<figure data-lightbox style="margin: 1.5rem 0;">
  <img src="/docs/assets/static/google-oauth/client-created.png" alt="Google's Desktop app client creation screen beside the OAuth client created dialog, showing Download JSON and a warning to save the secret before closing." loading="lazy" style="width: 100%; max-width: 705px; display: block;" />
  <figcaption>Download JSON before closing this dialog. This <a href="https://codelabs.developers.google.com/vertexai-gws-agents?hl=en">Google codelab screenshot</a> uses an Internal organization app; choose External for personal Gmail as described above. Project and credential values are redacted. <a href="https://creativecommons.org/licenses/by/4.0/">CC BY 4.0</a>, modified.</figcaption>
</figure>

You need the complete JSON file; an API key, service-account key, or copied
client ID does not work. As Google's
[client-management guide](https://support.google.com/cloud/answer/15549257)
explains, Google shows a new secret only at creation time. If you missed the
download, check your Downloads folder first in case the file is already there.
A masked secret cannot recreate the original file. For a new setup, create another
**Desktop app** client and save its JSON before closing the dialog. Do not
delete a client that an existing account uses.

Terminal authorization uses `http://localhost:8089/callback`, and a **Desktop
app** client needs no manually registered redirect URI. If you already use a
**Web application** client, register that exact URL as an additional authorized
redirect URI and download the updated credentials. The Web UI callback can stay
registered alongside it. For new CLI setups, use the **Desktop app** path above.

!!! warning
    Keep your client JSON and account tokens private. Never commit them to
    version control or include their contents in screenshots or support
    requests.

### Step 5: Configure msgvault

Move `client_secret.json` to a permanent location before configuring its path.
On macOS and Linux, `~/.msgvault/client_secret.json` is a good default. Restrict
the directory and credential file to your user account:

```bash
mkdir -p ~/.msgvault
chmod 700 ~/.msgvault
# Move the downloaded client_secret.json into ~/.msgvault, then:
chmod 600 ~/.msgvault/client_secret.json
```

For a fresh installation, run:

```bash
msgvault setup
```

When prompted for **Path to client_secret.json**, enter the file path, for
example `~/.msgvault/client_secret.json`. If you are setting up a local archive,
answer **No** when asked about configuring a remote NAS server, then check the
saved configuration path printed in the terminal.

If you already have a configuration file, edit only the `client_secrets` setting
in its existing `[oauth]` section and leave your other settings alone. Do not
replace the whole file with the example below. The setup wizard changes only the
settings it sets (such as `oauth.client_secrets`) in place and keeps your other
settings and comments.

Default configuration paths:

- **macOS / Linux.** `~/.msgvault/config.toml`
- **Windows.** `C:\Users\<you>\.msgvault\config.toml`

```toml
[oauth]
client_secrets = "~/.msgvault/client_secret.json"
```

On Windows, use an absolute path with forward slashes or escaped backslashes:

```toml
[oauth]
client_secrets = "C:/Users/you/.msgvault/client_secret.json"
```

Replace `you` with your Windows username. If you use a custom msgvault
directory, see [configuration paths](../configuration.md).

### Step 6: Add Your Account

In the commands below, replace `you@gmail.com` with the Gmail account you want
to archive. If your app is in Testing, use the same address you added under
**Test users**. Choose the access level you want:

```bash
# Default: read mail and allow later trash-based deletion
msgvault add-account you@gmail.com

# Alternatively, for a new account that should only be read:
msgvault add-account you@gmail.com --readonly
```

1. In your browser, select the Google account that matches the address in the
   command.
2. When Google shows an unverified app warning, verify that the app and project
   are yours. This warning is expected for your own unverified OAuth app.
   Depending on the screen Google shows, click **Continue** or **Advanced > Go
   to msgvault (unsafe)** to proceed. If you see an administrator's **app
   blocked** message instead, that is a separate issue; see
   [troubleshooting](#google-setup-troubleshooting).
3. Review the requested Gmail permissions, grant them, and click **Continue**.
4. Return to your terminal and wait for `Account you@gmail.com authorized successfully!`
   (or `Account you@gmail.com is already authorized.` when reusing a token). The
   browser shows its success page before msgvault finishes verifying and saving
   the token.

<figure data-lightbox style="margin: 1.5rem 0;">
  <img src="/docs/assets/static/google-oauth/unverified-app.png" alt="Google hasn't verified this app warning with Continue and Back to safety buttons." loading="lazy" style="width: 100%; max-width: 500px; display: block;" />
  <figcaption>One version of Google's unverified-app warning. Proceed only for your own app. Screenshot from <a href="https://developers.google.com/health/codelabs/make-your-first-api-call">Google's OAuth codelab</a>, <a href="https://creativecommons.org/licenses/by/4.0/">CC BY 4.0</a>, unchanged.</figcaption>
</figure>

msgvault stores tokens locally under `~/.msgvault/tokens/`. Once the terminal
confirms authorization, run your first full sync:

```bash
msgvault sync-full you@gmail.com
```

Enabling the Calendar API and declaring its scope do not authorize Calendar
access. To add and sync Calendar, follow [Calendar setup](../usage/calendar.md)
and run `msgvault add-calendar` separately.

#### Read-Only Access

By default, `add-account` requests read and modify access. To request read
access only, pass `--readonly`:

```bash
msgvault add-account you@gmail.com --readonly
```

With a read-only grant, you can sync, search, use the TUI, and read or preview
tags. Deletion and tag edits require write access.

If you run `--readonly` on an account that is already read-only, msgvault does
nothing and reuses the existing token. If you run a plain `add-account` on a
read-only account, msgvault warns you before requesting write access again.

**Access already granted cannot be narrowed.** Re-authorizing with `--readonly`
does not revoke what Google has on record, and an earlier refresh token keeps
working with its original write scopes. As a result, an account that once had
write access still has it regardless of which token msgvault holds. Because of
this, `add-account --readonly` refuses that account rather than appearing to
narrow its access, and `--force` is refused as well.

To switch an existing account to read-only, revoke its access and authorize it
again:

1. Revoke msgvault at [myaccount.google.com/permissions](https://myaccount.google.com/permissions).
2. Remove the local token file: `rm ~/.msgvault/tokens/you@gmail.com.json`.
3. Re-add the account as read-only: `msgvault add-account you@gmail.com --readonly`.

Revoking also clears Calendar and Drive access granted through the same OAuth
app. Reauthorize those integrations separately. The current
`add-synctech-sms-drive` command cannot restore a missing Drive scope when a
token already exists. This replaces credentials without touching your archived
mail.

To confirm the token's scopes, inspect the token file:

```bash
python3 -c "import json,os;print(*json.load(open(os.path.expanduser('~/.msgvault/tokens/you@gmail.com.json')))['scopes'],sep='\n')"
```

#### Headless Authorization

Before printing the sign-in URL, msgvault binds `localhost:8089`. If another
application is using that port, close it and retry.

To authorize through a browser over SSH, forward the callback port from the
computer running your browser to the server:

```bash
ssh -L 8089:localhost:8089 user@server
```

In that SSH session, run `msgvault add-account you@gmail.com`. Open the printed
Google authorization URL in your local browser, complete Step 6, and keep the
SSH connection open until the terminal confirms success. The browser's callback
then reaches msgvault on the server.

If you cannot forward the port, open the printed URL in any browser and complete
Step 6. The browser fails to load the `http://localhost:8089/callback?...` page.
Copy that full URL from the address bar and, while `add-account` is still
waiting, request it from a shell on the server:

```bash
curl -s 'http://localhost:8089/callback?PASTE_THE_REST_HERE'
```

You can also authorize locally and copy the token to the server; see
[Headless Server Setup](#headless-server-setup).

### Google setup troubleshooting

| What you see | What to check or do |
| --- | --- |
| `403 access_denied`, or the app is available only to approved testers | In the same project's **Audience > Test users**, add the exact account you selected in the browser, save, and retry. |
| `org_internal` | The app is Internal but the selected account is outside its organization. Personal Gmail needs an External app; an organization app needs an account in that organization. |
| Gmail API disabled or never used in this project | Enable **Gmail API** in the project that owns the OAuth client. Wait for Google's change to take effect, then retry. |
| `invalid_grant` after about a week | Check **Audience > Publishing status**. Testing expires Gmail refresh tokens after seven days. Reauthorize; for ongoing personal use, consider In production under the rules in Step 3. |
| `redirect_uri_mismatch` | For a new setup, download a **Desktop app** client. For a Web application client, register exactly `http://localhost:8089/callback`. |
| `OAuth client secrets not configured`, `OAuth client secrets file not accessible`, `read client secrets`, `parse client secrets`, or `file not found` from `msgvault setup` | Check `[oauth] client_secrets` points to the actual downloaded JSON, not a client ID or another credential type. Use the Windows path spelling in Step 5. |
| `token mismatch: expected ... but authorized as ...` | Run the command for the intended address and select that same Google account in the browser. |
| `authorized token missing required OAuth scopes` | Retry and grant every permission msgvault requested. Console scope declarations alone do not grant access. |
| An administrator blocks the app, or Google says the app is blocked | Ask your Workspace administrator about OAuth restrictions. Adding test users does not bypass organization policy; see [Google Workspace Accounts](#google-workspace-accounts). |

To replace an expired token or get a fresh grant after leaving Testing, run:

```bash
msgvault add-account you@gmail.com --force
```

If the account already uses a read-only grant, keep it read-only:

```bash
msgvault add-account you@gmail.com --force --readonly
```

`--force` deletes the local token and starts browser authorization again. It
does not revoke Google's existing grant or narrow write access; use the
[revoke-and-re-add procedure](#read-only-access) for that. Reauthorize Calendar
or other integrations separately if their scopes need restoring, and keep the
appropriate `--oauth-app` binding for any named OAuth app.

### Multiple Accounts

One `client_secret.json` works for all of your personal Gmail accounts. Each
`add-account` run creates its own token file:

```bash
msgvault add-account personal@gmail.com
msgvault add-account other@gmail.com

msgvault sync   # syncs all accounts
```

!!! tip
    While your app is in Testing, list every Gmail address you want to sync
    under **Google Auth Platform > Audience > Test users**
    ([Add test users](#add-test-users)). A missing test user is the most common
    reason a second account fails to authorize.

#### Google Workspace Accounts

Many Google Workspace organizations restrict OAuth to apps created inside their
own org. If you see an "access denied" or "app blocked" error when authorizing a
Workspace account with your personal OAuth app, the org likely requires its own
app.

Create a separate Google Cloud project inside the Workspace org (Steps 1-4
above), then register it as a named OAuth app in `config.toml`:

```toml
[oauth]
client_secrets = "/path/to/personal_secret.json"    # default for personal Gmail

[oauth.apps.acme]
client_secrets = "/path/to/acme_workspace_secret.json"
```

Specify the app name when you add Workspace accounts:

```bash
msgvault add-account you@acme.com --oauth-app acme
msgvault add-account personal@gmail.com              # uses default
```

msgvault stores the binding per account, so `sync`, `verify`, and `serve` pick
the right credentials automatically. You only need `--oauth-app` when adding an
account for the first time or switching it to another app.

<figure data-lightbox style="margin: 1.5rem 0; text-align: center;">
  <img src="/docs/assets/generated/concepts/oauth-multi-account-concept.png" alt="Two OAuth apps and the token files they create. A default app (config block [oauth]) authorizes personal Gmail accounts personal@gmail.com and other@gmail.com; a named app ([oauth.apps.acme]) authorizes the Workspace account you@acme.com. Each add-account run writes its own token file under ~/.msgvault/tokens/, color-matched to its account." loading="lazy" style="width: 100%; display: block;" />
</figure>

To switch an existing account to a different OAuth app:

```bash
msgvault add-account you@acme.com --oauth-app acme   # re-authorizes with new app
```

To move an account back to the default app:

```bash
msgvault add-account you@acme.com --oauth-app ""      # clears the binding
```

#### Google Workspace Service Accounts

Workspace administrators can skip per-user browser OAuth by setting up a Google
service account with domain-wide delegation.

1. Create a Google Cloud service account in the Workspace-owned project.
2. Create and download a JSON key for that service account.
3. In the Google Admin Console, authorize the service account client ID for:
   - `https://www.googleapis.com/auth/gmail.readonly`
   - `https://www.googleapis.com/auth/gmail.modify`
   - `https://www.googleapis.com/auth/calendar.readonly` if you sync Google Calendar
   - `https://mail.google.com/` if you run `delete-staged --permanent`
4. Restrict the key file to owner-only permissions, for example `chmod 600 /path/to/workspace-service-account.json`.

Both `gmail.readonly` and `gmail.modify` are required. Because the Gmail
service-account paths request that pair when minting a delegated token, a delegation grant
limited to `gmail.readonly` fails the token exchange, and `add-account`, `sync`,
`serve`, and `verify` all stop working for that account.

!!! note
    `--readonly` does not apply to service accounts and returns an error if you
    pass it. The delegation grant in the Admin Console sets the scope rather
    than msgvault flags. Narrowing what the Gmail service-account paths request,
    so a read-only delegation grant becomes usable, is a possible future change.

Configure the key as your default Google credential:

```toml
[oauth]
service_account_key = "/path/to/workspace-service-account.json"
```

Or bind it to a named app:

```toml
[oauth.apps.acme]
service_account_key = "/path/to/acme-service-account.json"
```

Add accounts as usual:

```bash
msgvault add-account you@acme.com
msgvault add-account teammate@acme.com --oauth-app acme
```

Service account mode validates the delegated Gmail profile and registers the
source without creating per-user token files. Do not combine service-account
accounts with `--headless`, `--force`, or `--readonly`. Delegated tokens are
minted on demand.

To sync Google Calendar with a service account, enable the Google Calendar API
and authorize the `calendar.readonly` scope listed above. Then configure a
`[[gcal]]` source and run `msgvault sync-calendar user@domain.com --oauth-app acme`
(or let `msgvault serve` run the schedule). No browser token is created.

### Headless Server Setup

On a headless server (such as SSH, a VPS, or Docker), there is no local browser
for OAuth. Because Google's device code flow does not support Gmail scopes, you
need to authorize on a machine with a browser and copy the token to your server.

Run `--headless` to print the setup instructions:

```bash
msgvault add-account you@gmail.com --headless
```

This prints:

```
=== Headless Server Setup ===

Google's OAuth device flow does not support Gmail scopes, so --headless
cannot directly authorize. Instead, authorize on a machine with a browser
and copy the token to your server.

Step 1: On a machine with a browser, run:

    msgvault add-account you@gmail.com

Step 2: Copy the token file to your headless server:

    ssh user@server mkdir -p '/home/you/.msgvault/tokens'
    scp '/home/you/.msgvault/tokens/you@gmail.com.json' user@server:'/home/you/.msgvault/tokens/you@gmail.com.json'

Step 3: On the headless server, register the account:

    msgvault add-account you@gmail.com

The token will be detected and the account registered. No browser needed.
All msgvault commands (sync, tui, etc.) will work normally.
```

!!! note "Read-only on a headless server"
    msgvault echoes `--readonly` into the commands above, so running
    `msgvault add-account you@gmail.com --headless --readonly` shows the
    read-only command to run on the machine with a browser.

    The revoke-and-re-add procedure works the same way on a headless host. Step
    3 prints an authorization URL you can open in any browser, then waits for
    the callback as described above.

#### Step-by-Step

1. **Authorize on your local machine.** On a computer with a browser, install
   msgvault, run the following command, and complete the OAuth flow in your
   browser:
   ```bash
   msgvault add-account you@gmail.com
   ```

2. **Copy the token to your headless server.**
   ```bash
   ssh user@server mkdir -p ~/.msgvault/tokens
   scp ~/.msgvault/tokens/you@gmail.com.json user@server:~/.msgvault/tokens/
   ```

3. **Register the account on the headless server.**
   ```bash
   msgvault add-account you@gmail.com
   ```
   msgvault detects the existing token and registers the account, printing:
   ```
   Account you@gmail.com is already authorized.
   Next step:
       msgvault sync-full you@gmail.com
   ```

4. **Sync your email.**
   ```bash
   msgvault sync-full you@gmail.com
   ```

The token file stores a refresh token that msgvault uses to renew short-lived
access tokens. If that refresh token expires or access is revoked, you need to
copy a replacement token. In an External app still in Testing, Gmail refresh
tokens expire after seven days; see
[Choose Testing or In production](#choose-testing-or-in-production).

!!! note
    Both machines must use the same OAuth client credentials. The token is tied
    to the OAuth client that created it. If the account uses a named OAuth app
    (`--oauth-app`), configure the same `[oauth.apps.<name>]` section on both
    machines.

## Microsoft 365 (Outlook / Hotmail)

Use `add-o365` to connect Outlook.com, Hotmail, Live.com, and Microsoft 365
organizational accounts with OAuth2 and XOAUTH2 IMAP authentication. No app
password is needed.

### Prerequisites: Azure AD App Registration

Before you use `add-o365`, register an application in Microsoft Entra (Azure
AD).

An Outlook.com-only account cannot register an app on its own. Register the app
in a tenant you can use, such as a work tenant or a free Azure/Entra directory,
and allow personal Microsoft accounts under supported account types.

1. Open [Azure Portal](https://portal.azure.com/) and go to **Microsoft Entra ID > App registrations > New registration**.
2. Fill in the registration fields:
   - **Name.** `msgvault`
   - **Supported account types.** "Accounts in any organizational directory and personal Microsoft accounts"
   - **Redirect URI.** Platform = **Mobile and desktop applications**, URI = your `redirect_uri` from `config.toml` (default: `http://localhost:8089/callback/microsoft`)
3. Click **Register**.
4. Under **API permissions**, click **Add a permission > Microsoft Graph > Delegated permissions**, then add `IMAP.AccessAsUser.All`.
5. Under **Authentication**, enable **Allow public client flows** (required for PKCE and `--headless`).
6. If you use a custom `redirect_uri` in `config.toml`, make sure the Redirect URI in the app registration matches it exactly, including scheme, host, port, and path. For `https://localhost/` on a privileged port (for example, 443), register that exact URI.
7. Copy the **Application (client) ID** from the app's Overview page.

### Configure msgvault

Add a `[microsoft]` section to your `config.toml`:

```toml
[microsoft]
client_id = "your-azure-app-client-id"
```

To restrict authorization to one organization, add `tenant_id`:

```toml
[microsoft]
client_id = "your-azure-app-client-id"
tenant_id = "your-org-tenant-id"
```

If you omit `tenant_id` (or set it to `"common"`), both personal Microsoft
accounts and organizational accounts can authorize.

### Mailboxes in multiple organizations

To sync mailboxes across more than one organization, register an app in each
organization and configure them separately:

```toml
[microsoft]
client_id = "org-a-client-id"
tenant_id = "org-a-tenant-id"

[microsoft.apps.orgb]
client_id = "org-b-client-id"
tenant_id = "org-b-tenant-id"
```

Restart the daemon after changing Microsoft app settings so scheduled syncs use
the updated configuration.

```bash
msgvault add-o365 user@org-a.example
msgvault add-o365 user@org-b.example --oauth-app orgb
msgvault add-o365 user@org-b.example --graph --oauth-app orgb --headless
```

When a named app omits `tenant_id` or `redirect_uri`, it defaults to `common`
and `http://localhost:8089/callback/microsoft`; set those fields on the named
app if it needs different values. To recover a lost token, re-run `add-o365`
without `--oauth-app`, and msgvault remembers the source's app. Pass another app
name to switch apps, or pass `--oauth-app ""` to switch back to `[microsoft]`.

msgvault stores named app tokens under `tokens/microsoft-apps/` in directories
keyed by client ID. Switching apps or changing a named app's `client_id` leaves
the previous token on disk until you remove the account or delete that token.
Teams, contacts, and the Web add-source template use `[microsoft]`.

If you change the default `[microsoft].client_id`, re-run `add-o365` for every
mailbox that uses the default app.

### Add Your Account

```bash
msgvault add-o365 you@outlook.com
```

This opens your browser for Microsoft OAuth consent. Once you authorize,
msgvault validates that the token matches the email address you specified,
detects the IMAP host for your account type, and configures XOAUTH2
authentication automatically.

IMAP checks Microsoft's `email` claim when present, and falls back to
`preferred_username` otherwise. If that username differs from your mailbox
address, pass your sign-in name with `--sign-in`, for example
`msgvault add-o365 john@company.com --sign-in jdoe@company.onmicrosoft.com`.
That flag also sets the browser login hint, and you need to pass it again when
re-authorizing (msgvault previously accepted a differing username with a
warning). `--sign-in` permits that username only when the `email` claim is
absent. Failed checks preserve your existing credentials.

Graph mail (`--graph`), Teams, and Microsoft contacts check your Microsoft
profile when token identity fields differ or are absent. They accept your
mailbox or an SMTP alias listed in your profile and do not need `--sign-in`.

On a machine without a browser, such as a server or container, pass `--headless`:

```bash
msgvault add-o365 you@outlook.com --headless
```

msgvault prints a Microsoft URL and a code. Open the URL on any device and enter
the code. `add-teams` accepts the same flag.

Personal accounts (hotmail.com, outlook.com, live.com, msn.com) connect to
`outlook.office.com`, and organizational accounts (company Microsoft 365) connect
to `outlook.office365.com`. msgvault detects the host automatically.

To restrict authorization to a specific tenant when adding the account:

```bash
msgvault add-o365 you@example.com --tenant your-org-tenant-id
```

### Microsoft Teams Graph Sync

Teams ingestion uses the same `[microsoft] client_id` and redirect URI, requests
Microsoft Graph delegated scopes, and saves a separate token file under
`tokens/teams_<email>.json`. The `microsoft_<email>.json` token created by
`add-o365` is for IMAP and is not reused for Teams.

To archive Teams chats and channels, add these **Microsoft Graph** delegated
permissions to your app registration:

- `Chat.Read`
- `ChannelMessage.Read.All`
- `Team.ReadBasic.All`
- `Channel.ReadBasic.All`
- `User.Read`
- `User.ReadBasic.All`
- `TeamMember.Read.All`
- `ChannelMember.Read.All`

Then authorize and sync Teams:

```bash
msgvault add-teams you@example.com
msgvault sync-teams you@example.com
```

Some organizations require administrator consent before you can use delegated
channel message permissions. See [Microsoft Teams](/docs/usage/teams/) for the
full Teams workflow.

### Microsoft Graph Mail Sync

If IMAP is disabled for a mailbox, `add-o365 --graph` syncs it through the
Microsoft Graph mail API using your selected Microsoft app and redirect URI. Add
the **Microsoft Graph** delegated permission `Mail.Read` to your app
registration, then authorize and sync:

```bash
msgvault add-o365 you@example.com --graph
msgvault sync you@example.com
```

msgvault saves the token under `tokens/msmail_<email>.json` and registers the
account with type `msmail`. Each mail folder becomes a label. The first sync
downloads every folder, and later syncs fetch only changes, including folder
moves and deletes. The daemon schedules the account like any other.

To delete messages at the source with `delete-staged`, also add the delegated
permission `Mail.ReadWrite`. Sync does not use this permission; the first
`delete-staged` run for the account prompts you to upgrade the token. See
[Deleting Email](/docs/usage/deletion/).

To sync Microsoft contacts, also add the delegated permission
`Contacts.ReadWrite`. See [CardDAV Contacts](/docs/usage/people-carddav/#microsoft-contacts).

A Graph account is stored as a separate account. If you also sync the same
mailbox over IMAP, your vault holds two copies. Run `msgvault dedup --collection`
to hide the extra copies, and pass `--undo` to reverse it.

### Sync Your Email

After adding the account, sync it the same way as any other account:

```bash
msgvault sync-full you@outlook.com
```

### Headless Servers

To sign in from SSH, a server, or a container without a local browser, run:

```bash
msgvault add-o365 you@outlook.com --headless
```

For Graph mail, add `--graph`. For Teams, run
`msgvault add-teams you@example.com --headless`. Open the printed Microsoft URL
on another device, enter the code, and finish signing in; msgvault saves the
token on the server.

You can also authorize on another machine and copy the token. For IMAP mail:

1. On your local machine, run `msgvault add-o365 you@outlook.com` and complete
   the browser sign-in.
2. Copy the token to the server:
   ```bash
   ssh user@server mkdir -p ~/.msgvault/tokens
   scp ~/.msgvault/tokens/microsoft_you@outlook.com.json \
       user@server:~/.msgvault/tokens/
   ```
3. On the server, run `msgvault add-o365 you@outlook.com` again. msgvault
   detects the existing token and registers the account without a browser.

Both machines must use the same `client_id` in their `[microsoft]` config.
