<#
.SYNOPSIS
  安裝 pico-utm-agent 為 Windows 服務。

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\install.ps1 -Endpoint https://collector.example.com -Token xxx -SiteId acme-taipei-hq

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\install.ps1 -Endpoint https://collector.example.com -Token xxx -SiteId acme-taipei-hq -LogLevel debug -Force
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Endpoint,
    [Parameter(Mandatory = $true)][string]$Token,
    [Parameter(Mandatory = $true)][string]$SiteId,
    [int]$Port = 5514,
    [string]$LogLevel = 'info',
    [string]$ProxyUrl = '',
    [string]$CaFile = '',
    [switch]$Force,
    [switch]$RegenerateId
)

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$ServiceName = 'pico-utm-agent'
$RuleName    = 'Pico-UTM Syslog'
$InstallDir  = Join-Path $env:ProgramFiles 'pico-utm-agent'
$DataDir     = Join-Path $env:ProgramData 'pico-utm-agent'
$ExePath     = Join-Path $InstallDir 'agent.exe'
$ConfigPath  = Join-Path $DataDir 'config.toml'
$SpoolDir    = Join-Path $DataDir 'spool'
$LogDir      = Join-Path $DataDir 'logs'
$SourceExe   = Join-Path $PSScriptRoot 'agent.exe'
$TotalSteps  = 11

$script:StepNo = 0
$script:StepName = ''
$script:Problems = New-Object System.Collections.Generic.List[string]
$script:CreatedDirs = @()
$script:Rollback = New-Object System.Collections.Generic.List[object]

function Step([string]$Name) {
    $script:StepNo++
    $script:StepName = $Name
    Write-Host ''
    Write-Host "==> [$($script:StepNo)/$TotalSteps] $Name" -ForegroundColor Cyan
}
function Info([string]$Msg) { Write-Host "    $Msg" }
function Ok([string]$Msg) { Write-Host "    [✓] $Msg" -ForegroundColor Green }
function Bad([string]$Msg) { Write-Host "    [✗] $Msg" -ForegroundColor Red; $script:Problems.Add($Msg) }
function AddRollback([string]$Desc, [scriptblock]$Action) {
    $bindings = New-Object 'System.Collections.Generic.List[System.Management.Automation.PSVariable]'
    foreach ($name in @('path', 'savedAcl', 'filter', 'originalPort')) {
        $variable = Get-Variable -Name $name -Scope 1 -ErrorAction SilentlyContinue
        if ($variable) { $bindings.Add((New-Object System.Management.Automation.PSVariable($name, $variable.Value))) }
    }
    $script:Rollback.Add(@{ Desc = $Desc; Action = $Action; Bindings = $bindings })
}
function TomlString([string]$s) { if ($s -match '[\x00-\x1f]') { throw 'Control characters are not allowed in configuration values' };  '"' + ($s -replace '\\', '\\' -replace '"', '\"') + '"' }
function RemoveCreatedDirectory([string]$path) {
    $full = [IO.Path]::GetFullPath($path).TrimEnd('\')
    $allowed = @($InstallDir, $DataDir, $SpoolDir, $LogDir) | ForEach-Object { [IO.Path]::GetFullPath($_).TrimEnd('\') }
    if ($full -notin $allowed) { throw "Unsafe removal path: $full" }
    if (Test-Path -LiteralPath $full) {
        if ((Get-Item -LiteralPath $full -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "Refusing reparse point: $full" }
        Remove-Item -LiteralPath $full -Recurse -Force
    }
}
function WaitExeReleased {
    for ($i = 0; $i -lt 20; $i++) {
        if (-not (Get-Process | Where-Object { $_.Path -eq $ExePath })) { return }
        Start-Sleep -Milliseconds 500
    }
}

# ---------------------------------------------------------------------------
Step '檢查權限'
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host '    [✗] 需要系統管理員權限。請在 PowerShell 圖示按右鍵 →「以系統管理員身分執行」後重新執行。' -ForegroundColor Red
    exit 1
}
Ok '以系統管理員身分執行'

# ---------------------------------------------------------------------------
Step '安裝前預檢（尚未做任何變更）'
$uri = $null
if ($Endpoint -match '^https://' -and [Uri]::TryCreate($Endpoint, [UriKind]::Absolute, [ref]$uri) -and $uri.Host) {
    Ok "endpoint：$Endpoint"
} else { Bad "-Endpoint 必須是 https:// 開頭的合法 URL（目前為 '$Endpoint'）" }
if ([string]::IsNullOrWhiteSpace($Token)) { Bad '-Token 不可為空' } else { Ok 'token：已提供（不顯示）' }
if ($SiteId -cmatch '^[a-z0-9][a-z0-9-]{2,63}$') { Ok "site_id：$SiteId" } else { Bad "-SiteId 必須符合 ^[a-z0-9][a-z0-9-]{2,63}$（目前為 '$SiteId'）" }
if ($Port -ge 1024 -and $Port -le 65535) { Ok "監聽埠：UDP $Port" } else { Bad "-Port 必須介於 1024–65535（目前為 $Port）" }
if (@('debug', 'info', 'warn', 'error') -ccontains $LogLevel) { Ok "log_level：$LogLevel" } else { Bad "-LogLevel 必須為 debug / info / warn / error（目前為 '$LogLevel'）" }
if (-not $ProxyUrl) { Ok 'proxy：未設定（仍會套用 HTTPS_PROXY / NO_PROXY 環境變數）' }
elseif ($ProxyUrl -match '^https?://') { Ok 'proxy：已設定' }
else { Bad '-ProxyUrl 必須是 http:// 或 https:// 開頭' }
if ($CaFile) {
    if (Test-Path -LiteralPath $CaFile -PathType Leaf) { $CaFile = (Resolve-Path -LiteralPath $CaFile).Path; Ok "ca_file：$CaFile" }
    else { Bad "-CaFile 找不到檔案：$CaFile" }
}
if (Test-Path -LiteralPath $SourceExe) { Ok "安裝來源：$SourceExe" } else { Bad "找不到 $SourceExe（install.ps1 須與 agent.exe 放在同一資料夾）" }

$existingSvc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
$svcPid = 0
if ($existingSvc) {
    $svcPid = [int](Get-CimInstance Win32_Service -Filter "Name='$ServiceName'").ProcessId
    if ($Force) { Ok "服務名稱 $ServiceName 已註冊（$($existingSvc.Status)）；-Force：將停止後更新並沿用註冊" }
    else { Bad "服務名稱 $ServiceName 已被註冊（$($existingSvc.Status)）。覆蓋安裝請加 -Force；移除請執行 uninstall.ps1" }
} else { Ok "服務名稱 $ServiceName 未被佔用" }

if ($existingSvc) {
    $serviceConfig = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'"
    $expected = '"' + $ExePath + '" run --config "' + $ConfigPath + '"'
    $plain = $ExePath + ' run --config ' + $ConfigPath
    if ($serviceConfig.PathName -ne $expected -and $serviceConfig.PathName -ne $plain -and
        $serviceConfig.PathName -ne ('"' + $ExePath + '" run --config ' + $ConfigPath)) {
        Bad "Service name belongs to another command: $($serviceConfig.PathName)"
    }
}
foreach ($value in @($Endpoint, $Token, $SiteId, $ProxyUrl, $CaFile)) {
    if ($value -match '[\x00-\x1f]') { Bad 'Configuration values cannot contain control characters' }
}
if ((Test-Path -LiteralPath $ConfigPath) -and -not $RegenerateId) {
    $ids = @(Select-String -LiteralPath $ConfigPath -Pattern '^\s*agent_id\s*=\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"')
    if ($ids.Count -ne 1) { Bad 'Existing config has no unambiguous valid agent_id; refusing to replace it' }
}
foreach ($rootPath in @($InstallDir, $DataDir, $SpoolDir, $LogDir)) {
    if (Test-Path -LiteralPath $rootPath) {
        $item = Get-Item -LiteralPath $rootPath -Force
        if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { Bad "Unsafe directory: $rootPath" }
    }
}

$installFiles = @(Get-ChildItem -LiteralPath $InstallDir -Force -ErrorAction SilentlyContinue)
if ($installFiles.Count -eq 0) { Ok "安裝路徑 $InstallDir 可用" }
elseif ($Force) { Ok "安裝路徑 $InstallDir 已有檔案（$(@($installFiles.Name) -join ', ')）；-Force：將覆蓋" }
else { Bad "安裝路徑 $InstallDir 已有檔案（$(@($installFiles.Name) -join ', ')）。覆蓋安裝請加 -Force；移除請執行 uninstall.ps1" }
if (Test-Path -LiteralPath $ConfigPath) { Info "發現既有設定檔 $ConfigPath：將沿用其 agent_id，並建立獨立回復快照" }

$udp = @(Get-NetUDPEndpoint -ErrorAction Stop | Where-Object LocalPort -eq $Port)
if ($udp.Count -eq 0) { Ok "UDP $Port 未被佔用" }
else {
    $owners = @($udp | Select-Object -ExpandProperty OwningProcess -Unique)
    $desc = ($owners | ForEach-Object {
        $p = Get-Process -Id $_ -ErrorAction SilentlyContinue
        "PID $_ $($p.ProcessName) $($p.Path)".Trim()
    }) -join '；'
    if ($Force -and $svcPid -gt 0 -and $owners.Count -eq 1 -and $owners[0] -eq $svcPid) { Ok "UDP $Port 由既有的 agent 服務使用（$desc），升級時會先停止" }
    else { Bad "UDP $Port 已被佔用：$desc" }
}

$drive = Get-PSDrive -Name $env:ProgramData.Substring(0, 1)
$freeGB = [math]::Round($drive.Free / 1GB, 1)
if ($drive.Free -ge 1GB) { Ok "磁碟 $($drive.Name): 可用 $freeGB GB" } else { Bad "磁碟 $($drive.Name): 可用空間不足 1GB（$freeGB GB）" }

foreach ($volume in @($env:ProgramFiles.Substring(0, 1), $env:ProgramData.Substring(0, 1)) | Select-Object -Unique) {
    $disk = Get-PSDrive -Name $volume
    if ($disk.Free -lt 1GB) { Bad "Less than 1 GiB free on ${volume}:" }
}
$preflightRules = @(Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue)
foreach ($rule in $preflightRules) {
    $portFilter = $rule | Get-NetFirewallPortFilter
    if ($portFilter.Protocol -ne 'UDP' -or $rule.Direction -ne 'Inbound' -or $rule.Action -ne 'Allow') {
        Bad "防火牆規則名稱被不相容的規則占用：$($rule.Name)"
    }
}
if ($script:Problems.Count -gt 0) {
    Write-Host ''
    Write-Host "[✗] 預檢未通過（$($script:Problems.Count) 項），未做任何變更。" -ForegroundColor Red
    exit 1
}

# ---------------------------------------------------------------------------
$BackupDir = Join-Path $DataDir ('.install-backup-' + [guid]::NewGuid().ToString('N'))
$dataExisted = Test-Path -LiteralPath $DataDir
try {
    if (-not $dataExisted) { New-Item -ItemType Directory -Path $DataDir | Out-Null; $script:CreatedDirs += $DataDir }
    New-Item -ItemType Directory -Path $BackupDir | Out-Null
    icacls $BackupDir /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Cannot protect rollback backup' }
    foreach ($path in @($DataDir, $ConfigPath)) {
        if (Test-Path -LiteralPath $path) {
            $savedAcl = Get-Acl -LiteralPath $path
            AddRollback "Restore ACL $path" { Set-Acl -LiteralPath $path -AclObject $savedAcl }
        }
    }
    Step '停止既有服務（保留原始註冊與啟動設定）'
    if ($existingSvc) {
        $wasRunning = $existingSvc.Status -eq 'Running'
        AddRollback 'Restore original service state' {
            if ($wasRunning) { Start-Service -Name $ServiceName }
        }
        Stop-Service -Name $ServiceName -ErrorAction Stop
        (Get-Service $ServiceName).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
        WaitExeReleased
        Info "Stopped $ServiceName; original service registration retained"
    }

    # -----------------------------------------------------------------------
    $statusPath = Join-Path $DataDir 'status.json'
    if (Test-Path -LiteralPath $statusPath) {
        Copy-Item -LiteralPath $statusPath -Destination (Join-Path $BackupDir 'status.json')
        AddRollback 'Restore status.json' { Copy-Item -LiteralPath (Join-Path $BackupDir 'status.json') -Destination $statusPath -Force }
    } else { AddRollback 'Remove new status.json' { Remove-Item -LiteralPath $statusPath -Force -ErrorAction SilentlyContinue } }
    Step '複製執行檔'
    if (Test-Path -LiteralPath $InstallDir) { Info "目錄已存在：$InstallDir" }
    elseif ($rules.Count -gt 0) {
        foreach ($rule in $rules) {
            $filter = $rule | Get-NetFirewallPortFilter
            $originalPort = $filter.LocalPort
            AddRollback "還原防火牆規則 $($rule.Name) 的連接埠" { $filter | Set-NetFirewallPortFilter -LocalPort $originalPort | Out-Null }
            $filter | Set-NetFirewallPortFilter -LocalPort $Port | Out-Null
            Info "更新防火牆規則 $($rule.Name)：UDP $originalPort → $Port"
        }
    } else {
        New-Item -ItemType Directory -Path $InstallDir | Out-Null
        Info "建立目錄：$InstallDir"
        $script:CreatedDirs += $InstallDir
    }
    if (Test-Path -LiteralPath $ExePath) {
        Copy-Item -LiteralPath $ExePath -Destination "$BackupDir\agent.exe" -Force
        Info "備份既有執行檔：$BackupDir\agent.exe"
        AddRollback "還原執行檔 $ExePath" { WaitExeReleased; Move-Item -LiteralPath "$BackupDir\agent.exe" -Destination $ExePath -Force }
    } else {
        AddRollback "刪除執行檔 $ExePath" { WaitExeReleased; Remove-Item -LiteralPath $ExePath -Force -ErrorAction SilentlyContinue }
    }
    if ((Resolve-Path -LiteralPath $SourceExe).Path -ne $ExePath) { Copy-Item -LiteralPath $SourceExe -Destination $ExePath -Force }
    Info "複製：$SourceExe → $ExePath"

    # -----------------------------------------------------------------------
    Step '決定 agent_id'
    $agentId = $null
    if ((Test-Path -LiteralPath $ConfigPath) -and -not $RegenerateId) {
        $m = Select-String -LiteralPath $ConfigPath -Pattern '^\s*agent_id\s*=\s*"([0-9a-fA-F-]{36})"' | Select-Object -First 1
        if ($m) { $agentId = $m.Matches[0].Groups[1].Value }
    }
    if ($agentId) { Info "沿用既有 agent_id：$agentId（來自 $ConfigPath）" }
    else {
        $agentId = [guid]::NewGuid().ToString()
        if ($RegenerateId) { Info "-RegenerateId：產生新的 agent_id：$agentId" } else { Info "產生新的 agent_id：$agentId" }
    }

    # -----------------------------------------------------------------------
    Step '寫入設定檔並限制權限'
    if (Test-Path -LiteralPath $DataDir) { Info "目錄已存在：$DataDir" }
    else {
        New-Item -ItemType Directory -Path $DataDir | Out-Null
        Info "建立目錄：$DataDir"
        $script:CreatedDirs += $DataDir
    }
    if (Test-Path -LiteralPath $ConfigPath) {
        Copy-Item -LiteralPath $ConfigPath -Destination "$BackupDir\config.toml" -Force
        Info "備份既有設定檔：$BackupDir\config.toml"
        AddRollback "還原設定檔 $ConfigPath" { Copy-Item -LiteralPath "$BackupDir\config.toml" -Destination $ConfigPath -Force }
    } else {
        AddRollback "刪除設定檔 $ConfigPath" { Remove-Item -LiteralPath $ConfigPath -Force -ErrorAction SilentlyContinue }
    }
    $config = @"
[agent]
agent_id = "$agentId"  # 安裝時產生，勿手動修改
site_id  = $(TomlString $SiteId)

[listener]
addr = "0.0.0.0"
port = $Port

[forwarder]
endpoint = $(TomlString $Endpoint)
token = $(TomlString $Token)
batch_size = 500
batch_interval_seconds = 60
ca_file = $(TomlString $CaFile)

[heartbeat]
interval_seconds = 300

[spool]
dir = '$SpoolDir'
retention_days = 30
max_size_gb = 10

[logging]
level = "$LogLevel"
dir = '$LogDir'

[proxy]
url = $(TomlString $ProxyUrl)
"@
    [IO.File]::WriteAllText($ConfigPath, $config, (New-Object Text.UTF8Encoding $false))
    Info "寫入：$ConfigPath（site_id=$SiteId、port=$Port、log_level=$LogLevel）"
    icacls $DataDir /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "設定 $DataDir 權限失敗" }
    icacls $ConfigPath /inheritance:r /grant:r '*S-1-5-18:F' '*S-1-5-32-544:F' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "設定 $ConfigPath 權限失敗" }
    Info "權限：$DataDir 與設定檔僅 SYSTEM、Administrators 可存取（已停用繼承）"

    # -----------------------------------------------------------------------
    Step '建立 spool 與 log 目錄'
    foreach ($d in @($SpoolDir, $LogDir)) {
        if (Test-Path -LiteralPath $d) { Info "已存在：$d" }
        else { New-Item -ItemType Directory -Path $d | Out-Null; $script:CreatedDirs += $d; Info "建立：$d" }
    }

    # -----------------------------------------------------------------------
    Step '設定防火牆'
    $rules = @(Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue)
    $same = $false
    if ($rules.Count -eq 1) {
        $pf = $rules[0] | Get-NetFirewallPortFilter
        $same = ($pf.Protocol -eq 'UDP') -and ("$($pf.LocalPort)" -eq "$Port") -and ($rules[0].Direction -eq 'Inbound') -and ($rules[0].Action -eq 'Allow')
    }
    if ($same) { Info "規則已存在且設定相同，沿用：「$RuleName」（輸入、UDP $Port、允許）" }
    else {
        $newRuleName = 'pico-utm-agent-' + [guid]::NewGuid().ToString('N')
        AddRollback "Remove firewall rule $newRuleName" { Remove-NetFirewallRule -Name $newRuleName -ErrorAction SilentlyContinue }
        New-NetFirewallRule -Name $newRuleName -DisplayName $RuleName -Description 'pico-utm-agent syslog' -Direction Inbound -Action Allow -Protocol UDP -LocalPort $Port -Profile Any | Out-Null
        Info "Added firewall rule $newRuleName ($RuleName), inbound UDP $Port"

    }

    # -----------------------------------------------------------------------
    Step '註冊 Windows 服務'
    if (-not $existingSvc) {
        AddRollback "Remove service $ServiceName" {
            if (Get-Service $ServiceName -ErrorAction SilentlyContinue) {
                & $ExePath uninstall | Out-Null
                if ($LASTEXITCODE -ne 0) { throw 'Service rollback failed' }
            }
            WaitExeReleased
        }
        & $ExePath install --config $ConfigPath | ForEach-Object { Info $_ }
        if ($LASTEXITCODE -ne 0) { throw 'Service registration failed' }
    } else { Info "Reused service registration: $ServiceName" }
    AddRollback 'Stop new agent before restoring files' {
        if (Get-Service $ServiceName -ErrorAction SilentlyContinue) { Stop-Service $ServiceName -ErrorAction Stop }
        WaitExeReleased
    }

    # -----------------------------------------------------------------------
    Step '執行 agent test'
    & $ExePath test --config $ConfigPath | ForEach-Object { Info $_ }
    if ($LASTEXITCODE -ne 0) { throw 'agent test 未通過，請依上方建議排除後重新安裝' }

    # -----------------------------------------------------------------------
    Step '啟動服務'
    if ($existingSvc) {
        $oldStartMode = $serviceConfig.StartMode
        AddRollback 'Restore original service startup mode' {
            $mode = @{ Auto = 'Automatic'; Manual = 'Manual'; Disabled = 'Disabled' }[$oldStartMode]
            Set-Service -Name $ServiceName -StartupType $mode
        }
        Set-Service -Name $ServiceName -StartupType Automatic
    }
    Start-Service -Name $ServiceName
    $svc = Get-Service -Name $ServiceName
    for ($i = 0; $i -lt 20 -and $svc.Status -ne 'Running'; $i++) { Start-Sleep -Milliseconds 500; $svc.Refresh() }
    Start-Sleep -Seconds 2
    $svc.Refresh()
    if ($svc.Status -ne 'Running') { throw "服務未能維持執行（狀態 $($svc.Status)），請查看 $LogDir 的 log" }
    $runningPid = (Get-CimInstance Win32_Service -Filter "Name='$ServiceName'").ProcessId
    Info "服務狀態：Running（PID $runningPid）"
    $svc.Dispose()
} catch {
    if ($svc) { $svc.Dispose() }
    Write-Host ''
    Write-Host "[✗] 安裝失敗於步驟 [$($script:StepNo)/$TotalSteps] $($script:StepName)：$($_.Exception.Message)" -ForegroundColor Red
    $rollbackFailed = $false
    if ($script:Rollback.Count -gt 0) {
        Write-Host '==> 回復已完成的變更' -ForegroundColor Yellow
        for ($i = $script:Rollback.Count - 1; $i -ge 0; $i--) {
            $r = $script:Rollback[$i]
            try { $r.Action.InvokeWithContext($null, $r.Bindings, @()) | Out-Null; Info "已回復：$($r.Desc)" }
            catch { $rollbackFailed = $true; Write-Host "    [!] 回復失敗：$($r.Desc)：$($_.Exception.Message)" -ForegroundColor Yellow }
        }
    }
    if (-not $rollbackFailed) {
        if (Test-Path -LiteralPath $BackupDir) {
            Get-ChildItem -LiteralPath $BackupDir -File | Remove-Item -Force
            [IO.Directory]::Delete($BackupDir)
        }
        foreach ($path in ($script:CreatedDirs | Sort-Object Length -Descending)) { RemoveCreatedDirectory $path }
    } else { Info "Rollback backup retained for inspection: $BackupDir" }
    exit 1
}

Remove-Item -LiteralPath "$BackupDir\agent.exe", "$BackupDir\config.toml" -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath (Join-Path $BackupDir 'status.json') -Force -ErrorAction SilentlyContinue
[IO.Directory]::Delete($BackupDir)
Write-Host ''
Write-Host '[✓] 安裝完成' -ForegroundColor Green
Info "服務      ：$ServiceName（Running、開機自動啟動）"
Info "agent_id  ：$agentId"
Info "執行檔    ：$ExePath"
Info "設定檔    ：$ConfigPath"
Info "spool     ：$SpoolDir"
Info "log       ：$LogDir"
Info "防火牆    ：「$RuleName」（UDP $Port）"
Write-Host ''
Info "查看狀態：& '$ExePath' status"
Info "查看 log ：& '$ExePath' logs -n 50"
Info "即時事件：& '$ExePath' tail"
Info "卸載    ：powershell -ExecutionPolicy Bypass -File `"$PSScriptRoot\uninstall.ps1`""
exit 0
