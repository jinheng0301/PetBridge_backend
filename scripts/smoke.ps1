# Smoke test: /healthz must return 200 and /readyz must report status ok, then (when dev login is
# enabled) login -> onboarding -> GET /me. then (B3) a second user joins the first user's room.
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

# B3 flow: a second temp user joins the first user's room with its signup code.
try {
    $email2 = "smoke-$([guid]::NewGuid().ToString('N').Substring(0,8))@local"
    $login2 = Invoke-RestMethod -Method Post -Uri "$base/auth/login/dev" -ContentType 'application/json' `
        -Body (@{ email = $email2 } | ConvertTo-Json) -TimeoutSec 5
    $headers2 = @{ Authorization = "Bearer $($login2.data.access_token)" }
    $body2 = @{ native_lang = 'ko'; mbti = 'INFP'; nickname = 'Smoke2'; age_band = '18_plus'; tz = 'Asia/Seoul' } | ConvertTo-Json
    Invoke-RestMethod -Method Post -Uri "$base/me/onboarding" -ContentType 'application/json' -Headers $headers2 -Body $body2 -TimeoutSec 5 | Out-Null

    $joinBody = @{ code = $ob.data.invite_code.code } | ConvertTo-Json
    $join = Invoke-RestMethod -Method Post -Uri "$base/rooms/join" -ContentType 'application/json' -Headers $headers2 -Body $joinBody -TimeoutSec 5
    if ($join.data.room.member_count -ne 2) {
        Write-Host "FAIL join: member_count is $($join.data.room.member_count), want 2"
        exit 1
    }
    if (-not ($join.data.rewards.Count -gt 0)) {
        Write-Host "FAIL join: no reward codes returned"
        exit 1
    }
    $mine = Invoke-RestMethod -Method Get -Uri "$base/rooms/mine" -Headers $headers2 -TimeoutSec 5
    $rooms = @($mine.data)
    if ($rooms.Count -ne 2 -or $rooms[0].is_owner -ne $true) {
        Write-Host "FAIL /rooms/mine unexpected: $($mine.data | ConvertTo-Json -Compress -Depth 5)"
        exit 1
    }
    Write-Host "OK join -> rooms/mine ($email2, member_count=$($join.data.room.member_count), rewards=$(@($join.data.rewards).Count), rooms=$($rooms.Count))"
} catch {
    Write-Host "FAIL join flow: $($_.Exception.Message)"
    exit 1
}

