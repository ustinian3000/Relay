param(
    [switch] $Stop
)
$ErrorActionPreference = 'Stop'

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$stateDir = Join-Path $root 'runtime\local-claude-test'
$authDir = Join-Path $root 'auths'
$configPath = Join-Path $root 'config.local.yaml'
$relayPath = Join-Path $root 'bin\relay.exe'
$stdoutPath = Join-Path $stateDir 'relay.out.log'
$stderrPath = Join-Path $stateDir 'relay.err.log'
$guardLogPath = Join-Path $stateDir 'guard.log'
$expectedPath = Join-Path $stateDir 'expected-proxies.json'
$pidPath = Join-Path $stateDir 'relay.pid'
$failurePath = Join-Path $stateDir 'guard-failure.txt'

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
function Stop-Relay([int] $processId) {
    $process = Get-Process -Id $processId -ErrorAction SilentlyContinue
    if ($null -ne $process) { Stop-Process -Id $processId -Force -ErrorAction SilentlyContinue }
}

if ($Stop) {
    if (Test-Path -LiteralPath $pidPath) { Stop-Relay ([int](Get-Content -LiteralPath $pidPath)) }
    exit 0
}

New-Item -ItemType Directory -Force -Path $stateDir | Out-Null
if (-not (Test-Path -LiteralPath $configPath)) { throw "Missing local config: $configPath" }
if (-not (Test-Path -LiteralPath $relayPath)) { throw "Missing Relay binary: $relayPath" }

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
    $proxyUri = [Uri] $proxyUrl
    if ([string]::IsNullOrWhiteSpace($proxyUrl) -or [string]::IsNullOrWhiteSpace($proxyUri.Host) -or $proxyUri.Port -le 0) {
        throw "Invalid proxy_url in $($_.Name)"
    }
    $egressIp = ''
    try { $egressIp = Get-EgressIp $proxyUrl } catch { }
    if ([string]::IsNullOrWhiteSpace($egressIp) -and $previousExpected.ContainsKey($_.Name)) {
        $previous = $previousExpected[$_.Name]
        if ($previous.proxyHash -eq (Get-Sha256 $proxyUrl) -and $previous.proxyHost -eq $proxyUri.Host -and $previous.proxyPort -eq $proxyUri.Port) {
            $egressIp = [string]$previous.egressIp
        }
    }
    $expected += [ordered]@{ file = $_.Name; proxyHash = Get-Sha256 $proxyUrl; proxyHost = $proxyUri.Host; proxyPort = $proxyUri.Port; egressIp = $egressIp }
}
if ($expected.Count -eq 0) { throw 'No Claude auth files found.' }
$expected | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $expectedPath -Encoding UTF8
Remove-Item -LiteralPath $stdoutPath, $stderrPath, $guardLogPath, $failurePath -Force -ErrorAction SilentlyContinue

$relay = Start-Process -FilePath $relayPath -ArgumentList @('--config', $configPath, '--local-model') -WorkingDirectory $root -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath -WindowStyle Hidden -PassThru
$relay.Id | Set-Content -LiteralPath $pidPath -Encoding ASCII
"Started Relay PID $($relay.Id)" | Set-Content -LiteralPath $guardLogPath -Encoding UTF8

try {
    while ($true) {
        if ($null -eq (Get-Process -Id $relay.Id -ErrorAction SilentlyContinue)) { break }
        try {
            $current = ConvertFrom-Json -InputObject (Get-Content -LiteralPath $expectedPath -Raw)
            foreach ($item in $current) {
                $path = Join-Path -Path $authDir -ChildPath ([string]$item.file)
                $record = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
                $proxyUrl = [string] $record.proxy_url
                $proxyUri = [Uri] $proxyUrl
                if ((Get-Sha256 $proxyUrl) -ne $item.proxyHash -or $proxyUri.Host -ne $item.proxyHost -or $proxyUri.Port -ne $item.proxyPort) { throw "Proxy binding changed: $($item.file)" }
                $currentEgress = Get-EgressIp $proxyUrl
                if (-not [string]::IsNullOrWhiteSpace([string]$item.egressIp) -and $currentEgress -ne $item.egressIp) { throw "Proxy egress changed: $($item.file)" }
            }
        } catch {
            $_.Exception.Message | Set-Content -LiteralPath $failurePath -Encoding UTF8

        }
        Start-Sleep -Seconds 30
    }
} finally {
    Remove-Item -LiteralPath $pidPath -Force -ErrorAction SilentlyContinue
}
