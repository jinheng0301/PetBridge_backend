# Smoke test: /healthz must return 200 and /readyz must report status ok, then (when dev login is
# enabled) login -> onboarding -> GET /me. Later batches add join room -> send message.
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
        # "degraded" (Redis down or misconfigured) is also HTTP 200, so read the body.
        if ($path -eq '/readyz' -and ($r.Content | ConvertFrom-Json).data.status -ne 'ok') {
            Write-Host "FAIL $url : status is not ok (is Redis running? try: task up)"
            $failed = $true
        }
    } catch {
        Write-Host "FAIL $url : $($_.Exception.Message)"
        $failed = $true
    }
}
if ($failed) { exit 1 }

# B2 flow: needs DEV_LOGIN_ENABLED=true (APP_ENV=local).
$base = "http://localhost:$port/api/v1"
$email = "smoke-$([guid]::NewGuid().ToString('N').Substring(0,8))@local"
try {
    $login = Invoke-RestMethod -Method Post -Uri "$base/auth/login/dev" -ContentType 'application/json' `
        -Body (@{ email = $email } | ConvertTo-Json) -TimeoutSec 5
    $headers = @{ Authorization = "Bearer $($login.data.access_token)" }
    $body = @{ native_lang = 'ja'; mbti = 'ENFP'; nickname = 'Smoke'; age_band = '18_plus'; tz = 'Asia/Tokyo' } | ConvertTo-Json
    $ob = Invoke-RestMethod -Method Post -Uri "$base/me/onboarding" -ContentType 'application/json' -Headers $headers -Body $body -TimeoutSec 5
    $me = Invoke-RestMethod -Method Get -Uri "$base/me" -Headers $headers -TimeoutSec 5
    if (-not $me.data.onboarded -or $me.data.pet_type -ne 'happy_shiba') {
        Write-Host "FAIL /me unexpected: $($me.data | ConvertTo-Json -Compress)"
        exit 1
    }
    Write-Host "OK login -> onboarding -> /me ($email, pet=$($me.data.pet_type), invite=$($ob.data.invite_code.code))"
} catch {
    Write-Host "FAIL auth flow: $($_.Exception.Message)"
    exit 1
}
