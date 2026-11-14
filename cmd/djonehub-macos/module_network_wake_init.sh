#!/bin/sh

DAEMON=/data/djonehub/bin/djonehub-network-wake
PIDFILE=/var/run/djonehub_network_wake.pid
LOGFILE=/data/djonehub/log/network-wake.log

owned_pid() {
    test -s "$PIDFILE" || return 1
    pid=$(cat "$PIDFILE" 2>/dev/null)
    case "$pid" in ''|*[!0-9]*) return 1;; esac
    test -d "/proc/$pid" || return 1
    tr '\000' ' ' <"/proc/$pid/cmdline" 2>/dev/null | grep -q 'djonehub-network-wake'
}

case "${1:-}" in
start)
    owned_pid && exit 0
    rm -f "$PIDFILE"
    mkdir -p /data/djonehub/log
    nohup setsid "$DAEMON" </dev/null >>"$LOGFILE" 2>&1 &
    sleep 1
    owned_pid
    ;;
stop)
    if owned_pid; then
        pid=$(cat "$PIDFILE")
        kill -TERM "$pid" 2>/dev/null || true
        n=0
        while test -d "/proc/$pid" -a "$n" -lt 30; do sleep 0.1; n=$((n + 1)); done
        test -d "/proc/$pid" && kill -KILL "$pid" 2>/dev/null || true
    fi
    rm -f "$PIDFILE"
    # SIGKILL while the shell waits for ping/sleep bypasses its EXIT cleanup.
    if test -w /sys/power/wake_unlock; then
        echo djonehub_network_wake >/sys/power/wake_unlock 2>/dev/null || true
    fi
    ;;
restart)
    "$0" stop
    "$0" start
    ;;
status)
    owned_pid
    ;;
*)
    echo "usage: $0 {start|stop|restart|status}" >&2
    exit 64
    ;;
esac
