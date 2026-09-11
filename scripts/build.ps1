# 构建:前端产物嵌入 + Go 单二进制
# 用法: powershell -File scripts\build.ps1  (产出 douyin-server.exe)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$go = "E:\tmp\tools\go\bin\go.exe"

Push-Location "$root\frontend"
npm run build
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "frontend build failed" }
Pop-Location

Push-Location $root
& $go build -o douyin-server.exe ./backend/cmd/server
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "backend build failed" }
Pop-Location

Write-Host "[build] OK -> $root\douyin-server.exe"
Write-Host "[build] run: .\douyin-server.exe   (http://127.0.0.1:8787)"
