# msgvault installer for Windows
# Usage: powershell -ExecutionPolicy ByPass -c "irm https://msgvault.io/install.ps1 | iex"

$ErrorActionPreference = 'Stop'

$repo = 'kenn-io/msgvault'
$binaryName = 'msgvault.exe'

function Write-Info($msg) { Write-Host $msg -ForegroundColor Green }
function Write-Warn($msg) { Write-Host $msg -ForegroundColor Yellow }
function Write-Err($msg) { Write-Host $msg -ForegroundColor Red }

function Test-EnvBool($name) {
    $val = [Environment]::GetEnvironmentVariable($name)
    return ($val -match '^(1|true|yes)$')
}

function Get-Architecture {
    # Check if running on ARM64
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') {
        return 'arm64'
    }

    # Check using .NET for more reliable detection
    try {
        $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
        switch ($arch.ToString()) {
            'X64' { return 'amd64' }
            'X86' { return '386' }
            'Arm64' { return 'arm64' }
            default { return 'amd64' }
        }
    } catch {
        # Fallback for older PowerShell
        if ([System.Environment]::Is64BitOperatingSystem) {
            return 'amd64'
        } else {
            return '386'
        }
    }
}

function Invoke-WebRequestCompat {
    # -UseBasicParsing is required in PowerShell 5.x but removed in 6+
    param([string]$Uri, [string]$OutFile)

    # Ensure TLS 1.2 for older PowerShell (5.x defaults may be TLS 1.0)
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }

    $params = @{ Uri = $Uri }
    if ($OutFile) { $params.OutFile = $OutFile }

    # Only add -UseBasicParsing for PowerShell 5.x (it's removed in 6+)
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    if ($OutFile) {
        Invoke-WebRequest @params
    } else {
        Invoke-RestMethod @params
    }
}

function Get-LatestVersion {
    # Use the HTML /releases/latest endpoint, which 302-redirects to
    # /releases/tag/<version>. Unlike api.github.com it is not rate-limited
    # at 60 req/hr per IP, so users behind shared NAT / VPN don't get 403.
    $url = "https://github.com/$repo/releases/latest"

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }

    $params = @{
        Uri = $url
        MaximumRedirection = 0
        ErrorAction = 'Stop'
    }
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    $location = $null
    try {
        # PowerShell 7+ returns the 302 as a normal response.
        $response = Invoke-WebRequest @params
        $location = $response.Headers.Location
    } catch {
        # PowerShell 5.x throws System.Net.WebException for 3xx when
        # MaximumRedirection=0. The Location header lives on the response.
        if ($_.Exception.Response) {
            $location = $_.Exception.Response.Headers['Location']
        } else {
            throw "Failed to fetch latest version: $_"
        }
    }

    if ($location -is [array]) { $location = $location[0] }
    if (-not $location) {
        throw "Failed to fetch latest version: no Location header from $url"
    }
    if ($location -notmatch '/releases/tag/([^/]+)/?$') {
        throw "Failed to fetch latest version: unexpected redirect target $location"
    }
    return $Matches[1]
}

function Get-InstallDir {
    # Use MSGVAULT_INSTALL_DIR if set, otherwise default to ~/.msgvault/bin
    if ($env:MSGVAULT_INSTALL_DIR) {
        return $env:MSGVAULT_INSTALL_DIR
    }
    return Join-Path $env:USERPROFILE '.msgvault\bin'
}

function Add-ToPath($dir) {
    $currentPath = [Environment]::GetEnvironmentVariable('Path', 'User')

    # Normalize for comparison: trim trailing slashes, compare case-insensitively
    $normalizedDir = $dir.TrimEnd('\', '/')
    $alreadyInPath = $currentPath -split ';' | Where-Object {
        $_.TrimEnd('\', '/') -ieq $normalizedDir
    }
    if ($alreadyInPath) {
        Write-Info "Directory already in PATH"
        return $false
    }

    $newPath = "$currentPath;$dir"
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')

    # Also update current session
    $env:Path = "$env:Path;$dir"

    return $true
}

function Install-Msgvault {
    Write-Info "Installing msgvault..."
    Write-Host ""

    $arch = Get-Architecture
    Write-Info "Platform: windows/$arch"

    # Handle unsupported architectures
    if ($arch -eq '386') {
        Write-Err "Error: 32-bit Windows is not supported."
        Write-Err "msgvault requires 64-bit Windows (amd64 or arm64)."
        exit 1
    }

    $version = Get-LatestVersion
    Write-Info "Latest version: $version"

    $versionNum = $version.TrimStart('v')
    $archiveName = "msgvault_${versionNum}_windows_${arch}.zip"
    $downloadUrl = "https://github.com/$repo/releases/download/$version/$archiveName"

    $installDir = Get-InstallDir
    Write-Info "Install directory: $installDir"
    Write-Host ""

    # Create install directory
    if (-not (Test-Path $installDir)) {
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    }

    # Create temp directory
    $tmpDir = Join-Path $env:TEMP "msgvault-install-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

    try {
        $archivePath = Join-Path $tmpDir $archiveName

        Write-Info "Downloading $archiveName..."
        Invoke-WebRequestCompat -Uri $downloadUrl -OutFile $archivePath

        # Verify checksum (fail-closed: errors abort installation unless MSGVAULT_SKIP_CHECKSUM env var is set)
        $checksumUrl = "https://github.com/$repo/releases/download/$version/SHA256SUMS"
        $checksumFile = Join-Path $tmpDir "SHA256SUMS"

        if (Test-EnvBool 'MSGVAULT_SKIP_CHECKSUM') {
            Write-Warn "Warning: Skipping checksum verification (MSGVAULT_SKIP_CHECKSUM is set)"
        } else {
            Write-Info "Verifying checksum..."
            try {
                Invoke-WebRequestCompat -Uri $checksumUrl -OutFile $checksumFile
            } catch {
                Write-Err "Error: Could not download checksums file: $_"
                Write-Err "Set MSGVAULT_SKIP_CHECKSUM=1 to bypass verification (not recommended)"
                exit 1
            }

            # Parse checksum file and find matching entry
            # Handle SHA256SUMS formats: "hash  filename", "hash *filename" (binary), "hash  ./filename"
            $matchingLines = @()
            foreach ($line in Get-Content $checksumFile) {
                if ($line -match '^\s*$') { continue }  # Skip empty lines
                $parts = $line -split '\s+', 2
                if ($parts.Count -lt 2) { continue }
                $hash = $parts[0]
                $filename = $parts[1]
                # Normalize: strip leading * (binary mode) or ./ (relative path)
                $filename = $filename -replace '^[\*]', ''
                $filename = $filename -replace '^\.\/', ''
                $filename = $filename -replace '^\.\\', ''  # Windows-style
                if ($filename -eq $archiveName) {
                    $matchingLines += $hash
                }
            }

            if ($matchingLines.Count -eq 0) {
                Write-Err "Error: Could not find checksum for $archiveName in SHA256SUMS"
                Write-Err "Set MSGVAULT_SKIP_CHECKSUM=1 to bypass verification (not recommended)"
                exit 1
            }

            if ($matchingLines.Count -gt 1) {
                Write-Err "Error: Multiple checksum entries found for $archiveName"
                exit 1
            }

            $expectedHash = $matchingLines[0]
            $actualHash = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()

            if ($actualHash -ne $expectedHash.ToLower()) {
                Write-Err "Error: Checksum verification failed!"
                Write-Err "Expected: $expectedHash"
                Write-Err "Got:      $actualHash"
                exit 1
            }
            Write-Info "Checksum verified."
        }

        Write-Info "Extracting..."
        if ($PSVersionTable.PSVersion.Major -lt 5) {
            Write-Err "Error: PowerShell 5.0 or later is required for Expand-Archive."
            Write-Err "Please upgrade PowerShell or download the release manually from GitHub."
            exit 1
        }
        try {
            Expand-Archive -Path $archivePath -DestinationPath $tmpDir -Force
        } catch {
            Write-Err "Error: Failed to extract archive: $_"
            exit 1
        }

        # Find the binary (may be in a top-level directory if GoReleaser wraps it)
        $binaryFile = Get-ChildItem -Path $tmpDir -Recurse -Filter $binaryName | Select-Object -First 1
        if (-not $binaryFile) {
            Write-Err "Error: Could not find $binaryName in extracted archive"
            exit 1
        }

        $destPath = Join-Path $installDir $binaryName

        # Remove existing binary if present
        if (Test-Path $destPath) {
            Remove-Item $destPath -Force
        }

        Move-Item $binaryFile.FullName $destPath -Force

        Write-Host ""
        Write-Info "Installation complete!"
        Write-Host ""

        # Add to PATH
        if (-not (Test-EnvBool 'MSGVAULT_NO_MODIFY_PATH')) {
            $pathUpdated = Add-ToPath $installDir
            if ($pathUpdated) {
                Write-Info "Added $installDir to PATH"
                Write-Warn "Restart your terminal for PATH changes to take effect."
                Write-Host ""
            }
        }

        # Get started instructions
        Write-Host "Get started:"
        Write-Host "  msgvault init-db"
        Write-Host "  msgvault add-account you@gmail.com"
        Write-Host "  msgvault sync-full you@gmail.com --limit 100"
        Write-Host "  msgvault tui"

    } finally {
        # Cleanup temp directory
        if (Test-Path $tmpDir) {
            Remove-Item $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

# Run installer
Install-Msgvault
