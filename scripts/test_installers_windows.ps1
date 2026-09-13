# Isolated transaction tests: system cmdlets and the agent executable are mocked.
# No real service, firewall, or system path is modified.
param([string]$CaseRoot, [string]$Case, [string]$Python)
$ErrorActionPreference = 'Stop'
function ReadState { Get-Content -LiteralPath (Join-Path $CaseRoot 'state.json') -Raw | ConvertFrom-Json }
function WriteState($state) { [IO.File]::WriteAllText((Join-Path $CaseRoot 'state.json'), ($state | ConvertTo-Json)) }
function Get-Service {
    param($Name, $ErrorAction)
    if (-not (Test-Path -LiteralPath (Join-Path $CaseRoot 'unit'))) { return }
    $state = ReadState
    $service = [pscustomobject]@{ Status = $(if ($state.running) { 'Running' } else { 'Stopped' }) }
    $service | Add-Member ScriptMethod Dispose {}
    $service | Add-Member ScriptMethod WaitForStatus {}
    $service | Add-Member ScriptMethod Refresh { $state = ReadState; $this.Status = $(if ($state.running) { 'Running' } else { 'Stopped' }) }
    $service
}
function Get-CimInstance {
    param($ClassName, $Filter)
    [pscustomobject]@{ ProcessId = 42; StartMode = 'Auto'; PathName = ('"' + (Join-Path $env:ProgramFiles 'pico-utm-agent\agent.cmd') + '" run --config "' + (Join-Path $env:ProgramData 'pico-utm-agent\config.toml') + '"') }
}
function Stop-Service { param($Name, [switch]$Force, $ErrorAction); $s=ReadState; $s.running=$false; WriteState $s }
function Start-Service {
    param($Name)
    $s=ReadState
    if ($Case -like '*start' -and -not $s.failed) { $s.failed=$true; WriteState $s; throw 'Injected start failure' }
    $s.running=$true; WriteState $s
}
function Set-Service { param($Name,$StartupType); $s=ReadState; $s.enabled=$StartupType -eq 'Automatic'; WriteState $s }
function Get-Process { param($Id) }
function Get-PSDrive { param($Name); [pscustomobject]@{Name=$Name; Free=10GB} }
function Get-NetUDPEndpoint { param($ErrorAction); if ($Case -eq 'occupied') { [pscustomobject]@{LocalPort=5514; OwningProcess=999} } }
function Get-NetFirewallRule {
    param($DisplayName,$Name,$ErrorAction)
    $s=ReadState
    if ($s.firewall) { [pscustomobject]@{Name='test-rule'; DisplayName='Pico-UTM Syslog'; Direction='Inbound'; Action='Allow'} }
}
function Get-NetFirewallPortFilter { process { [pscustomobject]@{Protocol='UDP'; LocalPort=5514} } }
function New-NetFirewallRule {
    param($Name,$DisplayName,$Description,$Direction,$Action,$Protocol,$LocalPort,$Profile)
    $s=ReadState; $s.firewall=$true; WriteState $s
    if ($Case -eq 'firewall') { $s.failed=$true; WriteState $s; throw 'Injected firewall failure after mutation' }
}
function Remove-NetFirewallRule { param($Name,$ErrorAction); process { $s=ReadState; $s.firewall=$false; WriteState $s } }
function Get-Acl { param($LiteralPath); [pscustomobject]@{TestAcl='original'} }
function Set-Acl { param($LiteralPath,$AclObject) }
function icacls { $global:LASTEXITCODE=0 }
function Start-Sleep { param($Seconds,$Milliseconds) }

$root = Split-Path $PSScriptRoot -Parent
$env:ProgramFiles = Join-Path $CaseRoot 'programfiles'
$env:ProgramData = Join-Path $CaseRoot 'programdata'
$env:INSTALL_TEST_ROOT = $CaseRoot
$env:INSTALL_TEST_FAIL = $Case -replace '^force-', ''
$env:PYTHONUTF8='1'
$source = [IO.File]::ReadAllText((Join-Path $PSScriptRoot 'install.ps1'))
$source = $source.Replace('$principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)', '$true').Replace('agent.exe', 'agent.cmd')
$testScript = Join-Path $CaseRoot 'install.ps1'
[IO.File]::WriteAllText($testScript, $source, (New-Object Text.UTF8Encoding $true))
$fake = '@echo off' + "`r`n" + '"' + $Python + '" "' + (Join-Path $PSScriptRoot 'test_installers.py') + '" --fake agent %*' + "`r`nexit /b %errorlevel%`r`n"
[IO.File]::WriteAllText((Join-Path $CaseRoot 'agent.cmd'), $fake)
$existing = $Case.StartsWith('force')
$initial = [pscustomobject]@{user=$existing; running=$existing; enabled=$existing; firewall=$existing; failed=$false}
WriteState $initial
$id='12345678-1234-1234-1234-123456789abc'
$data = Join-Path $env:ProgramData 'pico-utm-agent'
$bin = Join-Path $env:ProgramFiles 'pico-utm-agent'
if ($existing) {
    New-Item -ItemType Directory -Path (Join-Path $data 'spool'), $bin -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $data 'config.toml'), "[agent]`nagent_id = `"$id`"`n")
    [IO.File]::WriteAllText((Join-Path $data 'spool\customer-data'), 'must survive')
    Copy-Item -LiteralPath (Join-Path $CaseRoot 'agent.cmd') -Destination (Join-Path $bin 'agent.cmd')
    [IO.File]::WriteAllText((Join-Path $CaseRoot 'unit'), 'original registration')
}
$beforeConfig = if ($existing) { [IO.File]::ReadAllText((Join-Path $data 'config.toml')) }
& $testScript -Endpoint https://example.test -Token test-token -SiteId test-site -Force:$existing
$rc=$LASTEXITCODE
$after=ReadState
$success=$Case -in @('fresh','force')
if (($rc -eq 0) -ne $success) { throw "Unexpected exit $rc for $Case" }
if ($success) {
    if (-not $after.running -or -not $after.firewall) { throw 'Service/firewall not installed' }
    if ($existing -and -not ([IO.File]::ReadAllText((Join-Path $data 'config.toml')).Contains($id))) { throw 'ID changed' }
} else {
    if ($Case -ne 'occupied' -and -not $after.failed) { throw 'Failure injection was not reached' }
    foreach ($key in @('running','enabled','firewall')) { if ($after.$key -ne $initial.$key) { throw "Rollback mismatch: $key" } }
    if ($existing) {
        if ([IO.File]::ReadAllText((Join-Path $data 'config.toml')) -ne $beforeConfig) { throw 'Config not restored' }
        if ([IO.File]::ReadAllText((Join-Path $CaseRoot 'unit')) -ne 'original registration') { throw 'Service registration changed' }
        if (Get-ChildItem -LiteralPath $data -Filter '.install-backup-*' -Force) { throw 'Backup leaked' }
    } elseif ((Test-Path -LiteralPath $data) -or (Test-Path -LiteralPath $bin) -or (Test-Path -LiteralPath (Join-Path $CaseRoot 'unit'))) { throw 'Partial installation leaked' }
}
if ($existing -and [IO.File]::ReadAllText((Join-Path $data 'spool\customer-data')) -ne 'must survive') { throw 'Customer data changed' }
Write-Host "PASS Windows installer $Case"
exit 0
