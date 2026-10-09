#!/bin/sh
# The shim's gate: built against glibc, it must also load under musl, whose
# loader refuses to start a process whose preload has an unresolved symbol.
#
#   sh symbols.sh amd64|arm64 [path]
#
# Fails unless DT_NEEDED is exactly libc.so.6 and libdl.so.2, every strong
# undefined symbol is exported by musl 1.2.3, and no GLIBC_ version above 2.17
# is required.
set -eu

arch="${1:-}"
case "$arch" in
  amd64) platform=linux/amd64 ;;
  arm64) platform=linux/arm64 ;;
  *) echo "usage: $0 amd64|arm64 [path]" >&2; exit 2 ;;
esac

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../../.." && pwd)"
so="${2:-v1alpha1/attach/shell/ttyshim/ttyshim-$arch.so}"
[ -f "$root/$so" ] || { echo "symbols: $so missing" >&2; exit 2; }

docker run --rm --platform "$platform" -v "$root:/w:ro" -w /w \
  alpine:3.17@sha256:8fc3dacfb6d69da8d44e42390de777e48577085db99aa4e4af35f483eb08b989 sh -euc '
  apk add --no-cache binutils >/dev/null
  so="$1"
  musl=$(ls /lib/ld-musl-*.so.1)

  needed=$(objdump -p "$so" | awk "/NEEDED/ {print \$2}" | sort | tr "\n" " ")
  [ "$needed" = "libc.so.6 libdl.so.2 " ] || { echo "symbols: DT_NEEDED must be exactly libc.so.6 libdl.so.2, got: $needed" >&2; exit 1; }

  nm -D --defined-only "$musl" | awk "{print \$NF}" | sort -u > /tmp/musl
  nm -D --undefined-only "$so" | awk "\$1 == \"U\" {sub(/@.*/, \"\", \$2); print \$2}" | sort -u > /tmp/need
  [ -s /tmp/need ] || { echo "symbols: no undefined symbols at all; not a real build" >&2; exit 1; }
  missing=$(comm -23 /tmp/need /tmp/musl | tr "\n" " ")
  [ -z "$missing" ] || { echo "symbols: not exported by musl: $missing" >&2; exit 1; }

  newest=$(objdump -T "$so" | grep -oE "GLIBC_[0-9]+\.[0-9]+" | sort -uV | tail -1)
  [ "$(printf "%s\nGLIBC_2.17\n" "$newest" | sort -V | tail -1)" = "GLIBC_2.17" ] ||
    { echo "symbols: requires $newest, newer than GLIBC_2.17" >&2; exit 1; }

  echo "symbols: ok ($so: $(wc -l < /tmp/need | tr -d " ") symbols, newest $newest)"
' sh "$so"
