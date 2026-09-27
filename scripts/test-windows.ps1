# Run on Windows as the same user that will operate the console. Cross-building
# on macOS/Linux does not execute DPAPI or validate the Windows credential store.
param(
    [switch]$Browser,
    [string]$ChromePath = $env:CHROME_PATH
)
$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'This test must run on Windows.' }
Push-Location (Join-Path $PSScriptRoot '..')
try {
    & go test -count=1 -v ./internal/credentials -run 'TestDPAPI|TestNativeCredentialStoreSurvivesProcessRestart'
    if ($LASTEXITCODE -ne 0) { throw 'Native DPAPI tests failed.' }
    & go test -count=1 -v ./internal/privatefile ./internal/store -run 'TestWindows|TestPrivate|TestAtomic'
    if ($LASTEXITCODE -ne 0) { throw 'Native private-file ACL tests failed.' }
    & go test -count=1 -v ./cmd/tosub2 -run 'TestWindowsService'
    if ($LASTEXITCODE -ne 0) { throw 'Windows service-control lifecycle tests failed.' }
    & go test -count=1 -v ./internal/browser -run 'TestProcessTreeCancellationAndParentExitCleanDescendants|TestProfileCleanupErrorsRemainObservableAndIdempotent'
    if ($LASTEXITCODE -ne 0) { throw 'Native Windows Job Object process cleanup tests failed.' }
    & go test -count=1 ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go tests failed.' }
    # The race detector requires the supported Windows C toolchain. A missing
    # toolchain is a failure to address, never an implicit successful skip.
    & go test -count=1 -race ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go race tests failed or the C toolchain is missing.' }
    & go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go vet failed.' }
    if ($Browser) {
        if (-not $ChromePath) {
            foreach ($InstallRoot in @($env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:LOCALAPPDATA)) {
                if (-not $InstallRoot) { continue }
                $Candidate = Join-Path $InstallRoot 'Google\Chrome\Application\chrome.exe'
                if (Test-Path -LiteralPath $Candidate -PathType Leaf) { $ChromePath = $Candidate; break }
            }
        }
        if (-not $ChromePath -or -not (Test-Path -LiteralPath $ChromePath -PathType Leaf)) {
            throw 'Browser verification requested but Chrome is missing; pass -ChromePath. This is not a successful skip.'
        }
        $PreviousBrowserTest = $env:TOSUB2_BROWSER_TEST
        $PreviousChromePath = $env:CHROME_PATH
        try {
            $env:TOSUB2_BROWSER_TEST = '1'
            $env:CHROME_PATH = (Resolve-Path -LiteralPath $ChromePath).Path
            & go test -count=1 -race ./...
            if ($LASTEXITCODE -ne 0) { throw 'Native Windows Chromium/React/CLI integration tests failed.' }
        } finally {
            $env:TOSUB2_BROWSER_TEST = $PreviousBrowserTest
            $env:CHROME_PATH = $PreviousChromePath
        }
    } else {
        Write-Warning 'Chromium integration was not run. Re-run with -Browser to verify real Windows browser sessions.'
    }
    New-Item -ItemType Directory -Path bin -Force | Out-Null
    & go build -trimpath -o bin/tosub2.exe ./cmd/tosub2
    if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
} finally { Pop-Location }
