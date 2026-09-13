# 建置 Windows amd64 與 Linux amd64 發佈包至 dist\
#   .\scripts\build.ps1 [-Version 1.0.0]
param([string]$Version = '1.0.0')
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
Set-Location $root

$ldflags = "-s -w -X pico-utm-agent/internal/agent.Version=$Version"
$targets = @(
    @{ GOOS = 'windows'; Dir = 'dist\pico-utm-agent-windows-amd64'; Ext = '.exe'; Scripts = @('install.ps1', 'uninstall.ps1', 'verify.ps1') },
    @{ GOOS = 'linux';   Dir = 'dist\pico-utm-agent-linux-amd64';   Ext = '';     Scripts = @('install.sh', 'uninstall.sh', 'verify.sh') }
)

$env:CGO_ENABLED = '0'
$env:GOARCH = 'amd64'
try {
    foreach ($t in $targets) {
        $env:GOOS = $t.GOOS
        $target = [IO.Path]::GetFullPath((Join-Path $root $t.Dir))
        $distRoot = [IO.Path]::GetFullPath((Join-Path $root 'dist')) + [IO.Path]::DirectorySeparatorChar
        if (-not $target.StartsWith($distRoot, [StringComparison]::OrdinalIgnoreCase)) { throw "Unsafe build target: $target" }
        if (Test-Path -LiteralPath $target) {
            if ((Get-Item -LiteralPath $target -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "Refusing reparse point: $target" }
            Remove-Item -Recurse -Force -LiteralPath $target
        }
        New-Item -ItemType Directory -Force -Path $t.Dir | Out-Null
        foreach ($cmd in 'agent', 'mockserver', 'syslog-gen') {
            Write-Host "==> $($t.GOOS)/amd64 $cmd"
            go build -trimpath -ldflags $ldflags -o (Join-Path $t.Dir "$cmd$($t.Ext)") "./cmd/$cmd"
            if ($LASTEXITCODE -ne 0) { throw "建置 $($t.GOOS) $cmd 失敗" }
        }
        foreach ($s in $t.Scripts) {
            $src = Join-Path 'scripts' $s
            $dst = Join-Path $t.Dir $s
            if ($s.EndsWith('.sh')) {
                # shell script 一律 LF、無 BOM
                $text = [IO.File]::ReadAllText((Resolve-Path $src)) -replace "`r`n", "`n"
                [IO.File]::WriteAllText((Join-Path (Resolve-Path $t.Dir) $s), $text, (New-Object Text.UTF8Encoding $false))
            } else {
                Copy-Item $src $dst -Force
            }
        }
        Copy-Item 'configs\config.example.toml' $t.Dir -Force
    }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}
Remove-Item 'dist\mockserver.exe' -ErrorAction SilentlyContinue
Get-ChildItem -Recurse dist -File | Select-Object @{ N = 'File'; E = { $_.FullName.Replace("$root\", '') } }, @{ N = 'MB'; E = { [math]::Round($_.Length / 1MB, 1) } }
