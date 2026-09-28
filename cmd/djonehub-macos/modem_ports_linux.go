//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/modem"
)

// The module exposes one option-driver tty per serial USB interface. The
// numbering of /dev/ttyUSB* depends on probe order, so ports are located by
// USB identity and interface number through sysfs rather than by name.
//
// ModemManager keeps the QMI control port and the AT interface (MI_02) for
// data. DJOneHub takes the second AT interface (MI_03) and the NMEA interface
// (MI_01), which Voice over USB streams PCM on; the udev rule installed with
// the Linux package tells ModemManager to leave those two alone.
const (
	moduleInterfaceNMEA  = 1
	moduleInterfaceModem = 3
)

var moduleUSBIDs = [][2]int{
	{quectelUSBVendorID, quectelUSBProductID},
	{djiUSBVendorID, djiUSBProductID},
}

func readSysfsHex(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 16, 32)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// moduleTTY returns /dev/ttyUSBn for one USB interface of a supported module.
func moduleTTY(iface int) (string, error) {
	matches, _ := filepath.Glob("/sys/class/tty/ttyUSB*")
	for _, entry := range matches {
		// device points at the tty's node inside the USB interface directory.
		ttyDir, err := filepath.EvalSymlinks(filepath.Join(entry, "device"))
		if err != nil {
			continue
		}
		ifaceDir := filepath.Dir(ttyDir)
		number, ok := readSysfsHex(filepath.Join(ifaceDir, "bInterfaceNumber"))
		if !ok || number != iface {
			continue
		}
		usbDir := filepath.Dir(ifaceDir)
		vendorID, okV := readSysfsHex(filepath.Join(usbDir, "idVendor"))
		productID, okP := readSysfsHex(filepath.Join(usbDir, "idProduct"))
		if !okV || !okP {
			continue
		}
		for _, id := range moduleUSBIDs {
			if vendorID == id[0] && productID == id[1] {
				return "/dev/" + filepath.Base(entry), nil
			}
		}
	}
	return "", fmt.Errorf("没有找到模块接口 MI_%02d 对应的 /dev/ttyUSB 端口", iface)
}

// modulePCMPort locates the port Voice over USB streams PCM on.
func modulePCMPort() (string, error) {
	return moduleTTY(moduleInterfaceNMEA)
}

// platformPreferredATPort returns the AT port DJOneHub owns on Linux.
func platformPreferredATPort() (string, error) {
	return moduleTTY(moduleInterfaceModem)
}

// startPlatformWatchdog exits once the AT manager has been unusable for a
// while. A module reset (AT+CFUN=1,1, USB re-plug) re-creates the tty and the
// manager does not reopen it on its own; the service manager restarts the
// process, which rediscovers the port.
func startPlatformWatchdog(manager *modem.Manager, port string) {
	go func() {
		const limit = 4
		failures := 0
		for range time.Tick(5 * time.Second) {
			_, statErr := os.Stat(port)
			if statErr == nil && manager.CanExecuteAT() {
				failures = 0
				continue
			}
			failures++
			if failures >= limit {
				log.Printf("AT port %s lost (stat: %v); exiting so the service restarts", port, statErr)
				os.Exit(1)
			}
		}
	}()
}
