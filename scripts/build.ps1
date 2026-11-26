<#
.SYNOPSIS
  本地构建 wb2api-panel 可执行程序（参数与 .github/workflows/binaries.yml 对齐）。

.DESCRIPTION
  事实源是 CI：二进制名（wb2api / wb2api.exe）、包结构（dist/<os>-<arch>/ + 同名 zip，
  内含二进制 + config.example.json + README.md）、编译参数（CGO_ENABLED=0、-trimpath、
  -ldflags "-s -w -X main.appVersion=<版本>"）都与 binaries.yml:46-68 保持一致。
  改这里之前先看那边，否则本地产物和 Release 产物会不一样。

  版本号缺省取上海时间 yyyy.M.d.Hmm（月/日/时不补零、分两位），与 AGENTS.md 的发版
  口径一致。注意：本机没有 tzdata 时 `TZ=Asia/Shanghai date` 会静默回落 UTC，
  所以这里不走 TZ，直接 UTC+8——中国无夏令时，等价且不会静默错 8 小时。

.PARAMETER Version
  显式版本号，例如 -Version 2026.11.24.912（想和某个标签对上时用）。

.PARAMETER All
  交叉编译 windows/linux/darwin × amd64/arm64 共六个目标（CI 的全部产物）。

.PARAMETER Zip
  额外产出 dist/wb2api-panel-<版本>-<os>-<arch>.zip（-All 时自动开启）。

.PARAMETER OutDir
  输出目录，缺省 dist。

.EXAMPLE
  # 只构建当前平台，不打包
  .\scripts\build.ps1

.EXAMPLE
  # 六个平台全套 zip，版本号与某个标签一致
  .\scripts\build.ps1 -All -Version 2026.11.24.912
#>
[CmdletBinding()]
param(
    [string]$Version,
    [switch]$All,
    [switch]$Zip,
    [switch]$NoPause,
    [string]$OutDir = "dist"
)

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

# 双击运行时窗口会在脚本结束时立刻关闭，报错根本来不及看（就是「闪退」）。两件事兜住：
#   1) 全过程记进 <OutDir>\build.log，窗口关了也有据可查；
#   2) 结尾停一下等回车（-NoPause 关掉，给 CI / 批处理用）。
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$buildLog = Join-Path (Resolve-Path $OutDir) "build.log"
try { Start-Transcript -Path $buildLog -Force | Out-Null } catch { $buildLog = "" }

function Pause-IfConsole {
    if ($NoPause) { return }
    if ($Host.Name -ne "ConsoleHost") { return }
    Write-Host ""
    [void](Read-Host "按回车关闭窗口（脚本失败时日志已存到 build.log）")
}

trap {
    Write-Host ""
    Write-Host ("构建失败：{0}" -f $_.Exception.Message) -ForegroundColor Red
    if ($buildLog) { Write-Host "完整日志：$buildLog" }
    try { Stop-Transcript | Out-Null } catch { }
    Pause-IfConsole
    exit 1
}

# 先确认 go 在 PATH 上：双击启动的窗口里最常见的「一秒闪退」就是它。
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "PATH 里找不到 go。请先安装 Go，并在能跑 go version 的终端里执行本脚本。"
}

if (-not $Version) {
    if ($env:VERSION) {
        $Version = $env:VERSION
    } else {
        $Version = (Get-Date).ToUniversalTime().AddHours(8).ToString("yyyy.M.d.Hmm")
    }
}

# 目标列表：默认当前平台，-All 时与 CI 一致
if ($All) {
    $targets = @(
        @("windows", "amd64"), @("windows", "arm64"),
        @("linux", "amd64"), @("linux", "arm64"),
        @("darwin", "amd64"), @("darwin", "arm64")
    )
    $Zip = $true
} else {
    $curOS = (go env GOOS).Trim()
    $curArch = (go env GOARCH).Trim()
    $targets = @(, @($curOS, $curArch))
}

Write-Host "版本 $Version"
Write-Host "目标 $($targets.Count) 个：$(($targets | ForEach-Object { "$($_[0])/$($_[1])" }) -join ', ')"

$env:CGO_ENABLED = "0"
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

foreach ($t in $targets) {
    $os = $t[0]
    $arch = $t[1]
    $env:GOOS = $os
    $env:GOARCH = $arch

    $bin = if ($os -eq "windows") { "wb2api.exe" } else { "wb2api" }
    $dir = Join-Path $OutDir "$os-$arch"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $exe = Join-Path $dir $bin

    # 注意：go 往 stderr 打的任何一行（哪怕是 warning）在 $ErrorActionPreference=Stop 下都会
    # 变成 NativeCommandError 终止脚本，所以用 2> 落文件，只以 $LASTEXITCODE 判成败。
    $errFile = [System.IO.Path]::GetTempFileName()
    $ErrorActionPreference = "Continue"
    & go build -trimpath -ldflags "-s -w -X main.appVersion=$Version" -o $exe ./cmd/server 2> $errFile
    $code = $LASTEXITCODE
    $ErrorActionPreference = "Stop"
    if ($code -ne 0) {
        $msg = if (Test-Path $errFile) { Get-Content $errFile -Raw } else { "" }
        Remove-Item $errFile -Force -ErrorAction SilentlyContinue
        if ($msg) { Write-Host $msg.Trim() }
        if ($msg -match "Access is denied") {
            Write-Host "提示：Go 构建缓存/模块缓存目录不可写（默认 %LOCALAPPDATA%\go-build、%USERPROFILE%\go\pkg\mod）。"
            Write-Host "     设 $env:GOCACHE 到可写路径再重试，例如 $env:TEMP\wb2api-gocache。"
        }
        throw "go build 失败：$os/$arch（exit $code）"
    }
    Remove-Item $errFile -Force -ErrorAction SilentlyContinue
    Copy-Item config.example.json, README.md -Destination $dir -Force

    $size = [math]::Round((Get-Item $exe).Length / 1MB, 1)
    Write-Host ("  构建完成 {0}/{1} -> {2} ({3} MB)" -f $os, $arch, $exe, $size)

    if ($Zip) {
        # PowerShell 变量名大小写不敏感：这个变量绝不能叫 $zip，否则会把 -Zip 开关本身
        # 覆盖成字符串，再赋值时就报 Cannot convert System.String to SwitchParameter。
        $zipPath = Join-Path $OutDir "wb2api-panel-$Version-$os-$arch.zip"
        if (Test-Path $zipPath) { Remove-Item $zipPath -Force }
        # 只打目录内容，条目落在压缩包根部（与 CI 的 python zipfile 写法一致）
        Compress-Archive -Path (Join-Path $dir "*") -DestinationPath $zipPath -CompressionLevel Optimal
        $zsize = [math]::Round((Get-Item $zipPath).Length / 1MB, 1)
        Write-Host ("  打包完成 {0} ({1} MB)" -f $zipPath, $zsize)
    }
}

Write-Host "全部完成。产物在 $(Resolve-Path $OutDir)"
if ($buildLog) { Write-Host "构建日志：$buildLog" }
try { Stop-Transcript | Out-Null } catch { }
Pause-IfConsole
