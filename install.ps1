<#
.SYNOPSIS
    Install cli-to-api on Windows.

.DESCRIPTION
    irm https://raw.githubusercontent.com/baydisng13/cli-to-api/main/install.ps1 | iex

    Environment variables:
      CLI_TO_API_VERSION      Tag to install (e.g. v0.1.0). Default: latest release.
      CLI_TO_API_INSTALL_DIR  Install directory.
                              Default: $env:LOCALAPPDATA\Programs\cli-to-api

    Installs for the current user only. No administrator rights are required
    and the machine-wide PATH is never touched.

    Requires Windows PowerShell 5.1 or PowerShell 7+.
#>

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo    = 'baydisng13/cli-to-api'
$Binary  = 'cli-to-api'
$ExeName = "$Binary.exe"

function Write-Info { param([string]$Message) Write-Host $Message }

function Get-TargetArch {
    # PROCESSOR_ARCHITECTURE is the most portable signal across PS 5.1 and 7.
    $arch = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $arch = $env:PROCESSOR_ARCHITEW6432 }

    switch ($arch) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        'x86'   {
            throw "32-bit Windows (x86) is not supported. cli-to-api ships amd64 and arm64 builds only."
        }
        default {
            throw "Unsupported processor architecture: '$arch'. cli-to-api ships amd64 and arm64 builds only."
        }
    }
}

function Resolve-Version {
    if ($env:CLI_TO_API_VERSION) {
        Write-Info "Using requested version: $($env:CLI_TO_API_VERSION)"
        return $env:CLI_TO_API_VERSION
    }

    Write-Info "Resolving latest release of $Repo ..."
    try {
        $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
                                     -Headers @{ 'User-Agent' = 'cli-to-api-installer' } `
                                     -UseBasicParsing
    }
    catch {
        throw "Could not query the GitHub API for the latest release of $Repo. Either there are no published releases yet, or this request was rate-limited. Set `$env:CLI_TO_API_VERSION = 'vX.Y.Z' to install a specific tag. Underlying error: $($_.Exception.Message)"
    }

    $tag = $release.tag_name
    if ([string]::IsNullOrWhiteSpace($tag)) {
        throw "GitHub API did not return a tag_name for the latest release of $Repo."
    }
    if ($tag -notmatch '^v[0-9][A-Za-z0-9.+_-]*$') {
        throw "Refusing to use unexpected version string from GitHub API: '$tag'"
    }

    Write-Info "Latest release: $tag"
    return $tag
}

function Get-ExpectedChecksum {
    param(
        [Parameter(Mandatory = $true)][string]$ChecksumsPath,
        [Parameter(Mandatory = $true)][string]$ArchiveName
    )

    foreach ($line in (Get-Content -LiteralPath $ChecksumsPath)) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        # Format: "<sha256>  <filename>" (GNU coreutils may prefix * for binary).
        $parts = $line -split '\s+', 2
        if ($parts.Count -lt 2) { continue }
        $name = $parts[1].Trim().TrimStart('*')
        $name = Split-Path -Leaf $name
        if ($name -eq $ArchiveName) { return $parts[0].Trim() }
    }
    return $null
}

function Add-ToUserPath {
    param([Parameter(Mandatory = $true)][string]$Directory)

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($null -eq $userPath) { $userPath = '' }

    $entries = $userPath -split ';' | Where-Object { $_ -ne '' }
    $already = $entries | Where-Object { $_.TrimEnd('\') -ieq $Directory.TrimEnd('\') }

    if ($already) {
        Write-Info "$Directory is already on your user PATH."
        return
    }

    $newPath = if ($userPath.TrimEnd(';') -eq '') { $Directory } else { "$($userPath.TrimEnd(';'));$Directory" }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')

    # Make it usable in the current session too.
    $env:Path = "$env:Path;$Directory"

    Write-Info ""
    Write-Info "Added $Directory to your user PATH."
    Write-Info "Open a new terminal for this to take effect everywhere."
}

# --------------------------------------------------------------------- main ---

$tempDir = $null
try {
    # TLS 1.2 is not the default on stock Windows PowerShell 5.1.
    try {
        [Net.ServicePointManager]::SecurityProtocol = `
            [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    }
    catch { }

    $arch    = Get-TargetArch
    $version = Resolve-Version

    $archiveName  = "{0}_{1}_windows_{2}.zip" -f $Binary, $version, $arch
    $baseUrl      = "https://github.com/$Repo/releases/download/$version"
    $archiveUrl   = "$baseUrl/$archiveName"
    $checksumsUrl = "$baseUrl/checksums.txt"

    $installDir = if ($env:CLI_TO_API_INSTALL_DIR) {
        $env:CLI_TO_API_INSTALL_DIR
    }
    else {
        Join-Path $env:LOCALAPPDATA 'Programs\cli-to-api'
    }

    $tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("cli-to-api-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

    $archivePath   = Join-Path $tempDir $archiveName
    $checksumsPath = Join-Path $tempDir 'checksums.txt'

    Write-Info "Downloading $archiveName ..."
    try {
        Invoke-WebRequest -Uri $archiveUrl -OutFile $archivePath -UseBasicParsing
    }
    catch {
        throw "Download failed: $archiveUrl (is there a windows/$arch build for $version`?) Underlying error: $($_.Exception.Message)"
    }

    Write-Info "Downloading checksums.txt ..."
    Invoke-WebRequest -Uri $checksumsUrl -OutFile $checksumsPath -UseBasicParsing

    if ((Get-Item -LiteralPath $archivePath).Length -eq 0) {
        throw "Downloaded archive is empty: $archiveName"
    }
    if ((Get-Item -LiteralPath $checksumsPath).Length -eq 0) {
        throw "Downloaded checksums.txt is empty."
    }

    $expected = Get-ExpectedChecksum -ChecksumsPath $checksumsPath -ArchiveName $archiveName
    if (-not $expected) {
        throw "No checksum entry for $archiveName in checksums.txt. Aborting."
    }

    $actual = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash
    if ($actual -ine $expected) {
        Write-Host "CHECKSUM MISMATCH for $archiveName" -ForegroundColor Red
        Write-Host "  expected: $expected" -ForegroundColor Red
        Write-Host "  actual:   $actual" -ForegroundColor Red
        throw "Aborting: the downloaded archive does not match the published checksum."
    }
    Write-Info "Checksum OK (sha256 $($actual.ToLowerInvariant()))"

    # Confirm it really is a zip before extracting.
    $magic = [System.IO.File]::ReadAllBytes($archivePath)[0..1]
    if ($magic[0] -ne 0x50 -or $magic[1] -ne 0x4B) {
        throw "Downloaded file is not a ZIP archive: $archiveName"
    }

    $extractDir = Join-Path $tempDir 'extract'
    New-Item -ItemType Directory -Path $extractDir -Force | Out-Null
    Expand-Archive -LiteralPath $archivePath -DestinationPath $extractDir -Force

    $extractedExe = Join-Path $extractDir $ExeName
    if (-not (Test-Path -LiteralPath $extractedExe)) {
        throw "Archive did not contain $ExeName."
    }

    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    $targetExe = Join-Path $installDir $ExeName

    try {
        Copy-Item -LiteralPath $extractedExe -Destination $targetExe -Force
    }
    catch {
        throw "Could not write $targetExe. If cli-to-api is currently running, close it and re-run. Underlying error: $($_.Exception.Message)"
    }

    Write-Info ""
    Write-Info "Installed: $targetExe"
    try {
        $installed = & $targetExe version 2>$null
        Write-Info "Version:   $installed"
    }
    catch {
        Write-Info "Version:   (could not run binary)"
    }

    Add-ToUserPath -Directory $installDir
}
finally {
    if ($tempDir -and (Test-Path -LiteralPath $tempDir)) {
        Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
