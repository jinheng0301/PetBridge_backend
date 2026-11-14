# Checks that the local PostgreSQL Windows service is running.
$svc = Get-Service -Name 'postgresql-x64-16' -ErrorAction SilentlyContinue
if (-not $svc -or $svc.Status -ne 'Running') {
    Write-Error 'PostgreSQL service postgresql-x64-16 is not running.'
    exit 1
}
Write-Host 'PostgreSQL is running.'
