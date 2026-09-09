# Dependency-free Windows regression tests. Go builds only the harmless stub below.
# Run: powershell -NoProfile -File scripts/test/install.test.ps1
$ErrorActionPreference = "Stop"
Set-StrictMode -Version 2.0
if ($env:OS -ne "Windows_NT") { throw "These installer tests require Windows." }

$installer = Join-Path (Split-Path $PSScriptRoot -Parent) "install.ps1"
$root = Join-Path ([System.IO.Path]::GetTempPath()) "tailtab-install-test-$([guid]::NewGuid())"
$stub = Join-Path $root "stub.exe"
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  "AMD64" { "amd64" }
  "ARM64" { "arm64" }
  default { throw "unsupported test architecture: $env:PROCESSOR_ARCHITECTURE" }
}
$asset = "tailtab-windows-$arch.exe"
$settings = @{
  LOCALAPPDATA = (Join-Path $root "profile")
  TAILTAB_VERSION = "0.0.0-test"
  TAILTAB_TEST_LOG = (Join-Path $root "install.log")
  GOENV = "off"
  GO111MODULE = "off"
  GOWORK = "off"
  GOTOOLCHAIN = "local"
  GOOS = "windows"
  GOARCH = $arch
  GOFLAGS = ""
  GOCACHE = (Join-Path $root "go-cache")
  GOPATH = (Join-Path $root "go")
  GOTELEMETRY = "off"
}
$savedEnv = @{}

function Assert($Condition, [string]$Message) {
  if (-not $Condition) { throw $Message }
}

function Test-Install {
  param(
    [string]$Name,
    [string]$Checksum = "binary",
    [string]$HostState = "closed",
    [switch]$Fresh,
    [string]$DownloadFailure = "",
    [string]$ExpectedError = ""
  )
  $caseRoot = Join-Path $root $Name
  $env:LOCALAPPDATA = Join-Path $caseRoot "profile"
  $env:TAILTAB_TEST_LOG = Join-Path $caseRoot "install.log"
  $installedDir = Join-Path $env:LOCALAPPDATA "tailtab"
  $installedPath = Join-Path $installedDir "tailtab.exe"
  New-Item -ItemType Directory -Path $installedDir -Force | Out-Null
  $oldHash = $null
  if (-not $Fresh) {
    Copy-Item -LiteralPath $stub -Destination $installedPath
    # A PE overlay distinguishes the previous binary without changing its behavior.
    $stream = [System.IO.File]::Open($installedPath, [System.IO.FileMode]::Append)
    try { $stream.WriteByte(1) } finally { $stream.Dispose() }
    $oldHash = (Get-FileHash -LiteralPath $installedPath).Hash
  }
  $ready = Join-Path $caseRoot "ready"
  $stop = Join-Path $caseRoot "stop"
  $state = @{ Process = $null; Lock = $null; ChecksumFile = $null; Requests = 0 }

  function Start-TestHost([string]$Path) {
    $state.Process = Start-Process -FilePath $Path -ArgumentList @(
      "wait", "`"$ready`"", "`"$stop`"") -PassThru
    $timer = [System.Diagnostics.Stopwatch]::StartNew()
    while (-not (Test-Path -LiteralPath $ready) -and $timer.Elapsed.TotalSeconds -lt 10) {
      Assert (-not $state.Process.HasExited) "Stub host exited before becoming ready."
      Start-Sleep -Milliseconds 50
    }
    Assert (Test-Path -LiteralPath $ready) "Stub host did not become ready."
  }

  function Invoke-WebRequest {
    param([string]$Uri, [string]$OutFile, [switch]$UseBasicParsing)
    $state.Requests++
    Assert $UseBasicParsing "The installer must support Windows PowerShell basic parsing."
    if ($OutFile) {
      Assert ([System.IO.Path]::GetFullPath($OutFile).StartsWith(
        "$caseRoot\", [System.StringComparison]::OrdinalIgnoreCase)) "Download escaped test directory."
    }
    if ($Uri -eq "https://github.com/Stocist/Tailtab/releases/download/v0.0.0-test/$asset") {
      Assert ($OutFile -ne "") "Binary download must use OutFile."
      Copy-Item -LiteralPath $stub -Destination $OutFile
      if ($DownloadFailure -eq "binary") { throw "simulated binary download failure" }
    } elseif ($Uri -eq "https://github.com/Stocist/Tailtab/releases/download/v0.0.0-test/SHA256SUMS") {
      $text = "$newHash  $asset`r`n"
      if ($Checksum -eq "missing") { $text = "$newHash  other.exe`r`n" }
      if ($Checksum -eq "mismatch") { $text = "$('0' * 64)  $asset`r`n" }
      $bytes = [System.Text.Encoding]::UTF8.GetBytes($text)
      if ($OutFile) {
        $state.ChecksumFile = $OutFile
        [System.IO.File]::WriteAllBytes($OutFile, $bytes)
      } else {
        # PowerShell 5.1 returns byte[] Content for application/octet-stream.
        if ($Checksum -eq "text") { $content = $text } else { $content = $bytes }
        $contentType = if ($Checksum -eq "text") { "text/plain" } else { "application/octet-stream" }
        [pscustomobject]@{ Content = $content; Headers = @{ "Content-Type" = $contentType } }
      }
      if ($DownloadFailure -eq "checksum") { throw "simulated checksum download failure" }
      if ($HostState -eq "active" -or $HostState -eq "elsewhere") {
        $runningPath = $installedPath
        if ($HostState -eq "elsewhere") {
          $otherDir = Join-Path $caseRoot "other"
          New-Item -ItemType Directory -Path $otherDir | Out-Null
          $runningPath = Join-Path $otherDir "tailtab.exe"
          Copy-Item -LiteralPath $stub -Destination $runningPath
        }
        # Start during the downloads to ensure the activity check is near replacement.
        Start-TestHost $runningPath
      }
    } else {
      throw "Unexpected network request: $Uri"
    }
  }

  function Get-Process {
    [CmdletBinding()]
    param([string]$Name)
    $found = @(Microsoft.PowerShell.Management\Get-Process @PSBoundParameters)
    if ($HostState -eq "running-race") { Start-TestHost $installedPath }
    if ($HostState -eq "locked") {
      # Simulate a destination becoming locked after the process check.
      $state.Lock = [System.IO.File]::Open($installedPath, [System.IO.FileMode]::Open,
        [System.IO.FileAccess]::Read, [System.IO.FileShare]::None)
    }
    $found
  }

  try {
    $failure = ""
    try { & $installer } catch { $failure = $_.Exception.Message }
    if ($state.Lock) { $state.Lock.Dispose(); $state.Lock = $null }
    if ($ExpectedError) {
      Assert ($failure -like "*$ExpectedError*") "${Name}: expected '$ExpectedError', got '$failure'."
      Assert (-not (Test-Path -LiteralPath $env:TAILTAB_TEST_LOG)) "${Name}: registration ran after failure."
      if ($Fresh) {
        Assert (-not (Test-Path -LiteralPath $installedPath)) "${Name}: failed fresh install left a binary."
      } else {
        Assert ((Get-FileHash -LiteralPath $installedPath).Hash -eq $oldHash) "${Name}: existing binary changed."
      }
      if ($HostState -in @("active", "running-race", "locked")) {
        Assert ($failure -like "*Close all browsers and retry*") "${Name}: missing recovery instructions."
      }
    } else {
      Assert ($failure -eq "") "${Name}: unexpected installer failure: $failure"
      Assert ((Get-FileHash -LiteralPath $installedPath).Hash -eq $newHash) "${Name}: new binary was not installed."
      $arguments = [System.IO.File]::ReadAllText($env:TAILTAB_TEST_LOG)
      Assert ($arguments -eq "install`n--edge-id`nkejfineblfbjfolkgjkancapnpknomod`n--gecko-id`ntailtab@stocist.dev") "${Name}: registration arguments changed."
    }
    if ($state.Process) {
      Assert (-not $state.Process.HasExited) "${Name}: installer stopped a running host."
    }
    if ($DownloadFailure -ne "binary") {
      Assert ($state.Requests -eq 2) "${Name}: unexpected download count."
      Assert ($null -ne $state.ChecksumFile) "${Name}: checksum must be downloaded to a file."
      Assert (-not (Test-Path -LiteralPath $state.ChecksumFile)) "${Name}: checksum download was not cleaned up."
    }
    $leftovers = @(Get-ChildItem -LiteralPath $installedDir -Force | Where-Object { $_.Name -ne "tailtab.exe" })
    Assert ($leftovers.Count -eq 0) "${Name}: temporary downloads were not cleaned up."
    Write-Host "PASS $Name"
  } finally {
    if ($state.Lock) { $state.Lock.Dispose() }
    if ($state.Process) {
      [System.IO.File]::WriteAllText($stop, "stop")
      Assert ($state.Process.WaitForExit(10000)) "Test stub did not stop."
      $state.Process.Dispose()
    }
  }
}

try {
  New-Item -ItemType Directory -Path $root | Out-Null
  foreach ($name in $settings.Keys) {
    $savedEnv[$name] = [System.Environment]::GetEnvironmentVariable($name, "Process")
    [System.Environment]::SetEnvironmentVariable($name, $settings[$name], "Process")
  }
  $source = Join-Path $root "stub.go"
  @'
package main

import (
    "os"
    "strings"
    "time"
)

func main() {
    if len(os.Args) == 4 && os.Args[1] == "wait" {
        if err := os.WriteFile(os.Args[2], []byte("ready"), 0600); err != nil { panic(err) }
        for {
            if _, err := os.Stat(os.Args[3]); err == nil { return }
            time.Sleep(20 * time.Millisecond)
        }
    }
    if len(os.Args) > 1 && os.Args[1] == "install" {
        err := os.WriteFile(os.Getenv("TAILTAB_TEST_LOG"), []byte(strings.Join(os.Args[1:], "\n")), 0600)
        if err != nil { panic(err) }
        return
    }
    os.Exit(2)
}
'@ | Set-Content -LiteralPath $source -Encoding ASCII
  & go build -o $stub $source
  Assert ($LASTEXITCODE -eq 0) "Could not build the harmless test host."
  $newHash = (Get-FileHash -LiteralPath $stub).Hash.ToLower()

  Test-Install "valid-text" -Checksum text -Fresh
  Test-Install "valid-binary" -Fresh
  Test-Install "missing-checksum" -Checksum missing -ExpectedError "SHA256SUMS has no entry"
  Test-Install "mismatched-checksum" -Checksum mismatch -ExpectedError "checksum mismatch"
  Test-Install "fresh-mismatch" -Checksum mismatch -Fresh -ExpectedError "checksum mismatch"
  Test-Install "active-host" -HostState active -ExpectedError "Tailtab is running at"
  Test-Install "closed-host"
  Test-Install "other-host-path" -HostState elsewhere
  Test-Install "host-start-race" -HostState running-race -ExpectedError "Could not replace"
  Test-Install "replacement-lock-race" -HostState locked -ExpectedError "Could not replace"
  Test-Install "binary-download-failure" -DownloadFailure binary -ExpectedError "simulated binary download failure"
  Test-Install "checksum-download-failure" -DownloadFailure checksum -ExpectedError "simulated checksum download failure"
  Write-Host "All installer tests passed."
} finally {
  foreach ($name in $savedEnv.Keys) {
    [System.Environment]::SetEnvironmentVariable($name, $savedEnv[$name], "Process")
  }
  if (Test-Path -LiteralPath $root) { Remove-Item -LiteralPath $root -Recurse -Force }
}
