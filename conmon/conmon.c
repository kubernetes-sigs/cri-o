#define _GNU_SOURCE
#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <sys/eventfd.h>
#include <sys/stat.h>
#include <sys/uio.h>
#include <sys/ioctl.h>
#include <termios.h>
#include <syslog.h>
#include <unistd.h>
#include <inttypes.h>

#if __STDC_VERSION__ >= 199901L
/* C99 or later */
#else
#error conmon.c requires C99 or later
#endif

#include <glib.h>
#include <glib-unix.h>

#include "cmsg.h"
#include "config.h"

#define _cleanup_(x) __attribute__((cleanup(x)))

static inline void freep(void *p)
{
	free(*(void **)p);
}

static inline void closep(int *fd)
{
	if (*fd >= 0)
		close(*fd);
	*fd = -1;
}

static inline void fclosep(FILE **fp)
{
	if (*fp)
		fclose(*fp);
	*fp = NULL;
}

static inline void gstring_free_cleanup(GString **string)
{
	if (*string)
		g_string_free(*string, TRUE);
}

static inline void strv_cleanup(char ***strv)
{
	if (strv)
		g_strfreev(*strv);
}

#define _cleanup_free_ _cleanup_(freep)
#define _cleanup_close_ _cleanup_(closep)
#define _cleanup_fclose_ _cleanup_(fclosep)
#define _cleanup_gstring_ _cleanup_(gstring_free_cleanup)
#define _cleanup_strv_ _cleanup_(strv_cleanup)

static volatile pid_t container_pid = -1;
static volatile pid_t create_pid = -1;
static gboolean opt_version = FALSE;
static gboolean opt_terminal = FALSE;
static gboolean opt_stdin = FALSE;
static gboolean opt_leave_stdin_open = FALSE;
static gboolean opt_syslog = FALSE;
static char *opt_cid = NULL;
static char *opt_cuuid = NULL;
static char *opt_runtime_path = NULL;
static char *opt_bundle_path = NULL;
static char *opt_container_pid_file = NULL;
static char *opt_conmon_pid_file = NULL;
static gboolean opt_systemd_cgroup = FALSE;
static gboolean opt_no_pivot = FALSE;
static char *opt_exec_process_spec = NULL;
static gboolean opt_exec = FALSE;
static char *opt_restore_path = NULL;
static char *opt_log_path = NULL;
static char *opt_exit_dir = NULL;
static int opt_timeout = 0;
static int64_t opt_log_size_max = -1;
static char *opt_socket_path = DEFAULT_SOCKET_PATH;
static gboolean opt_no_new_keyring = FALSE;
static char *opt_exit_command = NULL;
static gchar **opt_exit_args = NULL;
static gboolean opt_replace_listen_pid = FALSE;
static char *opt_log_level = NULL;
static GOptionEntry opt_entries[] = {
	{"terminal", 't', 0, G_OPTION_ARG_NONE, &opt_terminal, "Terminal", NULL},
	{"stdin", 'i', 0, G_OPTION_ARG_NONE, &opt_stdin, "Stdin", NULL},
	{"leave-stdin-open", 0, 0, G_OPTION_ARG_NONE, &opt_leave_stdin_open, "Leave stdin open when attached client disconnects", NULL},
	{"cid", 'c', 0, G_OPTION_ARG_STRING, &opt_cid, "Container ID", NULL},
	{"cuuid", 'u', 0, G_OPTION_ARG_STRING, &opt_cuuid, "Container UUID", NULL},
	{"runtime", 'r', 0, G_OPTION_ARG_STRING, &opt_runtime_path, "Runtime path", NULL},
	{"restore", 0, 0, G_OPTION_ARG_STRING, &opt_restore_path, "Restore a container from a checkpoint", NULL},
	{"no-new-keyring", 0, 0, G_OPTION_ARG_NONE, &opt_no_new_keyring, "Do not create a new session keyring for the container", NULL},
	{"no-pivot", 0, 0, G_OPTION_ARG_NONE, &opt_no_pivot, "Do not use pivot_root", NULL},
	{"replace-listen-pid", 0, 0, G_OPTION_ARG_NONE, &opt_replace_listen_pid, "Replace listen pid if set for oci-runtime pid", NULL},
	{"bundle", 'b', 0, G_OPTION_ARG_STRING, &opt_bundle_path, "Bundle path", NULL},
	{"pidfile", 0, 0, G_OPTION_ARG_STRING, &opt_container_pid_file, "PID file (DEPRECATED)", NULL},
	{"container-pidfile", 'p', 0, G_OPTION_ARG_STRING, &opt_container_pid_file, "Container PID file", NULL},
	{"conmon-pidfile", 'P', 0, G_OPTION_ARG_STRING, &opt_conmon_pid_file, "Conmon daemon PID file", NULL},
	{"systemd-cgroup", 's', 0, G_OPTION_ARG_NONE, &opt_systemd_cgroup, "Enable systemd cgroup manager", NULL},
	{"exec", 'e', 0, G_OPTION_ARG_NONE, &opt_exec, "Exec a command in a running container", NULL},
	{"exec-process-spec", 0, 0, G_OPTION_ARG_STRING, &opt_exec_process_spec, "Path to the process spec for exec", NULL},
	{"exit-dir", 0, 0, G_OPTION_ARG_STRING, &opt_exit_dir, "Path to the directory where exit files are written", NULL},
	{"exit-command", 0, 0, G_OPTION_ARG_STRING, &opt_exit_command,
	 "Path to the program to execute when the container terminates its execution", NULL},
	{"exit-command-arg", 0, 0, G_OPTION_ARG_STRING_ARRAY, &opt_exit_args,
	 "Additional arg to pass to the exit command.  Can be specified multiple times", NULL},
	{"log-path", 'l', 0, G_OPTION_ARG_STRING, &opt_log_path, "Log file path", NULL},
	{"timeout", 'T', 0, G_OPTION_ARG_INT, &opt_timeout, "Timeout in seconds", NULL},
	{"log-size-max", 0, 0, G_OPTION_ARG_INT64, &opt_log_size_max, "Maximum size of log file", NULL},
	{"socket-dir-path", 0, 0, G_OPTION_ARG_STRING, &opt_socket_path, "Location of container attach sockets", NULL},
	{"version", 0, 0, G_OPTION_ARG_NONE, &opt_version, "Print the version and exit", NULL},
	{"syslog", 0, 0, G_OPTION_ARG_NONE, &opt_syslog, "Log to syslog (use with cgroupfs cgroup manager)", NULL},
	{"log-level", 0, 0, G_OPTION_ARG_STRING, &opt_log_level, "Print debug logs based on log level", NULL},
	{NULL}};

/* strlen("1997-03-25T13:20:42.999999999+01:00 stdout ") + 1 */
#define TSBUFLEN 44

#define CGROUP_ROOT "/sys/fs/cgroup"
#define OOM_SCORE "-1000"

static int log_fd = -1;

#define pexit(s) \
	do { \
		fprintf(stderr, "[conmon:e]: %s %s\n", s, strerror(errno)); \
		if (opt_syslog) \
			syslog(LOG_ERR, "conmon %.20s <error>: %s %s\n", opt_cid, s, strerror(errno)); \
		exit(EXIT_FAILURE); \
	} while (0)

#define pexitf(fmt, ...) \
	do { \
		fprintf(stderr, "[conmon:e]: " fmt " %s\n", ##__VA_ARGS__, strerror(errno)); \
		if (opt_syslog) \
			syslog(LOG_ERR, "conmon %.20s <error>: " fmt ": %s\n", opt_cid, ##__VA_ARGS__, strerror(errno)); \
		exit(EXIT_FAILURE); \
	} while (0)

#define pwarn(s) \
	do { \
		fprintf(stderr, "[conmon:w]: %s %s\n", s, strerror(errno)); \
		if (opt_syslog) \
			syslog(LOG_INFO, "conmon %.20s <pwarn>: %s %s\n", opt_cid, s, strerror(errno)); \
	} while (0)

#define nexit(s) \
	do { \
		fprintf(stderr, "[conmon:e] %s\n", s); \
		if (opt_syslog) \
			syslog(LOG_ERR, "conmon %.20s <error>: %s\n", opt_cid, s); \
		exit(EXIT_FAILURE); \
	} while (0)

#define nexitf(fmt, ...) \
	do { \
		fprintf(stderr, "[conmon:e]: " fmt "\n", ##__VA_ARGS__); \
		if (opt_syslog) \
			syslog(LOG_ERR, "conmon %.20s <error>: " fmt " \n", opt_cid, ##__VA_ARGS__); \
		exit(EXIT_FAILURE); \
	} while (0)

#define nwarn(s) \
	if (parse_level(opt_log_level) >= WARN_LEVEL) { \
		do { \
			fprintf(stderr, "[conmon:w]: %s\n", s); \
			if (opt_syslog) \
				syslog(LOG_INFO, "conmon %.20s <nwarn>: %s\n", opt_cid, s); \
		} while (0); \
	}

#define nwarnf(fmt, ...) \
	if (parse_level(opt_log_level) >= WARN_LEVEL) { \
		do { \
			fprintf(stderr, "[conmon:w]: " fmt "\n", ##__VA_ARGS__); \
			if (opt_syslog) \
				syslog(LOG_INFO, "conmon %.20s <nwarn>: " fmt " \n", opt_cid, ##__VA_ARGS__); \
		} while (0); \
	}

#define ninfo(s) \
	if (parse_level(opt_log_level) >= INFO_LEVEL) { \
		do { \
			fprintf(stderr, "[conmon:i]: %s\n", s); \
			if (opt_syslog) \
				syslog(LOG_INFO, "conmon %.20s <ninfo>: %s\n", opt_cid, s); \
		} while (0); \
	}

#define ninfof(fmt, ...) \
	if (parse_level(opt_log_level) >= INFO_LEVEL) { \
		do { \
			fprintf(stderr, "[conmon:i]: " fmt "\n", ##__VA_ARGS__); \
			if (opt_syslog) \
				syslog(LOG_INFO, "conmon %.20s <ninfo>: " fmt " \n", opt_cid, ##__VA_ARGS__); \
		} while (0); \
	}

#define ndebug(s) \
	if (parse_level(opt_log_level) >= DEBUG_LEVEL) { \
		do { \
			fprintf(stderr, "[conmon:d]: %s\n", s); \
			if (opt_syslog) \
				syslog(LOG_INFO, "conmon %.20s <ndebug>: %s\n", opt_cid, s); \
		} while (0); \
	}

#define ndebugf(fmt, ...) \
	if (parse_level(opt_log_level) >= DEBUG_LEVEL) { \
		do { \
			fprintf(stderr, "[conmon:d]: " fmt "\n", ##__VA_ARGS__); \
			if (opt_syslog) \
				syslog(LOG_INFO, "conmon %.20s <ndebug>: " fmt " \n", opt_cid, ##__VA_ARGS__); \
		} while (0); \
	}

/* Different levels of logging */
typedef enum {
	EXIT_LEVEL,
	WARN_LEVEL,
	INFO_LEVEL,
	DEBUG_LEVEL,
} log_level_t;

/* Parse_level parses the string value of the --log_level flag to its matching enum */
static log_level_t parse_level(char *level_name)
{
	if (!strcmp(level_name, "error") || !strcmp(level_name, "fatal") || !strcmp(level_name, "panic")) {
		return EXIT_LEVEL;
	} else if (!strcmp(level_name, "warn") || !strcmp(level_name, "warning")) {
		return WARN_LEVEL;
	} else if (!strcmp(level_name, "info")) {
		return INFO_LEVEL;
	} else if (!strcmp(level_name, "debug")) {
		return DEBUG_LEVEL;
	}
	nexitf("No such log level %s", level_name);
}

static ssize_t write_all(int fd, const void *buf, size_t count)
{
	size_t remaining = count;
	const char *p = buf;
	ssize_t res;

	while (remaining > 0) {
		do {
			res = write(fd, p, remaining);
		} while (res == -1 && errno == EINTR);

		if (res <= 0)
			return -1;

		remaining -= res;
		p += res;
	}

	return count;
}

#define WRITEV_BUFFER_N_IOV 128

typedef struct {
	int iovcnt;
	struct iovec iov[WRITEV_BUFFER_N_IOV];
} writev_buffer_t;

static ssize_t writev_buffer_flush(int fd, writev_buffer_t *buf)
{
	size_t count = 0;
	ssize_t res;
	struct iovec *iov;
	int iovcnt;

	iovcnt = buf->iovcnt;
	iov = buf->iov;

	while (iovcnt > 0) {
		do {
			res = writev(fd, iov, iovcnt);
		} while (res == -1 && errno == EINTR);

		if (res <= 0)
			return -1;

		count += res;

		while (res > 0) {
			size_t from_this = MIN((size_t)res, iov->iov_len);
			iov->iov_len -= from_this;
			iov->iov_base += from_this;
			res -= from_this;

			if (iov->iov_len == 0) {
				iov++;
				iovcnt--;
			}
		}
	}

	buf->iovcnt = 0;

	return count;
}

ssize_t writev_buffer_append_segment(int fd, writev_buffer_t *buf, const void *data, ssize_t len)
{
	if (data == NULL)
		return 1;

	if (buf->iovcnt == WRITEV_BUFFER_N_IOV && writev_buffer_flush(fd, buf) < 0)
		return -1;

	if (len > 0) {
		buf->iov[buf->iovcnt].iov_base = (void *)data;
		buf->iov[buf->iovcnt].iov_len = (size_t)len;
		buf->iovcnt++;
	}

	return 1;
}

int set_k8s_timestamp(char *buf, ssize_t buflen, const char *pipename)
{
	struct tm *tm;
	struct timespec ts;
	char off_sign = '+';
	int off, len, err = -1;

	if (clock_gettime(CLOCK_REALTIME, &ts) < 0) {
		/* If CLOCK_REALTIME is not supported, we set nano seconds to 0 */
		if (errno == EINVAL) {
			ts.tv_nsec = 0;
		} else {
			return err;
		}
	}

	if ((tm = localtime(&ts.tv_sec)) == NULL)
		return err;


	off = (int)tm->tm_gmtoff;
	if (tm->tm_gmtoff < 0) {
		off_sign = '-';
		off = -off;
	}

	len = snprintf(buf, buflen, "%d-%02d-%02dT%02d:%02d:%02d.%09ld%c%02d:%02d %s ", tm->tm_year + 1900, tm->tm_mon + 1, tm->tm_mday,
		       tm->tm_hour, tm->tm_min, tm->tm_sec, ts.tv_nsec, off_sign, off / 3600, (off % 3600) / 60, pipename);

	if (len < buflen)
		err = 0;
	return err;
}

/* stdpipe_t represents one of the std pipes (or NONE).
 * Sync with const in container_attach.go */
typedef enum {
	NO_PIPE,
	STDIN_PIPE, /* unused */
	STDOUT_PIPE,
	STDERR_PIPE,
} stdpipe_t;

const char *stdpipe_name(stdpipe_t pipe)
{
	switch (pipe) {
	case STDIN_PIPE:
		return "stdin";
	case STDOUT_PIPE:
		return "stdout";
	case STDERR_PIPE:
		return "stderr";
	default:
		return "NONE";
	}
}

/*
 * reopen_log_file reopens the log file fd.
 */
static void reopen_log_file(void)
{
	_cleanup_free_ char *opt_log_path_tmp = g_strdup_printf("%s.tmp", opt_log_path);

	/* Sync the logs to disk */
	if (fsync(log_fd) < 0) {
		pwarn("Failed to sync log file on reopen");
	}

	/* Close the current log_fd */
	close(log_fd);

	/* Open the log path file again */
	log_fd = open(opt_log_path_tmp, O_WRONLY | O_TRUNC | O_CREAT | O_CLOEXEC, 0600);
	if (log_fd < 0)
		pexitf("Failed to open log file %s", opt_log_path);

	/* Replace the previous file */
	if (rename(opt_log_path_tmp, opt_log_path) < 0) {
		pexit("Failed to rename log file");
	}
}

/*
 * The CRI requires us to write logs with a (timestamp, stream, line) format
 * for every newline-separated line. write_k8s_log writes said format for every
 * line in buf, and will partially write the final line of the log if buf is
 * not terminated by a newline.
 */
static int write_k8s_log(int fd, stdpipe_t pipe, const char *buf, ssize_t buflen)
{
	char tsbuf[TSBUFLEN];
	writev_buffer_t bufv = {0};
	static int64_t bytes_written = 0;
	int64_t bytes_to_be_written = 0;

	/*
	 * Use the same timestamp for every line of the log in this buffer.
	 * There is no practical difference in the output since write(2) is
	 * fast.
	 */
	if (set_k8s_timestamp(tsbuf, sizeof tsbuf, stdpipe_name(pipe)))
		/* TODO: We should handle failures much more cleanly than this. */
		return -1;

	while (buflen > 0) {
		const char *line_end = NULL;
		ptrdiff_t line_len = 0;
		bool partial = FALSE;

		/* Find the end of the line, or alternatively the end of the buffer. */
		line_end = memchr(buf, '\n', buflen);
		if (line_end == NULL) {
			line_end = &buf[buflen - 1];
			partial = TRUE;
		}
		line_len = line_end - buf + 1;

		/* This is line_len bytes + TSBUFLEN - 1 + 2 (- 1 is for ignoring \0). */
		bytes_to_be_written = line_len + TSBUFLEN + 1;

		/* If partial, then we add a \n */
		if (partial) {
			bytes_to_be_written += 1;
		}

		/*
		 * We re-open the log file if writing out the bytes will exceed the max
		 * log size. We also reset the state so that the new file is started with
		 * a timestamp.
		 */
		if ((opt_log_size_max > 0) && (bytes_written + bytes_to_be_written) > opt_log_size_max) {
			bytes_written = 0;

			if (writev_buffer_flush(fd, &bufv) < 0) {
				nwarn("failed to flush buffer to log");
				/*
				 * We are going to reopen the file anyway, in case of
				 * errors discard all we have in the buffer.
				 */
				bufv.iovcnt = 0;
			}
			reopen_log_file();

			/* Reassign to the new log file fd */
			fd = log_fd;
		}

		/* Output the timestamp */
		if (writev_buffer_append_segment(fd, &bufv, tsbuf, TSBUFLEN - 1) < 0) {
			nwarn("failed to write (timestamp, stream) to log");
			goto next;
		}

		/* Output log tag for partial or newline */
		if (partial) {
			if (writev_buffer_append_segment(fd, &bufv, "P ", 2) < 0) {
				nwarn("failed to write partial log tag");
				goto next;
			}
		} else {
			if (writev_buffer_append_segment(fd, &bufv, "F ", 2) < 0) {
				nwarn("failed to write end log tag");
				goto next;
			}
		}

		/* Output the actual contents. */
		if (writev_buffer_append_segment(fd, &bufv, buf, line_len) < 0) {
			nwarn("failed to write buffer to log");
			goto next;
		}

		/* Output a newline for partial */
		if (partial) {
			if (writev_buffer_append_segment(fd, &bufv, "\n", 1) < 0) {
				nwarn("failed to write newline to log");
				goto next;
			}
		}

		bytes_written += bytes_to_be_written;
	next:
		/* Update the head of the buffer remaining to output. */
		buf += line_len;
		buflen -= line_len;
	}

	if (writev_buffer_flush(fd, &bufv) < 0) {
		nwarn("failed to flush buffer to log");
	}

	return 0;
}

/*
 * Returns the path for specified controller name for a pid.
 * Returns NULL on error.
 */
static char *process_cgroup_subsystem_path(int pid, const char *subsystem)
{
	_cleanup_free_ char *cgroups_file_path = g_strdup_printf("/proc/%d/cgroup", pid);
	_cleanup_fclose_ FILE *fp = NULL;
	fp = fopen(cgroups_file_path, "re");
	if (fp == NULL) {
		nwarnf("Failed to open cgroups file: %s", cgroups_file_path);
		return NULL;
	}

	_cleanup_free_ char *line = NULL;
	ssize_t read;
	size_t len = 0;
	char *ptr, *path;
	char *subsystem_path = NULL;
	int i;
	while ((read = getline(&line, &len, fp)) != -1) {
		_cleanup_strv_ char **subsystems = NULL;
		ptr = strchr(line, ':');
		if (ptr == NULL) {
			nwarnf("Error parsing cgroup, ':' not found: %s", line);
			return NULL;
		}
		ptr++;
		path = strchr(ptr, ':');
		if (path == NULL) {
			nwarnf("Error parsing cgroup, second ':' not found: %s", line);
			return NULL;
		}
		*path = 0;
		path++;
		subsystems = g_strsplit(ptr, ",", -1);
		for (i = 0; subsystems[i] != NULL; i++) {
			if (strcmp(subsystems[i], subsystem) == 0) {
				char *subpath = strchr(subsystems[i], '=');
				if (subpath == NULL) {
					subpath = ptr;
				} else {
					*subpath = 0;
				}

				subsystem_path = g_strdup_printf("%s/%s%s", CGROUP_ROOT, subpath, path);
				subsystem_path[strlen(subsystem_path) - 1] = '\0';
				return subsystem_path;
			}
		}
	}

	return NULL;
}

static char *escape_json_string(const char *str)
{
	GString *escaped;
	const char *p;

	p = str;
	escaped = g_string_sized_new(strlen(str));

	while (*p != 0) {
		char c = *p++;
		if (c == '\\' || c == '"') {
			g_string_append_c(escaped, '\\');
			g_string_append_c(escaped, c);
		} else if (c == '\n') {
			g_string_append_printf(escaped, "\\n");
		} else if (c == '\t') {
			g_string_append_printf(escaped, "\\t");
		} else if ((c > 0 && c < 0x1f) || c == 0x7f) {
			g_string_append_printf(escaped, "\\u00%02x", (guint)c);
		} else {
			g_string_append_c(escaped, c);
		}
	}

	return g_string_free(escaped, FALSE);
}

static int get_pipe_fd_from_env(const char *envname)
{
	char *pipe_str, *endptr;
	int pipe_fd;

	pipe_str = getenv(envname);
	if (pipe_str == NULL)
		return -1;

	errno = 0;
	pipe_fd = strtol(pipe_str, &endptr, 10);
	if (errno != 0 || *endptr != '\0')
		pexitf("unable to parse %s", envname);
	if (fcntl(pipe_fd, F_SETFD, FD_CLOEXEC) == -1)
		pexitf("unable to make %s CLOEXEC", envname);

	return pipe_fd;
}

static void add_argv(GPtrArray *argv_array, ...) G_GNUC_NULL_TERMINATED;

static void add_argv(GPtrArray *argv_array, ...)
{
	va_list args;
	char *arg;

	va_start(args, argv_array);
	while ((arg = va_arg(args, char *)))
		g_ptr_array_add(argv_array, arg);
	va_end(args);
}

static void end_argv(GPtrArray *argv_array)
{
	g_ptr_array_add(argv_array, NULL);
}

/* Global state */

static int runtime_status = -1;
static int container_status = -1;

static int masterfd_stdin = -1;
static int masterfd_stdout = -1;
static int masterfd_stderr = -1;

/* Used for attach */
struct conn_sock_s {
	int fd;
	gboolean readable;
	gboolean writable;
};
GPtrArray *conn_socks = NULL;

static int oom_event_fd = -1;
static int attach_socket_fd = -1;
static int console_socket_fd = -1;
static int terminal_ctrl_fd = -1;

static gboolean timed_out = FALSE;

static GMainLoop *main_loop = NULL;

static void conn_sock_shutdown(struct conn_sock_s *sock, int how)
{
	if (sock->fd == -1)
		return;
	shutdown(sock->fd, how);
	if (how & SHUT_RD)
		sock->readable = false;
	if (how & SHUT_WR)
		sock->writable = false;
	if (!sock->writable && !sock->readable) {
		close(sock->fd);
		sock->fd = -1;
		g_ptr_array_remove(conn_socks, sock);
	}
}

static gboolean stdio_cb(int fd, GIOCondition condition, gpointer user_data);

static gboolean tty_hup_timeout_scheduled = false;

static gboolean tty_hup_timeout_cb(G_GNUC_UNUSED gpointer user_data)
{
	tty_hup_timeout_scheduled = false;
	g_unix_fd_add(masterfd_stdout, G_IO_IN, stdio_cb, GINT_TO_POINTER(STDOUT_PIPE));
	return G_SOURCE_REMOVE;
}

static bool read_stdio(int fd, stdpipe_t pipe, gboolean *eof)
{
	/* We use one extra byte at the start, which we don't read into, instead
	   we use that for marking the pipe when we write to the attached socket */
	char real_buf[STDIO_BUF_SIZE + 1];
	char *buf = real_buf + 1;
	ssize_t num_read = 0;
	size_t i;

	if (eof)
		*eof = false;

	num_read = read(fd, buf, STDIO_BUF_SIZE);
	if (num_read == 0) {
		if (eof)
			*eof = true;
		return false;
	} else if (num_read < 0) {
		nwarnf("stdio_input read failed %s", strerror(errno));
		return false;
	} else {
		if (write_k8s_log(log_fd, pipe, buf, num_read) < 0) {
			nwarn("write_k8s_log failed");
			return G_SOURCE_CONTINUE;
		}

		if (conn_socks == NULL) {
			return true;
		}

		real_buf[0] = pipe;
		for (i = conn_socks->len; i > 0; i--) {
			struct conn_sock_s *conn_sock = g_ptr_array_index(conn_socks, i - 1);

			if (conn_sock->writable && write_all(conn_sock->fd, real_buf, num_read + 1) < 0) {
				nwarn("Failed to write to socket");
				conn_sock_shutdown(conn_sock, SHUT_WR);
			}
		}
		return true;
	}
}

static void on_sigchld(G_GNUC_UNUSED int signal)
{
	raise(SIGUSR1);
}

static void on_sig_exit(int signal)
{
	if (container_pid > 0) {
		if (kill(container_pid, signal) == 0)
			return;
	} else if (create_pid > 0) {
		if (kill(create_pid, signal) == 0)
			return;
		if (errno == ESRCH) {
			/* The create_pid process might have exited, so try container_pid again.  */
			if (container_pid > 0 && kill(container_pid, signal) == 0)
				return;
		}
	}
	/* Just force a check if we get here.  */
	raise(SIGUSR1);
}

static void check_child_processes(GHashTable *pid_to_handler)
{
	void (*cb)(GPid, int, gpointer);

	for (;;) {
		int status;
		pid_t pid = waitpid(-1, &status, WNOHANG);

		if (pid < 0 && errno == EINTR)
			continue;
		if (pid < 0 && errno == ECHILD) {
			g_main_loop_quit(main_loop);
			return;
		}
		if (pid < 0)
			pexit("Failed to read child process status");

		if (pid == 0)
			return;

		/* If we got here, pid > 0, so we have a valid pid to check.  */
		cb = g_hash_table_lookup(pid_to_handler, &pid);
		if (cb)
			cb(pid, status, 0);
	}
}

static gboolean on_sigusr1_cb(gpointer user_data)
{
	GHashTable *pid_to_handler = (GHashTable *)user_data;
	check_child_processes(pid_to_handler);
	return G_SOURCE_CONTINUE;
}

static gboolean stdio_cb(int fd, GIOCondition condition, gpointer user_data)
{
	stdpipe_t pipe = GPOINTER_TO_INT(user_data);
	gboolean read_eof = FALSE;
	gboolean has_input = (condition & G_IO_IN) != 0;
	gboolean has_hup = (condition & G_IO_HUP) != 0;

	/* When we get here, condition can be G_IO_IN and/or G_IO_HUP.
	   IN means there is some data to read.
	   HUP means the other side closed the fd. In the case of a pine
	   this in final, and we will never get more data. However, in the
	   terminal case this just means that nobody has the terminal
	   open at this point, and this can be change whenever someone
	   opens the tty */

	/* Read any data before handling hup */
	if (has_input) {
		read_stdio(fd, pipe, &read_eof);
	}

	if (has_hup && opt_terminal && pipe == STDOUT_PIPE) {
		/* We got a HUP from the terminal master this means there
		   are no open slaves ptys atm, and we will get a lot
		   of wakeups until we have one, switch to polling
		   mode. */

		/* If we read some data this cycle, wait one more, maybe there
		   is more in the buffer before we handle the hup */
		if (has_input && !read_eof) {
			return G_SOURCE_CONTINUE;
		}

		if (!tty_hup_timeout_scheduled) {
			g_timeout_add(100, tty_hup_timeout_cb, NULL);
		}
		tty_hup_timeout_scheduled = true;
		return G_SOURCE_REMOVE;
	}

	if (read_eof || (has_hup && !has_input)) {
		/* End of input */
		if (pipe == STDOUT_PIPE)
			masterfd_stdout = -1;
		if (pipe == STDERR_PIPE)
			masterfd_stderr = -1;

		close(fd);
		return G_SOURCE_REMOVE;
	}

	return G_SOURCE_CONTINUE;
}

static gboolean timeout_cb(G_GNUC_UNUSED gpointer user_data)
{
	timed_out = TRUE;
	ninfo("Timed out, killing main loop");
	g_main_loop_quit(main_loop);
	return G_SOURCE_REMOVE;
}

static gboolean oom_cb(int fd, GIOCondition condition, G_GNUC_UNUSED gpointer user_data)
{
	uint64_t oom_event;
	ssize_t num_read = 0;

	if ((condition & G_IO_IN) != 0) {
		num_read = read(fd, &oom_event, sizeof(uint64_t));
		if (num_read < 0) {
			nwarn("Failed to read oom event from eventfd");
			return G_SOURCE_CONTINUE;
		}

		if (num_read > 0) {
			_cleanup_close_ int oom_fd = -1;
			if (num_read != sizeof(uint64_t))
				nwarn("Failed to read full oom event from eventfd");
			ninfo("OOM received");
			oom_fd = open("oom", O_CREAT, 0666);
			if (oom_fd < 0) {
				nwarn("Failed to write oom file");
			}
			return G_SOURCE_CONTINUE;
		}
	}

	/* End of input */
	close(fd);
	oom_event_fd = -1;
	return G_SOURCE_REMOVE;
}

#define CONN_SOCK_BUF_SIZE 32 * 1024 /* Match the write size in CopyDetachable */
static gboolean conn_sock_cb(int fd, GIOCondition condition, gpointer user_data)
{
	char buf[CONN_SOCK_BUF_SIZE];
	ssize_t num_read = 0;
	struct conn_sock_s *sock = (struct conn_sock_s *)user_data;

	if ((condition & G_IO_IN) != 0) {
		num_read = read(fd, buf, CONN_SOCK_BUF_SIZE);
		if (num_read < 0)
			return G_SOURCE_CONTINUE;

		if (num_read > 0 && masterfd_stdin >= 0) {
			if (write_all(masterfd_stdin, buf, num_read) < 0) {
				nwarn("Failed to write to container stdin");
			}
			return G_SOURCE_CONTINUE;
		}
	}

	/* End of input */
	conn_sock_shutdown(sock, SHUT_RD);
	if (masterfd_stdin >= 0 && opt_stdin) {
		if (!opt_leave_stdin_open) {
			close(masterfd_stdin);
			masterfd_stdin = -1;
		} else {
			ninfo("Not closing input");
		}
	}
	return G_SOURCE_REMOVE;
}

static gboolean attach_cb(int fd, G_GNUC_UNUSED GIOCondition condition, G_GNUC_UNUSED gpointer user_data)
{
	int conn_fd = accept(fd, NULL, NULL);
	if (conn_fd == -1) {
		if (errno != EWOULDBLOCK)
			nwarn("Failed to accept client connection on attach socket");
	} else {
		struct conn_sock_s *conn_sock;
		if (conn_socks == NULL) {
			conn_socks = g_ptr_array_new_with_free_func(free);
		}
		conn_sock = malloc(sizeof(*conn_sock));
		if (conn_sock == NULL) {
			pexit("Failed to allocate memory");
		}
		conn_sock->fd = conn_fd;
		conn_sock->readable = true;
		conn_sock->writable = true;
		g_unix_fd_add(conn_sock->fd, G_IO_IN | G_IO_HUP | G_IO_ERR, conn_sock_cb, conn_sock);
		g_ptr_array_add(conn_socks, conn_sock);
		ninfof("Accepted connection %d", conn_sock->fd);
	}

	return G_SOURCE_CONTINUE;
}

/*
 * resize_winsz resizes the pty window size.
 */
static void resize_winsz(int height, int width)
{
	struct winsize ws;
	int ret;

	ws.ws_row = height;
	ws.ws_col = width;
	ret = ioctl(masterfd_stdout, TIOCSWINSZ, &ws);
	if (ret == -1) {
		nwarn("Failed to set process pty terminal size");
	}
}

#define CTLBUFSZ 200
static gboolean ctrl_cb(int fd, G_GNUC_UNUSED GIOCondition condition, G_GNUC_UNUSED gpointer user_data)
{
	static char ctlbuf[CTLBUFSZ];
	static int readsz = CTLBUFSZ - 1;
	static char *readptr = ctlbuf;
	ssize_t num_read = 0;
	int ctl_msg_type = -1;
	int height = -1;
	int width = -1;
	int ret;

	num_read = read(fd, readptr, readsz);
	if (num_read <= 0) {
		nwarn("Failed to read from control fd");
		return G_SOURCE_CONTINUE;
	}

	readptr[num_read] = '\0';
	ninfof("Got ctl message: %s", ctlbuf);

	char *beg = ctlbuf;
	char *newline = strchrnul(beg, '\n');
	/* Process each message which ends with a line */
	while (*newline != '\0') {
		ret = sscanf(ctlbuf, "%d %d %d\n", &ctl_msg_type, &height, &width);
		if (ret != 3) {
			nwarn("Failed to sscanf message");
			return G_SOURCE_CONTINUE;
		}
		ninfof("Message type: %d, Height: %d, Width: %d", ctl_msg_type, height, width);
		switch (ctl_msg_type) {
		// This matches what we write from container_attach.go
		case 1:
			resize_winsz(height, width);
			break;
		case 2:
			reopen_log_file();
			break;
		default:
			ninfof("Unknown message type: %d", ctl_msg_type);
			break;
		}
		beg = newline + 1;
		newline = strchrnul(beg, '\n');
	}
	if (num_read == (CTLBUFSZ - 1) && beg == ctlbuf) {
		/*
		 * We did not find a newline in the entire buffer.
		 * This shouldn't happen as our buffer is larger than
		 * the message that we expect to receive.
		 */
		nwarn("Could not find newline in entire buffer");
	} else if (*beg == '\0') {
		/* We exhausted all messages that were complete */
		readptr = ctlbuf;
		readsz = CTLBUFSZ - 1;
	} else {
		/*
		 * We copy remaining data to beginning of buffer
		 * and advance readptr after that.
		 */
		int cp_rem = 0;
		do {
			ctlbuf[cp_rem++] = *beg++;
		} while (*beg != '\0');
		readptr = ctlbuf + cp_rem;
		readsz = CTLBUFSZ - 1 - cp_rem;
	}

	return G_SOURCE_CONTINUE;
}

static gboolean terminal_accept_cb(int fd, G_GNUC_UNUSED GIOCondition condition, G_GNUC_UNUSED gpointer user_data)
{
	const char *csname = user_data;
	struct file_t console;
	int connfd = -1;
	struct termios tset;

	ninfof("about to accept from console_socket_fd: %d", fd);
	connfd = accept4(fd, NULL, NULL, SOCK_CLOEXEC);
	if (connfd < 0) {
		nwarn("Failed to accept console-socket connection");
		return G_SOURCE_CONTINUE;
	}

	/* Not accepting anything else. */
	close(fd);
	unlink(csname);

	/* We exit if this fails. */
	ninfof("about to recvfd from connfd: %d", connfd);
	console = recvfd(connfd);

	ninfof("console = {.name = '%s'; .fd = %d}", console.name, console.fd);
	free(console.name);

	/* We change the terminal settings to match kube settings */
	if (tcgetattr(console.fd, &tset) == -1)
		pexit("Failed to get console terminal settings");

	tset.c_oflag |= ONLCR;

	if (tcsetattr(console.fd, TCSANOW, &tset) == -1)
		pexit("Failed to set console terminal settings");

	/* We only have a single fd for both pipes, so we just treat it as
	 * stdout. stderr is ignored. */
	masterfd_stdin = console.fd;
	masterfd_stdout = console.fd;

	/* Clean up everything */
	close(connfd);

	return G_SOURCE_CONTINUE;
}

static int get_exit_status(int status)
{
	if (WIFEXITED(status))
		return WEXITSTATUS(status);
	if (WIFSIGNALED(status))
		return 128 + WTERMSIG(status);
	return -1;
}

static void runtime_exit_cb(G_GNUC_UNUSED GPid pid, int status, G_GNUC_UNUSED gpointer user_data)
{
	runtime_status = status;
	create_pid = -1;
	g_main_loop_quit(main_loop);
}

static void container_exit_cb(G_GNUC_UNUSED GPid pid, int status, G_GNUC_UNUSED gpointer user_data)
{
	if (get_exit_status(status) != 0) {
		ninfof("container %d exited with status %d", pid, get_exit_status(status));
	}
	container_status = status;
	container_pid = -1;
	g_main_loop_quit(main_loop);
}

static void write_sync_fd(int sync_pipe_fd, int res, const char *message)
{
	_cleanup_free_ char *escaped_message = NULL;
	_cleanup_free_ char *json = NULL;
	const char *res_key;
	ssize_t len;

	if (sync_pipe_fd == -1)
		return;

	if (opt_exec)
		res_key = "exit_code";
	else
		res_key = "pid";

	if (message) {
		escaped_message = escape_json_string(message);
		json = g_strdup_printf("{\"%s\": %d, \"message\": \"%s\"}\n", res_key, res, escaped_message);
	} else {
		json = g_strdup_printf("{\"%s\": %d}\n", res_key, res);
	}

	len = strlen(json);
	if (write_all(sync_pipe_fd, json, len) != len) {
		pexit("Unable to send container stderr message to parent");
	}
}

static char *setup_console_socket(void)
{
	struct sockaddr_un addr = {0};
	_cleanup_free_ const char *tmpdir = g_get_tmp_dir();
	_cleanup_free_ char *csname = g_build_filename(tmpdir, "conmon-term.XXXXXX", NULL);
	/*
	 * Generate a temporary name. Is this unsafe? Probably, but we can
	 * replace it with a rename(2) setup if necessary.
	 */

	int unusedfd = g_mkstemp(csname);
	if (unusedfd < 0)
		pexit("Failed to generate random path for console-socket");
	close(unusedfd);

	addr.sun_family = AF_UNIX;
	strncpy(addr.sun_path, csname, sizeof(addr.sun_path) - 1);

	ninfof("addr{sun_family=AF_UNIX, sun_path=%s}", addr.sun_path);

	/* Bind to the console socket path. */
	console_socket_fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
	if (console_socket_fd < 0)
		pexit("Failed to create console-socket");
	if (fchmod(console_socket_fd, 0700))
		pexit("Failed to change console-socket permissions");
	/* XXX: This should be handled with a rename(2). */
	if (unlink(csname) < 0)
		pexit("Failed to unlink temporary random path");
	if (bind(console_socket_fd, (struct sockaddr *)&addr, sizeof(addr)) < 0)
		pexit("Failed to bind to console-socket");
	if (listen(console_socket_fd, 128) < 0)
		pexit("Failed to listen on console-socket");

	return g_strdup(csname);
}

static char *setup_attach_socket(void)
{
	_cleanup_free_ char *attach_sock_path = NULL;
	char *attach_symlink_dir_path;
	struct sockaddr_un attach_addr = {0};
	attach_addr.sun_family = AF_UNIX;

	/*
	 * Create a symlink so we don't exceed unix domain socket
	 * path length limit.
	 */
	attach_symlink_dir_path = g_build_filename(opt_socket_path, opt_cuuid, NULL);
	if (unlink(attach_symlink_dir_path) == -1 && errno != ENOENT)
		pexit("Failed to remove existing symlink for attach socket directory");

	if (symlink(opt_bundle_path, attach_symlink_dir_path) == -1)
		pexit("Failed to create symlink for attach socket");

	attach_sock_path = g_build_filename(opt_socket_path, opt_cuuid, "attach", NULL);
	ninfof("attach sock path: %s", attach_sock_path);

	strncpy(attach_addr.sun_path, attach_sock_path, sizeof(attach_addr.sun_path) - 1);
	ninfof("addr{sun_family=AF_UNIX, sun_path=%s}", attach_addr.sun_path);

	/*
	 * We make the socket non-blocking to avoid a race where client aborts connection
	 * before the server gets a chance to call accept. In that scenario, the server
	 * accept blocks till a new client connection comes in.
	 */
	attach_socket_fd = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
	if (attach_socket_fd == -1)
		pexit("Failed to create attach socket");

	if (fchmod(attach_socket_fd, 0700))
		pexit("Failed to change attach socket permissions");

	if (bind(attach_socket_fd, (struct sockaddr *)&attach_addr, sizeof(struct sockaddr_un)) == -1)
		pexitf("Failed to bind attach socket: %s", attach_sock_path);

	if (listen(attach_socket_fd, 10) == -1)
		pexitf("Failed to listen on attach socket: %s", attach_sock_path);

	g_unix_fd_add(attach_socket_fd, G_IO_IN, attach_cb, NULL);

	return attach_symlink_dir_path;
}

static void setup_terminal_control_fifo()
{
	_cleanup_free_ char *ctl_fifo_path = g_build_filename(opt_bundle_path, "ctl", NULL);
	ninfof("ctl fifo path: %s", ctl_fifo_path);

	/* Setup fifo for reading in terminal resize and other stdio control messages */

	if (mkfifo(ctl_fifo_path, 0666) == -1)
		pexitf("Failed to mkfifo at %s", ctl_fifo_path);

	terminal_ctrl_fd = open(ctl_fifo_path, O_RDONLY | O_NONBLOCK | O_CLOEXEC);
	if (terminal_ctrl_fd == -1)
		pexit("Failed to open control fifo");

	/*
	 * Open a dummy writer to prevent getting flood of POLLHUPs when
	 * last writer closes.
	 */
	int dummyfd = open(ctl_fifo_path, O_WRONLY | O_CLOEXEC);
	if (dummyfd == -1)
		pexit("Failed to open dummy writer for fifo");

	g_unix_fd_add(terminal_ctrl_fd, G_IO_IN, ctrl_cb, NULL);

	ninfof("terminal_ctrl_fd: %d", terminal_ctrl_fd);
}

static void setup_oom_handling(int container_pid)
{
	/* Setup OOM notification for container process */
	_cleanup_free_ char *memory_cgroup_path = process_cgroup_subsystem_path(container_pid, "memory");
	_cleanup_close_ int cfd = -1;
	int ofd = -1; /* Not closed */
	if (!memory_cgroup_path) {
		nexit("Failed to get memory cgroup path");
	}

	_cleanup_free_ char *memory_cgroup_file_path = g_build_filename(memory_cgroup_path, "cgroup.event_control", NULL);

	if ((cfd = open(memory_cgroup_file_path, O_WRONLY | O_CLOEXEC)) == -1) {
		nwarnf("Failed to open %s", memory_cgroup_file_path);
		return;
	}

	_cleanup_free_ char *memory_cgroup_file_oom_path = g_build_filename(memory_cgroup_path, "memory.oom_control", NULL);
	if ((ofd = open(memory_cgroup_file_oom_path, O_RDONLY | O_CLOEXEC)) == -1)
		pexitf("Failed to open %s", memory_cgroup_file_oom_path);

	if ((oom_event_fd = eventfd(0, EFD_CLOEXEC)) == -1)
		pexit("Failed to create eventfd");

	_cleanup_free_ char *data = g_strdup_printf("%d %d", oom_event_fd, ofd);
	if (write_all(cfd, data, strlen(data)) < 0)
		pexit("Failed to write to cgroup.event_control");

	g_unix_fd_add(oom_event_fd, G_IO_IN, oom_cb, NULL);
}

static void do_exit_command()
{
	gchar **args;
	size_t n_args = 0;

	/* Count the additional args, if any.  */
	if (opt_exit_args)
		for (; opt_exit_args[n_args]; n_args++)
			;

	args = malloc(sizeof(gchar *) * (n_args + 2));
	if (args == NULL)
		_exit(EXIT_FAILURE);

	args[0] = opt_exit_command;
	if (opt_exit_args)
		for (n_args = 0; opt_exit_args[n_args]; n_args++)
			args[n_args + 1] = opt_exit_args[n_args];
	args[n_args + 1] = NULL;

	execve(opt_exit_command, args, NULL);

	/* Should not happen, but better be safe. */
	_exit(EXIT_FAILURE);
}

int main(int argc, char *argv[])
{
	int ret;
	char cwd[PATH_MAX];
	_cleanup_free_ char *default_pid_file = NULL;
	_cleanup_free_ char *csname = NULL;
	GError *err = NULL;
	_cleanup_free_ char *contents = NULL;
	pid_t main_pid;
	/* Used for !terminal cases. */
	int slavefd_stdin = -1;
	int slavefd_stdout = -1;
	int slavefd_stderr = -1;
	char buf[BUF_SIZE];
	int num_read;
	int sync_pipe_fd = -1;
	int start_pipe_fd = -1;
	GError *error = NULL;
	GOptionContext *context;
	GPtrArray *runtime_argv = NULL;
	_cleanup_close_ int dev_null_r = -1;
	_cleanup_close_ int dev_null_w = -1;
	int fds[2];

	int oom_score_fd = -1;

	/* Command line parameters */
	context = g_option_context_new("- conmon utility");
	g_option_context_add_main_entries(context, opt_entries, "conmon");
	if (!g_option_context_parse(context, &argc, &argv, &error)) {
		g_print("option parsing failed: %s\n", error->message);
		exit(1);
	}
	if (opt_version) {
		g_print("conmon version " VERSION "\ncommit: " GIT_COMMIT "\n");
		exit(0);
	}

	if (opt_cid == NULL) {
		fprintf(stderr, "Container ID not provided. Use --cid\n");
		exit(EXIT_FAILURE);
	}

	oom_score_fd = open("/proc/self/oom_score_adj", O_WRONLY);
	if (oom_score_fd < 0) {
		ndebugf("failed to open /proc/self/oom_score_adj: %s\n", strerror(errno));
	} else {
		if (write(oom_score_fd, OOM_SCORE, strlen(OOM_SCORE)) < 0) {
			ndebugf("failed to write to /proc/self/oom_score_adj: %s\n", strerror(errno));
		}
		close(oom_score_fd);
	}


	main_loop = g_main_loop_new(NULL, FALSE);

	if (opt_restore_path && opt_exec)
		nexit("Cannot use 'exec' and 'restore' at the same time.");


	if (!opt_exec && opt_cuuid == NULL)
		nexit("Container UUID not provided. Use --cuuid");

	if (opt_runtime_path == NULL)
		nexit("Runtime path not provided. Use --runtime");
	if (access(opt_runtime_path, X_OK) < 0)
		pexitf("Runtime path %s is not valid", opt_runtime_path);

	if (opt_bundle_path == NULL && !opt_exec) {
		if (getcwd(cwd, sizeof(cwd)) == NULL) {
			nexit("Failed to get working directory");
		}
		opt_bundle_path = cwd;
	}

	dev_null_r = open("/dev/null", O_RDONLY | O_CLOEXEC);
	if (dev_null_r < 0)
		pexit("Failed to open /dev/null");

	dev_null_w = open("/dev/null", O_WRONLY | O_CLOEXEC);
	if (dev_null_w < 0)
		pexit("Failed to open /dev/null");

	if (opt_exec && opt_exec_process_spec == NULL) {
		nexit("Exec process spec path not provided. Use --exec-process-spec");
	}

	if (opt_container_pid_file == NULL) {
		default_pid_file = g_strdup_printf("%s/pidfile-%s", cwd, opt_cid);
		opt_container_pid_file = default_pid_file;
	}

	if (opt_log_path == NULL)
		nexit("Log file path not provided. Use --log-path");

	start_pipe_fd = get_pipe_fd_from_env("_OCI_STARTPIPE");
	if (start_pipe_fd >= 0) {
		/* Block for an initial write to the start pipe before
		   spawning any childred or exiting, to ensure the
		   parent can put us in the right cgroup. */
		num_read = read(start_pipe_fd, buf, BUF_SIZE);
		if (num_read < 0) {
			pexit("start-pipe read failed");
		}
		close(start_pipe_fd);
	}

	/* In the create-container case we double-fork in
	   order to disconnect from the parent, as we want to
	   continue in a daemon-like way */
	main_pid = fork();
	if (main_pid < 0) {
		pexit("Failed to fork the create command");
	} else if (main_pid != 0) {
		if (opt_conmon_pid_file) {
			char content[12];
			sprintf(content, "%i", main_pid);
			g_file_set_contents(opt_conmon_pid_file, content, strlen(content), &err);
			if (err) {
				nexitf("Failed to write conmon pidfile: %s", err->message);
			}
		}
		exit(0);
	}

	/* Disconnect stdio from parent. We need to do this, because
	   the parent is waiting for the stdout to end when the intermediate
	   child dies */
	if (dup2(dev_null_r, STDIN_FILENO) < 0)
		pexit("Failed to dup over stdin");
	if (dup2(dev_null_w, STDOUT_FILENO) < 0)
		pexit("Failed to dup over stdout");
	if (dup2(dev_null_w, STDERR_FILENO) < 0)
		pexit("Failed to dup over stderr");

	/* Create a new session group */
	setsid();

	/* Environment variables */
	sync_pipe_fd = get_pipe_fd_from_env("_OCI_SYNCPIPE");

	/* Open the log path file. */
	log_fd = open(opt_log_path, O_WRONLY | O_APPEND | O_CREAT | O_CLOEXEC, 0600);
	if (log_fd < 0)
		pexit("Failed to open log file");

	/*
	 * Set self as subreaper so we can wait for container process
	 * and return its exit code.
	 */
	ret = prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0);
	if (ret != 0) {
		pexit("Failed to set as subreaper");
	}

	if (opt_terminal) {
		csname = setup_console_socket();
	} else {

		/*
		 * Create a "fake" master fd so that we can use the same epoll code in
		 * both cases. The slavefd_*s will be closed after we dup over
		 * everything.
		 *
		 * We use pipes here because open(/dev/std{out,err}) will fail if we
		 * used anything else (and it wouldn't be a good idea to create a new
		 * pty pair in the host).
		 */

		if (opt_stdin) {
			if (pipe2(fds, O_CLOEXEC) < 0)
				pexit("Failed to create !terminal stdin pipe");

			masterfd_stdin = fds[1];
			slavefd_stdin = fds[0];
		}

		if (pipe2(fds, O_CLOEXEC) < 0)
			pexit("Failed to create !terminal stdout pipe");

		masterfd_stdout = fds[0];
		slavefd_stdout = fds[1];
	}

	/* We always create a stderr pipe, because that way we can capture
	   runc stderr messages before the tty is created */
	if (pipe2(fds, O_CLOEXEC) < 0)
		pexit("Failed to create stderr pipe");

	masterfd_stderr = fds[0];
	slavefd_stderr = fds[1];

	runtime_argv = g_ptr_array_new();
	add_argv(runtime_argv, opt_runtime_path, NULL);

	/* Generate the cmdline. */
	if (!opt_exec && opt_systemd_cgroup)
		add_argv(runtime_argv, "--systemd-cgroup", NULL);

	if (opt_exec) {
		add_argv(runtime_argv, "exec", "-d", "--pid-file", opt_container_pid_file, NULL);
	} else {
		char *command;
		if (opt_restore_path)
			command = "restore";
		else
			command = "create";

		add_argv(runtime_argv, command, "--bundle", opt_bundle_path, "--pid-file", opt_container_pid_file, NULL);

		if (opt_restore_path) {
			/*
			 * 'runc restore' is different from 'runc create'
			 * as the container is immediately running after
			 * a restore. Therefore the '--detach is needed'
			 * so that runc returns once the container is running.
			 *
			 * '--image-path' is the path to the checkpoint
			 * which will be become important when using pre-copy
			 * migration where multiple checkpoints can be created
			 * to reduce the container downtime during migration.
			 *
			 * '--work-path' is the directory CRIU will run in and
			 * also place its log files.
			 */
			add_argv(runtime_argv, "--detach", "--image-path", opt_restore_path, "--work-path", opt_bundle_path, NULL);
		}
	}

	if (!opt_exec && opt_no_pivot) {
		add_argv(runtime_argv, "--no-pivot", NULL);
	}

	if (!opt_exec && opt_no_new_keyring) {
		add_argv(runtime_argv, "--no-new-keyring", NULL);
	}

	if (csname != NULL) {
		add_argv(runtime_argv, "--console-socket", csname, NULL);
	}

	/* Set the exec arguments. */
	if (opt_exec) {
		add_argv(runtime_argv, "--process", opt_exec_process_spec, NULL);
	}

	/* Container name comes last. */
	add_argv(runtime_argv, opt_cid, NULL);
	end_argv(runtime_argv);

	sigset_t mask, oldmask;
	if ((sigemptyset(&mask) < 0) || (sigaddset(&mask, SIGTERM) < 0) || (sigaddset(&mask, SIGQUIT) < 0) || (sigaddset(&mask, SIGINT) < 0)
	    || sigprocmask(SIG_BLOCK, &mask, &oldmask) < 0)
		pexit("Failed to block signals");
	/*
	 * We have to fork here because the current runC API dups the stdio of the
	 * calling process over the container's fds. This is actually *very bad*
	 * but is currently being discussed for change in
	 * https://github.com/opencontainers/runtime-spec/pull/513. Hopefully this
	 * won't be the case for very long.
	 */

	/* Create our container. */
	create_pid = fork();
	if (create_pid < 0) {
		pexit("Failed to fork the create command");
	} else if (!create_pid) {
		/* FIXME: This results in us not outputting runc error messages to crio's log. */
		if (prctl(PR_SET_PDEATHSIG, SIGKILL) < 0)
			pexit("Failed to set PDEATHSIG");
		if (sigprocmask(SIG_SETMASK, &oldmask, NULL) < 0)
			pexit("Failed to unblock signals");

		if (slavefd_stdin < 0)
			slavefd_stdin = dev_null_r;
		if (dup2(slavefd_stdin, STDIN_FILENO) < 0)
			pexit("Failed to dup over stdout");

		if (slavefd_stdout < 0)
			slavefd_stdout = dev_null_w;
		if (dup2(slavefd_stdout, STDOUT_FILENO) < 0)
			pexit("Failed to dup over stdout");

		if (slavefd_stderr < 0)
			slavefd_stderr = slavefd_stdout;
		if (dup2(slavefd_stderr, STDERR_FILENO) < 0)
			pexit("Failed to dup over stderr");

		/* If LISTEN_PID env is set, we need to set the LISTEN_PID
		   it to the new child process */
		char *listenpid = getenv("LISTEN_PID");
		if (listenpid != NULL) {
			errno = 0;
			int lpid = strtol(listenpid, NULL, 10);
			if (errno != 0 || lpid <= 0)
				pexitf("Invalid LISTEN_PID %s", listenpid);
			if (opt_replace_listen_pid || lpid == getppid()) {
				gchar *pidstr = g_strdup_printf("%d", getpid());
				if (!pidstr)
					pexit("Failed to g_strdup_sprintf pid");
				if (setenv("LISTEN_PID", pidstr, true) < 0)
					pexit("Failed to setenv LISTEN_PID");
				free(pidstr);
			}
		}

		execv(g_ptr_array_index(runtime_argv, 0), (char **)runtime_argv->pdata);
		exit(127);
	}

	if ((signal(SIGTERM, on_sig_exit) == SIG_ERR) || (signal(SIGQUIT, on_sig_exit) == SIG_ERR)
	    || (signal(SIGINT, on_sig_exit) == SIG_ERR))
		pexit("Failed to register the signal handler");

	if (sigprocmask(SIG_SETMASK, &oldmask, NULL) < 0)
		pexit("Failed to unblock signals");

	if (opt_exit_command)
		atexit(do_exit_command);

	g_ptr_array_free(runtime_argv, TRUE);

	/* The runtime has that fd now. We don't need to touch it anymore. */
	close(slavefd_stdin);
	close(slavefd_stdout);
	close(slavefd_stderr);

	/* Map pid to its handler.  */
	GHashTable *pid_to_handler = g_hash_table_new(g_int_hash, g_int_equal);
	g_hash_table_insert(pid_to_handler, (pid_t *)&create_pid, runtime_exit_cb);

	/*
	 * Glib does not support SIGCHLD so use SIGUSR1 with the same semantic.  We will
	 * catch SIGCHLD and raise(SIGUSR1) in the signal handler.
	 */
	g_unix_signal_add(SIGUSR1, on_sigusr1_cb, pid_to_handler);

	if (signal(SIGCHLD, on_sigchld) == SIG_ERR)
		pexit("Failed to set handler for SIGCHLD");

	if (csname != NULL) {
		guint terminal_watch = g_unix_fd_add(console_socket_fd, G_IO_IN, terminal_accept_cb, csname);
		/* Process any SIGCHLD we may have missed before the signal handler was in place.  */
		check_child_processes(pid_to_handler);
		g_main_loop_run(main_loop);
		g_source_remove(terminal_watch);
	} else {
		int ret;
		/* Wait for our create child to exit with the return code. */
		do
			ret = waitpid(create_pid, &runtime_status, 0);
		while (ret < 0 && errno == EINTR);
		if (ret < 0) {
			if (create_pid > 0) {
				int old_errno = errno;
				kill(create_pid, SIGKILL);
				errno = old_errno;
			}
			pexitf("Failed to wait for `runtime %s`", opt_exec ? "exec" : "create");
		}
	}

	if (!WIFEXITED(runtime_status) || WEXITSTATUS(runtime_status) != 0) {
		if (sync_pipe_fd > 0) {
			/*
			 * Read from container stderr for any error and send it to parent
			 * We send -1 as pid to signal to parent that create container has failed.
			 */
			num_read = read(masterfd_stderr, buf, BUF_SIZE - 1);
			if (num_read > 0) {
				buf[num_read] = '\0';
				write_sync_fd(sync_pipe_fd, -1, buf);
			}
		}
		nexitf("Failed to create container: exit status %d", get_exit_status(runtime_status));
	}

	if (opt_terminal && masterfd_stdout == -1)
		nexit("Runtime did not set up terminal");

	/* Read the pid so we can wait for the process to exit */
	g_file_get_contents(opt_container_pid_file, &contents, NULL, &err);
	if (err) {
		nwarnf("Failed to read pidfile: %s", err->message);
		g_error_free(err);
		exit(1);
	}

	container_pid = atoi(contents);
	ndebugf("container PID: %d", container_pid);

	g_hash_table_insert(pid_to_handler, (pid_t *)&container_pid, container_exit_cb);

	/* Setup endpoint for attach */
	_cleanup_free_ char *attach_symlink_dir_path = NULL;
	if (!opt_exec) {
		attach_symlink_dir_path = setup_attach_socket();
	}

	if (!opt_exec) {
		setup_terminal_control_fifo();
	}

	/* Send the container pid back to parent */
	if (!opt_exec) {
		write_sync_fd(sync_pipe_fd, container_pid, NULL);
	}

	setup_oom_handling(container_pid);

	if (masterfd_stdout >= 0) {
		g_unix_fd_add(masterfd_stdout, G_IO_IN, stdio_cb, GINT_TO_POINTER(STDOUT_PIPE));
	}
	if (masterfd_stderr >= 0) {
		g_unix_fd_add(masterfd_stderr, G_IO_IN, stdio_cb, GINT_TO_POINTER(STDERR_PIPE));
	}

	if (opt_timeout > 0) {
		g_timeout_add_seconds(opt_timeout, timeout_cb, NULL);
	}

	check_child_processes(pid_to_handler);

	g_main_loop_run(main_loop);

	/* Drain stdout and stderr only if a timeout doesn't occur */
	if (masterfd_stdout != -1 && !timed_out) {
		g_unix_set_fd_nonblocking(masterfd_stdout, TRUE, NULL);
		while (read_stdio(masterfd_stdout, STDOUT_PIPE, NULL))
			;
	}
	if (masterfd_stderr != -1 && !timed_out) {
		g_unix_set_fd_nonblocking(masterfd_stderr, TRUE, NULL);
		while (read_stdio(masterfd_stderr, STDERR_PIPE, NULL))
			;
	}

	/* Sync the logs to disk */
	if (log_fd > 0) {
		if (fsync(log_fd) < 0) {
			pwarn("Failed to sync log file before exit");
		}
	}

	int exit_status = -1;
	const char *exit_message = NULL;

	if (timed_out && container_pid > 0) {
		pid_t process_group = getpgid(container_pid);
		/* if process_group is 1, we will end up calling
		 *  kill(-1), which kills everything conmon is allowed to. */
		if (process_group > 1)
			kill(-process_group, SIGKILL);
		else
			kill(container_pid, SIGKILL);
		exit_message = "command timed out";
	} else {
		exit_status = get_exit_status(container_status);
	}

	if (opt_exit_dir) {
		_cleanup_free_ char *status_str = g_strdup_printf("%d", exit_status);
		_cleanup_free_ char *exit_file_path = g_build_filename(opt_exit_dir, opt_cid, NULL);
		if (!g_file_set_contents(exit_file_path, status_str, -1, &err))
			nexitf("Failed to write %s to exit file: %s", status_str, err->message);
	}

	if (opt_exec) {
		/* Send the command exec exit code back to the parent */
		write_sync_fd(sync_pipe_fd, exit_status, exit_message);
	}

	if (attach_symlink_dir_path != NULL && unlink(attach_symlink_dir_path) == -1 && errno != ENOENT) {
		pexit("Failed to remove symlink for attach socket directory");
	}

	return EXIT_SUCCESS;
}
