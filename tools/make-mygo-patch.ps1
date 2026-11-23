# 把 .patched-mygo 相对上游 v0.8.1 的改动，导出成一份能直接 git apply 的 patch。
#
# 为什么要有这个文件：仓库里那份 .patched-mygo 是整棵 mygo 的副本，光看它
# 不知道相对上游到底改了哪几行。这个脚本每次把差异重新算一遍，输出到
# patches/mygo-<版本>.patch；-Verify 会把它应用到一个干净的上游副本上，
# 逐个文件比对，确认 patch 就是当前在编的那份补丁。
#
# 用法：
#   pwsh -NoProfile -File tools\make-mygo-patch.ps1
#   pwsh -NoProfile -File tools\make-mygo-patch.ps1 -Verify
param(
  [string]$Version = 'v0.8.1',
  [string]$Out = 'patches\mygo-v0.8.1.patch',
  [switch]$Verify
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$patched = Join-Path $root '.patched-mygo'
$upstream = Join-Path (go env GOMODCACHE) "github.com\egoist\mygo@$Version"
if (-not (Test-Path -LiteralPath $upstream)) { throw "上游源码不在模块缓存里：$upstream" }

# 补丁涉及的十一个文件。加文件要同时改这里，否则 -Verify 会报"还有差异没进 patch"。
$files = @(
  'content.go',
  'internal/gpu/d3d11/d3d11.go',
  'internal/windows/compositor.go',
  'internal/windows/surface.go',
  'internal/windows/window.go',
  'ui/frame.go',
  'ui/headless.go',
  'ui/runtime.go',
  'ui/services.go',
  'ui/widgets.go',
  'ui/window.go'
)

function Convert-ToRelativePatch([string]$rel, [string]$raw) {
  # PowerShell 从原生命令收尾时可能带回一个裸 \r（不在 \n 前面），它会被当成
  # 行尾字符写进 patch，git apply 就再也对不上上下文。源文件都是 LF，直接去掉。
  $raw = $raw -replace "`r", ''
  $lines = $raw -split "`n"
  # git diff 的每一行（包括空行）都是上下文，空行不能丢：丢了 hunk 行数就对不上。
  $result = New-Object System.Collections.Generic.List[string]
  for ($i = 0; $i -lt $lines.Count; $i++) {
    $line = $lines[$i]
    if ($line -like 'diff --git *') {
      $result.Add("diff --git a/$rel b/$rel")
    } elseif ($line -match '^index [0-9a-f]+\.\.[0-9a-f]+') {
      $result.Add($line)
    } elseif ($line -match '^--- ') {
      $result.Add("--- a/$rel")
    } elseif ($line -match '^\+\+\+ ') {
      $result.Add("+++ b/$rel")
    } elseif ($i -eq $lines.Count - 1 -and $line -eq '') {
      # 结尾那个换行不是内容
    } else {
      $result.Add($line)
    }
  }
  return ($result -join "`n")
}

$chunks = New-Object System.Collections.Generic.List[string]
foreach ($rel in $files) {
  $u = Join-Path $upstream ($rel -replace '/', '\')
  $p = Join-Path $patched ($rel -replace '/', '\')
  if (-not (Test-Path -LiteralPath $p)) { throw "补丁文件不存在：$p" }
  $raw = & git diff --no-index -U3 --src-prefix=a/ --dst-prefix=b/ -- "$u" "$p" 2>&1
  if ($LASTEXITCODE -eq 0) { continue }   # 没有差异
  $chunks.Add((Convert-ToRelativePatch $rel ($raw -join "`n")))
}

$header = @"
# 本文件由 tools\make-mygo-patch.ps1 生成，不要手改。
#
# 上游：github.com/egoist/mygo $Version
# 应用方式（在 mygo 源码根目录）：
#   git apply -p1 patches/mygo-$Version.patch
#
# 十条改动解决的问题见 docs/architecture.md 的「mygo 补丁」一节。

"@
$outPath = Join-Path $root $Out
$dir = Split-Path -Parent $outPath
if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Path $dir | Out-Null }
$text = $header + ($chunks -join "`n")
# 用 WriteAllText 而不是 Set-Content：后者会在末尾追加一个平台换行（Windows 上是
# CRLF），那一个 \r 落在最后一行上下文末尾，git apply 会判成上下文不匹配。
[IO.File]::WriteAllText($outPath, $text + "`n", (New-Object System.Text.UTF8Encoding($false)))
"写出 $outPath（$($chunks.Count) 个文件）"

if ($Verify) {
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("mygo-verify-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
  Copy-Item -LiteralPath $upstream -Destination $tmp -Recurse
  Push-Location $tmp
  try {
    # autocrlf 会把写出来的文件变成 CRLF，和仓库里的 LF 一对比就全是差异；
    # 关掉它，应用后的文件才和 .patched-mygo 逐字节一致。
    & git -c core.autocrlf=false apply -p1 --whitespace=nowarn $outPath
    if ($LASTEXITCODE -ne 0) { throw "patch 应用失败" }
    foreach ($rel in $files) {
      $a = Join-Path $tmp ($rel -replace '/', '\')
      $b = Join-Path $patched ($rel -replace '/', '\')
      if ((Get-FileHash -LiteralPath $a).Hash -ne (Get-FileHash -LiteralPath $b).Hash) {
        throw "应用 patch 后与 .patched-mygo 不一致：$rel"
      }
    }
    "验证通过：patch 应用后的 $($files.Count) 个文件与 .patched-mygo 完全一致"
  } finally {
    Pop-Location
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}
