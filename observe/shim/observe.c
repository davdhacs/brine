// cimemo observation shim: intercepts file opens to discover
// which files a command actually reads.
//
// Compiled as a dynamic library and injected via:
//   macOS: DYLD_INSERT_LIBRARIES=libcimemo_obs.dylib
//   Linux: LD_PRELOAD=libcimemo_obs.so
//
// Writes each opened path to $CIMEMO_READS_LOG (one per line).
// Only logs files opened for reading (not writing, not directories).

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/stat.h>

// Get the log file path from environment
static FILE *get_log(void) {
    static FILE *log = NULL;
    static int tried = 0;
    if (!tried) {
        tried = 1;
        const char *path = getenv("CIMEMO_READS_LOG");
        if (path) {
            log = fopen(path, "a");
        }
    }
    return log;
}

// Log a read-open
static void log_read(const char *path, int flags) {
    if (!path || path[0] == '\0') return;
    // Skip writes, directories, and special files
    if ((flags & (O_WRONLY | O_RDWR | O_CREAT | O_TRUNC)) != 0) return;
    // Skip /dev, /proc, /sys, /tmp (not project files)
    if (strncmp(path, "/dev/", 5) == 0 ||
        strncmp(path, "/proc/", 6) == 0 ||
        strncmp(path, "/sys/", 5) == 0 ||
        strncmp(path, "/var/", 5) == 0) return;

    FILE *log = get_log();
    if (log) {
        fprintf(log, "%s\n", path);
        // Don't flush every open (too slow) — flush at close
    }
}

// Intercept open()
int open(const char *path, int flags, ...) {
    log_read(path, flags);

    // Call the real open
    static int (*real_open)(const char *, int, ...) = NULL;
    if (!real_open) {
        real_open = dlsym(RTLD_NEXT, "open");
    }
    if (real_open) {
        // Handle the mode argument for O_CREAT
        if (flags & O_CREAT) {
            va_list ap;
            va_start(ap, flags);
            int mode = va_arg(ap, int);
            va_end(ap);
            return real_open(path, flags, mode);
        }
        return real_open(path, flags);
    }
    return -1;
}

// Intercept openat()
int openat(int dirfd, const char *path, int flags, ...) {
    log_read(path, flags);

    static int (*real_openat)(int, const char *, int, ...) = NULL;
    if (!real_openat) {
        real_openat = dlsym(RTLD_NEXT, "openat");
    }
    if (real_openat) {
        if (flags & O_CREAT) {
            va_list ap;
            va_start(ap, flags);
            int mode = va_arg(ap, int);
            va_end(ap);
            return real_openat(dirfd, path, flags, mode);
        }
        return real_openat(dirfd, path, flags);
    }
    return -1;
}

// Intercept fopen()
FILE *fopen(const char *path, const char *mode) {
    // Only log read-mode opens
    if (mode && (mode[0] == 'r')) {
        log_read(path, O_RDONLY);
    }

    static FILE *(*real_fopen)(const char *, const char *) = NULL;
    if (!real_fopen) {
        real_fopen = dlsym(RTLD_NEXT, "fopen");
    }
    return real_fopen ? real_fopen(path, mode) : NULL;
}

// Intercept stat() — tools often stat files to check existence
int stat(const char *path, struct stat *buf) {
    log_read(path, O_RDONLY);

    static int (*real_stat)(const char *, struct stat *) = NULL;
    if (!real_stat) {
        real_stat = dlsym(RTLD_NEXT, "stat");
    }
    return real_stat ? real_stat(path, buf) : -1;
}

// Intercept access()
int access(const char *path, int mode) {
    if (mode == R_OK || mode == F_OK) {
        log_read(path, O_RDONLY);
    }

    static int (*real_access)(const char *, int) = NULL;
    if (!real_access) {
        real_access = dlsym(RTLD_NEXT, "access");
    }
    return real_access ? real_access(path, mode) : -1;
}

// Constructor: flush log on library load
__attribute__((constructor))
static void init(void) {
    // Open the log file eagerly so it works even if the command
    // doesn't call any intercepted functions
    get_log();
}

// Destructor: flush and close
__attribute__((destructor))
static void cleanup(void) {
    FILE *log = get_log();
    if (log) {
        fflush(log);
        fclose(log);
    }
}
