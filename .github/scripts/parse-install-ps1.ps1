<#
.SYNOPSIS
Parses install.ps1 with the PowerShell parser and fails on any syntax error.

.DESCRIPTION
PowerShell has no shellcheck. This is the check that catches a broken installer
before a release tag does, so it lives in a file the repository can run the same
way CI does.

.PARAMETER Path
Script to parse. Defaults to the repository's install.ps1.
#>
[CmdletBinding()]
param([string]$Path = 'install.ps1')

$errors = $null
[void][System.Management.Automation.Language.Parser]::ParseFile($Path, [ref]$null, [ref]$errors)

if ($errors.Count -gt 0) {
    $errors | ForEach-Object { Write-Error $_ }
    exit 1
}

Write-Host "$Path parses cleanly"
