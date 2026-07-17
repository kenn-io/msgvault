# Build msgvault on Windows.
#
# Usage:
#   .\scripts\build.ps1                  # Debug build for the host architecture
#   .\scripts\build.ps1 -Release         # Optimized, stripped build
#   .\scripts\build.ps1 -Architecture arm64
#
# CGO is required for SQLite, sqlite-vec, and DuckDB. On Windows ARM64,
# duckdb-go does not publish a prebuilt library, so the first build compiles a
# pinned DuckDB static library and caches it under LocalAppData. Later builds
# reuse that library.

[CmdletBinding()]
param(
    [ValidateSet('amd64', 'arm64')]
    [string]$Architecture,

    [switch]$Release,

    [string]$OutputPath,

    [switch]$RebuildDuckDB
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repoRoot = Split-Path -Parent $PSScriptRoot
if (-not $OutputPath) {
    $OutputPath = Join-Path $repoRoot 'msgvault.exe'
} elseif (-not [IO.Path]::IsPathRooted($OutputPath)) {
    $OutputPath = Join-Path (Get-Location) $OutputPath
}

function Invoke-Checked {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Executable,

        [Parameter(ValueFromRemainingArguments = $true)]
        [string[]]$Arguments
    )

    & $Executable @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Executable failed with exit code $LASTEXITCODE"
    }
}

function Find-Executable {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Name,

        [string[]]$Candidates = @()
    )

    foreach ($candidate in $Candidates) {
        if ($candidate -and (Test-Path -LiteralPath $candidate)) {
            return (Resolve-Path -LiteralPath $candidate).Path
        }
    }

    $command = Get-Command $Name -ErrorAction SilentlyContinue
    if ($command) {
        return $command.Source
    }

    return $null
}

function Get-WindowsHostArchitecture {
    $hostArchitecture = $null
    try {
        $hostArchitecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    } catch {
        # RuntimeInformation is unavailable on older Windows PowerShell.
    }

    if ($hostArchitecture) {
        switch ($hostArchitecture) {
            'X64' { return 'amd64' }
            'Arm64' { return 'arm64' }
            'X86' { return '386' }
            'Arm' { return 'arm' }
            default { throw "Unsupported Windows host architecture: $hostArchitecture" }
        }
    }

    # This fallback is only used for host detection, never Go's target
    # settings, so an inherited GOARCH cannot affect the result.
    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        'x86' { return '386' }
        default { throw 'Unable to detect the Windows host architecture.' }
    }
}

function Invoke-WebRequestCompat {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Uri,

        [Parameter(Mandatory = $true)]
        [string]$OutFile
    )

    $parameters = @{
        Uri = $Uri
        OutFile = $OutFile
    }
    $isWindowsPowerShell = $PSVersionTable.PSVersion.Major -lt 6
    $originalSecurityProtocol = $null
    if ($isWindowsPowerShell) {
        # Windows PowerShell 5 can default to TLS 1.0 and its web cmdlet can
        # require the Internet Explorer parser unless basic parsing is set.
        $originalSecurityProtocol = [Net.ServicePointManager]::SecurityProtocol
        [Net.ServicePointManager]::SecurityProtocol = $originalSecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
        $parameters.UseBasicParsing = $true
    }

    try {
        Invoke-WebRequest @parameters | Out-Null
    } finally {
        if ($isWindowsPowerShell) {
            [Net.ServicePointManager]::SecurityProtocol = $originalSecurityProtocol
        }
    }
}

function Add-CgoFlag {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Variable,

        [Parameter(Mandatory = $true)]
        [string]$Flag
    )

    $existing = [Environment]::GetEnvironmentVariable($Variable, 'Process')
    if (-not $existing) {
        [Environment]::SetEnvironmentVariable($Variable, $Flag, 'Process')
    } elseif ($existing -notmatch [regex]::Escape($Flag)) {
        [Environment]::SetEnvironmentVariable($Variable, "$existing $Flag", 'Process')
    }
}

function Format-CgoPathFlag {
    param(
        [Parameter(Mandatory = $true)]
        [ValidateSet('-I', '-L')]
        [string]$Prefix,

        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    $normalizedPath = $Path.Replace('\', '/')
    if ($normalizedPath.Contains('"')) {
        throw "CGO path contains an unsupported quote character: $Path"
    }
    return "`"$Prefix$normalizedPath`""
}

function Format-GoCompilerCommand {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Path
    )

    if ($Path.Contains('"')) {
        throw "Compiler path contains an unsupported quote character: $Path"
    }
    return "`"$Path`""
}

function Get-GoModuleDirectory {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Module
    )

    $moduleJson = Invoke-Checked go mod download -json $Module | Out-String
    $moduleInfo = $moduleJson | ConvertFrom-Json
    if (-not $moduleInfo.Dir) {
        throw "Go module directory not found for $Module"
    }
    return $moduleInfo.Dir
}

function Initialize-SqliteHeader {
    param(
        [Parameter(Mandatory = $true)]
        [string]$CacheRoot
    )

    $includeDir = Join-Path $CacheRoot 'sqlite-include'
    $header = Join-Path $includeDir 'sqlite3.h'
    if (-not (Test-Path -LiteralPath $header)) {
        $sqliteModule = Get-GoModuleDirectory 'github.com/mattn/go-sqlite3'
        New-Item -ItemType Directory -Path $includeDir -Force | Out-Null
        Copy-Item (Join-Path $sqliteModule 'sqlite3-binding.h') $header
    }
    return $includeDir
}

function Initialize-Arm64Toolchain {
    param(
        [Parameter(Mandatory = $true)]
        [string]$CacheRoot
    )

    # Match the verified toolchain used by the Windows ARM64 release job.
    $toolchainVersion = '20260602'
    $toolchainName = 'llvm-mingw-20260602-ucrt-aarch64'
    $toolchainSha256 = 'cb5c20fbe1808e31ada5cbe4efd9daa2fee19c91dac6ec5ca1ac46a9c7247e37'
    $toolchainRoot = Join-Path $CacheRoot 'toolchains'
    $toolchain = Join-Path $toolchainRoot $toolchainName
    $compilerDirectory = Join-Path $toolchain 'bin'
    $compilerTools = @(
        (Join-Path $compilerDirectory 'aarch64-w64-mingw32-clang.exe'),
        (Join-Path $compilerDirectory 'aarch64-w64-mingw32-clang++.exe'),
        (Join-Path $compilerDirectory 'llvm-ar.exe'),
        (Join-Path $compilerDirectory 'llvm-ranlib.exe')
    )
    if (-not ($compilerTools | Where-Object { -not (Test-Path -LiteralPath $_) })) {
        return $compilerDirectory
    }

    New-Item -ItemType Directory -Path $toolchainRoot -Force | Out-Null
    $archive = Join-Path $toolchainRoot "$toolchainName.zip"
    $url = "https://github.com/mstorsjo/llvm-mingw/releases/download/$toolchainVersion/$toolchainName.zip"
    if (-not (Test-Path -LiteralPath $archive)) {
        $download = "$archive.download"
        Write-Host "Downloading the Windows ARM64 LLVM-MinGW toolchain (one-time setup)..."
        try {
            Invoke-WebRequestCompat -Uri $url -OutFile $download
            Move-Item -LiteralPath $download -Destination $archive -Force
        } finally {
            Remove-Item -LiteralPath $download -Force -ErrorAction SilentlyContinue
        }
    }

    $actualSha256 = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualSha256 -ne $toolchainSha256) {
        throw "Checksum mismatch for $archive`n  expected $toolchainSha256`n  actual   $actualSha256`nDelete the archive and retry."
    }

    Expand-Archive -LiteralPath $archive -DestinationPath $toolchainRoot -Force
    $missingTools = $compilerTools | Where-Object { -not (Test-Path -LiteralPath $_) }
    if ($missingTools) {
        throw "ARM64 compiler tools were not found after extracting $archive`n  missing: $($missingTools -join ', ')"
    }
    return $compilerDirectory
}

function Initialize-Arm64DuckDB {
    param(
        [Parameter(Mandatory = $true)]
        [string]$CacheRoot,

        [Parameter(Mandatory = $true)]
        [string]$CompilerDirectory
    )

    # duckdb-go v2.10504.0 uses DuckDB 1.5.4. Keep these pins aligned with
    # the Windows ARM64 release job in .github/workflows/release.yml.
    $duckdbVersion = '1.5.4'
    $duckdbCommit = '08e34c447bae34eaee3723cac61f2878b6bdf787'
    $duckdbGoVersion = (Invoke-Checked go list -m -f '{{.Version}}' github.com/duckdb/duckdb-go/v2 | Out-String).Trim()
    if ($duckdbGoVersion -ne 'v2.10504.0') {
        throw "scripts/build.ps1 expects duckdb-go v2.10504.0, but go.mod uses $duckdbGoVersion. Update the pinned DuckDB version and commit before building."
    }
    $source = Join-Path $CacheRoot "duckdb-$duckdbVersion"
    $build = Join-Path $source 'build\windows-arm64-static'
    $bundle = Join-Path $build 'libduckdb_bundle.a'

    if ($RebuildDuckDB -and (Test-Path -LiteralPath $build)) {
        Remove-Item -LiteralPath $build -Recurse -Force
    }
    if (Test-Path -LiteralPath $bundle) {
        return $build
    }

    $cmake = Find-Executable 'cmake.exe' @(
        (Join-Path $CompilerDirectory 'cmake.exe'),
        'C:\msys64\clangarm64\bin\cmake.exe',
        'C:\Program Files\CMake\bin\cmake.exe'
    )
    $ninja = Find-Executable 'ninja.exe' @(
        (Join-Path $CompilerDirectory 'ninja.exe'),
        'C:\msys64\clangarm64\bin\ninja.exe'
    )
    if (-not $cmake -or -not $ninja) {
        throw @"
Windows ARM64 needs CMake and Ninja for the one-time DuckDB build.
Install them from an MSYS2 CLANGARM64 shell:

  pacman -S --needed mingw-w64-clang-aarch64-cmake mingw-w64-clang-aarch64-ninja

Then rerun this command. The compiled library will be cached at:
  $build
"@
    }

    if (-not (Test-Path -LiteralPath $source)) {
        Write-Host "Downloading DuckDB $duckdbVersion source (one-time setup)..."
        Invoke-Checked git clone --depth 1 --branch "v$duckdbVersion" https://github.com/duckdb/duckdb.git $source | Out-Host
    }

    $actualCommit = (Invoke-Checked git -C $source rev-parse HEAD | Out-String).Trim()
    if ($actualCommit -ne $duckdbCommit) {
        throw "DuckDB source mismatch in $source`n  expected $duckdbCommit`n  actual   $actualCommit`nRemove that cache directory and retry."
    }

    $cc = Join-Path $CompilerDirectory 'aarch64-w64-mingw32-clang.exe'
    $cxx = Join-Path $CompilerDirectory 'aarch64-w64-mingw32-clang++.exe'
    $ar = Find-Executable 'llvm-ar.exe' @((Join-Path $CompilerDirectory 'llvm-ar.exe'))
    $ranlib = Find-Executable 'llvm-ranlib.exe' @((Join-Path $CompilerDirectory 'llvm-ranlib.exe'))
    foreach ($tool in @($cc, $cxx, $ar, $ranlib)) {
        if (-not $tool -or -not (Test-Path -LiteralPath $tool)) {
            throw "Required Windows ARM64 compiler tool not found: $tool"
        }
    }

    Write-Host "Building DuckDB $duckdbVersion for Windows ARM64 (one-time setup)..."
    Invoke-Checked $cmake -S $source -B $build -G Ninja `
        -DCMAKE_BUILD_TYPE=Release `
        "-DCMAKE_MAKE_PROGRAM=$ninja" `
        "-DCMAKE_C_COMPILER=$cc" `
        "-DCMAKE_CXX_COMPILER=$cxx" `
        "-DCMAKE_AR=$ar" `
        "-DCMAKE_RANLIB=$ranlib" `
        -DBUILD_SHELL=OFF `
        -DBUILD_UNITTESTS=OFF `
        '-DBUILD_EXTENSIONS=parquet;json;icu;autocomplete' | Out-Host

    $oldParallelLevel = $env:CMAKE_BUILD_PARALLEL_LEVEL
    try {
        $env:CMAKE_BUILD_PARALLEL_LEVEL = '2'
        Invoke-Checked $cmake --build $build --target `
            duckdb_static `
            parquet_extension `
            json_extension `
            icu_extension `
            autocomplete_extension `
            core_functions_extension `
            duckdb_generated_extension_loader | Out-Host
    } finally {
        $env:CMAKE_BUILD_PARALLEL_LEVEL = $oldParallelLevel
    }

    $libraries = @(
        Get-Item (Join-Path $build 'src\libduckdb_static.a')
        Get-Item (Join-Path $build 'extension\libduckdb_generated_extension_loader.a')
        Get-ChildItem (Join-Path $build 'extension') -Recurse -Filter 'lib*_extension.a'
        Get-ChildItem (Join-Path $build 'third_party') -Recurse -Filter 'libduckdb_*.a'
    ) | Sort-Object FullName -Unique
    $mri = @("CREATE $($bundle.Replace('\', '/'))")
    foreach ($library in $libraries) {
        $mri += "ADDLIB $($library.FullName.Replace('\', '/'))"
    }
    $mri += 'SAVE'
    $mri += 'END'
    $mri -join "`n" | & $ar -M | Out-Host
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $bundle)) {
        throw 'Failed to create the static DuckDB bundle'
    }

    return $build
}

$buildEnvironmentVariables = @(
    'GOOS',
    'GOARCH',
    'CGO_ENABLED',
    'CC',
    'PATH',
    'CGO_CFLAGS',
    'CGO_LDFLAGS',
    'CMAKE_BUILD_PARALLEL_LEVEL'
)
$originalBuildEnvironment = @{}
foreach ($variable in $buildEnvironmentVariables) {
    $originalBuildEnvironment[$variable] = [Environment]::GetEnvironmentVariable($variable, 'Process')
}

Push-Location $repoRoot
try {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw 'Go was not found in PATH. Install the version required by go.mod and retry.'
    }
    if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
        throw 'Git was not found in PATH. Install Git for Windows and retry.'
    }

    if (-not $Architecture) {
        $Architecture = Get-WindowsHostArchitecture
    }
    if ($Architecture -notin @('amd64', 'arm64')) {
        throw "Unsupported Windows architecture: $Architecture (expected amd64 or arm64)"
    }

    if ($env:MSGVAULT_BUILD_CACHE) {
        $cacheRoot = $env:MSGVAULT_BUILD_CACHE
    } elseif ($env:LOCALAPPDATA) {
        $cacheRoot = Join-Path $env:LOCALAPPDATA 'msgvault\build-cache'
    } else {
        $cacheRoot = Join-Path $env:TEMP 'msgvault-build-cache'
    }
    New-Item -ItemType Directory -Path $cacheRoot -Force | Out-Null

    $env:GOOS = 'windows'
    $env:GOARCH = $Architecture
    $env:CGO_ENABLED = '1'

    $tags = @('fts5', 'sqlite_vec')
    $includeDir = Initialize-SqliteHeader $cacheRoot
    Add-CgoFlag 'CGO_CFLAGS' (Format-CgoPathFlag '-I' $includeDir)
    Add-CgoFlag 'CGO_CFLAGS' '-fgnu89-inline'
    Add-CgoFlag 'CGO_LDFLAGS' '-Wl,--allow-multiple-definition'

    if ($Architecture -eq 'arm64') {
        $compilerDirectory = Initialize-Arm64Toolchain $cacheRoot
        $cc = Join-Path $compilerDirectory 'aarch64-w64-mingw32-clang.exe'
        $env:PATH = "$compilerDirectory;$env:PATH"
        $env:CC = Format-GoCompilerCommand $cc

        $duckdbBuild = Initialize-Arm64DuckDB $cacheRoot $compilerDirectory
        $tags += 'duckdb_use_static_lib'
        Add-CgoFlag 'CGO_LDFLAGS' (Format-CgoPathFlag '-L' $duckdbBuild)
        Add-CgoFlag 'CGO_LDFLAGS' '-lduckdb_bundle'
        Add-CgoFlag 'CGO_LDFLAGS' '-lws2_32 -lwsock32 -lrstrtmgr -lstdc++ -lm --static'
    } else {
        $cc = Find-Executable 'gcc.exe' @('C:\msys64\mingw64\bin\gcc.exe')
        if (-not $cc) {
            throw @"
The MinGW-w64 compiler was not found. Install MSYS2, then run:

  C:\msys64\usr\bin\pacman.exe -S --needed mingw-w64-x86_64-toolchain
"@
        }
        $compilerDirectory = Split-Path -Parent $cc
        $env:PATH = "$compilerDirectory;$env:PATH"
        $env:CC = Format-GoCompilerCommand $cc
    }

    $version = (& git describe --tags --always --dirty 2>$null | Out-String).Trim()
    if (-not $version) { $version = 'dev' }
    $commit = (& git rev-parse --short HEAD 2>$null | Out-String).Trim()
    if (-not $commit) { $commit = 'unknown' }
    $buildDate = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

    $ldflags = @(
        "-X go.kenn.io/msgvault/cmd/msgvault/cmd.Version=$version"
        "-X go.kenn.io/msgvault/cmd/msgvault/cmd.Commit=$commit"
        "-X go.kenn.io/msgvault/cmd/msgvault/cmd.BuildDate=$buildDate"
    )
    $buildArguments = @('build', '-tags', ($tags -join ' '))
    if ($Release) {
        $ldflags += @('-s', '-w')
        $buildArguments += '-trimpath'
    }
    $buildArguments += @('-ldflags', ($ldflags -join ' '), '-o', $OutputPath, './cmd/msgvault')

    $configuration = if ($Release) { 'release' } else { 'debug' }
    Write-Host "Building msgvault $version ($commit), windows/$Architecture $configuration..."
    Invoke-Checked go @buildArguments
    Write-Host "Built: $OutputPath" -ForegroundColor Green
} finally {
    foreach ($variable in $buildEnvironmentVariables) {
        [Environment]::SetEnvironmentVariable($variable, $originalBuildEnvironment[$variable], 'Process')
    }
    Pop-Location
}
