package main

import (
	"errors"
	"strings"
	"testing"
)

func TestPortScore(t *testing.T) {
	tests := []struct {
		name string
		port string
		want int
	}{
		{name: "named Quectel port", port: "/dev/cu.Quectel-AT", want: 100},
		{name: "usb modem", port: "/dev/cu.usbmodem2101", want: 80},
		{name: "usb serial", port: "/dev/cu.usbserial-1420", want: 60},
		{name: "windows COM", port: "COM12", want: 50},
		{name: "bluetooth", port: "/dev/cu.Bluetooth-Incoming-Port", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := portScore(tt.port); got != tt.want {
				t.Fatalf("portScore(%q) = %d, want %d", tt.port, got, tt.want)
			}
		})
	}
}

func TestFilterCandidateATPorts(t *testing.T) {
	windows := filterCandidateATPorts([]string{"COM3", "COM12", "LPT1", ""}, "windows")
	if len(windows) != 2 || windows[0] != "COM3" || windows[1] != "COM12" {
		t.Fatalf("Windows ports = %v, want [COM3 COM12]", windows)
	}

	darwin := filterCandidateATPorts([]string{
		"/dev/cu.Bluetooth-Incoming-Port",
		"/dev/cu.usbmodem2101",
		"/dev/cu.Quectel-AT",
	}, "darwin")
	if len(darwin) != 2 || darwin[0] != "/dev/cu.usbmodem2101" || darwin[1] != "/dev/cu.Quectel-AT" {
		t.Fatalf("Darwin ports = %v", darwin)
	}
}

func TestParseUSBNetMode(t *testing.T) {
	for _, tt := range []struct {
		response string
		want     string
	}{
		{response: "AT+QCFG=\"usbnet\"\r\n+QCFG: \"usbnet\",0\r\nOK", want: "0"},
		{response: "+QCFG: \"usbnet\",1\r\nOK", want: "1"},
		{response: "ERROR", want: ""},
	} {
		if got := parseUSBNetMode(tt.response); got != tt.want {
			t.Fatalf("parseUSBNetMode(%q) = %q, want %q", tt.response, got, tt.want)
		}
	}
}

func TestParseUSBConfigPreservesEveryNonUACField(t *testing.T) {
	config, err := parseUSBConfig(`AT+QCFG="usbcfg"
+QCFG: "usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,1
OK`)
	if err != nil {
		t.Fatal(err)
	}
	if !config.uacEnabled() {
		t.Fatal("expected UAC enabled")
	}
	want := `AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,0`
	if got := config.withUAC(false); got != want {
		t.Fatalf("mobile command = %q, want %q", got, want)
	}
}

func TestParseUSBConfigRejectsUnknownLayout(t *testing.T) {
	for _, response := range []string{
		`+QCFG: "usbcfg",0x2C7C,0x0125,1,1,1`,
		`+QCFG: "usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,2`,
		`ERROR`,
	} {
		if _, err := parseUSBConfig(response); err == nil {
			t.Fatalf("parseUSBConfig(%q) unexpectedly succeeded", response)
		}
	}
}

func TestMobileProfileMessageKeepsADBFailureNonBlockingAndVisible(t *testing.T) {
	message := mobileProfileMessage(errors.New("ADB interface not found"), false)
	for _, want := range []string{"已保存 iPhone/iPad 模式", "直接拔出", "未提供 ADB", "网络保持可能受限"} {
		if !strings.Contains(message, want) {
			t.Fatalf("mobile profile warning %q missing %q", message, want)
		}
	}
	if message := mobileProfileMessage(nil, false); strings.Contains(message, "ADB") {
		t.Fatalf("successful network wake should not show an ADB warning: %q", message)
	}
}

func TestSignalRecoveryPausesWhileMobileProfileWaitsForReconnect(t *testing.T) {
	a := &app{usbProfileMobileArmed: true}
	if !a.signalRecoveryPaused() {
		t.Fatal("cellular recovery must pause while the mobile profile is armed")
	}
	a.usbProfileMobileArmed = false
	if a.signalRecoveryPaused() {
		t.Fatal("cellular recovery must resume after the Mac profile is restored")
	}
}

func TestParseMacNetworkServices(t *testing.T) {
	input := `An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) Baiwang 2
(Hardware Port: Baiwang, Device: en8)

(*) Baiwang
(Hardware Port: Baiwang, Device: en10)
`
	services := parseMacNetworkServices(input)
	if len(services) != 3 {
		t.Fatalf("service count = %d, want 3", len(services))
	}
	if services[1].Name != "Baiwang 2" || services[1].Device != "en8" ||
		services[1].Disabled || !isDJICellularService(services[1]) {
		t.Fatalf("active cellular service = %+v", services[1])
	}
	if !services[2].Disabled || !isDJICellularService(services[2]) {
		t.Fatalf("disabled cellular service = %+v", services[2])
	}
}

func TestIsDJICellularServiceRelaxed(t *testing.T) {
	tests := []struct {
		name    string
		service macNetworkService
		want    bool
	}{
		{
			name:    "hardware port Baiwang",
			service: macNetworkService{Name: "Baiwang 2", HardwarePort: "Baiwang", Device: "en8"},
			want:    true,
		},
		{
			name:    "service name contains baiwang",
			service: macNetworkService{Name: "Baiwang", HardwarePort: "USB LAN", Device: "en6"},
			want:    true,
		},
		{
			name:    "mixed case",
			service: macNetworkService{Name: "BAIWANG", HardwarePort: "baiwang", Device: "en10"},
			want:    true,
		},
		{
			name:    "module product service",
			service: macNetworkService{Name: "EG25G-QDC507", HardwarePort: "EG25G-QDC507", Device: "en8"},
			want:    true,
		},
		{
			name:    "not a cellular service",
			service: macNetworkService{Name: "Wi-Fi", HardwarePort: "Wi-Fi", Device: "en0"},
			want:    false,
		},
		{
			name:    "baiwang without en device",
			service: macNetworkService{Name: "Baiwang", HardwarePort: "Baiwang", Device: "bridge0"},
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDJICellularService(tt.service); got != tt.want {
				t.Fatalf("isDJICellularService(%+v) = %v, want %v", tt.service, got, tt.want)
			}
		})
	}
}

func TestParseHiddenMacNetworkServices(t *testing.T) {
	services, err := parseMacNetworkServicesPreferences([]byte(`{
  "NetworkServices": {
    "hidden-cellular": {
      "UserDefinedName": "Baiwang",
      "Interface": {
        "DeviceName": "en4",
        "Hardware": "Ethernet",
        "UserDefinedName": "Ethernet Adapter (en4)",
        "HiddenConfiguration": true
      }
    },
    "vpn": {
      "UserDefinedName": "Shadowrocket",
      "Interface": {"Type": "VPN"}
    }
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].Name != "Baiwang" || services[0].Device != "en4" {
		t.Fatalf("hidden services = %+v, want Baiwang on en4", services)
	}
}

func TestMergeMacNetworkServicesDeduplicatesVisibleEntries(t *testing.T) {
	visible := []macNetworkService{{Name: "Wi-Fi", Device: "en0"}, {Name: "Baiwang", Device: "en4"}}
	hidden := []macNetworkService{{Name: "Baiwang", Device: "en4"}, {Name: "DJI 4G", Device: "en4"}}
	merged := mergeMacNetworkServices(visible, hidden)
	if len(merged) != 3 {
		t.Fatalf("merged services = %+v, want three unique name/device pairs", merged)
	}
}

func TestIsLocallyAdministeredMAC(t *testing.T) {
	tests := []struct {
		mac  string
		want bool
	}{
		{mac: "3e:cc:eb:30:27:93", want: true},
		{mac: "02:00:00:00:00:01", want: true},
		{mac: "3e-cc-eb-30-27-93", want: true},
		{mac: "00:1a:2b:3c:4d:5e", want: false},
		{mac: "ac:de:48:00:11:22", want: false},
		{mac: "", want: false},
		{mac: "not-a-mac", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.mac, func(t *testing.T) {
			if got := isLocallyAdministeredMAC(tt.mac); got != tt.want {
				t.Fatalf("isLocallyAdministeredMAC(%q) = %v, want %v", tt.mac, got, tt.want)
			}
		})
	}
}

func TestSelectUnprovisionedUSBInterface(t *testing.T) {
	interfaces := []macNetInterface{
		{Name: "en0", Kind: "ethernet", MAC: "ac:de:48:00:11:22"},
		{Name: "en8", Kind: "ethernet", Status: "active", MAC: "3e:cc:eb:30:27:93"},
		{Name: "en10", Kind: "ethernet", Status: "active", MAC: "02:00:00:00:00:01"},
		{Name: "awdl0", Kind: "apple-wireless", MAC: "3e:00:00:00:00:01"},
	}
	services := []macNetworkService{
		{Name: "Baiwang 2", HardwarePort: "Baiwang", Device: "en8"},
	}
	if got := selectUnprovisionedUSBInterface(interfaces, services); got != "en10" {
		t.Fatalf("selectUnprovisionedUSBInterface = %q, want en10 (en8 has a service, en0 built-in)", got)
	}
	if got := selectUnprovisionedUSBInterface(nil, services); got != "" {
		t.Fatalf("selectUnprovisionedUSBInterface(nil) = %q, want empty", got)
	}
}

func TestSelectUnprovisionedUSBInterfaceIgnoresStaleHiddenService(t *testing.T) {
	interfaces := []macNetInterface{
		{Name: "en4", Kind: "ethernet", Status: "inactive", MAC: "66:a4:1c:1b:16:7b"},
		{Name: "en8", Kind: "ethernet", Status: "active", MAC: "ae:44:3d:9d:bb:90"},
	}
	services := []macNetworkService{{Name: "Baiwang", HardwarePort: "Ethernet Adapter (en4)", Device: "en4"}}
	if got := selectUnprovisionedUSBInterface(interfaces, services); got != "en8" {
		t.Fatalf("selectUnprovisionedUSBInterface = %q, want newly enumerated en8", got)
	}
}

func TestUSBTrafficInterfaceAcceptsGlobalIPv6Only(t *testing.T) {
	interfaces := []macNetInterface{
		{Name: "en0", Kind: "ethernet", Status: "active", IPv4: "192.168.1.2"},
		{Name: "en8", Kind: "ethernet", Status: "active", IPv4: "169.254.196.67", IPv6: "2409:8929:e47:ddd2::1"},
	}
	if got := selectUSBTrafficInterface(interfaces, macDefaultRoute{Interface: "en0"}); got != "en8" {
		t.Fatalf("selectUSBTrafficInterface = %q, want IPv6-capable en8", got)
	}
}

func TestParseMacIPv4ServiceInfo(t *testing.T) {
	info := parseMacIPv4ServiceInfo(`DHCP Configuration
IP address: 192.168.225.29
Subnet mask: 255.255.255.0
Router: 192.168.225.1
`)
	if info.Address != "192.168.225.29" || info.Subnet != "255.255.255.0" {
		t.Fatalf("IPv4 service info = %+v", info)
	}
}

func TestInitUSBATESIMManagerAfterDelayedUSBOpen(t *testing.T) {
	instance := &app{}

	manager, switchAllowed := instance.currentESIMManager()
	if manager != nil || switchAllowed {
		t.Fatalf("initial eSIM state = (%v, %v), want unavailable", manager, switchAllowed)
	}

	instance.initUSBATESIMManager()
	manager, switchAllowed = instance.currentESIMManager()
	if manager == nil {
		t.Fatal("USB AT recovery did not initialize the eSIM manager")
	}
	if !switchAllowed {
		t.Fatal("USB AT eSIM manager should allow profile switching")
	}

	instance.initUSBATESIMManager()
	managerAgain, _ := instance.currentESIMManager()
	if managerAgain != manager {
		t.Fatal("repeated USB AT recovery replaced the existing eSIM manager")
	}
}
