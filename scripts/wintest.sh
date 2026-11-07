#!/usr/bin/env bash
# Runs this repo's Go tests on a Windows machine over ssh, without Go there:
# the test binaries are cross-compiled here and run there, one package at a
# time, each from its folder in a copy of the source tree (tests read their
# testdata relative to it).
#
#   CCWRAP_WINDOWS_HOST=winpc scripts/wintest.sh [packages]   # default ./...
#
# Optional: CCWRAP_WINDOWS_RUN (a -test.run pattern), CCWRAP_WINDOWS_TIMEOUT
# (per package, default 300s), GOARCH (default amd64).
#
# The remote side needs Windows OpenSSH with PowerShell as its shell (the
# default) and tar.exe (Windows 10 1803+). It works in
# ~\ccwrap-wintest, which each run replaces. Prints a summary; full logs land
# in artifacts/wintest/.
set -euo pipefail

host=${CCWRAP_WINDOWS_HOST:?set CCWRAP_WINDOWS_HOST to the ssh host of the Windows machine}
run=${CCWRAP_WINDOWS_RUN:-}
timeout=${CCWRAP_WINDOWS_TIMEOUT:-300s}
arch=${GOARCH:-amd64}
remote='$HOME\ccwrap-wintest'

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
work=$(mktemp -d "${TMPDIR:-/tmp}/ccwrap-wintest.XXXXXX")
trap 'rm -rf "$work"' EXIT
tree="$work/src"
mkdir -p "$tree/wintest"

# Tracked and untracked-but-not-ignored files that exist (not deleted ones);
# no macOS ._ metadata files.
git ls-files -z -co --exclude-standard |
	while IFS= read -r -d '' f; do [ -e "$f" ] && printf '%s\0' "$f"; done |
	COPYFILE_DISABLE=1 tar --null -T - -cf - | tar -xf - -C "$tree"

module=$(go list -m)
[ $# -gt 0 ] || set -- ./...
for pkg in $(GOOS=windows go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' "$@"); do
	rel=${pkg#"$module"}
	rel=${rel#/}
	name=$(printf '%s' "${rel:-root}" | tr '/' '_')
	# No symbol table or DWARF: a third smaller to copy; tracebacks keep names.
	GOOS=windows GOARCH=$arch go test -c -ldflags='-s -w' -o "$tree/wintest/$name.test.exe" "./$rel"
	printf '%s\t%s\n' "${rel:-.}" "$name" >>"$tree/wintest/packages.tsv"
done
[ -s "$tree/wintest/packages.tsv" ] || { echo "no test packages in: $*" >&2; exit 1; }

# PowerShell 5.1 reads a script without a BOM in the ANSI code page.
printf '\xef\xbb\xbf' >"$tree/wintest/run.ps1"
cat >>"$tree/wintest/run.ps1" <<'PS1'
param([string]$Run = '', [string]$Timeout = '300s')
$ErrorActionPreference = 'Continue'
$src = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$logs = Join-Path (Split-Path -Parent $src) 'logs'
New-Item -ItemType Directory -Force -Path $logs | Out-Null
foreach ($line in Get-Content (Join-Path $src 'wintest\packages.tsv')) {
  $rel, $name = $line -split "`t"
  $exe = Join-Path $src "wintest\$name.test.exe"
  $dir = Join-Path $src ($rel -replace '/', '\')
  $log = Join-Path $logs "$name.log"
  $testArgs = @('-test.v', '-test.count=1', "-test.timeout=$Timeout")
  if ($Run) { $testArgs += "-test.run=$Run" }
  $sw = [Diagnostics.Stopwatch]::StartNew()
  $p = Start-Process -FilePath $exe -ArgumentList $testArgs -WorkingDirectory $dir -RedirectStandardOutput $log -RedirectStandardError "$log.err" -NoNewWindow -Wait -PassThru
  $sw.Stop()
  $text = (Get-Content $log -Raw) + (Get-Content "$log.err" -Raw)
  $pass = ([regex]::Matches($text, '(?m)^\s*--- PASS: ')).Count
  $skip = ([regex]::Matches($text, '(?m)^\s*--- SKIP: ')).Count
  $fail = @([regex]::Matches($text, '(?m)^--- FAIL: (\S+)') | ForEach-Object { $_.Groups[1].Value })
  $note = ''
  if ($text -match 'test timed out') { $note = ' TIMEOUT' } elseif ($text -match '(?m)^panic: ') { $note = ' PANIC' }
  $status = if ($p.ExitCode -eq 0) { 'ok  ' } else { 'FAIL' }
  "{0} {1,-28} pass={2,-4} fail={3,-3} skip={4,-3} {5,4:N0}s{6} {7}" -f $status, $rel, $pass, $fail.Count, $skip, $sw.Elapsed.TotalSeconds, $note, ($fail -join ' ')
}
PS1

(cd "$tree" && COPYFILE_DISABLE=1 tar -czf "$work/bundle.tgz" .)
ssh "$host" "Remove-Item -Recurse -Force $remote -ErrorAction SilentlyContinue; New-Item -ItemType Directory -Force -Path $remote\\src | Out-Null"
scp -q "$work/bundle.tgz" "$host:ccwrap-wintest/bundle.tgz"
# Defender may still be scanning the fresh archive; retry briefly.
ssh "$host" "foreach (\$i in 1..10) { tar -xzf $remote\\bundle.tgz -C $remote\\src 2>\$null; if (\$LASTEXITCODE -eq 0) { break }; Start-Sleep 2 }; if (\$LASTEXITCODE -ne 0) { 'could not unpack the bundle'; exit 1 }"
# Windows PowerShell drops empty arguments to programs: pass -Run only when set.
run_arg=
[ -z "$run" ] || run_arg="-Run '$run'"
ssh "$host" "powershell -NoProfile -ExecutionPolicy Bypass -File $remote\\src\\wintest\\run.ps1 -Timeout '$timeout' $run_arg"

rm -rf artifacts/wintest
mkdir -p artifacts
scp -q -r "$host:ccwrap-wintest/logs" artifacts/wintest
echo "logs: artifacts/wintest/"
