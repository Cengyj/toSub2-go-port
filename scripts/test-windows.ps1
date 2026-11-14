# Run as the Windows user that will operate the console. Cross-building on
# macOS/Linux does not execute DPAPI or validate the Windows credential store.
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
    & go test -count=1 ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go tests failed.' }
    # The race detector requires the supported Windows C toolchain. A missing
    # toolchain is a failure to address, never an implicit successful skip.
    & go test -count=1 -race ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go race tests failed or the C toolchain is missing.' }
    & go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go vet failed.' }
    # Keep verification outputs separate from installed/workspace executables.
    # The complete builder owns the frontend lock and forces CGO_ENABLED=0.
    $VerificationBuildDir = Join-Path ([System.IO.Path]::GetTempPath()) ('tosub2-verify-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $VerificationBuildDir | Out-Null
    try {
        & go run ./cmd/build-web -build-go -build-dir $VerificationBuildDir
        if ($LASTEXITCODE -ne 0) { throw 'Complete frontend and Go build failed.' }
    } finally {
        Remove-Item -LiteralPath $VerificationBuildDir -Recurse -Force
    }
} finally { Pop-Location }
