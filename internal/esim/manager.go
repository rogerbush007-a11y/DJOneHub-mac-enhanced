package esim

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/damonto/euicc-go/bertlv"
	"github.com/damonto/euicc-go/driver"
	"github.com/damonto/euicc-go/lpa"
	sgp22 "github.com/damonto/euicc-go/v2"
	"github.com/iniwex5/vohive/internal/apduarbiter"
	backendpkg "github.com/iniwex5/vohive/internal/backend"
	"github.com/iniwex5/vohive/internal/modem"
	"github.com/iniwex5/vohive/pkg/logger"
)

// 支持的 ISD-R AID 列表
var AIDs = [][]byte{
	// eSTK.me Max 专用 SE0/SE1 AID
	{0xA0, 0x65, 0x73, 0x74, 0x6B, 0x6D, 0x65, 0xFF, 0xFF, 0x49, 0x53, 0x44, 0x2D, 0x52, 0x20, 0x30}, // eSTK.me SE0
	{0xA0, 0x65, 0x73, 0x74, 0x6B, 0x6D, 0x65, 0xFF, 0xFF, 0x49, 0x53, 0x44, 0x2D, 0x52, 0x20, 0x31}, // eSTK.me SE1
	// 通用 AID
	lpa.GSMAISDRApplicationAID, // 标准 GSMA ISD-R
	{0xA0, 0x00, 0x00, 0x05, 0x59, 0x10, 0x10, 0x00, 0x00, 0x00, 0x00, 0x89, 0x00, 0x00, 0x03, 0x00}, // eSIM.me
	{0xA0, 0x00, 0x00, 0x05, 0x59, 0x10, 0x10, 0x00, 0x00, 0x00, 0x89, 0x00, 0x00, 0x00, 0x03, 0x00}, // eSIM.me V2
	{0xA0, 0x00, 0x00, 0x05, 0x59, 0x10, 0x10, 0xFF, 0xFF, 0xFF, 0xFF, 0x89, 0x00, 0x05, 0x05, 0x00}, // 5ber.eSIM
	{0xA0, 0x00, 0x00, 0x05, 0x59, 0x10, 0x10, 0xFF, 0xFF, 0xFF, 0xFF, 0x89, 0x00, 0x00, 0x01, 0x77}, // XeSIM
	{0xA0, 0x00, 0x00, 0x05, 0x59, 0x10, 0x4C, 0x69, 0x6E, 0x6B, 0x73, 0x66, 0x69, 0x65, 0x6C, 0x64}, // LinksField
	{0xA0, 0x00, 0x00, 0x06, 0x28, 0x10, 0x10, 0xFF, 0xFF, 0xFF, 0xFF, 0x89, 0x00, 0x00, 0x01, 0x00}, // GlocalMe
	// eSTK.me 旧版 AUX AID（兼容老固件）
	{0xA0, 0x65, 0x73, 0x74, 0x6B, 0x6D, 0x65, 0xFF, 0xFF, 0xFF, 0xFF, 0x49, 0x53, 0x44, 0x2D, 0x52}, // eSTK.me AUX (deprecated)
}

// eSTK.me AID 共有前缀(7 bytes): A0 65 73 74 6B 6D 65
var estkmeAIDPrefix = []byte{0xA0, 0x65, 0x73, 0x74, 0x6B, 0x6D, 0x65}

var aidOrderRank = func() map[string]int {
	rank := make(map[string]int, len(AIDs))
	for i, aid := range AIDs {
		rank[strings.ToUpper(hex.EncodeToString(aid))] = i
	}
	return rank
}()

// isESTKmeAID 判断 AID 是否属于 eSTK.me 系列
func isESTKmeAID(aid []byte) bool {
	return len(aid) >= len(estkmeAIDPrefix) && bytes.Equal(aid[:len(estkmeAIDPrefix)], estkmeAIDPrefix)
}

// predictSkuName 尝试根据 EID 前缀和固件版本预测卡片的品牌名称（如 9eSIM）
// 参考自 OpenEUICC 源码逻辑
func predictSkuName(eid string, firmware string) string {
	if eid == "" || firmware == "" {
		return ""
	}

	// 判断是否为 9eSIM 典型的 EID 前缀 (890440458467274948 / 890440452167274948等)
	if strings.HasPrefix(eid, "890440458467274948") || strings.HasPrefix(eid, "890440452167274948") {
		// 简单解析固件版本
		parts := strings.Split(firmware, ".")
		if len(parts) == 3 {
			v1, _ := strconv.Atoi(parts[0])
			v2, _ := strconv.Atoi(parts[1])
			v3, _ := strconv.Atoi(parts[2])

			// 粗略判断版本对应名称（简化自 OpenEUICC 的版本阈值）
			var verName string
			if v1 > 37 || (v1 == 37 && v2 >= 4) {
				verName = "v3.2"
			} else if v1 == 37 && v2 == 1 && v3 >= 41 {
				verName = "v3.1"
			} else if v1 == 36 && v2 >= 18 {
				verName = "v3"
			} else if v1 == 36 && v2 >= 17 && v3 >= 39 {
				verName = "v3 (beta)"
			} else if v1 == 36 && v2 == 17 && v3 >= 4 {
				verName = "v2s"
			} else if v1 == 36 && v2 >= 9 {
				verName = "v2.1"
			} else if v1 == 36 && v2 >= 7 {
				verName = "v2"
			} else if v1 < 36 {
				// 兼容可能的新规则或者我们自己测试提取的 25.x 固件
				verName = fmt.Sprintf("v%d", v1)
			}

			if verName != "" {
				return "9eSIM " + verName
			}
		}
		return "9eSIM"
	}

	return ""
}

// shouldContinueAIDScanAfterSuccess 判断在已经发现可用 AID 后是否还需要继续扫描。
// 普通 eUICC 命中首个可用 AID 后即可停止；eSTK Max 可能同时暴露 SE0/SE1，
// 因此命中 eSTK AID 后仍继续探测后续 eSTK AID。
func shouldContinueAIDScanAfterSuccess(successAIDs [][]byte, nextAID []byte) bool {
	if len(successAIDs) == 0 {
		return true
	}
	for _, a := range successAIDs {
		if isESTKmeAID(a) && isESTKmeAID(nextAID) {
			return true
		}
	}
	return false
}

func getAIDRank(aidHex string) int {
	key := strings.ToUpper(strings.TrimSpace(aidHex))
	if v, ok := aidOrderRank[key]; ok {
		return v
	}
	return 1 << 30
}

func sortEUICCProfilesStable(groups []EUICCProfiles) {
	sort.SliceStable(groups, func(i, j int) bool {
		ri := getAIDRank(groups[i].AIDHex)
		rj := getAIDRank(groups[j].AIDHex)
		if ri != rj {
			return ri < rj
		}
		if groups[i].EID != groups[j].EID {
			return groups[i].EID < groups[j].EID
		}
		return strings.ToUpper(groups[i].AIDHex) < strings.ToUpper(groups[j].AIDHex)
	})
}

func sortEUICCInfosStable(infos []EUICCInfo) {
	sort.SliceStable(infos, func(i, j int) bool {
		ri := getAIDRank(infos[i].AIDHex)
		rj := getAIDRank(infos[j].AIDHex)
		if ri != rj {
			return ri < rj
		}
		if infos[i].EID != infos[j].EID {
			return infos[i].EID < infos[j].EID
		}
		return strings.ToUpper(infos[i].AIDHex) < strings.ToUpper(infos[j].AIDHex)
	})
}

func hasReusableChipProductInfo(info *EUICCChipInfo) bool {
	if info == nil {
		return false
	}
	// 仅 SkuName / SerialNumber 来自 eSTK.me Product AID 查询（或 SkuName 由 predictSkuName 推断），
	// 可作为"已获取产品信息"的判据。
	// Firmware 不能作为判据：当 eSTK.me Product AID 查询失败时（如逻辑通道资源耗尽），
	// info.Firmware 会被标准 EUICCInfo2 的固件版本兜底填充。若据此判定缓存可复用，
	// 会永久跳过 parseESTKmeInfo，导致 SkuName/SerialNumber 永远查询不到。
	return strings.TrimSpace(info.SkuName) != "" ||
		strings.TrimSpace(info.SerialNumber) != ""
}

type EUICCSpec string

const (
	EUICCSpecUnknown EUICCSpec = ""
	EUICCSpecSGP22   EUICCSpec = "sgp22"
	EUICCSpecSGP32   EUICCSpec = "sgp32"
	EUICCSpecSGP02   EUICCSpec = "sgp02"
)

// SGP.32 and SGP.02 labels are reserved for future read-only probes. Current consumer LPA-compatible ISD-R sessions are marked SGP.22 unless a later probe proves otherwise.

const (
	euiccSpecGuessSGP22Compat   = "sgp22_compatible"
	euiccSpecConfidenceInferred = "inferred"
	aidScanPolicyFullStatic     = aidScanPolicy("full_static")
)

type aidScanPolicy string

type aidScanPlan struct {
	Policy aidScanPolicy
	AIDs   [][]byte
}

func (p aidScanPlan) CloneAIDs() [][]byte {
	return cloneAIDList(p.AIDs)
}

// EUICCInfo 单个 eUICC 的信息
type EUICCInfo struct {
	AID                    []byte    `json:"-"`
	AIDHex                 string    `json:"aid"`
	EID                    string    `json:"eid"`
	Spec                   EUICCSpec `json:"spec,omitempty"`
	SpecGuess              string    `json:"spec_guess,omitempty"`
	SpecConfidence         string    `json:"spec_confidence,omitempty"`
	FreeNvramBytes         int32     `json:"free_nvram_bytes"`       // 可用 NV 存储（字节）
	FreeNvram              string    `json:"free_nvram"`             // 可用 NV 存储（格式化）
	Firmware               string    `json:"firmware,omitempty"`     // 提取自 EUICCInfo2 / EUICCInfo1
	Manufacturer           string    `json:"manufacturer,omitempty"` // 芯片制造商（基于 EID 和 PKI 数据查询）
	Certificates           []string  `json:"certificates,omitempty"` // 支持的证书签发机构列表
	InfoSource             string    `json:"info_source,omitempty"`  // euicc_info2 / euicc_info1
	InfoVersion            string    `json:"info_version,omitempty"` // 1 / 2
	InfoError              string    `json:"info_error,omitempty"`   // 标准信息读取失败时的诊断信息
	SASAccreditationNumber string    `json:"sas_accreditation_number,omitempty"`
	DefaultSMDPAddress     string    `json:"default_smdp_address,omitempty"`
	RootSMDSAddress        string    `json:"root_ds_address,omitempty"`
}

func buildDiscoveredEUICCInfo(aid []byte, eidStr string) EUICCInfo {
	info := EUICCInfo{
		AID:            append([]byte(nil), aid...),
		AIDHex:         fmt.Sprintf("%X", aid),
		EID:            eidStr,
		SpecGuess:      euiccSpecGuessSGP22Compat,
		SpecConfidence: euiccSpecConfidenceInferred,
	}
	return info
}

// ProfileItem 单个 profile 信息
type ProfileItem struct {
	ICCID               string `json:"iccid"`
	Name                string `json:"name"`
	ServiceProviderName string `json:"service_provider_name"`
	State               int    `json:"state"` // 0=disabled, 1=enabled
	StateText           string `json:"state_text"`
	ClassText           string `json:"class_text,omitempty"`
}

// EUICCProfiles 按 eUICC 分组的 profile 列表
type EUICCProfiles struct {
	EID      string        `json:"eid"`
	AIDHex   string        `json:"aid_hex"`
	Profiles []ProfileItem `json:"profiles"`
}

// EUICCChipInfo eUICC 芯片/卡的硬件信息
type EUICCChipInfo struct {
	EIDs         []EUICCInfo `json:"eids"`                    // 所有 EID 列表（含各自可用空间）
	SkuName      string      `json:"sku_name,omitempty"`      // 产品名称（如 "ESTKme Max"）
	SerialNumber string      `json:"serial_number,omitempty"` // 序列号（如 "T3VAMD0"）
	Firmware     string      `json:"firmware,omitempty"`      // 固件版本
}

// eSTK.me Product AID，用于查询设备名称、序列号和固件版本
var estkmeProductAID = []byte{0xA0, 0x65, 0x73, 0x74, 0x6B, 0x6D, 0x65, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x6D, 0x67, 0x74}

// Manager eSIM profile 管理器（支持多 eUICC）
type Manager struct {
	// modem 是基于 AT 命令的通道工厂（Modem 模式用），PC/SC 模式下为 nil
	modem         *modem.Manager
	backend       backendpkg.DeviceBackend
	deviceID      string
	transport     string
	controlDevice string
	imeiProvider  func(ctx context.Context) (string, error)

	// channelFactory 是通道工厂函数，不同模式下注入不同实现
	// Modem 模式：基于 AT 命令
	// PC/SC 模式：基于 scard.Card APDU 透传
	channelFactory          func(aid []byte) (*lpa.Client, error)
	smartCardChannelFactory func() (driver.SmartCardChannel, error)
	closeClient             func(client *lpa.Client) error

	// clearChannels 是在首轮 AID 扫描前清理逐辑通道的回调（可选）
	clearChannels func()

	cacheMu                     sync.RWMutex   // 保护 chipInfoCache、overviewCache 与 discoveredEUICCs 等快照状态
	opMu                        sync.Mutex     // eSIM 硬件操作互斥（同时只允许一个写操作）
	opDone                      chan struct{}  // 写操作完成通知（替代 TryLock+Sleep 轮询）
	chipInfoCache               *EUICCChipInfo // 芯片信息缓存（硬件信息基本不变）
	overviewCache               *EsimOverview  // eSIM 总览缓存（跟随 Manager / Worker 实例）
	overviewLastErr             error
	overviewReloading           bool
	overviewGeneration          uint64
	overviewLoader              func() (*EsimOverview, error)
	profilesLoader              func() ([]EUICCProfiles, error)
	suppressOverviewReloadUntil time.Time
	discoveredEUICCs            []EUICCInfo
	sf                          *singleflight.Group

	onBeforeSwitch       func(SwitchOperation, string) uint64              // 切卡前执行的回调，返回本次 switch token
	onAfterSwitch        func(SwitchOperation, uint64)                     // 切卡后网络就绪后执行的回调
	onSwitchFailed       func(SwitchOperation, uint64, error)              // 切卡失败后执行的回调
	onSwitchDegraded     func(SwitchOperation, uint64, SwitchPhase, error) // 切卡已接受但后处理降级回调
	onSwitchPhase        func(SwitchOperation, uint64, SwitchPhase)        // 切卡内部阶段变更回调
	switchSignal         chan string
	switchUseRefreshTrue bool

	apduArbiter          apduIdleWaiter
	postSwitchMinDelay   time.Duration
	readQueueWaitTimeout time.Duration

	// downloadCtx 是当前正在进行的下载操作的 context。
	// 如果不为 nil，由 smartCardChannelFactory 新建的 QMIChannel 会自动继承该 context，
	// 从而允许 BPP 安装阶段的长时延迟得到正确处理而不被默认超时中断。
	downloadCtx atomic.Pointer[context.Context]
}

// ErrOperationInProgress 表示当前有写操作（下载/切换/删除）正在进行中
// 读操作（GetProfiles / GetEsimOverview）在检测到此情况时立即降级，不进入 SIM 卡通道
var ErrOperationInProgress = fmt.Errorf("eSIM 操作进行中，请稍后重试")

type ManagerOptions struct {
	DeviceID                string
	Transport               string
	Modem                   *modem.Manager
	Backend                 backendpkg.DeviceBackend
	QMITransport            QMIAPDUTransport
	SmartCardChannelFactory func() (driver.SmartCardChannel, error)
	IMEIProvider            func(ctx context.Context) (string, error)
	OnBeforeSwitch          func(SwitchOperation, string) uint64
	OnAfterSwitch           func(SwitchOperation, uint64)
	OnSwitchFailed          func(SwitchOperation, uint64, error)
	OnSwitchDegraded        func(SwitchOperation, uint64, SwitchPhase, error)
	OnSwitchPhase           func(SwitchOperation, uint64, SwitchPhase)
	APDUArbiter             *apduarbiter.Arbiter
	PostSwitchMinDelay      time.Duration
	SwitchUseRefreshTrue    bool
}

type SwitchOperation string

const (
	SwitchOperationEnableProfile  SwitchOperation = "enable_profile"
	SwitchOperationDisableProfile SwitchOperation = "disable_profile"
)

type SwitchPhase string

const (
	SwitchPhasePrepare             SwitchPhase = "prepare"
	SwitchPhaseAPDUSwitching       SwitchPhase = "apdu_switching"
	SwitchPhaseCardResetSettling   SwitchPhase = "card_reset_settling"
	SwitchPhaseIdentityRefresh     SwitchPhase = "identity_refresh"
	SwitchPhaseRuntimeRestore      SwitchPhase = "runtime_restore"
	SwitchPhaseTransportRecovering SwitchPhase = "transport_recovering"
	SwitchPhaseDegraded            SwitchPhase = "degraded"
	SwitchPhaseDone                SwitchPhase = "done"
	SwitchPhaseFailed              SwitchPhase = "failed"
)

type SwitchProfileResult struct {
	SwitchToken       uint64
	Phase             SwitchPhase
	SwitchAccepted    bool
	RecoveryPending   bool
	DegradedReason    string
	PostSwitchAsync   bool
	SIMReloadWarning  string
	TargetICCID       string
	CachePatched      bool
	PowerCycleAttempt bool
}

const (
	transportAT     = "at"
	transportQMI    = "qmi"
	transportMBIM   = "mbim"
	transportCustom = "custom"
)

const (
	defaultSIMSlot                  uint8 = 1
	defaultPostSwitchMinDelay             = time.Second
	defaultReadQueueWaitTimeout           = 5 * time.Second
	writeOperationWarnThreshold           = 30 * time.Second
	postDownloadOverviewSettleDelay       = 2 * time.Second
)

var (
	switchFallbackPowerTimeout   = 20 * time.Second
	switchFallbackPowerCycleWait = 500 * time.Millisecond
)

// basicProfileTags 过滤掉耗时的 TagProfileIcon 数据（PNG等图片数据），
// 避免 ListProfile 发送几十条 APDU 以及长时间的数据传输。
var basicProfileTags = []bertlv.Tag{
	sgp22.TagICCID,
	sgp22.TagProfileState,
	sgp22.TagNickname,
	sgp22.TagServiceProviderName,
	sgp22.TagProfileName,
	sgp22.TagProfileClass,
}

func listBasicProfiles(client *lpa.Client) ([]*sgp22.ProfileInfo, error) {
	if client == nil || client.APDU == nil {
		return nil, fmt.Errorf("未配置 eUICC APDU 通道")
	}
	response, err := sgp22.InvokeAPDU(client.APDU, &sgp22.ProfileInfoListRequest{
		Tags: basicProfileTags,
	})
	if err != nil {
		return nil, err
	}
	return response.ProfileList, nil
}

type simPowerController interface {
	UIMPowerOffSIM(ctx context.Context, slot uint8) error
	UIMPowerOnSIM(ctx context.Context, slot uint8) error
}

type apduIdleWaiter interface {
	WaitIdle(ctx context.Context) error
}

func normalizeTransport(in string) string {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "", transportAT:
		return transportAT
	case transportQMI:
		return transportQMI
	case transportMBIM:
		return transportMBIM
	default:
		return strings.ToLower(strings.TrimSpace(in))
	}
}

type currentChannelProvider interface {
	CurrentChannel() byte
}

func newLPAClientWithChannel(ch driver.SmartCardChannel, aid []byte) (*lpa.Client, error) {
	opts := &lpa.Options{Channel: ch, AID: aid, MSS: 120}
	client, err := lpa.New(opts)
	if err != nil {
		if provider, ok := ch.(currentChannelProvider); ok {
			if current := provider.CurrentChannel(); current != 0 {
				_ = ch.CloseLogicalChannel(current)
			}
		}
		_ = ch.Disconnect()
		return nil, fmt.Errorf("创建 LPA client 失败 (AID=%X): %w", aid, err)
	}
	return client, nil
}

// NewManager 创建 eSIM 管理器（支持显式选择 AT/QMI 传输）
func NewManager(opts ManagerOptions) (*Manager, error) {
	transport := normalizeTransport(opts.Transport)
	mgr := &Manager{
		modem:                opts.Modem,
		backend:              opts.Backend,
		deviceID:             opts.DeviceID,
		transport:            transport,
		sf:                   &singleflight.Group{},
		imeiProvider:         opts.IMEIProvider,
		onBeforeSwitch:       opts.OnBeforeSwitch,
		onAfterSwitch:        opts.OnAfterSwitch,
		onSwitchFailed:       opts.OnSwitchFailed,
		onSwitchDegraded:     opts.OnSwitchDegraded,
		onSwitchPhase:        opts.OnSwitchPhase,
		switchSignal:         make(chan string, 16),
		switchUseRefreshTrue: opts.SwitchUseRefreshTrue,
		opDone:               make(chan struct{}),
		postSwitchMinDelay:   defaultPostSwitchMinDelay,
		readQueueWaitTimeout: defaultReadQueueWaitTimeout,
	}
	if opts.APDUArbiter != nil {
		mgr.apduArbiter = opts.APDUArbiter
	}
	if opts.PostSwitchMinDelay > 0 {
		mgr.postSwitchMinDelay = opts.PostSwitchMinDelay
	}

	switch transport {
	case transportAT:
		if opts.Backend == nil {
			return nil, fmt.Errorf("AT 传输需要 device backend")
		}
		if opts.Modem == nil {
			return nil, fmt.Errorf("AT 传输需要 modem 管理器")
		}
		mgr.smartCardChannelFactory = func() (driver.SmartCardChannel, error) {
			return NewModemChannel(opts.Modem), nil
		}
		mgr.clearChannels = func() {
			if opts.Modem != nil {
				opts.Modem.ClearLogicalChannels()
			}
		}
	case transportQMI, transportMBIM:
		// MBIM 与 QMI 共用泛化 APDU 通道（QMIChannel）：QMI 走 QMI UIM transport，
		// MBIM 走 MBIMEx UICC transport（均实现 QMIAPDUTransport）。
		if opts.Backend == nil {
			return nil, fmt.Errorf("%s 传输需要 device backend", transport)
		}
		if opts.QMITransport == nil {
			return nil, ErrQMITransportNotAvailable
		}
		mgr.controlDevice = opts.QMITransport.ControlDevice()
		if strings.TrimSpace(mgr.controlDevice) == "" {
			return nil, ErrQMIControlDeviceMissing
		}
		mgr.smartCardChannelFactory = func() (driver.SmartCardChannel, error) {
			ch := NewQMIChannel(opts.QMITransport, 1)
			// 如果当前处于下载中，将下载的 ctx 注入给新建的通道，
			// 使底层 APDU 传输能够直接继承该 ctx 的超时/取消语义。
			if p := mgr.downloadCtx.Load(); p != nil {
				ch.SetContext(*p)
			}
			return ch, nil
		}
	case transportCustom:
		if opts.SmartCardChannelFactory == nil {
			return nil, fmt.Errorf("custom 传输需要 smart card channel factory")
		}
		mgr.smartCardChannelFactory = opts.SmartCardChannelFactory
	default:
		return nil, fmt.Errorf("不支持的 eSIM transport: %s", transport)
	}

	mgr.channelFactory = func(aid []byte) (*lpa.Client, error) {
		ch, err := mgr.newSmartCardChannel()
		if err != nil {
			return nil, err
		}
		return newLPAClientWithChannel(ch, aid)
	}
	mgr.overviewLoader = mgr.loadOverviewFresh
	mgr.profilesLoader = mgr.loadProfilesFresh
	return mgr, nil
}

func (m *Manager) newSmartCardChannel() (driver.SmartCardChannel, error) {
	if m.smartCardChannelFactory == nil {
		return nil, fmt.Errorf("未配置 APDU 通道工厂")
	}
	return m.smartCardChannelFactory()
}

func (m *Manager) SetSmartCardChannelFactory(factory func() (driver.SmartCardChannel, error)) {
	m.smartCardChannelFactory = factory
}

func (m *Manager) SeedDiscoveredEUICCs(infos []EUICCInfo) {
	if m == nil {
		return
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	m.discoveredEUICCs = mergeEUICCInfosLocked(m.discoveredEUICCs, infos)
}

func (m *Manager) getEffectiveAIDPlan() aidScanPlan {
	return aidScanPlan{Policy: aidScanPolicyFullStatic, AIDs: cloneAIDList(AIDs)}
}

// getEffectiveAIDs 返回应被遍历的 AID 列表
func (m *Manager) getEffectiveAIDs() [][]byte {
	return m.getEffectiveAIDPlan().CloneAIDs()
}

func aidHexesForLog(aids [][]byte) []string {
	if len(aids) == 0 {
		return nil
	}
	out := make([]string, 0, len(aids))
	for _, aid := range aids {
		if len(aid) == 0 {
			continue
		}
		out = append(out, strings.ToUpper(hex.EncodeToString(aid)))
	}
	return out
}

func cloneAIDList(aids [][]byte) [][]byte {
	if len(aids) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(aids))
	for _, aid := range aids {
		if len(aid) == 0 {
			continue
		}
		out = append(out, append([]byte(nil), aid...))
	}
	return out
}

func mergeEUICCInfosLocked(existing []EUICCInfo, incoming []EUICCInfo) []EUICCInfo {
	out := cloneEUICCInfoList(existing)
	for _, info := range incoming {
		normalized, ok := normalizeEUICCInfo(info)
		if !ok {
			continue
		}
		replaced := false
		for i := range out {
			if sameEUICCIdentity(out[i], normalized) {
				out[i] = mergeEUICCInfo(out[i], normalized)
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, normalized)
		}
	}
	sortEUICCInfosStable(out)
	return out
}

func cloneEUICCInfoList(infos []EUICCInfo) []EUICCInfo {
	if len(infos) == 0 {
		return nil
	}
	out := make([]EUICCInfo, 0, len(infos))
	for _, info := range infos {
		normalized, ok := normalizeEUICCInfo(info)
		if ok {
			out = append(out, normalized)
		}
	}
	return out
}

func normalizeEUICCInfo(info EUICCInfo) (EUICCInfo, bool) {
	info.AIDHex = strings.ToUpper(strings.TrimSpace(info.AIDHex))
	info.EID = strings.TrimSpace(info.EID)
	info.SpecGuess = strings.TrimSpace(info.SpecGuess)
	info.SpecConfidence = strings.TrimSpace(info.SpecConfidence)
	if len(info.AID) == 0 && info.AIDHex != "" {
		if aid, err := hex.DecodeString(info.AIDHex); err == nil {
			info.AID = aid
		}
	}
	if len(info.AID) > 0 {
		info.AID = append([]byte(nil), info.AID...)
		if info.AIDHex == "" {
			info.AIDHex = fmt.Sprintf("%X", info.AID)
		}
	}
	return info, info.AIDHex != "" || info.EID != ""
}

func sameEUICCIdentity(a, b EUICCInfo) bool {
	aAID := strings.ToUpper(strings.TrimSpace(a.AIDHex))
	bAID := strings.ToUpper(strings.TrimSpace(b.AIDHex))
	if aAID != "" && bAID != "" && aAID == bAID {
		return true
	}
	aEID := strings.TrimSpace(a.EID)
	bEID := strings.TrimSpace(b.EID)
	return aEID != "" && bEID != "" && aEID == bEID
}

func mergeEUICCInfo(oldInfo, newInfo EUICCInfo) EUICCInfo {
	if len(newInfo.AID) > 0 {
		oldInfo.AID = append([]byte(nil), newInfo.AID...)
	}
	if strings.TrimSpace(newInfo.AIDHex) != "" {
		oldInfo.AIDHex = strings.ToUpper(strings.TrimSpace(newInfo.AIDHex))
	}
	if strings.TrimSpace(newInfo.EID) != "" {
		oldInfo.EID = strings.TrimSpace(newInfo.EID)
	}
	if newInfo.Spec != EUICCSpecUnknown {
		oldInfo.Spec = newInfo.Spec
	}
	if strings.TrimSpace(newInfo.SpecGuess) != "" {
		oldInfo.SpecGuess = strings.TrimSpace(newInfo.SpecGuess)
	}
	if strings.TrimSpace(newInfo.SpecConfidence) != "" {
		oldInfo.SpecConfidence = strings.TrimSpace(newInfo.SpecConfidence)
	}
	if newInfo.FreeNvramBytes != 0 {
		oldInfo.FreeNvramBytes = newInfo.FreeNvramBytes
	}
	if strings.TrimSpace(newInfo.FreeNvram) != "" {
		oldInfo.FreeNvram = strings.TrimSpace(newInfo.FreeNvram)
	}
	if strings.TrimSpace(newInfo.Firmware) != "" {
		oldInfo.Firmware = strings.TrimSpace(newInfo.Firmware)
	}
	if strings.TrimSpace(newInfo.Manufacturer) != "" {
		oldInfo.Manufacturer = strings.TrimSpace(newInfo.Manufacturer)
	}
	if len(newInfo.Certificates) > 0 {
		oldInfo.Certificates = append([]string(nil), newInfo.Certificates...)
	}
	if strings.TrimSpace(newInfo.InfoSource) != "" {
		oldInfo.InfoSource = strings.TrimSpace(newInfo.InfoSource)
	}
	if strings.TrimSpace(newInfo.InfoVersion) != "" {
		oldInfo.InfoVersion = strings.TrimSpace(newInfo.InfoVersion)
	}
	if strings.TrimSpace(newInfo.InfoError) != "" {
		oldInfo.InfoError = strings.TrimSpace(newInfo.InfoError)
	}
	if strings.TrimSpace(newInfo.SASAccreditationNumber) != "" {
		oldInfo.SASAccreditationNumber = strings.TrimSpace(newInfo.SASAccreditationNumber)
	}
	if strings.TrimSpace(newInfo.DefaultSMDPAddress) != "" {
		oldInfo.DefaultSMDPAddress = strings.TrimSpace(newInfo.DefaultSMDPAddress)
	}
	if strings.TrimSpace(newInfo.RootSMDSAddress) != "" {
		oldInfo.RootSMDSAddress = strings.TrimSpace(newInfo.RootSMDSAddress)
	}
	return oldInfo
}

// preCleanChannels 在遍历 AID 前统一清理一次递辑通道
func (m *Manager) preCleanChannels() {
	if m.clearChannels == nil {
		return
	}
	m.clearChannels()
}

// createLPAWithAID 用指定 AID 创建 LPA client（通过 channelFactory 统一分发）
func (m *Manager) createLPAWithAID(aid []byte) (*lpa.Client, error) {
	client, err := m.channelFactory(aid)
	if err != nil {
		return nil, fmt.Errorf("创建 LPA client 失败 (AID=%X): %w", aid, err)
	}
	logger.Info("LPA client 创建成功",
		"device", m.deviceID,
		"AID", fmt.Sprintf("%X", aid),
		"transport", m.transport,
		"control_device", m.controlDevice)
	return client, nil
}

func closeLPAClient(client *lpa.Client) (err error) {
	if client == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("关闭 LPA client panic 已恢复", "err", r)
			err = nil
		}
	}()
	return client.Close()
}

func isExpectedPostResetLPAClientCloseError(operation string, err error) bool {
	if err == nil {
		return false
	}
	switch strings.TrimSpace(operation) {
	case "switch_profile_pre_refresh",
		"switch_profile_deferred",
		"disable_profile_pre_refresh",
		"disable_profile_deferred":
	default:
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "close logical channel") &&
		(strings.Contains(msg, "qmi_uim_card_reset") || strings.Contains(msg, "error=0x0030"))
}

func (m *Manager) closeLPAClientForOperation(operation string, client *lpa.Client) error {
	closeFn := closeLPAClient
	if m != nil && m.closeClient != nil {
		closeFn = m.closeClient
	}
	err := closeFn(client)
	if err != nil {
		if isExpectedPostResetLPAClientCloseError(operation, err) {
			logger.Debug("关闭 LPA client 失败，卡片 reset 后按预期忽略",
				"device", m.deviceID,
				"operation", operation,
				"err", err)
			return err
		}
		logger.Warn("关闭 LPA client 失败",
			"device", m.deviceID,
			"operation", operation,
			"err", err)
	}
	return err
}

func (m *Manager) logWriteOperationHold(operation string, started time.Time) {
	if started.IsZero() {
		return
	}
	hold := time.Since(started)
	if hold < writeOperationWarnThreshold {
		return
	}
	logger.Warn("eSIM 写操作长时间持有写锁",
		"device", m.deviceID,
		"operation", operation,
		"hold_ms", hold.Milliseconds())
}

// forEachEUICC 遍历所有可用的 eUICC，对每个唯一 EID 调用回调函数。
// 每次从静态候选 AID 重新扫描；命中可用 AID 后停止，eSTK Max 的 SE0/SE1 例外。
// 回调参数: client=已打开的 LPA 客户端, aid=当前 AID, eidStr=当前 EID 字符串
func (m *Manager) forEachEUICC(fn func(client *lpa.Client, aid []byte, eidStr string) error) error {
	// 读请求在写操作进行中采用排队等待策略，超时才返回 busy。
	writeWaitStarted := time.Now()
	if err := m.waitForNoWriteOperation(); err != nil {
		logger.Warn("eSIM 读操作等待写锁超时",
			"device", m.deviceID,
			"wait_ms", time.Since(writeWaitStarted).Milliseconds())
		return err
	}
	if waited := time.Since(writeWaitStarted); waited > 100*time.Millisecond {
		logger.Debug("eSIM 读操作已等待写锁释放",
			"device", m.deviceID,
			"wait_ms", waited.Milliseconds())
	}
	// 设备级 APDU 仲裁：等待其它 SIM 鉴权会话释放，避免冲突。
	apduWaitStarted := time.Now()
	if err := m.waitForAPDUIdleForRead(); err != nil {
		logger.Warn("eSIM 读操作等待 APDU 仲裁空闲超时",
			"device", m.deviceID,
			"wait_ms", time.Since(apduWaitStarted).Milliseconds())
		return ErrOperationInProgress
	}
	if waited := time.Since(apduWaitStarted); waited > 100*time.Millisecond {
		logger.Debug("eSIM 读操作已等待 APDU 仲裁空闲",
			"device", m.deviceID,
			"wait_ms", waited.Milliseconds())
	}

	m.preCleanChannels()

	plan := m.getEffectiveAIDPlan()
	aids := plan.CloneAIDs()
	logger.Debug("准备执行 eUICC AID 扫描",
		"device", m.deviceID,
		"policy", plan.Policy,
		"candidate_count", len(aids),
		"candidate_aids", aidHexesForLog(aids))
	foundAny, err := m.doForEachEUICC(aids, fn)
	if foundAny {
		return err
	}

	logger.Warn("AID 扫描未发现 eUICC",
		"device", m.deviceID,
		"policy", plan.Policy,
		"triedCount", len(aids),
		"err", err)
	if err != nil {
		return fmt.Errorf("未发现任何 eUICC: %w", err)
	}
	return fmt.Errorf("未发现任何 eUICC")
}

func (m *Manager) waitForNoWriteOperation() error {
	// 快路径：锁空闲则直接返回
	if m.opMu.TryLock() {
		m.opMu.Unlock()
		return nil
	}
	timeout := m.readQueueWaitTimeout
	if timeout <= 0 {
		timeout = defaultReadQueueWaitTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-m.opDone:
			// 写操作已发出完成通知，尝试确认锁已释放
			if m.opMu.TryLock() {
				m.opMu.Unlock()
				return nil
			}
		case <-timer.C:
			return ErrOperationInProgress
		}
	}
}

func (m *Manager) acquireOperationLock() error {
	if m.opMu.TryLock() {
		return nil
	}
	timeout := m.readQueueWaitTimeout
	if timeout <= 0 {
		timeout = defaultReadQueueWaitTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		done := m.opDone
		if done == nil {
			done = make(chan struct{})
		}
		select {
		case <-done:
			if m.opMu.TryLock() {
				return nil
			}
		case <-timer.C:
			return ErrOperationInProgress
		}
	}
}

func (m *Manager) lockOperation(operation string) (func(), error) {
	if err := m.acquireOperationLock(); err != nil {
		return nil, err
	}
	started := time.Now()
	return func() {
		m.logWriteOperationHold(operation, started)
		m.opMu.Unlock()
		m.notifyWriteDone()
	}, nil
}

// notifyWriteDone 通知所有等待写操作完成的读方。
// 必须在 opMu.Unlock() 之后立即调用。
func (m *Manager) notifyWriteDone() {
	// 关闭旧 channel（广播通知），并新建一个供下次写操作使用
	old := m.opDone
	m.opDone = make(chan struct{})
	if old != nil {
		close(old)
	}
}

func (m *Manager) emitSwitchPhase(operation SwitchOperation, token uint64, phase SwitchPhase) {
	if m == nil || m.onSwitchPhase == nil || phase == "" {
		return
	}
	m.onSwitchPhase(operation, token, phase)
}

func (m *Manager) waitForAPDUIdleForRead() error {
	if m == nil || m.apduArbiter == nil {
		return nil
	}
	timeout := m.readQueueWaitTimeout
	if timeout <= 0 {
		timeout = defaultReadQueueWaitTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := m.apduArbiter.WaitIdle(ctx); err != nil {
		return ErrOperationInProgress
	}
	return nil
}

// doForEachEUICC 执行一轮 AID 遍历，返回是否发现了至少一个 eUICC。
// runEUICCCallback 调用 forEachEUICC 的每-AID 回调，并把回调内部的 panic 转换为普通
// error。回调最终会触达第三方 BER-TLV 解码库（如 euicc-go），面对个别卡片返回的
// 非标准/缺字段 profile 数据时该库可能直接 panic（已知问题：缺少 profile state
// TLV 时 nil 解引用）。一旦 panic 逃出这里，会一路冒泡到 gin 的 Recovery 中间件，
// 导致整个 eSIM 总览请求返回毫无信息量的 500，前端进而把已经探测到的 eUICC 误判为
// "未检测到"。这里 recover 后按普通错误处理，使其复用既有的 per-AID 容错路径。
func (m *Manager) runEUICCCallback(aidHex, eidStr string, client *lpa.Client, aid []byte, fn func(client *lpa.Client, aid []byte, eidStr string) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("eUICC 回调 panic 已恢复",
				"device", m.deviceID,
				"AID", aidHex,
				"EID", eidStr,
				"panic", r,
				"stack", string(debug.Stack()))
			err = fmt.Errorf("eUICC 回调处理失败 (AID=%s): %v", aidHex, r)
		}
	}()
	return fn(client, aid, eidStr)
}

func (m *Manager) doForEachEUICC(aids [][]byte, fn func(client *lpa.Client, aid []byte, eidStr string) error) (bool, error) {
	seenEIDs := make(map[string]bool)
	var successAIDs [][]byte
	foundAny := false
	var lastErr error

	for _, aid := range aids {
		if !shouldContinueAIDScanAfterSuccess(successAIDs, aid) {
			logger.Debug("命中可用 AID 后停止剩余 AID 扫描",
				"device", m.deviceID,
				"next_aid", fmt.Sprintf("%X", aid),
				"success_count", len(successAIDs))
			break
		}

		err := func() error {
			aidHex := fmt.Sprintf("%X", aid)
			client, err := m.createLPAWithAID(aid)
			if err != nil {
				logger.Debug("eUICC AID 扫描阶段",
					"device", m.deviceID,
					"stage", "select_open_failed",
					"AID", aidHex,
					"err", err)
				return err
			}
			defer m.closeLPAClientForOperation("for_each_euicc", client)
			logger.Debug("eUICC AID 扫描阶段",
				"device", m.deviceID,
				"stage", "select_open_ok",
				"AID", aidHex)

			eid, err := client.EID()
			if err != nil {
				logger.Debug("eUICC AID 扫描阶段",
					"device", m.deviceID,
					"stage", "eid_failed",
					"AID", aidHex,
					"err", err)
				return err
			}

			eidStr := hex.EncodeToString(eid)
			logger.Debug("eUICC AID 扫描阶段",
				"device", m.deviceID,
				"stage", "eid_ok",
				"AID", redactIdentifier(aidHex),
				"EID", redactIdentifier(eidStr))
			if seenEIDs[eidStr] {
				logger.Debug("跳过重复 EID", "AID", redactIdentifier(aidHex), "EID", redactIdentifier(eidStr))
				return nil
			}

			seenEIDs[eidStr] = true
			successAIDs = append(successAIDs, aid)
			m.SeedDiscoveredEUICCs([]EUICCInfo{buildDiscoveredEUICCInfo(aid, eidStr)})
			foundAny = true

			return m.runEUICCCallback(aidHex, eidStr, client, aid, fn)
		}()

		if err != nil {
			lastErr = err
		}
	}

	if !foundAny {
		return false, lastErr
	}

	if lastErr != nil {
		logger.Debug("AID 扫描过程中有错误，本轮已发现 eUICC，后续读取仍会重新全量扫描",
			"device", m.deviceID,
			"success_aids_count", len(successAIDs),
			"err", lastErr)
		return true, nil
	}

	return true, nil
}

// GetEIDs 获取所有 eUICC 的 EID 列表
func (m *Manager) GetEIDs() ([]EUICCInfo, error) {
	v, err, _ := m.sf.Do("GetEIDs", func() (interface{}, error) {
		logger.Info("读取 eUICC EID",
			"device", m.deviceID,
			"source", "scan")
		var result []EUICCInfo
		if err := m.forEachEUICC(func(client *lpa.Client, aid []byte, eidStr string) error {
			euiccInfo := buildDiscoveredEUICCInfo(aid, eidStr)
			result = append(result, euiccInfo)
			logger.Info("发现 eUICC", "device", m.deviceID, "AID", redactIdentifier(euiccInfo.AIDHex), "EID", redactIdentifier(eidStr))
			return nil
		}); err != nil {
			return nil, err
		}
		m.SeedDiscoveredEUICCs(result)
		logger.Info("读取 eUICC EID 完成",
			"device", m.deviceID,
			"source", "scan",
			"count", len(result))
		return result, nil
	})

	if err != nil {
		return nil, err
	}
	return v.([]EUICCInfo), nil
}

// GetEID 获取第一个 eUICC 的 EID（向后兼容）
func (m *Manager) GetEID() (string, error) {
	eids, err := m.GetEIDs()
	if err != nil {
		return "", err
	}
	return eids[0].EID, nil
}

// GetEUICCChipInfo 获取 eUICC 芯片的硬件信息（优先返回缓存）
// 包含：所有 EID（含各自可用空间）、产品名称、序列号、固件版本
func (m *Manager) GetEUICCChipInfo(forceRefresh bool) (*EUICCChipInfo, error) {
	// 优先返回缓存（硬件信息不会变，只有可用空间会变）
	if !forceRefresh {
		m.cacheMu.RLock()
		cached := m.chipInfoCache
		m.cacheMu.RUnlock()
		if cached != nil {
			return cached, nil
		}
	}

	v, err, _ := m.sf.Do("GetEUICCChipInfo", func() (interface{}, error) {
		info := &EUICCChipInfo{}
		// fnMu 保护并发 fn 对 info.EIDs 的写操作
		var fnMu sync.Mutex

		if err := m.forEachEUICC(func(client *lpa.Client, aid []byte, eidStr string) error {
			euiccInfo := buildDiscoveredEUICCInfo(aid, eidStr)
			// 在同一 channel 中查询 EUICCInfo2——APDU 读，per-channel 并发安全
			m.parseEUICCInfo2ForEID(client, &euiccInfo)

			fnMu.Lock()
			info.EIDs = append(info.EIDs, euiccInfo)
			fnMu.Unlock()

			logger.Info("获取 eUICC 信息",
				"device", m.deviceID,
				"AID", euiccInfo.AIDHex,
				"EID", eidStr,
				"freeNvram", euiccInfo.FreeNvram)
			return nil
		}); err != nil {
			return nil, err
		}
		sortEUICCInfosStable(info.EIDs)

		// 通过 eSTK.me Product AID 获取硬件标识（SkuName/SerialNumber/Firmware）
		// 优化：硬件标识不随使用变化，若已有缓存则直接复用，跳过 6 次 APDU 开销
		m.cacheMu.RLock()
		cachedChip := m.chipInfoCache
		m.cacheMu.RUnlock()
		if hasReusableChipProductInfo(cachedChip) {
			info.SkuName = cachedChip.SkuName
			info.SerialNumber = cachedChip.SerialNumber
			info.Firmware = cachedChip.Firmware
			logger.Debug("复用 chipInfoCache 跳过 eSTK.me Product AID 查询",
				"device", m.deviceID,
				"cached_sku", cachedChip.SkuName)
		} else {
			// 首次查询或换卡后重新查询
			m.parseESTKmeInfo(info)
		}

		// 如果 eSTK.me 专有指令未返回固件版本，则回退使用从标准 EUICCInfo2 中提取到的固件版本
		if info.Firmware == "" && len(info.EIDs) > 0 {
			info.Firmware = info.EIDs[0].Firmware
		}

		// 对没有私有接口获取 SkuName 的传统白卡（例如 9eSIM）尝试硬编码预测判定
		if info.SkuName == "" && len(info.EIDs) > 0 {
			if guessedName := predictSkuName(info.EIDs[0].EID, info.Firmware); guessedName != "" {
				info.SkuName = guessedName
				logger.Info("基于特征预测了 eSIM 品牌信息", "device", m.deviceID, "guessed_sku", info.SkuName)
			}
		}

		// 写入缓存
		m.cacheMu.Lock()
		m.chipInfoCache = info
		m.cacheMu.Unlock()
		return info, nil
	})

	if err != nil {
		return nil, err
	}
	return v.(*EUICCChipInfo), nil
}

// EsimOverview 合并的 eSIM 总览信息（芯片信息 + 按 eUICC 分组的 profiles）
type EsimOverview struct {
	ChipInfo *EUICCChipInfo  `json:"chip_info"` // 芯片硬件信息
	Profiles []EUICCProfiles `json:"profiles"`  // 按 eUICC 分组的 profile 列表
}

// parseEUICCInfo2ForEID 从标准 eUICC 信息接口解析单个 eUICC 的可用空间、固件版本、制造商和证书信息。
// 保留旧函数名以减少调用点 churn，内部会在 EUICCInfo2 失败时降级到 EUICCInfo1。
func (m *Manager) parseEUICCInfo2ForEID(client *lpa.Client, euicc *EUICCInfo) {
	m.enrichEUICCInfo(client, euicc)
}

// parseESTKmeInfo 通过 eSTK.me Product AID 获取设备名称、序列号和固件版本
func (m *Manager) parseESTKmeInfo(info *EUICCChipInfo) {
	ch, err := m.newSmartCardChannel()
	if err != nil {
		logger.Debug("当前 transport 无法创建原始 APDU 通道，跳过 Product AID 查询",
			"device", m.deviceID,
			"transport", m.transport,
			"err", err)
		return
	}
	if err := ch.Connect(); err != nil {
		logger.Debug("连接原始 APDU 通道失败，跳过 Product AID 查询",
			"device", m.deviceID,
			"transport", m.transport,
			"err", err)
		return
	}
	defer ch.Disconnect() //nolint:errcheck

	channelNum, err := ch.OpenLogicalChannel(estkmeProductAID)
	if err != nil {
		// 不是 eSTK.me 设备，跳过
		logger.Debug("非 eSTK.me 设备，跳过 Product AID 查询",
			"device", m.deviceID,
			"transport", m.transport,
			"err", err)
		return
	}
	defer ch.CloseLogicalChannel(channelNum) //nolint:errcheck

	// APDU 命令格式：CLA=00 INS=00 P1=xx P2=00 Le=00
	// P1=0x03 -> skuName, P1=0x00 -> serialNumber, P1=0x01 -> bootloader, P1=0x02 -> firmware
	readField := func(p1 byte) string {
		cmd := []byte{0x00, 0x00, p1, 0x00, 0x00}
		resp, err := ch.Transmit(cmd)
		if err != nil || len(resp) < 2 {
			return ""
		}
		// 检查 SW=9000
		if resp[len(resp)-2] != 0x90 || resp[len(resp)-1] != 0x00 {
			return ""
		}
		return string(resp[:len(resp)-2])
	}

	info.SkuName = readField(0x03)
	info.SerialNumber = readField(0x00)

	bl := readField(0x01) // bootloader version
	fw := readField(0x02) // firmware version
	if bl != "" && fw != "" {
		info.Firmware = bl + "-" + fw
	} else if fw != "" {
		info.Firmware = fw
	}

	logger.Info("获取到 eSTK.me 设备信息",
		"device", m.deviceID,
		"sku", info.SkuName,
		"serial", info.SerialNumber,
		"firmware", info.Firmware)
}

// formatBytes 将字节数格式化为人类可读的形式
func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
	)
	switch {
	case b >= mb:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func cloneProfiles(groups []EUICCProfiles) []EUICCProfiles {
	if len(groups) == 0 {
		return nil
	}
	cloned := make([]EUICCProfiles, len(groups))
	for i, group := range groups {
		cloned[i] = EUICCProfiles{
			EID:    group.EID,
			AIDHex: group.AIDHex,
		}
		if len(group.Profiles) > 0 {
			cloned[i].Profiles = append([]ProfileItem(nil), group.Profiles...)
		}
	}
	return cloned
}

func cloneChipInfo(info *EUICCChipInfo) *EUICCChipInfo {
	if info == nil {
		return nil
	}
	cloned := *info
	if len(info.EIDs) > 0 {
		cloned.EIDs = append([]EUICCInfo(nil), info.EIDs...)
	}
	return &cloned
}

func cloneOverview(overview *EsimOverview) *EsimOverview {
	if overview == nil {
		return nil
	}
	return &EsimOverview{
		ChipInfo: cloneChipInfo(overview.ChipInfo),
		Profiles: cloneProfiles(overview.Profiles),
	}
}

func (m *Manager) cachedOverview() *EsimOverview {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	return m.overviewCache
}

func (m *Manager) setOverviewCache(overview *EsimOverview, err error, generation uint64) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if generation != m.overviewGeneration {
		return
	}
	m.overviewCache = cloneOverview(overview)
	m.overviewLastErr = err
	if overview != nil {
		m.chipInfoCache = cloneChipInfo(overview.ChipInfo)
	}
}

func (m *Manager) invalidateOverviewCache(reason string) {
	if m == nil {
		return
	}
	m.cacheMu.Lock()
	m.overviewGeneration++
	m.overviewCache = nil
	m.overviewLastErr = nil
	m.chipInfoCache = nil
	m.cacheMu.Unlock()
	if strings.TrimSpace(reason) != "" {
		logger.Info("eSIM 总览缓存已失效", "device", m.deviceID, "reason", reason)
	}
}

func (m *Manager) clearHardwareDiscoveryCachesLocked() {
	m.overviewGeneration++
	m.overviewCache = nil
	m.overviewLastErr = nil
	m.chipInfoCache = nil
	m.discoveredEUICCs = nil
}

func (m *Manager) shouldSuppressOverviewReload() bool {
	if m == nil {
		return false
	}
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	return !m.suppressOverviewReloadUntil.IsZero() && time.Now().Before(m.suppressOverviewReloadUntil)
}

func (m *Manager) overviewReloadDelay() time.Duration {
	if m == nil {
		return 0
	}
	m.cacheMu.RLock()
	until := m.suppressOverviewReloadUntil
	m.cacheMu.RUnlock()
	if until.IsZero() {
		return 0
	}
	delay := time.Until(until)
	if delay < 0 {
		return 0
	}
	return delay
}

func (m *Manager) waitForOverviewReloadAllowed(ctx context.Context) error {
	delay := m.overviewReloadDelay()
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) overviewReloadInProgress() bool {
	if m == nil {
		return false
	}
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	return m.overviewReloading
}

func (m *Manager) beginOverviewReloadSuppression(delay time.Duration) {
	if m == nil || delay <= 0 {
		return
	}
	m.cacheMu.Lock()
	until := time.Now().Add(delay)
	if until.After(m.suppressOverviewReloadUntil) {
		m.suppressOverviewReloadUntil = until
	}
	m.cacheMu.Unlock()
}

func (m *Manager) beginSwitchOverviewSuppression() {
	m.beginOverviewReloadSuppression(5 * time.Second)
}

func (m *Manager) patchCachedActiveProfile(targetICCID string, aidHex string) bool {
	if m == nil {
		return false
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.overviewCache == nil {
		return false
	}
	target := normalizeICCIDValue(targetICCID)
	if target == "" {
		return false
	}
	groupKey := strings.ToUpper(strings.TrimSpace(aidHex))
	targetGroupIndex := -1
	for gi := range m.overviewCache.Profiles {
		group := &m.overviewCache.Profiles[gi]
		if groupKey != "" && strings.ToUpper(strings.TrimSpace(group.AIDHex)) != groupKey {
			continue
		}
		for pi := range group.Profiles {
			if isTargetICCIDActive(target, group.Profiles[pi].ICCID) {
				targetGroupIndex = gi
				break
			}
		}
		if targetGroupIndex >= 0 {
			break
		}
	}
	if targetGroupIndex < 0 {
		for gi := range m.overviewCache.Profiles {
			group := &m.overviewCache.Profiles[gi]
			for pi := range group.Profiles {
				if isTargetICCIDActive(target, group.Profiles[pi].ICCID) {
					targetGroupIndex = gi
					break
				}
			}
			if targetGroupIndex >= 0 {
				break
			}
		}
	}
	if targetGroupIndex < 0 {
		return false
	}

	patched := false
	for gi := range m.overviewCache.Profiles {
		group := &m.overviewCache.Profiles[gi]
		for pi := range group.Profiles {
			isTarget := isTargetICCIDActive(target, group.Profiles[pi].ICCID)
			if isTarget {
				group.Profiles[pi].State = int(sgp22.ProfileEnabled)
				group.Profiles[pi].StateText = "已启用"
				patched = true
			} else {
				group.Profiles[pi].State = int(sgp22.ProfileDisabled)
				group.Profiles[pi].StateText = "已禁用"
			}
		}
	}
	if patched {
		m.overviewLastErr = nil
	}
	return patched
}

func (m *Manager) loadOverview() (*EsimOverview, error) {
	if cached := m.cachedOverview(); cached != nil {
		return cloneOverview(cached), nil
	}
	m.cacheMu.RLock()
	generation := m.overviewGeneration
	m.cacheMu.RUnlock()
	key := fmt.Sprintf("GetEsimOverview:%d", generation)
	v, err, _ := m.sf.Do(key, func() (interface{}, error) {
		if cached := m.cachedOverview(); cached != nil {
			return cloneOverview(cached), nil
		}
		loader := m.overviewLoader
		if loader == nil {
			loader = m.loadOverviewFresh
		}
		overview, loadErr := loader()
		if loadErr != nil {
			m.setOverviewCache(nil, loadErr, generation)
			return nil, loadErr
		}
		m.setOverviewCache(overview, nil, generation)
		return cloneOverview(overview), nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*EsimOverview), nil
}

func (m *Manager) triggerOverviewReload(reason string) {
	if m == nil {
		return
	}
	m.cacheMu.Lock()
	if m.overviewReloading {
		m.cacheMu.Unlock()
		return
	}
	m.overviewReloading = true
	m.cacheMu.Unlock()
	go func() {
		defer func() {
			m.cacheMu.Lock()
			m.overviewReloading = false
			m.cacheMu.Unlock()
		}()
		if err := m.waitForOverviewReloadAllowed(context.Background()); err != nil {
			logger.Warn("eSIM 总览异步重载等待窗口失败", "device", m.deviceID, "reason", reason, "err", err)
			return
		}
		if _, err := m.loadOverview(); err != nil {
			logger.Warn("eSIM 总览异步重载失败", "device", m.deviceID, "reason", reason, "err", err)
		}
	}()
}

func (m *Manager) WarmOverviewAsync(reason string) {
	m.triggerOverviewReload(reason)
}

func buildProfileGroup(eidStr string, aid []byte, profiles []*sgp22.ProfileInfo) EUICCProfiles {
	aidHex := fmt.Sprintf("%X", aid)
	group := EUICCProfiles{
		EID:      eidStr,
		AIDHex:   aidHex,
		Profiles: make([]ProfileItem, 0, len(profiles)),
	}
	for _, p := range profiles {
		name := p.ProfileNickname
		if name == "" {
			name = p.ProfileName
		}
		stateText := "已禁用"
		if p.ProfileState == sgp22.ProfileEnabled {
			stateText = "已启用"
		}
		group.Profiles = append(group.Profiles, ProfileItem{
			ICCID:               p.ICCID.String(),
			Name:                name,
			ServiceProviderName: p.ServiceProviderName,
			State:               int(p.ProfileState),
			StateText:           stateText,
			ClassText:           p.ProfileClass.String(),
		})
	}
	return group
}

func (m *Manager) loadProfilesFresh() ([]EUICCProfiles, error) {
	var profileGroups []EUICCProfiles
	var fnMu sync.Mutex
	if err := m.forEachEUICC(func(client *lpa.Client, aid []byte, eidStr string) error {
		aidHex := fmt.Sprintf("%X", aid)
		logger.Debug("eUICC AID 扫描阶段",
			"device", m.deviceID,
			"stage", "profiles_start",
			"AID", aidHex,
			"EID", eidStr)
		profiles, profileErr := listBasicProfiles(client)

		fnMu.Lock()
		defer fnMu.Unlock()
		if profileErr != nil {
			logger.Debug("eUICC AID 扫描阶段",
				"device", m.deviceID,
				"stage", "profiles_failed",
				"AID", aidHex,
				"EID", eidStr,
				"err", profileErr)
			profileGroups = append(profileGroups, EUICCProfiles{
				EID:      eidStr,
				AIDHex:   aidHex,
				Profiles: []ProfileItem{},
			})
			return nil
		}
		group := buildProfileGroup(eidStr, aid, profiles)
		profileGroups = append(profileGroups, group)
		logger.Debug("eUICC AID 扫描阶段",
			"device", m.deviceID,
			"stage", "profiles_ok",
			"AID", aidHex,
			"EID", eidStr,
			"profileCount", len(profiles))
		logger.Info("获取 eUICC profiles",
			"device", m.deviceID,
			"AID", aidHex,
			"EID", eidStr,
			"profileCount", len(profiles))
		return nil
	}); err != nil {
		return nil, err
	}
	sortEUICCProfilesStable(profileGroups)
	return profileGroups, nil
}

func (m *Manager) loadOverviewFresh() (*EsimOverview, error) {
	info := &EUICCChipInfo{}
	var profileGroups []EUICCProfiles
	var fnMu sync.Mutex

	if err := m.forEachEUICC(func(client *lpa.Client, aid []byte, eidStr string) error {
		euiccInfo := buildDiscoveredEUICCInfo(aid, eidStr)
		aidHex := euiccInfo.AIDHex
		m.parseEUICCInfo2ForEID(client, &euiccInfo)
		logger.Debug("eUICC AID 扫描阶段",
			"device", m.deviceID,
			"stage", "profiles_start",
			"AID", aidHex,
			"EID", eidStr)
		profiles, profileErr := listBasicProfiles(client)

		fnMu.Lock()
		defer fnMu.Unlock()
		info.EIDs = append(info.EIDs, euiccInfo)
		if profileErr != nil {
			logger.Debug("eUICC AID 扫描阶段",
				"device", m.deviceID,
				"stage", "profiles_failed",
				"AID", aidHex,
				"EID", eidStr,
				"err", profileErr)
			profileGroups = append(profileGroups, EUICCProfiles{
				EID:      eidStr,
				AIDHex:   aidHex,
				Profiles: []ProfileItem{},
			})
			return nil
		}
		group := buildProfileGroup(eidStr, aid, profiles)
		profileGroups = append(profileGroups, group)
		logger.Debug("eUICC AID 扫描阶段",
			"device", m.deviceID,
			"stage", "profiles_ok",
			"AID", aidHex,
			"EID", eidStr,
			"profileCount", len(profiles))
		logger.Info("获取 eUICC 信息和 profiles",
			"device", m.deviceID,
			"AID", aidHex,
			"EID", eidStr,
			"freeNvram", euiccInfo.FreeNvram,
			"profileCount", len(profiles))
		return nil
	}); err != nil {
		return nil, err
	}
	sortEUICCInfosStable(info.EIDs)
	sortEUICCProfilesStable(profileGroups)

	m.cacheMu.RLock()
	cachedChip := m.chipInfoCache
	m.cacheMu.RUnlock()
	if hasReusableChipProductInfo(cachedChip) {
		info.SkuName = cachedChip.SkuName
		info.SerialNumber = cachedChip.SerialNumber
		info.Firmware = cachedChip.Firmware
		logger.Debug("复用 chipInfoCache 跳过 eSTK.me Product AID 查询",
			"device", m.deviceID,
			"cached_sku", cachedChip.SkuName)
	} else {
		m.parseESTKmeInfo(info)
	}
	if info.Firmware == "" && len(info.EIDs) > 0 {
		info.Firmware = info.EIDs[0].Firmware
	}
	if info.SkuName == "" && len(info.EIDs) > 0 {
		if guessedName := predictSkuName(info.EIDs[0].EID, info.Firmware); guessedName != "" {
			info.SkuName = guessedName
			logger.Info("基于特征预测了 eSIM 品牌信息", "device", m.deviceID, "guessed_sku", info.SkuName)
		}
	}

	return &EsimOverview{
		ChipInfo: info,
		Profiles: profileGroups,
	}, nil
}

// GetEsimOverview 获取 eSIM 总览信息（一次遍历同时获取芯片信息和 profiles）
func (m *Manager) GetEsimOverview() (*EsimOverview, error) {
	return m.loadOverview()
}

// GetProfiles 获取所有 eUICC 按分组的 profile 列表
func (m *Manager) GetProfiles() ([]EUICCProfiles, error) {
	overview, err := m.loadOverview()
	if err != nil {
		return nil, err
	}
	return cloneProfiles(overview.Profiles), nil
}

func (m *Manager) ActiveProfileName() (string, error) {
	overview := m.cachedOverview()
	if overview == nil {
		return "", nil
	}
	for _, group := range overview.Profiles {
		for _, profile := range group.Profiles {
			if profile.State != 1 {
				continue
			}
			return strings.TrimSpace(profile.Name), nil
		}
	}
	return "", nil
}

func (m *Manager) RefreshProfiles() error {
	if m == nil {
		return nil
	}
	loader := m.profilesLoader
	if loader == nil {
		loader = m.loadProfilesFresh
	}
	profiles, err := loader()
	if err != nil {
		return err
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.overviewCache == nil {
		m.overviewCache = &EsimOverview{}
	}
	m.overviewCache.Profiles = cloneProfiles(profiles)
	m.overviewLastErr = nil
	return nil
}

func (m *Manager) RefreshOverview() error {
	if m == nil {
		return nil
	}
	pendingReload := m.overviewReloadInProgress()
	if err := m.waitForOverviewReloadAllowed(context.Background()); err != nil {
		return err
	}
	if pendingReload || m.overviewReloadInProgress() {
		overview, err := m.loadOverview()
		if err != nil {
			return err
		}
		m.cacheMu.Lock()
		if overview == nil || overview.ChipInfo == nil {
			m.chipInfoCache = nil
		}
		m.cacheMu.Unlock()
		return nil
	}
	m.cacheMu.Lock()
	m.overviewGeneration++
	m.overviewCache = nil
	m.overviewLastErr = nil
	m.discoveredEUICCs = nil
	if !hasReusableChipProductInfo(m.chipInfoCache) {
		m.chipInfoCache = nil
	}
	m.cacheMu.Unlock()

	overview, err := m.loadOverview()
	if err != nil {
		return err
	}

	m.cacheMu.Lock()
	if overview == nil || overview.ChipInfo == nil {
		m.chipInfoCache = nil
	}
	m.cacheMu.Unlock()
	return nil
}

// findAIDForICCID 在所有 eUICC 中查找包含指定 ICCID 的 AID
func (m *Manager) findAIDForICCID(targetICCID string) ([]byte, error) {
	iccid, err := sgp22.NewICCID(targetICCID)
	if err != nil {
		return nil, NewDeleteProfileError(
			DeleteProfileErrorInvalidICCID,
			fmt.Sprintf("无效的 ICCID %q: %v", targetICCID, err),
			err,
		)
	}

	aids := m.getEffectiveAIDs()
	triedCount := 0
	var lastErr error
	for _, aid := range aids {
		found, err := func() (bool, error) {
			triedCount++
			aidHex := fmt.Sprintf("%X", aid)
			logger.Debug("查找 ICCID 所属 eUICC",
				"device", m.deviceID,
				"stage", "select_open_start",
				"ICCID", redactIdentifier(targetICCID),
				"AID", redactIdentifier(aidHex),
				"triedCount", triedCount)
			client, err := m.createLPAWithAID(aid)
			if err != nil {
				logger.Debug("查找 ICCID 所属 eUICC",
					"device", m.deviceID,
					"stage", "select_open_failed",
					"ICCID", redactIdentifier(targetICCID),
					"AID", redactIdentifier(aidHex),
					"err", err)
				return false, err
			}
			defer m.closeLPAClientForOperation("find_aid_for_iccid", client)
			logger.Debug("查找 ICCID 所属 eUICC",
				"device", m.deviceID,
				"stage", "select_open_ok",
				"ICCID", redactIdentifier(targetICCID),
				"AID", redactIdentifier(aidHex))

			logger.Debug("查找 ICCID 所属 eUICC",
				"device", m.deviceID,
				"stage", "profiles_start",
				"ICCID", redactIdentifier(targetICCID),
				"AID", redactIdentifier(aidHex))
			profiles, err := listBasicProfiles(client)
			if err != nil {
				logger.Debug("查找 ICCID 所属 eUICC",
					"device", m.deviceID,
					"stage", "profiles_failed",
					"ICCID", redactIdentifier(targetICCID),
					"AID", redactIdentifier(aidHex),
					"err", err)
				return false, err
			}
			logger.Debug("查找 ICCID 所属 eUICC",
				"device", m.deviceID,
				"stage", "profiles_ok",
				"ICCID", redactIdentifier(targetICCID),
				"AID", redactIdentifier(aidHex),
				"profileCount", len(profiles))
			for _, p := range profiles {
				if p.ICCID.String() == iccid.String() {
					return true, nil
				}
			}
			return false, nil
		}()
		if err != nil {
			lastErr = err
			continue
		}
		if found {
			logger.Info("找到 ICCID 所属 eUICC",
				"device", m.deviceID,
				"ICCID", redactIdentifier(targetICCID),
				"AID", redactIdentifier(fmt.Sprintf("%X", aid)))
			return aid, nil
		}
	}
	msg := fmt.Sprintf("在所有 eUICC 中未找到 ICCID %s (triedCount=%d)", targetICCID, triedCount)
	if lastErr != nil {
		msg = fmt.Sprintf("%s，lastErr=%v", msg, lastErr)
	}
	return nil, NewDeleteProfileError(
		DeleteProfileErrorProfileNotFound,
		msg,
		nil,
	)
}

func normalizeICCIDValue(in string) string {
	v := strings.TrimSpace(in)
	v = strings.Trim(v, "\"")
	v = strings.TrimRight(v, "Ff")
	return v
}

func isTargetICCIDActive(targetICCID string, currentICCID string) bool {
	target := normalizeICCIDValue(targetICCID)
	current := normalizeICCIDValue(currentICCID)
	return target != "" && current != "" && target == current
}

func (m *Manager) backendMode() string {
	if m.backend == nil {
		return transportCustom
	}
	mode := strings.TrimSpace(m.backend.Mode())
	if mode == "" {
		return "unknown"
	}
	return mode
}

// NotifyUIMIndication 用于将 UIM refresh/slot status 指示快速反馈给切卡确认循环。
// 失败场景仍会回退到固定轮询，不依赖该信号。
func (m *Manager) NotifyUIMIndication(source string) {
	if m == nil {
		return
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "indication"
	}
	if m.switchSignal != nil {
		select {
		case m.switchSignal <- source:
		default:
		}
	}
	if m.shouldSuppressOverviewReload() {
		logger.Info("切卡窗口内跳过 eSIM 总览自动重载", "device", m.deviceID, "source", source)
		return
	}
	m.invalidateOverviewCache("uim_" + source)
	m.triggerOverviewReload("uim_" + source)
}

// NotifyModemReset 通知 eSIM 管理器模组已重置（换卡场景）。
// 清空 chipInfoCache 和 discoveredEUICCs，确保下次读取重新全量查询硬件信息。
// 换卡必然触发 modem reset，因此这是保证 chipInfo 缓存正确性的关键路径。
func (m *Manager) NotifyModemReset() {
	if m == nil {
		return
	}
	m.cacheMu.Lock()
	m.clearHardwareDiscoveryCachesLocked()
	m.cacheMu.Unlock()
	if m.shouldSuppressOverviewReload() {
		logger.Info("切卡窗口内跳过 modem reset 导致的 eSIM 自动重载", "device", m.deviceID)
		return
	}
	logger.Info("eSIM 缓存已清空（modem reset）", "device", m.deviceID)
	m.triggerOverviewReload("modem_reset")
}

func (m *Manager) NotifyModemResetDelayed(delay time.Duration) {
	if m == nil {
		return
	}
	m.cacheMu.Lock()
	m.clearHardwareDiscoveryCachesLocked()
	m.cacheMu.Unlock()
	if m.shouldSuppressOverviewReload() {
		logger.Info("切卡窗口内跳过 modem reset 导致的 eSIM 自动重载", "device", m.deviceID)
		return
	}
	logger.Info("eSIM 缓存已清空（modem reset）", "device", m.deviceID, "reload_delay", delay.String())
	if delay > 0 {
		m.beginOverviewReloadSuppression(delay)
	}
	m.triggerOverviewReload("modem_reset")
}

func (m *Manager) drainSwitchSignals() {
	if m == nil || m.switchSignal == nil {
		return
	}
	for {
		select {
		case <-m.switchSignal:
		default:
			return
		}
	}
}

// isExpectedCardResetSignal 判断 err 是否为切卡（refresh=true）触发的预期 eUICC 内部 RESET 信号，
// 而非真正的失败。QMI 与 MBIM 各自有自己的协议层信号（见 ErrQMIUIMCardReset / ErrMBIMUICCInvalidChannel
// 的注释），统一在此处判定，使 finalizeEnableProfileResult 与 DisableProfile 不必关心具体 transport。
func isExpectedCardResetSignal(err error) bool {
	return errors.Is(err, ErrQMIUIMCardReset) || errors.Is(err, ErrMBIMUICCInvalidChannel)
}

func (m *Manager) finalizeEnableProfileResult(targetICCID string, enableErr error) error {
	if enableErr == nil {
		return nil
	}
	if isExpectedCardResetSignal(enableErr) {
		logger.Info("EnableProfile 触发了 eUICC 内部 RESET（预期信号），按切卡命令已提交处理",
			"device", m.deviceID,
			"target", targetICCID)
		return nil
	}
	return fmt.Errorf("启用 profile %s 失败: %w", targetICCID, enableErr)
}

type apduSwitchBarrier interface {
	BeginBarrier(ctx context.Context, req apduarbiter.Request, policy apduarbiter.BarrierPolicy) (*apduarbiter.Barrier, error)
}

func (m *Manager) beginSwitchAPDUBarrier(ctx context.Context) (*apduarbiter.Barrier, error) {
	if m == nil || m.apduArbiter == nil {
		return nil, nil
	}
	coordinator, ok := m.apduArbiter.(apduSwitchBarrier)
	if !ok {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return coordinator.BeginBarrier(ctx, apduarbiter.Request{
		Owner: "esim_switch",
		Mode:  strings.ToUpper(m.transport),
		Class: apduarbiter.APDUClassSwitchBarrier,
	}, apduarbiter.BarrierPolicy{BlockedClasses: []apduarbiter.APDUClass{
		apduarbiter.APDUClassUSIMAKA,
		apduarbiter.APDUClassSMSC,
	}})
}

// SwitchProfile 切换到指定 ICCID 的 profile
// aidHex 可选，前端已知时直接传入可跳过全量 AID 遍历
func (m *Manager) SwitchProfile(ctx context.Context, targetICCID string, aidHex string) error {
	_, err := m.SwitchProfileWithResult(ctx, targetICCID, aidHex)
	return err
}

func (m *Manager) SwitchProfileWithResult(ctx context.Context, targetICCID string, aidHex string) (SwitchProfileResult, error) {
	const operation = SwitchOperationEnableProfile
	result := SwitchProfileResult{TargetICCID: targetICCID}
	m.opMu.Lock()
	writeStarted := time.Now()
	switchSucceeded := false
	switchStarted := false
	var switchToken uint64
	var switchFailureErr error
	defer func() {
		if !switchSucceeded {
			m.emitSwitchPhase(operation, switchToken, SwitchPhaseFailed)
		}
		m.logWriteOperationHold("switch_profile", writeStarted)
		m.opMu.Unlock()
		m.notifyWriteDone()
		if !switchSucceeded && switchStarted && m.onSwitchFailed != nil {
			m.onSwitchFailed(operation, switchToken, switchFailureErr)
		}
	}()

	iccid, err := sgp22.NewICCID(targetICCID)
	if err != nil {
		return result, fmt.Errorf("无效的 ICCID %q: %w", targetICCID, err)
	}
	m.drainSwitchSignals()

	// 1. 查找 ICCID 所属的 eUICC
	var targetAID []byte
	if aidHex != "" {
		// 前端已知 AID，直接 decode
		targetAID, err = hex.DecodeString(aidHex)
		if err != nil {
			return result, fmt.Errorf("无效的 AID hex %q: %w", aidHex, err)
		}
		logger.Info("使用前端传入的 AID", "device", m.deviceID, "AID", redactIdentifier(aidHex))
	} else {
		// 回退：遍历查找
		targetAID, err = m.findAIDForICCID(targetICCID)
		if err != nil {
			return result, err
		}
	}

	// 2. 用对应 AID 创建 LPA client
	client, err := m.createLPAWithAID(targetAID)
	if err != nil {
		return result, err
	}
	clientClosed := false
	defer func() {
		if !clientClosed {
			_ = m.closeLPAClientForOperation("switch_profile_deferred", client)
		}
	}()

	logger.Info("开始切换 eSIM profile",
		"device", m.deviceID,
		"target", targetICCID,
		"AID", fmt.Sprintf("%X", targetAID))

	// 触发切卡前的回调，便于上层暂停会占用 SIM 通道的操作。
	if m.onBeforeSwitch != nil {
		switchToken = m.onBeforeSwitch(operation, targetICCID)
		switchStarted = true
	}
	result.SwitchToken = switchToken
	result.Phase = SwitchPhaseAPDUSwitching
	m.emitSwitchPhase(operation, switchToken, SwitchPhaseAPDUSwitching)

	switchBarrier, err := m.beginSwitchAPDUBarrier(ctx)
	barrierReleased := false
	releaseSwitchBarrier := func(nextPhase SwitchPhase) {
		if switchBarrier != nil && !barrierReleased {
			switchBarrier.Release()
			barrierReleased = true
		}
		if nextPhase != "" {
			result.Phase = nextPhase
			m.emitSwitchPhase(operation, switchToken, nextPhase)
		}
	}
	defer releaseSwitchBarrier("")
	if err != nil {
		switchFailureErr = err
		return result, fmt.Errorf("等待切卡 APDU barrier 失败: %w", err)
	}

	// refresh flag 由配置控制；refresh=true 可能立即触发 UIM refresh/slot indication，
	// 必须在发 APDU 前抑制自动 overview reload。
	m.beginSwitchOverviewSuppression()

	// 4. 启用目标 profile（refresh=true 会自动禁用当前活跃的 profile）。
	// refresh=true 时 UIM card reset（QMI: ErrQMIUIMCardReset / MBIM: ErrMBIMUICCInvalidChannel）
	// 是预期信号，后续 finalize 已处理，见 isExpectedCardResetSignal。
	// CatBusy (result=5) 是瞬态错误：飞行模式切换等操作会导致卡片 CAT 忙碌，
	// 等待短暂间隔后重试即可恢复。最多重试 3 次，间隔 800ms。
	var enableErr error
	const maxCatBusyRetries = 3
	for attempt := 0; attempt <= maxCatBusyRetries; attempt++ {
		enableErr = client.EnableProfile(iccid, m.switchUseRefreshTrue)
		if enableErr == nil || !errors.Is(enableErr, sgp22.ErrCatBusy) {
			break
		}
		if attempt < maxCatBusyRetries {
			logger.Warn("EnableProfile 返回 CatBusy，卡片 CAT 忙碌中，等待后重试",
				"device", m.deviceID,
				"target", targetICCID,
				"attempt", fmt.Sprintf("%d/%d", attempt+1, maxCatBusyRetries))
			time.Sleep(800 * time.Millisecond)
		}
	}

	// 在模组重启前主动关闭 LPA 逻辑通道（AT+CCHC）
	if err := m.closeLPAClientForOperation("switch_profile_pre_refresh", client); err == nil {
		clientClosed = true
	}

	// 5. 给卡片一点时间完成内部刷新动作。
	time.Sleep(200 * time.Millisecond)
	releaseSwitchBarrier(SwitchPhaseCardResetSettling)

	if enableErr != nil && !isExpectedCardResetSignal(enableErr) {
		switchFailureErr = enableErr
		if err := m.finalizeEnableProfileResult(targetICCID, enableErr); err != nil {
			return result, err
		}
	}
	result.SwitchAccepted = true

	patched := m.patchCachedActiveProfile(targetICCID, fmt.Sprintf("%X", targetAID))
	result.CachePatched = patched

	logger.Info("eSIM profile 切换指令已提交，后处理将异步继续",
		"device", m.deviceID, "target", targetICCID, "cache_patched", patched, "degraded_reason", result.DegradedReason)

	// 6. 切卡后回调改为 Ready + Delay 门控，避免与 eSIM 重读或 SIM 鉴权冲突。
	go m.runPostSwitchRecovery(operation, switchToken, targetICCID)
	result.PostSwitchAsync = true
	result.RecoveryPending = true

	switchSucceeded = true
	return result, nil
}

// DisableProfile 禁用指定 ICCID 的 profile。
// aidHex 可选，前端已知时直接传入可跳过全量 AID 遍历。
func (m *Manager) DisableProfile(ctx context.Context, targetICCID string, aidHex string) error {
	const operation = SwitchOperationDisableProfile
	m.opMu.Lock()
	writeStarted := time.Now()
	disableSucceeded := false
	disableStarted := false
	var switchToken uint64
	var switchFailureErr error
	defer func() {
		if !disableSucceeded {
			m.emitSwitchPhase(operation, switchToken, SwitchPhaseFailed)
		}
		m.logWriteOperationHold("disable_profile", writeStarted)
		m.opMu.Unlock()
		m.notifyWriteDone()
		if !disableSucceeded && disableStarted && m.onSwitchFailed != nil {
			m.onSwitchFailed(operation, switchToken, switchFailureErr)
		}
	}()

	iccid, err := sgp22.NewICCID(targetICCID)
	if err != nil {
		return fmt.Errorf("无效的 ICCID %q: %w", targetICCID, err)
	}

	var targetAID []byte
	if aidHex != "" {
		targetAID, err = hex.DecodeString(aidHex)
		if err != nil {
			return fmt.Errorf("无效的 AID hex %q: %w", aidHex, err)
		}
	} else {
		targetAID, err = m.findAIDForICCID(targetICCID)
		if err != nil {
			return err
		}
	}

	client, err := m.createLPAWithAID(targetAID)
	if err != nil {
		return err
	}
	clientClosed := false
	defer func() {
		if !clientClosed {
			_ = m.closeLPAClientForOperation("disable_profile_deferred", client)
		}
	}()

	logger.Info("开始禁用 eSIM profile",
		"device", m.deviceID,
		"ICCID", targetICCID,
		"AID", fmt.Sprintf("%X", targetAID))

	if m.onBeforeSwitch != nil {
		switchToken = m.onBeforeSwitch(operation, "")
		disableStarted = true
	}
	m.emitSwitchPhase(operation, switchToken, SwitchPhaseAPDUSwitching)

	switchBarrier, err := m.beginSwitchAPDUBarrier(ctx)
	barrierReleased := false
	releaseSwitchBarrier := func(nextPhase SwitchPhase) {
		if switchBarrier != nil && !barrierReleased {
			switchBarrier.Release()
			barrierReleased = true
		}
		if nextPhase != "" {
			m.emitSwitchPhase(operation, switchToken, nextPhase)
		}
	}
	defer releaseSwitchBarrier("")
	if err != nil {
		switchFailureErr = err
		return fmt.Errorf("等待禁用 profile APDU barrier 失败: %w", err)
	}

	disableErr := client.DisableProfile(iccid, true)
	if err := m.closeLPAClientForOperation("disable_profile_pre_refresh", client); err == nil {
		clientClosed = true
	}

	time.Sleep(200 * time.Millisecond)
	releaseSwitchBarrier(SwitchPhaseCardResetSettling)

	if disableErr != nil {
		if isExpectedCardResetSignal(disableErr) {
			logger.Info("DisableProfile 触发了 eUICC 内部 RESET（预期信号），按禁用指令提交成功处理",
				"device", m.deviceID,
				"target", targetICCID)
		} else {
			switchFailureErr = disableErr
			return fmt.Errorf("禁用 profile %s 失败: %w", targetICCID, disableErr)
		}
	}
	m.beginSwitchOverviewSuppression()

	m.invalidateOverviewCache("disable_profile")
	m.triggerOverviewReload("disable_profile")

	logger.Info("eSIM profile 禁用指令已提交",
		"device", m.deviceID,
		"target", targetICCID)

	go m.runPostSwitchHook(operation, switchToken)

	disableSucceeded = true
	return nil
}

func (m *Manager) runPostSwitchHook(operation SwitchOperation, token uint64) {
	if m == nil || m.onAfterSwitch == nil {
		return
	}
	hookStart, postSwitchDelayMS := m.waitPostSwitchHookDelay()
	m.finishPostSwitchHook(operation, token, hookStart, postSwitchDelayMS)
}

func (m *Manager) runPostSwitchRecovery(operation SwitchOperation, token uint64, targetICCID string) {
	if m == nil {
		return
	}
	hookStart, postSwitchDelayMS := m.waitPostSwitchHookDelay()
	m.finishPostSwitchHook(operation, token, hookStart, postSwitchDelayMS)
}

func (m *Manager) waitPostSwitchHookDelay() (time.Time, int64) {
	hookStart := time.Now()
	delay := m.postSwitchMinDelay
	if delay <= 0 {
		delay = defaultPostSwitchMinDelay
	}
	time.Sleep(delay)
	return hookStart, time.Since(hookStart).Milliseconds()
}

func (m *Manager) finishPostSwitchHook(operation SwitchOperation, token uint64, hookStart time.Time, postSwitchDelayMS int64) {
	logger.Info("切卡后 hook 阶段完成",
		"device", m.deviceID,
		"operation", operation,
		"switch_token", token,
		"post_switch_delay_ms", postSwitchDelayMS,
		"hook_total_ms", time.Since(hookStart).Milliseconds())
	if m.onAfterSwitch != nil {
		m.onAfterSwitch(operation, token)
	}
}

// DownloadProgressEvent 是进度回调的事件结构
type DownloadProgressEvent struct {
	Step string // 阶段标识，如 "preflight"、"auth_client"、"auth_server"、"install"、"notify"、"done"
	Msg  string // 中文描述
	Pct  int    // 进度百分比 0-100
}

// DownloadProgressFn 是进度回调函数类型
type DownloadProgressFn func(event DownloadProgressEvent)

type SpaceDeltaDirection string

const (
	SpaceDeltaDirectionReleased SpaceDeltaDirection = "released"
	SpaceDeltaDirectionConsumed SpaceDeltaDirection = "consumed"
)

type SpaceDelta struct {
	Direction SpaceDeltaDirection `json:"direction"`
	Bytes     int64               `json:"bytes"`
}

type DeleteProfileResult struct {
	Warning     string
	WarningCode string
	SpaceDelta  *SpaceDelta
}

type DownloadProfileResult struct {
	Warning     string
	WarningCode string
	SpaceDelta  *SpaceDelta
}

type spaceDeltaOperation string

const (
	spaceDeltaOperationDelete   spaceDeltaOperation = "delete"
	spaceDeltaOperationDownload spaceDeltaOperation = "download"
)

func buildSpaceDeltaForOperation(op spaceDeltaOperation, before int32, after int32) *SpaceDelta {
	if before <= 0 || after <= 0 {
		return nil
	}

	switch op {
	case spaceDeltaOperationDelete:
		if after <= before {
			return nil
		}
		return &SpaceDelta{
			Direction: SpaceDeltaDirectionReleased,
			Bytes:     int64(after - before),
		}
	case spaceDeltaOperationDownload:
		if after >= before {
			return nil
		}
		return &SpaceDelta{
			Direction: SpaceDeltaDirectionConsumed,
			Bytes:     int64(before - after),
		}
	default:
		return nil
	}
}

func (m *Manager) readFreeNvramBytes(client *lpa.Client) int32 {
	if client == nil {
		return 0
	}
	var info EUICCInfo
	m.parseEUICCInfo2ForEID(client, &info)
	return info.FreeNvramBytes
}

func (m *Manager) readFreeNvramBytesWithRetry(client *lpa.Client, attempts int, delay time.Duration) int32 {
	for attempt := 0; attempt < attempts; attempt++ {
		if freeBytes := m.readFreeNvramBytes(client); freeBytes > 0 {
			return freeBytes
		}
		if attempt < attempts-1 {
			time.Sleep(delay)
		}
	}
	return 0
}

type NotificationItem struct {
	SequenceNumber int64  `json:"sequence_number"`
	Event          string `json:"event"`
	ICCID          string `json:"iccid,omitempty"`
	Address        string `json:"address,omitempty"`
	AIDHex         string `json:"aid_hex,omitempty"`
	CanRetry       bool   `json:"can_retry"`
}

type NotificationErrorCode string

const (
	NotificationErrorInvalidSequence NotificationErrorCode = "INVALID_SEQUENCE"
	NotificationErrorInvalidAIDHex   NotificationErrorCode = "INVALID_AID_HEX"
	NotificationErrorNotFound        NotificationErrorCode = "NOT_FOUND"
	NotificationErrorBusy            NotificationErrorCode = "BUSY"
	NotificationErrorInternal        NotificationErrorCode = "INTERNAL"
)

type NotificationError struct {
	Code    NotificationErrorCode
	Message string
	Err     error
}

func (e *NotificationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "notification operation failed"
}

func (e *NotificationError) Unwrap() error { return e.Err }

func NewNotificationError(code NotificationErrorCode, message string, err error) error {
	return &NotificationError{Code: code, Message: message, Err: err}
}

func ClassifyNotificationError(err error) NotificationErrorCode {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrOperationInProgress) {
		return NotificationErrorBusy
	}
	var ne *NotificationError
	if errors.As(err, &ne) && ne.Code != "" {
		return ne.Code
	}
	return NotificationErrorInternal
}

func maxNotificationSequence(notifications []*sgp22.NotificationMetadata) sgp22.SequenceNumber {
	var lastSeq sgp22.SequenceNumber
	for _, notification := range notifications {
		if notification != nil && notification.SequenceNumber > lastSeq {
			lastSeq = notification.SequenceNumber
		}
	}
	return lastSeq
}

func downloadResultNotificationMetadata(result *sgp22.LoadBoundProfilePackageResponse) *sgp22.NotificationMetadata {
	if result == nil || result.Notification == nil || result.Notification.SequenceNumber <= 0 {
		return nil
	}
	return &sgp22.NotificationMetadata{SequenceNumber: result.Notification.SequenceNumber}
}

func downloadNotificationBaseline(preDownloadNotifications []*sgp22.NotificationMetadata, resultNotification *sgp22.NotificationMetadata) sgp22.SequenceNumber {
	baseline := maxNotificationSequence(preDownloadNotifications)
	if resultNotification != nil && resultNotification.SequenceNumber > 0 {
		derived := resultNotification.SequenceNumber - 1
		if derived > baseline {
			baseline = derived
		}
	}
	return baseline
}

func downloadNotificationResult(observed bool, retrieveErr error, handleErr error) DownloadProfileResult {
	if !observed {
		return DownloadProfileResult{
			Warning:     "Profile 下载完成，但通知未完全确认",
			WarningCode: "download_notification_not_observed",
		}
	}
	if retrieveErr != nil {
		return DownloadProfileResult{
			Warning:     "Profile 下载完成，但通知未完全确认",
			WarningCode: "download_notification_retrieve_failed",
		}
	}
	if handleErr != nil {
		return DownloadProfileResult{
			Warning:     "Profile 下载完成，但通知未完全确认",
			WarningCode: "download_notification_handle_failed",
		}
	}
	return DownloadProfileResult{}
}

func downloadFinalizeRecoveredResult() DownloadProfileResult {
	return DownloadProfileResult{
		Warning:     "Profile 已安装，下载会话收尾返回异常；安装通知已发送，已按成功处理",
		WarningCode: "download_installed_finalize_error_recovered",
	}
}

func safeListNotification(client *lpa.Client, filters ...sgp22.NotificationEvent) (notifications []*sgp22.NotificationMetadata, err error) {
	if client == nil {
		return nil, fmt.Errorf("LPA client 为空")
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("解析通知列表响应失败: %v", r)
			notifications = nil
		}
	}()
	notifications, err = client.ListNotification(filters...)
	if err != nil {
		return nil, wrapNotificationParseError("解析通知列表响应失败", err)
	}
	return notifications, nil
}

func safeRetrieveNotificationList(client *lpa.Client, seq sgp22.SequenceNumber) (pendingNotifications []*sgp22.PendingNotification, err error) {
	if client == nil {
		return nil, fmt.Errorf("LPA client 为空")
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("解析待发送通知响应失败: %v", r)
			pendingNotifications = nil
		}
	}()
	pendingNotifications, err = client.RetrieveNotificationList(seq)
	if err != nil {
		return nil, wrapNotificationParseError("解析待发送通知响应失败", err)
	}
	return pendingNotifications, nil
}

func wrapNotificationParseError(prefix string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sgp22.ErrUndefined) || errors.Is(err, sgp22.ErrUnexpectedTag) {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return err
}

func isRecoverableDownloadInstallFinalizeError(err error) bool {
	if err == nil {
		return false
	}
	var bppPtr *sgp22.LoadBoundProfilePackageError
	var bppValue sgp22.LoadBoundProfilePackageError
	if errors.As(err, &bppPtr) && bppPtr != nil {
		return false
	}
	if errors.As(err, &bppValue) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"apdu",
		"pc/sc",
		"cancel session",
		"execution error",
		"unexpected response",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func deleteNotificationResult(observed bool, retrieveErr error, handleErr error) DeleteProfileResult {
	if !observed {
		return DeleteProfileResult{
			Warning:     "Profile 已删除，但删除通知发送未完全确认",
			WarningCode: "delete_notification_not_observed",
		}
	}
	if retrieveErr != nil {
		return DeleteProfileResult{
			Warning:     "Profile 已删除，但删除通知发送未完全确认",
			WarningCode: "delete_notification_retrieve_failed",
		}
	}
	if handleErr != nil {
		return DeleteProfileResult{
			Warning:     "Profile 已删除，但删除通知发送未完全确认",
			WarningCode: "delete_notification_handle_failed",
		}
	}
	return DeleteProfileResult{}
}

func notificationEventName(event sgp22.NotificationEvent) string {
	switch event {
	case sgp22.NotificationEventInstall:
		return "install"
	case sgp22.NotificationEventEnable:
		return "enable"
	case sgp22.NotificationEventDisable:
		return "disable"
	case sgp22.NotificationEventDelete:
		return "delete"
	default:
		return "unknown"
	}
}

func buildNotificationItems(notifications []*sgp22.NotificationMetadata, aidHex string) []NotificationItem {
	items := make([]NotificationItem, 0, len(notifications))
	for _, notification := range notifications {
		if notification == nil {
			continue
		}
		item := NotificationItem{
			SequenceNumber: int64(notification.SequenceNumber),
			Event:          notificationEventName(notification.ProfileManagementOperation),
			Address:        notification.Address,
			AIDHex:         aidHex,
			CanRetry:       notification.Address != "",
		}
		if len(notification.ICCID) > 0 {
			item.ICCID = notification.ICCID.String()
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].SequenceNumber > items[j].SequenceNumber
	})
	return items
}

func isAutoCleanableLoadedNotification(notification *sgp22.NotificationMetadata) bool {
	if notification == nil || notification.SequenceNumber <= 0 {
		return false
	}
	switch notification.ProfileManagementOperation {
	case sgp22.NotificationEventEnable, sgp22.NotificationEventDisable:
		return true
	default:
		return false
	}
}

func filterCleanedNotifications(notifications []*sgp22.NotificationMetadata, cleaned map[sgp22.SequenceNumber]bool) []*sgp22.NotificationMetadata {
	if len(cleaned) == 0 {
		return notifications
	}
	filtered := make([]*sgp22.NotificationMetadata, 0, len(notifications))
	for _, notification := range notifications {
		if notification == nil || !cleaned[notification.SequenceNumber] {
			filtered = append(filtered, notification)
		}
	}
	return filtered
}

func (m *Manager) autoCleanLoadedNotifications(client *lpa.Client, notifications []*sgp22.NotificationMetadata, aidHex string) map[sgp22.SequenceNumber]bool {
	cleaned := make(map[sgp22.SequenceNumber]bool)
	for _, metadata := range notifications {
		if !isAutoCleanableLoadedNotification(metadata) {
			continue
		}
		seq := metadata.SequenceNumber
		eventName := notificationEventName(metadata.ProfileManagementOperation)

		var pendingNotifications []*sgp22.PendingNotification
		if err := retryWithBackoff(3, 300*time.Millisecond,
			func(attempt int, wait time.Duration, err error) {
				logger.Warn("eSIM 加载通知自动清理获取待发送通知失败，稍后重试",
					"device", m.deviceID,
					"AID", aidHex,
					"sequence", seq,
					"event", eventName,
					"attempt", fmt.Sprintf("%d/%d", attempt, 3),
					"wait_ms", wait.Milliseconds(),
					"err", err)
			},
			func() error {
				var err error
				pendingNotifications, err = safeRetrieveNotificationList(client, seq)
				return err
			},
		); err != nil {
			logger.Warn("eSIM 加载通知自动清理失败，获取待发送通知失败",
				"device", m.deviceID,
				"AID", aidHex,
				"sequence", seq,
				"event", eventName,
				"err", err)
			continue
		}
		if len(pendingNotifications) == 0 {
			logger.Warn("eSIM 加载通知自动清理跳过，卡片未返回待发送通知",
				"device", m.deviceID,
				"AID", aidHex,
				"sequence", seq,
				"event", eventName)
			continue
		}

		handledAny := false
		handleErr := false
		for _, notification := range pendingNotifications {
			if notification == nil {
				continue
			}
			if err := retryWithBackoff(3, 300*time.Millisecond,
				func(attempt int, wait time.Duration, err error) {
					logger.Warn("eSIM 加载通知自动清理发送通知失败，稍后重试",
						"device", m.deviceID,
						"AID", aidHex,
						"sequence", seq,
						"event", eventName,
						"attempt", fmt.Sprintf("%d/%d", attempt, 3),
						"wait_ms", wait.Milliseconds(),
						"err", err)
				},
				func() error {
					return client.HandleNotification(notification)
				},
			); err != nil {
				logger.Warn("eSIM 加载通知自动清理失败，发送通知失败",
					"device", m.deviceID,
					"AID", aidHex,
					"sequence", seq,
					"event", eventName,
					"err", err)
				handleErr = true
				break
			}
			handledAny = true
		}
		if handleErr {
			continue
		}
		if !handledAny {
			logger.Warn("eSIM 加载通知自动清理跳过，卡片返回空待发送通知",
				"device", m.deviceID,
				"AID", aidHex,
				"sequence", seq,
				"event", eventName)
			continue
		}

		if err := retryWithBackoff(3, 300*time.Millisecond,
			func(attempt int, wait time.Duration, err error) {
				logger.Warn("eSIM 加载通知自动清理移除卡内通知失败，稍后重试",
					"device", m.deviceID,
					"AID", aidHex,
					"sequence", seq,
					"event", eventName,
					"attempt", fmt.Sprintf("%d/%d", attempt, 3),
					"wait_ms", wait.Milliseconds(),
					"err", err)
			},
			func() error {
				err := client.RemoveNotificationFromList(seq)
				if errors.Is(err, sgp22.ErrNothingToDelete) {
					return nil
				}
				return err
			},
		); err != nil {
			logger.Warn("eSIM 加载通知自动清理失败，移除卡内通知失败",
				"device", m.deviceID,
				"AID", aidHex,
				"sequence", seq,
				"event", eventName,
				"err", err)
			continue
		}

		cleaned[seq] = true
		logger.Info("eSIM 加载通知已自动清理状态通知",
			"device", m.deviceID,
			"AID", aidHex,
			"sequence", seq,
			"event", eventName)
	}
	return cleaned
}

func (m *Manager) listNotificationItemsWithCleanup(client *lpa.Client, aidHex string) ([]NotificationItem, error) {
	notifications, err := safeListNotification(client)
	if err != nil {
		return nil, err
	}
	cleaned := m.autoCleanLoadedNotifications(client, notifications, aidHex)
	if len(cleaned) > 0 {
		refreshed, refreshErr := safeListNotification(client)
		if refreshErr != nil {
			logger.Warn("eSIM 加载通知自动清理后刷新列表失败，使用本地过滤结果",
				"device", m.deviceID,
				"AID", aidHex,
				"err", refreshErr)
			notifications = filterCleanedNotifications(notifications, cleaned)
		} else {
			notifications = refreshed
		}
	}
	return buildNotificationItems(notifications, aidHex), nil
}

func (m *Manager) resolveNotificationAID(aidHex string) ([]byte, error) {
	if aidHex != "" {
		targetAID, err := hex.DecodeString(aidHex)
		if err != nil {
			return nil, NewNotificationError(
				NotificationErrorInvalidAIDHex,
				fmt.Sprintf("无效的 AID hex %q: %v", aidHex, err),
				err,
			)
		}
		return targetAID, nil
	}
	return nil, nil
}

func (m *Manager) notificationCandidateAIDs() [][]byte {
	aids := m.getEffectiveAIDs()
	candidates := make([][]byte, 0, len(aids))
	for _, aid := range aids {
		candidates = append(candidates, append([]byte(nil), aid...))
	}
	return candidates
}

func (m *Manager) listNotificationsForCurrentCard() ([]NotificationItem, error) {
	unlock, err := m.lockOperation("list_notifications_current_card")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := m.waitForAPDUIdleForRead(); err != nil {
		return nil, err
	}
	m.preCleanChannels()

	items := make([]NotificationItem, 0)
	var lastErr error
	var successCount int
	for _, aid := range m.notificationCandidateAIDs() {
		client, err := m.createLPAWithAID(aid)
		if err != nil {
			lastErr = err
			continue
		}
		aidHex := strings.ToUpper(hex.EncodeToString(aid))
		aidItems, listErr := m.listNotificationItemsWithCleanup(client, aidHex)
		_ = m.closeLPAClientForOperation("list_notifications_current_card", client)
		if listErr != nil {
			lastErr = listErr
			continue
		}
		successCount++
		items = append(items, aidItems...)
	}
	if len(items) > 0 {
		sort.SliceStable(items, func(i, j int) bool {
			return items[i].SequenceNumber > items[j].SequenceNumber
		})
		return items, nil
	}
	if successCount > 0 {
		return []NotificationItem{}, nil
	}
	if lastErr != nil {
		return nil, NewNotificationError(NotificationErrorInternal, fmt.Sprintf("获取通知列表失败: %v", lastErr), lastErr)
	}
	return []NotificationItem{}, nil
}

func (m *Manager) ListNotifications(aidHex string) ([]NotificationItem, error) {
	targetAID, err := m.resolveNotificationAID(aidHex)
	if err != nil {
		return nil, err
	}
	if len(targetAID) == 0 {
		return m.listNotificationsForCurrentCard()
	}
	unlock, err := m.lockOperation("list_notifications")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := m.waitForAPDUIdleForRead(); err != nil {
		return nil, err
	}
	m.preCleanChannels()
	client, err := m.createLPAWithAID(targetAID)
	if err != nil {
		return nil, NewNotificationError(NotificationErrorInternal, fmt.Sprintf("创建 LPA client 失败: %v", err), err)
	}
	items, err := m.listNotificationItemsWithCleanup(client, strings.ToUpper(hex.EncodeToString(targetAID)))
	if closeErr := m.closeLPAClientForOperation("list_notifications", client); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, NewNotificationError(NotificationErrorInternal, fmt.Sprintf("获取通知列表失败: %v", err), err)
	}
	return items, nil
}

func (m *Manager) RetryNotification(sequenceNumber int64, aidHex string) error {
	m.opMu.Lock()
	writeStarted := time.Now()
	defer func() {
		m.logWriteOperationHold("retry_notification", writeStarted)
		m.opMu.Unlock()
		m.notifyWriteDone()
	}()
	if sequenceNumber <= 0 {
		return NewNotificationError(NotificationErrorInvalidSequence, fmt.Sprintf("无效的通知序号 %d", sequenceNumber), nil)
	}
	targetAID, err := m.resolveNotificationAID(aidHex)
	if err != nil {
		return err
	}
	client, err := m.createLPAWithAID(targetAID)
	if err != nil {
		return NewNotificationError(NotificationErrorInternal, fmt.Sprintf("创建 LPA client 失败: %v", err), err)
	}
	defer m.closeLPAClientForOperation("retry_notification", client)
	seq := sgp22.SequenceNumber(sequenceNumber)
	pendingNotifications, err := safeRetrieveNotificationList(client, seq)
	if err != nil {
		if errors.Is(err, sgp22.ErrUndefined) {
			return NewNotificationError(NotificationErrorNotFound, fmt.Sprintf("通知 %d 不存在", sequenceNumber), err)
		}
		return NewNotificationError(NotificationErrorInternal, fmt.Sprintf("获取待发送通知失败: %v", err), err)
	}
	if len(pendingNotifications) == 0 {
		return NewNotificationError(NotificationErrorNotFound, fmt.Sprintf("通知 %d 不存在", sequenceNumber), nil)
	}
	for _, notification := range pendingNotifications {
		if notification == nil {
			continue
		}
		if err := retryWithBackoff(3, 300*time.Millisecond, nil, func() error {
			return client.HandleNotification(notification)
		}); err != nil {
			return NewNotificationError(NotificationErrorInternal, fmt.Sprintf("重试发送通知失败: %v", err), err)
		}
	}
	return nil
}

// DownloadProfile 下载 eSIM profile 到指定 SE
// aidHex 为目标 AID（hex 字符串），为空则使用第一个可用 AID
// smdp 为 SM-DP+ 服务器地址，matchingID 和 confirmationCode 可选
// downloadIMEI 为可选的前端指定 IMEI；为空时使用设备真实 IMEI
// progressFn 为可选进度回调，为 nil 时静默执行
func (m *Manager) DownloadProfile(ctx context.Context, aidHex, smdp, matchingID, confirmationCode, downloadIMEI string, progressFn DownloadProgressFn) (DownloadProfileResult, error) {
	report := func(step, msg string, pct int) {
		if progressFn != nil {
			progressFn(DownloadProgressEvent{Step: step, Msg: msg, Pct: pct})
		}
	}
	result := DownloadProfileResult{}
	m.opMu.Lock()
	writeStarted := time.Now()
	defer func() {
		m.logWriteOperationHold("download_profile", writeStarted)
		m.downloadCtx.Store(nil)
		m.opMu.Unlock()
		m.notifyWriteDone()
	}()
	m.downloadCtx.Store(&ctx)
	var client *lpa.Client
	var targetAID []byte
	if aidHex != "" {
		aid, err := hex.DecodeString(aidHex)
		if err != nil {
			return DownloadProfileResult{}, fmt.Errorf("无效的 AID hex %q: %w", aidHex, err)
		}
		targetAID = aid
		client, err = m.createLPAWithAID(targetAID)
		if err != nil {
			return DownloadProfileResult{}, err
		}
	} else {
		aids := m.getEffectiveAIDs()
		for _, aid := range aids {
			c, err := m.createLPAWithAID(aid)
			if err != nil {
				continue
			}
			client = c
			targetAID = append([]byte(nil), aid...)
			aidHex = fmt.Sprintf("%X", targetAID)
			break
		}
		if client == nil {
			return DownloadProfileResult{}, fmt.Errorf("未找到可用的 eUICC AID")
		}
	}
	defer func() {
		if client != nil {
			m.closeLPAClientForOperation("download_profile", client)
		}
	}()

	preDownloadNotifications, preDownloadNotificationsErr := safeListNotification(client, sgp22.NotificationEventInstall)

	report("preflight", "正在检查 eUICC 剩余空间...", 10)
	beforeFreeNvramBytes := int32(0)
	{
		checkInfo := EUICCInfo{}
		m.parseEUICCInfo2ForEID(client, &checkInfo)
		beforeFreeNvramBytes = checkInfo.FreeNvramBytes
		if checkInfo.FreeNvramBytes > 0 && checkInfo.FreeNvramBytes < 81920 {
			return DownloadProfileResult{}, fmt.Errorf("已触发防炸卡保护拦截：目标 EID 剩余空间极度紧张（%d Bytes / %s，低于安全阈值 80KB）。请先删除多余的 Profile 释放空间后再试。",
				checkInfo.FreeNvramBytes, checkInfo.FreeNvram)
		}
		logger.Info("防炸卡预检通过", "device", m.deviceID, "freeNvram", checkInfo.FreeNvram)
	}

	imei, err := m.resolveDownloadIMEI(ctx, downloadIMEI)
	if err != nil {
		return DownloadProfileResult{}, err
	}

	smdpAddr := strings.TrimSpace(smdp)
	if smdpAddr == "" {
		return DownloadProfileResult{}, fmt.Errorf("SM-DP+ 地址不能为空")
	}
	if !strings.Contains(smdpAddr, "://") {
		smdpAddr = "https://" + smdpAddr
	}
	parsedURL, err := url.Parse(smdpAddr)
	if err != nil || parsedURL.Host == "" {
		return DownloadProfileResult{}, fmt.Errorf("无效的 SM-DP+ 地址 %q", smdp)
	}

	activationCode := &lpa.ActivationCode{
		SMDP:             &url.URL{Scheme: "https", Host: parsedURL.Host},
		MatchingID:       strings.TrimSpace(matchingID),
		IMEI:             imei,
		ConfirmationCode: strings.TrimSpace(confirmationCode),
	}

	logger.Info("开始下载 eSIM profile",
		"device", m.deviceID,
		"smdp", parsedURL.Host,
		"matching_id_present", strings.TrimSpace(matchingID) != "",
		"AID", redactIdentifier(aidHex))

	installStarted := false
	opts := &lpa.DownloadOptions{
		OnProgress: func(stage lpa.DownloadStage) {
			switch stage {
			case lpa.DownloadStageAuthenticateClient:
				report("auth_client", "正在向 SM-DP+ 进行客户端身份认证...", 30)
			case lpa.DownloadStageAuthenticateServer:
				report("auth_server", "正在向 SM-DP+ 请求 Profile 数据包...", 60)
			case lpa.DownloadStageInstall:
				installStarted = true
				report("install", "正在将 Profile 写入 eUICC...", 80)
			}
		},
	}
	downloadResult, err := client.DownloadProfile(ctx, activationCode, opts)
	if err != nil {
		downloadErr := NewDownloadProfileError(err)
		logger.Warn("下载 eSIM profile 失败",
			"device", m.deviceID,
			"smdp", parsedURL.Host,
			"matching_id_present", strings.TrimSpace(matchingID) != "",
			"AID", redactIdentifier(aidHex),
			"freeNvram_before", beforeFreeNvramBytes,
			"error_code", downloadErr.Code,
			"bpp_command_id", downloadErr.BPPCommandID,
			"bpp_error_reason", downloadErr.BPPErrorReason,
			"details", downloadErr.Details,
			"err", err)
		if installStarted && preDownloadNotificationsErr == nil {
			report("notify", "安装结果异常，正在确认并发送下载通知...", 90)
			m.closeLPAClientForOperation("download_profile_finalize_error", client)
			client = nil
			if recovered, ok := m.recoverDownloadInstallFinalizeError(ctx, targetAID, preDownloadNotifications, err); ok {
				result = recovered
				m.invalidateOverviewCache("download_profile_recovered")
				m.beginOverviewReloadSuppression(postDownloadOverviewSettleDelay)
				m.triggerOverviewReload("download_profile_recovered")
				return result, nil
			}
		}
		m.invalidateOverviewCache("download_profile_failed")
		return DownloadProfileResult{}, downloadErr
	}

	report("notify", "正在向运营商发送下载通知...", 90)
	lastSeq := downloadNotificationBaseline(preDownloadNotifications, downloadResultNotificationMetadata(downloadResult))
	result = m.sendDownloadInstallNotification(client, lastSeq, 300*time.Millisecond)

	afterFreeNvramBytes := m.readFreeNvramBytesWithRetry(client, 3, 300*time.Millisecond)
	result.SpaceDelta = buildSpaceDeltaForOperation(spaceDeltaOperationDownload, beforeFreeNvramBytes, afterFreeNvramBytes)

	logger.Info("eSIM profile 下载完成",
		"device", m.deviceID,
		"AID", aidHex,
		"warning_code", result.WarningCode,
		"space_delta", result.SpaceDelta)

	m.invalidateOverviewCache("download_profile")
	m.beginOverviewReloadSuppression(postDownloadOverviewSettleDelay)
	m.triggerOverviewReload("download_profile")

	return result, nil
}

func (m *Manager) resolveDownloadIMEI(ctx context.Context, downloadIMEI string) (string, error) {
	if strings.TrimSpace(downloadIMEI) != "" {
		imei, err := validateDownloadIMEI(downloadIMEI)
		if err != nil {
			return "", err
		}
		return imei, nil
	}
	if m.imeiProvider != nil {
		imei, err := m.imeiProvider(ctx)
		imei = strings.TrimSpace(imei)
		if imei != "" {
			return imei, nil
		}
		if err != nil {
			logger.Warn("eSIM 下载获取设备 IMEI 失败", "device", m.deviceID, "err", err)
		}
	}
	if m.backend != nil {
		imei, err := m.backend.GetIMEI(ctx)
		imei = strings.TrimSpace(imei)
		if imei != "" {
			return imei, nil
		}
		if err != nil {
			logger.Warn("eSIM 下载获取设备 IMEI 失败",
				"device", m.deviceID,
				"backend", m.backendMode(),
				"err", err)
		}
	}
	return "", fmt.Errorf("无法获取设备 IMEI")
}

func validateDownloadIMEI(imei string) (string, error) {
	imei = strings.TrimSpace(imei)
	if len(imei) != 15 {
		return "", fmt.Errorf("无效的 IMEI：必须为 15 位数字")
	}
	for _, r := range imei {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("无效的 IMEI：必须为 15 位数字")
		}
	}
	if imei[14] != imeiLuhnCheckDigit(imei[:14]) {
		return "", fmt.Errorf("无效的 IMEI：校验位不正确")
	}
	return imei, nil
}

func imeiLuhnCheckDigit(base string) byte {
	sum := 0
	double := true
	for i := len(base) - 1; i >= 0; i-- {
		n := int(base[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return byte('0' + (10-sum%10)%10)
}

// RenameProfile 修改指定 ICCID 的 eSIM profile 名称（Nickname）
// aidHex 可选，前端已知时直接传入可跳过全量 AID 遍历
func (m *Manager) RenameProfile(targetICCID string, newName string, aidHex string) error {
	m.opMu.Lock()
	writeStarted := time.Now()
	defer func() {
		m.logWriteOperationHold("rename_profile", writeStarted)
		m.opMu.Unlock()
		m.notifyWriteDone()
	}()

	iccid, err := sgp22.NewICCID(targetICCID)
	if err != nil {
		return fmt.Errorf("无效的 ICCID %q: %w", targetICCID, err)
	}

	// 1. 确定 ICCID 所属的 AID
	var targetAID []byte
	if aidHex != "" {
		targetAID, err = hex.DecodeString(aidHex)
		if err != nil {
			return fmt.Errorf("无效的 AID hex %q: %w", aidHex, err)
		}
	} else {
		// 回退：遍历查找
		targetAID, err = m.findAIDForICCID(targetICCID)
		if err != nil {
			return err
		}
	}

	// 2. 创建 LPA client
	client, err := m.createLPAWithAID(targetAID)
	if err != nil {
		return err
	}
	defer m.closeLPAClientForOperation("rename_profile", client)

	// 3. 设置新名称
	if err := client.SetNickname(iccid, newName); err != nil {
		return fmt.Errorf("修改 profile 名称失败: %w", err)
	}

	logger.Info("eSIM profile 名称修改成功",
		"device", m.deviceID,
		"ICCID", targetICCID,
		"newName", newName)
	m.invalidateOverviewCache("rename_profile")
	m.triggerOverviewReload("rename_profile")
	return nil
}

// DeleteProfile 删除指定 ICCID 的 eSIM profile
// aidHex 可选，前端已知时直接传入可跳过全量 AID 遍历
func (m *Manager) DeleteProfile(targetICCID string, aidHex string) (DeleteProfileResult, error) {
	m.opMu.Lock()
	writeStarted := time.Now()
	defer func() {
		m.logWriteOperationHold("delete_profile", writeStarted)
		m.opMu.Unlock()
		m.notifyWriteDone()
	}()

	result := DeleteProfileResult{}
	iccid, err := sgp22.NewICCID(targetICCID)
	if err != nil {
		return DeleteProfileResult{}, NewDeleteProfileError(
			DeleteProfileErrorInvalidICCID,
			fmt.Sprintf("无效的 ICCID %q: %v", targetICCID, err),
			err,
		)
	}

	var targetAID []byte
	if aidHex != "" {
		targetAID, err = hex.DecodeString(aidHex)
		if err != nil {
			return DeleteProfileResult{}, NewDeleteProfileError(
				DeleteProfileErrorInvalidAIDHex,
				fmt.Sprintf("无效的 AID hex %q: %v", aidHex, err),
				err,
			)
		}
	} else {
		targetAID, err = m.findAIDForICCID(targetICCID)
		if err != nil {
			return DeleteProfileResult{}, err
		}
	}

	client, err := m.createLPAWithAID(targetAID)
	if err != nil {
		return DeleteProfileResult{}, NewDeleteProfileError(
			DeleteProfileErrorInternal,
			fmt.Sprintf("创建 LPA client 失败: %v", err),
			err,
		)
	}
	defer m.closeLPAClientForOperation("delete_profile", client)

	logger.Info("开始删除 eSIM profile",
		"device", m.deviceID,
		"ICCID", targetICCID,
		"AID", fmt.Sprintf("%X", targetAID))

	beforeFreeNvramBytes := m.readFreeNvramBytes(client)

	var lastSeq sgp22.SequenceNumber
	currentNotifications, nerr := safeListNotification(client)
	if nerr == nil {
		for _, n := range currentNotifications {
			if n.SequenceNumber > lastSeq {
				lastSeq = n.SequenceNumber
			}
		}
	}

	if err := client.DeleteProfile(iccid); err != nil {
		return DeleteProfileResult{}, NewDeleteProfileError(
			DeleteProfileErrorInternal,
			fmt.Sprintf("删除 profile %s 失败: %v", targetICCID, err),
			err,
		)
	}

	var pendingNotifications []*sgp22.PendingNotification
	result = m.resolveDeleteNotificationResult(func() ([]*sgp22.NotificationMetadata, error) {
		return safeListNotification(client, sgp22.NotificationEventDelete)
	}, lastSeq, iccid, 300*time.Millisecond, func(seq sgp22.SequenceNumber) error {
		logger.Info("发送删除通知",
			"device", m.deviceID,
			"sequence", seq)
		return retryWithBackoff(3, 300*time.Millisecond,
			func(attempt int, wait time.Duration, err error) {
				logger.Warn("获取通知列表失败，稍后重试",
					"device", m.deviceID,
					"sequence", seq,
					"attempt", fmt.Sprintf("%d/%d", attempt, 3),
					"wait_ms", wait.Milliseconds(),
					"err", err)
			},
			func() error {
				var err error
				pendingNotifications, err = safeRetrieveNotificationList(client, seq)
				return err
			},
		)
	}, func(seq sgp22.SequenceNumber) error {
		for _, notif := range pendingNotifications {
			herr := retryWithBackoff(3, 300*time.Millisecond,
				func(attempt int, wait time.Duration, err error) {
					logger.Warn("处理删除通知失败，稍后重试",
						"device", m.deviceID,
						"sequence", seq,
						"attempt", fmt.Sprintf("%d/%d", attempt, 3),
						"wait_ms", wait.Milliseconds(),
						"err", err)
				},
				func() error {
					return client.HandleNotification(notif)
				},
			)
			if herr != nil {
				logger.Warn("处理删除通知失败，已达到重试上限",
					"device", m.deviceID,
					"sequence", seq,
					"err", herr)
				return herr
			}
		}
		return nil
	})

	afterFreeNvramBytes := m.readFreeNvramBytesWithRetry(client, 3, 300*time.Millisecond)
	result.SpaceDelta = buildSpaceDeltaForOperation(spaceDeltaOperationDelete, beforeFreeNvramBytes, afterFreeNvramBytes)

	logger.Info("eSIM profile 删除完成",
		"device", m.deviceID,
		"ICCID", targetICCID,
		"delete_ok", true,
		"notification_ok", result.WarningCode == "",
		"notification_reason", result.WarningCode,
		"space_delta", result.SpaceDelta)

	m.invalidateOverviewCache("delete_profile")
	m.triggerOverviewReload("delete_profile")

	return result, nil
}

func findMatchingDeleteNotification(notifications []*sgp22.NotificationMetadata, lastSeq sgp22.SequenceNumber, iccid sgp22.ICCID) *sgp22.NotificationMetadata {
	for _, n := range notifications {
		if n == nil {
			continue
		}
		if n.SequenceNumber > lastSeq && bytes.Equal(n.ICCID, iccid) {
			return n
		}
	}
	return nil
}

func (m *Manager) findDeleteNotificationWithWait(listFn func() ([]*sgp22.NotificationMetadata, error), lastSeq sgp22.SequenceNumber, iccid sgp22.ICCID, attempts int, delay time.Duration) (*sgp22.NotificationMetadata, bool) {
	for attempt := 0; attempt < attempts; attempt++ {
		notifications, err := listFn()
		if err == nil {
			if notification := findMatchingDeleteNotification(notifications, lastSeq, iccid); notification != nil {
				return notification, true
			}
		}
		if attempt < attempts-1 {
			time.Sleep(delay)
		}
	}
	return nil, false
}

func (m *Manager) resolveDeleteNotificationResult(listFn func() ([]*sgp22.NotificationMetadata, error), lastSeq sgp22.SequenceNumber, iccid sgp22.ICCID, delay time.Duration, retrieveFn func(seq sgp22.SequenceNumber) error, handleFn func(seq sgp22.SequenceNumber) error) DeleteProfileResult {
	notification, found := m.findDeleteNotificationWithWait(listFn, lastSeq, iccid, 3, delay)
	if !found || notification == nil {
		return deleteNotificationResult(false, nil, nil)
	}
	if err := retrieveFn(notification.SequenceNumber); err != nil {
		return deleteNotificationResult(true, err, nil)
	}
	if err := handleFn(notification.SequenceNumber); err != nil {
		return deleteNotificationResult(true, nil, err)
	}
	return DeleteProfileResult{}
}

func findMatchingNotificationAfterSeq(notifications []*sgp22.NotificationMetadata, lastSeq sgp22.SequenceNumber) *sgp22.NotificationMetadata {
	for _, n := range notifications {
		if n == nil {
			continue
		}
		if n.SequenceNumber > lastSeq {
			return n
		}
	}
	return nil
}

func (m *Manager) findNotificationWithWait(listFn func() ([]*sgp22.NotificationMetadata, error), lastSeq sgp22.SequenceNumber, attempts int, delay time.Duration) (*sgp22.NotificationMetadata, bool) {
	for attempt := 0; attempt < attempts; attempt++ {
		notifications, err := listFn()
		if err == nil {
			if notification := findMatchingNotificationAfterSeq(notifications, lastSeq); notification != nil {
				return notification, true
			}
		}
		if attempt < attempts-1 {
			time.Sleep(delay)
		}
	}
	return nil, false
}

func (m *Manager) resolveDownloadNotificationResult(listFn func() ([]*sgp22.NotificationMetadata, error), lastSeq sgp22.SequenceNumber, delay time.Duration, retrieveFn func(seq sgp22.SequenceNumber) error, handleFn func(seq sgp22.SequenceNumber) error) DownloadProfileResult {
	notification, found := m.findNotificationWithWait(listFn, lastSeq, 3, delay)
	if !found || notification == nil {
		return downloadNotificationResult(false, nil, nil)
	}
	if err := retrieveFn(notification.SequenceNumber); err != nil {
		return downloadNotificationResult(true, err, nil)
	}
	if err := handleFn(notification.SequenceNumber); err != nil {
		return downloadNotificationResult(true, nil, err)
	}
	return DownloadProfileResult{}
}

func (m *Manager) sendDownloadInstallNotification(client *lpa.Client, lastSeq sgp22.SequenceNumber, delay time.Duration) DownloadProfileResult {
	var pendingNotifications []*sgp22.PendingNotification
	return m.resolveDownloadNotificationResult(func() ([]*sgp22.NotificationMetadata, error) {
		return safeListNotification(client, sgp22.NotificationEventInstall)
	}, lastSeq, delay, func(seq sgp22.SequenceNumber) error {
		logger.Info("发送下载通知",
			"device", m.deviceID,
			"sequence", seq)
		return retryWithBackoff(3, 300*time.Millisecond,
			func(attempt int, wait time.Duration, err error) {
				logger.Warn("获取下载通知列表失败",
					"device", m.deviceID,
					"sequence", seq,
					"attempt", fmt.Sprintf("%d/%d", attempt, 3),
					"wait_ms", wait.Milliseconds(),
					"err", err)
			},
			func() error {
				var err error
				pendingNotifications, err = safeRetrieveNotificationList(client, seq)
				return err
			},
		)
	}, func(seq sgp22.SequenceNumber) error {
		for _, notification := range pendingNotifications {
			herr := retryWithBackoff(3, 300*time.Millisecond,
				func(attempt int, wait time.Duration, err error) {
					logger.Warn("处理下载通知失败，稍后重试",
						"device", m.deviceID,
						"sequence", seq,
						"attempt", fmt.Sprintf("%d/%d", attempt, 3),
						"wait_ms", wait.Milliseconds(),
						"err", err)
				},
				func() error {
					return client.HandleNotification(notification)
				},
			)
			if herr != nil {
				logger.Warn("处理下载通知失败，已达到重试上限",
					"device", m.deviceID,
					"sequence", seq,
					"err", herr)
				return herr
			}
		}
		return nil
	})
}

func (m *Manager) recoverDownloadInstallFinalizeError(ctx context.Context, targetAID []byte, preDownloadNotifications []*sgp22.NotificationMetadata, downloadErr error) (DownloadProfileResult, bool) {
	if len(targetAID) == 0 || !isRecoverableDownloadInstallFinalizeError(downloadErr) {
		return DownloadProfileResult{}, false
	}
	if err := ctx.Err(); err != nil {
		return DownloadProfileResult{}, false
	}
	client, err := m.createLPAWithAID(targetAID)
	if err != nil {
		logger.Warn("下载收尾异常后重建 LPA client 失败，无法恢复确认",
			"device", m.deviceID,
			"AID", fmt.Sprintf("%X", targetAID),
			"err", err)
		return DownloadProfileResult{}, false
	}
	defer m.closeLPAClientForOperation("download_finalize_recovery", client)

	lastSeq := downloadNotificationBaseline(preDownloadNotifications, nil)
	notificationResult := m.sendDownloadInstallNotification(client, lastSeq, 300*time.Millisecond)
	if strings.TrimSpace(notificationResult.WarningCode) != "" {
		logger.Warn("下载收尾异常后未能确认安装通知，保持下载失败",
			"device", m.deviceID,
			"AID", fmt.Sprintf("%X", targetAID),
			"warning_code", notificationResult.WarningCode,
			"err", downloadErr)
		return DownloadProfileResult{}, false
	}
	result := downloadFinalizeRecoveredResult()
	logger.Warn("下载收尾异常，但安装通知已发送，按成功处理",
		"device", m.deviceID,
		"AID", fmt.Sprintf("%X", targetAID),
		"warning_code", result.WarningCode,
		"err", downloadErr)
	return result, true
}

func retryWithBackoff(maxRetries int, baseDelay time.Duration, onRetry func(attempt int, wait time.Duration, err error), op func() error) error {
	delay := baseDelay
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := op(); err != nil {
			if attempt < maxRetries {
				if onRetry != nil {
					onRetry(attempt+1, delay, err)
				}
				time.Sleep(delay)
				delay *= 2
				continue
			}
			return err
		}
		return nil
	}
	return nil
}
