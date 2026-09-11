param(
    [Parameter(Mandatory = $true)]
    [string]$Package,

    [ValidateRange(1, 64)]
    [int]$ShardCount = 4,

    [string]$Tags = "",

    [string]$Timeout = "20m"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

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

$goListArgs = @("list", "-f", "{{.Dir}}")
if ($Tags) {
    $goListArgs += @("-tags", $Tags)
}
$goListArgs += $Package

$packageDir = & go @goListArgs
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}
$packageDir = $packageDir.Trim()

$testBinary = Join-Path ([System.IO.Path]::GetTempPath()) (
    "msgvault-tests-{0}-{1}.exe" -f $PID, [guid]::NewGuid().ToString("N")
)

try {
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
    if ($testNames.Count -eq 0) {
        Write-Host "No tests found in $Package"
        exit 0
    }

    $activeShards = [Math]::Min($ShardCount, $testNames.Count)
    $shards = [object[]]::new($activeShards)
    for ($i = 0; $i -lt $activeShards; $i++) {
        $shards[$i] = [System.Collections.Generic.List[string]]::new()
    }
    for ($i = 0; $i -lt $testNames.Count; $i++) {
        $shards[$i % $activeShards].Add($testNames[$i])
    }

    Write-Host "Running $($testNames.Count) tests from $Package in $activeShards shards"

    # Reserve space for the quoted executable, flags, separators, and terminating NUL.
    $patternLimit = 32767 - $testBinary.Length - 128
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

    $failed = $false
    $runs = @(for ($i = 0; $i -lt $activeShards; $i++) {
        [pscustomobject]@{ Batch = 0; Process = $null; StandardOut = $null; StandardErr = $null }
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
                    $elapsed = $run.Process.ExitTime - $run.Process.StartTime
                    $spent[$i] += $elapsed.TotalSeconds
                    $stdout = $run.StandardOut.GetAwaiter().GetResult()
                    $stderr = $run.StandardErr.GetAwaiter().GetResult()
                    if ($run.Process.ExitCode -eq 0) {
                        Write-Host ("ok shard {0}, batch {1}/{2} ({3:N1}s)" -f ($i + 1), ($run.Batch + 1), $batches[$i].Count, $elapsed.TotalSeconds)
                    } else {
                        $failed = $true
                        Write-Error ("shard {0}, batch {1} failed with exit code {2}" -f ($i + 1), ($run.Batch + 1), $run.Process.ExitCode) -ErrorAction Continue
                        if ($stdout) { Write-Output $stdout }
                        if ($stderr) { Write-Error $stderr -ErrorAction Continue }
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
                $startInfo.FileName = $testBinary
                $startInfo.WorkingDirectory = $packageDir
                $startInfo.UseShellExecute = $false
                $startInfo.CreateNoWindow = $true
                $startInfo.RedirectStandardOutput = $true
                $startInfo.RedirectStandardError = $true
                $startInfo.ArgumentList.Add("-test.run=$($batches[$i][$run.Batch])")
                $startInfo.ArgumentList.Add("-test.timeout=$batchTimeout")

                $process = [System.Diagnostics.Process]::new()
                $process.StartInfo = $startInfo
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
    Remove-Item -LiteralPath $testBinary -Force -ErrorAction SilentlyContinue
}
