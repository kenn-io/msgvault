# Dev build script for Windows
# Usage: powershell -File scripts/build.ps1
#
# Builds msgvault.exe in the repo root with debug info and FTS5 +
# sqlite-vec support. Requires Go, a C compiler (GCC via MSYS2/MinGW
# or TDM-GCC), and sqlite3 development headers. Install them under
# MSYS2 with `pacman -S mingw-w64-x86_64-sqlite3` and ensure
# CGO_CFLAGS points to the include directory (this script sets it
# automatically for the default MSYS2 path).

$ErrorActionPreference = 'Stop'

$version = & git describe --tags --always --dirty 2>$null
if (-not $version) { $version = "dev" }
$commit = & git rev-parse --short HEAD 2>$null
if (-not $commit) { $commit = "unknown" }
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$ldflags = @(
    "-X go.kenn.io/msgvault/cmd/msgvault/cmd.Version=$version"
    "-X go.kenn.io/msgvault/cmd/msgvault/cmd.Commit=$commit"
    "-X go.kenn.io/msgvault/cmd/msgvault/cmd.BuildDate=$buildDate"
) -join ' '

$env:CGO_ENABLED = 1

# sqlite_vec on Windows + MinGW needs a specific set of C/linker flags,
# each of which must be present no matter what the user already exported
# in CGO_CFLAGS/CGO_LDFLAGS:
#   - -IC:/msys64/mingw64/include points to the MSYS2-provided sqlite3.h.
#   - -fgnu89-inline makes arrow-go/v18's plain `inline` helpers emit an
#     external definition; otherwise MinGW 15 leaves ArrowArrayIsReleased
#     and friends undefined at link time.
#   - -Wl,--allow-multiple-definition then tells ld to keep the first of
#     the duplicate externals the previous flag produces in every TU.
# Append each flag only when missing, so users who already set their own
# CGO_CFLAGS/CGO_LDFLAGS (e.g. a custom SQLite include path) don't get
# them silently overwritten.
function Add-CgoFlag([string]$var, [string]$flag) {
    $existing = [Environment]::GetEnvironmentVariable($var, 'Process')
    if (-not $existing) {
        [Environment]::SetEnvironmentVariable($var, $flag, 'Process')
    } elseif ($existing -notmatch [regex]::Escape($flag)) {
        [Environment]::SetEnvironmentVariable($var, "$existing $flag", 'Process')
    }
}

if (Test-Path "C:\msys64\mingw64\include\sqlite3.h") {
    Add-CgoFlag "CGO_CFLAGS" "-IC:/msys64/mingw64/include"
}
Add-CgoFlag "CGO_CFLAGS" "-fgnu89-inline"
Add-CgoFlag "CGO_LDFLAGS" "-Wl,--allow-multiple-definition"

Write-Host "Building msgvault $version ($commit)..."
& go build -tags "fts5 sqlite_vec" -ldflags "$ldflags" -o msgvault.exe ./cmd/msgvault
if ($LASTEXITCODE -ne 0) {
    Write-Host "Build failed." -ForegroundColor Red
    exit 1
}
Write-Host "Built: msgvault.exe" -ForegroundColor Green
