# E2E CBL-7.2/7.3: API-ключи и старт сервиса на compose-стенде billing-api.
# Запуск из каталога сервиса: powershell -NoProfile -File test/e2e/api_keys.ps1
# Сценарий: compose up (PostgreSQL + Redis) -> linux-сборка keyctl -> issue ->
# verify (miss, затем hit через Redis) -> rotate (прежний ключ в окне
# перекрытия, retiring один) -> revoke (verify отказ, кэш очищен, повтор
# идемпотентен) -> list со статусами -> сборка и старт сервиса (профиль app),
# readyz, миграции версии 2 (ключи и идемпотентность).
# keyctl запускается контейнером в сети compose: хост-порт 6379 может быть
# занят локальным redis на машине разработчика.
$ErrorActionPreference = "Stop"

$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..\..")
$ComposeFile = Join-Path $RepoRoot "deploy\docker-compose.yml"
$Network = "cbl-billing-api_cbl-net"
$ToolsDir = Join-Path $env:TEMP "opencode\cbl-billing-e2e-tools"
$DbDsn = "postgres://cbl:cbl@postgres:5432/billing?sslmode=disable"
$RedisAddr = "redis:6379"

# Invoke-Compose выполняет docker compose с подавлением NativeCommandError
# (прогресс пишется в stderr) и возвращает вывод и код.
function Invoke-Compose([string[]]$ComposeArgs) {
    $previous = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        $output = docker compose -f $ComposeFile @ComposeArgs 2>&1
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previous
    }
    return @{ Output = ($output -join "`n"); Code = $code }
}

# Build-Keyctl собирает keyctl под linux и копирует в каталог для монтирования.
function Build-Keyctl() {
    New-Item -ItemType Directory -Force -Path $ToolsDir | Out-Null
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    try {
        go build -o (Join-Path $ToolsDir "keyctl") ./cmd/keyctl
        if ($LASTEXITCODE -ne 0) { throw "keyctl build failed" }
    } finally {
        Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
    }
}

# Mask-Secret вырезает полные токены из диагностического вывода, чтобы они не
# попадали в логи e2e и сообщения об ошибках.
function Mask-Secret([string]$Text) {
    return ($Text -replace 'cbl_v1_[0-9a-f]{10}_[A-Za-z0-9_-]+', 'cbl_v1_***_***')
}

# Invoke-Keyctl выполняет команду keyctl в сети compose и возвращает вывод;
# ненулевой код возврата - ошибка (кроме ExpectedFail).
function Invoke-Keyctl([string[]]$KeyctlArgs, [switch]$ExpectedFail) {
    $previous = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        $output = docker run --rm --network $Network -v "${ToolsDir}:/tools" -v "${RepoRoot}:/src" `
            -e "PG_DSN=$DbDsn" -e "REDIS_ADDR=$RedisAddr" -e "MIGRATIONS_DIR=/src/migrations" `
            redis:7-alpine /tools/keyctl @KeyctlArgs 2>&1
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previous
    }
    $masked = Mask-Secret ($output -join "`n")
    $maskedArgs = Mask-Secret ($KeyctlArgs -join " ")
    if ($ExpectedFail) {
        if ($code -eq 0) { throw "keyctl $maskedArgs unexpectedly succeeded: $masked" }
    } elseif ($code -ne 0) {
        throw "keyctl $maskedArgs failed: $masked"
    }
    return ($output -join "`n")
}

# TokenFromOutput выделяет полный токен из вывода issue/rotate.
function TokenFromOutput([string]$Output) {
    $line = ($Output -split "`n" | Where-Object { $_ -like "token: *" } | Select-Object -First 1)
    if (-not $line) { throw "token not found in output: $Output" }
    return $line.Substring("token: ".Length).Trim()
}

# PrefixFromToken выделяет prefix из формы cbl_v1_<prefix>_<secret>.
function PrefixFromToken([string]$Token) {
    $parts = $Token -split "_", 3
    if ($parts.Count -ne 3 -or $parts[0] -ne "cbl" -or $parts[1] -ne "v1") { throw "unexpected token: $Token" }
    return ($parts[2] -split "_", 2)[0]
}

# CacheKeyFromToken вычисляет ключ Redis-контракта проверки (sha256 токена).
function CacheKeyFromToken([string]$Token) {
    $sha256 = [System.Security.Cryptography.SHA256]::Create()
    try {
        $sha = $sha256.ComputeHash([Text.Encoding]::UTF8.GetBytes($Token))
    } finally {
        $sha256.Dispose()
    }
    $hex = ([BitConverter]::ToString($sha) -replace "-", "").ToLower()
    return "apikey:v1:$hex"
}

Push-Location $RepoRoot
try {
    Invoke-Compose @("down", "-v") | Out-Null
    $up = Invoke-Compose @("up", "-d")
    if ($up.Code -ne 0) { throw "docker compose up failed: $($up.Output)" }

    $ready = $false
    foreach ($attempt in 1..30) {
        $ping = Invoke-Compose @("exec", "-T", "postgres", "pg_isready", "-U", "cbl", "-d", "billing")
        if ($ping.Code -eq 0) { $ready = $true; break }
        Start-Sleep -Seconds 2
    }
    if (-not $ready) { throw "postgres is not ready" }
    Build-Keyctl

    # issue: единственный активный ключ; повторный issue отклоняется.
    $issued = Invoke-Keyctl @("issue", "--merchant", "m-e2e")
    $token = TokenFromOutput $issued
    $prefix = PrefixFromToken $token
    if ($issued -notmatch "status=active") { throw "issued key is not active: $issued" }
    $secondIssue = Invoke-Keyctl @("issue", "--merchant", "m-e2e") -ExpectedFail
    if ($secondIssue -notmatch "already exists") { throw "second issue must fail with active exists: $secondIssue" }

    # verify: первый прогон по PostgreSQL, второй - из Redis-кэша.
    $verifyMiss = Invoke-Keyctl @("verify", $token)
    if ($verifyMiss -notmatch "merchant_id=m-e2e cache=miss") { throw "verify miss unexpected: $verifyMiss" }
    $cacheKey = CacheKeyFromToken $token
    $cached = (Invoke-Compose @("exec", "-T", "redis", "redis-cli", "GET", $cacheKey)).Output
    if (($cached -join "").Trim() -ne "ok:m-e2e") { throw "cache value unexpected: $cached" }
    $verifyHit = Invoke-Keyctl @("verify", $token)
    if ($verifyHit -notmatch "merchant_id=m-e2e cache=hit") { throw "verify hit unexpected: $verifyHit" }

    # rotate: новый активный ключ, прежний - retiring в окне перекрытия.
    $rotated = Invoke-Keyctl @("rotate", "--merchant", "m-e2e")
    $secondToken = TokenFromOutput $rotated
    $oldVerify = Invoke-Keyctl @("verify", $token)
    if ($oldVerify -notmatch "merchant_id=m-e2e") { throw "retiring key must stay valid: $oldVerify" }
    $secondVerify = Invoke-Keyctl @("verify", $secondToken)
    if ($secondVerify -notmatch "merchant_id=m-e2e") { throw "rotated key must be valid: $secondVerify" }
    $listed = Invoke-Keyctl @("list", "--merchant", "m-e2e")
    if (($listed -split "status=active").Count -ne 2) { throw "exactly one active key expected: $listed" }
    if (($listed -split "status=retiring").Count -ne 2) { throw "exactly one retiring key expected: $listed" }

    # Вторая ротация отзывает прежний retiring: tombstone deny перекрывает
    # позднюю запись успешной проверки, verify по нему отказывает.
    $rotateAgain = Invoke-Keyctl @("rotate", "--merchant", "m-e2e")
    $thirdToken = TokenFromOutput $rotateAgain
    $tombstone = (Invoke-Compose @("exec", "-T", "redis", "redis-cli", "GET", $cacheKey)).Output
    if (($tombstone -join "").Trim() -ne "deny") { throw "tombstone for revoked retiring expected: $tombstone" }
    $denied = Invoke-Keyctl @("verify", $token) -ExpectedFail
    if ($denied -notmatch "unauthorized") { throw "revoked retiring must be denied: $denied" }
    $secondRetiringVerify = Invoke-Keyctl @("verify", $secondToken)
    if ($secondRetiringVerify -notmatch "merchant_id=m-e2e") { throw "second retiring must stay valid: $secondRetiringVerify" }
    $thirdVerify = Invoke-Keyctl @("verify", $thirdToken)
    if ($thirdVerify -notmatch "merchant_id=m-e2e") { throw "third key must be valid: $thirdVerify" }

    # revoke: retiring отозван вручную, tombstone ставится, повтор идемпотентен.
    $secondPrefix = PrefixFromToken $secondToken
    $revoked = Invoke-Keyctl @("revoke", "--key", $secondPrefix)
    if ($revoked -notmatch "status=revoked") { throw "revoke unexpected: $revoked" }
    $repeat = Invoke-Keyctl @("revoke", "--key", $secondPrefix)
    if ($repeat -notmatch "status=revoked") { throw "repeated revoke must be idempotent: $repeat" }
    $secondDenied = Invoke-Keyctl @("verify", $secondToken) -ExpectedFail
    if ($secondDenied -notmatch "unauthorized") { throw "revoked key must be denied: $secondDenied" }

    # Итоговый список: один активный и два отозванных ключа.
    $finalList = Invoke-Keyctl @("list", "--merchant", "m-e2e")
    if (($finalList -split "status=active").Count -ne 2) { throw "exactly one active key at the end: $finalList" }
    if (($finalList -split "status=revoked").Count -ne 3) { throw "two revoked keys expected: $finalList" }
    $finalVerify = Invoke-Keyctl @("verify", $thirdToken)
    if ($finalVerify -notmatch "merchant_id=m-e2e") { throw "third key must stay valid: $finalVerify" }

    # Smoke сервиса (7.3): сборка по services-контексту, старт, readyz и
    # миграции до версии 2 (api_keys, idempotency_keys).
    $appUp = Invoke-Compose @("--profile", "app", "up", "-d", "--build")
    if ($appUp.Code -ne 0) { throw "service build/up failed: $($appUp.Output)" }
    $serviceReady = $false
    foreach ($attempt in 1..30) {
        $previous = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try {
            $response = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 http://localhost:8010/readyz
            if ($response.StatusCode -eq 200) { $serviceReady = $true; break }
        } catch {
            Start-Sleep -Seconds 2
        } finally {
            $ErrorActionPreference = $previous
        }
    }
    if (-not $serviceReady) { throw "billing-api readyz is not green" }
    $migrations = (Invoke-Compose @("exec", "-T", "postgres", "psql", "-U", "cbl", "-d", "billing", "-t", "-A", "-c", "SELECT max(version_id) FROM goose_db_version")).Output
    if (($migrations -join "").Trim() -ne "2") { throw "migrations version expected 2: $migrations" }

    Write-Host "OK: migrations applied, issue/verify(cache)/rotate/revoke/list green, retiring window, tombstone инвалидация и идемпотентный revoke verified, service smoke (readyz, migrations=2) green"
} finally {
    Invoke-Compose @("--profile", "app", "down", "-v") | Out-Null
    if (Test-Path $ToolsDir) { Remove-Item -Recurse -Force $ToolsDir }
    Pop-Location
}
