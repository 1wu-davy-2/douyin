# 开发模式:mock 后端(:8787)+ Vite dev server(:5173)
# 用法: powershell -File scripts\dev.ps1
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$go = "E:\tmp\tools\go\bin\go.exe"

Write-Host "[dev] backend :8787 (DY_MOCK=1)  frontend :5173 (proxy -> 8787)"
$env:DY_MOCK = "1"
$env:DY_DATA_DIR = "$root\data"
$backend = Start-Process -FilePath $go -ArgumentList "run","./cmd/server" -WorkingDirectory "$root\backend" -PassThru -NoNewWindow
try {
  Push-Location "$root\frontend"
  npm run dev
} finally {
  Pop-Location
  if (!$backend.HasExited) { Stop-Process -Id $backend.Id -Force }
}
