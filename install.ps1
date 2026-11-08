# ccwrap installer for Windows - downloads a prebuilt release, verifies its
# sha256 against the published checksums.txt, and installs ccwrap.exe.
#
#   irm https://raw.githubusercontent.com/Hoper-J/ccwrap/main/install.ps1 | iex
#
# Env knobs:
#   CCWRAP_VERSION         pin a version (e.g. 0.2.0 or v0.2.0); default = latest release
#   CCWRAP_BINDIR          install folder; default = %USERPROFILE%\.local\bin
#   CCWRAP_NO_MODIFY_PATH  set to 1 to leave the user PATH alone

& {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue' # Windows PowerShell downloads crawl with a progress bar
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

  $repo = 'Hoper-J/ccwrap'
  function Fail([string]$msg) { throw "install.ps1: $msg" }

  # --- detect platform -------------------------------------------------------
  # The machine's own architecture, not this PowerShell's: an x64 PowerShell
  # emulated on ARM64 still gets the ARM64 build.
  $machine = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment' -ErrorAction SilentlyContinue).PROCESSOR_ARCHITECTURE
  if (-not $machine) { $machine = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE } }
  $arch = switch ($machine) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Fail "unsupported architecture '$machine'" }
  }

  # --- resolve version -------------------------------------------------------
  $ver = $env:CCWRAP_VERSION
  if (-not $ver) {
    try {
      $ver = (Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$repo/releases/latest").tag_name
    } catch {
      Fail "could not determine the latest version (set CCWRAP_VERSION): $($_.Exception.Message)"
    }
    if (-not $ver) { Fail 'could not determine the latest version (set CCWRAP_VERSION)' }
  }
  $tag = if ($ver.StartsWith('v')) { $ver } else { "v$ver" } # the download path uses the v-prefixed tag
  $num = $tag.Substring(1)                                      # archive names use the bare number

  $base = "https://github.com/$repo/releases/download/$tag"
  $archive = "ccwrap_${num}_windows_${arch}.zip"

  # --- fetch + verify --------------------------------------------------------
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("ccwrap-install-" + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    Write-Host "Downloading $tag (windows/$arch)..."
    try {
      Invoke-WebRequest -UseBasicParsing "$base/$archive" -OutFile (Join-Path $tmp $archive)
    } catch {
      Fail "download failed: $base/$archive ($($_.Exception.Message))"
    }
    try {
      Invoke-WebRequest -UseBasicParsing "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')
    } catch {
      Fail "checksums download failed ($($_.Exception.Message))"
    }

    $want = $null
    foreach ($line in Get-Content (Join-Path $tmp 'checksums.txt')) {
      $fields = -split $line
      if ($fields.Count -eq 2 -and $fields[1] -eq $archive) { $want = $fields[0].ToLowerInvariant() }
    }
    if (-not $want) { Fail "no checksum for $archive in checksums.txt" }
    $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLowerInvariant()
    if ($want -ne $got) { Fail "checksum mismatch for $archive (want $want, got $got)" }
    Write-Host 'Checksum OK.'
    # Note: this checks integrity against checksums.txt (fetched over the same
    # TLS connection) - it is NOT a signature check. For full supply-chain
    # verification (cosign signature pinned to the release's signing
    # identity), see SECURITY.md.

    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath (Join-Path $tmp 'x')
    $exe = Join-Path $tmp 'x\ccwrap.exe'
    if (-not (Test-Path -LiteralPath $exe)) { Fail 'archive did not contain ccwrap.exe' }

    # --- choose bindir + install ---------------------------------------------
    $bindir = if ($env:CCWRAP_BINDIR) { $env:CCWRAP_BINDIR } else { Join-Path $env:USERPROFILE '.local\bin' }
    New-Item -ItemType Directory -Force -Path $bindir | Out-Null
    $dest = Join-Path $bindir 'ccwrap.exe'
    # A running ccwrap.exe (a session, ccwrap ui) cannot be replaced, only
    # renamed: it moves aside, and ccwrap removes it once nothing runs it.
    if (Test-Path -LiteralPath $dest) {
      $old = "ccwrap.exe.old-" + [DateTime]::UtcNow.Ticks
      try { Rename-Item -LiteralPath $dest -NewName $old } catch { Fail "could not replace $dest ($($_.Exception.Message))" }
      Remove-Item -LiteralPath (Join-Path $bindir $old) -ErrorAction SilentlyContinue
    }
    try { Move-Item -LiteralPath $exe -Destination $dest } catch { Fail "could not install to $bindir (set CCWRAP_BINDIR to a writable folder): $($_.Exception.Message)" }
    Write-Host "Installed ccwrap to $dest"

    # --- PATH ------------------------------------------------------------------
    $onPath = ($env:Path -split ';') | Where-Object { $_ -and $_.TrimEnd('\') -ieq $bindir.TrimEnd('\') }
    if (-not $onPath) {
      if ($env:CCWRAP_NO_MODIFY_PATH -eq '1') {
        Write-Host "Note: $bindir is not on your PATH - add it so you can run ccwrap directly."
      } else {
        # Edit the raw registry value, keeping its type: rewriting it through
        # [Environment] would turn %VAR% entries into fixed text.
        $key = Get-Item -LiteralPath 'HKCU:\Environment'
        $raw = $key.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
        if (-not (($raw -split ';') | Where-Object { $_ -and $_.TrimEnd('\') -ieq $bindir.TrimEnd('\') })) {
          $kind = if ($raw) { $key.GetValueKind('Path') } else { 'ExpandString' }
          $new = if (-not $raw) { $bindir } elseif ($raw.EndsWith(';')) { $raw + $bindir } else { "$raw;$bindir" }
          Set-ItemProperty -LiteralPath 'HKCU:\Environment' -Name Path -Value $new -Type $kind
          # Setting a user variable through [Environment] tells running
          # programs (Explorer, new terminals) that the environment changed.
          [Environment]::SetEnvironmentVariable('CCWRAP_INSTALL_PATH_CHANGED', '1', 'User')
          [Environment]::SetEnvironmentVariable('CCWRAP_INSTALL_PATH_CHANGED', $null, 'User')
          Write-Host "Added $bindir to your user PATH (new terminals see it)."
        }
        $env:Path = "$bindir;$env:Path"
      }
    }
    & $dest version
  } finally {
    Remove-Item -Recurse -Force -LiteralPath $tmp -ErrorAction SilentlyContinue
  }
}
