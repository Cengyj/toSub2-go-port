# Requires elevated PowerShell. This file registers a service only when run.
# Use the SAME Windows user as the existing console so DPAPI data stays usable.
# Services have no interactive desktop; use install-task.ps1 or the foreground
# console when a challenge needs a visible browser for manual interaction.
[CmdletBinding(SupportsShouldProcess=$true)]
param(
    [Parameter(Mandatory=$true)][string]$Binary,
    [string]$OutputDirectory = (Join-Path $env:LOCALAPPDATA 'toSub2\data'),
    [string]$ChromePath,
    [ValidateRange(1,65535)][int]$Port = 4399,
    [System.Management.Automation.PSCredential]$Credential
)
$ErrorActionPreference = 'Stop'
$ServiceName = 'toSub2-Go'
$ResolvedBinary = (Resolve-Path -LiteralPath $Binary).Path
$ResolvedOutput = [System.IO.Path]::GetFullPath($OutputDirectory)
$Paths = @($ResolvedBinary, $ResolvedOutput)
if ($ChromePath) { $ChromePath = (Resolve-Path -LiteralPath $ChromePath).Path; $Paths += $ChromePath }
foreach ($Path in $Paths) {
    if ($Path.Contains('"') -or $Path.Contains([string][char]13) -or $Path.Contains([string][char]10)) { throw 'Paths must not contain quotes or newlines.' }
}
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    throw 'toSub2-Go already exists. Stop and review the existing service before changing its identity or paths.'
}
$Identity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $Credential) { $Credential = Get-Credential -UserName $Identity.Name -Message 'Use the current Windows account for DPAPI-compatible service storage.' }
if (-not $Credential) { throw 'No service credential was provided.' }
$Account = New-Object System.Security.Principal.NTAccount($Credential.UserName)
$ServiceSID = $Account.Translate([System.Security.Principal.SecurityIdentifier])
if ($ServiceSID.Value -ne $Identity.User.Value) { throw 'The service account must match the current user to preserve DPAPI access.' }
$BinaryPath = '"' + $ResolvedBinary + '" --windows-service --host=127.0.0.1 --port=' + $Port + ' --output-root="' + $ResolvedOutput + '"'
if ($ChromePath) { $BinaryPath += ' --chrome-path="' + $ChromePath + '"' }
if ($PSCmdlet.ShouldProcess($ServiceName, 'Register native Go service under current Windows user')) {
    New-Item -ItemType Directory -Path $ResolvedOutput -Force | Out-Null
    New-Service -Name $ServiceName -DisplayName 'toSub2 Go Console' -BinaryPathName $BinaryPath -Credential $Credential -StartupType Automatic -Description 'Native Go local console with graceful SCM shutdown and current-user DPAPI.' | Out-Null
    Write-Host 'Registered; not started. Start-Service toSub2-Go to start; Stop-Service toSub2-Go waits for Go cleanup.'
    Write-Host 'The account needs the Windows Log on as a service right. Use the interactive console for visible browser challenges.'
}
