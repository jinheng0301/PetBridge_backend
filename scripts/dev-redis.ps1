# Starts the local Memurai (Redis-compatible) server if it is not already running.
# Memurai is not installed as a service on this machine, so it must be started by hand
# and does not survive a reboot.
$ErrorActionPreference = 'Stop'

$root = if ($env:MEMURAI_DIR) { $env:MEMURAI_DIR } else { 'C:\Users\user\tools\memurai' }
$cli  = Join-Path $root 'Memurai\memurai-cli.exe'
$exe  = Join-Path $root 'Memurai\memurai.exe'
$data = Join-Path $root 'data'

function Test-Redis {
    try { return ((& $cli -p 6379 ping 2>$null) -eq 'PONG') } catch { return $false }
}

if (Test-Redis) {
    Write-Host 'Redis (Memurai) is already running.'
    exit 0
}

New-Item -ItemType Directory -Force $data | Out-Null
Start-Process -WindowStyle Hidden -FilePath $exe -ArgumentList @(
    '--port', '6379', '--bind', '127.0.0.1', '--save', '""', '--appendonly', 'no', '--dir', $data
)

for ($i = 0; $i -lt 10; $i++) {
    Start-Sleep -Milliseconds 500
    if (Test-Redis) {
        Write-Host 'Redis (Memurai) started.'
        exit 0
    }
}
Write-Error 'Redis (Memurai) did not answer PONG within 5 seconds.'
exit 1
