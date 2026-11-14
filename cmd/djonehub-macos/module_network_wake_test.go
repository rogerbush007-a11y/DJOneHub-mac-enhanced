//go:build darwin && cgo

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleNetworkWakeIsMobileScopedAndReleasesLock(t *testing.T) {
	checks := [][]byte{
		[]byte("network-wake.enabled"),
		[]byte("CONFIGURED"),
		[]byte("*,ecm,*"),
		[]byte("*,audio,*"),
		[]byte("wake_lock"),
		[]byte("wake_unlock"),
		[]byte("/proc/net/arp"),
		[]byte("1.1.1.1"),
		[]byte("/statistics/tx_bytes"),
	}
	for _, check := range checks {
		if !bytes.Contains(moduleNetworkWakeScript, check) {
			t.Fatalf("network wake script missing %q", check)
		}
	}
}

func TestModuleNetworkWakeFindsResolvedUSBHost(t *testing.T) {
	script, _, ok := strings.Cut(string(moduleNetworkWakeScript), "\ntrap cleanup EXIT")
	if !ok {
		t.Fatal("missing daemon entry point")
	}
	script = strings.ReplaceAll(script, "/proc/net/arp", `"$1"`) + "\nhost_address \"$1\"\n"
	for _, tt := range []struct{ name, arp, want string }{
		{"dhcp-host", "192.168.225.25 0x1 0x2 aa:bb:cc:dd:ee:ff * bridge0\n", "192.168.225.25"},
		{"ignore-unresolved-and-unrelated", "192.168.225.1 0x1 0x2 aa:bb:cc:dd:ee:ff * bridge0\n192.168.225.20 0x1 0x0 00:00:00:00:00:00 * bridge0\n192.168.225.21 0x1 0x2 aa:bb:cc:dd:ee:ff * eth0\n192.168.225.26 0x1 0x2 aa:bb:cc:dd:ee:ff * ecm0\n", "192.168.225.26"},
		{"no-host", "IP address HW type Flags HW address Mask Device\n", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "arp")
			if err := os.WriteFile(path, []byte(tt.arp), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("sh", "-c", script, "sh", path).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != tt.want {
				t.Fatalf("host = %q, error = %v, want %q", out, err, tt.want)
			}
		})
	}
}

func TestModuleNetworkWakeStopReleasesOrphanedLock(t *testing.T) {
	dir := t.TempDir()
	unlock := filepath.Join(dir, "wake_unlock")
	if err := os.WriteFile(unlock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(moduleNetworkWakeInitScript), "/var/run/djonehub_network_wake.pid", filepath.Join(dir, "missing.pid"))
	script = strings.ReplaceAll(script, "/sys/power/wake_unlock", unlock)
	if out, err := exec.Command("sh", "-c", script, "sh", "stop").CombinedOutput(); err != nil {
		t.Fatalf("stop: %v: %s", err, out)
	}
	data, err := os.ReadFile(unlock)
	if err != nil || strings.TrimSpace(string(data)) != "djonehub_network_wake" {
		t.Fatalf("wake lock not released: %q, %v", data, err)
	}
}

func TestModuleNetworkWakeRecoversOnlyStalledResume(t *testing.T) {
	script, _, ok := strings.Cut(string(moduleNetworkWakeScript), "\ntrap cleanup EXIT")
	if !ok {
		t.Fatal("missing daemon entry point")
	}
	for _, tt := range []struct{ name, commands string }{
		{"awake-idle", "check_resume 10 0 100 200; check_resume 500 0 100 200; test $soft = 0; test $hard = 0"},
		{"asleep", "check_resume 10 1 100 200; check_resume 500 1 100 200; test $soft = 0; test $hard = 0"},
		{"grace-and-soft-success", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 34 0 100 200; test $soft = 0; check_resume 35 0 100 200; test $soft = 1; check_resume 40 0 101 201; check_resume 500 0 101 201; test $hard = 0; test $recovery_attempts = 0"},
		{"both-directions-cancel", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 30 0 101 200; check_resume 32 0 101 201; check_resume 500 0 101 201; test $soft = 0; test $hard = 0"},
		{"rx-only-does-not-cancel", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 30 0 101 200; check_resume 35 0 102 200; test $soft = 1; check_resume 45 0 103 200; check_resume 50 0 104 200; test $hard = 1; test $recovery_attempts = 2"},
		{"tx-only-does-not-cancel", "check_resume 10 1 100 200; check_resume 20 0 100 201; check_resume 35 0 100 202; test $soft = 1; check_resume 50 0 100 203; test $hard = 1"},
		{"short-background-wake", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 30 1 100 200; check_resume 500 1 100 200; test $soft = 0"},
		{"cooldown-and-budget", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 35 0 100 200; check_resume 50 0 101 200; check_resume 349 0 102 200; test $hard = 1; check_resume 350 0 103 200; test $hard = 2; check_resume 1000 0 104 200; test $hard = 2; test $recovery_attempts = 3"},
		{"counter-reset-after-usb-rebind", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 35 0 100 200; check_resume 50 0 101 200; check_resume 55 0 1 2; test $recovery_attempts = 2; check_resume 60 0 3 4; test $recovery_attempts = 0; test $saw_suspend = 0"},
		{"recorded-ipad-half-recovery", "check_resume 209 1 2965 3781439; check_resume 330 0 2965 3781439; check_resume 345 0 2965 3781439; test $soft = 1; check_resume 360 0 3008 3781439; test $hard = 1"},
		{"missing-state", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 35 '' 100 200; check_resume 100 0 100 200; test $soft = 0"},
		{"bad-counter", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 35 0 100 unavailable; check_resume 100 0 100 200; test $soft = 0"},
		{"disconnect-after-soft-clears-session", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 35 0 100 200; check_connection 40 0; check_connection 60 1; check_resume 60 0 2 3; test $soft = 1; test $hard = 0; test $recovery_attempts = 0"},
		{"own-rebind-preserves-budget", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 35 0 100 200; check_resume 50 0 100 200; check_connection 53 0; test $recovery_attempts = 2; check_connection 55 1; check_resume 55 0 1 2; check_resume 60 0 2 3; test $hard = 1; test $recovery_attempts = 0"},
		{"rebind-timeout-before-new-host", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 35 0 100 200; check_resume 50 0 100 200; check_connection 55 0; check_connection 400 1; check_resume 400 0 1 2; test $hard = 1; test $saw_suspend = 0; test $recovery_attempts = 0"},
		{"rebind-timeout-while-disconnected", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 35 0 100 200; check_resume 50 0 100 200; check_connection 70 0; test $recovery_attempts = 0; test $saw_suspend = 0"},
		{"mode-change-cancels-rebind", "check_resume 10 1 100 200; check_resume 20 0 100 200; check_resume 35 0 100 200; check_resume 50 0 100 200; mobile_usb_profile() { return 1; }; check_connection 53 0; test $saw_suspend = 0; test $recovery_attempts = 0"},
		{"no-initial-traffic", "check_resume 10 1 0 0; check_resume 20 0 0 0; check_resume 500 0 0 0; test $soft = 0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := script + "\nset -e\nsoft=0\nhard=0\nmobile_usb_profile() { :; }\nsync() { :; }\nrestart_ecm() { soft=$((soft + 1)); }\nrestart_usb() { hard=$((hard + 1)); }\n" + tt.commands
			if out, err := exec.Command("sh", "-c", body).CombinedOutput(); err != nil {
				t.Fatalf("resume handling: %v: %s", err, out)
			}
		})
	}
}

func TestModuleNetworkWakeRestoresECMOnFailure(t *testing.T) {
	script, _, ok := strings.Cut(string(moduleNetworkWakeScript), "\ntrap cleanup EXIT")
	if !ok {
		t.Fatal("missing daemon entry point")
	}
	for _, fail := range []string{"", "down", "up"} {
		t.Run("fail-"+fail, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "actions")
			body := script + `
ifconfig() { echo "$*" >>"$ACTION_LOG"; test "$2" != "$FAIL"; }
`
			body += "\nsleep() { :; }\nrestart_ecm\n"
			cmd := exec.Command("sh", "-c", body)
			cmd.Env = append(os.Environ(), "ACTION_LOG="+log, "FAIL="+fail)
			out, err := cmd.CombinedOutput()
			if (err != nil) != (fail != "") {
				t.Fatalf("unexpected result: %v: %s", err, out)
			}
			data, err := os.ReadFile(log)
			if err != nil || !strings.HasPrefix(string(data), "ecm0 down\n") || !strings.HasSuffix(string(data), "ecm0 up\n") {
				t.Fatalf("link was not restored: %q, %v", data, err)
			}
		})
	}
}

func TestModuleNetworkWakeInitHasLifecycleCommands(t *testing.T) {
	for _, check := range [][]byte{[]byte("start)"), []byte("stop)"), []byte("restart)"), []byte("status)")} {
		if !bytes.Contains(moduleNetworkWakeInitScript, check) {
			t.Fatalf("network wake init missing %q", check)
		}
	}
}

func TestModuleNetworkWakeUSBReenumerationRestoresEnable(t *testing.T) {
	script, _, ok := strings.Cut(string(moduleNetworkWakeScript), "\ntrap cleanup EXIT")
	if !ok {
		t.Fatal("missing daemon entry point")
	}
	for _, initial := range []string{"1", "0"} {
		t.Run(initial, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "enable")
			if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			body := script + `
USB_ENABLE="$1"
sleep() { test "$(cat "$USB_ENABLE")" = 0 || exit 1; echo down-observed; }
restart_usb
`
			out, err := exec.Command("sh", "-c", body, "sh", path).CombinedOutput()
			if (err != nil) != (initial == "0") {
				t.Fatalf("unexpected result %v: %s", err, out)
			}
			data, err := os.ReadFile(path)
			if err != nil || strings.TrimSpace(string(data)) != initial {
				t.Fatalf("USB not restored: %q %v", data, err)
			}
			if initial == "1" && !strings.Contains(string(out), "down-observed") {
				t.Fatal("USB was not toggled")
			}
		})
	}
}

func TestModuleNetworkWakeStopToleratesMissingKernelLock(t *testing.T) {
	dir := t.TempDir()
	script := strings.ReplaceAll(string(moduleNetworkWakeInitScript), "/var/run/djonehub_network_wake.pid", filepath.Join(dir, "missing.pid"))
	// A writable directory makes the release write fail, as an absent kernel lock does.
	script = strings.ReplaceAll(script, "/sys/power/wake_unlock", dir)
	if out, err := exec.Command("sh", "-c", script, "sh", "stop").CombinedOutput(); err != nil {
		t.Fatalf("idempotent stop: %v: %s", err, out)
	}
}

func TestModuleNetworkWakeInstallRestoresMountAndReplacesFiles(t *testing.T) {
	for _, tt := range []struct{ name, options, failure, mounts string }{
		{"readonly", "ro,relatime", "", "remount,rw\nremount,ro\n"},
		{"writable", "rw,relatime", "", ""},
		{"copy-fails", "ro,relatime", "copy", "remount,rw\nremount,ro\n"},
		{"hangup", "ro,relatime", "hangup", "remount,rw\nremount,ro\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"data/local/tmp", "data/djonehub/bin", "etc/init.d", "etc/rc5.d"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(name, data string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("data/local/tmp/djonehub-network-wake.new", "new daemon\n")
			write("data/local/tmp/djonehub_network_wake.new", "#!/bin/sh\nexit 0\n")
			write("data/djonehub/bin/djonehub-network-wake", "old daemon\n")
			write("mounts", "rootfs / rootfs rw 0 0\nubi0:rootfs / ubifs "+tt.options+" 0 0\n")
			original := filepath.Join(dir, "original-inode")
			if err := os.Link(filepath.Join(dir, "data/djonehub/bin/djonehub-network-wake"), original); err != nil {
				t.Fatal(err)
			}
			command := strings.NewReplacer("/data/", dir+"/data/", "/etc/", dir+"/etc/", "/proc/mounts", dir+"/mounts").Replace(moduleNetworkWakeInstallCommand())
			prefix := `
mount() { echo "$2" >>"$MOUNT_LOG"; }
sync() { :; }
cp() {
    case "$*" in
        *djonehub_network_wake.new*)
            case "$FAIL" in copy) return 1;; hangup) kill -HUP $$;; esac;;
    esac
    command cp "$@"
}
`
			cmd := exec.Command("sh", "-c", prefix+command)
			cmd.Env = append(os.Environ(), "FAIL="+tt.failure, "MOUNT_LOG="+filepath.Join(dir, "mount-log"))
			out, err := cmd.CombinedOutput()
			if (err != nil) != (tt.failure != "") {
				t.Fatalf("install: %v: %s", err, out)
			}
			mounts, err := os.ReadFile(filepath.Join(dir, "mount-log"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if string(mounts) != tt.mounts {
				t.Fatalf("mounts = %q, want %q", mounts, tt.mounts)
			}
			old, err := os.ReadFile(original)
			if err != nil || string(old) != "old daemon\n" {
				t.Fatalf("running inode changed: %q, %v", old, err)
			}
			want := "new daemon\n"
			if tt.failure != "" {
				want = "old daemon\n"
			}
			data, err := os.ReadFile(filepath.Join(dir, "data/djonehub/bin/djonehub-network-wake"))
			if err != nil || string(data) != want {
				t.Fatalf("installed daemon: %q, want %q, error %v", data, want, err)
			}
		})
	}
}
