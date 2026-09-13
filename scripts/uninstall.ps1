<#
.SYNOPSIS
  卸載 pico-utm-agent：移除服務註冊、防火牆規則與執行檔。
  設定檔、spool 與 log 會保留（避免誤刪客戶資料），並顯示手動完整清除的方式。

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\uninstall.ps1
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$ServiceName = 'pico-utm-agent'
$RuleName    = 'Pico-UTM Syslog'
$InstallDir  = Join-Path $env:ProgramFiles 'pico-utm-agent'
$DataDir     = Join-Path $env:ProgramData 'pico-utm-agent'
$ExePath     = Join-Path $InstallDir 'agent.exe'
$ConfigPath  = Join-Path $DataDir 'config.toml'

$failed = New-Object System.Collections.Generic.List[string]
function Step([string]$m) { Write-Host ''; Write-Host "==> $m" -ForegroundColor Cyan }
function Info([string]$m) { Write-Host "    $m" }
function Bad([string]$m) { Write-Host "    [✗] $m" -ForegroundColor Red; $failed.Add($m) }

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host '[✗] 需要系統管理員權限。請以「系統管理員身分執行」PowerShell 後重新執行。' -ForegroundColor Red
    exit 1
}

# ---------------------------------------------------------------------------
Step '[1/4] 移除 Windows 服務'
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($svc) {
    Info "找到服務：$ServiceName（狀態 $($svc.Status)）"
    $svc.Dispose()
    try {
        if (Test-Path -LiteralPath $ExePath) {
            & $ExePath uninstall | ForEach-Object { Info $_ }
            if ($LASTEXITCODE -ne 0) { throw 'agent uninstall 失敗' }
        } else {
            Info "找不到 $ExePath，改用 sc.exe 移除"
            Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
            Info '服務已停止'
            sc.exe delete $ServiceName | Out-Null
            for ($i = 0; $i -lt 30 -and (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue); $i++) { Start-Sleep -Seconds 1 }
        }
    } catch { Info "錯誤：$($_.Exception.Message)" }
    if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) { Bad "服務 $ServiceName 仍存在" }
    else { Info "[✓] 服務註冊已移除：$ServiceName" }
} else { Info '未註冊服務，略過' }

if ($failed.Count -gt 0) { exit 1 }

# ---------------------------------------------------------------------------
Step '[2/4] 移除防火牆規則'
$rules = @(Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue)
if ($rules.Count -gt 0) {
    foreach ($r in $rules) {
        $pf = $r | Get-NetFirewallPortFilter
        try {
            $r | Remove-NetFirewallRule
            Info "[✓] 已移除防火牆規則：「$RuleName」（$($r.Direction)、$($pf.Protocol) $($pf.LocalPort)）"
        } catch { Bad "移除防火牆規則失敗：$($_.Exception.Message)" }
    }
} else { Info "找不到防火牆規則「$RuleName」，略過" }

# ---------------------------------------------------------------------------
Step '[3/4] 移除執行檔'
if (Test-Path -LiteralPath $InstallDir) {
    $procs = @()
    for ($i = 0; $i -lt 20; $i++) {
        $procs = @(Get-Process | Where-Object { $_.Path -and $_.Path.StartsWith($InstallDir, [StringComparison]::OrdinalIgnoreCase) })
        if ($procs.Count -eq 0) { break }
        Start-Sleep -Milliseconds 500
    }
    if ($procs.Count -gt 0) {
        Bad "仍有行程在使用執行檔（$(($procs | ForEach-Object { "PID $($_.Id) $($_.ProcessName)" }) -join '、')），例如開著的 agent tail / logs；請關閉後重新執行 uninstall.ps1"
    } else {
        Get-ChildItem -LiteralPath $InstallDir -Force | ForEach-Object { Info "刪除：$($_.FullName)" }
        try {
            $resolved = (Resolve-Path -LiteralPath $InstallDir).Path.TrimEnd('\')
            $expected = [IO.Path]::GetFullPath((Join-Path $env:ProgramFiles 'pico-utm-agent')).TrimEnd('\')
            if ($resolved -ne $expected -or ((Get-Item -LiteralPath $InstallDir -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Unsafe uninstall path: $resolved" }
            Remove-Item -Recurse -Force -LiteralPath $resolved
            Info "[✓] 已刪除目錄：$InstallDir"
        } catch { Bad "刪除 $InstallDir 失敗：$($_.Exception.Message)" }
    }
} else { Info "找不到 $InstallDir，略過" }

# ---------------------------------------------------------------------------
Step '[4/4] 保留的資料'
if (Test-Path -LiteralPath $DataDir) {
    Info '以下資料未刪除（避免誤刪客戶資料）：'
    foreach ($p in @($ConfigPath, (Join-Path $DataDir 'status.json'), (Join-Path $DataDir 'spool'), (Join-Path $DataDir 'logs'))) {
        if (-not (Test-Path -LiteralPath $p)) { continue }
        $item = Get-Item -LiteralPath $p
        if ($item.PSIsContainer) {
            $files = @(Get-ChildItem -LiteralPath $p -Recurse -File -Force)
            $size = ($files | Measure-Object -Property Length -Sum).Sum
            Info ("  {0}（{1} 個檔案，{2:N1} MB）" -f $p, $files.Count, ($size / 1MB))
        } else { Info "  $p" }
    }
    if (Test-Path -LiteralPath $ConfigPath) {
        $m = Select-String -LiteralPath $ConfigPath -Pattern '^\s*agent_id\s*=\s*"([^"]+)"' | Select-Object -First 1
        if ($m) { Info "重新安裝時會沿用 agent_id：$($m.Matches[0].Groups[1].Value)" }
    }
    Info ''
    Info '若確定要完整清除（含未轉送的事件），請以系統管理員執行：'
    Info "  Remove-Item -Recurse -Force '$DataDir'"
} else { Info "找不到 $DataDir，沒有保留資料" }

Write-Host ''
if ($failed.Count -gt 0) {
    Write-Host "[✗] 卸載未完全完成（$($failed.Count) 項）：" -ForegroundColor Red
    $failed | ForEach-Object { Write-Host "    - $_" -ForegroundColor Red }
    exit 1
}
Write-Host '[✓] 卸載完成（服務、防火牆規則、執行檔已移除）' -ForegroundColor Green
exit 0
