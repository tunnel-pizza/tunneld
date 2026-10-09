#!/bin/sh
# Builds c/ttyshim.c for one architecture into ttyshim-<arch>.so beside it, then
# gates it with symbols.sh. amd64 also refreshes the xterm-256color terminfo
# entry and its licence from the same image.
#
#   sh build.sh amd64|arm64 [source] [output]
set -eu

arch="${1:-}"
case "$arch" in
  amd64) platform=linux/amd64; extra= ;;
  arm64) platform=linux/arm64; extra=-mno-outline-atomics ;;
  *) echo "usage: $0 amd64|arm64 [source] [output]" >&2; exit 2 ;;
esac

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../../.." && pwd)"
rel=v1alpha1/attach/shell/ttyshim
src="${2:-$rel/c/ttyshim.c}"
out="${3:-$rel/ttyshim-$arch.so}"
cflags="${TTYSHIM_CFLAGS:--O2 -s -shared -fPIC -Wall -Wextra -Werror -ftls-model=initial-exec -fno-stack-protector -U_FORTIFY_SOURCE}"

# initial-exec TLS keeps ld-linux out of DT_NEEDED; no stack protector or
# fortify keeps glibc-only __*_chk out; -mno-outline-atomics keeps libgcc's
# LSE helpers (and their getauxval) out. dlsym is bound at its original
# version through a stub libdl.so.2, so glibc before 2.34 loads the object.
docker run --rm --platform "$platform" -v "$root:/w" -w /w \
  -e src="$src" -e out="$out" -e cflags="$cflags" -e extra="$extra" -e arch="$arch" -e rel="$rel" \
  debian:12@sha256:2c037a04925515fdd6ea85ea14a682d0e79931f5e9f5d07b6dbfc6ba12f9e858 sh -euc '
  # Packages from a fixed snapshot of the archive, not the live mirror: a
  # point release must not change the bytes CI compares with the committed
  # objects.
  rm -f /etc/apt/sources.list.d/debian.sources
  echo "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261001T000000Z bookworm main" > /etc/apt/sources.list
  apt-get -qq update >/dev/null
  apt-get -qq install -y --no-install-recommends gcc libc6-dev ncurses-base >/dev/null
  case $(uname -m) in x86_64) dlver=GLIBC_2.2.5 ;; *) dlver=GLIBC_2.17 ;; esac
  mkdir -p /tmp/stub
  echo "void *dlsym(void *h, const char *s) { (void)h; (void)s; return 0; }" > /tmp/stub/dl.c
  echo "$dlver { global: dlsym; local: *; };" > /tmp/stub/dl.map
  gcc -shared -fPIC -Wl,-soname,libdl.so.2 -Wl,--version-script=/tmp/stub/dl.map /tmp/stub/dl.c -o /tmp/stub/libdl.so.2
  gcc $cflags $extra -ffile-prefix-map=/w/= "$src" -L/tmp/stub -Wl,--no-as-needed -l:libdl.so.2 -o "$out"
  if [ "$arch" = amd64 ] && [ "$src" = "$rel/c/ttyshim.c" ]; then
    entry=$(find /usr/share/terminfo /lib/terminfo -name xterm-256color | head -1)
    mkdir -p "$rel/terminfo/x"
    cp "$entry" "$rel/terminfo/x/xterm-256color"
    cp /usr/share/doc/ncurses-base/copyright "$rel/NOTICE.ncurses"
  fi
  chown "$(stat -c %u:%g /w)" "$out" 2>/dev/null || true
'
echo "built $out"
sh "$here/symbols.sh" "$arch" "$out"
