# Smoke test: /healthz must return 200 and /readyz must report status ok, then (when dev login is
# enabled) login -> onboarding -> GET /me. then (B3) a second user joins the first user's room, then (B5a) the owner sends a message the second user lists, then (B5b) both polish entry points end in done or fallback, then (B3b) the second user leaves, and the owner issues a code and dissolves.
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

# B5a flow: while the second user is seated, the owner sends a text message and the second user lists it.
try {
    $msgRoom = $ob.data.room.id
    $cmid = [guid]::NewGuid().ToString()
    $msgBody = @{ client_msg_id = $cmid; kind = 'text'; text = 'Good morning from the smoke test' } | ConvertTo-Json
    $send = Invoke-WebRequest -UseBasicParsing -Method Post -Uri "$base/rooms/$msgRoom/messages" -ContentType 'application/json; charset=utf-8' `
        -Headers $headers -Body ([System.Text.Encoding]::UTF8.GetBytes($msgBody)) -TimeoutSec 5
    if ($send.StatusCode -ne 202) {
        Write-Host "FAIL message send: status $($send.StatusCode), want 202"
        exit 1
    }
    $sent = ($send.Content | ConvertFrom-Json).data
    $list = Invoke-RestMethod -Method Get -Uri "$base/rooms/$msgRoom/messages" -Headers $headers2 -TimeoutSec 5
    $got = @($list.data.items) | Where-Object { $_.id -eq $sent.id }
    if (-not $got) {
        Write-Host "FAIL message list: message $($sent.id) not in the second user's list"
        exit 1
    }
    $status = $got.delivery.translation_status
    if ($status -ne 'pending' -and $status -ne 'done') {
        Write-Host "FAIL message list: translation_status is '$status', want pending or done"
        exit 1
    }
    Write-Host "OK message send -> list (message=$($sent.id), translation_status=$status)"
} catch {
    Write-Host "FAIL message flow: $($_.Exception.Message)"
    exit 1
}

# B5b flow: the owner sends with the polish toggle on; the second user presses polish on another
# message once its translation is done, and both polishes end in done or fallback.
try {
    $waitDelivery = {
        param($messageId, $field, [string[]]$want)
        for ($i = 0; $i -lt 20; $i++) {
            $page = Invoke-RestMethod -Method Get -Uri "$base/rooms/$msgRoom/messages" -Headers $headers2 -TimeoutSec 5
            $m = @($page.data.items) | Where-Object { $_.id -eq $messageId }
            if ($m -and ($want -contains $m.delivery.$field)) { return $m.delivery }
            Start-Sleep -Milliseconds 500
        }
        return $null
    }
    $pBody = @{ client_msg_id = [guid]::NewGuid().ToString(); kind = 'text'; text = 'See you at the station at 7, bring the tickets'; polish = $true } | ConvertTo-Json
    $pSend = Invoke-WebRequest -UseBasicParsing -Method Post -Uri "$base/rooms/$msgRoom/messages" -ContentType 'application/json; charset=utf-8' `
        -Headers $headers -Body ([System.Text.Encoding]::UTF8.GetBytes($pBody)) -TimeoutSec 5
    $pSent = ($pSend.Content | ConvertFrom-Json).data
    if ($pSend.StatusCode -ne 202 -or -not $pSent.polish_requested) {
        Write-Host "FAIL polish toggle: status $($pSend.StatusCode), polish_requested $($pSent.polish_requested)"
        exit 1
    }
    $d1 = & $waitDelivery $pSent.id 'polish_status' @('done', 'fallback')
    if (-not $d1) {
        Write-Host "FAIL polish toggle: polish did not end within 10s (is the worker running?)"
        exit 1
    }

    $d2 = & $waitDelivery $sent.id 'translation_status' @('done')
    if (-not $d2) {
        Write-Host "FAIL polish button: translation of $($sent.id) not done within 10s"
        exit 1
    }
    $btn = Invoke-WebRequest -UseBasicParsing -Method Post -Uri "$base/deliveries/$($d2.id)/polish" -Headers $headers2 -TimeoutSec 5
    if ($btn.StatusCode -ne 202) {
        Write-Host "FAIL polish button: status $($btn.StatusCode), want 202"
        exit 1
    }
    $d3 = & $waitDelivery $sent.id 'polish_status' @('done', 'fallback')
    if (-not $d3) {
        Write-Host "FAIL polish button: polish did not end within 10s"
        exit 1
    }
    Write-Host "OK polish (toggle=$($d1.polish_status), button=$($d3.polish_status))"
} catch {
    Write-Host "FAIL polish flow: $($_.Exception.Message)"
    exit 1
}

# B3b flow: the second user leaves, then the first user (the owner) issues a code and dissolves.
try {
    $roomId = $ob.data.room.id
    $leave = Invoke-WebRequest -UseBasicParsing -Method Post -Uri "$base/rooms/$roomId/leave" -Headers $headers2 -TimeoutSec 5
    if ($leave.StatusCode -ne 204) {
        Write-Host "FAIL leave: status $($leave.StatusCode), want 204"
        exit 1
    }
    $mine2 = Invoke-RestMethod -Method Get -Uri "$base/rooms/mine" -Headers $headers2 -TimeoutSec 5
    if (@($mine2.data).Count -ne 1) {
        Write-Host "FAIL leave: /rooms/mine still lists $(@($mine2.data).Count) rooms, want 1"
        exit 1
    }
    $regen = Invoke-RestMethod -Method Post -Uri "$base/rooms/$roomId/codes" -Headers $headers -TimeoutSec 5
    if (-not $regen.data.code) {
        Write-Host "FAIL regen: no code returned"
        exit 1
    }
    $dissolve = Invoke-RestMethod -Method Post -Uri "$base/rooms/$roomId/dissolve" -Headers $headers -TimeoutSec 5
    if ($dissolve.data.room.member_count -ne 1) {
        Write-Host "FAIL dissolve: member_count is $($dissolve.data.room.member_count), want 1"
        exit 1
    }
    Write-Host "OK leave -> regen -> dissolve ($email2 left, owner issued a code and dissolved)"
} catch {
    Write-Host "FAIL room management flow: $($_.Exception.Message)"
    exit 1
}
