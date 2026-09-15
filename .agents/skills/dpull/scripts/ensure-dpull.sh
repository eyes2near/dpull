#!/usr/bin/env bash
# ensure-dpull.sh — 定位或安装 dpull，把可执行文件路径打印到 stdout。
#
# 约定（给 agent 看的）：
#   退出 0  stdout 就是可用的 dpull 路径，直接用
#   退出 10 本机可以装但没装：先向用户说明要装什么，取得同意后再加 --install 跑一次
#   退出 11 装不了（缺 Go / 找不到源码 / 拉不下来）：按 stderr 的原因向用户汇报，
#          不要静默改用 docker pull 或手撕 registry API
#
# 只用 stdout 拿路径，诊断都在 stderr，所以 `DPULL=$(... | tail -1)` 之类是安全的。

set -uo pipefail

repo_hint="${DPULL_REPO:-}"
prefix="${DPULL_PREFIX:-$HOME/.local/bin}"
do_install=0
want_version=0

while [ $# -gt 0 ]; do
  case "$1" in
    --install) do_install=1 ;;
    --repo) shift; repo_hint="${1:-}" ;;
    --prefix) shift; prefix="${1:-}" ;;
    --version) want_version=1 ;;
    -h|--help)
      sed -n '2,14p' "$0"
      exit 0
      ;;
    *) echo "未知参数: $1" >&2; exit 64 ;;
  esac
  shift
done

say() { printf '%s\n' "$*" >&2; }
path_on() { [ -n "$1" ] && [ -x "$1" ]; }

# 已有可用副本？
found=""
if path_on "${DPULL:-}"; then
  found="$DPULL"
else
  found="$(command -v dpull 2>/dev/null || true)"
  path_on "$found" || found=""
fi
path_on "$prefix/dpull" && found="$prefix/dpull"

# 找源码检出：显式给的 --repo > 当前目录 > 本脚本所在仓库（skill 随仓库分发时）
find_repo() {
  local c
  for c in "$repo_hint" "$PWD" "$(CDPATH= cd -- "$(dirname -- "$0")/../../.." 2>/dev/null && pwd)"; do
    [ -n "$c" ] && [ -f "$c/go.mod" ] && grep -q '^module dpull' "$c/go.mod" 2>/dev/null && { printf '%s\n' "$c"; return 0; }
  done
  return 1
}
repo="$(find_repo || true)"

# 源码比在用的二进制更新？那就重装，别拿旧副本干活
if [ -n "$found" ] && [ -n "$repo" ] && [ -n "$(find "$repo/cmd" "$repo/internal" -name '*.go' -newer "$found" 2>/dev/null | head -1)" ]; then
  say "检测到的源码比 $found 更新，重新安装"
  found=""
  do_install=1
fi

if [ -n "$found" ] && [ "$do_install" -eq 0 ]; then
  printf '%s\n' "$found"
  [ "$want_version" -eq 1 ] && say "$($found version 2>/dev/null || echo '(version 取不到)')"
  exit 0
fi

# 到这里说明确实没有可用的 dpull
if [ -z "$repo" ]; then
  say "本机没有 dpull，也没有源码检出。"
  say "可以从公开仓库装：https://github.com/eyes2near/dpull （需要 git 与 Go 工具链）"
  if ! command -v go >/dev/null 2>&1; then
    say "缺 Go 工具链（go 不在 PATH），装不了：请让用户安装 Go，或让用户自己装好 dpull。"
    exit 11
  fi
  if [ "$do_install" -eq 0 ]; then
    say "已具备安装条件（git+go）。请先向用户说明「要从 github.com/eyes2near/dpull 克隆并编译安装到 $prefix」，得到同意后用 --install 重跑。"
    exit 10
  fi
  tmp="$(mktemp -d 2>/dev/null || echo /tmp/dpull-clone.$$)"
  rm -rf "$tmp"
  say "正在克隆 https://github.com/eyes2near/dpull …"
  if ! git clone --depth 1 -q https://github.com/eyes2near/dpull "$tmp" >&2; then
    say "克隆失败：多半是无法访问 github.com（需要代理）。请让用户提供代理或镜像。"
    exit 11
  fi
  repo="$tmp"
  trap 'rm -rf "$tmp"' EXIT
fi

if ! command -v go >/dev/null 2>&1; then
  say "有源码但没有 Go 工具链，无法编译：$repo"
  say "请让用户安装 Go，或改用已装好的 dpull。"
  exit 11
fi

say "正在从源码安装到 $prefix …"
if ! (cd "$repo" && make install PREFIX="$prefix" >&2); then
  say "make install 失败（看上面的编译输出）。"
  exit 11
fi
if ! path_on "$prefix/dpull"; then
  say "安装声称成功但找不到 $prefix/dpull"
  exit 11
fi
printf '%s\n' "$prefix/dpull"
case ":$PATH:" in
  *":$prefix:"*) say "已安装，且 $prefix 在 PATH 上：可直接用 dpull" ;;
  *) say "已安装，但 $prefix 不在 PATH 上，请让用户加入：export PATH=\"$prefix:\$PATH\"（或后续都用绝对路径）" ;;
esac
exit 0
