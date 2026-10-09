#!/bin/sh
# Proves symbols.sh fails each way a build can go wrong. A gate that passes
# everything would pass a broken shim too.
#
#   sh negative.sh amd64|arm64
set -eu

arch="${1:-}"
case "$arch" in amd64|arm64) ;; *) echo "usage: $0 amd64|arm64" >&2; exit 2 ;; esac

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../../.." && pwd)"
rel=v1alpha1/attach/shell/ttyshim
scratch="$rel/.negative"
mkdir -p "$root/$scratch"
trap 'rm -rf "$root/$scratch"' EXIT

expect_fail() { # name, source, extra cflags
  printf '%s\n' "$2" > "$root/$scratch/$1.c"
  flags="-O2 -s -shared -fPIC -Wall -Wextra -fno-stack-protector $3"
  if TTYSHIM_CFLAGS="$flags" sh "$here/build.sh" "$arch" "$scratch/$1.c" "$scratch/$1.so" >"$root/$scratch/$1.log" 2>&1; then
    echo "negative: $1 passed the gate; it must not" >&2
    cat "$root/$scratch/$1.log" >&2
    exit 1
  fi
  echo "negative: $1 fails the gate, as it should"
}

tls=-ftls-model=global-dynamic
[ "$arch" = arm64 ] && tls="$tls -mtls-dialect=trad"

expect_fail glibc-only '#include <gnu/libc-version.h>
const char *v(void) { return gnu_get_libc_version(); }' ''
expect_fail general-dynamic-tls '__thread int t;
int *p(void) { return &t; }' "$tls"
expect_fail fortify '#include <string.h>
void c(char *d, const char *s, unsigned long n) { char b[8]; memcpy(b, s, n); memcpy(d, b, 8); }' '-D_FORTIFY_SOURCE=2'
expect_fail new-glibc '#include <sys/stat.h>
int f(int fd) { struct stat s; return fstat(fd, &s); }' ''
expect_fail empty 'int nothing;' ''
echo "negative: ok"
