package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/damonto/euicc-go/driver"
	"github.com/iniwex5/vohive/internal/backend"
	"github.com/iniwex5/vohive/internal/config"
	"github.com/iniwex5/vohive/internal/esim"
	"github.com/iniwex5/vohive/internal/modem"
	"github.com/iniwex5/vohive/pkg/smscodec"
	"go.bug.st/serial"
)

//go:embed web/*
var webAssets embed.FS

type receivedSMS struct {
	Sender    string    `json:"sender"`
	Content   string    `json:"content"`
	Code      string    `json:"code,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type profileNote struct {
	Label string `json:"label"`
	Phone string `json:"phone"`
	Tags  string `json:"tags"`
}

type phonebookProbeResult struct {
	StorageSupported bool              `json:"storage_supported"`
	StorageSelected  bool              `json:"storage_selected"`
	ReadSupported    bool              `json:"read_supported"`
	WriteSupported   bool              `json:"write_supported"`
	StorageStatus    string            `json:"storage_status"`
	Responses        map[string]string `json:"responses"`
}

type moduleProfileNote struct {
	Index int    `json:"index"`
	ICCID string `json:"iccid"`
	Label string `json:"label"`
	Phone string `json:"phone"`
	Tags  string `json:"tags"`
}

type modulePhonebookEntry struct {
	Index  int
	Number string
	Text   string
}

type app struct {
	modem             *modem.Manager
	esimMu            sync.RWMutex
	esim              *esim.Manager
	esimSwitchAllowed bool
	usbAT             *usbAT
	port              string
	demo              bool
	webConsole        bool
	discoveryError    string
	usbDevice         *usbDeviceStatus
	usbATBackoffUntil time.Time
	usbATBackoffErr   string

	cachedModemMu       sync.Mutex
	cachedModemFirmware string
	cachedModemICCID    string
	cachedModemIMSI     string
	cachedModemUSBNet   int

	smsMu          sync.RWMutex
	smsOperationMu sync.Mutex
	sms            []receivedSMS
	smsSendMu      sync.Mutex
	smsReassembler *smscodec.Reassembler

	smsPollInterval  time.Duration
	smsAutoCleanupME bool
	smsLastPoll      time.Time
	smsLastPollError string

	callMu             sync.RWMutex
	activeCall         *callRecord
	callHistory        []callRecord
	callPollInterval   time.Duration
	callLastPoll       time.Time
	callLastPollError  string
	callConfigured     bool
	lastAnswerAt       time.Time
	callNotifier       func(callRecord)
	audio              *audioRouter
	audioManualSet     bool
	audioManualOn      bool
	lastAudioHealthLog time.Time
	// The native notifier registers this before a call.  When present, the
	// backend owns only module control and call state; MaVo's Swift service owns
	// every host-audio callback and media loop.
	swiftAudioHost bool

	moduleVoiceMu     sync.Mutex
	moduleVoiceOpMu   sync.Mutex
	moduleVoiceReady  bool
	moduleVoiceLast   time.Time
	moduleVoiceErr    string
	moduleVoiceDetail string

	moduleSetupMu sync.RWMutex
	moduleSetup   moduleSetupStatus

	gpsMu          sync.RWMutex
	gpsEnabled     bool
	gpsLastFix     *gpsFix
	gpsLastChecked time.Time
	gpsLastError   string

	profileNotesMu     sync.Mutex
	profileNotes       map[string]profileNote
	profileNotesLoaded bool
	profileNotesPath   string

	moduleNotesMu sync.Mutex

	trafficMu        sync.Mutex
	trafficBaselines map[string]networkByteCounters

	networkPolicyMu     sync.Mutex
	networkPolicyLoaded bool
	force4GOff          bool
	disabled4GServices  []string
	networkPolicyPath   string

	networkRepairMu        sync.Mutex
	usbProfileMu           sync.Mutex
	usbProfileIntentLoaded bool
	usbProfileMobileArmed  bool
	usbProfileIntentPath   string

	usbATOpenMu      sync.Mutex
	recoveryMu       sync.Mutex
	lostSignalCount  int
	lastModemReboot  time.Time
	lastNetworkCheck time.Time
}

type usbInterfaceStatus struct {
	Number    int `json:"number"`
	Class     int `json:"class"`
	Subclass  int `json:"subclass"`
	Protocol  int `json:"protocol"`
	Endpoints int `json:"endpoints"`
}

type usbDeviceStatus struct {
	Product    string               `json:"product"`
	Vendor     string               `json:"vendor"`
	VendorID   string               `json:"vendor_id"`
	ProductID  string               `json:"product_id"`
	LocationID string               `json:"location_id"`
	Speed      string               `json:"speed"`
	Mode       string               `json:"mode"`
	Interfaces []usbInterfaceStatus `json:"interfaces"`
}

type networkDiagnostic struct {
	USBNetMode        string            `json:"usbnet_mode"`
	USBCfg            string            `json:"usbcfg"`
	PDPContexts       []pdpContext      `json:"pdp_contexts"`
	ActiveContexts    []int             `json:"active_contexts"`
	PDPAddresses      []string          `json:"pdp_addresses"`
	MacInterfaces     []macNetInterface `json:"mac_interfaces"`
	DefaultRoute      macDefaultRoute   `json:"default_route"`
	USBNetworkPresent bool              `json:"usb_network_present"`
	USBDevice         *usbDeviceStatus  `json:"usb_device,omitempty"`
	Raw               map[string]string `json:"raw,omitempty"`
	Errors            map[string]string `json:"errors,omitempty"`
}

type pdpContext struct {
	ID  int    `json:"id"`
	PDN string `json:"pdn"`
	APN string `json:"apn"`
}

type usbProfileStatus struct {
	Mode           string `json:"mode"`
	UACEnabled     bool   `json:"uac_enabled"`
	Configuration  string `json:"configuration"`
	NeedsReconnect bool   `json:"needs_reconnect"`
	Message        string `json:"message,omitempty"`
}

type usbConfig struct{ fields []string }

func (c usbConfig) uacEnabled() bool { return len(c.fields) == 9 && c.fields[8] == "1" }

func (c usbConfig) withUAC(enabled bool) string {
	fields := append([]string(nil), c.fields...)
	if enabled {
		fields[8] = "1"
	} else {
		fields[8] = "0"
	}
	return `AT+QCFG="usbcfg",` + strings.Join(fields, ",")
}

func parseUSBConfig(resp string) (usbConfig, error) {
	re := regexp.MustCompile(`(?im)^\s*\+QCFG:\s*"usbcfg"\s*,\s*([^\r\n]+)`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return usbConfig{}, errors.New("模块未返回可识别的 USBCFG 配置")
	}
	fields := strings.Split(match[1], ",")
	if len(fields) != 9 {
		return usbConfig{}, fmt.Errorf("USBCFG 功能位数量异常（%d），已拒绝写入", len(fields))
	}
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
		if fields[i] == "" {
			return usbConfig{}, errors.New("USBCFG 含空字段，已拒绝写入")
		}
	}
	if fields[8] != "0" && fields[8] != "1" {
		return usbConfig{}, fmt.Errorf("USBCFG UAC 位异常（%s），已拒绝写入", fields[8])
	}
	return usbConfig{fields: fields}, nil
}

type macNetInterface struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	IPv4   string `json:"ipv4"`
	MAC    string `json:"mac,omitempty"`
	Kind   string `json:"kind"`
}

type macDefaultRoute struct {
	Interface string `json:"interface"`
	Gateway   string `json:"gateway"`
}

type networkByteCounters struct {
	RX uint64
	TX uint64
}

type networkTrafficSnapshot struct {
	Available    bool   `json:"available"`
	Interface    string `json:"interface,omitempty"`
	RXBytes      uint64 `json:"rx_bytes"`
	TXBytes      uint64 `json:"tx_bytes"`
	SessionRX    uint64 `json:"session_rx_bytes"`
	SessionTX    uint64 `json:"session_tx_bytes"`
	SessionTotal uint64 `json:"session_total_bytes"`
	SampledAtMS  int64  `json:"sampled_at_ms"`
	Error        string `json:"error,omitempty"`
}

type networkCheckResult struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
}

type cellularPolicyStatus struct {
	ForceOff bool     `json:"force_off"`
	Services []string `json:"services"`
}

func main() {
	platformCleanup := initPlatformRuntime()
	defer platformCleanup()

	var port string
	var listen string
	var demo bool
	var webConsole bool
	flag.StringVar(&port, "port", "", "AT serial port; auto-detected when omitted")
	flag.StringVar(&listen, "listen", "127.0.0.1:7575", "HTTP listen address")
	flag.BoolVar(&demo, "demo", false, "run the web UI with simulated modem data")
	flag.BoolVar(&webConsole, "web-console", false, "serve the embedded compatibility console")
	flag.Parse()

	if demo {
		instance := newDemoApp()
		instance.webConsole = webConsole
		log.Printf("DJOneHub demo mode")
		serve(instance, listen)
		return
	}

	if strings.TrimSpace(port) == "" {
		var err error
		port, err = discoverATPort()
		if err != nil {
			usbDevice := discoverDJIUSBDevice()
			usbATDevice, usbATErr := openDJIUSBAT()
			instance := &app{
				port:             "未发现 AT 串口",
				discoveryError:   err.Error(),
				usbDevice:        usbDevice,
				usbAT:            usbATDevice,
				smsPollInterval:  8 * time.Second,
				smsAutoCleanupME: true,
				smsReassembler:   smscodec.NewReassembler(),
				callPollInterval: 3 * time.Second,
				audio:            newAudioRouter(),
				webConsole:       webConsole,
			}
			if usbDevice != nil {
				log.Printf("DJI USB device detected without AT serial port: %s %s (%s:%s)",
					usbDevice.Vendor, usbDevice.Product, usbDevice.VendorID, usbDevice.ProductID)
			}
			if usbATErr != nil {
				log.Printf("USB AT unavailable: %v", usbATErr)
			} else {
				instance.port = usbATDevice.Description()
				instance.discoveryError = ""
				defer usbATDevice.Close()
				log.Printf("USB AT bridge opened on DJI %s", usbATDevice.Description())
				if changed, profileErr := instance.autoEnableMacAudioProfile(); profileErr != nil {
					log.Printf("Mac audio profile check: %v", profileErr)
				} else if changed {
					instance.discoveryError = "正在恢复 Mac 完整模式，请等待模块重新连接"
				}
				if instance.usbAT != nil {
					instance.initUSBATESIMManager()
					// Network repair can wait through two DHCP attempts. Do not
					// hold the local API listener hostage during startup: the App
					// must be able to show module/setup progress while 4G settles.
					go instance.ensureCellularDHCP()
				}
			}
			log.Printf("modem discovery skipped: %v", err)
			go instance.startSMSPoller(context.Background())
			go instance.startCallPoller(context.Background())
			go instance.startGPSPoller(context.Background())
			go instance.startSignalRecovery(context.Background())
			go instance.syncGPSState()
			serve(instance, listen)
			return
		}
	}

	cfg := config.DeviceConfig{
		ID:            "mac-modem",
		Name:          "DJI 4G Module",
		ATPort:        port,
		ManagePort:    port,
		DeviceBackend: backend.BackendAT,
		ESIMTransport: "at",
		BaudRate:      115200,
		DataBits:      8,
		StopBits:      1,
		Parity:        "none",
		SMSEnabled:    true,
	}
	manager, err := modem.New(cfg)
	if err != nil {
		log.Fatalf("create modem manager: %v", err)
	}

	instance := &app{
		modem: manager, port: port,
		smsPollInterval: 8 * time.Second, smsAutoCleanupME: true,
		callPollInterval: 3 * time.Second,
		audio:            newAudioRouter(),
		webConsole:       webConsole,
	}
	manager.SetSMSCallback(instance.recordSMS)
	if err := manager.Start(); err != nil {
		log.Fatalf("open modem on %s: %v", port, err)
	}
	defer manager.Stop()

	if !manager.WaitReady(15 * time.Second) {
		log.Printf("modem initialization is still running; the web UI will remain available")
	}

	atBackend := backend.NewATBackend(manager)
	esimManager, err := esim.NewManager(esim.ManagerOptions{
		DeviceID:  "mac-modem",
		Transport: "at",
		Modem:     manager,
		Backend:   atBackend,
	})
	if err != nil {
		log.Printf("eSIM manager unavailable: %v", err)
	} else {
		instance.installESIMManager(esimManager, false)
	}

	go manager.CheckAllSMS()
	go instance.startCallPoller(context.Background())
	go instance.startGPSPoller(context.Background())
	go instance.syncGPSState()

	serve(instance, listen)
}

func (a *app) initUSBATESIMManager() {
	if manager, _ := a.currentESIMManager(); manager != nil {
		return
	}
	esimManager, err := esim.NewManager(esim.ManagerOptions{
		DeviceID:  "mac-usbat",
		Transport: "custom",
		SmartCardChannelFactory: func() (driver.SmartCardChannel, error) {
			return newUSBATESIMChannel(a.runATCommand), nil
		},
	})
	if err != nil {
		log.Printf("eSIM manager unavailable over USB AT: %v", err)
		return
	}
	if a.installESIMManager(esimManager, true) {
		log.Printf("eSIM manager is available over USB AT with profile switching enabled")
	}
}

func (a *app) currentESIMManager() (*esim.Manager, bool) {
	a.esimMu.RLock()
	defer a.esimMu.RUnlock()
	return a.esim, a.esimSwitchAllowed
}

func (a *app) installESIMManager(manager *esim.Manager, switchAllowed bool) bool {
	if manager == nil {
		return false
	}
	a.esimMu.Lock()
	defer a.esimMu.Unlock()
	if a.esim != nil {
		return false
	}
	a.esim = manager
	a.esimSwitchAllowed = switchAllowed
	return true
}

func serve(instance *app, listen string) {
	server := &http.Server{
		Addr:              listen,
		Handler:           instance.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !instance.demo {
		go instance.startCellularPolicyGuard(ctx)
	}

	if !instance.demo {
		log.Printf("DJOneHub is using %s", instance.port)
	}
	log.Printf("Open http://%s", listen)
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		if platformOpenExistingUI("http://" + listen) {
			return
		}
		log.Printf("HTTP listen failed: %v", err)
		return
	}
	openPlatformUI("http://" + listen)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP server stopped unexpectedly: %v", err)
		}
	case <-ctx.Done():
		log.Printf("DJOneHub is stopping")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown: %v", err)
		}
	}
}

func newDemoApp() *app {
	now := time.Now()
	return &app{
		demo:             true,
		port:             "Demo · Quectel EG25-G",
		smsPollInterval:  8 * time.Second,
		callPollInterval: 3 * time.Second,
		sms: []receivedSMS{
			{
				Sender:    "10086",
				Content:   "【DJOneHub 演示】本月套餐剩余流量 18.6GB。",
				Timestamp: now.Add(-18 * time.Minute),
			},
			{
				Sender:    "+44 7400 123456",
				Content:   "Your verification code is 482913. It expires in 10 minutes.",
				Code:      "482913",
				Timestamp: now.Add(-2 * time.Hour),
			},
		},
	}
}

func discoverATPort() (string, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return "", fmt.Errorf("list serial ports: %w", err)
	}
	ports = filterCandidateATPorts(ports, runtime.GOOS)

	sort.SliceStable(ports, func(i, j int) bool {
		return portScore(ports[i]) > portScore(ports[j])
	})
	var attempted []string
	for _, port := range ports {
		attempted = append(attempted, port)
		if _, err := modem.ProbeIMEICached(port, 2*time.Second); err == nil {
			return port, nil
		}
	}
	if len(attempted) == 0 {
		if runtime.GOOS == "windows" {
			return "", errors.New("no Windows COM ports found; install the module serial driver or pass -port COMx explicitly")
		}
		return "", errors.New("no Quectel/DJI USB serial ports found; pass -port /dev/cu.* explicitly")
	}
	return "", fmt.Errorf("no AT-capable port found among %s", strings.Join(attempted, ", "))
}

func filterCandidateATPorts(ports []string, goos string) []string {
	filtered := make([]string, 0, len(ports))
	for _, port := range ports {
		name := strings.ToLower(strings.TrimSpace(port))
		if name == "" {
			continue
		}
		if goos == "windows" {
			if strings.HasPrefix(name, "com") {
				filtered = append(filtered, port)
			}
			continue
		}
		if strings.Contains(name, "usbmodem") ||
			strings.Contains(name, "usbserial") ||
			strings.Contains(name, "wchusbserial") ||
			strings.Contains(name, "quectel") ||
			strings.Contains(name, "dji") {
			filtered = append(filtered, port)
		}
	}
	return filtered
}

func portScore(port string) int {
	name := strings.ToLower(port)
	if strings.Contains(name, "quectel") || strings.Contains(name, "dji") {
		return 100
	}
	if strings.Contains(name, "usbmodem") {
		return 80
	}
	if strings.Contains(name, "usbserial") {
		return 60
	}
	if strings.HasPrefix(name, "com") {
		return 50
	}
	return 0
}

var (
	cachedUSBDevice   *usbDeviceStatus
	cachedUSBDeviceAt time.Time
	cachedUSBMu       sync.Mutex
)

func discoverDJIUSBDevice() *usbDeviceStatus {
	cachedUSBMu.Lock()
	defer cachedUSBMu.Unlock()
	if time.Since(cachedUSBDeviceAt) < 1500*time.Millisecond {
		return cachedUSBDevice
	}
	cachedUSBDevice = scanDJIUSBDeviceInternal()
	cachedUSBDeviceAt = time.Now()
	return cachedUSBDevice
}

func scanDJIUSBDeviceInternal() *usbDeviceStatus {
	out, err := exec.Command("ioreg", "-r", "-c", "IOUSBHostInterface", "-l", "-w", "0").Output()
	if err != nil {
		return nil
	}

	var device *usbDeviceStatus
	for _, block := range strings.Split(string(out), "\n\n") {
		vendorID, okVendor := intProperty(block, "idVendor")
		productID, okProduct := intProperty(block, "idProduct")
		if !okVendor || !okProduct || !isSupportedUSBModuleIdentity(vendorID, productID) {
			continue
		}
		if device == nil {
			device = &usbDeviceStatus{
				Product:    stringProperty(block, "USB Product Name"),
				Vendor:     stringProperty(block, "USB Vendor Name"),
				VendorID:   fmt.Sprintf("%04x", vendorID),
				ProductID:  fmt.Sprintf("%04x", productID),
				LocationID: formatHexProperty(block, "locationID"),
				Speed:      usbSpeedName(intPropertyOrZero(block, "USBSpeed")),
				Mode:       "vendor-specific USB mode",
			}
			if strings.TrimSpace(device.Product) == "" {
				device.Product = "DJI 4G Module"
			}
			if strings.TrimSpace(device.Vendor) == "" {
				device.Vendor = "DJI"
			}
		}
		ifaceNumber, okIface := intProperty(block, "bInterfaceNumber")
		if !okIface {
			continue
		}
		iface := usbInterfaceStatus{
			Number:    ifaceNumber,
			Class:     intPropertyOrZero(block, "bInterfaceClass"),
			Subclass:  intPropertyOrZero(block, "bInterfaceSubClass"),
			Protocol:  intPropertyOrZero(block, "bInterfaceProtocol"),
			Endpoints: intPropertyOrZero(block, "bNumEndpoints"),
		}
		device.Interfaces = append(device.Interfaces, iface)
	}
	if device == nil {
		return nil
	}
	sort.SliceStable(device.Interfaces, func(i, j int) bool {
		return device.Interfaces[i].Number < device.Interfaces[j].Number
	})
	if allVendorSpecific(device.Interfaces) {
		device.Mode = "vendor-specific QMI/diagnostic mode"
	}
	return device
}

func allVendorSpecific(interfaces []usbInterfaceStatus) bool {
	if len(interfaces) == 0 {
		return false
	}
	for _, iface := range interfaces {
		if iface.Class != 255 {
			return false
		}
	}
	return true
}

func intPropertyOrZero(block, name string) int {
	value, _ := intProperty(block, name)
	return value
}

func intProperty(block, name string) (int, bool) {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*=\s*(\d+)`)
	match := pattern.FindStringSubmatch(block)
	if len(match) != 2 {
		return 0, false
	}
	value, err := strconv.Atoi(match[1])
	return value, err == nil
}

func stringProperty(block, name string) string {
	pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*=\s*"([^"]*)"`)
	match := pattern.FindStringSubmatch(block)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func formatHexProperty(block, name string) string {
	value, ok := intProperty(block, name)
	if !ok {
		return ""
	}
	return fmt.Sprintf("0x%x", value)
}

func usbSpeedName(speed int) string {
	switch speed {
	case 1:
		return "low-speed"
	case 2:
		return "full-speed"
	case 3:
		return "high-speed"
	case 4:
		return "super-speed"
	default:
		if speed == 0 {
			return ""
		}
		return fmt.Sprintf("speed-%d", speed)
	}
}

func (a *app) recordSMS(sender, content string, timestamp time.Time) {
	a.mergeSMS([]receivedSMS{{
		Sender: sender, Content: content, Timestamp: timestamp,
	}})
}

func (a *app) mergeSMS(messages []receivedSMS) (newCount int, total int) {
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	seen := make(map[string]bool, len(a.sms)+len(messages))
	for _, item := range a.sms {
		seen[smsCacheKey(item)] = true
	}
	for _, item := range messages {
		if item.Code == "" {
			item.Code = extractSMSCode(item.Content)
		}
		key := smsCacheKey(item)
		if seen[key] {
			continue
		}
		seen[key] = true
		a.sms = append(a.sms, item)
		newCount++
	}
	sort.SliceStable(a.sms, func(i, j int) bool {
		return a.sms[i].Timestamp.After(a.sms[j].Timestamp)
	})
	if len(a.sms) > 500 {
		a.sms = a.sms[:500]
	}
	return newCount, len(a.sms)
}

func smsCacheKey(item receivedSMS) string {
	return item.Sender + "\x00" + item.Content + "\x00" + item.Timestamp.Format(time.RFC3339Nano)
}

func (a *app) setSMSPollStatus(err error) {
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	a.smsLastPoll = time.Now()
	if err != nil {
		a.smsLastPollError = err.Error()
		return
	}
	a.smsLastPollError = ""
}

func (a *app) startSMSPoller(ctx context.Context) {
	interval := a.smsPollInterval
	if interval <= 0 {
		interval = 12 * time.Second
	}
	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			nextInterval := interval
			if a.currentUSBDevice() == nil {
				nextInterval = 15 * time.Second
			}
			if err := a.pollSMSOnce(); err != nil {
				log.Printf("SMS poll failed: %v", err)
			}
			timer.Reset(nextInterval)
		}
	}
}

func (a *app) pollSMSOnce() error {
	if a.demo || a.modem != nil {
		return nil
	}
	a.smsOperationMu.Lock()
	defer a.smsOperationMu.Unlock()
	if err := a.ensureUSBAT(); err != nil {
		a.setSMSPollStatus(err)
		return err
	}
	messages, err := a.readUSBATSMS()
	if err != nil {
		a.resetUSBATIfGone(err)
		a.setSMSPollStatus(err)
		return err
	}
	newCount, total := a.mergeSMS(messages)
	if a.smsAutoCleanupME && len(messages) > 0 {
		before, after, cleanupErr := a.clearUSBATSMSMemory("ME")
		if cleanupErr != nil {
			log.Printf("auto cleanup ME SMS failed: %v", cleanupErr)
		} else if before != after {
			log.Printf("auto cleanup ME SMS: %d -> %d", before, after)
		}
	}
	a.setSMSPollStatus(nil)
	if newCount > 0 {
		log.Printf("SMS poll cached %d new message(s), total %d", newCount, total)
	}
	return nil
}

func (a *app) ensureUSBAT() error {
	if a.demo || a.modem != nil || a.usbAT != nil {
		return nil
	}
	if a.currentUSBDevice() == nil {
		a.port = "未检测到 DJI USB 设备"
		a.discoveryError = "DJI USB device is not connected"
		return errors.New("DJI USB device is not connected")
	}
	if !a.usbATBackoffUntil.IsZero() && time.Now().Before(a.usbATBackoffUntil) {
		if a.usbATBackoffErr != "" {
			return fmt.Errorf("USB AT is cooling down after disconnect: %s", a.usbATBackoffErr)
		}
		return errors.New("USB AT is cooling down after disconnect")
	}

	// Serialize opens: a stuck libusb open must not freeze every caller.
	a.usbATOpenMu.Lock()
	defer a.usbATOpenMu.Unlock()
	if a.usbAT != nil {
		return nil
	}

	dev, err := a.openUSBATWithTimeout()
	if err != nil {
		if strings.Contains(err.Error(), "open timed out") {
			// The stuck libusb call keeps running in the background; back off
			// so retries do not pile up while the module USB is unstable.
			a.usbATBackoffUntil = time.Now().Add(30 * time.Second)
			a.usbATBackoffErr = err.Error()
		}
		return err
	}
	a.usbAT = dev
	a.usbATBackoffUntil = time.Time{}
	a.usbATBackoffErr = ""
	a.port = dev.Description()
	a.discoveryError = ""
	log.Printf("USB AT bridge opened on DJI %s", dev.Description())
	if changed, err := a.autoEnableMacAudioProfile(); err != nil {
		log.Printf("Mac audio profile check: %v", err)
	} else if changed {
		return nil
	}
	// The first open may fail while USB is re-enumerating. When a later poll
	// succeeds, rebuild the eSIM service that startup could not create.
	a.initUSBATESIMManager()
	a.ensureCellularDHCP()
	a.kickModuleVoice()
	return nil
}

// autoEnableMacAudioProfile restores UAC only after this installation explicitly
// saved iPhone/iPad mode. A newly installed App must never infer intent from a
// non-UAC USB tuple: that tuple may be an untouched factory module or a module
// configured by another tool.
func (a *app) autoEnableMacAudioProfile() (bool, error) {
	if a.demo || a.usbAT == nil {
		return false, nil
	}
	a.usbProfileMu.Lock()
	err := a.loadUSBProfileIntentLocked()
	armed := a.usbProfileMobileArmed
	a.usbProfileMu.Unlock()
	if err != nil {
		return false, err
	}
	if !armed {
		return false, nil
	}
	response, err := a.usbAT.Command(`AT+QCFG="usbcfg"`, 8*time.Second)
	if err != nil {
		return false, err
	}
	config, err := parseUSBConfig(response)
	if err != nil {
		return false, err
	}
	if config.uacEnabled() {
		return false, nil
	}
	if _, err := a.usbAT.Command(config.withUAC(true), 8*time.Second); err != nil {
		return false, err
	}
	a.usbProfileMu.Lock()
	a.usbProfileMobileArmed = false
	if err := a.persistUSBProfileIntentLocked(); err != nil {
		a.usbProfileMu.Unlock()
		return false, err
	}
	a.usbProfileMu.Unlock()
	if _, err := a.usbAT.Command("AT+CFUN=1,1", 3*time.Second); err != nil {
		return false, err
	}
	log.Printf("module arrived in iPhone/iPad mode; restoring Mac USB Audio and re-enumerating")
	a.markUSBATDetached("switching to Mac complete USB profile")
	return true, nil
}

const usbATOpenTimeout = 12 * time.Second

// openUSBATWithTimeout opens the USB AT bridge but never blocks the caller for
// longer than usbATOpenTimeout. libusb open/claim calls cannot be cancelled,
// so a slow attempt is abandoned and reaped in the background to avoid leaking
// a claimed interface when it finally completes.
func (a *app) openUSBATWithTimeout() (*usbAT, error) {
	type openResult struct {
		dev *usbAT
		err error
	}
	done := make(chan openResult, 1)
	go func() {
		dev, err := openDJIUSBAT()
		done <- openResult{dev: dev, err: err}
	}()
	select {
	case res := <-done:
		return res.dev, res.err
	case <-time.After(usbATOpenTimeout):
		go func() {
			res := <-done
			if res.dev != nil {
				res.dev.Close()
			}
		}()
		return nil, errors.New("USB AT open timed out after 12s; the module USB may be unstable")
	}
}

func (a *app) resetUSBATIfGone(err error) {
	if err == nil || a.usbAT == nil {
		return
	}
	text := strings.ToUpper(err.Error())
	if !strings.Contains(text, "NO_DEVICE") &&
		!strings.Contains(text, "NOT_FOUND") &&
		!strings.Contains(text, "DEVICE_NOT_FOUND") &&
		!strings.Contains(text, "LIBUSB_ERROR_NO_DEVICE") {
		return
	}
	a.markUSBATDetached(err.Error())
}

// markUSBATDetached clears state belonging to a physically removed module.
// A later status/SMS poll will discover and open a newly connected module.
func (a *app) markUSBATDetached(reason string) {
	if a.usbAT != nil {
		log.Printf("USB AT bridge detached; waiting for a new enumeration: %s", reason)
		a.usbAT.Close()
		a.usbAT = nil
	}
	a.usbDevice = nil
	a.port = "未检测到 DJI USB 设备"
	a.discoveryError = "DJI USB device is not connected"
	a.usbATBackoffUntil = time.Now().Add(2 * time.Second)
	a.usbATBackoffErr = reason
	a.cachedModemMu.Lock()
	a.cachedModemFirmware = ""
	a.cachedModemICCID = ""
	a.cachedModemIMSI = ""
	a.cachedModemUSBNet = -1
	a.cachedModemMu.Unlock()
	// A module reboot or re-enumeration can change USBCFG outside this process.
	// Do not keep reporting a previously cached "ready" state in that case.
	a.invalidateReadyModuleSetup()
	a.callMu.Lock()
	a.callConfigured = false
	a.callMu.Unlock()
	if manager, _ := a.currentESIMManager(); manager != nil {
		manager.NotifyModemReset()
	}
}

// startSignalRecovery runs a self-check loop that keeps the USB AT bridge
// open, watches cellular registration, and escalates through gentle recovery
// steps when the module loses the network for a sustained period.
func (a *app) startSignalRecovery(ctx context.Context) {
	const checkInterval = 25 * time.Second
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.currentUSBDevice() != nil {
				a.signalRecoveryOnce()
			}
		}
	}
}

func (a *app) signalRecoveryOnce() {
	if a.demo || a.modem != nil {
		return
	}
	if err := a.ensureUSBAT(); err != nil {
		return
	}
	if a.usbAT == nil {
		return
	}

	reg, signal, err := a.probeCellularHealth()
	if err != nil {
		// The USB AT channel itself broke; let the next cycle re-open it.
		a.resetUSBATIfGone(err)
		return
	}
	if reg == 1 || reg == 5 {
		if a.lostSignalCount > 0 {
			log.Printf("cellular signal recovered: registration=%d signal=%d dBm", reg, signal)
		}
		a.lostSignalCount = 0
	} else {
		a.lostSignalCount++
		log.Printf("cellular signal lost %d/3 checks: registration=%d signal=%d dBm", a.lostSignalCount, reg, signal)
		if a.lostSignalCount >= 3 {
			a.recoverSignal()
		}
	}

	// The USB network interface rarely drops by itself, so only re-check
	// DHCP every minute to avoid hammering networksetup.
	if time.Since(a.lastNetworkCheck) >= 60*time.Second {
		a.lastNetworkCheck = time.Now()
		a.ensureCellularDHCP()
	}
}

// probeCellularHealth cheaply reads registration and signal without running
// the full status command set.
func (a *app) probeCellularHealth() (reg int, signalDBM int, err error) {
	cregResp, err := a.usbAT.Command("AT+CREG?", 3*time.Second)
	if err != nil {
		return 0, 0, err
	}
	ceregResp, _ := a.usbAT.Command("AT+CEREG?", 3*time.Second)
	csqResp, _ := a.usbAT.Command("AT+CSQ", 3*time.Second)
	return firstNonZeroRegistration(cregResp, ceregResp), parseUSBATCSQDBM(csqResp), nil
}

// recoverSignal escalates step by step after the module has been without a
// network for several consecutive checks: re-attach, radio cycle, and finally
// a throttled full module reboot.
func (a *app) recoverSignal() {
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	if a.usbAT == nil {
		return
	}
	switch {
	case a.lostSignalCount == 3:
		log.Printf("cellular signal recovery: forcing PS attach and automatic network selection")
		_, _ = a.usbAT.Command("AT+CGATT=1", 5*time.Second)
		_, _ = a.usbAT.Command("AT+COPS=0", 5*time.Second)
	case a.lostSignalCount == 6:
		log.Printf("cellular signal recovery: cycling radio (AT+CFUN=0 then 1)")
		_, _ = a.usbAT.Command("AT+CFUN=0", 5*time.Second)
		time.Sleep(3 * time.Second)
		if a.usbAT != nil {
			_, _ = a.usbAT.Command("AT+CFUN=1", 10*time.Second)
		}
	case a.lostSignalCount >= 9 && time.Since(a.lastModemReboot) >= 10*time.Minute:
		a.lastModemReboot = time.Now()
		log.Printf("cellular signal recovery: rebooting module (AT+CFUN=1,1)")
		_, _ = a.usbAT.Command("AT+CFUN=1,1", 3*time.Second)
		a.markUSBATDetached("cellular signal recovery reboot")
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/platform", a.platformInfo)
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("GET /api/sms", a.listSMS)
	mux.HandleFunc("GET /api/sms/status", a.smsStatus)
	mux.HandleFunc("PATCH /api/sms/settings", a.updateSMSSettings)
	mux.HandleFunc("GET /api/sim/identity", a.simIdentity)
	mux.HandleFunc("POST /api/sms/send", a.sendSMS)
	mux.HandleFunc("POST /api/sms/refresh", a.refreshSMS)
	mux.HandleFunc("POST /api/sms/clear-module", a.clearModuleSMS)
	mux.HandleFunc("GET /api/calls/status", a.callStatus)
	mux.HandleFunc("POST /api/calls/reject", a.rejectCall)
	mux.HandleFunc("POST /api/calls/answer", a.answerCall)
	mux.HandleFunc("POST /api/calls/hangup", a.hangupCall)
	mux.HandleFunc("POST /api/calls/dtmf", a.dtmfCall)
	mux.HandleFunc("POST /api/calls/dial", a.dialCall)
	mux.HandleFunc("POST /api/calls/audio/start", a.audioStart)
	mux.HandleFunc("POST /api/calls/audio/stop", a.audioStop)
	mux.HandleFunc("POST /api/calls/audio/mute", a.audioMute)
	mux.HandleFunc("POST /api/calls/audio/record", a.audioRecord)
	mux.HandleFunc("POST /api/calls/audio/host/register", a.audioHostRegister)
	mux.HandleFunc("GET /api/calls/audio/host/config", a.audioHostConfig)
	mux.HandleFunc("GET /api/voice/status", a.voiceStatusAPI)
	mux.HandleFunc("POST /api/voice/provision", a.voiceProvisionAPI)
	mux.HandleFunc("GET /api/module/setup", a.moduleSetupStatusAPI)
	mux.HandleFunc("POST /api/module/setup", a.moduleSetupStartAPI)
	mux.HandleFunc("POST /api/voice/start", a.voiceStartAPI)
	mux.HandleFunc("POST /api/voice/stop", a.voiceStopAPI)
	mux.HandleFunc("GET /api/gps", a.gpsStatus)
	mux.HandleFunc("POST /api/gps/start", a.startGPS)
	mux.HandleFunc("POST /api/gps/stop", a.stopGPS)
	mux.HandleFunc("POST /api/gps/refresh", a.refreshGPS)
	mux.HandleFunc("POST /api/at", a.executeAT)
	mux.HandleFunc("GET /api/network", a.networkDiagnostic)
	mux.HandleFunc("GET /api/network/traffic", a.networkTraffic)
	mux.HandleFunc("GET /api/network/cellular-policy", a.getCellularPolicy)
	mux.HandleFunc("POST /api/network/cellular-policy", a.setCellularPolicy)
	mux.HandleFunc("POST /api/network/check-4g", a.check4GRoute)
	mux.HandleFunc("POST /api/network/check-proxy", a.checkProxyRoute)
	mux.HandleFunc("POST /api/network/usbnet", a.setUSBNetMode)
	mux.HandleFunc("POST /api/network/reboot-module", a.rebootModule)
	mux.HandleFunc("GET /api/usb/profile", a.usbProfile)
	mux.HandleFunc("POST /api/usb/profile", a.setUSBProfile)
	mux.HandleFunc("GET /api/esim", a.esimOverview)
	mux.HandleFunc("GET /api/esim/notes", a.listESIMNotes)
	mux.HandleFunc("PUT /api/esim/notes", a.saveESIMNote)
	mux.HandleFunc("GET /api/esim/module-notes", a.listModuleESIMNotes)
	mux.HandleFunc("PUT /api/esim/module-notes", a.saveModuleESIMNote)
	mux.HandleFunc("GET /api/esim/health", a.esimHealth)
	mux.HandleFunc("POST /api/esim/phonebook/probe", a.probeESIMPhonebook)
	mux.HandleFunc("POST /api/esim/switch", a.switchESIM)
	mux.HandleFunc("PATCH /api/esim/profile", a.renameESIMProfile)
	mux.HandleFunc("DELETE /api/esim/profile", a.deleteESIMProfile)
	mux.HandleFunc("POST /api/esim/download", a.downloadESIMProfile)
	if runtime.GOOS == "windows" || a.webConsole {
		assets, err := fs.Sub(webAssets, "web")
		if err != nil {
			panic(fmt.Sprintf("open embedded web console: %v", err))
		}
		mux.Handle("/", http.FileServer(http.FS(assets)))
		return securityHeaders(mux)
	}

	// macOS 日常操作迁移到独立 App；根路径保留兼容提示。
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("DJOneHub 已迁移到 macOS 应用，请使用 DJOneHub App 完成全部操作。"))
	})
	return securityHeaders(mux)
}

func (a *app) platformInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":               "1.2.4",
		"os":                    runtime.GOOS,
		"web_console":           runtime.GOOS == "windows" || a.webConsole,
		"call_audio":            runtime.GOOS == "darwin",
		"direct_usb_at":         runtime.GOOS == "darwin",
		"esim_full":             runtime.GOOS == "darwin",
		"network_policy_native": runtime.GOOS == "darwin",
		"native_contacts":       runtime.GOOS == "darwin",
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *app) health(w http.ResponseWriter, _ *http.Request) {
	usbDevice := a.currentUSBDevice()
	esimManager, _ := a.currentESIMManager()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "port": a.port, "esim_available": a.demo || esimManager != nil, "demo": a.demo,
		"usb_device": usbDevice, "discovery_error": a.discoveryError,
	})
}

func (a *app) status(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusOK, modem.DeviceStatus{
			IMEI:          "867400000000001",
			Firmware:      "EG25GGBR07A08M2G",
			ICCID:         "89860123456789012345",
			IMSI:          "460001234567890",
			Operator:      "中国移动",
			SimInserted:   true,
			SignalDBM:     -73,
			SignalRSRP:    -96,
			SignalRSRQ:    -9,
			RegStatus:     1,
			RegStatusText: "已注册",
			NetworkMode:   "LTE",
			NetworkDuplex: "FDD",
			RadioBand:     "B3",
			USBNetMode:    0,
		})
		return
	}
	if a.modem == nil {
		// A libusb handle may survive a physical unplug. Refresh the macOS USB
		// inventory before using it so the UI never reports a stale connection.
		if a.usbAT != nil && a.currentUSBDevice() == nil {
			a.markUSBATDetached("DJI USB device disconnected")
		}
		if err := a.ensureUSBAT(); err != nil {
			log.Printf("USB AT retry failed: %v", err)
		}
		if a.usbAT != nil {
			status, err := a.usbATStatus()
			if err == nil {
				writeJSON(w, http.StatusOK, status)
				return
			}
			a.resetUSBATIfGone(err)
			log.Printf("USB AT status failed: %v", err)
		}
		usbDevice := a.currentUSBDevice()
		summary := "未发现 AT 串口"
		operator := "未连接"
		network := "不可用"
		if usbDevice != nil {
			summary = fmt.Sprintf("%s %s (%s:%s)", usbDevice.Vendor, usbDevice.Product, usbDevice.VendorID, usbDevice.ProductID)
			operator = "已检测到 USB 设备"
			network = usbDevice.Mode
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"operator":        operator,
			"signal_dbm":      nil,
			"network_mode":    network,
			"sim_inserted":    false,
			"hardware_status": summary,
			"discovery_error": a.discoveryError,
			"usb_device":      usbDevice,
		})
		return
	}
	writeJSON(w, http.StatusOK, a.modem.GetFullStatus())
}

func (a *app) currentUSBDevice() *usbDeviceStatus {
	if a.modem != nil || a.demo {
		return a.usbDevice
	}
	usbDevice := discoverDJIUSBDevice()
	// Never retain the last successful scan: that is stale after an unplug.
	a.usbDevice = usbDevice
	return usbDevice
}

func (a *app) usbATStatus() (modem.DeviceStatus, error) {
	a.cachedModemMu.Lock()
	if a.cachedModemFirmware == "" {
		if fwResp, err := a.usbAT.Command("ATI", 1200*time.Millisecond); err == nil {
			a.cachedModemFirmware = parseUSBATFirmware(fwResp)
		}
		if qccidResp, err := a.usbAT.Command("AT+QCCID", 1200*time.Millisecond); err == nil {
			a.cachedModemICCID = parseUSBATPrefixed(qccidResp, "+QCCID:")
		}
		if cimiResp, err := a.usbAT.Command("AT+CIMI", 1200*time.Millisecond); err == nil {
			a.cachedModemIMSI = parseUSBATIMSI(cimiResp)
		}
		if usbnetResp, err := a.usbAT.Command(`AT+QCFG="usbnet"`, 1200*time.Millisecond); err == nil {
			if parsedMode, err := strconv.Atoi(parseUSBNetMode(usbnetResp)); err == nil {
				a.cachedModemUSBNet = parsedMode
			}
		}
	}
	cachedFirmware := a.cachedModemFirmware
	cachedICCID := a.cachedModemICCID
	cachedIMSI := a.cachedModemIMSI
	cachedUSBNet := a.cachedModemUSBNet
	a.cachedModemMu.Unlock()

	cpinResp, _ := a.usbAT.Command("AT+CPIN?", 1000*time.Millisecond)
	csqResp, _ := a.usbAT.Command("AT+CSQ", 1000*time.Millisecond)
	ceregResp, _ := a.usbAT.Command("AT+CEREG?", 1000*time.Millisecond)
	cregResp, _ := a.usbAT.Command("AT+CREG?", 1000*time.Millisecond)
	copsResp, _ := a.usbAT.Command("AT+COPS?", 1000*time.Millisecond)
	qnwinfoResp, _ := a.usbAT.Command("AT+QNWINFO", 1000*time.Millisecond)

	regStatus := firstNonZeroRegistration(ceregResp, cregResp)
	mode, duplex, band, channel := parseUSBATQNWInfo(qnwinfoResp)
	upperCPIN := strings.ToUpper(cpinResp)
	simInserted := strings.Contains(upperCPIN, "READY") || strings.Contains(upperCPIN, "PIN") || strings.Contains(upperCPIN, "PUK")

	operatorName := modem.NormalizeServingOperatorName(parseUSBATOperator(copsResp), cachedIMSI)
	if operatorName == "" && strings.Contains(copsResp, "CHN-UNICOM") {
		operatorName = "中国联通"
	}
	if operatorName == "" && cachedIMSI != "" {
		operatorName = modem.NormalizeServingOperatorName("", cachedIMSI)
	}

	status := modem.DeviceStatus{
		Firmware:      cachedFirmware,
		ICCID:         cachedICCID,
		IMSI:          cachedIMSI,
		Operator:      operatorName,
		SimInserted:   simInserted,
		SignalDBM:     parseUSBATCSQDBM(csqResp),
		RegStatus:     regStatus,
		RegStatusText: registrationText(regStatus),
		NetworkMode:   mode,
		NetworkDuplex: duplex,
		RadioBand:     band,
		RadioChannel:  channel,
		USBNetMode:    cachedUSBNet,
	}
	return status, nil
}

func parseUSBATFirmware(resp string) string {
	lines := splitATLines(resp)
	var useful []string
	for _, line := range lines {
		up := strings.ToUpper(line)
		if strings.HasPrefix(up, "ATI") || up == "OK" {
			continue
		}
		useful = append(useful, line)
	}
	return strings.Join(useful, " · ")
}

func splitATLines(resp string) []string {
	resp = strings.ReplaceAll(resp, "\r", "\n")
	raw := strings.Split(resp, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseUSBATPrefixed(resp, prefix string) string {
	for _, line := range splitATLines(resp) {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func parseUSBATIMSI(resp string) string {
	for _, line := range splitATLines(resp) {
		up := strings.ToUpper(line)
		if up == "OK" || strings.HasPrefix(up, "AT") {
			continue
		}
		if _, err := strconv.ParseUint(line, 10, 64); err == nil && len(line) >= 5 {
			return line
		}
	}
	return ""
}

func parseUSBATCSQDBM(resp string) int {
	re := regexp.MustCompile(`\+CSQ:\s*(\d+),`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return 0
	}
	rssi, err := strconv.Atoi(match[1])
	if err != nil || rssi == 99 {
		return 0
	}
	return -113 + 2*rssi
}

func parseUSBATOperator(resp string) string {
	re := regexp.MustCompile(`\+COPS:\s*\d+,\d+,"([^"]*)"`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func firstNonZeroRegistration(responses ...string) int {
	for _, resp := range responses {
		re := regexp.MustCompile(`\+(?:CE)?REG:\s*\d+,(\d+)`)
		match := re.FindStringSubmatch(resp)
		if len(match) != 2 {
			continue
		}
		status, err := strconv.Atoi(match[1])
		if err == nil && status != 0 {
			return status
		}
	}
	return 0
}

func registrationText(status int) string {
	switch status {
	case 1:
		return "已注册"
	case 5:
		return "漫游注册"
	case 2:
		return "搜索中"
	case 3:
		return "注册被拒绝"
	default:
		return "未注册"
	}
}

func parseUSBATQNWInfo(resp string) (mode, duplex, band string, channel uint32) {
	re := regexp.MustCompile(`\+QNWINFO:\s*"([^"]*)","[^"]*","([^"]*)",(\d+)`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 4 {
		return "", "", "", 0
	}
	mode = match[1]
	radio := match[2]
	if strings.Contains(strings.ToUpper(mode), "FDD") {
		duplex = "FDD"
		mode = strings.TrimSpace(strings.TrimPrefix(mode, "FDD"))
	}
	if strings.Contains(strings.ToUpper(mode), "TDD") {
		duplex = "TDD"
		mode = strings.TrimSpace(strings.TrimPrefix(mode, "TDD"))
	}
	band = strings.TrimPrefix(radio, "LTE ")
	if value, err := strconv.ParseUint(match[3], 10, 32); err == nil {
		channel = uint32(value)
	}
	return mode, duplex, band, channel
}

func (a *app) readUSBATSMS() ([]receivedSMS, error) {
	if _, err := a.usbAT.Command("AT+CMGF=0", 3*time.Second); err != nil {
		return nil, fmt.Errorf("set SMS PDU mode: %w", err)
	}
	memories := []string{"SM", "ME"}
	seen := make(map[string]bool)
	messages := make([]receivedSMS, 0)
	var errs []string
	for _, memory := range memories {
		items, err := a.readUSBATSMSFromMemory(memory)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", memory, err))
			continue
		}
		for _, item := range items {
			key := item.Sender + "\x00" + item.Content + "\x00" + item.Timestamp.Format(time.RFC3339Nano)
			if seen[key] {
				continue
			}
			seen[key] = true
			messages = append(messages, item)
		}
	}
	if len(messages) == 0 && len(errs) == len(memories) {
		return nil, fmt.Errorf("list SMS failed: %s", strings.Join(errs, "; "))
	}
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Timestamp.After(messages[j].Timestamp)
	})
	return messages, nil
}

func (a *app) readUSBATSMSFromMemory(memory string) ([]receivedSMS, error) {
	if _, err := a.usbAT.Command(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, memory, memory, memory), 5*time.Second); err != nil {
		return nil, fmt.Errorf("select storage: %w", err)
	}
	resp, err := a.usbAT.Command("AT+CMGL=4", 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("list SMS: %w", err)
	}
	pdus := parseUSBATCMGL(resp)
	messages := make([]receivedSMS, 0, len(pdus))
	for _, item := range pdus {
		msg, concat, err := decodeUSBATPDU(item.header, item.pdu)
		if err != nil {
			messages = append(messages, receivedSMS{
				Sender:    "PDU",
				Content:   fmt.Sprintf("[短信解析失败] %v\n%s", err, item.pdu),
				Timestamp: time.Now(),
			})
			continue
		}
		if concat.IsConcat {
			if a.smsReassembler == nil {
				a.smsReassembler = smscodec.NewReassembler()
			}
			complete, content := a.smsReassembler.Add(msg.Sender, concat, msg.Content)
			if !complete {
				continue
			}
			msg.Content = content
			log.Printf("USB AT long SMS reassembled: sender=%s segments=%d", msg.Sender, concat.Total)
		}
		messages = append(messages, msg)
	}
	if a.smsReassembler != nil {
		a.smsReassembler.Cleanup(10 * time.Minute)
	}
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].Timestamp.After(messages[j].Timestamp)
	})
	return messages, nil
}

func (a *app) clearUSBATSMSMemory(memory string) (before, after int, err error) {
	resp, err := a.usbAT.Command(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, memory, memory, memory), 5*time.Second)
	if err != nil {
		return 0, 0, fmt.Errorf("select storage: %w", err)
	}
	before = parseUSBATCPMSUsed(resp)
	if _, err := a.usbAT.Command("AT+CMGD=1,4", 20*time.Second); err != nil {
		return before, 0, fmt.Errorf("delete messages: %w", err)
	}
	resp, err = a.usbAT.Command(fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, memory, memory, memory), 5*time.Second)
	if err != nil {
		return before, 0, fmt.Errorf("recheck storage: %w", err)
	}
	after = parseUSBATCPMSUsed(resp)
	return before, after, nil
}

type smsStorageClearResult struct {
	Memory string `json:"memory"`
	Before int    `json:"before"`
	After  int    `json:"after"`
}

// clearAllUSBATSMS removes messages from both stores surfaced by readUSBATSMS.
// The previous implementation only cleared ME while the inbox also showed SM,
// which made a successful delete look like it had done nothing.
func (a *app) clearAllUSBATSMS() ([]smsStorageClearResult, error) {
	results := make([]smsStorageClearResult, 0, 2)
	for _, memory := range []string{"SM", "ME"} {
		before, after, err := a.clearUSBATSMSMemory(memory)
		if err != nil {
			return results, fmt.Errorf("clear %s SMS: %w", memory, err)
		}
		results = append(results, smsStorageClearResult{
			Memory: memory,
			Before: before,
			After:  after,
		})
	}
	return results, nil
}

func (a *app) clearSMSCache() {
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	a.sms = nil
}

func parseUSBATCPMSUsed(resp string) int {
	re := regexp.MustCompile(`\+CPMS:\s*(\d+),`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return 0
	}
	used, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return used
}

type usbATSMSPDU struct {
	header string
	pdu    string
}

func parseUSBATCMGL(resp string) []usbATSMSPDU {
	lines := splitATLines(resp)
	var out []usbATSMSPDU
	for i := 0; i < len(lines)-1; i++ {
		if !strings.HasPrefix(lines[i], "+CMGL:") {
			continue
		}
		next := strings.TrimSpace(lines[i+1])
		if !smscodec.IsHexString(next) {
			continue
		}
		pdu, _ := smscodec.TrimFullPDUHexByATHeader(next, lines[i])
		out = append(out, usbATSMSPDU{header: lines[i], pdu: pdu})
		i++
	}
	return out
}

func decodeUSBATPDU(header, pduHex string) (receivedSMS, smscodec.ConcatInfo, error) {
	raw := strings.TrimSpace(pduHex)
	if trimmed, ok := smscodec.TrimFullPDUHexByATHeader(raw, header); ok {
		raw = trimmed
	}
	full, err := hex.DecodeString(raw)
	if err != nil {
		return receivedSMS{}, smscodec.ConcatInfo{}, err
	}
	if len(full) < 2 {
		return receivedSMS{}, smscodec.ConcatInfo{}, errors.New("PDU too short")
	}
	smscLen := int(full[0])
	tpduOffset := 1 + smscLen
	if tpduOffset >= len(full) {
		return receivedSMS{}, smscodec.ConcatInfo{}, errors.New("PDU has invalid SMSC length")
	}
	sender, content, timestamp, concat, err := smscodec.DecodeDeliverTPDU(full[tpduOffset:])
	if err != nil {
		return receivedSMS{}, smscodec.ConcatInfo{}, err
	}
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	return receivedSMS{Sender: sender, Content: content, Timestamp: timestamp}, concat, nil
}

func (a *app) listSMS(w http.ResponseWriter, _ *http.Request) {
	a.smsMu.RLock()
	items := append([]receivedSMS(nil), a.sms...)
	a.smsMu.RUnlock()
	if items == nil {
		items = []receivedSMS{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *app) smsStatus(w http.ResponseWriter, _ *http.Request) {
	a.smsMu.RLock()
	lastPoll := a.smsLastPoll
	lastPollError := a.smsLastPollError
	count := len(a.sms)
	a.smsMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"count":           count,
		"polling":         !a.demo && a.modem == nil,
		"poll_interval_s": int(a.smsPollInterval.Seconds()),
		"auto_cleanup_me": a.smsAutoCleanupME,
		"last_poll":       lastPoll,
		"last_poll_error": lastPollError,
	})
}

func (a *app) updateSMSSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoCleanupME *bool `json:"auto_cleanup_me"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.AutoCleanupME == nil {
		writeError(w, http.StatusBadRequest, "auto_cleanup_me is required")
		return
	}
	a.smsOperationMu.Lock()
	a.smsAutoCleanupME = *body.AutoCleanupME
	a.smsOperationMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"auto_cleanup_me": a.smsAutoCleanupME})
}

func (a *app) simIdentity(w http.ResponseWriter, r *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"phone_number": "+8613800138000"})
		return
	}
	a.smsOperationMu.Lock()
	defer a.smsOperationMu.Unlock()
	if err := a.ensureUSBAT(); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	response, err := a.runATCommand("AT+CNUM", 3*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"phone_number": parseCNUM(response)})
}

func parseCNUM(response string) string {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "+CNUM:") {
			continue
		}
		fields := strings.Split(strings.TrimSpace(line[len("+CNUM:"):]), ",")
		if len(fields) < 2 {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(fields[1]), `"`)
		digits := regexp.MustCompile(`[^0-9+]`).ReplaceAllString(raw, "")
		if len(strings.TrimPrefix(digits, "+")) >= 5 {
			return digits
		}
	}
	return ""
}

func (a *app) refreshSMS(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
		return
	}
	if a.modem == nil {
		if err := a.pollSMSOnce(); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		a.smsMu.RLock()
		count := len(a.sms)
		a.smsMu.RUnlock()
		writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "count": count})
		return
	}
	go a.modem.CheckAllSMS()
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (a *app) clearModuleSMS(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]any{"cleared": true, "before": 0, "after": 0, "storages": []string{"SM", "ME"}})
		return
	}
	if a.modem != nil {
		writeError(w, http.StatusServiceUnavailable, "module SMS cleanup is only available through USB AT")
		return
	}
	if err := a.ensureUSBAT(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "AT serial port is unavailable: "+err.Error())
		return
	}
	a.smsOperationMu.Lock()
	defer a.smsOperationMu.Unlock()
	results, err := a.clearAllUSBATSMS()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	before, after := 0, 0
	for _, result := range results {
		before += result.Before
		after += result.After
	}
	// The inbox is a local cache so it can continue showing messages after
	// automatic ME cleanup. A manual delete is explicit, therefore remove the
	// matching cached view as well instead of making the UI appear unchanged.
	a.clearSMSCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"cleared":  true,
		"before":   before,
		"after":    after,
		"storages": results,
	})
}

func (a *app) runATCommand(command string, timeout time.Duration) (string, error) {
	if a.demo {
		responses := map[string]string{
			"AT":                 "OK",
			"AT+CSQ":             "+CSQ: 22,99\r\nOK",
			"AT+COPS?":           "+COPS: 0,0,\"China Mobile\",7\r\nOK",
			"AT+QNWINFO":         "+QNWINFO: \"FDD LTE\",\"46000\",\"LTE BAND 3\",1650\r\nOK",
			"AT+QCFG=\"USBNET\"": "+QCFG: \"usbnet\",1\r\nOK",
			"AT+QCFG=\"USBCFG\"": "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,0,0\r\nOK",
			"AT+CGDCONT?":        "+CGDCONT: 1,\"IPV4V6\",\"3gnet\",\"0.0.0.0\",0,0,0,0\r\nOK",
			"AT+CGACT?":          "+CGACT: 1,1\r\nOK",
			"AT+CGPADDR=1":       "+CGPADDR: 1,\"10.23.45.67\"\r\nOK",
		}
		response := responses[strings.ToUpper(strings.TrimSpace(command))]
		if response == "" {
			response = "OK"
		}
		return response, nil
	}
	if a.modem == nil {
		if err := a.ensureUSBAT(); err != nil {
			return "", err
		}
		if a.usbAT == nil {
			return "", errors.New("AT serial port is unavailable")
		}
		response, err := a.usbAT.Command(command, timeout)
		if err != nil {
			a.resetUSBATIfGone(err)
		}
		return response, err
	}
	return a.modem.ExecuteAT(command, timeout)
}

func (a *app) sendSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone   string `json:"phone"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Phone) == "" || strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, "phone and message are required")
		return
	}
	if a.demo {
		a.recordSMS("已发送至 "+body.Phone, body.Message, time.Now())
		writeJSON(w, http.StatusOK, map[string]any{"sent": true, "segments": 1})
		return
	}
	segments, err := a.sendTextSMS(body.Phone, body.Message)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "segments": segments})
}

func (a *app) sendTextSMS(phone, message string) (int, error) {
	if a.modem == nil {
		return a.sendUSBATSMS(phone, message)
	}
	if err := a.modem.SendSMSWithOptions(phone, message, smsSubmitOptions(message)); err != nil {
		return 0, err
	}
	return 1, nil
}

func (a *app) sendUSBATSMS(phone, message string) (int, error) {
	a.smsSendMu.Lock()
	defer a.smsSendMu.Unlock()

	if err := a.ensureUSBAT(); err != nil {
		return 0, err
	}
	if a.usbAT == nil {
		return 0, errors.New("AT serial port is unavailable")
	}

	modeResponse, err := a.usbAT.Command("AT+CMGF=0", 5*time.Second)
	if err != nil {
		a.resetUSBATIfGone(err)
		return 0, fmt.Errorf("set SMS PDU mode: %w", err)
	}
	if !atProbeSucceeded(modeResponse) {
		return 0, fmt.Errorf("set SMS PDU mode failed: %s", modeResponse)
	}

	tpdus, tpduLengths, err := smscodec.BuildSubmitTPDUsWithOptions(phone, message, smsSubmitOptions(message))
	if err != nil {
		return 0, fmt.Errorf("build SMS PDU: %w", err)
	}
	for i, tpdu := range tpdus {
		pdu := append([]byte{0x00}, tpdu...)
		payload := []byte(strings.ToUpper(hex.EncodeToString(pdu)) + "\x1a")
		response, sendErr := a.usbAT.CommandWithPrompt(
			fmt.Sprintf("AT+CMGS=%d", tpduLengths[i]),
			payload,
			45*time.Second,
		)
		if sendErr != nil {
			a.resetUSBATIfGone(sendErr)
			return i, fmt.Errorf("send SMS segment %d/%d: %w", i+1, len(tpdus), sendErr)
		}
		if atResponseIsError(response) || !strings.Contains(response, "+CMGS:") || !atProbeSucceeded(response) {
			return i, fmt.Errorf("send SMS segment %d/%d failed: %s", i+1, len(tpdus), response)
		}
		if i+1 < len(tpdus) {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return len(tpdus), nil
}

func smsSubmitOptions(message string) smscodec.SubmitOptions {
	for _, r := range message {
		if r > 127 {
			return smscodec.SubmitOptions{Encoding: smscodec.SMSEncodingUCS2}
		}
	}
	return smscodec.SubmitOptions{}
}

func (a *app) executeAT(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(body.Command)), "AT") {
		writeError(w, http.StatusBadRequest, "command must start with AT")
		return
	}
	if a.demo {
		response, _ := a.runATCommand(body.Command, 20*time.Second)
		writeJSON(w, http.StatusOK, map[string]string{"response": response})
		return
	}
	response, err := a.runATCommand(body.Command, 20*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"response": response})
}

func (a *app) networkDiagnostic(w http.ResponseWriter, _ *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("network diagnostic panic: %v", recovered)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("network diagnostic failed: %v", recovered))
		}
	}()
	raw := make(map[string]string)
	errs := make(map[string]string)
	diag := networkDiagnostic{
		USBDevice:     a.currentUSBDevice(),
		MacInterfaces: discoverMacNetworkInterfaces(),
		DefaultRoute:  discoverMacDefaultRoute(),
		Raw:           raw,
		Errors:        errs,
	}
	diag.USBNetworkPresent = hasLikelyUSBNetworkInterface(diag.MacInterfaces)

	commands := map[string]string{
		"usbnet":  `AT+QCFG="usbnet"`,
		"usbcfg":  `AT+QCFG="usbcfg"`,
		"cgdcont": `AT+CGDCONT?`,
		"cgact":   `AT+CGACT?`,
		"cgpaddr": `AT+CGPADDR=1`,
	}
	for key, command := range commands {
		resp, err := a.runATCommand(command, 8*time.Second)
		if err != nil {
			errs[key] = err.Error()
			continue
		}
		raw[key] = resp
	}

	diag.USBNetMode = parseUSBNetMode(raw["usbnet"])
	diag.USBCfg = parseUSBATPrefixed(raw["usbcfg"], "+QCFG:")
	diag.PDPContexts = parsePDPContexts(raw["cgdcont"])
	diag.ActiveContexts = parseActivePDPContexts(raw["cgact"])
	diag.PDPAddresses = parsePDPAddresses(raw["cgpaddr"])
	if len(errs) == 0 {
		diag.Errors = nil
	}
	writeJSON(w, http.StatusOK, diag)
}

var (
	cachedTrafficIfName string
	cachedTrafficIfAt   time.Time
	cachedTrafficIfMu   sync.Mutex
)

func (a *app) selectTrafficInterfaceCached() string {
	cachedTrafficIfMu.Lock()
	defer cachedTrafficIfMu.Unlock()
	if time.Since(cachedTrafficIfAt) < 8*time.Second && cachedTrafficIfName != "" {
		return cachedTrafficIfName
	}
	interfaces := discoverMacNetworkInterfaces()
	name := selectUSBTrafficInterface(interfaces, discoverMacDefaultRoute())
	cachedTrafficIfName = name
	cachedTrafficIfAt = time.Now()
	return name
}

func (a *app) networkTraffic(w http.ResponseWriter, _ *http.Request) {
	snapshot := networkTrafficSnapshot{
		SampledAtMS: time.Now().UnixMilli(),
	}
	if policy, err := a.cellularPolicyStatus(); err == nil && policy.ForceOff {
		snapshot.Error = "4G 已关闭"
		writeJSON(w, http.StatusOK, snapshot)
		return
	}

	name := a.selectTrafficInterfaceCached()
	if name == "" {
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	counters, err := discoverMacInterfaceCounters()
	if err != nil {
		snapshot.Interface = name
		snapshot.Error = err.Error()
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	current, ok := counters[name]
	if !ok {
		snapshot.Interface = name
		snapshot.Error = "未读取到网卡计数"
		writeJSON(w, http.StatusOK, snapshot)
		return
	}

	a.trafficMu.Lock()
	if a.trafficBaselines == nil {
		a.trafficBaselines = make(map[string]networkByteCounters)
	}
	baseline, exists := a.trafficBaselines[name]
	if !exists || current.RX < baseline.RX || current.TX < baseline.TX {
		baseline = current
		a.trafficBaselines[name] = baseline
	}
	a.trafficMu.Unlock()

	snapshot.Available = true
	snapshot.Interface = name
	snapshot.RXBytes = current.RX
	snapshot.TXBytes = current.TX
	snapshot.SessionRX, snapshot.SessionTX, snapshot.SessionTotal = sessionTrafficFromCounters(current, baseline)
	writeJSON(w, http.StatusOK, snapshot)
}

func sessionTrafficFromCounters(current, baseline networkByteCounters) (rx, tx, total uint64) {
	rx = current.RX - baseline.RX
	tx = current.TX - baseline.TX
	return rx, tx, rx + tx
}

func (a *app) check4GRoute(w http.ResponseWriter, _ *http.Request) {
	route := discoverMacDefaultRoute()
	interfaces := discoverMacNetworkInterfaces()
	var active *macNetInterface
	for i := range interfaces {
		if interfaces[i].Name == route.Interface {
			active = &interfaces[i]
			break
		}
	}
	if route.Interface == "" {
		writeJSON(w, http.StatusOK, networkCheckResult{
			OK:      false,
			Summary: "未读取到默认出口",
			Detail:  "macOS 没有返回 default route",
		})
		return
	}
	if active != nil && active.Name != "en0" && active.Kind == "ethernet" && active.Status == "active" {
		writeJSON(w, http.StatusOK, networkCheckResult{
			OK:      true,
			Summary: "当前正在走 4G 模块",
			Detail:  fmt.Sprintf("默认出口 %s -> %s，IP %s", route.Interface, route.Gateway, active.IPv4),
		})
		return
	}
	detail := fmt.Sprintf("默认出口 %s -> %s", route.Interface, route.Gateway)
	if active != nil && active.IPv4 != "" {
		detail += "，IP " + active.IPv4
	}
	writeJSON(w, http.StatusOK, networkCheckResult{
		OK:      false,
		Summary: "当前没有优先走 4G 模块",
		Detail:  detail,
	})
}

func (a *app) checkProxyRoute(w http.ResponseWriter, _ *http.Request) {
	proxyURL, _ := url.Parse("http://127.0.0.1:7890")
	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	req, err := http.NewRequest(http.MethodHead, "https://www.google.com/generate_204", nil)
	if err != nil {
		writeJSON(w, http.StatusOK, networkCheckResult{OK: false, Summary: "代理检测请求创建失败", Detail: err.Error()})
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, networkCheckResult{
			OK:      false,
			Summary: "代理未打通",
			Detail:  "127.0.0.1:7890 代理访问失败：" + err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || (resp.StatusCode >= 200 && resp.StatusCode < 400) {
		writeJSON(w, http.StatusOK, networkCheckResult{
			OK:      true,
			Summary: "代理已打通",
			Detail:  fmt.Sprintf("127.0.0.1:7890 -> google generate_204 返回 %s", resp.Status),
		})
		return
	}
	writeJSON(w, http.StatusOK, networkCheckResult{
		OK:      false,
		Summary: "代理响应异常",
		Detail:  fmt.Sprintf("127.0.0.1:7890 返回 %s", resp.Status),
	})
}

type macNetworkService struct {
	Name         string
	HardwarePort string
	Device       string
	Disabled     bool
}

func discoverMacNetworkServices() ([]macNetworkService, error) {
	out, err := exec.Command("/usr/sbin/networksetup", "-listnetworkserviceorder").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("读取 macOS 网络服务失败: %s", strings.TrimSpace(string(out)))
	}
	return parseMacNetworkServices(string(out)), nil
}

func parseMacNetworkServices(output string) []macNetworkService {
	header := regexp.MustCompile(`^\((\*|\d+)\)\s+(.+)$`)
	detail := regexp.MustCompile(`^\(Hardware Port:\s*([^,]+),\s*Device:\s*([^)]+)\)$`)
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	var services []macNetworkService
	for index := 0; index+1 < len(lines); index++ {
		match := header.FindStringSubmatch(strings.TrimSpace(lines[index]))
		if len(match) != 3 {
			continue
		}
		info := detail.FindStringSubmatch(strings.TrimSpace(lines[index+1]))
		if len(info) != 3 {
			continue
		}
		services = append(services, macNetworkService{
			Name:         strings.TrimSpace(match[2]),
			HardwarePort: strings.TrimSpace(info[1]),
			Device:       strings.TrimSpace(info[2]),
			Disabled:     match[1] == "*",
		})
	}
	return services
}

func isDJICellularService(service macNetworkService) bool {
	port := strings.ToLower(strings.TrimSpace(service.HardwarePort))
	name := strings.ToLower(strings.TrimSpace(service.Name))
	matchesBaiwang := strings.Contains(port, "baiwang") || strings.Contains(name, "baiwang")
	return matchesBaiwang && regexp.MustCompile(`^en\d+$`).MatchString(service.Device)
}

func setMacNetworkServiceEnabled(name string, enabled bool) error {
	value := "off"
	if enabled {
		value = "on"
	}
	out, err := exec.Command("/usr/sbin/networksetup", "-setnetworkserviceenabled", name, value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func renewMacNetworkServiceDHCP(name string) error {
	out, err := exec.Command("/usr/sbin/networksetup", "-setdhcp", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureCellularDHCP renews DHCP on the DJI cellular network service when its
// USB network interface has no usable IPv4 address. It runs after the USB AT
// bridge reopens so a re-enumerated module regains its 4G fallback route
// without user interaction.
func (a *app) ensureCellularDHCP() {
	if a.demo || a.modem != nil {
		return
	}
	a.networkRepairMu.Lock()
	defer a.networkRepairMu.Unlock()

	services, err := discoverMacNetworkServices()
	if err != nil {
		log.Printf("cellular DHCP repair: %v", err)
		return
	}
	// Find the DJI cellular network service. macOS may name it differently on
	// a fresh machine ("Baiwang", "Baiwang 2", localized variants), and it may
	// also be disabled, in which case DHCP renewals never apply.
	var target string
	for _, service := range services {
		if !isDJICellularService(service) {
			continue
		}
		if service.Disabled {
			log.Printf("cellular DHCP repair: enabling disabled DJI cellular service %s", service.Name)
			if err := setMacNetworkServiceEnabled(service.Name, true); err != nil {
				log.Printf("cellular DHCP repair: enable %s failed: %v", service.Name, err)
				continue
			}
		}
		target = service.Name
		break
	}
	// No service yet: on a machine that never saw this module, macOS may not
	// have created a network service for the modem's USB adapter. Create one
	// so DHCP can be renewed automatically.
	if target == "" {
		device := findUnprovisionedUSBModemInterface(services)
		if device == "" {
			log.Printf("cellular DHCP repair: no DJI cellular network service and no unprovisioned USB interface")
			return
		}
		var err error
		target, err = createCellularNetworkService(device)
		if err != nil {
			log.Printf("cellular DHCP repair: create network service on %s failed: %v", device, err)
			return
		}
		log.Printf("cellular DHCP repair: created network service %s on %s", target, device)
	}
	if _, err := readMacIPv4ServiceInfo(target); err == nil {
		return
	}
	const attempts = 2
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := renewMacNetworkServiceDHCP(target); err != nil {
			log.Printf("cellular DHCP repair: renew DHCP for %s failed: %v", target, err)
			return
		}
		info, waitErr := waitForMacIPv4Service(target, 15*time.Second)
		if waitErr == nil {
			log.Printf("cellular DHCP repair: %s -> %s", target, info.Address)
			return
		}
		log.Printf("cellular DHCP repair: attempt %d/2: %v", attempt, waitErr)
	}
	// DHCP keeps failing: report the modem USB networking mode so a fresh
	// machine can be diagnosed (usbnet=0 means the adapter never comes up).
	if a.usbAT != nil {
		if resp, err := a.usbAT.Command(`AT+QCFG="usbnet"`, 3*time.Second); err == nil {
			log.Printf("cellular DHCP repair: AT+QCFG usbnet => %s", strings.TrimSpace(resp))
		}
	}
}

// findUnprovisionedUSBModemInterface returns the en* interface that looks like
// the DJI modem's USB network adapter but has no macOS network service yet.
// USB cellular adapters typically use a locally administered (randomized) MAC
// address rather than a burned-in vendor OUI, which distinguishes them from
// built-in Ethernet and Wi-Fi.
func findUnprovisionedUSBModemInterface(services []macNetworkService) string {
	return selectUnprovisionedUSBInterface(discoverMacNetworkInterfaces(), services)
}

func selectUnprovisionedUSBInterface(interfaces []macNetInterface, services []macNetworkService) string {
	hasService := make(map[string]bool)
	for _, service := range services {
		hasService[service.Device] = true
	}
	for _, item := range interfaces {
		if item.Kind != "ethernet" || item.Name == "en0" || !isLocallyAdministeredMAC(item.MAC) {
			continue
		}
		if !hasService[item.Name] {
			return item.Name
		}
	}
	return ""
}

func isLocallyAdministeredMAC(mac string) bool {
	clean := strings.ReplaceAll(strings.TrimSpace(mac), "-", ":")
	parts := strings.Split(clean, ":")
	if len(parts) == 0 {
		return false
	}
	first, err := strconv.ParseUint(parts[0], 16, 8)
	if err != nil {
		return false
	}
	return first&0x02 != 0
}

func createCellularNetworkService(device string) (string, error) {
	for _, name := range []string{"Baiwang", "Baiwang 2", "DJI 4G"} {
		if err := createMacNetworkService(name, device); err != nil {
			continue
		}
		return name, nil
	}
	return "", fmt.Errorf("no available service name for %s", device)
}

func createMacNetworkService(name, device string) error {
	out, err := exec.Command("/usr/sbin/networksetup", "-createnetworkservice", name, device).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s on %s: %s", name, device, strings.TrimSpace(string(out)))
	}
	return nil
}

type macIPv4ServiceInfo struct {
	Address string
	Subnet  string
}

func parseMacIPv4ServiceInfo(output string) macIPv4ServiceInfo {
	var info macIPv4ServiceInfo
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "IP address":
			info.Address = strings.TrimSpace(value)
		case "Subnet mask":
			info.Subnet = strings.TrimSpace(value)
		}
	}
	return info
}

func readMacIPv4ServiceInfo(name string) (macIPv4ServiceInfo, error) {
	out, err := exec.Command("/usr/sbin/networksetup", "-getinfo", name).CombinedOutput()
	if err != nil {
		return macIPv4ServiceInfo{}, fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	info := parseMacIPv4ServiceInfo(string(out))
	address := net.ParseIP(info.Address)
	if address == nil || address.IsUnspecified() || address.IsLinkLocalUnicast() || net.ParseIP(info.Subnet) == nil {
		return macIPv4ServiceInfo{}, fmt.Errorf("%s: 4G 网卡尚未取得有效 IPv4 地址", name)
	}
	return info, nil
}

func waitForMacIPv4Service(name string, timeout time.Duration) (macIPv4ServiceInfo, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		info, err := readMacIPv4ServiceInfo(name)
		if err == nil {
			return info, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return macIPv4ServiceInfo{}, lastErr
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func blockMacNetworkServiceRoute(name string, info macIPv4ServiceInfo) error {
	// Keep the USB ECM interface and its local address alive, but point its
	// router at itself. macOS therefore cannot use it as an internet fallback.
	// Restoring DHCP later returns the real QDC507 gateway without disabling
	// the network service, which avoids dropping the ECM carrier.
	out, err := exec.Command(
		"/usr/sbin/networksetup",
		"-setmanual",
		name,
		info.Address,
		info.Subnet,
		info.Address,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *app) loadNetworkPolicyLocked() error {
	if a.networkPolicyLoaded {
		return nil
	}
	if a.networkPolicyPath == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		a.networkPolicyPath = filepath.Join(configDir, "DJOneHub", "network-policy.json")
	}
	var state cellularPolicyStatus
	data, err := os.ReadFile(a.networkPolicyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &state); err != nil {
			return err
		}
	}
	a.force4GOff = state.ForceOff
	a.disabled4GServices = append([]string(nil), state.Services...)
	a.networkPolicyLoaded = true
	return nil
}

func (a *app) persistNetworkPolicyLocked() error {
	if err := os.MkdirAll(filepath.Dir(a.networkPolicyPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cellularPolicyStatus{
		ForceOff: a.force4GOff,
		Services: a.disabled4GServices,
	}, "", "  ")
	if err != nil {
		return err
	}
	temporary := a.networkPolicyPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, a.networkPolicyPath)
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

func (a *app) applyCellularPolicyLocked() error {
	services, err := discoverMacNetworkServices()
	if err != nil {
		return err
	}
	if a.force4GOff {
		// Keep the ECM carrier online for SMS/calls, but remove its usable internet
		// gateway. This is deliberately stronger than changing service priority:
		// an automatic fallback may still select 4G, but it cannot send traffic
		// through a route whose gateway is the interface itself.
		for _, service := range services {
			if !isDJICellularService(service) {
				continue
			}
			info, err := readMacIPv4ServiceInfo(service.Name)
			if err != nil {
				// 网卡尚未取得有效 IP 时，使用标准 QDC 子网虚拟地址屏蔽网关路由
				info = macIPv4ServiceInfo{Address: "192.168.225.250", Subnet: "255.255.255.0"}
			}
			if err := blockMacNetworkServiceRoute(service.Name, info); err != nil {
				// 若设置手动路由失败，则直接尝试关闭该服务作为后备方案
				_ = setMacNetworkServiceEnabled(service.Name, false)
			}
			a.disabled4GServices = appendUnique(a.disabled4GServices, service.Name)
		}
		return a.persistNetworkPolicyLocked()
	}
	var restoreErrors []string
	for _, name := range a.disabled4GServices {
		_ = setMacNetworkServiceEnabled(name, true)
		if err := renewMacNetworkServiceDHCP(name); err != nil {
			log.Printf("restore DHCP for 4G network service %q: %v", name, err)
			restoreErrors = append(restoreErrors, err.Error())
		}
	}
	if len(restoreErrors) > 0 {
		if err := a.persistNetworkPolicyLocked(); err != nil {
			restoreErrors = append(restoreErrors, err.Error())
		}
		return errors.New(strings.Join(restoreErrors, "; "))
	}
	a.disabled4GServices = nil
	return a.persistNetworkPolicyLocked()
}

func (a *app) cellularPolicyStatus() (cellularPolicyStatus, error) {
	a.networkPolicyMu.Lock()
	defer a.networkPolicyMu.Unlock()
	if err := a.loadNetworkPolicyLocked(); err != nil {
		return cellularPolicyStatus{}, err
	}
	return cellularPolicyStatus{
		ForceOff: a.force4GOff,
		Services: append([]string(nil), a.disabled4GServices...),
	}, nil
}

func (a *app) getCellularPolicy(w http.ResponseWriter, _ *http.Request) {
	state, err := a.cellularPolicyStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *app) setCellularPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ForceOff bool `json:"force_off"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.networkPolicyMu.Lock()
	defer a.networkPolicyMu.Unlock()
	if err := a.loadNetworkPolicyLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	previousForceOff := a.force4GOff
	previousServices := append([]string(nil), a.disabled4GServices...)
	a.force4GOff = body.ForceOff
	if err := a.applyCellularPolicyLocked(); err != nil {
		a.force4GOff = previousForceOff
		a.disabled4GServices = previousServices
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cellularPolicyStatus{
		ForceOff: a.force4GOff,
		Services: append([]string(nil), a.disabled4GServices...),
	})
}

func (a *app) startCellularPolicyGuard(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 仅当连接了 DJI USB 设备且开启了关闭 4G 策略时，才低频检查是否有未处理的新网卡
			if a.currentUSBDevice() == nil {
				continue
			}
			a.networkPolicyMu.Lock()
			if err := a.loadNetworkPolicyLocked(); err == nil && a.force4GOff {
				// 仅在当前确实存在尚未禁用的 DJI 网卡时才调用 applyCellularPolicyLocked
				services, err := discoverMacNetworkServices()
				if err == nil {
					hasUnapplied := false
					for _, s := range services {
						if isDJICellularService(s) && !s.Disabled {
							isAlreadyDisabled := false
							for _, d := range a.disabled4GServices {
								if d == s.Name {
									isAlreadyDisabled = true
									break
								}
							}
							if !isAlreadyDisabled {
								hasUnapplied = true
								break
							}
						}
					}
					if hasUnapplied {
						if err := a.applyCellularPolicyLocked(); err != nil {
							log.Printf("enforce cellular force-off policy: %v", err)
						}
					}
				}
			}
			a.networkPolicyMu.Unlock()
		}
	}
}

func (a *app) setUSBNetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode int `json:"mode"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Mode < 0 || body.Mode > 3 {
		writeError(w, http.StatusBadRequest, "only usbnet mode 0, 1, 2 or 3 is allowed")
		return
	}
	command := fmt.Sprintf(`AT+QCFG="usbnet",%d`, body.Mode)
	response, err := a.runATCommand(command, 8*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":         body.Mode,
		"response":     response,
		"needs_reboot": true,
	})
}

func (a *app) rebootModule(w http.ResponseWriter, _ *http.Request) {
	response, err := a.runATCommand("AT+CFUN=1,1", 3*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": true,
		"response": response,
	})
}

func (a *app) readUSBProfile() (usbConfig, string, error) {
	response, err := a.runATCommand(`AT+QCFG="usbcfg"`, 8*time.Second)
	if err != nil {
		return usbConfig{}, "", err
	}
	config, err := parseUSBConfig(response)
	if err != nil {
		return usbConfig{}, "", err
	}
	return config, strings.TrimSpace(response), nil
}

func profileStatus(config usbConfig, raw string, needsReconnect bool, message string) usbProfileStatus {
	mode := "mac"
	if !config.uacEnabled() {
		mode = "mobile"
	}
	return usbProfileStatus{Mode: mode, UACEnabled: config.uacEnabled(), Configuration: raw, NeedsReconnect: needsReconnect, Message: message}
}

type usbProfileIntent struct {
	MobileArmed bool `json:"mobile_armed"`
}

func (a *app) loadUSBProfileIntentLocked() error {
	if a.usbProfileIntentLoaded {
		return nil
	}
	if a.usbProfileIntentPath == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		a.usbProfileIntentPath = filepath.Join(configDir, "DJOneHub", "usb-profile-intent.json")
	}
	var intent usbProfileIntent
	data, err := os.ReadFile(a.usbProfileIntentPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &intent); err != nil {
			return err
		}
	}
	a.usbProfileMobileArmed = intent.MobileArmed
	a.usbProfileIntentLoaded = true
	return nil
}

func (a *app) persistUSBProfileIntentLocked() error {
	if err := a.loadUSBProfileIntentLocked(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.usbProfileIntentPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(usbProfileIntent{MobileArmed: a.usbProfileMobileArmed}, "", "  ")
	if err != nil {
		return err
	}
	temporary := a.usbProfileIntentPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, a.usbProfileIntentPath)
}

func (a *app) usbProfile(w http.ResponseWriter, _ *http.Request) {
	config, raw, err := a.readUSBProfile()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, profileStatus(config, raw, false, ""))
}

// setUSBProfile changes only Quectel's documented UAC field. Mobile mode saves
// the configuration without rebooting; unplugging to iPhone/iPad applies it.
func (a *app) setUSBProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	mode := strings.ToLower(strings.TrimSpace(body.Mode))
	if mode != "mobile" && mode != "mac" {
		writeError(w, http.StatusBadRequest, "mode must be mobile or mac")
		return
	}
	a.usbProfileMu.Lock()
	defer a.usbProfileMu.Unlock()
	config, raw, err := a.readUSBProfile()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	wantUAC := mode == "mac"
	if config.uacEnabled() == wantUAC {
		a.usbProfileMobileArmed = mode == "mobile"
		if err := a.persistUSBProfileIntentLocked(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("保存连接模式意图失败: %v", err))
			return
		}
		message := "当前已是 Mac 完整模式"
		if mode == "mobile" {
			message = "当前已是 iPhone/iPad 模式；拔插到移动设备后生效"
		}
		writeJSON(w, http.StatusOK, profileStatus(config, raw, false, message))
		return
	}
	if mode == "mobile" {
		if _, err := a.runATCommand("AT+QPCMV=0", 5*time.Second); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("关闭当前语音流失败: %v", err))
			return
		}
	}
	if _, err := a.runATCommand(config.withUAC(wantUAC), 8*time.Second); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("写入 USB 配置失败: %v", err))
		return
	}
	updated := usbConfig{fields: append(append([]string(nil), config.fields[:8]...), map[bool]string{true: "1", false: "0"}[wantUAC])}
	if mode == "mobile" {
		a.usbProfileMobileArmed = true
		if err := a.persistUSBProfileIntentLocked(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("保存 iPhone/iPad 模式意图失败: %v", err))
			return
		}
		writeJSON(w, http.StatusAccepted, profileStatus(updated, raw, true, "已保存 iPhone/iPad 模式；现在直接拔出并连接移动设备即可"))
		return
	}
	a.usbProfileMobileArmed = false
	if err := a.persistUSBProfileIntentLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("保存 Mac 模式意图失败: %v", err))
		return
	}
	if _, err := a.runATCommand("AT+CFUN=1,1", 3*time.Second); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("已写入 Mac 模式，但模块重启失败: %v", err))
		return
	}
	writeJSON(w, http.StatusAccepted, profileStatus(updated, raw, true, "已恢复 Mac 完整模式，模块正在重新连接"))
}

func parseUSBNetMode(resp string) string {
	re := regexp.MustCompile(`\+QCFG:\s*"usbnet",(\d+)`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func parsePDPContexts(resp string) []pdpContext {
	re := regexp.MustCompile(`\+CGDCONT:\s*(\d+),"([^"]*)","([^"]*)"`)
	var contexts []pdpContext
	for _, match := range re.FindAllStringSubmatch(resp, -1) {
		id, _ := strconv.Atoi(match[1])
		contexts = append(contexts, pdpContext{ID: id, PDN: match[2], APN: match[3]})
	}
	return contexts
}

func parseActivePDPContexts(resp string) []int {
	re := regexp.MustCompile(`\+CGACT:\s*(\d+),1`)
	var contexts []int
	for _, match := range re.FindAllStringSubmatch(resp, -1) {
		id, _ := strconv.Atoi(match[1])
		contexts = append(contexts, id)
	}
	return contexts
}

func parsePDPAddresses(resp string) []string {
	re := regexp.MustCompile(`\+CGPADDR:\s*\d+,"([^"]*)"`)
	match := re.FindStringSubmatch(resp)
	if len(match) != 2 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(match[1], ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func discoverMacNetworkInterfaces() []macNetInterface {
	out, err := exec.Command("ifconfig").Output()
	if err != nil {
		return nil
	}
	var interfaces []macNetInterface
	for _, block := range splitIfconfigBlocks(string(out)) {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}
		fields := strings.Fields(lines[0])
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		if name == "" || strings.HasPrefix(name, "lo") || strings.HasPrefix(name, "utun") {
			continue
		}
		item := macNetInterface{Name: name, Status: "unknown", Kind: classifyMacInterfaceName(name)}
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "status:") {
				item.Status = strings.TrimSpace(strings.TrimPrefix(line, "status:"))
			}
			if strings.HasPrefix(line, "ether ") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					item.MAC = fields[1]
				}
			}
			if strings.HasPrefix(line, "inet ") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					item.IPv4 = fields[1]
				}
			}
		}
		interfaces = append(interfaces, item)
	}
	return interfaces
}

func discoverMacDefaultRoute() macDefaultRoute {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return macDefaultRoute{}
	}
	var route macDefaultRoute
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			route.Gateway = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
		if strings.HasPrefix(line, "interface:") {
			route.Interface = strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	return route
}

func splitIfconfigBlocks(out string) []string {
	var blocks []string
	var current []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if line[0] != '\t' && line[0] != ' ' && strings.Contains(line, ":") {
			if len(current) > 0 {
				blocks = append(blocks, strings.Join(current, "\n"))
			}
			current = []string{line}
			continue
		}
		if len(current) > 0 {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

func classifyMacInterfaceName(name string) string {
	switch {
	case strings.HasPrefix(name, "en"):
		return "ethernet"
	case strings.HasPrefix(name, "bridge"):
		return "bridge"
	case strings.HasPrefix(name, "awdl") || strings.HasPrefix(name, "llw") || strings.HasPrefix(name, "ap"):
		return "apple-wireless"
	default:
		return "other"
	}
}

func hasLikelyUSBNetworkInterface(interfaces []macNetInterface) bool {
	for _, item := range interfaces {
		if item.Kind == "ethernet" && item.Name != "en0" && item.Status == "active" {
			return true
		}
	}
	return false
}

func selectUSBTrafficInterface(interfaces []macNetInterface, route macDefaultRoute) string {
	for _, item := range interfaces {
		if item.Name == route.Interface && isUsableUSBTrafficInterface(item) {
			return item.Name
		}
	}
	for _, item := range interfaces {
		if isUsableUSBTrafficInterface(item) {
			return item.Name
		}
	}
	return ""
}

func isUsableUSBTrafficInterface(item macNetInterface) bool {
	if item.Kind != "ethernet" || item.Name == "en0" || item.Status != "active" {
		return false
	}
	address := net.ParseIP(item.IPv4)
	return address != nil && !address.IsUnspecified() && !address.IsLinkLocalUnicast()
}

func discoverMacInterfaceCounters() (map[string]networkByteCounters, error) {
	out, err := exec.Command("netstat", "-ibn").Output()
	if err != nil {
		return nil, err
	}
	return parseMacInterfaceCounters(string(out)), nil
}

func parseMacInterfaceCounters(out string) map[string]networkByteCounters {
	counters := make(map[string]networkByteCounters)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.HasPrefix(fields[2], "<Link#") {
			continue
		}
		name := strings.TrimSuffix(fields[0], "*")
		rx, rxErr := strconv.ParseUint(fields[6], 10, 64)
		tx, txErr := strconv.ParseUint(fields[9], 10, 64)
		if name == "" || rxErr != nil || txErr != nil {
			continue
		}
		counters[name] = networkByteCounters{RX: rx, TX: tx}
	}
	return counters
}

func (a *app) loadProfileNotesLocked() error {
	if a.profileNotesLoaded {
		return nil
	}
	path := a.profileNotesPath
	if path == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("locate profile notes directory: %w", err)
		}
		path = filepath.Join(configDir, "DJOneHub", "profile-notes.json")
		a.profileNotesPath = path
	}
	notes := make(map[string]profileNote)
	readPath := path
	if _, err := os.Stat(readPath); errors.Is(err, os.ErrNotExist) {
		legacyPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "VoHive macOS", "profile-notes.json")
		if _, legacyErr := os.Stat(legacyPath); legacyErr == nil {
			readPath = legacyPath
		}
	}
	data, err := os.ReadFile(readPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read profile notes: %w", err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &notes); err != nil {
			return fmt.Errorf("parse profile notes: %w", err)
		}
	}
	a.profileNotes = notes
	a.profileNotesLoaded = true
	return nil
}

func (a *app) persistProfileNotesLocked() error {
	if err := os.MkdirAll(filepath.Dir(a.profileNotesPath), 0o700); err != nil {
		return fmt.Errorf("create profile notes directory: %w", err)
	}
	data, err := json.MarshalIndent(a.profileNotes, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile notes: %w", err)
	}
	temporary := a.profileNotesPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write profile notes: %w", err)
	}
	if err := os.Rename(temporary, a.profileNotesPath); err != nil {
		return fmt.Errorf("replace profile notes: %w", err)
	}
	return nil
}

func (a *app) listESIMNotes(w http.ResponseWriter, _ *http.Request) {
	a.profileNotesMu.Lock()
	defer a.profileNotesMu.Unlock()
	if err := a.loadProfileNotesLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": a.profileNotes})
}

func (a *app) saveESIMNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ICCID string `json:"iccid"`
		Label string `json:"label"`
		Phone string `json:"phone"`
		Tags  string `json:"tags"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	body.Label = strings.TrimSpace(body.Label)
	body.Phone = strings.TrimSpace(body.Phone)
	body.Tags = strings.TrimSpace(body.Tags)
	if body.ICCID == "" {
		writeError(w, http.StatusBadRequest, "iccid is required")
		return
	}
	if len(body.Label) > 80 || len(body.Phone) > 80 || len(body.Tags) > 200 {
		writeError(w, http.StatusBadRequest, "本地备注字段过长")
		return
	}
	a.profileNotesMu.Lock()
	defer a.profileNotesMu.Unlock()
	if err := a.loadProfileNotesLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.Label == "" && body.Phone == "" && body.Tags == "" {
		delete(a.profileNotes, body.ICCID)
	} else {
		a.profileNotes[body.ICCID] = profileNote{Label: body.Label, Phone: body.Phone, Tags: body.Tags}
	}
	if err := a.persistProfileNotesLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "本地备注已保存", "note": a.profileNotes[body.ICCID]})
}

func atCommandSucceeded(response string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(response), "\r\n", "\n")
	return normalized == "OK" || strings.HasSuffix(normalized, "\nOK")
}

func (a *app) phonebookProbeCommand(command string, result *phonebookProbeResult) bool {
	response, err := a.runATCommand(command, 6*time.Second)
	if err != nil {
		result.Responses[command] = err.Error()
		return false
	}
	result.Responses[command] = strings.TrimSpace(response)
	return atCommandSucceeded(response)
}

// probeESIMPhonebook performs only AT test/read commands. It never writes a
// phonebook entry, so it is safe to use before enabling portable card notes.
func (a *app) probeESIMPhonebook(w http.ResponseWriter, _ *http.Request) {
	result := phonebookProbeResult{Responses: make(map[string]string)}
	if a.demo {
		result.StorageSupported = true
		result.StorageSelected = true
		result.ReadSupported = true
		result.WriteSupported = true
		result.StorageStatus = `+CPBS: "SM",0,250`
		result.Responses[`AT+CPBS=?`] = `+CPBS: ("SM","ME")\r\nOK`
		result.Responses[`AT+CPBR=?`] = `+CPBR: (1-250),40,14\r\nOK`
		result.Responses[`AT+CPBW=?`] = `+CPBW: (1-250),40,(129,145),16\r\nOK`
		writeJSON(w, http.StatusOK, result)
		return
	}

	if !a.phonebookProbeCommand(`AT+CPBS=?`, &result) {
		writeJSON(w, http.StatusOK, result)
		return
	}
	result.StorageSupported = strings.Contains(strings.ToUpper(result.Responses[`AT+CPBS=?`]), `"SM"`)
	if !result.StorageSupported {
		writeJSON(w, http.StatusOK, result)
		return
	}
	result.StorageSelected = a.phonebookProbeCommand(`AT+CPBS="SM"`, &result)
	if !result.StorageSelected {
		writeJSON(w, http.StatusOK, result)
		return
	}
	if a.phonebookProbeCommand(`AT+CPBS?`, &result) {
		result.StorageStatus = result.Responses[`AT+CPBS?`]
	}
	result.ReadSupported = a.phonebookProbeCommand(`AT+CPBR=?`, &result)
	result.WriteSupported = a.phonebookProbeCommand(`AT+CPBW=?`, &result)
	writeJSON(w, http.StatusOK, result)
}

const moduleNotePrefix = "VH1|"

func encodeModuleProfileNote(note moduleProfileNote) (string, error) {
	note.ICCID = strings.TrimSpace(note.ICCID)
	note.Label = strings.TrimSpace(note.Label)
	note.Phone = strings.TrimSpace(note.Phone)
	note.Tags = strings.TrimSpace(note.Tags)
	if note.ICCID == "" {
		return "", errors.New("iccid is required")
	}
	if len(note.Label) > 48 || len(note.Phone) > 40 || len(note.Tags) > 48 {
		return "", errors.New("模块资料名称、手机号或标签过长")
	}
	encode := func(value string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	encoded := strings.Join([]string{moduleNotePrefix[:len(moduleNotePrefix)-1], note.ICCID, encode(note.Label), encode(note.Phone), encode(note.Tags)}, "|")
	if len(encoded) > 255 {
		return "", errors.New("模块通讯录记录超过容量")
	}
	return encoded, nil
}

func decodeModuleProfileNote(index int, text string) (moduleProfileNote, bool) {
	parts := strings.Split(text, "|")
	if len(parts) != 5 || parts[0] != strings.TrimSuffix(moduleNotePrefix, "|") || strings.TrimSpace(parts[1]) == "" {
		return moduleProfileNote{}, false
	}
	decode := func(value string) (string, bool) {
		data, err := base64.RawURLEncoding.DecodeString(value)
		return string(data), err == nil
	}
	label, labelOK := decode(parts[2])
	phone, phoneOK := decode(parts[3])
	tags, tagsOK := decode(parts[4])
	if !labelOK || !phoneOK || !tagsOK {
		return moduleProfileNote{}, false
	}
	return moduleProfileNote{Index: index, ICCID: parts[1], Label: label, Phone: phone, Tags: tags}, true
}

func (a *app) runATOK(command string, timeout time.Duration) (string, error) {
	response, err := a.runATCommand(command, timeout)
	if err != nil {
		return "", err
	}
	if !atCommandSucceeded(response) {
		return "", fmt.Errorf("%s: %s", command, strings.TrimSpace(response))
	}
	return response, nil
}

func parseMEPhonebookStatus(response string) (used, total int, err error) {
	re := regexp.MustCompile(`\+CPBS:\s*"ME",(\d+),(\d+)`)
	match := re.FindStringSubmatch(response)
	if len(match) != 3 {
		return 0, 0, errors.New("ME 通讯录容量未返回")
	}
	used, err = strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, err
	}
	total, err = strconv.Atoi(match[2])
	return used, total, err
}

func parseMEPhonebookEntries(response string) []modulePhonebookEntry {
	re := regexp.MustCompile(`(?m)\+CPBR:\s*(\d+),"([^"]*)",\d+,"([^"]*)"`)
	entries := make([]modulePhonebookEntry, 0)
	for _, match := range re.FindAllStringSubmatch(response, -1) {
		index, err := strconv.Atoi(match[1])
		if err == nil {
			entries = append(entries, modulePhonebookEntry{Index: index, Number: match[2], Text: match[3]})
		}
	}
	return entries
}

func (a *app) readModuleESIMNotes() (map[string]moduleProfileNote, map[int]bool, int, int, error) {
	if _, err := a.runATOK(`AT+CPBS="ME"`, 6*time.Second); err != nil {
		return nil, nil, 0, 0, err
	}
	status, err := a.runATOK(`AT+CPBS?`, 6*time.Second)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	used, total, err := parseMEPhonebookStatus(status)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	notes := make(map[string]moduleProfileNote)
	occupied := make(map[int]bool)
	if used == 0 {
		return notes, occupied, used, total, nil
	}
	response, err := a.runATOK(fmt.Sprintf("AT+CPBR=1,%d", total), 20*time.Second)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	for _, entry := range parseMEPhonebookEntries(response) {
		occupied[entry.Index] = true
		if note, ok := decodeModuleProfileNote(entry.Index, entry.Text); ok {
			notes[note.ICCID] = note
		}
	}
	return notes, occupied, used, total, nil
}

func (a *app) listModuleESIMNotes(w http.ResponseWriter, _ *http.Request) {
	a.moduleNotesMu.Lock()
	defer a.moduleNotesMu.Unlock()
	notes, _, used, total, err := a.readModuleESIMNotes()
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("读取模块资料库失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes, "used": used, "total": total})
}

func (a *app) saveModuleESIMNote(w http.ResponseWriter, r *http.Request) {
	var body moduleProfileNote
	if !decodeJSON(w, r, &body) {
		return
	}
	a.moduleNotesMu.Lock()
	defer a.moduleNotesMu.Unlock()
	notes, occupied, _, total, err := a.readModuleESIMNotes()
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("读取模块资料库失败: %v", err))
		return
	}
	current, exists := notes[strings.TrimSpace(body.ICCID)]
	if strings.TrimSpace(body.Label) == "" && strings.TrimSpace(body.Phone) == "" && strings.TrimSpace(body.Tags) == "" {
		if !exists {
			writeJSON(w, http.StatusOK, map[string]string{"message": "模块资料库中没有此记录"})
			return
		}
		if _, err := a.runATOK(fmt.Sprintf("AT+CPBW=%d", current.Index), 8*time.Second); err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("删除模块资料失败: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "模块资料已删除"})
		return
	}
	encoded, err := encodeModuleProfileNote(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	index := current.Index
	if !exists {
		for candidate := 1; candidate <= total; candidate++ {
			if !occupied[candidate] {
				index = candidate
				break
			}
		}
	}
	if index == 0 {
		writeError(w, http.StatusConflict, "模块通讯录已满")
		return
	}
	command := fmt.Sprintf(`AT+CPBW=%d,"00000000000",129,"%s"`, index, encoded)
	if _, err := a.runATOK(command, 8*time.Second); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("写入模块资料失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "模块资料已保存", "index": index})
}

func (a *app) esimOverview(w http.ResponseWriter, _ *http.Request) {
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]any{
			"chip_info": map[string]any{
				"sku_name":      "eUICC Demo Card",
				"serial_number": "DEMO-001",
				"firmware":      "1.0.0",
				"eids": []map[string]any{{
					"aid": "A0000005591010FFFFFFFF8900000100",
					"eid": "89049032000000000000000000000001",
				}},
			},
			"profiles": []map[string]any{{
				"eid":     "89049032000000000000000000000001",
				"aid_hex": "A0000005591010FFFFFFFF8900000100",
				"profiles": []map[string]any{
					{
						"iccid": "89860123456789012345", "name": "中国移动",
						"service_provider_name": "China Mobile", "state": 1, "state_text": "enabled",
					},
					{
						"iccid": "8944100000000000001", "name": "英国旅行卡",
						"service_provider_name": "giffgaff UK", "state": 0, "state_text": "disabled",
					},
					{
						"iccid": "8949020000000000002", "name": "欧洲数据卡",
						"service_provider_name": "Travel Europe", "state": 0, "state_text": "disabled",
					},
				},
			}},
		})
		return
	}
	esimManager, _ := a.currentESIMManager()
	if esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	overview, err := esimManager.GetEsimOverview()
	if err != nil {
		if isPhysicalSIMESIMProbeError(err) {
			writeJSON(w, http.StatusOK, map[string]any{
				"card_type": "physical_sim",
				"message":   "当前卡片为实体卡，非 eSIM 卡片",
			})
			return
		}
		log.Printf("eSIM overview failed: %v", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

// A normal physical SIM cannot open the GSMA eUICC management AIDs. The
// manager reports that as no eUICC discovered with an AT+CCHO ERROR; expose it
// as a neutral card type instead of leaking an implementation error to the UI.
func isPhysicalSIMESIMProbeError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "未发现任何 euicc") &&
		strings.Contains(message, "at+ccho") &&
		strings.Contains(message, "error")
}

func (a *app) esimHealth(w http.ResponseWriter, _ *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	overview, err := esimManager.GetEsimOverview()
	if err != nil {
		if isPhysicalSIMESIMProbeError(err) {
			writeJSON(w, http.StatusOK, map[string]any{"card_type": "physical_sim"})
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	var active *esim.ProfileItem
	for _, group := range overview.Profiles {
		for index := range group.Profiles {
			if group.Profiles[index].State == 1 {
				active = &group.Profiles[index]
				break
			}
		}
		if active != nil {
			break
		}
	}
	if active == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "eSIM 卡片已识别，但没有已启用的 Profile"})
		return
	}

	if err := a.ensureUSBAT(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	status, err := a.usbATStatus()
	if err != nil {
		a.resetUSBATIfGone(err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	registered := status.RegStatus == 1 || status.RegStatus == 5
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             status.SimInserted && registered,
		"active_profile": active,
		"module_iccid":   status.ICCID,
		"imsi":           status.IMSI,
		"operator":       status.Operator,
		"registration":   status.RegStatusText,
		"registered":     registered,
		"signal_dbm":     status.SignalDBM,
		"network_mode":   status.NetworkMode,
	})
}

func (a *app) switchESIM(w http.ResponseWriter, r *http.Request) {
	esimManager, switchAllowed := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ICCID) == "" {
		writeError(w, http.StatusBadRequest, "iccid is required")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]any{
			"switch_accepted": true,
			"phase":           "done",
			"target_iccid":    body.ICCID,
		})
		return
	}
	if !switchAllowed && a.modem == nil {
		writeError(w, http.StatusServiceUnavailable, "USB AT eSIM/卡片当前暂不允许切换 Profile")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	result, err := esimManager.SwitchProfileWithResult(ctx, body.ICCID, body.AID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Enabling a Profile resets the eUICC, but the DJI modem can keep the
	// previous SIM session (and therefore its CNUM) until its own firmware is
	// restarted.  Reload it here so the newly enabled Profile actually becomes
	// the modem's active subscriber identity.
	rebootResponse, rebootErr := a.runATCommand("AT+CFUN=1,1", 3*time.Second)
	rebootRequested := rebootErr == nil
	rebootWarning := ""
	if rebootErr != nil {
		// A USB disconnect immediately after CFUN is expected on some firmware;
		// the command may already have been accepted before the bridge drops.
		upper := strings.ToUpper(rebootErr.Error())
		if strings.Contains(upper, "NO_DEVICE") || strings.Contains(upper, "NOT_FOUND") {
			rebootRequested = true
		} else {
			rebootWarning = rebootErr.Error()
			log.Printf("eSIM profile switched but module restart was not confirmed: %v", rebootErr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"switch_accepted":         result.SwitchAccepted,
		"phase":                   result.Phase,
		"target_iccid":            result.TargetICCID,
		"recovery_pending":        result.RecoveryPending,
		"module_reboot_requested": rebootRequested,
		"module_reboot_response":  rebootResponse,
		"module_reboot_warning":   rebootWarning,
		"reconnect_wait_seconds":  10,
	})
}

func (a *app) renameESIMProfile(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
		Name  string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	body.Name = strings.TrimSpace(body.Name)
	if body.ICCID == "" || body.Name == "" {
		writeError(w, http.StatusBadRequest, "iccid and name are required")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 名称修改成功"})
		return
	}
	if err := esimManager.RenameProfile(body.ICCID, body.Name, body.AID); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("修改 Profile 名称失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 名称修改成功"})
}

func (a *app) deleteESIMProfile(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	if body.ICCID == "" {
		writeError(w, http.StatusBadRequest, "iccid is required")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 已删除"})
		return
	}
	result, err := esimManager.DeleteProfile(body.ICCID, body.AID)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("删除 Profile 失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) downloadESIMProfile(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		SMDP             string `json:"smdp"`
		MatchingID       string `json:"matching_id"`
		ConfirmationCode string `json:"confirmation_code"`
		AID              string `json:"aid"`
		IMEI             string `json:"imei"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.SMDP = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(body.SMDP, "https://"), "http://"))
	if body.SMDP == "" {
		writeError(w, http.StatusBadRequest, "smdp is required")
		return
	}
	if strings.TrimSpace(body.IMEI) == "" {
		writeError(w, http.StatusBadRequest, "imei is required for USB AT eSIM download")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": "演示：Profile 下载完成"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	result, err := esimManager.DownloadProfile(ctx, body.AID, body.SMDP, body.MatchingID, body.ConfirmationCode, body.IMEI, func(event esim.DownloadProgressEvent) {
		log.Printf("eSIM download %d%% %s", event.Pct, event.Msg)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("下载 Profile 失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
