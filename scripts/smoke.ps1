# Smoke test: /healthz and /readyz must both return 200.
# From B2 on this grows into: login -> onboarding -> join room -> send message.
$ErrorActionPreference = 'Stop'

# Port: APP_PORT from the environment, else from .env, else 8080.
$port = $env:APP_PORT
if (-not $port -and (Test-Path .env)) {
    $line = Select-String -Path .env -Pattern '^\s*APP_PORT\s*=\s*(\d+)' | Select-Object -First 1
    if ($line) { $port = $line.Matches[0].Groups[1].Value }
}
if (-not $port) { $port = '8080' }

$failed = $false
foreach ($path in '/healthz', '/readyz') {
    $url = "http://localhost:$port$path"
    try {
        $r = Invoke-WebRequest -UseBasicParsing -Uri $url -TimeoutSec 5
        Write-Host "$($r.StatusCode) $url $($r.Content)"
        if ($r.StatusCode -ne 200) { $failed = $true }
    } catch {
        Write-Host "FAIL $url : $($_.Exception.Message)"
        $failed = $true
    }
}
if ($failed) { exit 1 }
