$ErrorActionPreference = 'Stop'

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$stateDir = Join-Path $root 'runtime\local-claude-test'
$authDir = Join-Path $root 'auths'
$configPath = Join-Path $root 'config.local.yaml'
$relayPath = Join-Path $root 'bin\relay.exe'
$stdoutPath = Join-Path $stateDir 'relay.out.log'
$stderrPath = Join-Path $stateDir 'relay.err.log'
$guardOutPath = Join-Path $stateDir 'guard.out.log'
$guardErrPath = Join-Path $stateDir 'guard.err.log'
$expectedPath = Join-Path $stateDir 'expected-proxies.json'
$pidPath = Join-Path $stateDir 'relay.pid'

New-Item -ItemType Directory -Force -Path $stateDir | Out-Null
if (-not (Test-Path -LiteralPath $configPath)) { throw "Missing local config: $configPath" }
if (-not (Test-Path -LiteralPath $relayPath)) { throw "Missing Relay binary: $relayPath" }

function Get-Sha256([string] $value) {
    $bytes = [Text.Encoding]::UTF8.GetBytes($value)
    return ([BitConverter]::ToString(([Security.Cryptography.SHA256]::Create()).ComputeHash($bytes))).Replace('-', '').ToLowerInvariant()
}

function Get-EgressIp([string] $proxyUrl) {
    $result = & curl.exe --proxy $proxyUrl --silent --show-error --max-time 15 https://api.ipify.org 2>$null
    if ($LASTEXITCODE -ne 0) { throw 'Residential proxy health check failed.' }
    $ip = ($result | Out-String).Trim()
    if ($ip -notmatch '^(?:\d{1,3}\.){3}\d{1,3}$') { throw 'Residential proxy returned an invalid egress IP.' }
    return $ip
}

$previousExpected = @{}
if (Test-Path -LiteralPath $expectedPath) {
    try {
        foreach ($item in (ConvertFrom-Json -InputObject (Get-Content -LiteralPath $expectedPath -Raw))) {
            $previousExpected[[string]$item.file] = $item
        }
    } catch {
        $previousExpected = @{}
    }
}
$expected = @()
Get-ChildItem -LiteralPath $authDir -Filter 'claude-*.json' -File | ForEach-Object {
    $record = Get-Content -LiteralPath $_.FullName -Raw | ConvertFrom-Json
    $proxyUrl = [string] $record.proxy_url
    if ([string]::IsNullOrWhiteSpace($proxyUrl)) { throw "Missing proxy_url in $($_.Name)" }
    $proxyUri = [Uri] $proxyUrl
    if ([string]::IsNullOrWhiteSpace($proxyUri.Host) -or $proxyUri.Port -le 0) { throw "Invalid proxy_url in $($_.Name)" }
    $egressIp = ''
    try { $egressIp = Get-EgressIp $proxyUrl } catch { }
    if ([string]::IsNullOrWhiteSpace($egressIp) -and $previousExpected.ContainsKey($_.Name)) {
        $previous = $previousExpected[$_.Name]
        if ($previous.proxyHash -eq (Get-Sha256 $proxyUrl) -and $previous.proxyHost -eq $proxyUri.Host -and $previous.proxyPort -eq $proxyUri.Port) {
            $egressIp = [string]$previous.egressIp
        }
    }
    $expected += [ordered]@{
        file = $_.Name
        proxyHash = Get-Sha256 $proxyUrl
        proxyHost = $proxyUri.Host
        proxyPort = $proxyUri.Port
        egressIp = $egressIp
    }
}
if ($expected.Count -eq 0) { throw 'No Claude auth files found.' }

$expected | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $expectedPath -Encoding UTF8
Remove-Item -LiteralPath $stdoutPath, $stderrPath, $guardOutPath, $guardErrPath -Force -ErrorAction SilentlyContinue

$relay = Start-Process -FilePath $relayPath `
    -ArgumentList @('--config', $configPath, '--local-model') `
    -WorkingDirectory $root `
    -RedirectStandardOutput $stdoutPath `
    -RedirectStandardError $stderrPath `
    -WindowStyle Hidden `
    -PassThru
$relay.Id | Set-Content -LiteralPath $pidPath -Encoding ASCII

$guard = Start-Process -FilePath (Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe') `
    -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', (Join-Path $PSScriptRoot 'watch.ps1'), '-RelayPid', $relay.Id) `
    -WorkingDirectory $root `
    -RedirectStandardOutput $guardOutPath `
    -RedirectStandardError $guardErrPath `
    -WindowStyle Hidden `
    -PassThru

Write-Output "Relay started: PID $($relay.Id), port 8318"
Write-Output "Proxy guard started: PID $($guard.Id)"
Write-Output "Logs: $stdoutPath and $stderrPath"
