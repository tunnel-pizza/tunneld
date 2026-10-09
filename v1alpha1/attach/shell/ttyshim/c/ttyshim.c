/*
 * ttyshim.c: a terminal for a program that has none.
 *
 * Preloaded by tunneld into a program it serves on a machine with no
 * pseudo-terminals. The program's stdin, stdout and stderr are one end of a
 * socket; this object answers the terminal questions about that socket from
 * a page shared with tunneld (TUNNELD_TTY), and leaves every other descriptor
 * to libc.
 *
 * One object loads under glibc and musl: built against glibc 2.36, linked
 * only to libc.so.6 and libdl.so.2, which musl's loader resolves to itself.
 * musl refuses to start a process whose preload has an unresolved symbol, so
 * glibc-only functions are reached through dlsym, never by reference.
 *
 * Both libcs call themselves internally without going through the PLT (musl's
 * isatty is a raw ioctl syscall; glibc's tcgetattr an inline one), so every
 * public entry point is wrapped on its own. Wrapping ioctl alone catches none
 * of the others.
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <spawn.h>
#include <stdarg.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <termios.h>
#include <unistd.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/syscall.h>

extern char **environ;

struct page {
    uint32_t magic, version, lock, loaded;
    uint64_t dev, ino;
    uint32_t iflag, oflag, cflag, lflag;
    uint8_t line, cc[32];
    uint32_t ispeed, ospeed;
    uint16_t rows, cols, xpixel, ypixel;
    int32_t fg_pgrp, sid;
};
_Static_assert(offsetof(struct page, lock) == 8, "page layout");
_Static_assert(offsetof(struct page, loaded) == 12, "page layout");
_Static_assert(offsetof(struct page, dev) == 16, "page layout");
_Static_assert(offsetof(struct page, iflag) == 32, "page layout");
_Static_assert(offsetof(struct page, line) == 48, "page layout");
_Static_assert(offsetof(struct page, cc) == 49, "page layout");
_Static_assert(offsetof(struct page, ispeed) == 84, "page layout");
_Static_assert(offsetof(struct page, rows) == 92, "page layout");
_Static_assert(offsetof(struct page, fg_pgrp) == 100, "page layout");
_Static_assert(offsetof(struct page, sid) == 104, "page layout");
_Static_assert(sizeof(struct termios) == 60, "libc termios");

/* The kernel's struct termios, which TCGETS and TCSETS* carry. */
struct kterm {
    uint32_t iflag, oflag, cflag, lflag;
    uint8_t line, cc[19];
};
_Static_assert(sizeof(struct kterm) == 36, "kernel termios");

#define PAGE_MAGIC 0x31595454u
#define PAGE_BYTES 4096
#define FD_FLOOR 256
#define LOCK_STEAL 100000
#define DEV_TTY "/dev/tty"

static struct page *pg;
static int g_init;
static int g_ttyfd = -1;
static char *g_shim;    /* the shim's path, from TUNNELD_TTY_SHIM */
static char *g_ttyenv;  /* "TUNNELD_TTY=..." */
static char *g_shimenv; /* "TUNNELD_TTY_SHIM=..." */

#define REAL(name) \
    static __typeof__(&name) real_; \
    if (!real_) real_ = (__typeof__(&name))dlsym(RTLD_NEXT, #name)
#define REAL_T(name, type) \
    static __typeof__(type) real_; \
    if (!real_) real_ = (__typeof__(type))dlsym(RTLD_NEXT, #name)
/* name, or fallback where libc has no name (musl 1.2.4 dropped the *64s). */
#define REAL2(name, fallback, type) \
    static __typeof__(type) real_; \
    if (!real_) real_ = (__typeof__(type))next2(#name, #fallback)
#define ENSURE do { if (!g_init) init(); } while (0)

static void init(void) __attribute__((constructor));

static void *next2(const char *name, const char *fallback) {
    void *f = dlsym(RTLD_NEXT, name);
    return f ? f : dlsym(RTLD_NEXT, fallback);
}

/* ---- the page ---------------------------------------------------------- */

static void lock(void) {
    for (int i = 0; __atomic_exchange_n(&pg->lock, 1, __ATOMIC_ACQUIRE); i++) {
        if (i > LOCK_STEAL) return; /* its holder died holding it */
        sched_yield();
    }
}

static void unlock(void) { __atomic_store_n(&pg->lock, 0, __ATOMIC_RELEASE); }

static int is_term(int fd) {
    if (!pg || fd < 0) return 0;
    int saved = errno;
    struct stat st;
    long r = syscall(SYS_fstat, fd, &st);
    errno = saved;
    return r == 0 && S_ISSOCK(st.st_mode) &&
           (uint64_t)st.st_dev == pg->dev && (uint64_t)st.st_ino == pg->ino;
}

static void get_settings(struct termios *t) {
    memset(t, 0, sizeof *t);
    lock();
    t->c_iflag = pg->iflag;
    t->c_oflag = pg->oflag;
    t->c_cflag = pg->cflag;
    t->c_lflag = pg->lflag;
    t->c_line = pg->line;
    memcpy(t->c_cc, pg->cc, sizeof pg->cc);
    t->c_ispeed = pg->ispeed;
    t->c_ospeed = pg->ospeed;
    unlock();
}

static void set_settings(const struct termios *t) {
    lock();
    pg->iflag = t->c_iflag;
    pg->oflag = t->c_oflag;
    pg->cflag = t->c_cflag;
    pg->lflag = t->c_lflag;
    pg->line = t->c_line;
    memcpy(pg->cc, t->c_cc, sizeof pg->cc);
    pg->ispeed = t->c_ispeed;
    pg->ospeed = t->c_ospeed;
    unlock();
}

static pid_t fg(void) {
    int32_t g = __atomic_load_n(&pg->fg_pgrp, __ATOMIC_ACQUIRE);
    return g > 0 ? g : getpgrp();
}

static pid_t sid(void) {
    int32_t s = __atomic_load_n(&pg->sid, __ATOMIC_ACQUIRE);
    return s > 0 ? s : getsid(0);
}

/* ---- the terminal's descriptor ----------------------------------------- */

static int term_fd(void) {
    if (g_ttyfd >= 0 && is_term(g_ttyfd)) return g_ttyfd;
    for (int i = 0; i <= 2; i++)
        if (is_term(i)) return i;
    errno = ENXIO;
    return -1;
}

static int open_term(int flags) {
    int fd = term_fd();
    if (fd < 0) return -1;
    return fcntl(fd, (flags & O_CLOEXEC) ? F_DUPFD_CLOEXEC : F_DUPFD, 0);
}

static int names_term(const char *path) {
    return pg && path && strcmp(path, DEV_TTY) == 0;
}

static char *entry(const char *name) {
    size_t n = strlen(name);
    for (char **e = environ; e && *e; e++)
        if (strncmp(*e, name, n) == 0 && (*e)[n] == '=') return strdup(*e);
    return NULL;
}

static void init(void) {
    if (g_init) return;
    g_init = 1;
    const char *path = getenv("TUNNELD_TTY");
    if (!path || !*path) return;
    int fd = (int)syscall(SYS_openat, AT_FDCWD, path, O_RDWR | O_CLOEXEC);
    if (fd < 0) return;
    void *m = mmap(NULL, PAGE_BYTES, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    syscall(SYS_close, fd);
    if (m == MAP_FAILED) return;
    struct page *p = m;
    if (__atomic_load_n(&p->magic, __ATOMIC_ACQUIRE) != PAGE_MAGIC || p->version != 1) {
        munmap(m, PAGE_BYTES);
        return;
    }
    pg = p;
    __atomic_store_n(&pg->loaded, 1, __ATOMIC_RELEASE);
    for (int i = 0; i <= 2; i++)
        if (is_term(i)) { g_ttyfd = fcntl(i, F_DUPFD_CLOEXEC, FD_FLOOR); break; }
    const char *shim = getenv("TUNNELD_TTY_SHIM");
    if (shim) g_shim = strdup(shim);
    g_ttyenv = entry("TUNNELD_TTY");
    g_shimenv = entry("TUNNELD_TTY_SHIM");
    /* Over a socket both libcs pick full buffering whatever isatty says. */
    if (is_term(1)) setvbuf(stdout, NULL, _IOLBF, 0);
    if (is_term(2)) setvbuf(stderr, NULL, _IONBF, 0);
}

/* ---- terminal queries -------------------------------------------------- */

int isatty(int fd) {
    ENSURE;
    if (is_term(fd)) return 1;
    REAL(isatty);
    return real_(fd);
}

char *ttyname(int fd) {
    ENSURE;
    static char name[] = DEV_TTY;
    if (is_term(fd)) return name;
    REAL(ttyname);
    return real_(fd);
}

int ttyname_r(int fd, char *buf, size_t len) {
    ENSURE;
    if (is_term(fd)) {
        if (len < sizeof DEV_TTY) return ERANGE;
        memcpy(buf, DEV_TTY, sizeof DEV_TTY);
        return 0;
    }
    REAL(ttyname_r);
    return real_(fd, buf, len);
}

int tcgetattr(int fd, struct termios *t) {
    ENSURE;
    if (is_term(fd)) {
        if (!t) { errno = EFAULT; return -1; }
        get_settings(t);
        return 0;
    }
    REAL(tcgetattr);
    return real_(fd, t);
}

int tcsetattr(int fd, int act, const struct termios *t) {
    ENSURE;
    if (is_term(fd)) {
        if (!t) { errno = EFAULT; return -1; }
        if (act != TCSANOW && act != TCSADRAIN && act != TCSAFLUSH) { errno = EINVAL; return -1; }
        set_settings(t);
        return 0;
    }
    REAL(tcsetattr);
    return real_(fd, act, t);
}

pid_t tcgetpgrp(int fd) {
    ENSURE;
    if (is_term(fd)) return fg();
    REAL(tcgetpgrp);
    return real_(fd);
}

int tcsetpgrp(int fd, pid_t pgrp) {
    ENSURE;
    if (is_term(fd)) {
        __atomic_store_n(&pg->fg_pgrp, (int32_t)pgrp, __ATOMIC_RELEASE);
        return 0;
    }
    REAL(tcsetpgrp);
    return real_(fd, pgrp);
}

pid_t tcgetsid(int fd) {
    ENSURE;
    if (is_term(fd)) return sid();
    REAL(tcgetsid);
    return real_(fd);
}

int tcdrain(int fd) {
    ENSURE;
    if (is_term(fd)) return 0;
    REAL(tcdrain);
    return real_(fd);
}

int tcflush(int fd, int q) {
    ENSURE;
    if (is_term(fd)) return 0;
    REAL(tcflush);
    return real_(fd, q);
}

int tcflow(int fd, int act) {
    ENSURE;
    if (is_term(fd)) return 0;
    REAL(tcflow);
    return real_(fd, act);
}

int tcsendbreak(int fd, int dur) {
    ENSURE;
    if (is_term(fd)) return 0;
    REAL(tcsendbreak);
    return real_(fd, dur);
}

/* glibc: int ioctl(int, unsigned long, ...); musl passes an int request, so
 * the upper half of the register is undefined and only 32 bits compare. */
int ioctl(int fd, unsigned long req, ...) {
    ENSURE;
    va_list ap;
    va_start(ap, req);
    void *arg = va_arg(ap, void *);
    va_end(ap);
    REAL(ioctl);
    if (!is_term(fd)) return real_(fd, req, arg);
    unsigned r = (unsigned)req;
    switch (r) {
    case TIOCSCTTY: case TIOCNOTTY:
        return 0;
    }
    if (!arg) { errno = EFAULT; return -1; }
    switch (r) {
    case TCGETS: {
        struct termios t;
        get_settings(&t);
        struct kterm *k = arg;
        k->iflag = t.c_iflag; k->oflag = t.c_oflag; k->cflag = t.c_cflag; k->lflag = t.c_lflag;
        k->line = t.c_line;
        memcpy(k->cc, t.c_cc, sizeof k->cc);
        return 0;
    }
    case TCSETS: case TCSETSW: case TCSETSF: {
        const struct kterm *k = arg;
        struct termios t;
        get_settings(&t);
        t.c_iflag = k->iflag; t.c_oflag = k->oflag; t.c_cflag = k->cflag; t.c_lflag = k->lflag;
        t.c_line = k->line;
        memcpy(t.c_cc, k->cc, sizeof k->cc);
        set_settings(&t);
        return 0;
    }
    case TIOCGWINSZ: {
        struct winsize *w = arg;
        lock();
        w->ws_row = pg->rows; w->ws_col = pg->cols; w->ws_xpixel = pg->xpixel; w->ws_ypixel = pg->ypixel;
        unlock();
        return 0;
    }
    case TIOCSWINSZ: {
        const struct winsize *w = arg;
        lock();
        pg->rows = w->ws_row; pg->cols = w->ws_col; pg->xpixel = w->ws_xpixel; pg->ypixel = w->ws_ypixel;
        unlock();
        return 0;
    }
    case TIOCGPGRP:
        *(pid_t *)arg = fg();
        return 0;
    case TIOCSPGRP:
        __atomic_store_n(&pg->fg_pgrp, (int32_t)*(const pid_t *)arg, __ATOMIC_RELEASE);
        return 0;
    case TIOCGSID:
        *(pid_t *)arg = sid();
        return 0;
    }
    return real_(fd, req, arg);
}

/* ---- opening the terminal by name -------------------------------------- */

static mode_t mode_of(int flags, va_list ap) {
    return (flags & (O_CREAT | O_TMPFILE)) ? va_arg(ap, mode_t) : 0;
}

int open(const char *path, int flags, ...) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL(open);
    return real_(path, flags, mode);
}

int open64(const char *path, int flags, ...) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL2(open64, open, int (*)(const char *, int, ...));
    return real_(path, flags, mode);
}

int openat(int dirfd, const char *path, int flags, ...) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL(openat);
    return real_(dirfd, path, flags, mode);
}

int openat64(int dirfd, const char *path, int flags, ...) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL2(openat64, openat, int (*)(int, const char *, int, ...));
    return real_(dirfd, path, flags, mode);
}

int __open_2(const char *path, int flags) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    REAL2(__open_2, open, int (*)(const char *, int));
    return real_(path, flags);
}

int __open64_2(const char *path, int flags) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    REAL2(__open64_2, open, int (*)(const char *, int));
    return real_(path, flags);
}

int __openat_2(int dirfd, const char *path, int flags) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    REAL2(__openat_2, openat, int (*)(int, const char *, int));
    return real_(dirfd, path, flags);
}

int __openat64_2(int dirfd, const char *path, int flags) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    REAL2(__openat64_2, openat, int (*)(int, const char *, int));
    return real_(dirfd, path, flags);
}

int creat(const char *path, mode_t mode) {
    ENSURE;
    if (names_term(path)) return open_term(O_WRONLY);
    REAL(creat);
    return real_(path, mode);
}

int creat64(const char *path, mode_t mode) {
    ENSURE;
    if (names_term(path)) return open_term(O_WRONLY);
    REAL2(creat64, creat, int (*)(const char *, mode_t));
    return real_(path, mode);
}

static FILE *fopen_term(const char *mode) {
    int fd = open_term(strchr(mode, 'e') ? O_CLOEXEC : 0);
    if (fd < 0) return NULL;
    FILE *f = fdopen(fd, mode);
    if (!f) close(fd);
    return f;
}

FILE *fopen(const char *path, const char *mode) {
    ENSURE;
    if (names_term(path)) return fopen_term(mode);
    REAL(fopen);
    return real_(path, mode);
}

FILE *fopen64(const char *path, const char *mode) {
    ENSURE;
    if (names_term(path)) return fopen_term(mode);
    REAL2(fopen64, fopen, FILE *(*)(const char *, const char *));
    return real_(path, mode);
}

static FILE *freopen_term(FILE *stream) {
    int fd = open_term(0);
    if (fd < 0) return NULL;
    fflush(stream);
    int ok = dup2(fd, fileno(stream)) >= 0;
    close(fd);
    if (!ok) return NULL;
    clearerr(stream);
    return stream;
}

FILE *freopen(const char *path, const char *mode, FILE *stream) {
    ENSURE;
    if (names_term(path)) return freopen_term(stream);
    REAL(freopen);
    return real_(path, mode, stream);
}

FILE *freopen64(const char *path, const char *mode, FILE *stream) {
    ENSURE;
    if (names_term(path)) return freopen_term(stream);
    REAL2(freopen64, freopen, FILE *(*)(const char *, const char *, FILE *));
    return real_(path, mode, stream);
}

/* ---- staying loaded across exec ---------------------------------------- */

/* envp with the shim put back if it was dropped (env -i, a program building
 * its own environment), or NULL when envp already carries it. A terminal
 * survives env -i, and this stands in for one. The array, and a rebuilt
 * LD_PRELOAD in *built, are the caller's to free if exec returns. */
static char **sticky(char *const envp[], char **built) {
    static char *const none[] = { NULL };
    *built = NULL;
    if (!pg || !g_shim || !g_ttyenv || !g_shimenv) return NULL;
    if (!envp) envp = none; /* musl's clearenv leaves environ NULL */
    int n = 0, pre = -1, tty = -1, shim = -1;
    for (; envp[n]; n++) {
        if (strncmp(envp[n], "LD_PRELOAD=", 11) == 0) pre = n;
        else if (strncmp(envp[n], "TUNNELD_TTY=", 12) == 0) tty = n;
        else if (strncmp(envp[n], "TUNNELD_TTY_SHIM=", 17) == 0) shim = n;
    }
    int need_pre = pre < 0 || !strstr(envp[pre] + 11, g_shim);
    if (!need_pre && tty >= 0 && shim >= 0) return NULL;
    char **out = malloc((size_t)(n + 4) * sizeof *out);
    if (!out) return NULL;
    int m = 0;
    for (int i = 0; i < n; i++)
        if (i != pre || !need_pre) out[m++] = envp[i];
    if (need_pre) {
        const char *old = pre >= 0 ? envp[pre] + 11 : "";
        size_t len = 11 + strlen(old) + 1 + strlen(g_shim) + 1;
        *built = malloc(len);
        if (!*built) { free(out); return NULL; }
        snprintf(*built, len, "LD_PRELOAD=%s%s%s", old, *old ? ":" : "", g_shim);
        out[m++] = *built;
    }
    if (tty < 0) out[m++] = g_ttyenv;
    if (shim < 0) out[m++] = g_shimenv;
    out[m] = NULL;
    return out;
}

int execve(const char *path, char *const argv[], char *const envp[]) {
    ENSURE;
    char *built;
    char **e = sticky(envp, &built);
    REAL(execve);
    int r = real_(path, argv, e ? e : envp);
    free(e); free(built);
    return r;
}

int execv(const char *path, char *const argv[]) {
    ENSURE;
    char *built;
    char **e = sticky(environ, &built);
    REAL_T(execve, int (*)(const char *, char *const[], char *const[]));
    int r = real_(path, argv, e ? e : environ);
    free(e); free(built);
    return r;
}

int execvpe(const char *file, char *const argv[], char *const envp[]) {
    ENSURE;
    char *built;
    char **e = sticky(envp, &built);
    REAL(execvpe);
    int r = real_(file, argv, e ? e : envp);
    free(e); free(built);
    return r;
}

int execvp(const char *file, char *const argv[]) {
    ENSURE;
    char *built;
    char **e = sticky(environ, &built);
    REAL_T(execvpe, int (*)(const char *, char *const[], char *const[]));
    int r = real_(file, argv, e ? e : environ);
    free(e); free(built);
    return r;
}

int posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *fa,
                const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    ENSURE;
    char *built;
    char **e = sticky(envp, &built);
    REAL(posix_spawn);
    int r = real_(pid, path, fa, attr, argv, e ? e : envp);
    free(e); free(built);
    return r;
}

int posix_spawnp(pid_t *pid, const char *file, const posix_spawn_file_actions_t *fa,
                 const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    ENSURE;
    char *built;
    char **e = sticky(envp, &built);
    REAL(posix_spawnp);
    int r = real_(pid, file, fa, attr, argv, e ? e : envp);
    free(e); free(built);
    return r;
}
