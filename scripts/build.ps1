# 构建:前端产物拷入 embed 目录 + Go 单二进制
# 用法: powershell -File scripts\build.ps1  (产出 backend\douyin-server.exe)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$go = "E:\tmp\tools\go\bin\go.exe"

Push-Location "$root\frontend"
npm run build
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "frontend build failed" }
Pop-Location

# go:embed 不能跨出 module,先把前端产物拷进 backend/internal/api/dist/
Remove-Item "$root\backend\internal\api\dist\*" -Recurse -Force -ErrorAction SilentlyContinue
Copy-Item "$root\frontend\dist\*" "$root\backend\internal\api\dist\" -Recurse -Force

Push-Location "$root\backend"
& $go build -o douyin-server.exe ./cmd/server
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "backend build failed" }
Pop-Location

Write-Host "[build] OK -> $root\backend\douyin-server.exe"
Write-Host "[build] run: .\backend\douyin-server.exe   (http://127.0.0.1:8787, data 在仓库根 data/)"
