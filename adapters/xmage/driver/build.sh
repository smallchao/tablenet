#!/bin/sh
# SPDX-License-Identifier: MIT
#
# 编 Java 驱动：对着 XMage 服务器的 lib 目录编译，打成 tablenet-xmage-driver.jar（只有本驱动的类，不带 XMage 的 jar）。
#
#   XMAGE_LIB=/opt/xmage/mage-server/lib ./build.sh [输出目录，默认 ../../../bin]
#
# 要 JDK 17 或更新。XMage 升级后要重编：驱动用的 XMage 类必须和服务器同一版。
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out=${1:-"$here/../../../bin"}
: "${XMAGE_LIB:?要设 XMAGE_LIB 为 XMage 服务器的 lib 目录}"
sep=":"
case "$(uname -s 2>/dev/null)" in MINGW*|MSYS*|CYGWIN*) sep=";" ;; esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
javac -encoding UTF-8 --release 17 -cp "$XMAGE_LIB/*" -d "$tmp" "$here"/src/tablenet/xmage/*.java
mkdir -p "$out"
jar --create --file "$out/tablenet-xmage-driver.jar" -C "$tmp" .
echo "$out/tablenet-xmage-driver.jar"
