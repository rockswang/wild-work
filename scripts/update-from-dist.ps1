#Requires -Version 5.1
<#
.SYNOPSIS
  Deploy the newest dist build onto the running wild-work instance.

.DESCRIPTION
  Compares the SHA256 of the build artifact in dist with the deployed instance
  binary. If they are identical there is no new build and the script exits
  quietly (exit code 0). Otherwise it stops the running instance, backs up the
  old binary, copies the new artifact into place under the same file name,
  verifies the copy, and starts the instance again.

  IMPORTANT: run this from YOUR OWN terminal. A process started by the assistant
  may be reclaimed when the assistant turn ends.

.PARAMETER DistDir
  Folder holding the build artifacts. Default: <repo>\dist (parent of this script).

.PARAMETER TargetExe
  Full path of the deployed instance binary. Default: executable path of the
  running instance, or %USERPROFILE%\Downloads\wild-work-windows-amd64.exe.

.PARAMETER ProcessName
  Process name (without .exe) of the running instance.

.PARAMETER StartArgs
  Arguments used to start the new instance. Default: reuse the running instance
  arguments.

.PARAMETER Port
  TCP port probed after start. 0 disables the probe.

.PARAMETER NoBackup
  Do not keep a .bak.exe copy of the replaced binary.

.PARAMETER DryRun
  Report what would happen and exit without touching anything.

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\update-from-dist.ps1 -DryRun

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\update-from-dist.ps1
#>
[CmdletBinding()]
param(
    [string] $DistDir,
    [string] $TargetExe,
    [string] $ProcessName = 'wild-work-windows-amd64',
    [string] $StartArgs,
    [int]    $Port = 7863,
    [switch] $NoBackup,
    [switch] $DryRun
)

$ErrorActionPreference = 'Stop'

function Write-Info([string] $Message) { Write-Host $Message }
function Stop-WithError([string] $Message) { Write-Host ("ERROR: " + $Message) -ForegroundColor Red; exit 1 }
function Get-Sha256([string] $Path) { return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash }

# ---- resolve the dist folder ----------------------------------------------
if (-not $DistDir) {
    $repoRoot = Split-Path -Parent $PSScriptRoot
    $DistDir  = Join-Path $repoRoot 'dist'
}
if (-not (Test-Path -LiteralPath $DistDir -PathType Container)) {
    Stop-WithError ("dist folder not found: " + $DistDir)
}

# ---- resolve the running instance and the target binary -------------------
$running = @(Get-Process -Name $ProcessName -ErrorAction SilentlyContinue)

if (-not $StartArgs -and $running.Count -gt 0) {
    try {
        $ci = Get-CimInstance -ClassName Win32_Process -Filter ("ProcessId=" + $running[0].Id) -ErrorAction SilentlyContinue
        if ($ci -and $ci.CommandLine) {
            $cmd = $ci.CommandLine.Trim()
            if ($cmd.StartsWith('"')) {
                $end = $cmd.IndexOf('"', 1)
                if ($end -ge 0) { $StartArgs = $cmd.Substring($end + 1).Trim() }
            } else {
                $sp = $cmd.IndexOf(' ')
                if ($sp -ge 0) { $StartArgs = $cmd.Substring($sp + 1).Trim() } else { $StartArgs = '' }
            }
        }
    } catch { $StartArgs = '' }
}

$target = $TargetExe
if (-not $target) {
    if ($running.Count -gt 0) {
        try { $target = $running[0].Path } catch { $target = $null }
    }
    if (-not $target) {
        $target = Join-Path (Join-Path $env:USERPROFILE 'Downloads') ($ProcessName + '.exe')
    }
}
$targetDir = Split-Path -Parent $target
if (-not (Test-Path -LiteralPath $targetDir -PathType Container)) {
    Stop-WithError ("target folder not found: " + $targetDir)
}

# ---- pick the artifact, preferring one named like the instance ------------
$targetLeaf = Split-Path -Leaf $target
$candidates = @(
    (Join-Path $DistDir $targetLeaf),
    (Join-Path $DistDir ($ProcessName + '.exe')),
    (Join-Path $DistDir 'wild-work-windows-amd64.exe'),
    (Join-Path $DistDir 'wild-work.exe')
)
$source = $null
foreach ($c in $candidates) {
    if (Test-Path -LiteralPath $c -PathType Leaf) { $source = $c; break }
}
if (-not $source) { Stop-WithError ("no build artifact found in " + $DistDir) }

Write-Info ("source  : " + $source)
Write-Info ("instance: " + $target)

$sourceHash = Get-Sha256 $source
$targetHash = $null
if (Test-Path -LiteralPath $target -PathType Leaf) { $targetHash = Get-Sha256 $target }

Write-Info ("source sha256  : " + $sourceHash)
if ($targetHash) { Write-Info ("instance sha256: " + $targetHash) }
else             { Write-Info  "instance sha256: <not present>" }

if ($targetHash -eq $sourceHash) {
    Write-Info "no new build (hashes identical), nothing to do."
    exit 0
}

Write-Info "new build detected."
if ($DryRun) {
    Write-Info ("[dry-run] would stop '" + $ProcessName + "', back up the old binary, copy the artifact, and start it again.")
    exit 0
}

# ---- stop the running instance (graceful first, then force) ---------------
$running = @(Get-Process -Name $ProcessName -ErrorAction SilentlyContinue)
if ($running.Count -gt 0) {
    foreach ($p in $running) {
        Write-Info ("stopping PID " + $p.Id + " ...")
        try { [void] $p.CloseMainWindow() } catch { }
    }
    $deadline = (Get-Date).AddSeconds(5)
    while ((Get-Date) -lt $deadline -and @(Get-Process -Name $ProcessName -ErrorAction SilentlyContinue).Count -gt 0) {
        Start-Sleep -Milliseconds 300
    }
    foreach ($p in @(Get-Process -Name $ProcessName -ErrorAction SilentlyContinue)) {
        Write-Info ("force stopping PID " + $p.Id)
        Stop-Process -Id $p.Id -Force
    }
    Start-Sleep -Milliseconds 500
} else {
    Write-Info "no running instance found, will just deploy and start."
}

# ---- back up the current binary -------------------------------------------
if ((-not $NoBackup) -and (Test-Path -LiteralPath $target -PathType Leaf)) {
    $backup = $target + '.bak.exe'
    Copy-Item -LiteralPath $target -Destination $backup -Force
    Write-Info ("backup  : " + $backup)
}

# ---- copy the artifact into place (retry while the old process releases it)
$copied = $false
for ($i = 1; $i -le 5; $i++) {
    try {
        Copy-Item -LiteralPath $source -Destination $target -Force
        $copied = $true
        break
    } catch {
        Write-Info ("copy attempt " + $i + " failed: " + $_.Exception.Message)
        Start-Sleep -Seconds 1
    }
}
if (-not $copied) { Stop-WithError "copy failed after 5 attempts" }

# ---- verify the copy on both sides ----------------------------------------
$sourceHashAfter = Get-Sha256 $source
if ($sourceHashAfter -ne $sourceHash) { Stop-WithError ("source changed during copy: " + $source) }
$targetHashAfter = Get-Sha256 $target
if ($targetHashAfter -ne $sourceHash) { Stop-WithError "copied binary hash mismatch" }
Write-Info "copy verified."

# ---- start the instance ---------------------------------------------------
if ($StartArgs) { $newProc = Start-Process -FilePath $target -ArgumentList $StartArgs -PassThru }
else            { $newProc = Start-Process -FilePath $target -PassThru }
Write-Info ("started PID " + $newProc.Id)

# ---- probe the port -------------------------------------------------------
if ($Port -gt 0) {
    $listening = $false
    for ($i = 0; $i -lt 20; $i++) {
        Start-Sleep -Milliseconds 500
        try {
            if (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) { $listening = $true; break }
        } catch { }
    }
    if ($listening) { Write-Info ("port " + $Port + " is listening.") }
    else            { Write-Info ("WARN: port " + $Port + " is not listening yet.") }
}

exit 0
