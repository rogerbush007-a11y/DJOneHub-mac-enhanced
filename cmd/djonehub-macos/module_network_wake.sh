#!/bin/sh

# Keep the QDC507 data path awake while an iPhone/iPad is attached. No mobile
# companion app or private Agent is required.
set -u

NAME=djonehub_network_wake
ENABLED=/data/djonehub/network-wake.enabled
PIDFILE=/var/run/djonehub_network_wake.pid
USB_STATE=/sys/class/android_usb/android0/state
USB_FUNCTIONS=/sys/class/android_usb/android0/functions
WAKE_LOCK=/sys/power/wake_lock
WAKE_UNLOCK=/sys/power/wake_unlock
USB_ENABLE=/sys/class/android_usb/android0/enable
locked=0
last_rx=
last_tx=
rx_progress=0
tx_progress=0
saw_suspend=0
resume_since=0
recovery_after=0
recovery_attempts=0
rebind_until=0

release_lock() {
    if test "$locked" = 1 && test -w "$WAKE_UNLOCK"; then
        echo "$NAME" >"$WAKE_UNLOCK" 2>/dev/null || true
    fi
    locked=0
}

cleanup() {
    release_lock
    rm -f "$PIDFILE"
}

mobile_usb_profile() {
    test -f "$ENABLED" || return 1
    functions=$(cat "$USB_FUNCTIONS" 2>/dev/null || true)
    case ",$functions," in *,ecm,*) ;; *) return 1;; esac
    # v1.2.9 mobile mode removes USB Audio but may retain serial/ADB.
    case ",$functions," in *,audio,*) return 1;; esac
    return 0
}

mobile_usb_active() {
    test "$(cat "$USB_STATE" 2>/dev/null)" = CONFIGURED && mobile_usb_profile
}

reset_resume() {
    last_rx=
    last_tx=
    rx_progress=0
    tx_progress=0
    saw_suspend=0
    resume_since=0
    recovery_after=0
    recovery_attempts=0
    rebind_until=0
}

check_connection() {
    if test "$rebind_until" != 0 &&
       { test "$1" -ge "$rebind_until" || ! mobile_usb_profile; }; then
        reset_resume
    fi
    if test "$2" = 1; then
        rebind_until=0
    elif test "$rebind_until" = 0; then
        reset_resume
    else
        last_rx=
        last_tx=
        rx_progress=0
        tx_progress=0
    fi
}

acquire_lock() {
    test "$locked" = 1 && return 0
    if test -w "$WAKE_LOCK"; then
        echo "$NAME" >"$WAKE_LOCK" 2>/dev/null && locked=1
    fi
}

host_address() {
    # QDC507's BusyBox ip lacks neigh; use the kernel's resolved USB neighbors.
    awk '$1 ~ /^192\.168\.225\./ && $1 != "192.168.225.1" && $3 == "0x2" &&
         $6 ~ /^(ecm0|bridge0|usb0|rndis0)$/ { print $1; exit }' /proc/net/arp
}

restart_ecm() (
    # A separate shell restores the link even if the service is stopped mid-reset.
    trap 'ifconfig ecm0 up >/dev/null 2>&1' EXIT
    trap 'exit 1' INT TERM HUP
    ifconfig ecm0 down || exit 1
    sleep 1
    ifconfig ecm0 up || exit 1
    trap - EXIT
)

restart_usb() (
    test -w "$USB_ENABLE" && test "$(cat "$USB_ENABLE")" = 1 || exit 1
    # The detached service's child restores USB even if its parent is stopped.
    trap 'echo 1 >"$USB_ENABLE"' EXIT
    trap 'exit 1' INT TERM HUP
    echo 0 >"$USB_ENABLE" || exit 1
    sleep 2
    echo 1 >"$USB_ENABLE" || exit 1
    trap - EXIT
)

check_resume() {
    now=$1
    usb_sleep=$2
    rx=$3
    tx=$4
    for value in "$now" "$rx" "$tx"; do
        case "$value" in
            ''|*[!0-9]*) saw_suspend=0; resume_since=0; last_rx=; last_tx=; return;;
        esac
    done
    case "$usb_sleep" in 0|1) ;; *) saw_suspend=0; resume_since=0; return;; esac
    if test "$usb_sleep" = 1; then
        if test "$resume_since" != 0; then
            echo "ECM_RESUME_CANCEL uptime=$now reason=usb-suspend rx_packets=$rx tx_bytes=$tx"
            sync
        fi
        saw_suspend=1
        resume_since=0
        rx_progress=0
        tx_progress=0
    elif test "$saw_suspend" = 1; then
        if { test -n "$last_rx" && test "$rx" -lt "$last_rx"; } ||
           { test -n "$last_tx" && test "$tx" -lt "$last_tx"; }; then
            rx_progress=0
            tx_progress=0
        else
            if test -n "$last_rx" && test "$rx" -gt "$last_rx"; then rx_progress=1; fi
            if test -n "$last_tx" && test "$tx" -gt "$last_tx"; then tx_progress=1; fi
        fi
    fi
    last_rx=$rx
    last_tx=$tx
    test "$usb_sleep" = 0 && test "$saw_suspend" = 1 || return 0
    if test "$rx_progress" = 1 && test "$tx_progress" = 1; then
        echo "ECM_RECOVERY_CONFIRMED uptime=$now attempts=$recovery_attempts rx_packets=$rx tx_bytes=$tx"
        sync
        recovery_attempts=0
        recovery_after=0
        saw_suspend=0
        resume_since=0
        rx_progress=0
        tx_progress=0
        return
    fi
    test "$rx" -gt 0 || test "$tx" -gt 0 || return 0
    if test "$resume_since" = 0; then
        resume_since=$now
        echo "ECM_RESUME_WAIT uptime=$now rx_packets=$rx tx_bytes=$tx grace=15 cooldown_until=$recovery_after attempts=$recovery_attempts"
        sync
    fi
    # shortcut: USB resume does not identify screen unlock; use host evidence if needed.
    test "$((now - resume_since))" -ge 15 && test "$now" -ge "$recovery_after" &&
        test "$recovery_attempts" -lt 3 || return 0
    recovery_attempts=$((recovery_attempts + 1))
    if test "$recovery_attempts" = 1; then
        action=ecm-link
        recovery_after=$((now + 15))
    else
        action=usb-reenumerate
        recovery_after=$((now + 300))
    fi
    echo "ECM_RECOVERY_BEGIN uptime=$now attempt=$recovery_attempts action=$action rx_packets=$rx tx_bytes=$tx"
    sync
    if test "$action" = ecm-link; then
        if restart_ecm; then result=action-complete; else result=failed; fi
    else
        rebind_until=0
        if restart_usb; then
            result=action-complete
            # shortcut: allow 20s for our own re-enumeration; a host identity would distinguish rapid unplugging.
            rebind_until=$((now + 20))
        else
            result=failed
        fi
    fi
    echo "ECM_RECOVERY uptime=$now attempt=$recovery_attempts action=$action result=$result awaiting=rx-and-tx"
    sync
    resume_since=$now
    rx_progress=0
    tx_progress=0
}

trap cleanup EXIT
trap 'exit 0' INT TERM HUP
echo $$ >"$PIDFILE"

while :; do
    now=$(cut -d. -f1 /proc/uptime)
    if mobile_usb_active; then
        check_connection "$now" 1
        acquire_lock
        # Aborted USB requests also count as tx_packets; tx_bytes counts successful data.
        check_resume "$now" \
            "$(cat /sys/class/android_usb/android0/usb_sleep 2>/dev/null)" \
            "$(cat /sys/class/net/ecm0/statistics/rx_packets 2>/dev/null)" \
            "$(cat /sys/class/net/ecm0/statistics/tx_bytes 2>/dev/null)"
        host=$(host_address)
        test -z "$host" || busybox ping -c 1 -W 2 "$host" >/dev/null 2>&1 || true
        busybox ping -c 1 -W 2 1.1.1.1 >/dev/null 2>&1 || true
        if test "$resume_since" != 0; then sleep 3; else sleep 15; fi
    else
        release_lock
        check_connection "$now" 0
        sleep 3
    fi
done
