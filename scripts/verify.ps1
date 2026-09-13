<#
.SYNOPSIS
  開發測試用一鍵驗收：mock server → 安裝 agent → 灌入事件 → 比對 → 清理。
  僅限測試環境！會覆蓋安裝 pico-utm-agent，結束後刪除其設定檔、spool 與 log。

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\verify.ps1
.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\verify.ps1 -Count 5000 -KeepRunning
#>
[CmdletBinding()]
param(
    [ValidateRange(1, 10000000)][int]$Count = 1000,
    [ValidateRange(1, 86400)][int]$TimeoutSeconds = 180,
    [switch]$KeepRunning
)

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$Dir         = $PSScriptRoot
$ServiceName = 'pico-utm-agent'
$MockPort    = 18443
$UdpPort     = 5514
$Endpoint    = "https://127.0.0.1:$MockPort"
$DataDir     = Join-Path $env:ProgramData 'pico-utm-agent'
$ConfigPath  = Join-Path $DataDir 'config.toml'
$StatusPath  = Join-Path $DataDir 'status.json'
$LogDir      = Join-Path $DataDir 'logs'
$ExePath     = Join-Path $env:ProgramFiles 'pico-utm-agent\agent.exe'
$WorkDir     = Join-Path $env:ProgramData 'pico-utm-agent-verify'
$CertDir     = Join-Path $WorkDir 'certs'
$CaPath      = Join-Path $CertDir 'mock-ca.pem'
$Token       = 'verify-' + [guid]::NewGuid().ToString('N')

function Section([string]$m) { Write-Host ''; Write-Host "==> $m" -ForegroundColor Cyan }
function Info([string]$m) { Write-Host "    $m" }

# ---- 警告與確認 ------------------------------------------------------------
Write-Host '==================================================================' -ForegroundColor Yellow
Write-Host ' 警告：此腳本僅供開發測試環境使用，請勿在客戶環境執行！' -ForegroundColor Yellow
Write-Host '==================================================================' -ForegroundColor Yellow
Write-Host " 將會："
Write-Host "  1. 在 127.0.0.1:$MockPort 啟動 mock server"
Write-Host "  2. 以 -Force 覆蓋安裝 $ServiceName 服務，指向 mock"
Write-Host "  3. 送出 $Count 筆測試事件到 UDP $UdpPort 並比對結果"
if ($KeepRunning) {
    Write-Host "  4. -KeepRunning：結束後保留服務、mock server 與所有資料"
} else {
    Write-Host "  4. 結束後移除服務，並【刪除】以下資料："
    Write-Host "       $DataDir（設定檔、spool、log）"
    Write-Host "       $WorkDir"
}
Write-Host ''

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host '[✗] 需要系統管理員權限' -ForegroundColor Red
    exit 1
}
foreach ($f in 'agent.exe', 'mockserver.exe', 'syslog-gen.exe', 'install.ps1', 'uninstall.ps1') {
    if (-not (Test-Path -LiteralPath (Join-Path $Dir $f))) {
        Write-Host "[✗] 找不到 $(Join-Path $Dir $f)（請在建置產物資料夾 pico-utm-agent-windows-amd64 中執行）" -ForegroundColor Red
        exit 1
    }
}
foreach ($path in @($DataDir, $WorkDir, (Split-Path $ExePath))) {
    if (Test-Path -LiteralPath $path) { throw "Existing data/install path: $path. Refusing to overwrite. Clean up the previous installation manually first." }
}
if (Get-Service $ServiceName -ErrorAction SilentlyContinue) { throw "Service $ServiceName already exists" }
Get-Command curl.exe -ErrorAction Stop | Out-Null
Write-Host 'Any failure preserves the service, mock and data for debugging.' -ForegroundColor Yellow
$answer = Read-Host '確認要繼續？請輸入 YES'
if ($answer -cne 'YES') { Write-Host '已取消，未做任何變更。'; exit 1 }

# ---- 輔助函式 --------------------------------------------------------------
$script:Results = New-Object System.Collections.Generic.List[object]
$script:Mock = $null

function Record([string]$Name, [bool]$Pass, [string]$Detail = '') {
    $status = if ($Pass) { 'PASS' } else { 'FAIL' }
    $script:Results.Add([pscustomobject]@{ Name = $Name; Status = $status; Detail = $Detail })
    $line = "    [$status] $Name"
    if ($Detail) { $line += " — $Detail" }
    Write-Host $line -ForegroundColor $(if ($Pass) { 'Green' } else { 'Red' })
}

function MockGet([string]$Path) {
    if (-not (Test-Path -LiteralPath $CaPath)) { return $null }
    $out = & curl.exe -s --max-time 5 --cacert $CaPath -H "Authorization: Bearer $Token" "$Endpoint$Path"
    if ($LASTEXITCODE -ne 0) { return $null }
    return ($out -join "`n")
}

function MockStats {
    $raw = MockGet '/_control/stats'
    if (-not $raw) { return $null }
    try { return $raw | ConvertFrom-Json } catch { return $null }
}

function Invoke-Verify {
    # ---- 1 -----------------------------------------------------------------
    Section '1. 啟動 mock server'
    New-Item -ItemType Directory -Force -Path $CertDir | Out-Null
    if (Get-NetTCPConnection -LocalPort $MockPort -State Listen -ErrorAction SilentlyContinue) {
        Record '1. mock server 啟動' $false "TCP $MockPort 已被佔用"
        return
    }
    $script:Mock = Start-Process -FilePath (Join-Path $Dir 'mockserver.exe') `
        -ArgumentList @('-addr', "127.0.0.1:$MockPort", '-token', $Token, '-cert-dir', ('"' + $CertDir + '"')) `
        -RedirectStandardError (Join-Path $WorkDir 'mock.log') -RedirectStandardOutput (Join-Path $WorkDir 'mock.out.log') `
        -WindowStyle Hidden -PassThru
    $up = $false
    for ($i = 0; $i -lt 60; $i++) {
        Start-Sleep -Milliseconds 500
        if ($script:Mock.HasExited) { break }
        if ((MockGet '/healthz') -match '"ok":\s*true') { $up = $true; break }
    }
    Record "1. mock server 啟動（$Endpoint）" $up "PID $($script:Mock.Id)，log：$(Join-Path $WorkDir 'mock.log')"
    if (-not $up) { return }

    # ---- 2 -----------------------------------------------------------------
    Section '2. 安裝並啟動 agent（指向 mock）'
    & (Join-Path $Dir 'install.ps1') -Endpoint $Endpoint -Token $Token -SiteId 'verify-test' -CaFile $CaPath -LogLevel debug -Force
    $rc = $LASTEXITCODE
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    $installed = ($rc -eq 0) -and $svc -and ($svc.Status -eq 'Running')
    Record '2. agent 安裝並啟動（服務 Running）' $installed "install.ps1 exit $rc"
    if ($svc) { $svc.Dispose() }
    if (-not $installed) { return }

    # ---- 3 -----------------------------------------------------------------
    Section "3. syslog-gen 送出 $Count 筆"
    $gen = & (Join-Path $Dir 'syslog-gen.exe') -target "127.0.0.1:$UdpPort" -n $Count -rate 500 -start-id 123456700000000
    $rc = $LASTEXITCODE
    $gen | ForEach-Object { Info $_ }
    Record "3. syslog-gen 送出 $Count 筆" ($rc -eq 0) "exit $rc"
    if ($rc -ne 0) { return }

    # ---- 4 -----------------------------------------------------------------
    Section "4. 等待轉送完成（逾時 $TimeoutSeconds 秒）"
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $accepted = 0
    while ($sw.Elapsed.TotalSeconds -lt $TimeoutSeconds) {
        $stats = MockStats
        if ($stats) { $accepted = [int]$stats.events_accepted }
        if ($accepted -ge $Count) { break }
        Start-Sleep -Seconds 2
    }
    Record '4. 轉送完成' ($accepted -ge $Count) ("接收端 accepted={0}，耗時 {1:N0} 秒" -f $accepted, $sw.Elapsed.TotalSeconds)

    # ---- 5 -----------------------------------------------------------------
    Section '5. 比對接收端 unique 事件數'
    Start-Sleep -Seconds 3
    $stats = MockStats
    $detail = if ($stats) { "accepted=$($stats.events_accepted)，received=$($stats.events_received)，duplicated=$($stats.events_duplicated)，batches=$($stats.batches_received)，heartbeats=$($stats.heartbeats)" } else { '無法取得 /_control/stats' }
    Record "5. unique 事件數 = $Count" ([bool]$stats -and ([int]$stats.events_accepted -eq $Count)) $detail

    # ---- 6 -----------------------------------------------------------------
    Section '6. 檢查 status.json'
    $st = $null
    $raw = ''
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt 45) {
        if (Test-Path -LiteralPath $StatusPath) {
            try { $raw = Get-Content -LiteralPath $StatusPath -Raw -Encoding UTF8; $st = $raw | ConvertFrom-Json } catch { $st = $null }
        }
        if ($st -and [int64]$st.events_forwarded_total -ge $Count) { break }
        Start-Sleep -Seconds 2
    }
    if (-not $st) {
        Record '6. status.json 可讀取' $false $StatusPath
    } else {
        $required = @('updated_at', 'events_received_total', 'events_forwarded_total', 'events_dead_lettered_total', 'events_dropped_total', 'spool_write_errors_total', 'spool_backlog_bytes', 'last_event_received_at', 'last_forward_success_at', 'last_heartbeat_at', 'last_forward_error', 'agent_id', 'utm_source_ips', 'proxy_in_use', 'config')
        $missing = @($required | Where-Object { $_ -notin $st.PSObject.Properties.Name })
        Record '6. status.json required fields exist' ($missing.Count -eq 0) ($missing -join ', ')
        $age = ((Get-Date).ToUniversalTime() - ([DateTime]::Parse($st.updated_at)).ToUniversalTime()).TotalSeconds
        $cfgMatch = Select-String -LiteralPath $ConfigPath -Pattern '^\s*agent_id\s*=\s*"([^"]+)"' | Select-Object -First 1
        $cfgId = if ($cfgMatch) { $cfgMatch.Matches[0].Groups[1].Value } else { '' }
        Record '6. status.json：updated_at 在 60 秒內' ($age -ge -5 -and $age -lt 60) ("updated_at={0}（{1:N0} 秒前）" -f $st.updated_at, $age)
        Record "6. status.json：events_received_total = $Count" ([int64]$st.events_received_total -eq $Count) "實際 $($st.events_received_total)"
        Record "6. status.json：events_forwarded_total = $Count" ([int64]$st.events_forwarded_total -eq $Count) "實際 $($st.events_forwarded_total)"
        foreach ($k in 'events_dead_lettered_total', 'events_dropped_total', 'spool_write_errors_total', 'spool_backlog_bytes') {
            Record "6. status.json：$k = 0" ($null -ne $st.$k -and [int64]$st.$k -eq 0) "實際 $($st.$k)"
        }
        foreach ($k in 'last_event_received_at', 'last_forward_success_at', 'last_heartbeat_at') {
            Record "6. status.json：$k 非 null" ([bool]$st.$k) "$($st.$k)"
        }
        Record '6. status.json：last_forward_error 為 null' (-not $st.last_forward_error) "$($st.last_forward_error)"
        Record '6. status.json：agent_id 與設定檔一致' ($cfgId -and $st.agent_id -eq $cfgId) "$($st.agent_id)"
        Record '6. status.json：utm_source_ips 含 127.0.0.1' (@($st.utm_source_ips) -contains '127.0.0.1') (@($st.utm_source_ips) -join ', ')
        Record '6. status.json：proxy_in_use = false' ($st.proxy_in_use -eq $false) ''
        Record '6. status.json：config.log_level = debug' ($st.config.log_level -eq 'debug') "$($st.config.log_level)"
        Record '6. status.json：不含 token' (-not $raw.Contains($Token)) ''
    }

    # ---- 7 -----------------------------------------------------------------
    Section '7. 檢查 log'
    $logs = @(Get-ChildItem -LiteralPath $LogDir -Filter 'agent-*.log' -ErrorAction SilentlyContinue)
    Record '7. log 檔存在' ($logs.Count -gt 0) ((@($logs | ForEach-Object { $_.FullName })) -join ', ')
    $hits = @($logs | Select-String -SimpleMatch -Pattern $Token -Encoding UTF8)
    $hitDetail = if ($hits.Count -gt 0) { "出現於 $($hits[0].Path):$($hits[0].LineNumber)" } else { '' }
    Record '7. log 不含 token' ($logs.Count -gt 0 -and $hits.Count -eq 0) $hitDetail
    $batches = @($logs | Select-String -SimpleMatch -Pattern '批次轉送成功' -Encoding UTF8)
    Record '7. log 含批次轉送紀錄' ($batches.Count -gt 0) "$($batches.Count) 筆"
    & $ExePath logs -n 5 | Out-Null
    Record '7. agent logs 子命令可執行' ($LASTEXITCODE -eq 0) ''
}

# ---- 執行 ------------------------------------------------------------------
try {
    Invoke-Verify
} catch {
    Record 'Unexpected execution error' $false $_.Exception.Message
} finally {
    if ($KeepRunning -or @($script:Results | Where-Object Status -eq 'FAIL').Count -gt 0) {
        Section '保留現場（-KeepRunning）'
        Info "服務：Get-Service $ServiceName"
        if ($script:Mock -and -not $script:Mock.HasExited) { Info "mock server：PID $($script:Mock.Id)，$Endpoint，log $(Join-Path $WorkDir 'mock.log')" }
        Info "mock 統計：curl.exe -s --cacert `"$CaPath`" $Endpoint/_control/stats"
        Info "agent 狀態：& '$ExePath' status"
        Info "agent log ：& '$ExePath' logs -n 100"
        Info '手動清理：'
        Info "  powershell -ExecutionPolicy Bypass -File `"$(Join-Path $Dir 'uninstall.ps1')`""
        if ($script:Mock -and -not $script:Mock.HasExited) { Info "  Stop-Process -Id $($script:Mock.Id)" }
        Info "  Remove-Item -Recurse -Force '$DataDir', '$WorkDir'"
    } else {
        Section '清理'
        & (Join-Path $Dir 'uninstall.ps1')
        $cleanupOK = $LASTEXITCODE -eq 0
        Record '9. service/firewall/executable removal' $cleanupOK "uninstall exit $LASTEXITCODE"
        if ($cleanupOK -and $script:Mock -and -not $script:Mock.HasExited) {
            Stop-Process -Id $script:Mock.Id -Force
            $script:Mock.WaitForExit(5000) | Out-Null
            Info "已關閉 mock server（PID $($script:Mock.Id)）"
        }
        foreach ($d in @($DataDir, $WorkDir)) {
            if (-not (Test-Path -LiteralPath $d)) { continue }
            if (-not $cleanupOK) { Info "Preserved after uninstall failure: $d"; continue }
            $full = (Resolve-Path -LiteralPath $d).Path.TrimEnd('\')
            $expected = [IO.Path]::GetFullPath($d).TrimEnd('\')
            if ($full -ne $expected -or ((Get-Item -LiteralPath $d -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                Record '9. cleanup path safety' $false $full
                continue
            }
            Remove-Item -Recurse -Force -LiteralPath $full -ErrorAction Continue
            Record "9. removed test data $d" (-not (Test-Path -LiteralPath $d))
            if (Test-Path -LiteralPath $d) { Write-Host "    [!] 無法刪除 $d" -ForegroundColor Yellow } else { Info "已刪除測試資料：$d" }
        }
    }
}

Section '驗收結果'
foreach ($r in $script:Results) {
    Write-Host "    [$($r.Status)] $($r.Name)" -ForegroundColor $(if ($r.Status -eq 'PASS') { 'Green' } else { 'Red' })
}
$fails = @($script:Results | Where-Object { $_.Status -ne 'PASS' })
Write-Host ''
if ($script:Results.Count -gt 0 -and $fails.Count -eq 0) {
    Write-Host "PASS：$($script:Results.Count) 項全部通過" -ForegroundColor Green
    exit 0
}
Write-Host "FAIL：$($fails.Count) / $($script:Results.Count) 項未通過" -ForegroundColor Red
foreach ($f in $fails) { Write-Host "    - $($f.Name)：$($f.Detail)" -ForegroundColor Red }
if (-not $KeepRunning) { Write-Host '    提示：加上 -KeepRunning 可保留現場除錯' }
exit 1
