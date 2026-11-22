# build.ps1 — Windows-first release build for hearth & hearth-agent.
# Requires Go 1.27+. Produces dist\<version>\ artifacts with SHA256 checksums.
#
# Usage:
#   .\scripts\build.ps1                    # current platform (windows amd64)
#   .\scripts\build.ps1 -All               # windows + linux + macOS matrix
param(
    [switch]$All
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
# Single source of truth: internal/version/version.go
$VersionFile = Join-Path $Root "internal\version\version.go"
$Version = (Select-String -Path $VersionFile -Pattern 'const Version = "([^"]+)"').Matches[0].Groups[1].Value
if (-not $Version) { throw "cannot read version from internal\version\version.go" }
$Dist = Join-Path $Root "dist" | Join-Path -ChildPath $Version

if (Get-Command go -ErrorAction SilentlyContinue) {
    $Go = "go"
} else {
    $Go = "$env:ProgramFiles\Go\bin\go.exe"
    if (-not (Test-Path $Go)) { throw "go not found in PATH or $env:ProgramFiles\Go\bin" }
}

New-Item -ItemType Directory -Force -Path $Dist | Out-Null
Push-Location $Root
try {
    $targets = @(
        @{ OS = "windows"; Arch = "amd64"; Ext = ".exe"; Archive = "zip" }
    )
    if ($All) {
        $targets += @(
            @{ OS = "windows"; Arch = "arm64"; Ext = ".exe"; Archive = "zip" },
            @{ OS = "linux";   Arch = "amd64"; Ext = "";      Archive = "tar.gz" },
            @{ OS = "linux";   Arch = "arm64"; Ext = "";      Archive = "tar.gz" },
            @{ OS = "darwin";  Arch = "amd64"; Ext = "";      Archive = "tar.gz" },
            @{ OS = "darwin";  Arch = "arm64"; Ext = "";      Archive = "tar.gz" }
        )
    }

    foreach ($t in $targets) {
        $env:GOOS = $t.OS
        $env:GOARCH = $t.Arch
        $suffix = "${t.OS}_${t.Arch}"

        # hearth (TUI stack; CGO off keeps the build static)
        $env:CGO_ENABLED = "0"
        & $Go build -trimpath -ldflags "-s -w" -o "$Dist\hearth_${suffix}$($t.Ext)" ./cmd/hearth
        if ($LASTEXITCODE -ne 0) { throw "build hearth failed for $suffix" }

        # hearth-agent (headless, no TUI deps)
        & $Go build -trimpath -ldflags "-s -w" -o "$Dist\hearth-agent_${suffix}$($t.Ext)" ./cmd/hearth-agent
        if ($LASTEXITCODE -ne 0) { throw "build hearth-agent failed for $suffix" }

        # archive (zip native on Windows; tar.gz elsewhere)
        if ($t.Archive -eq "zip") {
            $packDir = Join-Path $Root "packaging\windows"
            Compress-Archive -Path "$Dist\hearth_${suffix}$($t.Ext)", "$Dist\hearth-agent_${suffix}$($t.Ext)", `
                "$packDir\INSTALL.txt", "$packDir\run-tui.cmd", "$packDir\run-agent.cmd" `
                -DestinationPath "$Dist\hearth_${suffix}.zip" -Force
            Remove-Item "$Dist\hearth_${suffix}$($t.Ext)", "$Dist\hearth-agent_${suffix}$($t.Ext)"
        } else {
            tar -czf "$Dist/hearth_${suffix}.tar.gz" -C $Dist ("hearth_${suffix}$($t.Ext)", "hearth-agent_${suffix}$($t.Ext)")
            Remove-Item "$Dist\hearth_${suffix}$($t.Ext)", "$Dist\hearth-agent_${suffix}$($t.Ext)"
        }
        Write-Host "built $suffix"
    }

    # sha256 checksums
    $checksums = Get-ChildItem $Dist -File | ForEach-Object {
        $hash = (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower()
        "$hash  $($_.Name)"
    }
    $checksums | Set-Content "$Dist\checksums.sha256"
    Write-Host "`nArtifacts in $Dist :"
    Get-ChildItem $Dist -File | ForEach-Object { Write-Host ("  {0,12}  {1}" -f $_.Length, $_.Name) }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    Pop-Location
}
