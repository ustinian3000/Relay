param(
    [Parameter(Mandatory = $true)]
    [int] $RelayPid
)
$ErrorActionPreference = 'Stop'

$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$stateDir = Join-Path $root 'runtime\local-claude-test'
$authDir = Join-Path $root 'auths'
$expectedPath = Join-Path $stateDir 'expected-proxies.json'
$failurePath = Join-Path $stateDir 'guard-failure.txt'
$disabledDir = Join-Path $stateDir 'disabled-auths'

function Get-Sha256([string] $value) {
    $bytes = [Text.Encoding]::UTF8.GetBytes($value)
    return ([BitConverter]::ToString(([Security.Cryptography.SHA256]::Create()).ComputeHash($bytes))).Replace('-', '').ToLowerInvariant()
}

function Disable-Auth([object] $item, [string] $reason) {
    $path = Join-Path $authDir ([string]$item.file)
    if (-not (Test-Path -LiteralPath $path)) { return }
    New-Item -ItemType Directory -Force -Path $disabledDir | Out-Null
    $backup = Join-Path $disabledDir ([string]$item.file)
    if (-not (Test-Path -LiteralPath $backup)) {
        Copy-Item -LiteralPath $path -Destination $backup -Force
    }
    $record = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
    if ($record.disabled -ne $true) {
        $record | Add-Member -NotePropertyName disabled -NotePropertyValue $true -Force
        $json = $record | ConvertTo-Json -Depth 100
        [IO.File]::WriteAllText($path, $json, [Text.UTF8Encoding]::new($false))
    }
    "$(Get-Date -Format o) $($item.file): proxy unavailable; auth disabled without stopping Relay ($reason)" | Add-Content -LiteralPath $failurePath -Encoding UTF8
}

function Restore-Auth([object] $item) {
    $path = Join-Path $authDir ([string]$item.file)
    $backup = Join-Path $disabledDir ([string]$item.file)
    if (-not (Test-Path -LiteralPath $backup)) { return }
    if (-not (Test-Path -LiteralPath $path)) { return }
    $record = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
    if ((Get-Sha256 ([string]$record.proxy_url)) -ne $item.proxyHash) {
        "$(Get-Date -Format o) $($item.file): proxy binding changed while disabled; left disabled" | Add-Content -LiteralPath $failurePath -Encoding UTF8
        return
    }
    Copy-Item -LiteralPath $backup -Destination $path -Force
    Remove-Item -LiteralPath $backup -Force
}

function Get-EgressIp([string] $proxyUrl) {
    $result = & curl.exe --proxy $proxyUrl --silent --show-error --max-time 15 https://api.ipify.org 2>$null
    if ($LASTEXITCODE -ne 0) { throw 'Residential proxy health check failed.' }
    $ip = ($result | Out-String).Trim()
    if ($ip -notmatch '^(?:\d{1,3}\.){3}\d{1,3}$') { throw 'Residential proxy returned an invalid egress IP.' }
    return $ip
}

$expected = ConvertFrom-Json -InputObject (Get-Content -LiteralPath $expectedPath -Raw)
while ($true) {
    $failures = @()
    foreach ($item in $expected) {
        try {
            $path = Join-Path -Path $authDir -ChildPath ([string]$item.file)
            $record = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
            $proxyUrl = [string] $record.proxy_url
            $proxyUri = [Uri] $proxyUrl
            if ((Get-Sha256 $proxyUrl) -ne $item.proxyHash -or $proxyUri.Host -ne $item.proxyHost -or $proxyUri.Port -ne $item.proxyPort) {
                throw "Proxy binding changed: $($item.file)"
            }
            $currentEgress = Get-EgressIp $proxyUrl
            if ([string]::IsNullOrWhiteSpace([string]$item.egressIp)) {
                $item.egressIp = $currentEgress
                $expected | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $expectedPath -Encoding UTF8
            } elseif ($currentEgress -ne $item.egressIp) {
                throw "Proxy egress changed: $($item.file)"
            }
            Restore-Auth $item
        } catch {
            $failures += $_.Exception.Message
            Disable-Auth $item $_.Exception.Message
        }
    }
    if ($failures.Count -eq 0) {
        Remove-Item -LiteralPath $failurePath -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Seconds 30
}
