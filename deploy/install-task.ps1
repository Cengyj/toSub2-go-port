# Register a per-user login task; no Node, Python, PM2 or stored account password.
param(
    [Parameter(Mandatory=$true)][string]$Binary,
    [string]$OutputDirectory = (Join-Path $env:LOCALAPPDATA 'toSub2\data')
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'windows-paths.ps1')
$ResolvedBinary = Resolve-ToSub2FileSystemPath $Binary
if (-not [System.IO.File]::Exists($ResolvedBinary)) { throw 'Binary must be an existing file.' }
$ResolvedOutput = Resolve-ToSub2FileSystemPath $OutputDirectory
[System.IO.Directory]::CreateDirectory($ResolvedOutput) | Out-Null
$CurrentUser = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$Arguments = '--host=127.0.0.1 --port=4399 --output-root=' + (ConvertTo-ToSub2QuotedPath $ResolvedOutput)
$Action = New-ScheduledTaskAction -Execute $ResolvedBinary -Argument $Arguments -WorkingDirectory (Split-Path -Parent $ResolvedBinary)
$Trigger = New-ScheduledTaskTrigger -AtLogOn -User $CurrentUser
$Principal = New-ScheduledTaskPrincipal -UserId $CurrentUser -LogonType Interactive -RunLevel Limited
$Settings = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName 'toSub2-Go' -Action $Action -Trigger $Trigger -Principal $Principal -Settings $Settings -Description 'toSub2 Go local console, current-user credentials' -Force | Out-Null
Write-Host 'Registered toSub2-Go for next login. Run Start-ScheduledTask -TaskName toSub2-Go to start now.'
