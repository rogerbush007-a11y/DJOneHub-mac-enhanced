package netcfg

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"syscall"
)

// WindowsConfigurator implements NetworkConfigurator for Windows using netsh
// WindowsConfigurator 使用 netsh 实现 Windows 的 NetworkConfigurator
type WindowsConfigurator struct{}

func NewWindowsConfigurator() *WindowsConfigurator {
	return &WindowsConfigurator{}
}

func (w *WindowsConfigurator) run(args ...string) error {
	cmd := exec.Command("netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("command failed: %s, output: %s", err, string(out))
	}
	return nil
}

func (w *WindowsConfigurator) SetIPAddress(ifname string, ip net.IP, prefixLen int) error {
	// netsh interface ip set address "ifname" static IP Mask Gateway
	mask := net.IP(net.CIDRMask(prefixLen, 32)).String()
	// Note: We don't set gateway here, it's done in AddDefaultRoute
	// 注意: 我们不在这里设置网关，它在 AddDefaultRoute 中完成
	return w.run("interface", "ip", "set", "address", fmt.Sprintf("\"%s\"", ifname), "static", ip.String(), mask)
}

func (w *WindowsConfigurator) SetIPv6Address(ifname string, ip net.IP, prefixLen int) error {
	// netsh interface ipv6 add address "ifname" IP/Prefix
	return w.run("interface", "ipv6", "add", "address", fmt.Sprintf("\"%s\"", ifname), fmt.Sprintf("%s/%d", ip.String(), prefixLen))
}

func (w *WindowsConfigurator) FlushAddresses(ifname string) error {
	// Reset to DHCP is the easiest way to clear static IPs on Windows
	// 在 Windows 上清除静态 IP 最简单的方法是重置为 DHCP
	_ = w.run("interface", "ip", "set", "address", fmt.Sprintf("\"%s\"", ifname), "dhcp")
	_ = w.run("interface", "ipv6", "set", "address", fmt.Sprintf("\"%s\"", ifname), "dhcp")
	return nil
}

func (w *WindowsConfigurator) AddDefaultRoute(ifname string, gateway net.IP) error {
	// Gateway is usually set with IP address in Windows, but can be added separately
	// Windows 中网关通常与 IP 地址一起设置，但也可以单独添加
	if gateway.To4() != nil {
		return w.run("interface", "ip", "add", "address", fmt.Sprintf("\"%s\"", ifname), "gateway="+gateway.String(), "gwmetric=1")
	}
	// IPv6 route
	return w.run("interface", "ipv6", "add", "route", "::/0", fmt.Sprintf("\"%s\"", ifname), "nexthop="+gateway.String())
}

func (w *WindowsConfigurator) AddDefaultRouteDirect(ifname string, ipv6 bool) error {
	// Interface-based route
	if ipv6 {
		return w.run("interface", "ipv6", "add", "route", "::/0", fmt.Sprintf("\"%s\"", ifname))
	}
	return w.run("interface", "ip", "add", "route", "0.0.0.0/0", fmt.Sprintf("\"%s\"", ifname))
}

func (w *WindowsConfigurator) FlushRoutes(ifname string) error {
	// Removing IP address usually clears routes, but we can try explicit delete
	// 移除 IP 地址通常会清除路由，但我们可以尝试显式删除
	_ = w.run("interface", "ip", "delete", "route", "0.0.0.0/0", fmt.Sprintf("\"%s\"", ifname))
	_ = w.run("interface", "ipv6", "delete", "route", "::/0", fmt.Sprintf("\"%s\"", ifname))
	return nil
}

func (w *WindowsConfigurator) BringUp(ifname string) error {
	return w.run("interface", "set", "interface", fmt.Sprintf("\"%s\"", ifname), "admin=enable")
}

func (w *WindowsConfigurator) BringDown(ifname string) error {
	return w.run("interface", "set", "interface", fmt.Sprintf("\"%s\"", ifname), "admin=disable")
}

func (w *WindowsConfigurator) SetMTU(ifname string, mtu int) error {
	// netsh interface ipv4 set subinterface "ifname" mtu=1500 store=persistent
	if err := w.run("interface", "ipv4", "set", "subinterface", fmt.Sprintf("\"%s\"", ifname), "mtu="+strconv.Itoa(mtu), "store=persistent"); err != nil {
		return err
	}
	return w.run("interface", "ipv6", "set", "subinterface", fmt.Sprintf("\"%s\"", ifname), "mtu="+strconv.Itoa(mtu), "store=persistent")
}

func (w *WindowsConfigurator) GetCurrentIP(ifname string) (net.IP, error) {
	// Parsing `netsh interface ip show config "ifname"` is painful.
	// Using Go's standard library is better for reading.
	// 解析 `netsh interface ip show config "ifname"` 很痛苦。
	// 使用 Go 的标准库读取更好。
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP, nil
			}
		}
	}
	return nil, nil
}

func (w *WindowsConfigurator) IsUp(ifname string) (bool, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return false, err
	}
	return iface.Flags&net.FlagUp != 0, nil
}

func (w *WindowsConfigurator) UpdateDNS(dns1, dns2 string) error {
	// DNS is per-interface in Windows. We don't have ifname here, which is a design flaw in the interface for Windows.
	// Windows 中 DNS 是基于接口的。我们这里没有 ifname，这是接口设计在 Windows 上的缺陷。
	// We'll assume a global variable or modify interface later. For now, skipping.
	// 我们稍后将假设一个全局变量或修改接口。目前跳过。
	return fmt.Errorf("UpdateDNS not supported on Windows directly without interface name")
}

func (w *WindowsConfigurator) RestoreDNS() error {
	return nil
}

// QMAP 多路复用在 Windows 上不支持
func (w *WindowsConfigurator) AddQMAPMux(masterIface string, muxID uint8) (string, error) {
	return "", fmt.Errorf("QMAP 多路复用在 Windows 上不可用")
}
func (w *WindowsConfigurator) DelQMAPMux(masterIface string, muxID uint8) error       { return nil }
func (w *WindowsConfigurator) GetQMAPMuxIface(masterIface string, muxID uint8) string { return "" }
func (w *WindowsConfigurator) EnableRawIP(ifname string) error                        { return nil }
