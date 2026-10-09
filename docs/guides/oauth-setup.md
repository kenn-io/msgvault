---
title: OAuth Setup
description: Create OAuth credentials for Gmail (Google Cloud) or Microsoft 365 (Azure AD) and authorize msgvault.
---

## Google (Gmail and Calendar)

Create a Google OAuth client for your own copy of msgvault, then authorize each
Gmail account you want to archive. OAuth lets Google grant msgvault access
without giving it your Google password.

You need [msgvault installed](../setup.md) and a Google account with Gmail.
Use a browser on the computer where you will run `add-account`; for a server,
see [Headless Server Setup](#headless-server-setup).

There are three pieces to keep straight:

| Piece | What it does | Where it lives |
| --- | --- | --- |
| Google Cloud project | Holds enabled APIs and your OAuth app settings | Google Cloud Console |
| OAuth client JSON | Identifies your copy of msgvault to Google | A file you download and point msgvault at |
| Account token | Records one account's authorization | `~/.msgvault/tokens/`, created by msgvault |

The client JSON alone does not grant access to a mailbox. The browser sign-in
in Step 6 creates that account's token. You can reuse one client for your own
personal Gmail accounts.

Google changes its console layout. The steps below use **Google Auth Platform**;
older tutorials call it **OAuth consent screen**. Check the project picker at
the top of the console whenever you open a link: every step must use the same
project. Google's [Gmail quickstart](https://developers.google.com/workspace/gmail/api/quickstart/go)
and [consent setup guide](https://developers.google.com/workspace/guides/configure-oauth-consent)
are the references for current button names.

### Step 1: Create a Google Cloud Project

1. Open [Google Cloud Console](https://console.cloud.google.com/). Sign in with
   the account that will own the project.
2. On your first visit, complete Google's country and terms prompts.
3. Open the project picker at the top, then click **New project**. An existing
   project also works if you can manage its APIs and OAuth clients.
4. Enter `msgvault` as the project name. For personal Gmail, leave the organization
   as **No organization** if that option is available.
5. Click **Create**, wait for the notification to finish, then **Select project**.
6. Check that the picker now shows `msgvault`.

You are creating a place for API and authentication settings. This Gmail setup
does not require creating a VM or enabling unrelated paid cloud services.

### Step 2: Enable Google APIs

1. Open the [Gmail API library page](https://console.cloud.google.com/apis/library/gmail.googleapis.com).
2. Confirm the selected project, then click **Enable**. If you see **Manage**,
   Gmail is already enabled for that project.
3. For Calendar archiving too, open the
   [Google Calendar API library page](https://console.cloud.google.com/apis/library/calendar-json.googleapis.com)
   in the same project and click **Enable**.
4. For Google Contacts over CardDAV, open the API library, search for
   "CardDAV", open **Google Contacts CardDAV API**, and click **Enable**.
   The Gmail and Calendar APIs do not cover contacts. Without this API,
   saving the CardDAV account fails during discovery because Google rejects
   every request with `SERVICE_DISABLED`.

**Checkpoint:** Gmail API appears under **APIs & Services > Enabled APIs & services**.

### Step 3: Configure OAuth Consent Screen

1. Open [Google Auth Platform > Branding](https://console.cloud.google.com/auth/branding).
   If Google says the platform is not configured, click **Get started**.
2. Under **App Information**, enter `msgvault` for **App name** and select your
   email for **User support email**. Click **Next**.
3. Under **Audience**, choose **External** for personal Gmail. Choose **Internal**
   only when the project belongs to a Google Workspace or Cloud Identity
   organization and every account you will authorize belongs to that organization.
   Click **Next**.
4. Under **Contact Information**, enter an email you monitor. Click **Next**.
5. Review Google's user data policy, select its agreement checkbox if you agree,
   then click **Continue** and **Create**.

If the app is already configured, its settings are split across **Branding**,
**Audience**, and **Data Access**; you do not need to create it again.

#### Add test users

For an External app, open
[Audience](https://console.cloud.google.com/auth/audience) and check **Publishing status**.
While it says **Testing**:

1. Scroll to **Test users** and click **Add users**.
2. Enter the exact Gmail address you will pass to `msgvault add-account`.
3. Click **Save**. Add every additional account you intend to authorize.

The project owner's address is not automatically a test user. A Google account
can own the project but still fail to authorize until you add it here.

#### Choose Testing or In production

Start in **Testing** to check the setup. Google's
[Audience rules](https://support.google.com/cloud/answer/15549945)
limit Testing to 100 listed test users and expire Gmail authorizations, including
refresh tokens, seven days after consent. Automatic token refresh does not extend
that seven-day limit.

For ongoing personal archiving, you can use **Audience > Publish app** to change
an External app to **In production**. Publishing status and verification are
separate. Google provides a
[personal-use verification exemption](https://support.google.com/cloud/answer/13464323)
for apps used by fewer than 100 users. An unverified personal app still shows
warnings and has a user cap; it must comply with Google's user data policy.
**In production** removes Testing's seven-day expiry, but tokens can still be
revoked or expire for other reasons. See
[Google's token-expiration rules](https://developers.google.com/identity/protocols/oauth2#expiration).

After changing the status, get a fresh authorization if your existing token was
issued in Testing; see [Google setup troubleshooting](#google-setup-troubleshooting).
For an organization-managed account or an app distributed to others, follow
Google's verification requirements and your administrator's policy.

#### Declare the scopes you will use

A scope names a permission. Open
[Data Access](https://console.cloud.google.com/auth/scopes), then **Add or Remove Scopes**.
Under **Manually add scopes**, enter the URLs for your intended workflow, one per
line. Click **Add to table**, **Update**, then **Save**.

| Workflow | Scopes to declare | msgvault command |
| --- | --- | --- |
| Gmail archiving with later trash-based deletion | `https://www.googleapis.com/auth/gmail.readonly` and `https://www.googleapis.com/auth/gmail.modify` | `msgvault add-account you@gmail.com` |
| Gmail archiving with read access only | `https://www.googleapis.com/auth/gmail.readonly` | `msgvault add-account you@gmail.com --readonly` |
| Optional Calendar archiving | Also add `https://www.googleapis.com/auth/calendar.readonly` | Authorize separately with `msgvault add-calendar you@gmail.com` |

!!! warning "This page does not restrict what gets granted"
    Console scope declarations describe your app for Google's review. The scopes
    msgvault requests determine the grant. Removing `gmail.modify` here does not
    make msgvault read-only; choose `--readonly` when adding the account.

!!! note
    By default msgvault requests `gmail.readonly` and `gmail.modify`. Sync reads
    mail; `gmail.modify` enables later trash-based deletion. The first
    `delete-staged --permanent` run prompts for full `https://mail.google.com/`
    access for batch deletion. See [Read-Only Access](#read-only-access) before
    changing an account that already has write access.

### Step 4: Create OAuth Client Credentials

1. Open [Google Auth Platform > Clients](https://console.cloud.google.com/auth/clients).
2. Click **Create client**.
3. For **Application type**, choose **Desktop app**. This supports msgvault's
   browser sign-in and local callback. **TVs and Limited Input devices** does not
   support Gmail scopes.
4. Enter `msgvault` for the client **Name**, then click **Create**.
5. In the **OAuth client created** dialog, click **Download JSON** immediately,
   before closing it. Save the downloaded file as `client_secret.json`.
6. Check that your browser downloaded a JSON file, then close the dialog.

<figure data-lightbox style="margin: 1.5rem 0;">
  <img src="/docs/assets/static/google-oauth/client-created.png" alt="Google's Desktop app client creation screen beside the OAuth client created dialog, showing Download JSON and a warning to save the secret before closing." loading="lazy" style="width: 100%; max-width: 705px; display: block;" />
  <figcaption>Download JSON before closing this dialog. This <a href="https://codelabs.developers.google.com/vertexai-gws-agents?hl=en">Google codelab screenshot</a> uses an Internal organization app; choose External for personal Gmail as described above. Project and credential values are redacted. <a href="https://creativecommons.org/licenses/by/4.0/">CC BY 4.0</a>, modified.</figcaption>
</figure>

Download the complete JSON file. An API key, service-account key, or copied
client ID is not a substitute. Google's
[client-management guide](https://support.google.com/cloud/answer/15549257)
explains that a newly created secret is shown only at creation. If you missed
the download, check your Downloads folder first; a masked secret cannot recreate
the original file. For a new setup, create another **Desktop app** client and
save its JSON before closing the dialog. Do not delete a client used by an
existing account.

Terminal authorization uses `http://localhost:8089/callback`. A Desktop app
client needs no manually registered redirect URI. If you already use a
**Web application** client, register that exact URL as an additional authorized
redirect URI and download credentials for the updated configuration. The Web
UI's callback can remain registered alongside it. For new CLI setups, use the
Desktop app path above.

!!! warning
    Keep the client JSON and account tokens private. Never commit them to version
    control or include their contents in screenshots or support requests.

### Step 5: Configure msgvault

Put `client_secret.json` in a permanent location before configuring its path.
On macOS/Linux, you can use `~/.msgvault/client_secret.json`. Restrict the
directory and credential file to your account:

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

At **Path to client_secret.json**, enter the file's path, for example
`~/.msgvault/client_secret.json`. For a local archive, answer **No** to configuring
a remote NAS server. Check the terminal's saved configuration path.

If you already have a configuration, edit only the `client_secrets` setting in
its existing `[oauth]` section. Preserve the other settings; do not replace the
whole file with this example. The setup wizard rewrites TOML and does not
preserve comments.

Default configuration paths:

- **macOS / Linux:** `~/.msgvault/config.toml`
- **Windows:** `C:\Users\<you>\.msgvault\config.toml`

```toml
[oauth]
client_secrets = "~/.msgvault/client_secret.json"
```

On Windows, use an absolute path with forward slashes or escaped backslashes:

```toml
[oauth]
client_secrets = "C:/Users/you/.msgvault/client_secret.json"
```

Replace `you` with your Windows username. See
[configuration paths](../configuration.md) if you use a custom msgvault directory.

### Step 6: Add Your Account

Replace `you@gmail.com` in the commands below with the Gmail account you will
archive. Use the same address you added under **Test users** if the app is in
Testing. Choose one grant:

```bash
# Default: read mail and allow later trash-based deletion
msgvault add-account you@gmail.com

# Alternatively, for a new account that should only be read:
msgvault add-account you@gmail.com --readonly
```

1. In the browser, select the Google account matching the address in the command.
2. An unverified app warning is expected for your own unverified OAuth app.
   Check the app/project is yours. Depending on Google's screen, use **Continue**
   or **Advanced > Go to msgvault (unsafe)** to proceed with your own app.
   An administrator's **app blocked** message is a different problem; see
   [troubleshooting](#google-setup-troubleshooting).
3. Review and grant the requested Gmail permissions, then click **Continue**.
4. Return to the terminal. Wait for `Account you@gmail.com authorized successfully!`
   (or `Account you@gmail.com is already authorized.` when reusing a token). The browser's
   success page appears before msgvault finishes verifying and saving the token.

<figure data-lightbox style="margin: 1.5rem 0;">
  <img src="/docs/assets/static/google-oauth/unverified-app.png" alt="Google hasn't verified this app warning with Continue and Back to safety buttons." loading="lazy" style="width: 100%; max-width: 500px; display: block;" />
  <figcaption>One version of Google's unverified-app warning. Proceed only for your own app. Screenshot from <a href="https://developers.google.com/health/codelabs/make-your-first-api-call">Google's OAuth codelab</a>, <a href="https://creativecommons.org/licenses/by/4.0/">CC BY 4.0</a>, unchanged.</figcaption>
</figure>

Tokens are stored locally under `~/.msgvault/tokens/`. Once the terminal confirms
success, start the first archive:

```bash
msgvault sync-full you@gmail.com
```

For Calendar, enabling the API and declaring its scope do not authorize it.
Follow [Calendar setup](../usage/calendar.md) to run `msgvault add-calendar` and
configure Calendar syncing separately.

#### Read-Only Access

By default `add-account` requests read and modify access. To request read access only:

```bash
msgvault add-account you@gmail.com --readonly
```

Sync, search, and the TUI all work on a read-only grant. Deletion does not.

Running `--readonly` against an account that is already read-only does nothing and reuses the existing token. A plain `add-account` run against one warns before requesting write access again.

**Access already granted cannot be narrowed.** Re-authorizing with `--readonly` does not revoke what Google has on record, and a refresh token issued earlier keeps working with its original write scopes — so an account that once had write access still has it, whatever token msgvault happens to hold. `add-account --readonly` therefore refuses such an account rather than appearing to narrow it. `--force` does not change this and is refused too.

To make an existing account read-only, remove its access and grant it again:

1. Revoke msgvault at [myaccount.google.com/permissions](https://myaccount.google.com/permissions)
2. `rm ~/.msgvault/tokens/you@gmail.com.json`
3. `msgvault add-account you@gmail.com --readonly`

Revoking also clears Calendar and Drive access granted through the same OAuth app. Reauthorize those integrations separately. The current `add-synctech-sms-drive` command cannot restore a missing Drive scope when a token already exists. Your archived mail is untouched: this replaces credentials, not data.

Confirm the result by reading the token's scopes:

```bash
python3 -c "import json,os;print(*json.load(open(os.path.expanduser('~/.msgvault/tokens/you@gmail.com.json')))['scopes'],sep='\n')"
```

#### Headless Authorization

msgvault binds `localhost:8089` before printing the sign-in URL. If that port is
busy, close the application using it and retry.

For browser authorization over SSH, forward the callback port from the computer
running the browser to the server:

```bash
ssh -L 8089:localhost:8089 user@server
```

In that SSH session, run `msgvault add-account you@gmail.com`. Open the printed
Google authorization URL in your local browser and complete Step 6. Keep the
SSH connection open until the terminal confirms success. The browser's callback
then reaches msgvault on the server.

If you cannot forward the port, open the printed URL in any browser and complete
Step 6. The browser then fails to load a `http://localhost:8089/callback?...`
page. Copy that full URL from the address bar and, while `add-account` is still
waiting, request it on the server:

```bash
curl -s 'http://localhost:8089/callback?PASTE_THE_REST_HERE'
```

An alternative is to authorize locally and copy the token as described in
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

To replace an expired token or obtain a fresh grant after leaving Testing:

```bash
msgvault add-account you@gmail.com --force
```

For an account already using a read-only grant, keep it read-only:

```bash
msgvault add-account you@gmail.com --force --readonly
```

`--force` deletes the local token and starts browser authorization. It does not
revoke Google's existing grant or narrow write access; follow the
[revoke-and-re-add procedure](#read-only-access) for that. Reauthorize Calendar
or other integrations separately if their scopes need restoring. For a named
OAuth app, keep the appropriate `--oauth-app` binding.

### Multiple Accounts

For personal Gmail accounts, a single `client_secret.json` works for all of them. Each `add-account` call creates a separate token file:

```bash
msgvault add-account personal@gmail.com
msgvault add-account other@gmail.com

msgvault sync   # syncs all accounts
```

!!! tip
    While the app is in Testing, list every Gmail address you want to sync under **Google Auth Platform > Audience > Test users** ([Add test users](#add-test-users)). This is the most common reason a second account fails to authorize.

#### Google Workspace Accounts

Many Google Workspace organizations restrict OAuth to apps created within their own org. If you get an "access denied" or "app blocked" error when authorizing a Workspace account with your personal OAuth app, the org likely requires its own app.

To handle this, create a separate Google Cloud project inside the Workspace org (Steps 1-4 above), then add it as a named OAuth app in `config.toml`:

```toml
[oauth]
client_secrets = "/path/to/personal_secret.json"    # default for personal Gmail

[oauth.apps.acme]
client_secrets = "/path/to/acme_workspace_secret.json"
```

Then specify the app when adding Workspace accounts:

```bash
msgvault add-account you@acme.com --oauth-app acme
msgvault add-account personal@gmail.com              # uses default
```

The binding is stored per account, so `sync`, `verify`, and `serve` automatically use the correct credentials. You only need `--oauth-app` when first adding or rebinding an account.

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

Workspace admins can avoid per-user browser OAuth by using a Google service account with domain-wide delegation.

1. Create a Google Cloud service account in the Workspace-owned project.
2. Create and download a JSON key for the service account.
3. In the Google Admin Console, authorize the service account client ID for:
   - `https://www.googleapis.com/auth/gmail.readonly`
   - `https://www.googleapis.com/auth/gmail.modify`
   - `https://www.googleapis.com/auth/calendar.readonly` if you will sync Google Calendar
   - `https://mail.google.com/` if you will run `delete-staged --permanent`
4. Store the key with owner-only permissions, for example `chmod 600 /path/to/workspace-service-account.json`.

Both `gmail.readonly` and `gmail.modify` are required. The Gmail service-account paths request that pair when minting a delegated token, so a delegation grant limited to `gmail.readonly` fails the token exchange and `add-account`, `sync`, `serve`, and `verify` all stop working for that account.

!!! note
    `--readonly` does not apply to service accounts, and is rejected with an error if passed. Scope is set by the delegation grant in the Admin Console rather than by msgvault flags. Narrowing what the Gmail service-account paths request, so that a read-only delegation grant becomes usable, is a possible future change.

Configure the key as the default Google credential:

```toml
[oauth]
service_account_key = "/path/to/workspace-service-account.json"
```

Or bind it to a named app:

```toml
[oauth.apps.acme]
service_account_key = "/path/to/acme-service-account.json"
```

Then add accounts normally:

```bash
msgvault add-account you@acme.com
msgvault add-account teammate@acme.com --oauth-app acme
```

Service account mode validates the delegated Gmail profile and registers the source, but it does not create per-user token files. Do not combine service-account accounts with `--headless`, `--force` or `--readonly`; delegated tokens are minted on demand.

For Google Calendar with a service account, enable the Google Calendar API and authorize the `calendar.readonly` scope above. Then configure a `[[gcal]]` source and run `msgvault sync-calendar user@domain.com --oauth-app acme` (or let `msgvault serve` run the schedule). No browser token is created.

### Headless Server Setup

When running msgvault on a headless server (SSH, VPS, Docker), there is no browser available for OAuth. Google's device code flow does not support Gmail scopes, so you must authorize on a machine with a browser and copy the token to your server.

Run `--headless` to see the setup instructions:

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

    ssh user@server 'mkdir -p ~/.msgvault/tokens'
    scp ~/.msgvault/tokens/you@gmail.com.json user@server:~/.msgvault/tokens/

Step 3: On the headless server, register the account:

    msgvault add-account you@gmail.com

The token will be detected and the account registered. No browser needed.
```

!!! note "Read-only on a headless server"
    `--readonly` is echoed into the commands printed above, so `msgvault add-account you@gmail.com --headless --readonly` shows the read-only variant to run on the machine with a browser.

    The revoke-and-re-add procedure above works unchanged on a headless host. Step 3 prints an authorization URL you can open from any browser, then waits for the callback exactly as described here.

#### Step-by-Step

1. **On your local machine** (with a browser), install msgvault and run:
   ```bash
   msgvault add-account you@gmail.com
   ```
   Complete the OAuth flow in your browser.

2. **Copy the token** to your headless server:
   ```bash
   ssh user@server mkdir -p ~/.msgvault/tokens
   scp ~/.msgvault/tokens/you@gmail.com.json user@server:~/.msgvault/tokens/
   ```

3. **On the headless server**, register the account:
   ```bash
   msgvault add-account you@gmail.com
   ```
   msgvault detects the existing token and registers the account. Output:
   ```
   Account you@gmail.com is ready.
   You can now run: msgvault sync-full you@gmail.com
   ```

4. **Sync your email**:
   ```bash
   msgvault sync-full you@gmail.com
   ```

The token file contains a refresh token that msgvault uses to renew short-lived
access tokens. You need to copy a replacement if that refresh token expires or
access is revoked. In an External app still in Testing, Gmail refresh tokens
expire after seven days; see [Choose Testing or In production](#choose-testing-or-in-production).

!!! note
    Both machines must use the same OAuth client credentials. The token is tied to the OAuth client that created it. If the account uses a named OAuth app (`--oauth-app`), configure the same `[oauth.apps.<name>]` section on both machines.

## Microsoft 365 (Outlook / Hotmail)

The `add-o365` command connects Outlook.com, Hotmail, Live.com, and Microsoft 365 organizational accounts via OAuth2 with XOAUTH2 IMAP authentication. No app password is needed.

### Prerequisites: Azure AD App Registration

You need to register an application in Microsoft Entra (Azure AD) before using `add-o365`.

An Outlook.com-only account can't register an app on its own. Register it in a tenant you can use, such as a work tenant or a free Azure/Entra directory, and allow personal Microsoft accounts under supported account types.

1. Go to [Azure Portal](https://portal.azure.com/) and navigate to **Microsoft Entra ID > App registrations > New registration**
2. Set the fields:
   - **Name:** `msgvault`
   - **Supported account types:** "Accounts in any organizational directory and personal Microsoft accounts"
    - **Redirect URI:** Platform = **Mobile and desktop applications**, URI = your `redirect_uri` from `config.toml` (default: `http://localhost:8089/callback/microsoft`)
3. Click **Register**
4. Under **API permissions**, click **Add a permission > Microsoft Graph > Delegated permissions**, then add `IMAP.AccessAsUser.All`
5. Under **Authentication**, enable **Allow public client flows** (required for PKCE and for `--headless`)
6. If you will use a custom `redirect_uri` in `config.toml`, make sure the Redirect URI in the app registration matches it exactly — including scheme, host, port, and path. For `https://localhost/` on a privileged port (e.g. 443), register that exact URI.
7. Copy the **Application (client) ID** from the app's Overview page

### Configure msgvault

Add a `[microsoft]` section to your `config.toml`:

```toml
[microsoft]
client_id = "your-azure-app-client-id"
```

To restrict authorization to a specific organization, set `tenant_id`:

```toml
[microsoft]
client_id = "your-azure-app-client-id"
tenant_id = "your-org-tenant-id"
```

When `tenant_id` is omitted (or set to `"common"`), both personal Microsoft accounts and organizational accounts can authorize.

### Add Your Account

```bash
msgvault add-o365 you@outlook.com
```

This opens your browser for Microsoft OAuth consent. After you authorize, msgvault:

- Validates the token matches the email you specified
- Auto-detects the correct IMAP host based on account type
- Configures XOAUTH2 authentication automatically

IMAP checks Microsoft's `email` claim when present, otherwise `preferred_username`. If that username differs from your mailbox address, pass your own sign-in name with `--sign-in`: `msgvault add-o365 john@company.com --sign-in jdoe@company.onmicrosoft.com`. This also sets the browser login hint. Re-authorization needs the flag too; msgvault previously accepted a differing username with a warning. The flag permits that username only when `email` is absent. Failed checks preserve existing credentials.

Graph mail (`--graph`), Teams, and Microsoft contacts consult your Microsoft profile when token identity fields differ or are absent. They accept your mailbox or an SMTP alias listed there and need no `--sign-in` flag.

On a machine without a browser, such as a server or a container, add `--headless`:

```bash
msgvault add-o365 you@outlook.com --headless
```

msgvault prints a Microsoft URL and a code. Open the URL on any device and enter the code. `add-teams` accepts the same flag.

Personal accounts (hotmail.com, outlook.com, live.com, msn.com) connect to `outlook.office.com`. Organizational accounts (company Microsoft 365) connect to `outlook.office365.com`. This detection is automatic.

To restrict to a specific tenant at authorization time:

```bash
msgvault add-o365 you@example.com --tenant your-org-tenant-id
```

### Microsoft Teams Graph Sync

Teams ingestion uses the same `[microsoft] client_id` and redirect URI, but it
requests Microsoft Graph delegated scopes and stores a separate token file under
`tokens/teams_<email>.json`. The `microsoft_<email>.json` token created by
`add-o365` is for IMAP and is not reused for Teams.

If you will archive Teams chats and channels, add these **Microsoft Graph**
delegated permissions to the app registration:

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

Some organizations require administrator consent before delegated channel
message permissions can be used. See [Microsoft Teams](/docs/usage/teams/) for the
full Teams workflow.

### Microsoft Graph Mail Sync

If IMAP is turned off for a mailbox, `add-o365 --graph` syncs it through the
Microsoft Graph mail API. It uses the same `[microsoft] client_id` and redirect
URI. Add the **Microsoft Graph** delegated permission `Mail.Read` to the app
registration, then authorize and sync:

```bash
msgvault add-o365 you@example.com --graph
msgvault sync you@example.com
```

The token is saved under `tokens/msmail_<email>.json`, and the account has the
type `msmail`. Each mail folder becomes a label. The first sync downloads every
folder. Later syncs fetch only the changes, including moves between folders
and deletes. The daemon schedules the account like any other.

To delete messages at the source with `delete-staged`, also add the delegated
permission `Mail.ReadWrite`. Sync does not use it. The first `delete-staged`
for the account asks to upgrade the token. See
[Deleting Email](/docs/usage/deletion/).

To sync Microsoft contacts, also add the delegated permission
`Contacts.ReadWrite`. See [CardDAV Contacts](/docs/usage/people-carddav/#microsoft-contacts).

A Graph account is a new account. If the same mailbox is also synced over
IMAP, the vault holds two copies. Run `msgvault dedup --collection` to hide the
extra copies, and `--undo` to reverse it.

### Sync Your Email

After adding the account, sync it the same way as any other account:

```bash
msgvault sync-full you@outlook.com
```

### Headless Servers

Sign in from SSH, a server, or a container without opening a local browser:

```bash
msgvault add-o365 you@outlook.com --headless
```

For Graph mail, add `--graph`. For Teams, run
`msgvault add-teams you@example.com --headless`.
Open the printed Microsoft URL on another device and enter the code. Complete
sign-in there; msgvault saves the token on the server.

You can also authorize on another machine and copy its token. For IMAP mail:

1. On your local machine, run `msgvault add-o365 you@outlook.com` and complete the browser flow.
2. Copy the token to the server:
   ```bash
   ssh user@server mkdir -p ~/.msgvault/tokens
   scp ~/.msgvault/tokens/microsoft_you@outlook.com.json \
       user@server:~/.msgvault/tokens/
   ```
3. On the server, run `msgvault add-o365 you@outlook.com` again. It detects the existing token and registers the account without a browser.

Both machines must use the same `client_id` in their `[microsoft]` config.
