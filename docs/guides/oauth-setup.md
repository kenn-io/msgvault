---
title: OAuth Setup
description: Create OAuth credentials for Gmail (Google Cloud) or Microsoft 365 (Azure AD) and authorize msgvault.
---

## Google (Gmail and Calendar)

msgvault requires OAuth credentials to access the Gmail API. This section walks through the complete setup.

### Step 1: Create a Google Cloud Project

1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. If it's your first time using Google Cloud Console, select your country and agree to the Terms of Service
3. Click **Select a Project** and click **New project** (recommended) or select an existing one
4. Name your project `msgvault`. Parent resource can be left as the default of "No organization"
5. Click **Create**
6. The Notifications will spin for a few seconds and then allow you to click Select Project for the new `msgvault` project

### Step 2: Enable Google APIs

1. On the left bar/under the hamburger menu, navigate to **APIs & Services > Library**
2. In the search bar, search for "Gmail API" and click the **Gmail API** box
3. Click **Enable**
4. If you wish to sync Google Calendar too, click **Library**, search for "Google Calendar API", click the **Google Calendar API** box and click **Enable**

### Step 3: Configure OAuth Consent Screen

1. Go to **APIs & Services > OAuth consent screen** (Google may call this **Google Auth Platform**)
2. Click **Get started**
3. Fill in required fields as you click through:
   - App name: `msgvault`
   - User support email: your email
   - Audience: **External** for regular Gmail, **Internal** for Google Workspace
   - Contact Information: your email
   - Agree to the "Google API Services: User Data Policy" checkbox
4. Click **Create**
5. Click **Data Access** on the left bar and click **Add or Remove Scopes**
6. At the bottom of the page under "Manually add scopes", enter the scopes you intend to use, one per line, then click **Add to table**:
    - `https://www.googleapis.com/auth/gmail.readonly` — always
    - `https://www.googleapis.com/auth/gmail.modify` — unless you will only ever run read-only (see below)
    - `https://www.googleapis.com/auth/calendar.readonly` — if you will sync Google Calendar
7. Click **Update** then **Save**
8. Go to **Audience** on the left bar and in the **Test users** section, click the **Add users** button and add your Gmail email address

!!! warning "This page does not restrict what gets granted"
    The scopes listed here are a declaration used for Google's verification review. They do not limit what the authorization server grants at request time, that is determined solely by what msgvault requests. Removing `gmail.modify` here will **not** give you a read-only setup; use `--readonly` when adding the account instead.

!!! note
    By default msgvault requests `gmail.readonly` and `gmail.modify`. Sync itself only ever reads, `gmail.modify` is what later enables trash-based deletion. When you first run `delete-staged --permanent`, msgvault prompts you to upgrade to full `mail.google.com` access for batch deletion.

    If you never intend to delete mail from Gmail, add the account with `--readonly` (see [Read-Only Access](#read-only-access)) and you can leave `gmail.modify` off this page entirely.

### Step 4: Create OAuth Client Credentials

1. Through the hamburger menu, go to **APIs & Services > Credentials**
2. Click **Create Credentials > OAuth client ID**
3. Choose **Desktop app** as the application type. Not "TVs and Limited Input devices" — Google's device-code flow does not support Gmail scopes
4. Name it `msgvault` (or similar)
5. Click **Create**
6. Click **Download JSON** to download the JSON file
7. Save it as `client_secret.json` in a secure location
8. Click OK

!!! warning
    Never commit `client_secret.json` to version control.

### Step 5: Configure msgvault

Create a file called `config.toml` in your msgvault directory.

- **macOS / Linux:** `~/.msgvault/config.toml`
- **Windows:** `C:\Users\<you>\.msgvault\config.toml`

!!! tip
    The `.msgvault` directory is created automatically the first time you run any msgvault command. If you're unsure of the exact path, run `msgvault add-account you@gmail.com`; the error message may show you where to create the config file.

```toml
[oauth]
client_secrets = "/path/to/your/client_secret.json"
```

On Windows, use forward slashes or escaped backslashes for the path:
```toml
[oauth]
client_secrets = "C:/Users/you/Downloads/client_secret.json"
```

!!! tip
    These commands will do what's needed on macOS/Linux/WSL assuming you're putting the client_secret.json file in ~/.msgvault/:
    ```
    mkdir -m 700 -p ~/.msgvault
    printf '[oauth]\nclient_secrets = "~/.msgvault/client_secret.json"\n' > ~/.msgvault/config.toml
    echo client_secret.json >> ~/.msgvault/.gitignore
    ```

Copy your `client_secret.json` file to `~/.msgvault/` or wherever you've referenced it in the configuration file and set permissions to limit access (`chmod 600 ~/.msgvault/client_secret.json` on macOS/Linux).

### Step 6: Add Your Account

```bash
msgvault add-account you@gmail.com
```

This opens your browser to Google's OAuth consent page. Sign in, grant access, and tokens are stored locally in `~/.msgvault/tokens/`.

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

Revoking clears every Google scope for that account, so re-run whichever commands granted them — `msgvault add-calendar` for Calendar, `msgvault add-synctech-sms-drive` for Drive. Your archived mail is untouched: this replaces credentials, not data.

Confirm the result by reading the token's scopes:

```bash
python3 -c "import json,os;print(*json.load(open(os.path.expanduser('~/.msgvault/tokens/you@gmail.com.json')))['scopes'],sep='\n')"
```

#### Headless Authorization

```
Starting browser authorization...
Opening browser for authorization...
If browser doesn't open, visit:
https://accounts.google.com/o/oauth2/auth?access_type=offline&LONG_STRING_HERE
```

msgvault will be unable to open a browser but will give a URL `https://accounts.google.com/o/oauth2/auth?access_type=offline&LONG_STRING_HERE`. Copy this to your browser
- Click **Continue** at the "Google hasn't verified this app" prompt
- Confirm the Gmail and Calendar access if chosen by clicking the **Select all** checkbox and then **Continue**
- The browser will try open a connection to localhost:8089 with the authorization code. Copy this URL from your browser and on the headless host run: `curl -s 'http://localhost:8089/callback?DIFFERENT_LONG_STRING'` with the full URL from your browser. (Another option is to forward the port from the computer running the browser to the headless host with `ssh -L 8089:localhost:8089 user@headlessserver`)

### Multiple Accounts

For personal Gmail accounts, a single `client_secret.json` works for all of them. Each `add-account` call creates a separate token file:

```bash
msgvault add-account personal@gmail.com
msgvault add-account other@gmail.com

msgvault sync   # syncs all accounts
```

!!! tip
    Make sure all Gmail addresses you want to sync are listed as **Test users** in your Google Cloud OAuth consent screen (Step 3 above). This is the most common reason a second account fails to authorize.

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
  <img src="/assets/generated/concepts/oauth-multi-account-concept.png" alt="Two OAuth apps and the token files they create. A default app (config block [oauth]) authorizes personal Gmail accounts personal@gmail.com and other@gmail.com; a named app ([oauth.apps.acme]) authorizes the Workspace account you@acme.com. Each add-account run writes its own token file under ~/.msgvault/tokens/, color-matched to its account." loading="lazy" style="width: 100%; display: block;" />
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

The token file contains OAuth refresh tokens that are automatically renewed. You only need to copy it once unless you revoke access.

!!! note
    Both machines must use the same OAuth client credentials. The token is tied to the OAuth client that created it. If the account uses a named OAuth app (`--oauth-app`), configure the same `[oauth.apps.<name>]` section on both machines.

## Microsoft 365 (Outlook / Hotmail)

The `add-o365` command connects Outlook.com, Hotmail, Live.com, and Microsoft 365 organizational accounts via OAuth2 with XOAUTH2 IMAP authentication. No app password is needed.

### Prerequisites: Azure AD App Registration

You need to register an application in Microsoft Entra (Azure AD) before using `add-o365`.

1. Go to [Azure Portal](https://portal.azure.com/) and navigate to **Microsoft Entra ID > App registrations > New registration**
2. Set the fields:
   - **Name:** `msgvault`
   - **Supported account types:** "Accounts in any organizational directory and personal Microsoft accounts"
    - **Redirect URI:** Platform = **Mobile and desktop applications**, URI = your `redirect_uri` from `config.toml` (default: `http://localhost:8089/callback/microsoft`)
3. Click **Register**
4. Under **API permissions**, click **Add a permission > APIs my organization uses**, search for **Office 365 Exchange Online**, select **Delegated permissions**, then add `IMAP.AccessAsUser.All`
5. Under **Authentication**, enable **Allow public client flows** (required for PKCE)
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
message permissions can be used. See [Microsoft Teams](/usage/teams/) for the
full Teams workflow.

### Sync Your Email

After adding the account, sync it the same way as any other account:

```bash
msgvault sync-full you@outlook.com
```

### Headless Servers

On a headless server (SSH, VPS, Docker), authorize on a machine with a browser and copy the token file to the server:

1. On your local machine, run `msgvault add-o365 you@outlook.com` and complete the browser flow.
2. Copy the token to the server:
   ```bash
   ssh user@server mkdir -p ~/.msgvault/tokens
   scp ~/.msgvault/tokens/microsoft_you@outlook.com.json \
       user@server:~/.msgvault/tokens/
   ```
3. On the server, run `msgvault add-o365 you@outlook.com` again. It detects the existing token and registers the account without a browser.

Both machines must use the same `client_id` in their `[microsoft]` config.
