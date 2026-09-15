#!/usr/bin/env bash
# ensure-dpull.sh — 定位或获取 dpull，把可执行文件路径打印到 stdout。
#
# 优先下载官方 Release 里的预编译二进制（不需要 Go）；本机有源码检出 + Go 时改用源码编译。
#
# 退出码约定（给 agent 看的，按它决定下一步，别猜）：
#   0   stdout 就是可用的 dpull 绝对路径
#   10  本机可以装但没装。先向用户说明要装什么、装到哪，取得同意后再加 --install 跑一次
#   11  获取不了（下载失败 / 校验不过 / 网络不通）。把 stderr 原因汇报给用户并停下，
#       不要静默改用 docker pull 或手撕 registry API
#
# 路径只走 stdout，诊断都在 stderr。
#
# 可选参数：--install  --binary 强制下预编译包  --source 强制源码编译
#           --repo DIR  --prefix DIR  --version vX.Y.Z 固定版本  --base URL 镜像前缀
#           --proxy URL 下载走代理（http/https/socks5h），大陆网络直连 GitHub 常失败
#           --min-version vX.Y.Z 本 skill 要求的最低版本（默认 1.1.0；已装副本太旧会触发升级）

set -uo pipefail

repo_hint="${DPULL_REPO:-}"
prefix="${DPULL_PREFIX:-$HOME/.local/bin}"
pin="${DPULL_VERSION:-}"                      # 例 v1.0.0；留空 = latest
# 本 SKILL.md 描述的行为有最低版本要求：低于它的副本会让 agent 按错的假设干活
# （v1.1.0 才有按上游分组的内置改写缓存，1.0.0 上 `pull gcr.io/...` 是必失败的）。
min_version="${DPULL_MIN_VERSION:-1.1.0}"
base="${DPULL_DOWNLOAD_BASE:-}"               # 镜像前缀（大陆网络常需要）
do_install=0
force_binary=0
force_source=0
verbose=0
proxy="${DPULL_PROXY_URL:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --install) do_install=1 ;;
    --repo) shift; repo_hint="${1:-}" ;;
    --prefix) shift; prefix="${1:-}" ;;
    --version) shift; pin="${1:-}" ;;
    --min-version) shift; min_version="${1:-}" ;;
    --base) shift; base="${1:-}" ;;
    --proxy) shift; proxy="${1:-}" ;;
    --binary) force_binary=1 ;;
    --source) force_source=1 ;;
    -v|--verbose) verbose=1 ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 64 ;;
  esac
  shift
done

if [ -n "$proxy" ]; then
  export HTTPS_PROXY="$proxy" HTTP_PROXY="$proxy" ALL_PROXY="$proxy"
  export https_proxy="$proxy" http_proxy="$proxy" all_proxy="$proxy"
fi

say() { printf '%s\n' "$*" >&2; }
path_on() { [ -n "$1" ] && [ -x "$1" ]; }
have() { command -v "$1" >/dev/null 2>&1; }

# version_at_least <二进制> <a.b.c>：拿 `dpull version` 里的版本号做数值比较。
# 取不到版本就当作不满足——宁可多装一次，也不要让一个行为不同的旧副本假装符合本 skill 的描述。
version_at_least() {
  local bin="$1" want="$2" got
  got="$("$bin" version 2>/dev/null | tr -c '0-9.' ' ' | tr -s ' ' '\n' | grep -E '^[0-9]+\.[0-9]+' | head -1)"
  [ -n "$got" ] || return 1
  [ "$(printf '%s\n%s\n' "$got" "$want" | sort -V | head -1)" = "$want" ]
}

REPO_OWNER="eyes2near"
REPO_NAME="dpull"
[ -n "$base" ] || base="https://github.com/$REPO_OWNER/$REPO_NAME/releases/$([ -n "$pin" ] && echo "download/$pin" || echo "latest/download")"

# ── 目标平台对应的产物名 ──────────────────────────────────────────────
asset_name() {
  local os arch
  case "$(uname -s 2>/dev/null)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) return 1 ;;
  esac
  case "$(uname -m 2>/dev/null)" in
    arm64 | aarch64) arch=arm64 ;;
    x86_64 | amd64) arch=amd64 ;;
    *) return 1 ;;
  esac
  printf 'dpull-%s-%s\n' "$os" "$arch"
}

sha256_of() {
  if have sha256sum; then sha256sum "$1" | cut -d' ' -f1
  elif have shasum; then shasum -a 256 "$1" | cut -d' ' -f1
  else return 1; fi
}

fetch() { # fetch URL 落地路径
  if have curl; then
    # 先按默认协商；GitHub 的 HTTP/2 在部分网络会被干扰，退回 HTTP/1.1 再试一次
    curl -fsSL --connect-timeout 10 --retry 2 --retry-delay 2 -o "$2" "$1" 2>/dev/null ||
      curl -fsSL --http1.1 --connect-timeout 15 --retry 2 -o "$2" "$1"
  elif have wget; then
    wget -q --timeout=20 -O "$2" "$1"
  else
    return 1
  fi
}

# Release 附件优先用 gh 取：它走 api.github.com，比 github.com 稳得多
# （github.com 间歇性超时/HTTP2 干扰时，gh 往往还能成）
fetch_release_asset() { # fetch_release_asset 产物名 目标目录
  local a="$1" dir="$2" rel
  rel="${pin:-latest}"
  # Go 的 net/http 只认 http/https 代理，socks 走不通 gh（会白等一次直连超时），
  # 所以配了 socks 代理时直接跳过 gh，用 curl（curl 认 socks）。
  if have gh && [ -z "${proxy##socks*}" ]; then
    say "已配 socks 代理，跳过 gh（Go 不支持 socks 代理环境变量），改用 curl"
  elif have gh; then
    say "用 gh 从 Release($rel) 取 $a …"
    if gh release download "$rel" -R "$REPO_OWNER/$REPO_NAME" -p "$a" -p SHA256SUMS -D "$dir" --clobber >&2 2>&1; then
      [ -s "$dir/$a" ] && [ -s "$dir/SHA256SUMS" ] && return 0
    fi
    say "gh 取包没成功，回退到直接下载 …"
  fi
  fetch "$base/$a" "$dir/$a" || return 1
  fetch "$base/SHA256SUMS" "$dir/SHA256SUMS"
}

# ── 已经有可用副本？ ─────────────────────────────────────────────────
find_repo() {
  local c
  for c in "$repo_hint" "$PWD" "$(CDPATH= cd -- "$(dirname -- "$0")/../../.." 2>/dev/null && pwd)"; do
    [ -n "$c" ] && [ -f "$c/go.mod" ] && grep -q '^module dpull' "$c/go.mod" 2>/dev/null && { printf '%s\n' "$c"; return 0; }
  done
  return 1
}
repo="$(find_repo || true)"

found=""
if path_on "${DPULL:-}"; then found="$DPULL"
else found="$(command -v dpull 2>/dev/null || true)"; path_on "$found" || found=""; fi
path_on "$prefix/dpull" && found="$prefix/dpull"

# 源码比在用的二进制新 → 有 Go 就重装；没 Go 就继续用旧的并说明，别把活卡住
if [ -n "$found" ] && [ -n "$repo" ] && [ -n "$(find "$repo/cmd" "$repo/internal" -name '*.go' -newer "$found" 2>/dev/null | head -1)" ]; then
  if have go; then
    say "检测到的源码比 $found 更新，重新编译安装"
    found=""
    do_install=1
    force_source=1
  else
    say "提示：源码比 $found 新，但本机没有 Go，继续使用现有二进制"
  fi
fi

# 版本闸门：无论用户是否已经 --install 都要跑（带 --install 时清掉 found 就是“同意升级”）。
if [ -n "$found" ]; then
  if ! version_at_least "$found" "$min_version"; then
    if [ -n "$pin" ]; then
      # 用户钉了版本就尊重它，但要把差异说清，别让 agent 以为新行为存在
      say "提示：按 --version $pin 钉住的副本低于 ${min_version}，本 skill 里写的 gcr/k8s 等上游改写缓存可能不存在"
    else
      say "现有副本低于本 skill 要求的 ${min_version}（按上游分组的内置改写缓存需要 1.1.0+），准备升级"
      # 只清掉 found，**不要**动 do_install：它是 --install 的等价开关，
      # 在这里置 1 等于绕开“先跟用户说一声”的契约，默默把用户工具箱里的东西换掉。
      found=""
    fi
  fi
fi

if [ -n "$found" ] && [ "$do_install" -eq 0 ]; then
  printf '%s\n' "$found"
  [ "$verbose" -eq 1 ] && say "$($found version 2>/dev/null || echo '(version 取不到)')"
  exit 0
fi

# ── 需要获取：决定源码编译还是直接下二进制 ───────────────────────────
# 有源码检出且有 Go → 编译（开发者在本仓库里干活，改完就该用新的）；
# 否则直接下官方预编译二进制（不需要 Go）；两者都不成立才退到克隆源码编译。
plan=""
if [ "$force_binary" -eq 1 ] && asset_name >/dev/null 2>&1 && { have curl || have wget; }; then
  plan="binary"
elif { [ "$force_source" -eq 1 ] || [ -n "$repo" ]; } && [ -n "$repo" ] && have go; then
  plan="source"
elif asset_name >/dev/null 2>&1 && { have curl || have wget; }; then
  plan="binary"
elif have go && have git; then
  plan="source-clone"
fi

case "$plan" in
  source)
    say "计划：从本机源码编译并安装到 $prefix"
    ;;
  source-clone)
    say "计划：从 https://github.com/$REPO_OWNER/$REPO_NAME 克隆源码、编译并安装到 ${prefix}（需要 Go）"
    ;;
  binary)
    if ! asset="$(asset_name 2>/dev/null)"; then
      say "本平台 $(uname -s)/$(uname -m) 没有对应的预编译产物。"
      exit 11
    fi
    say "计划：下载官方预编译二进制 ${asset}（$base/${asset}），sha256 校验后安装到 $prefix/dpull（不需要 Go）"
    ;;
  *)
    say "本机既没有可用的下载工具（curl/wget），也没有 Go，无法获取 dpull。"
    say "请让用户手动安装，或提供代理后重试。"
    exit 11
    ;;
esac

if [ "$do_install" -eq 0 ]; then
  say "尚未安装。请先向用户说明上面的计划并取得同意，再用 --install 重跑本脚本。"
  exit 10
fi

mkdir -p "$prefix" 2>/dev/null || { say "无法创建 $prefix"; exit 11; }
tmpdir="$(mktemp -d 2>/dev/null || echo /tmp/dpull-get.$$)"
mkdir -p "$tmpdir"
trap 'rm -rf "$tmpdir"' EXIT

install_binary() {
  asset="$(asset_name)" || { say "不支持的平台: $(uname -s)/$(uname -m)"; return 11; }
  if ! fetch_release_asset "$asset" "$tmpdir"; then
    say "下载失败：$base/$asset"
    say "本机直连 GitHub 受限。按顺序试："
    say "  1) 让用户提供代理：$0 --install --proxy socks5h://127.0.0.1:1080"
    say "  2) 已装 gh 的话本脚本已优先走 gh（走 api.github.com，更稳）"
    say "  3) --base 指定可信镜像前缀（镜像必须同时转发 SHA256SUMS）"
    return 11
  fi
  want="$(awk -v a="$asset" '$2 == a || $2 == "*"a { print $1; exit }' "$tmpdir/SHA256SUMS")"
  if [ -z "$want" ]; then
    say "SHA256SUMS 里没有 $asset 这一条，拒绝安装。"
    return 11
  fi
  got="$(sha256_of "$tmpdir/$asset" 2>/dev/null || true)"
  if [ -z "$got" ]; then
    say "本机没有 sha256sum/shasum，无法校验，拒绝安装。"
    return 11
  fi
  if [ "$want" != "$got" ]; then
    say "sha256 校验不通过，拒绝安装！"
    say "  期望 $want"
    say "  实际 $got"
    say "来源可能被替换（代理/镜像换包）。请让用户从官方 Release 重新获取。"
    return 11
  fi
  say "sha256 校验通过 ${got:0:12}…"
  case "$base" in
    *github.com*) : ;;
    *) say "注意：本次来自镜像 ${base}，校验和也来自同一镜像，只能证明内容自洽，不能证明未被替换。" ;;
  esac
  mv "$tmpdir/$asset" "$prefix/dpull" || { say "无法写入 $prefix/dpull"; return 11; }
  chmod +x "$prefix/dpull"
  return 0
}

install_source() {
  local src="$1"
  (cd "$src" && make install PREFIX="$prefix" >&2) || { say "make install 失败（看上面的编译输出）"; return 11; }
  path_on "$prefix/dpull" || { say "声称成功但找不到 $prefix/dpull"; return 11; }
  return 0
}

target=""
case "$plan" in
  binary)
    install_binary || exit 11
    target="$prefix/dpull"
    ;;
  source)
    install_source "$repo" || exit 11
    target="$prefix/dpull"
    ;;
  source-clone)
    clone="$tmpdir/clone"
    say "正在克隆 https://github.com/$REPO_OWNER/$REPO_NAME …"
    have git || { say "缺 git，无法克隆源码"; exit 11; }
    git clone --depth 1 -q "https://github.com/$REPO_OWNER/$REPO_NAME" "$clone" >&2 || {
      say "克隆失败：多半是无法访问 github.com。请提供代理，或改用预编译二进制方案。"; exit 11; }
    install_source "$clone" || exit 11
    target="$prefix/dpull"
    ;;
esac

# 真跑一次确认能执行（架构不匹配、macOS 拦截等都在这一步暴露）
if ! v="$("$target" version 2>&1)"; then
  say "二进制装上了但跑不起来：$v"
  say "可能是架构不匹配或被系统拦截（macOS 可在「系统设置 → 隐私与安全性」放行）。"
  exit 11
fi
say "已就绪：$v"
printf '%s\n' "$target"
case ":$PATH:" in
  *":$prefix:"*) ;;
  *) say "提示：$prefix 不在 PATH 上，请让用户加入 export PATH=\"$prefix:\$PATH\"，或后续用绝对路径 —— 这不算失败。" ;;
esac
exit 0
