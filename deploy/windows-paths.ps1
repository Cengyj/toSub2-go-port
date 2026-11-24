# Shared by the task and service installers. Keep this file beside them.
function Resolve-ToSub2FileSystemPath([string]$Path) {
    $Provider = $null
    $Drive = $null
    # PowerShell Set-Location need not change .NET's process current directory.
    # Resolve even a not-yet-created path against the invoking PS location.
    $Resolved = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path, [ref]$Provider, [ref]$Drive)
    if ($Provider.Name -ne 'FileSystem') { throw 'Paths must use the FileSystem provider.' }
    if ($Resolved.Contains('"') -or $Resolved.Contains([string][char]13) -or $Resolved.Contains([string][char]10)) {
        throw 'Paths must not contain quotes or newlines.'
    }
    return $Resolved
}

function ConvertTo-ToSub2QuotedPath([string]$Path) {
    # Paths have already rejected quotes. Windows argv parsing requires every
    # trailing backslash to be doubled before the closing double quote.
    return '"' + ($Path -replace '(\\+)$', '$1$1') + '"'
}
