# 建置 Windows amd64 與 Linux amd64 Agent 發佈包至 dist\
#   .\scripts\build.ps1 [-Version 1.0.0]
param([string]$Version = '1.0.0')
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
Set-Location $root

$ldflags = "-s -w -X pico-utm-agent/internal/agent.Version=$Version"
$targets = @(
    @{ GOOS = 'windows'; Dir = "dist\pico-utm-agent-$Version-windows-amd64"; Ext = '.exe'; Scripts = @('install.ps1', 'uninstall.ps1') },
    @{ GOOS = 'linux';   Dir = "dist\pico-utm-agent-$Version-linux-amd64";   Ext = '';     Scripts = @('install.sh', 'uninstall.sh') }
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
        Write-Host "==> $($t.GOOS)/amd64 agent"
        go build -trimpath -ldflags $ldflags -o (Join-Path $t.Dir "agent$($t.Ext)") './cmd/agent'
        if ($LASTEXITCODE -ne 0) { throw "建置 $($t.GOOS) agent 失敗" }
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
        Copy-Item 'README.md' $t.Dir -Force
    }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}
$linuxDir = "dist\pico-utm-agent-$Version-linux-amd64"
$windowsDir = "dist\pico-utm-agent-$Version-windows-amd64"
$linuxArchive = "dist\pico-utm-agent-$Version-linux-amd64.tar.gz"
$windowsArchive = "dist\pico-utm-agent-$Version-windows-amd64.zip"
Remove-Item -Force -LiteralPath $linuxArchive, $windowsArchive -ErrorAction SilentlyContinue
tar -czf $linuxArchive -C dist (Split-Path $linuxDir -Leaf)
if ($LASTEXITCODE -ne 0) { throw '建立 Linux tar.gz 失敗' }
Compress-Archive -LiteralPath $windowsDir -DestinationPath $windowsArchive -CompressionLevel Optimal
Get-FileHash -Algorithm SHA256 $linuxArchive, $windowsArchive |
    ForEach-Object { "$($_.Hash.ToLower())  $(Split-Path $_.Path -Leaf)" } |
    Set-Content -LiteralPath "dist\SHA256SUMS-$Version.txt" -Encoding ascii
Get-ChildItem dist -File | Select-Object Name, @{ N = 'MB'; E = { [math]::Round($_.Length / 1MB, 1) } }
