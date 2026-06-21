---
title: OAuth Setup
description: Create OAuth credentials for Gmail (Google Cloud) or Microsoft 365 (Azure AD) and authorize msgvault.
---

## Google (Gmail)

msgvault requires OAuth credentials to access the Gmail API. This section walks through the complete setup.

### Step 1: Create a Google Cloud Project

1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. Create a new project or select an existing one
3. Note your project ID

### Step 2: Enable the Gmail API

1. Navigate to **APIs & Services > Library**
2. Search for "Gmail API"
3. Click **Enable**

### Step 3: Configure OAuth Consent Screen

1. Go to **APIs & Services > OAuth consent screen** (Google may call this **Google Auth Platform**)
2. Choose **External** user type (or **Internal** for Google Workspace)
3. Fill in required fields:
   - App name: `msgvault`
   - User support email: your email
   - Developer contact email: your email
4. Click **Save and Continue**
5. On the **Data Access** page, click **Add or Remove Scopes**
6. Add the scope: `https://www.googleapis.com/auth/gmail.modify`
7. Save and continue through the remaining screens
8. Under **Test users**, add all Gmail addresses you want to sync

!!! note
    The `gmail.modify` scope enables deletion features while sync operations remain read-only. When you first run `delete-staged`, msgvault will prompt you to upgrade to full `mail.google.com` access for batch deletion.

### Step 4: Create OAuth Client Credentials

1. Go to **APIs & Services > Credentials**
2. Click **Create Credentials > OAuth client ID**
3. Choose **Desktop app** as the application type
4. Name it `msgvault` (or similar)
5. Click **Create**
6. Download the JSON file
7. Save it as `client_secret.json` in a secure location

!!! warning
    Never commit `client_secret.json` to version control.

!!! warning "Desktop App Only"
    You must choose **Desktop app** as the client type. Do not use "TVs and Limited Input devices" as Google's device code flow does not support Gmail scopes.

### Step 5: Configure msgvault

Create `config.toml` in your msgvault data directory:

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

### Step 6: Add Your Account

```bash
msgvault add-account you@gmail.com
```

This opens your browser to Google's OAuth consent page. Sign in, grant access, and tokens are stored locally in `~/.msgvault/tokens/`.

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
   - `https://mail.google.com/` if you will run `delete-staged --permanent`
4. Store the key with owner-only permissions, for example `chmod 600 /path/to/workspace-service-account.json`.

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

Service account mode validates the delegated Gmail profile and registers the source, but it does not create per-user token files. Do not combine service-account accounts with `--headless` or `--force`; delegated tokens are minted on demand.

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
   - **Redirect URI:** Platform = **Mobile and desktop applications**, URI = `http://localhost:8089/callback/microsoft`
3. Click **Register**
4. Under **API permissions**, click **Add a permission > APIs my organization uses**, search for **Office 365 Exchange Online**, select **Delegated permissions**, then add `IMAP.AccessAsUser.All`
5. Under **Authentication**, enable **Allow public client flows** (required for PKCE)
6. Copy the **Application (client) ID** from the app's Overview page

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
msgvault add-o365 you@company.com --tenant your-org-tenant-id
```

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
