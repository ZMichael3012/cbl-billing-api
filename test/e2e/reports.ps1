# E2E CBL-11.4: прокси /v1/reports мерчант API на compose-стенде billing-api.
# Запуск из каталога сервиса: powershell -NoProfile -File test/e2e/reports.ps1
# Сценарий: compose up (PostgreSQL + Redis + stub отчетности) -> ключ мерчанта
# -> GET /v1/reports/payments: мерчант из токена (параметр запроса игнорируется),
# пагинация по курсору -> 404 unknown_report_type -> 400 validation_failed ->
# 401 без ключа -> 503 при остановленном stub -> down -v.
$ErrorActionPreference = "Stop"

$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..\..")
$ComposeFile = Join-Path $RepoRoot "deploy\docker-compose.yml"
$Network = "cbl-billing-api_cbl-net"
$ToolsDir = Join-Path $env:TEMP "opencode\cbl-billing-e2e-tools"
$DbDsn = "postgres://cbl:cbl@postgres:5432/billing?sslmode=disable"
$RedisAddr = "redis:6379"

# Invoke-Compose выполняет docker compose с подавлением NativeCommandError.
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

# Mask-Secret вырезает полные токены из диагностического вывода.
function Mask-Secret([string]$Text) {
    return ($Text -replace 'cbl_v1_[0-9a-f]{10}_[A-Za-z0-9_-]+', 'cbl_v1_***_***')
}

# Invoke-Keyctl выполняет команду keyctl в сети compose и возвращает вывод.
function Invoke-Keyctl([string[]]$KeyctlArgs) {
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
    if ($code -ne 0) { throw "keyctl $($KeyctlArgs -join ' ') failed: $masked" }
    return ($output -join "`n")
}

# TokenFromOutput выделяет полный токен из вывода issue.
function TokenFromOutput([string]$Output) {
    $line = ($Output -split "`n" | Where-Object { $_ -like "token: *" } | Select-Object -First 1)
    if (-not $line) { throw "token not found in output: $(Mask-Secret $Output)" }
    return $line.Substring("token: ".Length).Trim()
}

# Invoke-Api выполняет GET мерчант API и возвращает статус и тело (curl.exe:
# Invoke-WebRequest 5.1 теряет тело problem+json на ошибках).
function Invoke-Api([string]$Path, [string]$Token) {
    $curlArgs = @("-s", "-S", "-o", "-", "-w", "`n%{http_code}")
    if ($Token) { $curlArgs += @("-H", "Authorization: Bearer $Token") }
    $curlArgs += "http://localhost:8010$Path"
    $previous = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        $output = & curl.exe @curlArgs 2>&1
    } finally {
        $ErrorActionPreference = $previous
    }
    $text = ($output -join "`n")
    $lines = $text -split "`n"
    $status = [int]$lines[-1].Trim()
    $body = ($lines[0..($lines.Count - 2)] -join "`n")
    return @{ Status = $status; Body = $body }
}

# Problem-Code извлекает код problem+json из тела ответа.
function Problem-Code([string]$Body) {
    return ($Body | ConvertFrom-Json).code
}

Push-Location $RepoRoot
try {
    Invoke-Compose @("--profile", "app", "down", "-v") | Out-Null
    $up = Invoke-Compose @("up", "-d", "--build")
    if ($up.Code -ne 0) { throw "infra up failed: $($up.Output)" }

    foreach ($attempt in 1..40) {
        $health = Invoke-Compose @("ps", "--format", "{{.Service}} {{.Health}}")
        if ($health.Output -notmatch "starting") { break }
        Start-Sleep -Seconds 2
    }

    Build-Keyctl
    $issue = Invoke-Keyctl @("issue", "--merchant", "m-1")
    $token = TokenFromOutput $issue

    $appUp = Invoke-Compose @("--profile", "app", "up", "-d", "--build")
    if ($appUp.Code -ne 0) { throw "service up failed: $($appUp.Output)" }
    $ready = $false
    foreach ($attempt in 1..60) {
        $previous = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try {
            $response = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 http://localhost:8010/readyz
            if ($response.StatusCode -eq 200) { $ready = $true; break }
        } catch {
            Start-Sleep -Seconds 2
        } finally {
            $ErrorActionPreference = $previous
        }
    }
    if (-not $ready) { throw "billing-api readyz is not green" }

    # Успешный отчет: мерчант берется из токена, query-параметр игнорируется.
    # Rows в REST - плоские объекты строковых значений (контракт ReportPage).
    $report = Invoke-Api "/v1/reports/payments?limit=5&merchant_id=hacker" $token
    if ($report.Status -ne 200) { throw "reports: HTTP $($report.Status): $($report.Body)" }
    $page = $report.Body | ConvertFrom-Json
    if ($page.rows.Count -ne 1 -or $page.rows[0].merchant_id -ne "m-1") {
        throw "reports: мерчант не из токена: $($report.Body)"
    }
    if ($page.rows[0].expected_amount -ne "100.000000" -or $page.rows[0].limit -ne "5") {
        throw "reports: значения не совпали: $($report.Body)"
    }

    # Пагинация: курсор проходит через прокси прозрачно.
    $firstPage = Invoke-Api "/v1/reports/payments?limit=1" $token
    $pageOne = $firstPage.Body | ConvertFrom-Json
    if ([string]::IsNullOrEmpty($pageOne.next_cursor)) { throw "пагинация: нет курсора: $($firstPage.Body)" }
    $secondPage = Invoke-Api ("/v1/reports/payments?limit=1&cursor=" + $pageOne.next_cursor) $token
    $pageTwo = $secondPage.Body | ConvertFrom-Json
    if ($pageTwo.rows[0].cursor -ne $pageOne.next_cursor -or -not [string]::IsNullOrEmpty($pageTwo.next_cursor)) {
        throw "пагинация: курсор не прошел: $($secondPage.Body)"
    }

    # 404: неизвестный тип отчета (ответ stub).
    $notFound = Invoke-Api "/v1/reports/invoices" $token
    if ($notFound.Status -ne 404 -or (Problem-Code $notFound.Body) -ne "unknown_report_type") {
        throw "404: $($notFound.Status) $($notFound.Body)"
    }

    # 400: невалидные параметры отбивает BFF до Reporting.
    $invalid = Invoke-Api "/v1/reports/payments?limit=101" $token
    if ($invalid.Status -ne 400 -or (Problem-Code $invalid.Body) -ne "validation_failed") {
        throw "400: $($invalid.Status) $($invalid.Body)"
    }

    # 401: без ключа.
    $unauthorized = Invoke-Api "/v1/reports/payments" ""
    if ($unauthorized.Status -ne 401) { throw "401: $($unauthorized.Status) $($unauthorized.Body)" }

    # 503: отчетность недоступна (stub остановлен), остальной API жив.
    Invoke-Compose @("stop", "reporting-stub") | Out-Null
    $unavailable = Invoke-Api "/v1/reports/payments" $token
    if ($unavailable.Status -ne 503 -or (Problem-Code $unavailable.Body) -ne "service_unavailable") {
        throw "503: $($unavailable.Status) $($unavailable.Body)"
    }
    Invoke-Compose @("start", "reporting-stub") | Out-Null

    Write-Host "OK: reports proxy green (merchant from token, cursor passthrough, 404 unknown_report_type, 400 validation_failed, 401, 503 on stub down)"
} finally {
    Invoke-Compose @("--profile", "app", "down", "-v") | Out-Null
    Remove-Item -Recurse -Force $ToolsDir -ErrorAction SilentlyContinue
    Pop-Location
}
