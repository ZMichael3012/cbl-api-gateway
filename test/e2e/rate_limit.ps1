# E2E CBL-7.5: периметр на compose-стенде (redis + stub-апстрим + api-gateway).
# Запуск из каталога сервиса: powershell -NoProfile -File test/e2e/rate_limit.ps1
# Сценарий: compose up -> ok-ключ в кэше проверки -> проксирование /v1 ->
# всплеск -> 429 rate_limited с Retry-After -> сброс окна -> deny-ключ -> 401 ->
# битый заголовок -> 401 -> неизвестный токен делегируется BFF -> /admin/v1
# проксируется -> остановка stub -> 503 service_unavailable -> readyz.
$ErrorActionPreference = "Stop"

$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..\..")
$ComposeFile = Join-Path $RepoRoot "deploy\docker-compose.yml"
$BaseUrl = "http://localhost:8000"
$MerchantId = "01912f2f-6f82-7d5e-8e04-4d4b5b6a1234"

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

# TokenHashHex вычисляет sha256-ключ кэша проверки (контракт 7.2 FR-6).
function TokenHashHex([string]$Token) {
    $sha256 = [System.Security.Cryptography.SHA256]::Create()
    try {
        $hash = $sha256.ComputeHash([Text.Encoding]::UTF8.GetBytes($Token))
    } finally {
        $sha256.Dispose()
    }
    return ([BitConverter]::ToString($hash) -replace "-", "").ToLower()
}

# Set-KeyCache пишет значение кэша проверки в Redis стенда.
function Set-KeyCache([string]$Token, [string]$Value) {
    $key = "apikey:v1:$(TokenHashHex $Token)"
    $result = Invoke-Compose @("exec", "-T", "redis", "redis-cli", "SET", $key, $Value)
    if ($result.Code -ne 0 -or ($result.Output -notmatch "OK")) {
        throw "redis SET failed: $($result.Output)"
    }
}

# Invoke-Http выполняет запрос и возвращает статус, тело и заголовки даже для
# ошибочных ответов (PowerShell 5.1 не имеет -SkipHttpErrorCheck).
function Invoke-Http([string]$Method, [string]$Url, [hashtable]$Headers) {
    $request = [System.Net.HttpWebRequest]::Create($Url)
    $request.Method = $Method
    $request.Timeout = 10000
    foreach ($name in $Headers.Keys) {
        $request.Headers.Add($name, $Headers[$name])
    }
    try {
        $response = $request.GetResponse()
    } catch [System.Net.WebException] {
        if (-not $_.Exception.Response) { throw "HTTP $Method $Url failed: $($_.Exception.Message)" }
        $response = $_.Exception.Response
    }
    try {
        $reader = New-Object System.IO.StreamReader($response.GetResponseStream())
        $body = $reader.ReadToEnd()
    } finally {
        $response.Close()
    }
    return @{
        Status  = [int]$response.StatusCode
        Body    = $body
        Retry   = $response.Headers["Retry-After"]
        Auth    = $response.Headers["WWW-Authenticate"]
        Request = $response.Headers["X-Request-Id"]
    }
}

# Wait-Ready ожидает готовности периметра.
function Wait-Ready() {
    foreach ($attempt in 1..40) {
        try {
            $response = Invoke-Http "GET" "$BaseUrl/readyz" @{}
            if ($response.Status -eq 200) { return }
        } catch {
            # сервис еще поднимается
        }
        Start-Sleep -Seconds 2
    }
    throw "api-gateway readyz is not green"
}

# Wait-Upstream ожидает готовности stub-апстрима за периметром.
function Wait-Upstream([string]$Token) {
    foreach ($attempt in 1..30) {
        $response = Invoke-Http "GET" "$BaseUrl/v1/warmup" @{ Authorization = "Bearer $Token" }
        if ($response.Status -ne 503) { return }
        Start-Sleep -Seconds 1
    }
    throw "stub upstream is not reachable through gateway"
}

Push-Location $RepoRoot
try {
    Invoke-Compose @("--profile", "app", "down", "-v") | Out-Null
    $up = Invoke-Compose @("--profile", "app", "up", "-d", "--build")
    if ($up.Code -ne 0) { throw "docker compose up failed: $($up.Output)" }
    Wait-Ready

    # Liveness: отдельный от готовности маршрут.
    $health = Invoke-Http "GET" "$BaseUrl/healthz" @{}
    if ($health.Status -ne 200) { throw "healthz failed: $($health.Status)" }

    # Подтвержденный ключ: периметр читает кэш BFF и проксирует запрос.
    $okToken = "cbl_v1_0123456789_e2e_ok"
    Set-KeyCache $okToken "ok:$MerchantId"
    Wait-Upstream $okToken
    # Прогрев мог израсходовать окно лимита: начинаем с чистого окна.
    Start-Sleep -Milliseconds 1200

    $authorization = @{ Authorization = "Bearer $okToken" }
    $allowed = Invoke-Http "GET" "$BaseUrl/v1/invoices/e2e" $authorization
    if ($allowed.Status -ne 200) { throw "allowed request failed: $($allowed.Status) $($allowed.Body)" }
    if ($allowed.Body -notmatch '"upstream":"stub"') { throw "response is not from stub: $($allowed.Body)" }
    if ($allowed.Body -notmatch '"path":"/v1/invoices/e2e"') { throw "path is not forwarded: $($allowed.Body)" }
    if ($allowed.Body -notmatch '"request_id":"..') { throw "request id is not forwarded: $($allowed.Body)" }
    if (-not $allowed.Request) { throw "response lacks X-Request-Id header" }

    # Всплеск сверх лимита (RATE_LIMIT_PER_SEC=2): часть запросов отклоняется
    # 429 в формате контракта; окно может смениться на границе секунды.
    $limitedStatus = 0
    $limitedRetry = $null
    $limitedBody = $null
    foreach ($request in 1..10) {
        $response = Invoke-Http "GET" "$BaseUrl/v1/invoices/burst" $authorization
        if ($response.Status -eq 429) {
            $limitedStatus = $response.Status
            $limitedRetry = $response.Retry
            $limitedBody = $response.Body
            break
        }
        if ($response.Status -ne 200) { throw "unexpected status in burst: $($response.Status)" }
    }
    if ($limitedStatus -ne 429) { throw "rate limit was not enforced during burst" }
    if ($limitedBody -notmatch '"code":"rate_limited"') { throw "429 body is not rate_limited problem: $limitedBody" }
    if (-not $limitedRetry) { throw "429 response lacks Retry-After" }

    # Новое окно: единичный запрос снова проходит.
    Start-Sleep -Milliseconds 1200
    $afterWindow = Invoke-Http "GET" "$BaseUrl/v1/invoices/after-window" $authorization
    if ($afterWindow.Status -ne 200) { throw "request after window failed: $($afterWindow.Status)" }

    # Tombstone отозванного ключа: отказ без похода в апстрим.
    $denyToken = "cbl_v1_0123456789_e2e_deny"
    Set-KeyCache $denyToken "deny"
    $denied = Invoke-Http "GET" "$BaseUrl/v1/invoices/denied" @{ Authorization = "Bearer $denyToken" }
    if ($denied.Status -ne 401) { throw "deny key must return 401: $($denied.Status)" }
    if ($denied.Body -notmatch '"code":"unauthorized"') { throw "401 body is not unauthorized problem: $($denied.Body)" }
    if (-not $denied.Auth) { throw "401 response lacks WWW-Authenticate" }

    # Строгая форма заголовка: без Authorization - 401.
    $missing = Invoke-Http "GET" "$BaseUrl/v1/invoices/no-auth" @{}
    if ($missing.Status -ne 401) { throw "missing Authorization must return 401: $($missing.Status)" }

    # Неизвестный токен (промах кэша) делегируется BFF: stub отвечает 200.
    $coldToken = "cbl_v1_0123456789_e2e_cold"
    $delegated = Invoke-Http "GET" "$BaseUrl/v1/invoices/cold" @{ Authorization = "Bearer $coldToken" }
    if ($delegated.Status -ne 200) { throw "cache miss must be delegated: $($delegated.Status)" }

    # /admin/v1 проксируется в Back-office (в стенде - stub) без лимита мерчанта.
    $admin = Invoke-Http "GET" "$BaseUrl/admin/v1/payouts" @{}
    if ($admin.Status -ne 200) { throw "admin route failed: $($admin.Status)" }
    if ($admin.Body -notmatch '"path":"/admin/v1/payouts"') { throw "admin path is not forwarded: $($admin.Body)" }

    # Неизвестный путь: 404 problem+json.
    $notFound = Invoke-Http "GET" "$BaseUrl/unknown" @{}
    if ($notFound.Status -ne 404) { throw "unknown path must return 404: $($notFound.Status)" }

    # Апстрим остановлен: периметр отвечает 503 service_unavailable, наружу не
    # утекают детали сбоя.
    $stop = Invoke-Compose @("stop", "stub")
    if ($stop.Code -ne 0) { throw "stub stop failed: $($stop.Output)" }
    Start-Sleep -Milliseconds 1200
    $outage = Invoke-Http "GET" "$BaseUrl/v1/invoices/outage" $authorization
    if ($outage.Status -ne 503) { throw "upstream outage must return 503: $($outage.Status)" }
    if ($outage.Body -notmatch '"code":"service_unavailable"') { throw "503 body is not service_unavailable problem: $($outage.Body)" }
    if (-not $outage.Retry) { throw "503 response lacks Retry-After" }

    Write-Host "OK: proxy /v1 and /admin/v1 green, key cache ok/deny/miss paths verified, 429 rate_limited with Retry-After, window reset, 503 service_unavailable on upstream outage, readyz green"
} finally {
    Invoke-Compose @("--profile", "app", "down", "-v") | Out-Null
    Pop-Location
}
