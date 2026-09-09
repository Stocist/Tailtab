# Installs the Tailtab native host on Windows and registers it with the
# browsers on this machine. No admin needed; everything goes under the user's
# profile and HKCU.
#
#   irm https://raw.githubusercontent.com/Stocist/Tailtab/main/scripts/install.ps1 | iex
#
# Environment:
#   TAILTAB_VERSION   release to install, e.g. 0.2.3 (default: latest)
$ErrorActionPreference = "Stop"

$repo = "Stocist/Tailtab"
$edgeId = "kejfineblfbjfolkgjkancapnpknomod"
$geckoId = "tailtab@stocist.dev"

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  "ARM64" { "arm64" }
  "AMD64" { "amd64" }
  default { throw "unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
}

if ($env:TAILTAB_VERSION) {
  $base = "https://github.com/$repo/releases/download/v$($env:TAILTAB_VERSION.TrimStart('v'))"
} else {
  $base = "https://github.com/$repo/releases/latest/download"
}
$asset = "tailtab-windows-$arch.exe"
$dir = Join-Path $env:LOCALAPPDATA "tailtab"
$dest = Join-Path $dir "tailtab.exe"

New-Item -ItemType Directory -Force -Path $dir | Out-Null
$tmp = Join-Path $dir "$asset.$([guid]::NewGuid()).download"
$sumsPath = "$tmp.SHA256SUMS"
$backupPath = "$tmp.previous"

try {
  Write-Host "downloading $base/$asset"
  Invoke-WebRequest -Uri "$base/$asset" -OutFile $tmp -UseBasicParsing
  Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sumsPath -UseBasicParsing
  $sums = Get-Content -LiteralPath $sumsPath

  $want = ($sums | Where-Object { $_ -match "\s$([regex]::Escape($asset))\s*$" } | ForEach-Object { ($_ -split "\s+")[0] })
  if (-not $want) { throw "SHA256SUMS has no entry for $asset" }
  $got = (Get-FileHash -Algorithm SHA256 $tmp).Hash.ToLower()
  if ($got -ne $want.ToLower()) { throw "checksum mismatch for ${asset}: got $got, want $want" }

  if (Get-Process -Name tailtab -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $dest }) {
    throw "Tailtab is running at '$dest'. Close all browsers and retry; the existing binary has not been changed."
  }
  $destinationLock = $null
  try {
    if (Test-Path -LiteralPath $dest) {
      # A write handle refuses mapped executables, including a host started after the check.
      # Allow reads/deletes for Replace, but block new executable mappings during the swap.
      $destinationLock = [System.IO.File]::Open($dest, [System.IO.FileMode]::Open,
        [System.IO.FileAccess]::Write, ([System.IO.FileShare]::Read -bor [System.IO.FileShare]::Delete))
      [System.IO.File]::Replace($tmp, $dest, $backupPath)
    } else {
      [System.IO.File]::Move($tmp, $dest)
    }
  } catch {
    $replacementError = $_.Exception.Message
    if (Test-Path -LiteralPath $backupPath) {
      # Replace can fail after renaming the original; never overwrite a concurrent winner.
      try { [System.IO.File]::Move($backupPath, $dest) } catch {
        throw "Could not replace '$dest'. Close all browsers and retry. The previous binary is preserved at '$backupPath'. $replacementError"
      }
    }
    throw "Could not replace '$dest'. Close all browsers and retry; the destination may be locked. $replacementError"
  } finally {
    if ($destinationLock) { $destinationLock.Dispose() }
  }
  Remove-Item -LiteralPath $backupPath -Force -ErrorAction SilentlyContinue
} finally {
  Remove-Item -LiteralPath $tmp, $sumsPath -Force -ErrorAction SilentlyContinue
}

Write-Host "installed $dest"
& $dest install --edge-id $edgeId --gecko-id $geckoId
if ($LASTEXITCODE -ne 0) { throw "tailtab install failed with exit code $LASTEXITCODE" }

Write-Host @"

Host installed. Now add the extension to your browser:

  Zen / Firefox:  open $base/tailtab-<version>.xpi in the browser
                  (signed by Mozilla; installs permanently and self-updates)
  Edge / Chrome:  unzip $base/tailtab-chromium-<version>.zip and load it
                  unpacked from edge://extensions or chrome://extensions
                  with developer mode on

The binary is not code-signed yet, so SmartScreen may warn the first time a
browser starts it. Release page: https://github.com/$repo/releases/latest
"@
