//go:build windows

package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Execute the real installer with real PowerShell path resolution and Windows
// argv parsing. Every registration cmdlet is replaced before the script runs;
// these fixtures never create a service/task or authenticate the synthetic
// credential. They must execute natively on Windows, not under a PS emulator.
const windowsInstallerHarness = `
param([string]$FixturePath)
$ErrorActionPreference = 'Stop'
$Fixture = Get-Content -LiteralPath $FixturePath -Raw -Encoding UTF8 | ConvertFrom-Json
$global:InstallerReceipt = @{ Calls = 0; Error = ''; Execute = ''; Arguments = ''; WorkingDirectory = ''; CommandLine = '' }
function New-ScheduledTaskAction {
    param($Execute, $Argument, $WorkingDirectory)
    return @{ Execute = $Execute; Arguments = $Argument; WorkingDirectory = $WorkingDirectory }
}
function New-ScheduledTaskTrigger { param([switch]$AtLogOn, $User) return @{ User = $User } }
function New-ScheduledTaskPrincipal { param($UserId, $LogonType, $RunLevel) return @{ UserId = $UserId } }
function New-ScheduledTaskSettingsSet {
    param($RestartCount, $RestartInterval, $ExecutionTimeLimit, $MultipleInstances, [switch]$AllowStartIfOnBatteries, [switch]$DontStopIfGoingOnBatteries)
    return @{ MultipleInstances = $MultipleInstances }
}
function Register-ScheduledTask {
    param($TaskName, $Action, $Trigger, $Principal, $Settings, $Description, [switch]$Force)
    $global:InstallerReceipt.Calls++
    $global:InstallerReceipt.Execute = $Action.Execute
    $global:InstallerReceipt.Arguments = $Action.Arguments
    $global:InstallerReceipt.WorkingDirectory = $Action.WorkingDirectory
}
function Get-Service { param($Name, $ErrorAction) return $null }
function Get-Credential { throw 'Installer unexpectedly requested an interactive credential.' }
function New-Service {
    param($Name, $DisplayName, $BinaryPathName, $Credential, $StartupType, $Description)
    $global:InstallerReceipt.Calls++
    $global:InstallerReceipt.CommandLine = $BinaryPathName
}
try {
    Set-Location -LiteralPath $Fixture.Location
    [System.Environment]::CurrentDirectory = $Fixture.ProcessDirectory
    if ($Fixture.Kind -eq 'task') {
        & $Fixture.Installer -Binary $Fixture.Binary -OutputDirectory $Fixture.Output
    } else {
        $CurrentUser = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
        $Password = ConvertTo-SecureString 'synthetic-not-a-real-password' -AsPlainText -Force
        $Credential = New-Object System.Management.Automation.PSCredential($CurrentUser, $Password)
        & $Fixture.Installer -Binary $Fixture.Binary -OutputDirectory $Fixture.Output -Credential $Credential -WhatIf:$Fixture.WhatIf
    }
} catch {
    $global:InstallerReceipt.Error = $_.Exception.Message
} finally {
    [System.IO.File]::WriteAllText($Fixture.Receipt, ($global:InstallerReceipt | ConvertTo-Json -Depth 6))
}
`

type windowsInstallerFixture struct {
	Kind, Installer, Binary, Output, Location, ProcessDirectory, Receipt string
	WhatIf                                                               bool
}

type windowsInstallerReceipt struct {
	Calls                                                    int
	Error, Execute, Arguments, WorkingDirectory, CommandLine string
}

func TestWindowsInstallersBindOutputToInvokingPowerShellLocation(t *testing.T) {
	for _, kind := range []string{"task", "service"} {
		for _, test := range []struct {
			name, output string
			absolute     bool
			wantError    bool
			whatIf       bool
		}{
			{name: "relative-with-spaces-and-literal-brackets", output: `data pool\key[01]`},
			{name: "relative-with-trailing-separator", output: `data pool\trailing\`},
			{name: "absolute-with-trailing-separator", output: `absolute data\`, absolute: true},
			{name: "reject-non-filesystem-provider", output: `env:PATH`, wantError: true},
			{name: "reject-quote", output: `bad"path`, wantError: true},
			{name: "reject-newline", output: "bad\npath", wantError: true},
			{name: "service-what-if", output: `what if data`, whatIf: true},
		} {
			if test.whatIf && kind != "service" {
				continue
			}
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				base := t.TempDir()
				location := filepath.Join(base, "invoking PS location")
				processDirectory := filepath.Join(base, "different process directory")
				binary := filepath.Join(base, "binary files", "tosub2.exe")
				for _, directory := range []string{location, processDirectory, filepath.Dir(binary)} {
					if err := os.MkdirAll(directory, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(binary, []byte("synthetic binary; never executed"), 0600); err != nil {
					t.Fatal(err)
				}
				_, source, _, _ := runtime.Caller(0)
				output := test.output
				if test.absolute {
					output = base + `\` + output
				}
				fixture := windowsInstallerFixture{
					Kind: kind, Installer: filepath.Join(filepath.Dir(source), "..", "deploy", "install-"+kind+".ps1"),
					Binary: binary, Output: output, Location: location, ProcessDirectory: processDirectory,
					Receipt: filepath.Join(base, "receipt.json"), WhatIf: test.whatIf,
				}
				input, err := json.Marshal(fixture)
				if err != nil {
					t.Fatal(err)
				}
				fixturePath, harnessPath := filepath.Join(base, "fixture.json"), filepath.Join(base, "harness.ps1")
				for path, data := range map[string][]byte{fixturePath: input, harnessPath: []byte(windowsInstallerHarness)} {
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				command := exec.CommandContext(t.Context(), "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", harnessPath, "-FixturePath", fixturePath)
				command.Dir = processDirectory
				if out, err := command.CombinedOutput(); err != nil {
					t.Fatalf("execute isolated installer: %v\n%s", err, out)
				}
				data, err := os.ReadFile(fixture.Receipt)
				if err != nil {
					t.Fatal(err)
				}
				var receipt windowsInstallerReceipt
				if err := json.Unmarshal(data, &receipt); err != nil {
					t.Fatal(err)
				}
				if test.wantError {
					if receipt.Error == "" || receipt.Calls != 0 {
						t.Fatalf("invalid path reached registration: %+v", receipt)
					}
					return
				}
				if receipt.Error != "" {
					t.Fatalf("installer rejected valid fixture: %+v", receipt)
				}
				wantOutput := filepath.Clean(output)
				if !test.absolute {
					wantOutput = filepath.Join(location, output)
				}
				if test.whatIf {
					if _, err := os.Stat(wantOutput); !os.IsNotExist(err) || receipt.Calls != 0 {
						t.Fatal("WhatIf created data or reached registration", receipt, err)
					}
					return
				}
				if receipt.Calls != 1 {
					t.Fatalf("registration count=%d want=1", receipt.Calls)
				}
				if info, err := os.Stat(wantOutput); err != nil || !info.IsDir() {
					t.Fatal("data directory was not created at the invoking PS location", err)
				}
				commandLine := receipt.CommandLine
				wantArgs := []string{binary, "--windows-service", "--host=127.0.0.1", "--port=4399"}
				if kind == "task" {
					if receipt.Execute != binary || receipt.WorkingDirectory != filepath.Dir(binary) {
						t.Fatalf("task changed executable or working directory: %+v", receipt)
					}
					commandLine = windows.EscapeArg(receipt.Execute) + " " + receipt.Arguments
					wantArgs = []string{binary, "--host=127.0.0.1", "--port=4399"}
				}
				args, err := windows.DecomposeCommandLine(commandLine)
				if err != nil || len(args) != len(wantArgs)+1 || !reflect.DeepEqual(args[:len(wantArgs)], wantArgs) {
					t.Fatalf("Windows argv mismatch: %q %v", args, err)
				}
				argumentOutput := strings.TrimPrefix(args[len(wantArgs)], "--output-root=")
				if argumentOutput == args[len(wantArgs)] || !filepath.IsAbs(argumentOutput) || filepath.Clean(argumentOutput) != wantOutput {
					t.Fatalf("data creation and command refer to different paths: argv=%q created=%q", argumentOutput, wantOutput)
				}
				if !test.absolute {
					for _, wrongBase := range []string{processDirectory, filepath.Dir(binary)} {
						if _, err := os.Stat(filepath.Join(wrongBase, output)); !os.IsNotExist(err) {
							t.Fatal("installer also created data under a different current directory", wrongBase, err)
						}
					}
				}
			})
		}
	}
}
