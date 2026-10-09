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
#include <time.h>
#include <unistd.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/uio.h>

extern char **environ;

struct page {
    uint32_t magic, version, lock, loaded;
    uint64_t dev, ino;
    uint32_t iflag, oflag, cflag, lflag;
    uint8_t line, cc[32];
    uint32_t ispeed, ospeed;
    uint16_t rows, cols, xpixel, ypixel;
    int32_t fg_pgrp, sid;
    uint32_t gone; /* set by tunneld when the run is over */
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
_Static_assert(offsetof(struct page, gone) == 108, "page layout");
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
#define FD_SPAN 16
#define NB_MAX 1024
#define LOCK_STEAL 100000
#define DEV_TTY "/dev/tty"

static struct page *pg;
static int g_init;
static int g_ttyfd = -1;
/* Descriptors this process asked to be non-blocking. The socket is one file
 * description shared with every process on the terminal, and a reopened
 * terminal would be a description of its own, so O_NONBLOCK is kept here
 * per descriptor and the description itself stays blocking. */
static unsigned char g_nb[NB_MAX];
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

/* Waits, briefly, for the terminal's output to have been read by tunneld:
 * output processing is applied as tunneld reads, so a change to it must not
 * overtake what was written under the old one (TCSADRAIN's promise). */
static void drain(int fd) {
    for (int i = 0; i < 200; i++) {
        int queued = 0;
        if (syscall(SYS_ioctl, fd, TIOCOUTQ, &queued) != 0 || queued <= 0) return;
        struct timespec ms = {0, 1000000};
        syscall(SYS_nanosleep, &ms, NULL);
    }
}

/* set_settings, after draining when the output processing changes or the
 * caller asked for a drain. */
static void apply(int fd, int drained, const struct termios *t) {
    int saved = errno;
    uint32_t was = __atomic_load_n(&pg->oflag, __ATOMIC_ACQUIRE);
    if (drained || was != t->c_oflag) drain(fd);
    errno = saved;
    set_settings(t);
}

static int nonblocking(int fd) { return fd >= 0 && fd < NB_MAX && g_nb[fd] && is_term(fd); }

static void set_nb(int fd, int on) {
    if (fd >= 0 && fd < NB_MAX) g_nb[fd] = (unsigned char)(on != 0);
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

#define NOT_TERM (-1)
#define ANY_TERM (-2)

/* Which terminal descriptor path names: ANY_TERM for /dev/tty; N for
 * /dev/std{in,out,err}, /dev/fd/N or /proc/self/fd/N when N is the terminal,
 * since Linux refuses to reopen a socket through /proc; NOT_TERM otherwise. */
static int named(const char *path) {
    if (!pg || !path) return NOT_TERM;
    if (strcmp(path, DEV_TTY) == 0) return ANY_TERM;
    int fd = -1;
    if (strcmp(path, "/dev/stdin") == 0) fd = 0;
    else if (strcmp(path, "/dev/stdout") == 0) fd = 1;
    else if (strcmp(path, "/dev/stderr") == 0) fd = 2;
    else {
        const char *n = NULL;
        if (strncmp(path, "/dev/fd/", 8) == 0) n = path + 8;
        else if (strncmp(path, "/proc/self/fd/", 14) == 0) n = path + 14;
        if (n && *n) {
            long v = 0;
            for (; *n >= '0' && *n <= '9' && v <= 1 << 20; n++) v = v * 10 + (*n - '0');
            if (!*n && v <= 1 << 20) fd = (int)v;
        }
    }
    return fd >= 0 && is_term(fd) ? fd : NOT_TERM;
}

static int open_named(int which, int flags) {
    int fd = which == ANY_TERM ? term_fd() : which;
    if (fd < 0) return -1;
    int nfd = fcntl(fd, (flags & O_CLOEXEC) ? F_DUPFD_CLOEXEC : F_DUPFD, 0);
    set_nb(nfd, flags & O_NONBLOCK);
    return nfd;
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
    /* A descriptor to the terminal past the ones programs use, kept across
     * exec, so /dev/tty still opens in a program started after its parent
     * closed stdin, stdout and stderr. */
    for (int i = FD_FLOOR; i < FD_FLOOR + FD_SPAN && g_ttyfd < 0; i++)
        if (is_term(i)) g_ttyfd = i;
    for (int i = 0; i <= 2 && g_ttyfd < 0; i++)
        if (is_term(i)) g_ttyfd = fcntl(i, F_DUPFD, FD_FLOOR);
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
        apply(fd, act != TCSANOW, t);
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
        apply(fd, r != TCSETS, &t);
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
    case FIONBIO:
        set_nb(fd, *(const int *)arg);
        return 0;
    }
    return real_(fd, req, arg);
}

/* ---- opening the terminal by name -------------------------------------- */

static mode_t mode_of(int flags, va_list ap) {
    return ((flags & O_CREAT) || (flags & O_TMPFILE) == O_TMPFILE) ? va_arg(ap, mode_t) : 0;
}

int open(const char *path, int flags, ...) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL(open);
    return real_(path, flags, mode);
}

int open64(const char *path, int flags, ...) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL2(open64, open, int (*)(const char *, int, ...));
    return real_(path, flags, mode);
}

int openat(int dirfd, const char *path, int flags, ...) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL(openat);
    return real_(dirfd, path, flags, mode);
}

int openat64(int dirfd, const char *path, int flags, ...) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL2(openat64, openat, int (*)(int, const char *, int, ...));
    return real_(dirfd, path, flags, mode);
}

int __open_2(const char *path, int flags) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, flags);
    REAL2(__open_2, open, int (*)(const char *, int));
    return real_(path, flags);
}

int __open64_2(const char *path, int flags) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, flags);
    REAL2(__open64_2, open, int (*)(const char *, int));
    return real_(path, flags);
}

int __openat_2(int dirfd, const char *path, int flags) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, flags);
    REAL2(__openat_2, openat, int (*)(int, const char *, int));
    return real_(dirfd, path, flags);
}

int __openat64_2(int dirfd, const char *path, int flags) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, flags);
    REAL2(__openat64_2, openat, int (*)(int, const char *, int));
    return real_(dirfd, path, flags);
}

int creat(const char *path, mode_t mode) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, O_WRONLY);
    REAL(creat);
    return real_(path, mode);
}

int creat64(const char *path, mode_t mode) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return open_named(t, O_WRONLY);
    REAL2(creat64, creat, int (*)(const char *, mode_t));
    return real_(path, mode);
}

static FILE *fopen_named(int which, const char *mode) {
    int fd = open_named(which, strchr(mode, 'e') ? O_CLOEXEC : 0);
    if (fd < 0) return NULL;
    FILE *f = fdopen(fd, mode);
    if (!f) { close(fd); return NULL; }
    /* A terminal's FILE is line-buffered; libc decides that from S_ISCHR. */
    setvbuf(f, NULL, _IOLBF, 0);
    return f;
}

FILE *fopen(const char *path, const char *mode) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return fopen_named(t, mode);
    REAL(fopen);
    return real_(path, mode);
}

FILE *fopen64(const char *path, const char *mode) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return fopen_named(t, mode);
    REAL2(fopen64, fopen, FILE *(*)(const char *, const char *));
    return real_(path, mode);
}

static FILE *freopen_named(int which, FILE *stream) {
    int fd = open_named(which, 0);
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
    int t = named(path);
    if (t != NOT_TERM) return freopen_named(t, stream);
    REAL(freopen);
    return real_(path, mode, stream);
}

FILE *freopen64(const char *path, const char *mode, FILE *stream) {
    ENSURE;
    int t = named(path);
    if (t != NOT_TERM) return freopen_named(t, stream);
    REAL2(freopen64, freopen, FILE *(*)(const char *, const char *, FILE *));
    return real_(path, mode, stream);
}

/* ---- non-blocking, per descriptor ---------------------------------------- */

static int fcntl_term(int fd, int cmd, long arg, int *handled) {
    *handled = 0;
    if (!is_term(fd)) return 0;
    if (cmd == F_GETFL) {
        REAL_T(fcntl, int (*)(int, int, ...));
        int fl = real_(fd, F_GETFL);
        if (fl < 0) return fl;
        *handled = 1;
        return (fl & ~O_NONBLOCK) | (fd < NB_MAX && g_nb[fd] ? O_NONBLOCK : 0);
    }
    if (cmd == F_SETFL) {
        REAL_T(fcntl, int (*)(int, int, ...));
        set_nb(fd, arg & O_NONBLOCK);
        *handled = 1;
        return real_(fd, F_SETFL, arg & ~O_NONBLOCK);
    }
    return 0;
}

int fcntl(int fd, int cmd, ...) {
    ENSURE;
    va_list ap; va_start(ap, cmd); long arg = va_arg(ap, long); va_end(ap);
    int handled, r = fcntl_term(fd, cmd, arg, &handled);
    if (handled) return r;
    REAL_T(fcntl, int (*)(int, int, ...));
    return real_(fd, cmd, arg);
}

int fcntl64(int fd, int cmd, ...) {
    ENSURE;
    va_list ap; va_start(ap, cmd); long arg = va_arg(ap, long); va_end(ap);
    int handled, r = fcntl_term(fd, cmd, arg, &handled);
    if (handled) return r;
    REAL2(fcntl64, fcntl, int (*)(int, int, ...));
    return real_(fd, cmd, arg);
}

ssize_t read(int fd, void *buf, size_t n) {
    if (nonblocking(fd)) return recv(fd, buf, n, MSG_DONTWAIT);
    REAL(read);
    return real_(fd, buf, n);
}

/* glibc's fortified read, where the buffer's size is known at compile time. */
ssize_t __read_chk(int fd, void *buf, size_t n, size_t buflen) {
    if (nonblocking(fd)) return recv(fd, buf, n < buflen ? n : buflen, MSG_DONTWAIT);
    {
        REAL_T(__read_chk, ssize_t (*)(int, void *, size_t, size_t));
        if (real_) return real_(fd, buf, n, buflen);
    }
    REAL_T(read, ssize_t (*)(int, void *, size_t));
    return real_(fd, buf, n);
}

ssize_t write(int fd, const void *buf, size_t n) {
    if (nonblocking(fd)) return send(fd, buf, n, MSG_DONTWAIT);
    REAL(write);
    return real_(fd, buf, n);
}

ssize_t readv(int fd, const struct iovec *iov, int cnt) {
    if (nonblocking(fd)) {
        struct msghdr m = {0};
        m.msg_iov = (struct iovec *)iov;
        m.msg_iovlen = (size_t)cnt;
        return recvmsg(fd, &m, MSG_DONTWAIT);
    }
    REAL(readv);
    return real_(fd, iov, cnt);
}

ssize_t writev(int fd, const struct iovec *iov, int cnt) {
    if (nonblocking(fd)) {
        struct msghdr m = {0};
        m.msg_iov = (struct iovec *)iov;
        m.msg_iovlen = (size_t)cnt;
        return sendmsg(fd, &m, MSG_DONTWAIT);
    }
    REAL(writev);
    return real_(fd, iov, cnt);
}

/* ---- staying loaded across exec ---------------------------------------- */

/* What an exec's environment needs: the shim put back if it was dropped (env
 * -i, a program building its own environment), since a terminal survives
 * that; or, once the run is over (gone), the shim taken out, so a program
 * that outlived its target starts what it runs without a loader error. Built
 * on the caller's stack: a vfork child must not allocate. */
struct need {
    int n, pre, tty, shim;
    int any;    /* the environment changes */
    int strip;  /* the run is over */
    size_t len; /* bytes for a rebuilt LD_PRELOAD entry */
};

static char *const no_env[] = {NULL};

static struct need needs(char *const envp[]) {
    struct need d = {0, -1, -1, -1, 0, 0, 1};
    if (!pg || !g_shim || !g_ttyenv || !g_shimenv) return d;
    if (!envp) envp = no_env; /* musl's clearenv leaves environ NULL */
    for (; envp[d.n]; d.n++) {
        if (strncmp(envp[d.n], "LD_PRELOAD=", 11) == 0) d.pre = d.n;
        else if (strncmp(envp[d.n], "TUNNELD_TTY=", 12) == 0) d.tty = d.n;
        else if (strncmp(envp[d.n], "TUNNELD_TTY_SHIM=", 17) == 0) d.shim = d.n;
    }
    d.strip = __atomic_load_n(&pg->gone, __ATOMIC_ACQUIRE) != 0;
    int has = d.pre >= 0 && strstr(envp[d.pre] + 11, g_shim);
    if (d.strip) d.any = has || d.tty >= 0 || d.shim >= 0;
    else d.any = !has || d.tty < 0 || d.shim < 0;
    d.len = 11 + (d.pre >= 0 ? strlen(envp[d.pre] + 11) : 0) + 1 + strlen(g_shim) + 1;
    return d;
}

/* LD_PRELOAD's value without the shim, written to out. */
static void without(const char *value, char *out) {
    size_t sl = strlen(g_shim);
    char *w = out;
    const char *p = value;
    while (*p) {
        const char *e = p;
        while (*e && *e != ':' && *e != ' ') e++;
        size_t l = (size_t)(e - p);
        if (l && !(l == sl && strncmp(p, g_shim, sl) == 0)) {
            if (w != out) *w++ = ':';
            memcpy(w, p, l);
            w += l;
        }
        p = *e ? e + 1 : e;
    }
    *w = 0;
}

static char *const *fill(char *const envp[], const struct need *d, char **out, char *pre) {
    if (!envp) envp = no_env;
    int m = 0;
    for (int i = 0; i < d->n; i++) {
        if (i == d->pre || i == d->tty || i == d->shim) continue;
        out[m++] = envp[i];
    }
    if (d->strip) {
        if (d->pre >= 0) {
            memcpy(pre, "LD_PRELOAD=", 11);
            without(envp[d->pre] + 11, pre + 11);
            if (pre[11]) out[m++] = pre;
        }
    } else {
        const char *old = d->pre >= 0 ? envp[d->pre] + 11 : "";
        if (d->pre >= 0 && strstr(old, g_shim)) {
            out[m++] = envp[d->pre];
        } else {
            snprintf(pre, d->len, "LD_PRELOAD=%s%s%s", old, *old ? ":" : "", g_shim);
            out[m++] = pre;
        }
        out[m++] = d->tty >= 0 ? envp[d->tty] : g_ttyenv;
        out[m++] = d->shim >= 0 ? envp[d->shim] : g_shimenv;
    }
    out[m] = NULL;
    return out;
}

/* e: envp as the exec should get it, on this frame's stack. */
#define STICKY(envp, e) \
    struct need d_ = needs(envp); \
    char *out_[d_.any ? d_.n + 4 : 1]; \
    char pre_[d_.any ? d_.len : 1]; \
    char *const *e = d_.any ? fill(envp, &d_, out_, pre_) : (envp)

int execve(const char *path, char *const argv[], char *const envp[]) {
    ENSURE;
    STICKY(envp, e);
    REAL(execve);
    return real_(path, argv, e);
}

int execv(const char *path, char *const argv[]) {
    ENSURE;
    STICKY(environ, e);
    REAL_T(execve, int (*)(const char *, char *const[], char *const[]));
    return real_(path, argv, e);
}

int execvpe(const char *file, char *const argv[], char *const envp[]) {
    ENSURE;
    STICKY(envp, e);
    REAL(execvpe);
    return real_(file, argv, e);
}

int execvp(const char *file, char *const argv[]) {
    ENSURE;
    STICKY(environ, e);
    REAL_T(execvpe, int (*)(const char *, char *const[], char *const[]));
    return real_(file, argv, e);
}

int fexecve(int fd, char *const argv[], char *const envp[]) {
    ENSURE;
    STICKY(envp, e);
    REAL(fexecve);
    return real_(fd, argv, e);
}

/* The execl family, gathered into an argv; both libcs implement them by
 * calling execve inside themselves, out of a preload's reach. */
#define GATHER(arg, argv, after) \
    int n_ = 0; \
    va_list ap_; \
    if (arg) { \
        n_ = 1; \
        va_start(ap_, arg); \
        while (va_arg(ap_, char *)) n_++; \
        va_end(ap_); \
    } \
    char *argv[n_ + 1]; \
    argv[0] = (char *)(arg); \
    va_start(ap_, arg); \
    for (int i_ = 1; i_ < n_; i_++) argv[i_] = va_arg(ap_, char *); \
    if (n_) (void)va_arg(ap_, char *); \
    after; \
    va_end(ap_); \
    argv[n_] = NULL

/* glibc declares arg nonnull; POSIX allows an empty argv. */
#pragma GCC diagnostic push
#pragma GCC diagnostic ignored "-Wnonnull-compare"

int execl(const char *path, const char *arg, ...) {
    GATHER(arg, argv, (void)0);
    return execv(path, argv);
}

int execlp(const char *file, const char *arg, ...) {
    GATHER(arg, argv, (void)0);
    return execvp(file, argv);
}

int execle(const char *path, const char *arg, ...) {
    char *const *envp;
    GATHER(arg, argv, envp = va_arg(ap_, char *const *));
    return execve(path, argv, envp);
}

#pragma GCC diagnostic pop

int posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *fa,
                const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    ENSURE;
    STICKY(envp, e);
    REAL(posix_spawn);
    return real_(pid, path, fa, attr, argv, e);
}

int posix_spawnp(pid_t *pid, const char *file, const posix_spawn_file_actions_t *fa,
                 const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    ENSURE;
    STICKY(envp, e);
    REAL(posix_spawnp);
    return real_(pid, file, fa, attr, argv, e);
}
