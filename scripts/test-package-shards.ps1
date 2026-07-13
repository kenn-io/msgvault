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

    $runs = for ($i = 0; $i -lt $activeShards; $i++) {
        $escapedNames = $shards[$i] | ForEach-Object { [regex]::Escape($_) }
        $pattern = "^(" + ($escapedNames -join "|") + ")$"

        $startInfo = [System.Diagnostics.ProcessStartInfo]::new()
        $startInfo.FileName = $testBinary
        $startInfo.WorkingDirectory = $packageDir
        $startInfo.UseShellExecute = $false
        $startInfo.RedirectStandardOutput = $true
        $startInfo.RedirectStandardError = $true
        $startInfo.ArgumentList.Add("-test.run=$pattern")
        $startInfo.ArgumentList.Add("-test.timeout=$Timeout")

        $process = [System.Diagnostics.Process]::new()
        $process.StartInfo = $startInfo
        if (-not $process.Start()) {
            throw "Failed to start test shard $i"
        }

        [pscustomobject]@{
            Index       = $i
            Count       = $shards[$i].Count
            Process     = $process
            StandardOut = $process.StandardOutput.ReadToEndAsync()
            StandardErr = $process.StandardError.ReadToEndAsync()
        }
    }

    $failed = $false
    foreach ($run in $runs) {
        $run.Process.WaitForExit()
        $elapsed = $run.Process.ExitTime - $run.Process.StartTime
        $stdout = $run.StandardOut.GetAwaiter().GetResult()
        $stderr = $run.StandardErr.GetAwaiter().GetResult()

        if ($run.Process.ExitCode -eq 0) {
            Write-Host ("ok shard {0}: {1} tests ({2:N1}s)" -f (
                $run.Index + 1
            ), $run.Count, $elapsed.TotalSeconds)
        } else {
            $failed = $true
            Write-Error ("shard {0} failed with exit code {1}" -f (
                $run.Index + 1
            ), $run.Process.ExitCode) -ErrorAction Continue
            if ($stdout) {
                Write-Output $stdout
            }
            if ($stderr) {
                Write-Error $stderr -ErrorAction Continue
            }
        }

        $run.Process.Dispose()
    }

    if ($failed) {
        exit 1
    }
} finally {
    Remove-Item -LiteralPath $testBinary -Force -ErrorAction SilentlyContinue
}
