//go:build windows

package main

import (
	"fmt"
	"strings"

	"go.bug.st/serial"
	"golang.org/x/sys/windows/registry"
)

// The module exposes several USB interfaces; Windows gives each serial one its
// own COM port. Voice over USB streams PCM on the NMEA interface while AT stays
// on its own, so the voice path has to find a sibling port of the AT port
// rather than assume a number.
const (
	moduleInterfaceDM    = 0
	moduleInterfaceNMEA  = 1
	moduleInterfaceAT    = 2
	moduleInterfaceModem = 3
)

// moduleCOMPort returns the COM port Windows assigned to one USB interface of a
// module, for example interface 1 (NMEA) of 2C7C:0125.
//
// Candidates come from the device enumeration registry, then are intersected
// with the ports that actually exist right now: a machine accumulates stale
// enumeration keys for every USB port a module was ever plugged into, and those
// keys keep a PortName that no longer resolves to anything.
func moduleCOMPort(vendorID, productID, iface int) (string, error) {
	key := fmt.Sprintf(`SYSTEM\CurrentControlSet\Enum\USB\VID_%04X&PID_%04X&MI_%02d`,
		vendorID, productID, iface)
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.READ)
	if err != nil {
		return "", fmt.Errorf("模块接口 MI_%02d 未在系统中登记: %w", iface, err)
	}
	defer root.Close()

	instances, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return "", err
	}

	present := map[string]bool{}
	if ports, err := serial.GetPortsList(); err == nil {
		for _, p := range ports {
			present[strings.ToUpper(p)] = true
		}
	}

	var stale []string
	for _, instance := range instances {
		params, err := registry.OpenKey(registry.LOCAL_MACHINE,
			key+`\`+instance+`\Device Parameters`, registry.READ)
		if err != nil {
			continue
		}
		name, _, err := params.GetStringValue("PortName")
		params.Close()
		if err != nil || name == "" {
			continue
		}
		if present[strings.ToUpper(name)] {
			return name, nil
		}
		stale = append(stale, name)
	}
	if len(stale) > 0 {
		return "", fmt.Errorf("模块接口 MI_%02d 只找到已失效的端口记录 (%s)；请重新插拔模块",
			iface, strings.Join(stale, ", "))
	}
	return "", fmt.Errorf("模块接口 MI_%02d 没有对应的 COM 端口", iface)
}

// modulePCMPort locates the port Voice over USB streams PCM on, trying both USB
// identities the supported modules enumerate under.
func modulePCMPort() (string, error) {
	var last error
	for _, id := range [][2]int{
		{quectelUSBVendorID, quectelUSBProductID},
		{djiUSBVendorID, djiUSBProductID},
	} {
		port, err := moduleCOMPort(id[0], id[1], moduleInterfaceNMEA)
		if err == nil {
			return port, nil
		}
		last = err
	}
	return "", last
}
