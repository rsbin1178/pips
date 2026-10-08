<#
.SYNOPSIS
Installs pips on Windows, macOS or Linux.

.DESCRIPTION
Downloads the release archive for this platform, verifies its SHA-256 against the
published SHA256SUMS, installs the binary, and reports the installed version.
Nothing is installed when the checksum does not match.

Windows (PowerShell 5.1 and newer):
  irm https://raw.githubusercontent.com/rsbin1178/pips/main/install.ps1 | iex

Parameters fall back to the environment, so either form works:

  PIPS_VERSION         release tag to install (default: the latest release)
  PIPS_BIN_DIR         install directory (default: %LOCALAPPDATA%\Programs\pips on
                       Windows, ~/.local/bin elsewhere)
  PIPS_NO_MODIFY_PATH  set to 1 to leave PATH untouched, printing the line instead
  PIPS_REPO            install from a fork (default: rsbin1178/pips)
  PIPS_API_BASE        releases API base (default: https://api.github.com)
  PIPS_DOWNLOAD_BASE   release asset base (default: the GitHub release URL)
  GH_TOKEN             token for the API call, to avoid rate limits

Windows PowerShell 5.1 is supported: the script sets TLS 1.2 itself and only uses
cmdlets that ship with that version. The Windows-specific parts (the user PATH and
%LOCALAPPDATA% default) cannot be exercised on a non-Windows host, so they are
written to fail closed rather than guess.
#>
[CmdletBinding()]
param(
    [string]$Version = $env:PIPS_VERSION,
    [string]$BinDir = $env:PIPS_BIN_DIR,
    [switch]$NoPathUpdate,
    [string]$Repo = $(if ($env:PIPS_REPO) { $env:PIPS_REPO } else { 'rsbin1178/pips' }),
    [string]$ApiBase = $(if ($env:PIPS_API_BASE) { $env:PIPS_API_BASE } else { 'https://api.github.com' }),
    [string]$DownloadBase = $env:PIPS_DOWNLOAD_BASE
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# Windows PowerShell 5.1 defaults to TLS 1.0 and to the IE parsing engine.
$legacy = $PSVersionTable.PSEdition -ne 'Core'
if ($legacy) {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}

$onWindows = $true
$onMacOS = $false
$onLinux = $false
if (-not $legacy) {
    $onWindows = $IsWindows
    $onMacOS = $IsMacOS
    $onLinux = $IsLinux
}

function Say { param([string]$Message) Write-Host "  $Message" }
function Fail {
    param([string]$Message)
    [Console]::Error.WriteLine("pips install: $Message")
    exit 1
}

function Get-Headers {
    $headers = @{ Accept = 'application/vnd.github+json' }

    if ($token) { $headers['Authorization'] = "Bearer $token" }

    return $headers
}

function Invoke-PipsWebRequest {
    param([string]$Uri, [string]$OutFile)

    $arguments = @{ Uri = $Uri; Headers = $(Get-Headers) }

    if ($legacy) { $arguments['UseBasicParsing'] = $true }

    if ($OutFile) { $arguments['OutFile'] = $OutFile }

    return Invoke-WebRequest @arguments
}

function Get-HttpStatus {
    param($ErrorRecord)

    $response = $ErrorRecord.Exception.Response
    if ($null -ne $response -and $null -ne $response.StatusCode) {
        return [int]$response.StatusCode
    }

    return 0
}

function Fail-ForStatus {
    param([int]$Status, [string]$Subject)

    switch ($Status) {
        0 { Fail "could not reach the releases API for $Repo" }
        404 { Fail "$Subject not found in $Repo" }
        403 { Fail "GitHub refused the release lookup (HTTP $Status). Retry later, or set GH_TOKEN to a token with public read access." }
        429 { Fail "GitHub refused the release lookup (HTTP $Status). Retry later, or set GH_TOKEN to a token with public read access." }
        default { Fail "the releases API returned HTTP $Status for $Repo" }
    }
}

$token = $env:GH_TOKEN
if (-not $token -and (Get-Command gh -ErrorAction SilentlyContinue)) {
    try { $token = (gh auth token 2>$null) } catch { $token = $null }
}

if (-not $BinDir) {
    if ($onWindows) {
        $BinDir = Join-Path $env:LOCALAPPDATA 'Programs\pips'
    }
    else {
        $BinDir = Join-Path $HOME '.local/bin'
    }
}

if (-not $DownloadBase) { $DownloadBase = "https://github.com/$Repo/releases/download" }

if ($onWindows) { $os = 'windows' }
elseif ($onMacOS) { $os = 'darwin' }
elseif ($onLinux) { $os = 'linux' }
else { Fail "unsupported operating system" }

switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
    'X64' { $arch = 'amd64' }
    'Arm64' { $arch = 'arm64' }
    default { Fail "unsupported architecture: $([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture)" }
}

$extension = '.zip'
if (-not $onWindows) { $extension = '.tar.gz' }

if ($Version) {
    Say "Resolving release $Version..."
    try {
        $release = Invoke-RestMethod -Uri "$ApiBase/repos/$Repo/releases/tags/$Version" -Headers (Get-Headers)
    }
    catch {
        Fail-ForStatus -Status (Get-HttpStatus $_) -Subject "release $Version"
    }
}
else {
    Say "Resolving the latest release..."
    try {
        $release = Invoke-RestMethod -Uri "$ApiBase/repos/$Repo/releases/latest" -Headers (Get-Headers)
    }
    catch {
        Fail-ForStatus -Status (Get-HttpStatus $_) -Subject 'the latest release'
    }

    $Version = $release.tag_name
    if (-not $Version) { Fail "could not read the tag name from the release response (set PIPS_VERSION)" }
}

$asset = "pips_${Version}_${os}_${arch}$extension"
$base = "$DownloadBase/$Version"
$temp = Join-Path ([System.IO.Path]::GetTempPath()) ("pips-install-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $temp -Force | Out-Null

try {
    Say "Downloading $asset..."
    try {
        Invoke-PipsWebRequest -Uri "$base/$asset" -OutFile (Join-Path $temp $asset) | Out-Null
    }
    catch {
        Fail "could not download $asset from release $Version. That release may have no $os/$arch asset; see https://github.com/$Repo/releases/tag/$Version"
    }

    Say "Downloading SHA256SUMS..."
    try {
        Invoke-PipsWebRequest -Uri "$base/SHA256SUMS" -OutFile (Join-Path $temp 'SHA256SUMS') | Out-Null
    }
    catch {
        Fail "could not download SHA256SUMS from $Version"
    }

    $expected = $null
    foreach ($line in Get-Content (Join-Path $temp 'SHA256SUMS')) {
        $parts = $line -split '\s+'
        if ($parts.Count -ge 2 -and $parts[1] -eq $asset) { $expected = $parts[0]; break }
    }

    if (-not $expected) { Fail "$Version has no checksum for $asset" }

    $actual = (Get-FileHash -Path (Join-Path $temp $asset) -Algorithm SHA256).Hash.ToLowerInvariant()

    if ($expected -ne $actual) {
        [Console]::Error.WriteLine("  expected sha256: $expected")
        [Console]::Error.WriteLine("  actual   sha256: $actual")
        Fail "CHECKSUM MISMATCH. Refusing to install: the download does not match the published checksum."
    }

    Say "Checksum verified: $actual"

    $extracted = Join-Path $temp 'expanded'
    $archive = Join-Path $temp $asset

    if ($extension -eq '.zip') {
        Expand-Archive -Path $archive -DestinationPath $extracted -Force
    }
    else {
        New-Item -ItemType Directory -Path $extracted -Force | Out-Null
        & tar -xzf $archive -C $extracted
        if ($LASTEXITCODE -ne 0) { Fail "could not extract $asset" }
    }

    $binary = Get-ChildItem -Path $extracted -Recurse -File |
        Where-Object { $_.Name -eq 'pips' -or $_.Name -eq 'pips.exe' } |
        Select-Object -First 1

    if (-not $binary) { Fail "the archive did not contain pips" }

    New-Item -ItemType Directory -Path $BinDir -Force | Out-Null

    $target = Join-Path $BinDir $binary.Name
    if (Test-Path $target) { Say "Replacing the existing $target" }

    Move-Item -Path $binary.FullName -Destination $target -Force
    Say "Installed $target"

    $separator = ':'
    if ($onWindows) { $separator = ';' }

    $onPath = ($env:PATH -split [regex]::Escape($separator)) -contains $BinDir

    if (-not $onPath) {
        if ($NoPathUpdate -or $env:PIPS_NO_MODIFY_PATH -eq '1') {
            if ($onWindows) {
                Say "$BinDir is not in your PATH; add it yourself with:"
                Say "  [Environment]::SetEnvironmentVariable('Path', `"$BinDir;`$([Environment]::GetEnvironmentVariable('Path', 'User'))`", 'User')"
            }
            else {
                Say "$BinDir is not on your PATH; add it yourself with:"
                Say "  export PATH='$BinDir':`"`$PATH`""
            }
        }
        elseif ($onWindows) {
            $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
            if (-not $userPath) { $userPath = '' }

            if (($userPath -split ';') -notcontains $BinDir) {
                $updated = $BinDir
                if ($userPath) { $updated = "$BinDir;$userPath" }

                [Environment]::SetEnvironmentVariable('Path', $updated, 'User')
                Say "Added $BinDir to your user PATH; restart your terminal"
            }
        }
        else {
            Say "$BinDir is not on your PATH; add it yourself with:"
            Say "  export PATH='$BinDir':`"`$PATH`""
        }
    }

    Write-Host ''
    & $target version
    if ($LASTEXITCODE -ne 0) { Fail "$target did not run" }

    Write-Host ''
    Write-Host "Installed pips $Version."

    if ($onWindows) {
        Write-Host @'

Windows notes:
  - Native Windows has no sandbox backend, so the default workspace-write mode
    and read-only cannot run commands here. Use WSL2 for the default sandbox, or
    pass --sandbox full-access knowing commands then run unconfined.
  - Run `pips doctor` to check credentials and workspace state.
'@
    }
}
finally {
    if (Test-Path $temp) { Remove-Item -Path $temp -Recurse -Force -ErrorAction SilentlyContinue }
}
