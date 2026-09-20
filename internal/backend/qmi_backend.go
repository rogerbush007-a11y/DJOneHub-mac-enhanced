package backend

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/manager"
	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"github.com/iniwex5/vohive/internal/modem"
	"github.com/iniwex5/vohive/pkg/logger"
	"github.com/iniwex5/vohive/pkg/smscodec"
	"github.com/warthog618/sms/encoding/tpdu"
)

// QMISource 定义了 QMI 后端需要从底层 QMI Core 提供者 (qmicore.Manager) 获得的所有功能接口。
// 这层抽象确保了 QMIBackend 与 qmicore.Manager 解耦，同时防止包循环引用。
type QMISource interface {
	GetDeviceSerialNumbers(ctx context.Context) (*qmi.DeviceInfo, error)
	GetDeviceRevision(ctx context.Context) (string, string, error)
	GetIMSI(ctx context.Context) (string, error)
	GetICCID(ctx context.Context) (string, error)
	GetMSISDN(ctx context.Context) (string, error)
	GetSIMStatus(ctx context.Context) (qmi.SIMStatus, error)
	GetUIMReadiness(ctx context.Context) (manager.UIMReadiness, error)
	GetServingSystem(ctx context.Context) (*qmi.ServingSystem, error)
	GetSignalStrength(ctx context.Context) (*qmi.SignalStrength, error)
	GetSignalInfo(ctx context.Context) (*qmi.SignalInfo, error)
	GetSysInfo(ctx context.Context) (*qmi.SysInfo, error)
	NASGetRFBandInfo(ctx context.Context) (*qmi.RFBandInfo, error)
	NASGetCellLocationInfo(ctx context.Context) (*qmi.CellLocationInfo, error)
	GetOperatingMode(ctx context.Context) (qmi.OperatingMode, error)
	SetOperatingMode(ctx context.Context, mode qmi.OperatingMode) error

	// WMS 短信相关
	WMSSendRawMessage(ctx context.Context, format uint8, pdu []byte) error
	WMSRawReadMessage(ctx context.Context, storageType uint8, index uint32) ([]byte, error)
	WMSDeleteMessage(ctx context.Context, storageType uint8, index uint32) error
	WMSListMessagesAuto(ctx context.Context, storageType uint8) ([]struct {
		Index uint32
		Tag   qmi.MessageTagType
	}, error)
	WMSDeleteMessagesByTag(ctx context.Context, storageType uint8, tag qmi.MessageTagType, mode qmi.MessageMode) error

	// UIM 鉴权相关 (与 eUICC 共用)
	OpenEUICCLogicalChannel(ctx context.Context, slot byte, aid []byte) (byte, error)
	CloseEUICCLogicalChannel(ctx context.Context, slot byte, channel byte) error
	TransmitEUICCAPDU(ctx context.Context, slot byte, channel byte, command []byte) ([]byte, error)
	UIMPowerOffSIM(ctx context.Context, slot uint8) error
	UIMPowerOnSIM(ctx context.Context, slot uint8) error
	UIMPostSwitchReload(ctx context.Context, readiness manager.UIMReadiness, opts manager.UIMPostSwitchReloadOptions) (uint8, error)
	EnsureSIMProvisioned(ctx context.Context, opts manager.EnsureSIMProvisionedOptions) (manager.UIMReadiness, error)

	// 获取原生 MCC 和 MNC
	GetNativeMCCMNC(ctx context.Context) (mcc, mnc string, err error)

	// 获取 SIM EF_SPN 服务提供商名称
	GetNativeSPN(ctx context.Context) (string, error)

	// 获取 SIM/eSIM profile 原生元数据
	GetSIMMetadata(ctx context.Context) (*qmi.SIMMetadata, error)

	// 获取短信中心号码（由底层 QMI 库实现）
	GetSMSC(ctx context.Context) (string, error)

	// 获取设备状态快照（由 NAS Indication 事件驱动更新，零 IPC）
	GetDeviceSnapshot() *manager.DeviceSnapshot
}

type qmiSIMAuthLogicalChannelSource interface {
	OpenSIMAuthLogicalChannel(ctx context.Context, slot byte, aid []byte) (byte, error)
	CloseSIMAuthLogicalChannel(ctx context.Context, slot byte, channel byte) error
}

type qmiSIMAuthAIDSource interface {
	GetUSIMAID(ctx context.Context) ([]byte, error)
	GetISIMAID(ctx context.Context) ([]byte, error)
}

var ErrSIMAuthAIDNotReady = errors.New("sim_auth_aid_not_ready")

type qmiEFADReader interface {
	UIMReadTransparentWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) ([]byte, error)
}

// QMIBackend QMI 后端 — 复用 qmicore.Manager 提供的 QMI 资源池通信
type QMIBackend struct {
	source      QMISource
	controlPath string // cdc-wdm 设备节点路径

	ussdOnce         sync.Once
	ussdInitErr      error
	ussdMu           sync.Mutex
	ussdResult       chan USSDResult
	ussdErr          chan error
	ussdRelease      chan struct{}
	ussdAwaitRelease bool
}

type NASRegisterRequest struct {
	Mode              string `json:"mode"`
	MCC               uint16 `json:"mcc,omitempty"`
	MNC               uint16 `json:"mnc,omitempty"`
	IncludesPCSDigit  bool   `json:"includes_pcs_digit,omitempty"`
	RadioAccessTech   uint8  `json:"radio_access_tech,omitempty"`
	ChangeDuration    uint8  `json:"change_duration,omitempty"`
	HasChangeDuration bool   `json:"has_change_duration,omitempty"`
}

type NASSignalConfigRequest struct {
	LTEEnabled    bool  `json:"lte_enabled"`
	LTERSRPDelta  uint8 `json:"lte_rsrp_delta,omitempty"`
	LTERSRQDelta  uint8 `json:"lte_rsrq_delta,omitempty"`
	LTESNRDelta   uint8 `json:"lte_snr_delta,omitempty"`
	NR5GEnabled   bool  `json:"nr5g_enabled"`
	NR5GRSRPDelta uint8 `json:"nr5g_rsrp_delta,omitempty"`
	NR5GRSRQDelta uint8 `json:"nr5g_rsrq_delta,omitempty"`
	NR5GSINRDelta uint8 `json:"nr5g_sinr_delta,omitempty"`
}

type NASOperatorInfo struct {
	ServiceProviderName string `json:"service_provider_name,omitempty"`
	OperatorStringName  string `json:"operator_string_name,omitempty"`
	PLMNLongName        string `json:"plmn_long_name,omitempty"`
	PLMNShortName       string `json:"plmn_short_name,omitempty"`
}

type nasControlSource interface {
	NASInitiateNetworkRegister(ctx context.Context, req qmi.NASInitiateNetworkRegisterRequest) error
	NASForceNetworkSearch(ctx context.Context) error
	NASAttachDetach(ctx context.Context, attached bool) error
	NASSetSystemSelectionPreference(ctx context.Context, pref qmi.SystemSelectionPreference) error
	NASGetOperatorName(ctx context.Context) (*qmi.NASOperatorNameInfo, error)
	NASGetPLMNName(ctx context.Context, req qmi.NASPLMNNameRequest) (*qmi.NASPLMNNameInfo, error)
	NASConfigSignalInfoV2(ctx context.Context, cfg qmi.NASSignalInfoConfigV2) error
	NASRegisterIndications(ctx context.Context, cfg qmi.NASIndicationRegistration) error
	NASPerformNetworkScan(ctx context.Context) ([]qmi.NetworkScanResult, error)
	NASIncrementalNetworkScanSnapshot() (*qmi.NASIncrementalNetworkScanInfo, time.Time, bool)
	NASGetSystemSelectionPreference(ctx context.Context) (*qmi.SystemSelectionPreference, error)
}

func (q *QMIBackend) nasSource() (nasControlSource, error) {
	src, ok := q.source.(nasControlSource)
	if !ok {
		return nil, fmt.Errorf("qmi source does not expose NAS control")
	}
	return src, nil
}

// NewQMIBackend 创建 QMI 后端
// source: QMI Core 资源提供者（通常是 qmicore.Manager）
func NewQMIBackend(controlPath string, source QMISource) (*QMIBackend, error) {
	if source == nil {
		return nil, fmt.Errorf("QMI 资源源不能为空")
	}

	return &QMIBackend{
		source:      source,
		controlPath: controlPath,
	}, nil
}

// Mode 返回后端模式标识
func (q *QMIBackend) Mode() string { return "qmi" }

// Close QMIBackend 现在不再持有独立的 Client，因此 Close 无需主动关闭资源
// 资源生命周期由 qmicore.Manager 统一控制
func (q *QMIBackend) Close() error {
	return nil
}

// ============================================================================
// DeviceInfoProvider 实现
// ============================================================================

func (q *QMIBackend) GetIMEI(ctx context.Context) (string, error) {
	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if ids, ready := snap.Identities(); ready && ids.IMEI != "" {
			return ids.IMEI, nil
		}
	}
	info, err := q.source.GetDeviceSerialNumbers(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(info.IMEI), nil
}

func (q *QMIBackend) GetIMSI(ctx context.Context) (string, error) {
	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if ids, ready := snap.Identities(); ready && ids.IMSI != "" {
			return ids.IMSI, nil
		}
	}
	return q.source.GetIMSI(ctx)
}

// GetIMSILive 强制实时读取 IMSI（绕过 snapshot 优先路径）。
func (q *QMIBackend) GetIMSILive(ctx context.Context) (string, error) {
	type strictLive interface {
		GetIMSIStrictLive(context.Context) (string, error)
	}
	if src, ok := q.source.(strictLive); ok {
		return src.GetIMSIStrictLive(ctx)
	}
	return q.source.GetIMSI(ctx)
}

func (q *QMIBackend) GetICCID(ctx context.Context) (string, error) {
	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if ids, ready := snap.Identities(); ready && ids.ICCID != "" {
			return ids.ICCID, nil
		}
	}
	return q.source.GetICCID(ctx)
}

func (q *QMIBackend) GetMSISDN(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	msisdn, err := q.source.GetMSISDN(ctx)
	if err != nil {
		return "", err
	}
	msisdn = strings.TrimSpace(msisdn)
	if msisdn == "" {
		return "", nil
	}
	allDigits := true
	for i := 0; i < len(msisdn); i++ {
		if msisdn[i] < '0' || msisdn[i] > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return "+" + msisdn, nil
	}
	return msisdn, nil
}

// GetICCIDLive 强制实时读取 ICCID（绕过 snapshot 优先路径）。
func (q *QMIBackend) GetICCIDLive(ctx context.Context) (string, error) {
	type strictLive interface {
		GetICCIDStrictLive(context.Context) (string, error)
	}
	if src, ok := q.source.(strictLive); ok {
		return src.GetICCIDStrictLive(ctx)
	}
	return q.source.GetICCID(ctx)
}

func (q *QMIBackend) GetRevision(ctx context.Context) (string, error) {
	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if ids, ready := snap.Identities(); ready && ids.FirmwareRevision != "" {
			return ids.FirmwareRevision, nil
		}
	}
	rev, _, err := q.source.GetDeviceRevision(ctx)
	return rev, err
}

// qmiSNRToDB 把 QMI NAS 的 SNR（0.1 dB 缩放整数，如 134 = 13.4 dB）四舍五入为 dB 整数。
// LTE RSSNR 与 5G NR SINR 在 QMI 里均为该编码；RSRP/RSRQ 则是直接的 dBm/dB 值，无需换算。
func qmiSNRToDB(raw int16) int {
	if raw >= 0 {
		return int((raw + 5) / 10)
	}
	return int((raw - 5) / 10)
}

func (q *QMIBackend) GetSignalInfo(ctx context.Context) (*SignalInfo, error) {
	info := &SignalInfo{}
	hasSnapshotData := false

	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if sigInfo, _, valid := snap.NASSignalInfo(); valid && sigInfo != nil {
			if sigInfo.LTERSRP != 0 {
				info.RSRP = int(sigInfo.LTERSRP)
			}
			if sigInfo.LTERSRQ != 0 {
				info.RSRQ = int(sigInfo.LTERSRQ)
			}
			if sigInfo.LTERSSNR != 0 {
				info.SINR = qmiSNRToDB(sigInfo.LTERSSNR)
			}
			if sigInfo.NR5GRSRP != 0 {
				info.NR5GRSRP = int(sigInfo.NR5GRSRP)
			}
			if sigInfo.NR5GRSRQ != 0 {
				info.NR5GRSRQ = int(sigInfo.NR5GRSRQ)
			}
			if sigInfo.NR5GSINR != 0 {
				info.NR5GSINR = qmiSNRToDB(sigInfo.NR5GSINR)
			}
			hasSnapshotData = true
		}
		if sig, _ := snap.Signal(); sig != nil {
			info.RSSI = int(sig.RSSI)
			if info.RSRP == 0 && sig.RSRP != 0 {
				info.RSRP = int(sig.RSRP)
			}
			if info.RSRQ == 0 && sig.RSRQ != 0 {
				info.RSRQ = int(sig.RSRQ)
			}
			hasSnapshotData = true
		}
	}

	if hasSnapshotData {
		return info, nil
	}

	// 首先使用 NAS GetSignalInfo（字段最全，1 次 IPC）
	if sigInfo, err := q.source.GetSignalInfo(ctx); err == nil && sigInfo != nil {
		if sigInfo.LTERSRP != 0 {
			info.RSRP = int(sigInfo.LTERSRP)
		}
		if sigInfo.LTERSRQ != 0 {
			info.RSRQ = int(sigInfo.LTERSRQ)
		}
		if sigInfo.LTERSSNR != 0 {
			info.SINR = qmiSNRToDB(sigInfo.LTERSSNR)
		}
		// 5G
		if sigInfo.NR5GRSRP != 0 {
			info.NR5GRSRP = int(sigInfo.NR5GRSRP)
		}
		if sigInfo.NR5GRSRQ != 0 {
			info.NR5GRSRQ = int(sigInfo.NR5GRSRQ)
		}
		if sigInfo.NR5GSINR != 0 {
			info.NR5GSINR = qmiSNRToDB(sigInfo.NR5GSINR)
		}
	}

	// 层 3：仅当 RSSI 仍为空时才补发 GetSignalStrength（从 2 次 IPC 降为条件性 1 次）
	if info.RSSI == 0 {
		if sig, err := q.source.GetSignalStrength(ctx); err == nil && sig != nil {
			info.RSSI = int(sig.RSSI)
			if info.RSRP == 0 && sig.RSRP != 0 {
				info.RSRP = int(sig.RSRP)
			}
			if info.RSRQ == 0 && sig.RSRQ != 0 {
				info.RSRQ = int(sig.RSRQ)
			}
		}
	}

	return info, nil
}

func (q *QMIBackend) GetServingSystem(ctx context.Context) (*ServingSystem, error) {
	ss := &ServingSystem{}

	var (
		serving          *qmi.ServingSystem
		err              error
		fromSnapshot     bool
		snapshotUpdateAt time.Time
		operatorRetried  bool
	)
	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if cached, ts := snap.ServingSystem(); cached != nil {
			serving = cached
			fromSnapshot = true
			snapshotUpdateAt = ts
		}
	}
	if fromSnapshot && serving != nil {
		stale := snapshotUpdateAt.IsZero() || time.Since(snapshotUpdateAt) > 30*time.Second
		suspicious := serving.RegistrationState == qmi.RegStateNotRegistered &&
			(serving.MCC != 0 || serving.MNC != 0 || serving.RadioInterface != 0)
		if stale || suspicious {
			// reason := "stale"
			// if suspicious {
			// 	reason = "suspicious"
			// }
			// logger.Debug("QMI serving snapshot 回源校验",
			// 	"reason", reason,
			// 	"reg_state", serving.RegistrationState.String(),
			// 	"mcc", serving.MCC,
			// 	"mnc", serving.MNC,
			// 	"rat", serving.RadioInterface)
			if live, liveErr := q.source.GetServingSystem(ctx); liveErr == nil && live != nil {
				serving = live
			}
		}
	}
	if serving == nil {
		// 直接发起 NAS GetServingSystem 请求
		serving, err = q.source.GetServingSystem(ctx)
		if err != nil {
			return nil, err
		}
	}

	// 映射 QMI RegistrationState → AT-style regStatus
	switch serving.RegistrationState {
	case qmi.RegStateNotRegistered:
		ss.RegStatus = 0
		ss.RegStatusText = "未注册"
	case qmi.RegStateRegistered:
		ss.RegStatus = 1
		ss.RegStatusText = "已注册(本地)"
		ss.PSAttached = serving.PSAttached
	case qmi.RegStateRoaming:
		ss.RegStatus = 5
		ss.RegStatusText = "已注册(漫游)"
		ss.PSAttached = serving.PSAttached
	case qmi.RegStateSearching:
		ss.RegStatus = 2
		ss.RegStatusText = "搜索中"
	case qmi.RegStateDenied:
		ss.RegStatus = 3
		ss.RegStatusText = "注册被拒"
	default:
		ss.RegStatus = 4
		ss.RegStatusText = "未知"
	}

	// PLMN
	ss.MCC = serving.MCC
	ss.MNC = serving.MNC
	if serving.MCC > 0 {
		ss.Operator = qmiOperatorDisplay(serving.MCC, serving.MNC)
	}
	if (ss.RegStatus == 1 || ss.RegStatus == 5) && strings.TrimSpace(ss.Operator) == "" && !operatorRetried {
		logger.Debug("QMI serving 命中已注册但运营商为空，触发一次回源",
			"reg_status", ss.RegStatus,
			"mcc", ss.MCC,
			"mnc", ss.MNC,
			"rat", serving.RadioInterface)
		operatorRetried = true
		if live, liveErr := q.source.GetServingSystem(ctx); liveErr == nil && live != nil {
			serving = live
			ss.MCC = serving.MCC
			ss.MNC = serving.MNC
			if serving.MCC > 0 {
				ss.Operator = qmiOperatorDisplay(serving.MCC, serving.MNC)
			}
		}
	}

	// 网络模式映射 (基于 QmiNasRadioInterface 标准)
	switch serving.RadioInterface {
	case 0x01, 0x02:
		ss.NetworkMode = "CDMA"
	case 0x03:
		ss.NetworkMode = "AMPS"
	case 0x04:
		ss.NetworkMode = "GSM"
	case 0x05, 0x09:
		ss.NetworkMode = "UMTS"
	case 0x08:
		ss.NetworkMode = "LTE"
		if bandInfo, bandErr := q.source.NASGetRFBandInfo(ctx); bandErr == nil {
			ss.NetworkDuplex = qmi.GetLTEDuplexModeFromBandInfo(bandInfo)
			ss.RadioBand, ss.RadioChannel = qmiRadioBandAndChannel(bandInfo)
		}
		if ss.NetworkDuplex == "" {
			if cellInfo, cellErr := q.source.NASGetCellLocationInfo(ctx); cellErr == nil {
				ss.NetworkDuplex = qmi.GetLTEDuplexModeFromCellLocation(cellInfo)
			}
		}
	case 0x0C:
		ss.NetworkMode = "NR5G"
	default:
		ss.NetworkMode = "Unknown"
	}

	// 尝试获取 SysInfo（LAC/CellID）
	var sysInfo *qmi.SysInfo
	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if cached, _ := snap.SysInfo(); cached != nil {
			sysInfo = cached
		}
	}
	if sysInfo == nil {
		sysInfo, _ = q.source.GetSysInfo(ctx)
	}

	if sysInfo != nil {
		if sysInfo.TAC > 0 {
			ss.LAC = fmt.Sprintf("%04X", sysInfo.TAC)
		} else if sysInfo.LAC > 0 {
			ss.LAC = fmt.Sprintf("%04X", sysInfo.LAC)
		}
		if sysInfo.CellID > 0 {
			ss.CellID = fmt.Sprintf("%X", sysInfo.CellID)
		}
	}

	return ss, nil
}

func qmiRadioBandAndChannel(info *qmi.RFBandInfo) (string, uint32) {
	if info == nil {
		return "", 0
	}
	for _, band := range info.Bands {
		if band.RadioInterface == 0x08 {
			return fmt.Sprintf("LTE BAND %d", band.ActiveBandClass), band.ActiveChannel
		}
	}
	for _, band := range info.Bands {
		if band.RadioInterface != 0 {
			return fmt.Sprintf("RAT %d BAND %d", band.RadioInterface, band.ActiveBandClass), band.ActiveChannel
		}
	}
	return "", 0
}

func qmiOperatorDisplay(mcc, mnc uint16) string {
	if mcc == 0 {
		return ""
	}

	plmn5 := fmt.Sprintf("%03d%02d", mcc, mnc)
	if name, ok := modem.LookupServingOperatorNameFromPLMN(plmn5); ok {
		return name
	}

	plmn6 := fmt.Sprintf("%03d%03d", mcc, mnc)
	if name, ok := modem.LookupServingOperatorNameFromPLMN(plmn6); ok {
		return name
	}

	if mnc >= 100 {
		return plmn6
	}
	return plmn5
}

func (q *QMIBackend) IsSimInserted(ctx context.Context) (bool, error) {
	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if ids, ready := snap.Identities(); ready && ids.SimInserted != nil {
			return *ids.SimInserted, nil
		}
	}
	status, err := q.source.GetSIMStatus(ctx)
	if err != nil {
		return false, err
	}
	return status != qmi.SIMAbsent, nil
}

func (q *QMIBackend) GetUIMReadiness(ctx context.Context) (manager.UIMReadiness, error) {
	return q.source.GetUIMReadiness(ctx)
}

func (q *QMIBackend) RequestCoreRecovery(reason string) bool {
	type coreRecoveryRequester interface {
		RequestCoreRecovery(reason string) bool
	}
	if q == nil || q.source == nil {
		return false
	}
	requester, ok := q.source.(coreRecoveryRequester)
	return ok && requester.RequestCoreRecovery(reason)
}

func (q *QMIBackend) WaitCoreReady(ctx context.Context) error {
	type coreReadyWaiter interface {
		WaitCoreReady(ctx context.Context) error
	}
	if q == nil || q.source == nil {
		return fmt.Errorf("qmi_source_not_available")
	}
	waiter, ok := q.source.(coreReadyWaiter)
	if !ok {
		return fmt.Errorf("qmi_core_ready_wait_not_supported")
	}
	return waiter.WaitCoreReady(ctx)
}

func (q *QMIBackend) GetNativeMCCMNC(ctx context.Context) (mcc, mnc string, err error) {
	imsi, err := q.source.GetIMSI(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to get IMSI: %w", err)
	}

	var efAD []byte
	if reader, ok := q.source.(qmiEFADReader); ok {
		efAD, _ = readQMIEFAD(ctx, reader)
	}

	mcc, mnc, _, _, err = modem.HomeMCCMNCFromIMSIAndEFAD(imsi, efAD)
	return mcc, mnc, err
}

func readQMIEFAD(ctx context.Context, reader qmiEFADReader) ([]byte, error) {
	paths := [][]byte{
		{0x00, 0x3F, 0xFF, 0x7F},
		{0x20, 0x7F},
		{},
	}
	var lastErr error
	for _, path := range paths {
		data, err := reader.UIMReadTransparentWithSession(ctx, qmi.UIMSessionTypePrimaryGWProvisioning, 0x6FAD, path)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("EF_AD read failed")
	}
	return nil, lastErr
}

func (q *QMIBackend) GetNativeSPN(ctx context.Context) (string, error) {
	return q.source.GetNativeSPN(ctx)
}

func (q *QMIBackend) GetNativeSPNLive(ctx context.Context) (string, error) {
	return q.source.GetNativeSPN(ctx)
}

func (q *QMIBackend) GetSIMMetadata(ctx context.Context) (*SIMMetadata, error) {
	meta, err := q.source.GetSIMMetadata(ctx)
	out := mapQMISIMMetadata(meta)
	if out != nil {
		if mcc, mnc, homeErr := q.GetNativeMCCMNC(ctx); homeErr == nil {
			out.NativeMCC = mcc
			out.NativeMNC = mnc
		}
	}
	return out, err
}

func (q *QMIBackend) GetSIMMetadataLive(ctx context.Context) (*SIMMetadata, error) {
	return q.GetSIMMetadata(ctx)
}

func mapQMISIMMetadata(meta *qmi.SIMMetadata) *SIMMetadata {
	if meta == nil {
		return nil
	}
	out := &SIMMetadata{
		NativeMCC: meta.NativeMCC,
		NativeMNC: meta.NativeMNC,
		GID1:      meta.GID1,
		GID2:      meta.GID2,
	}
	if len(meta.PNN) > 0 {
		out.PNN = make([]PNNRecord, 0, len(meta.PNN))
		for _, rec := range meta.PNN {
			out.PNN = append(out.PNN, PNNRecord(rec))
		}
	}
	if len(meta.OPL) > 0 {
		out.OPL = make([]OPLRecord, 0, len(meta.OPL))
		for _, rec := range meta.OPL {
			out.OPL = append(out.OPL, OPLRecord(rec))
		}
	}
	if meta.ServiceTable != nil {
		out.ServiceTable = (*SIMServiceTable)(meta.ServiceTable)
	}
	return out
}

func (q *QMIBackend) NASAttachDetach(ctx context.Context, attached bool) error {
	src, err := q.nasSource()
	if err != nil {
		return err
	}
	return src.NASAttachDetach(ctx, attached)
}

func (q *QMIBackend) NASInitiateNetworkRegister(ctx context.Context, req NASRegisterRequest) error {
	src, err := q.nasSource()
	if err != nil {
		return err
	}
	mode := qmi.NASNetworkRegisterAutomatic
	if strings.EqualFold(strings.TrimSpace(req.Mode), "manual") {
		mode = qmi.NASNetworkRegisterManual
	}
	return src.NASInitiateNetworkRegister(ctx, qmi.NASInitiateNetworkRegisterRequest{
		Mode:              mode,
		MCC:               req.MCC,
		MNC:               req.MNC,
		IncludesPCSDigit:  req.IncludesPCSDigit,
		RadioAccessTech:   req.RadioAccessTech,
		ChangeDuration:    req.ChangeDuration,
		HasChangeDuration: req.HasChangeDuration,
	})
}

func (q *QMIBackend) NASForceNetworkSearch(ctx context.Context) error {
	src, err := q.nasSource()
	if err != nil {
		return err
	}
	return src.NASForceNetworkSearch(ctx)
}

func (q *QMIBackend) NASSetSystemSelectionAutomatic(ctx context.Context) error {
	src, err := q.nasSource()
	if err != nil {
		return err
	}
	return src.NASSetSystemSelectionPreference(ctx, qmi.SystemSelectionPreference{
		NetworkSelectionPreference:    qmi.NASNetworkSelectionAutomatic,
		HasNetworkSelectionPreference: true,
		ChangeDuration:                qmi.NASChangeDurationPermanent,
		HasChangeDuration:             true,
	})
}

func (q *QMIBackend) NASGetOperatorInfo(ctx context.Context) (*NASOperatorInfo, error) {
	src, err := q.nasSource()
	if err != nil {
		return nil, err
	}

	info := &NASOperatorInfo{}
	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if op, _, valid := snap.NASOperatorName(); valid && op != nil {
			info.ServiceProviderName = strings.TrimSpace(op.ServiceProviderName)
			info.OperatorStringName = strings.TrimSpace(op.OperatorStringName)
		}
	}
	if info.ServiceProviderName == "" && info.OperatorStringName == "" {
		if op, opErr := src.NASGetOperatorName(ctx); opErr == nil && op != nil {
			info.ServiceProviderName = strings.TrimSpace(op.ServiceProviderName)
			info.OperatorStringName = strings.TrimSpace(op.OperatorStringName)
		}
	}

	var serving *qmi.ServingSystem
	if snap := q.source.GetDeviceSnapshot(); snap != nil {
		if cached, _ := snap.ServingSystem(); cached != nil {
			serving = cached
		}
	}
	if serving == nil {
		serving, _ = q.source.GetServingSystem(ctx)
	}
	if serving != nil && serving.MCC != 0 {
		plmn, plmnErr := src.NASGetPLMNName(ctx, qmi.NASPLMNNameRequest{
			MCC:              serving.MCC,
			MNC:              serving.MNC,
			IncludesPCSDigit: serving.MNC >= 100,
		})
		if plmnErr == nil && plmn != nil {
			info.PLMNLongName = strings.TrimSpace(plmn.LongName)
			info.PLMNShortName = strings.TrimSpace(plmn.ShortName)
		}
	}
	return info, nil
}

func (q *QMIBackend) NASConfigSignalInfoV2(ctx context.Context, req NASSignalConfigRequest) error {
	src, err := q.nasSource()
	if err != nil {
		return err
	}
	return src.NASConfigSignalInfoV2(ctx, qmi.NASSignalInfoConfigV2{
		LTEEnabled:    req.LTEEnabled,
		LTERSRPDelta:  req.LTERSRPDelta,
		LTERSRQDelta:  req.LTERSRQDelta,
		LTESNRDelta:   req.LTESNRDelta,
		NR5GEnabled:   req.NR5GEnabled,
		NR5GRSRPDelta: req.NR5GRSRPDelta,
		NR5GRSRQDelta: req.NR5GRSRQDelta,
		NR5GSINRDelta: req.NR5GSINRDelta,
	})
}

func (q *QMIBackend) NASRegisterIndications(ctx context.Context) error {
	src, err := q.nasSource()
	if err != nil {
		return err
	}
	return src.NASRegisterIndications(ctx, qmi.NASIndicationRegistration{
		ServingSystemChanged:        true,
		SystemInfo:                  true,
		NetworkTime:                 true,
		SignalInfo:                  true,
		OperatorName:                true,
		NetworkReject:               true,
		IncrementalNetworkScan:      true,
		EventReportSignalThresholds: []int8{-60, -85},
	})
}

// GetSMSC 读取短信中心号码（SMSC）。
// 具体解析逻辑在 quectel-qmi-go 库内实现。
func (q *QMIBackend) GetSMSC(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return q.source.GetSMSC(ctx)
}

// ============================================================================
// SMSProvider 实现
// ============================================================================

func (q *QMIBackend) SendSMS(ctx context.Context, to, body string) error {
	return q.SendSMSWithOptions(ctx, to, body, smscodec.SubmitOptions{})
}

func (q *QMIBackend) SendSMSWithOptions(ctx context.Context, to, body string, opts smscodec.SubmitOptions) error {
	tpdus, _, err := smscodec.BuildSubmitTPDUsWithOptions(to, body, opts)
	if err != nil {
		return fmt.Errorf("PDU 编码失败: %w", err)
	}
	if len(tpdus) == 0 {
		return fmt.Errorf("PDU 编码结果为空")
	}

	// 逐段发送（支持长短信自动分段）
	for i, binaryTPDU := range tpdus {
		pduWithSMSC := append([]byte{0x00}, binaryTPDU...)

		// format=0x06 表示 GW PP (3GPP Point-to-Point)
		sendStart := time.Now()
		if err := q.source.WMSSendRawMessage(ctx, 0x06, pduWithSMSC); err != nil {
			logger.Warn("QMI 短信发送失败",
				"to", redactSMSRecipient(to),
				"part", i+1,
				"parts", len(tpdus),
				"tpdu_len", len(binaryTPDU),
				"pdu_len", len(pduWithSMSC),
				"elapsed_ms", time.Since(sendStart).Milliseconds(),
				"err", err,
			)
			return fmt.Errorf("发送第 %d/%d 段失败: %w", i+1, len(tpdus), err)
		}
	}

	logger.Info("QMI 短信发送成功", "to", redactSMSRecipient(to), "parts", len(tpdus), "encoding", opts.Encoding)
	return nil
}

func (q *QMIBackend) ReadSMS(ctx context.Context, index int) (*SMS, error) {
	// 优先从 NV 存储（storageType=1）读取
	pdu, err := q.source.WMSRawReadMessage(ctx, 1, uint32(index))
	if err != nil {
		// 回退到 SIM 存储（storageType=0）
		pdu, err = q.source.WMSRawReadMessage(ctx, 0, uint32(index))
		if err != nil {
			return nil, err
		}
	}
	return &SMS{
		Index:   index,
		Content: hex.EncodeToString(pdu),
	}, nil
}

func (q *QMIBackend) DeleteSMS(ctx context.Context, index int) error {
	return q.source.WMSDeleteMessage(ctx, 1, uint32(index))
}

func (q *QMIBackend) ListSMS(ctx context.Context) ([]SMSSummary, error) {
	msgs, err := q.source.WMSListMessagesAuto(ctx, 1)
	if err != nil {
		return nil, err
	}
	result := make([]SMSSummary, 0, len(msgs))
	for _, m := range msgs {
		result = append(result, SMSSummary{
			Index: int(m.Index),
			Tag:   int(m.Tag),
		})
	}
	return result, nil
}

func (q *QMIBackend) DeleteAllSMS(ctx context.Context) error {
	// 删除 UIM/NV 两类存储里的已读和未读消息，避免任一侧残留占满容量。
	var errs []error
	for _, storage := range []uint8{0, 1} {
		for _, tag := range []qmi.MessageTagType{qmi.TagTypeMTRead, qmi.TagTypeMTNotRead} {
			if err := q.source.WMSDeleteMessagesByTag(ctx, storage, tag, qmi.MessageModeGW); err != nil {
				errs = append(errs, fmt.Errorf("storage %d tag %d: %w", storage, tag, err))
			}
		}
	}
	return errors.Join(errs...)
}

// ============================================================================
// OperatingModeController 实现
// ============================================================================

func (q *QMIBackend) SetOperatingMode(ctx context.Context, mode OperatingMode) error {
	var qmiMode qmi.OperatingMode
	switch mode {
	case ModeOnline:
		qmiMode = qmi.ModeOnline
	case ModeLowPower:
		qmiMode = qmi.ModeLowPower
	case ModeRFOff:
		// 飞行模式：QMI 切换为 ModeLowPower (0x01)，这等价于 AT+CFUN=4。
		qmiMode = qmi.ModeLowPower
	default:
		return fmt.Errorf("不支持的操作模式: %d", mode)
	}
	return q.source.SetOperatingMode(ctx, qmiMode)
}

func (q *QMIBackend) GetOperatingMode(ctx context.Context) (OperatingMode, error) {
	qmiMode, err := q.source.GetOperatingMode(ctx)
	if err != nil {
		return ModeOnline, err
	}
	return qmiOperatingModeToBackend(qmiMode), nil
}

func qmiOperatingModeToBackend(qmiMode qmi.OperatingMode) OperatingMode {
	switch qmiMode {
	case qmi.ModeOnline:
		return ModeOnline
	case qmi.ModeLowPower, qmi.ModeOffline, qmi.ModeShutdown, qmi.ModeOnlyLowPower:
		return ModeLowPower
	case qmi.ModePersistLow:
		return ModeRFOff
	default:
		return OperatingMode(int(qmiMode))
	}
}

func (q *QMIBackend) Reboot(ctx context.Context) error {
	return q.source.SetOperatingMode(ctx, qmi.ModeReset)
}

// UIMPowerOffSIM powers off the specified SIM slot.
func (q *QMIBackend) UIMPowerOffSIM(ctx context.Context, slot uint8) error {
	return q.source.UIMPowerOffSIM(ctx, slot)
}

// UIMPowerOnSIM powers on the specified SIM slot.
func (q *QMIBackend) UIMPowerOnSIM(ctx context.Context, slot uint8) error {
	return q.source.UIMPowerOnSIM(ctx, slot)
}

func (q *QMIBackend) UIMPostSwitchReload(ctx context.Context, readiness manager.UIMReadiness, opts manager.UIMPostSwitchReloadOptions) (uint8, error) {
	return q.source.UIMPostSwitchReload(ctx, readiness, opts)
}

func (q *QMIBackend) EnsureSIMProvisioned(ctx context.Context, opts manager.EnsureSIMProvisionedOptions) (manager.UIMReadiness, error) {
	return q.source.EnsureSIMProvisioned(ctx, opts)
}

// ============================================================================
// SIMAuthProvider 实现
// ============================================================================

func (q *QMIBackend) OpenLogicalChannel(ctx context.Context, aid string) (int, error) {
	aidBytes, err := hex.DecodeString(aid)
	if err != nil {
		return 0, fmt.Errorf("AID hex 解码失败: %w", err)
	}
	if source, ok := q.source.(qmiSIMAuthLogicalChannelSource); ok {
		ch, err := source.OpenSIMAuthLogicalChannel(ctx, 1, aidBytes)
		if err != nil {
			return 0, err
		}
		return int(ch), nil
	}
	return 0, fmt.Errorf("QMI source 不支持 SIMAuth 逻辑通道")
}

func (q *QMIBackend) ResolveSIMAuthAID(ctx context.Context, app string, fallbackAID string) (string, string, error) {
	expectedPrefix := ""
	var getAID func(qmiSIMAuthAIDSource, context.Context) ([]byte, error)
	switch strings.ToLower(strings.TrimSpace(app)) {
	case "usim":
		expectedPrefix = "A0000000871002"
		getAID = qmiSIMAuthAIDSource.GetUSIMAID
	case "isim":
		expectedPrefix = "A0000000871004"
		getAID = qmiSIMAuthAIDSource.GetISIMAID
	default:
		return "", "sim_auth_aid_not_ready", fmt.Errorf("%w: unsupported app %q", ErrSIMAuthAIDNotReady, app)
	}
	source, ok := q.source.(qmiSIMAuthAIDSource)
	if !ok {
		return "", "sim_auth_aid_not_ready", fmt.Errorf("%w: qmi card status unavailable", ErrSIMAuthAIDNotReady)
	}
	aid, err := getAID(source, ctx)
	if err != nil {
		return "", "sim_auth_aid_not_ready", fmt.Errorf("%w: %v", ErrSIMAuthAIDNotReady, err)
	}
	aidHex := strings.ToUpper(hex.EncodeToString(aid))
	if !strings.HasPrefix(aidHex, expectedPrefix) {
		return "", "sim_auth_aid_not_ready", fmt.Errorf("%w: QMI %s AID 不匹配: %s", ErrSIMAuthAIDNotReady, strings.ToUpper(strings.TrimSpace(app)), aidHex)
	}
	if len(aidHex) <= len(expectedPrefix) {
		return "", "sim_auth_aid_not_ready", fmt.Errorf("%w: QMI %s AID 不是 full AID: %s", ErrSIMAuthAIDNotReady, strings.ToUpper(strings.TrimSpace(app)), aidHex)
	}
	return aidHex, "qmi_card_status", nil
}

func (q *QMIBackend) CloseLogicalChannel(ctx context.Context, channelID int) error {
	if source, ok := q.source.(qmiSIMAuthLogicalChannelSource); ok {
		return source.CloseSIMAuthLogicalChannel(ctx, 1, byte(channelID))
	}
	return fmt.Errorf("QMI source 不支持 SIMAuth 逻辑通道")
}

func (q *QMIBackend) TransmitAPDU(ctx context.Context, channelID int, command string) (string, error) {
	cmdBytes, err := hex.DecodeString(command)
	if err != nil {
		return "", fmt.Errorf("APDU hex 解码失败: %w", err)
	}

	resp, err := q.source.TransmitEUICCAPDU(ctx, 1, byte(channelID), cmdBytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(resp), nil
}

func isLikelyShortCode(phone string) bool {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return false
	}
	if strings.HasPrefix(phone, "+") {
		return false
	}
	digits := strings.TrimLeft(phone, "0123456789")
	return digits == "" && len(phone) <= 6
}

func normalizeSubmitDestinationForShortCode(pdu *tpdu.TPDU) {
	if pdu == nil {
		return
	}
	da := pdu.DA
	da.SetTypeOfNumber(tpdu.TonUnknown)
	da.SetNumberingPlan(tpdu.NpISDN)
	pdu.DA = da
}
