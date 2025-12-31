param(
  [int]$BasePort = 7000,
  [int]$Nodes = 3,
  [int]$DurationSec = 20,
  [int]$Difficulty = 16,
  [string]$TxInterval = "1s"
)

$ErrorActionPreference = "Stop"

if ($Nodes -lt 2) {
  throw "Nodes must be >= 2"
}
if ($DurationSec -lt 1) {
  throw "DurationSec must be >= 1"
}

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$repo = Split-Path -Parent $root
Set-Location $repo

$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$outDir = Join-Path $repo "data\tcp-demo\$runId"
$binDir = Join-Path $outDir "bin"
$logDir = Join-Path $outDir "logs"

New-Item -ItemType Directory -Force -Path $binDir | Out-Null
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

$exe = Join-Path $binDir "simchain.exe"
Write-Host "Building $exe ..."
go build -o $exe .\cmd\simchain

$seedAddr = "127.0.0.1:$BasePort"
$procs = @()

try {
  for ($i = 0; $i -lt $Nodes; $i++) {
    $port = $BasePort + $i
    $listen = "127.0.0.1:$port"
    $nodeDir = Join-Path $outDir "node-$port"
    New-Item -ItemType Directory -Force -Path $nodeDir | Out-Null

    $args = @(
      "--transport=tcp",
      "--listen=$listen",
      "--seeds=$seedAddr",
      "--data-dir=$nodeDir",
      "--difficulty=$Difficulty",
      "--duration=$($DurationSec)s",
      "--tx-interval=$TxInterval"
    )
    if ($i -eq 0) {
      # Keep the seed quiet unless you want it to generate txs too.
      $args += "--tx-interval=0"
    }

    $stdout = Join-Path $logDir "node-$port.out.log"
    $stderr = Join-Path $logDir "node-$port.err.log"
    Write-Host "Starting node listen=$listen (logs: $stdout)"

    $p = Start-Process -FilePath $exe -ArgumentList $args -NoNewWindow -PassThru `
      -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    $procs += $p
    Start-Sleep -Milliseconds 150
  }

  Write-Host "Running for $DurationSec seconds..."
  Start-Sleep -Seconds $DurationSec
}
finally {
  foreach ($p in $procs) {
    if ($null -ne $p -and -not $p.HasExited) {
      Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    }
  }
  Write-Host "Done. Logs in: $logDir"
}

