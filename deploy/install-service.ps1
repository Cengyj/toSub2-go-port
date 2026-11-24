# Requires elevated PowerShell. This file registers a service only when run.
# Use the SAME Windows user as the existing console so DPAPI data stays usable.
[CmdletBinding(SupportsShouldProcess=$true)]
param(
    [Parameter(Mandatory=$true)][string]$Binary,
    [string]$OutputDirectory = (Join-Path $env:LOCALAPPDATA 'toSub2\data'),
    [ValidateRange(1,65535)][int]$Port = 4399,
    [System.Management.Automation.PSCredential]$Credential
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'windows-paths.ps1')
$ServiceName = 'toSub2-Go'
$ResolvedBinary = Resolve-ToSub2FileSystemPath $Binary
if (-not [System.IO.File]::Exists($ResolvedBinary)) { throw 'Binary must be an existing file.' }
$ResolvedOutput = Resolve-ToSub2FileSystemPath $OutputDirectory
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    throw 'toSub2-Go already exists. Stop and review the existing service before changing its identity or paths.'
}
$Identity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $Credential) { $Credential = Get-Credential -UserName $Identity.Name -Message 'Use the current Windows account for DPAPI-compatible service storage.' }
if (-not $Credential) { throw 'No service credential was provided.' }
$Account = New-Object System.Security.Principal.NTAccount($Credential.UserName)
$ServiceSID = $Account.Translate([System.Security.Principal.SecurityIdentifier])
if ($ServiceSID.Value -ne $Identity.User.Value) { throw 'The service account must match the current user to preserve DPAPI access.' }
$BinaryPath = (ConvertTo-ToSub2QuotedPath $ResolvedBinary) + ' --windows-service --host=127.0.0.1 --port=' + $Port + ' --output-root=' + (ConvertTo-ToSub2QuotedPath $ResolvedOutput)
if ($PSCmdlet.ShouldProcess($ServiceName, 'Register native Go service under current Windows user')) {
    [System.IO.Directory]::CreateDirectory($ResolvedOutput) | Out-Null
    New-Service -Name $ServiceName -DisplayName 'toSub2 Go Console' -BinaryPathName $BinaryPath -Credential $Credential -StartupType Automatic -Description 'Native Go local console with graceful SCM shutdown and current-user DPAPI.' | Out-Null
    Write-Host 'Registered; not started. Start-Service toSub2-Go to start; Stop-Service toSub2-Go waits for Go cleanup.'
    Write-Host 'The account needs the Windows Log on as a service right.'
}
