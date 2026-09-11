# 抖音归档工具 v2 —— 签名侧车启动脚本
# 用法: powershell -File sidecar\run.ps1 --port 18787 --token <t> [--mock]
$ErrorActionPreference = "Stop"

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$python = Join-Path $here ".venv\Scripts\python.exe"
$main = Join-Path $here "main.py"

if (-not (Test-Path $python)) {
    Write-Error "sidecar/.venv 不存在。请先执行: python -m venv $here\.venv; $here\.venv\Scripts\pip install -r $here\requirements.txt"
}

& $python $main @args
exit $LASTEXITCODE
