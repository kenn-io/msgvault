param(
    [Parameter(Mandatory = $true)]
    [string]$Package,

    [ValidateRange(1, 64)]
    [int]$ShardCount = 4,

    [string]$Tags = "",

    [string]$Timeout = "20m",

    # Runs only part PartIndex of PartCount so several machines can split one package.
    [ValidateRange(1, 64)]
    [int]$PartCount = 1,

    [ValidateRange(1, 64)]
    [int]$PartIndex = 1,

    # Names gotestsum/<Suite>.json under the current directory, which collects
    # every batch's test2json events for CI to upload as an artifact.
    # Defaults to the package's last path element.
    [string]$Suite = ""
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

if ($PartIndex -gt $PartCount) {
    throw "PartIndex $PartIndex exceeds PartCount $PartCount"
}

# Go accepts compound durations, including fractional units and zero to disable the timeout.
$units = @{ ns = 1e-9; us = 1e-6; 'µs' = 1e-6; ms = 1e-3; s = 1; m = 60; h = 3600 }
if ($Timeout -cnotmatch '^[+-]?(?:(?:\d+(?:\.\d*)?|\.\d+)(?:ns|us|µs|μs|ms|s|m|h))+$|^[+-]?0$') {
    throw "Invalid Go test timeout: $Timeout"
}
$timeoutSeconds = 0.0
foreach ($part in [regex]::Matches($Timeout, '(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)')) {
    $timeoutSeconds += [double]::Parse($part.Groups[1].Value, [cultureinfo]::InvariantCulture) * $units[$part.Groups[2].Value]
}
if ($Timeout.StartsWith('-')) { $timeoutSeconds = -$timeoutSeconds }

# Import paths contain no spaces; the directory may.
$goListArgs = @("list", "-f", "{{.ImportPath}} {{.Dir}}")
if ($Tags) {
    $goListArgs += @("-tags", $Tags)
}
$goListArgs += $Package

$packageInfo = & go @goListArgs
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}
$importPath, $packageDir = $packageInfo.Trim() -split ' ', 2
if (-not $Suite) { $Suite = ($importPath -split '/')[-1] }
$suiteJson = Join-Path (Get-Location) "gotestsum/$Suite.json"

$scratch = Join-Path ([System.IO.Path]::GetTempPath()) (
    "msgvault-tests-{0}-{1}" -f $PID, [guid]::NewGuid().ToString("N")
)
New-Item -ItemType Directory -Path $scratch | Out-Null
$testBinary = Join-Path $scratch "tests.exe"
$gotestsum = Join-Path $scratch "gotestsum.exe"
$test2json = Join-Path $scratch "test2json.exe"

try {
    # gotestsum is a tool dependency of this module, which the package under
    # test need not belong to. test2json turns each batch's -test.v=test2json
    # output into the events go test -json prints; gotestsum records them and
    # prints them as verbose test output.
    & go -C $PSScriptRoot build -o $gotestsum gotest.tools/gotestsum
    if ($LASTEXITCODE -ne 0) {
        exit $LASTEXITCODE
    }
    & go -C $PSScriptRoot build -o $test2json cmd/test2json
    if ($LASTEXITCODE -ne 0) {
        exit $LASTEXITCODE
    }

    $compileArgs = @("test", "-c", "-o", $testBinary)
    if ($Tags) {
        $compileArgs += @("-tags", $Tags)
    }
    $compileArgs += $Package

    & go @compileArgs
    if ($LASTEXITCODE -ne 0) {
        exit $LASTEXITCODE
    }

    $testNames = @(& $testBinary "-test.list=^(Test|Example|Fuzz)")
    if ($LASTEXITCODE -ne 0) {
        exit $LASTEXITCODE
    }
    if ($PartCount -gt 1) {
        $testNames = @(for ($i = $PartIndex - 1; $i -lt $testNames.Count; $i += $PartCount) { $testNames[$i] })
        Write-Host "Running part $PartIndex of $PartCount"
    }
    if ($testNames.Count -eq 0) {
        Write-Host "No tests found in $Package"
        exit 0
    }

    # Windows limits a process command line to 32,767 characters. Increase the
    # shard count when the test-name patterns need more room, leaving space for
    # the executable path and the other test flags.
    $maxPatternCharacters = 24000
    $patternCharacters = 3
    foreach ($testName in $testNames) {
        $patternCharacters += [regex]::Escape($testName).Length + 1
    }
    $requiredShards = [int][Math]::Ceiling($patternCharacters / $maxPatternCharacters)
    $activeShards = [Math]::Min([Math]::Max($ShardCount, $requiredShards), $testNames.Count)
    while ($true) {
        $shards = [object[]]::new($activeShards)
        for ($i = 0; $i -lt $activeShards; $i++) {
            $shards[$i] = [System.Collections.Generic.List[string]]::new()
        }
        for ($i = 0; $i -lt $testNames.Count; $i++) {
            $shards[$i % $activeShards].Add($testNames[$i])
        }

        $largestPatternCharacters = 0
        foreach ($shard in $shards) {
            $shardPatternCharacters = 3
            foreach ($testName in $shard) {
                $shardPatternCharacters += [regex]::Escape($testName).Length + 1
            }
            $largestPatternCharacters = [Math]::Max($largestPatternCharacters, $shardPatternCharacters)
        }
        if ($largestPatternCharacters -le $maxPatternCharacters) {
            break
        }
        if ($activeShards -eq $testNames.Count) {
            throw "A test name in $Package exceeds the $maxPatternCharacters-character shard limit"
        }
        $activeShards++
    }

    Write-Host "Running $($testNames.Count) tests from $Package in $activeShards shards"

    # Reserve space for the quoted executables, batch JSON path, flags, separators, and terminating NUL.
    $patternLimit = 32767 - $gotestsum.Length - $test2json.Length - $testBinary.Length - $scratch.Length - $importPath.Length - 256
    $batches = [object[]]::new($activeShards)
    $spent = [double[]]::new($activeShards)
    for ($i = 0; $i -lt $activeShards; $i++) {
        $batches[$i] = [System.Collections.Generic.List[string]]::new()
        $pattern = '^('
        foreach ($name in $shards[$i]) {
            $escaped = [regex]::Escape($name)
            if ($escaped.Length + 4 -gt $patternLimit) { throw "Test name exceeds the Windows command-line limit: $name" }
            if ($pattern.Length + $escaped.Length + 3 -gt $patternLimit) {
                $batches[$i].Add($pattern + ')$')
                $pattern = '^('
            }
            if ($pattern.Length -gt 2) { $pattern += '|' }
            $pattern += $escaped
        }
        $batches[$i].Add($pattern + ')$')
    }

    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $suiteJson) | Out-Null
    [System.IO.File]::WriteAllText($suiteJson, '')
    $failed = $false
    $runs = @(for ($i = 0; $i -lt $activeShards; $i++) {
        [pscustomobject]@{ Batch = 0; Process = $null; Timer = $null; StandardOut = $null; StandardErr = $null }
    })
    try {
        do {
            $active = $false
            for ($i = 0; $i -lt $activeShards; $i++) {
                $run = $runs[$i]
                if ($run.Process) {
                    if (-not $run.Process.HasExited) {
                        $active = $true
                        continue
                    }
                    $run.Timer.Stop()
                    $elapsed = $run.Timer.Elapsed
                    $spent[$i] += $elapsed.TotalSeconds
                    $stdout = $run.StandardOut.GetAwaiter().GetResult()
                    $stderr = $run.StandardErr.GetAwaiter().GetResult()
                    $batchJson = Join-Path $scratch "shard$i-batch$($run.Batch).json"
                    if (Test-Path -LiteralPath $batchJson) {
                        [System.IO.File]::AppendAllText($suiteJson, [System.IO.File]::ReadAllText($batchJson))
                        Remove-Item -LiteralPath $batchJson
                    }
                    # Each batch's results end with the package line go test prints,
                    # so a reader of the log can tell which package they belong to.
                    $duration = $elapsed.TotalSeconds.ToString('F3', [cultureinfo]::InvariantCulture) + 's'
                    if ($run.Process.ExitCode -eq 0) {
                        $stdout -split '\r?\n' | Where-Object { $_ -match '^\s*--- (PASS|SKIP): ' } | ForEach-Object { Write-Host $_ }
                        Write-Host "ok  `t$importPath`t$duration"
                        Write-Host ("ok shard {0}, batch {1}/{2} ({3:N1}s)" -f ($i + 1), ($run.Batch + 1), $batches[$i].Count, $elapsed.TotalSeconds)
                    } else {
                        $failed = $true
                        Write-Error ("shard {0}, batch {1} failed with exit code {2}" -f ($i + 1), ($run.Batch + 1), $run.Process.ExitCode) -ErrorAction Continue
                        if ($stdout) { Write-Output $stdout }
                        if ($stderr) { Write-Error $stderr -ErrorAction Continue }
                        Write-Output "FAIL`t$importPath`t$duration"
                    }
                    $run.Process.Dispose()
                    $run.Process = $null
                    $run.Batch++
                }
                if ($run.Batch -ge $batches[$i].Count) { continue }
                $batchTimeout = '0'
                if ($timeoutSeconds -gt 0) {
                    $remaining = $timeoutSeconds - $spent[$i]
                    if ($remaining -le 0) {
                        $failed = $true
                        Write-Error "shard $($i + 1) exhausted its $Timeout timeout" -ErrorAction Continue
                        $run.Batch = $batches[$i].Count
                        continue
                    }
                    $batchTimeout = $remaining.ToString('F9', [cultureinfo]::InvariantCulture) + 's'
                }
                $startInfo = [System.Diagnostics.ProcessStartInfo]::new()
                $startInfo.FileName = $gotestsum
                $startInfo.WorkingDirectory = $packageDir
                $startInfo.UseShellExecute = $false
                $startInfo.CreateNoWindow = $true
                $startInfo.RedirectStandardOutput = $true
                $startInfo.RedirectStandardError = $true
                foreach ($argument in @(
                    '--format', 'standard-verbose', '--format-icons', 'text',
                    '--jsonfile', (Join-Path $scratch "shard$i-batch$($run.Batch).json"),
                    '--raw-command', '--', $test2json, '-t', '-p', $importPath,
                    $testBinary, '-test.v=test2json'
                )) {
                    $startInfo.ArgumentList.Add($argument)
                }
                $startInfo.ArgumentList.Add("-test.run=$($batches[$i][$run.Batch])")
                $startInfo.ArgumentList.Add("-test.timeout=$batchTimeout")

                $process = [System.Diagnostics.Process]::new()
                $process.StartInfo = $startInfo
                $run.Timer = [System.Diagnostics.Stopwatch]::StartNew()
                if (-not $process.Start()) { throw "Failed to start test shard $i" }
                $run.Process = $process
                $run.StandardOut = $process.StandardOutput.ReadToEndAsync()
                $run.StandardErr = $process.StandardError.ReadToEndAsync()
                $active = $true
            }
            if ($active) { Start-Sleep -Milliseconds 100 }
        } while ($active)
    } finally {
        foreach ($run in $runs) {
            if ($run.Process) {
                if (-not $run.Process.HasExited) { $run.Process.Kill($true) }
                $run.Process.WaitForExit()
                $run.Process.Dispose()
            }
        }
    }
    if ($failed) {
        exit 1
    }
} finally {
    Remove-Item -LiteralPath $scratch -Recurse -Force -ErrorAction SilentlyContinue
}
