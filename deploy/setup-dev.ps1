# EveSynapse local-development setup for Windows.
#
# The dev counterpart of deploy/setup.sh: where that script installs
# a production server on Linux, this one prepares a Windows laptop
# to run EveSynapse locally for development. It makes sure the
# PostgreSQL service is running, creates the evesynapse role and
# database (with a generated password), writes a dev .env at the
# repo root (DATABASE_URL, a generated TOKEN_ENCRYPTION_KEY, and
# DEV_LOGIN=1 so /dev-login works), decodes the embedded assets
# (the `make assets` step), and builds evesynapse-dev.exe.
#
# Re-running it is safe: an existing .env is never overwritten, the
# database pieces are only created when missing, and the binary is
# simply rebuilt.
#
# Usage (from the repo root, in PowerShell):
#   powershell -ExecutionPolicy Bypass -File deploy\setup-dev.ps1
#   .\deploy\setup-dev.ps1 -SkipBuild        # everything but the build
#
# You will be asked for the PostgreSQL superuser password you set
# when you installed PostgreSQL (only when the database still has
# to be created). Starting a stopped PostgreSQL service needs an
# elevated (Administrator) PowerShell; the script says so if it
# comes to that.

param(
    [switch]$SkipBuild
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $RepoRoot

function Die($msg) { Write-Host "setup-dev: $msg" -ForegroundColor Red; exit 1 }

function New-HexSecret($bytes) {
    $buf = New-Object byte[] $bytes
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($buf)
    return ($buf | ForEach-Object { $_.ToString("x2") }) -join ""
}

# --- 0. Tools -------------------------------------------------------------
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Die "Go is not on PATH. Install it from https://go.dev/dl/ and run this again."
}
Write-Host "Go: $(go version)"

$psql = Get-Command psql -ErrorAction SilentlyContinue
if (-not $psql) {
    $found = Get-ChildItem "C:\Program Files\PostgreSQL\*\bin\psql.exe" -ErrorAction SilentlyContinue |
        Sort-Object FullName -Descending | Select-Object -First 1
    if ($found) { $psql = $found.FullName } else {
        Die "psql not found. Install PostgreSQL 16 (https://www.postgresql.org/download/windows/) and run this again."
    }
} else { $psql = $psql.Source }
Write-Host "psql: $psql"

# --- 1. PostgreSQL service -------------------------------------------------
$svc = Get-Service -Name "postgresql*" -ErrorAction SilentlyContinue | Select-Object -First 1
if ($svc -and $svc.Status -ne "Running") {
    try {
        Start-Service $svc.Name
        Write-Host "Started the PostgreSQL service ($($svc.Name))."
    } catch {
        Die "PostgreSQL is installed but its service ($($svc.Name)) is stopped and could not be started. Re-run this script from an elevated (Administrator) PowerShell."
    }
}

# --- 2. Role + database (skipped when .env already names a database) ------
$envFile = Join-Path $RepoRoot ".env"
$envHasDb = (Test-Path $envFile) -and (Select-String -Path $envFile -Pattern "^DATABASE_URL=" -Quiet)
$dbPassword = $null
if ($envHasDb) {
    Write-Host "Your .env already has a DATABASE_URL; leaving the database alone."
} else {
    $secure = Read-Host "PostgreSQL superuser (postgres) password" -AsSecureString
    $env:PGPASSWORD = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
        [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure))
    try {
        & $psql -U postgres -tAc "SELECT 1" | Out-Null
        if ($LASTEXITCODE -ne 0) { Die "could not connect to PostgreSQL as the postgres superuser (wrong password?)." }
        $dbPassword = New-HexSecret 24
        $sql = @"
DO `$`$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'evesynapse') THEN
    CREATE ROLE evesynapse LOGIN PASSWORD '$dbPassword';
  ELSE
    ALTER ROLE evesynapse PASSWORD '$dbPassword';
  END IF;
END
`$`$;
"@
        $sql | & $psql -U postgres -v ON_ERROR_STOP=1 -q | Out-Null
        if ($LASTEXITCODE -ne 0) { Die "could not create the evesynapse database role." }
        $exists = & $psql -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname = 'evesynapse'"
        if ($exists -notmatch "1") {
            & $psql -U postgres -c "CREATE DATABASE evesynapse OWNER evesynapse" | Out-Null
            if ($LASTEXITCODE -ne 0) { Die "could not create the evesynapse database." }
        }
        Write-Host "PostgreSQL ready: database 'evesynapse' owned by role 'evesynapse'."
    } finally {
        Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
    }
}

# --- 3. The dev .env (an existing one is never overwritten) ----------------
if (Test-Path $envFile) {
    Write-Host "Keeping your existing .env."
    if (-not (Select-String -Path $envFile -Pattern "^DEV_LOGIN=" -Quiet)) {
        Write-Host "NOTE: add DEV_LOGIN=1 to .env if you want the /dev-login shortcut."
    }
} else {
    Copy-Item (Join-Path $RepoRoot ".env.example") $envFile
    Add-Content $envFile ""
    Add-Content $envFile "# Encrypts the EVE login tokens stored in the database (generated by setup-dev)."
    Add-Content $envFile "TOKEN_ENCRYPTION_KEY=$(New-HexSecret 32)"
    Add-Content $envFile ""
    Add-Content $envFile "# DEV ONLY: enables the /dev-login route of evesynapse-dev (instant fake"
    Add-Content $envFile "# signed-in session for local development). Never enable in production."
    Add-Content $envFile "DEV_LOGIN=1"
    Write-Host "Wrote .env with dev defaults."
}
if ($dbPassword -and -not $envHasDb) {
    Add-Content $envFile ""
    Add-Content $envFile "# Postgres connection (generated by setup-dev; the password is only stored here)."
    Add-Content $envFile "DATABASE_URL=postgres://evesynapse:$dbPassword@localhost:5432/evesynapse?sslmode=disable"
    Write-Host "Wrote DATABASE_URL to .env."
}

# --- 4. Embedded assets (the `make assets` step) ---------------------------
$fontsDir = Join-Path $RepoRoot "internal\app\static\fonts"
New-Item -ItemType Directory -Force $fontsDir | Out-Null
foreach ($f in Get-ChildItem "ci-assets\fonts\*.b64") {
    $b64 = (Get-Content $f.FullName -Raw) -replace "\s", ""
    [IO.File]::WriteAllBytes((Join-Path $fontsDir ($f.BaseName)), [Convert]::FromBase64String($b64))
}
$bgText = (Get-ChildItem "ci-assets\bg.jpg.b64.part-*" | Sort-Object Name |
    ForEach-Object { Get-Content $_.FullName -Raw }) -join ""
$bgText = $bgText -replace "\s", ""
[IO.File]::WriteAllBytes((Join-Path $RepoRoot "internal\app\static\bg.jpg"), [Convert]::FromBase64String($bgText))
Write-Host "Decoded the embedded assets (fonts, wallpaper)."

# --- 5. Build the dev binary ----------------------------------------------
if ($SkipBuild) {
    Write-Host "Skipping the build (-SkipBuild)."
} else {
    go build -o evesynapse-dev.exe ./cmd/evesynapse-dev
    if ($LASTEXITCODE -ne 0) { Die "the build failed; see the output above." }
    Write-Host "Built evesynapse-dev.exe."
}

Write-Host ""
Write-Host "Done. Run it with:"
Write-Host "  .\evesynapse-dev.exe"
Write-Host "The first start imports the SDE and takes a while. Then open"
Write-Host "  http://localhost:8080/dev-login"
Write-Host "for an instant signed-in session (a fake account: pages needing"
Write-Host "real characters stay empty; for those, register a second EVE app"
Write-Host "with the callback http://localhost:8080/auth/callback and put its"
Write-Host "credentials in .env). Stop the server with Ctrl+C."
Write-Host ""
Write-Host "Never deploy evesynapse-dev.exe anywhere reachable: /dev-login"
Write-Host "hands a session to anyone who asks. Production uses cmd/evesynapse."
