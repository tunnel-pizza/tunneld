/*
 * ttyshim.c: a terminal for a program that has none.
 *
 * Preloaded by tunneld into a program it serves on a machine with no
 * pseudo-terminals. The program's stdin, stdout and stderr are one end of a
 * socket; this object answers the terminal questions about that socket from
 * a page shared with tunneld (TUNNELD_TTY).
 *
 * One object loads under glibc and musl: built against glibc 2.36, linked
 * only to libc.so.6 and libdl.so.2, which musl's loader resolves to itself.
 * musl refuses to start a process whose preload has an unresolved symbol, so
 * glibc-only functions are reached through dlsym, never by reference.
 */
#define _GNU_SOURCE
#include <stdint.h>
#include <stdlib.h>

static int g_init;

static void init(void) __attribute__((constructor));
static void init(void) {
    if (g_init) return;
    g_init = 1;
    (void)getenv("TUNNELD_TTY");
}
