package modem

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/iniwex5/vohive/internal/apduarbiter"
	"github.com/iniwex5/vohive/internal/config"
	"github.com/iniwex5/vohive/pkg/logger"
	"github.com/iniwex5/vohive/pkg/smscodec"
	"github.com/warthog618/sms/encoding/gsm7"

	"go.bug.st/serial"
)

// SMSCallback 短信回调函数类型
type SMSCallback func(sender, content string, timestamp time.Time)

// rxMsg 串口接收到的消息包装
type rxMsg struct {
	Data string
	Err  error
}

// commandRequest AT 命令请求结构
type commandRequest struct {
	cmd          string
	respChan     chan string
	errChan      chan error
	timeout      time.Duration
	silent       bool
	highPriority bool

	// 交互式模式支持
	interactive bool   // 是否为交互式命令 (如发送短信)
	waitPrompt  bool   // 是否等待 "> " 提示符
	followUp    string // 后续指令 (当 waitPrompt=true 且收到提示符时发送)
}

// Manager 管理单个 EC20 模块的 AT 指令通信
type Manager struct {
	cfg      config.DeviceConfig
	atPort   string
	port     serial.Port
	portMode *serial.Mode

	// 通道驱动的异步架构
	stop        chan struct{}
	stopOnce    sync.Once
	loopWG      sync.WaitGroup
	cmdChan     chan commandRequest // 普通优先级
	cmdChanHigh chan commandRequest // 高优先级 (短信, IP 切换)
	rxChan      chan rxMsg
	triggerChan chan struct{} // 短信触发信号
	ready       chan struct{}
	readyOnce   sync.Once

	// 资源池
	reqPool sync.Pool

	// 状态
	running  bool
	busy     bool
	busyMu   sync.Mutex
	healthy  bool
	eofCount int // readLoop 中连续 EOF 计数，用于检测设备断开

	atTimeoutMu     sync.Mutex
	atTimeoutStreak int

	// 设备信息 (从 AT 指令获取)
	imei        string
	firmware    string
	iccid       string
	imsi        string
	operator    string
	simInserted bool
	signalDBM   int
	signalRSRQ  int
	signalRSRP  int

	// 网络信息
	regStatus     int    // 网络注册状态 (0-5)
	regStatusText string // 注册状态文本
	lac           string // 位置区代码
	cellID        string // 小区 ID
	apn           string // 接入点
	imsStatus     int    // IMS 注册状态
	networkMode   string // 网络模式 (LTE/WCDMA/GSM等)
	networkDuplex string // 网络双工方式 (FDD/TDD)
	usbnetMode    int    // USBNET 模式 (0: QMI, 1: ECM)

	infoMu sync.RWMutex

	// 回调
	smsCallback            SMSCallback
	newSMSHandler          func(index string) // 处理新短信索引的回调 (用于 bubble up URC)
	disableURCRead         bool               // 如果启用 QMI，禁用 AT 自动读取
	simStatusHandler       func(inserted *bool, state string)
	onDisconnectWithReason func(reason string)

	// CS 来电回调
	ringCallback    func()              // RING URC 回调
	clipCallback    func(number string) // +CLIP URC 回调 (来电号码)
	hangupCallback  func()              // NO CARRIER URC 回调 (对方挂断)
	connectCallback func()              // CONNECT/OK URC 回调 (对方接听外呼)
	qpcmvChan       chan int            // +QPCMV URC 流控通道 (0=忙, 1=就绪)

	reassembler *smscodec.Reassembler

	// SIM 卡低频巡检告警状态
	simFailCount int
	simAlerting  bool

	// USSD 会话通道：当有协程在等待 USSD 响应时，+CUSD URC 会被投递到此通道
	ussdChan chan USSDResult

	// RDY 事件订阅（模组重启后广播）
	rdyMu   sync.Mutex
	rdySubs []chan struct{}

	// APDU 仲裁（设备级全局）
	apduArbiter  *apduarbiter.Arbiter
	apduLeaseMu  sync.Mutex
	apduSessions map[int]apduSessionInfo
}

const atTimeoutWatchdogThreshold = 5

type apduSessionInfo struct {
	Channel  int
	Owner    string
	Class    apduarbiter.APDUClass
	OpenedAt time.Time
}

func (m *Manager) DeviceID() string {
	return m.cfg.ID
}

func pureQMIBackendConfig(cfg config.DeviceConfig) bool {
	mode := strings.ToLower(strings.TrimSpace(cfg.DeviceBackend))
	return mode == "qmi" || mode == "mbim" || (mode == "" && strings.TrimSpace(cfg.ControlDevice) != "")
}

func (m *Manager) pureQMIBackend() bool {
	return pureQMIBackendConfig(m.cfg)
}

func New(cfg config.DeviceConfig) (*Manager, error) {
	m := &Manager{
		cfg:          cfg,
		atPort:       cfg.ATPort,
		stop:         make(chan struct{}),
		cmdChan:      make(chan commandRequest, 10),
		cmdChanHigh:  make(chan commandRequest, 5),
		rxChan:       make(chan rxMsg, 100),
		triggerChan:  make(chan struct{}, 1),
		ready:        make(chan struct{}),
		healthy:      true,
		reassembler:  smscodec.NewReassembler(),
		ussdChan:     make(chan USSDResult, 1),
		apduSessions: make(map[int]apduSessionInfo),
		reqPool: sync.Pool{
			New: func() interface{} {
				return &commandRequest{
					respChan: make(chan string, 1),
					errChan:  make(chan error, 1),
				}
			},
		},
	}

	// 如果未指定 AT 端口，使用 ManagePort
	if m.atPort == "" {
		m.atPort = cfg.ManagePort
	}
	// QMI 后端模式下允许 AT 端口为空（模组不依赖 AT 串口）
	// AT 模式仍然要求 AT 端口非空
	if m.atPort == "" && !pureQMIBackendConfig(cfg) {
		return nil, errors.New("AT port not configured")
	}

	m.portMode = &serial.Mode{
		BaudRate: 115200,
	}

	return m, nil
}

func (m *Manager) markReady() {
	m.readyOnce.Do(func() {
		close(m.ready)
	})
}

func (m *Manager) WaitReady(timeout time.Duration) bool {
	select {
	case <-m.ready:
		return true
	case <-time.After(timeout):
		return false
	case <-m.stop:
		return false
	}
}

// forceReleasePort 检查端口是否被占用，如果是则杀掉占用者
func (m *Manager) forceReleasePort(portPath string) {
	// 设备文件不存在时 fuser 可能返回内核线程 PID，直接跳过避免误杀。
	if _, err := os.Stat(portPath); err != nil {
		return
	}

	// 先查询占用进程，再排除当前进程(及其线程)后定向释放，避免误杀自身。
	out, _ := exec.Command("fuser", portPath).CombinedOutput()
	if len(out) == 0 {
		return
	}

	occupiedPIDs := parseFuserPIDs(string(out))
	if len(occupiedPIDs) == 0 {
		return
	}

	selfTaskPIDs := currentProcessTaskPIDSet()
	released := make([]int, 0, len(occupiedPIDs))
	skipped := make([]int, 0, len(occupiedPIDs))

	for _, pid := range occupiedPIDs {
		// 跳过内核关键进程: PID 1 (init/systemd), PID 2 (kthreadd)
		if pid <= 2 {
			skipped = append(skipped, pid)
			continue
		}
		if _, isSelf := selfTaskPIDs[pid]; isSelf {
			skipped = append(skipped, pid)
			continue
		}
		if err := killProcess(pid); err == nil {
			released = append(released, pid)
		}
	}

	if len(skipped) > 0 {
		logger.Warn(fmt.Sprintf("[%s] 端口被当前进程占用，跳过自杀式释放", m.cfg.ID), "port", portPath, "self_pids", skipped)
	}
	if len(released) > 0 {
		logger.Warn(fmt.Sprintf("[%s] 检测到端口被外部进程占用，正在强制释放", m.cfg.ID), "port", portPath, "pids", released)
		// 等待进程完全退出
		time.Sleep(200 * time.Millisecond)
	}
}

func parseFuserPIDs(raw string) []int {
	// fuser 输出通常形如: "/dev/ttyUSB2: 1234 5678"
	// 只解析冒号后的 PID，避免把设备名中的数字(如 ttyUSB2)误当作 PID。
	if idx := strings.Index(raw, ":"); idx >= 0 && idx+1 < len(raw) {
		raw = raw[idx+1:]
	}
	seen := make(map[int]struct{})
	out := make([]int, 0)
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r < '0' || r > '9'
	})
	for _, f := range fields {
		pid, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || pid <= 0 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		out = append(out, pid)
	}
	return out
}

func currentProcessTaskPIDSet() map[int]struct{} {
	out := map[int]struct{}{
		os.Getpid(): {},
	}
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(strings.TrimSpace(e.Name()))
		if err != nil || pid <= 0 {
			continue
		}
		out[pid] = struct{}{}
	}
	return out
}

// SetSMSCallback 设置短信接收回调
func (m *Manager) SetSMSCallback(cb SMSCallback) {
	m.smsCallback = cb
}

// SetNewSMSHandler 设置新短信索引回调 (当收到 URC 时调用，用于接管读取流程)
func (m *Manager) SetNewSMSHandler(handler func(index string)) {
	m.infoMu.Lock()
	m.newSMSHandler = handler
	m.infoMu.Unlock()
}

// SetDisableURCRead 启用/禁用 URC 自动读取 (当 QMI 接管时应禁用)
func (m *Manager) SetDisableURCRead(disable bool) {
	m.infoMu.Lock()
	m.disableURCRead = disable
	m.infoMu.Unlock()
}

func (m *Manager) SetSIMStatusHandler(handler func(inserted *bool, state string)) {
	m.infoMu.Lock()
	m.simStatusHandler = handler
	m.infoMu.Unlock()
}

// SetRingCallback 设置来电 RING 回调
func (m *Manager) SetRingCallback(cb func()) {
	m.infoMu.Lock()
	m.ringCallback = cb
	m.infoMu.Unlock()
}

// SetClipCallback 设置 +CLIP 来电号码回调
func (m *Manager) SetClipCallback(fn func(number string)) {
	m.infoMu.Lock()
	defer m.infoMu.Unlock()
	m.clipCallback = fn
}

// SetHangupCallback 设置 NO CARRIER 对方挂断回调
func (m *Manager) SetHangupCallback(fn func()) {
	m.infoMu.Lock()
	defer m.infoMu.Unlock()
	m.hangupCallback = fn
}

// GetQPCMVChan 获取 +QPCMV URC 流控通道 (0=模块忙, 1=就绪)
func (m *Manager) GetQPCMVChan() <-chan int {
	if m.qpcmvChan == nil {
		m.qpcmvChan = make(chan int, 4)
	}
	return m.qpcmvChan
}

// AnswerCall 接听来电 (ATA)
func (m *Manager) AnswerCall() error {
	_, err := m.ExecuteAT("ATA", 5*time.Second)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] 接听来电失败", m.cfg.ID), "err", err)
		return err
	}
	logger.Info(fmt.Sprintf("[%s] 已接听来电", m.cfg.ID))
	return nil
}

// DialCall 发起语音外呼 (ATD<number>;)
func (m *Manager) DialCall(number string) error {
	cmd := fmt.Sprintf("ATD%s;", number)
	_, err := m.ExecuteAT(cmd, 60*time.Second)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] 拨号失败", m.cfg.ID), "err", err, "number", redactIdentifier(number))
		return err
	}
	logger.Info(fmt.Sprintf("[%s] 拨号指令已发出", m.cfg.ID), "number", redactIdentifier(number))
	return nil
}

// HangupCall 挂断通话 (ATH)
func (m *Manager) HangupCall() error {
	_, err := m.ExecuteAT("ATH", 3*time.Second)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] 挂断通话失败", m.cfg.ID), "err", err)
		return err
	}
	logger.Info(fmt.Sprintf("[%s] 已挂断通话", m.cfg.ID))
	return nil
}

// SetConnectCallback 设置 CONNECT/OK (对方接听外呼) 回调
func (m *Manager) SetConnectCallback(fn func()) {
	m.infoMu.Lock()
	defer m.infoMu.Unlock()
	m.connectCallback = fn
}

// SetOnDisconnectWithReason 设置带原因的串口/控制面掉线回调。
func (m *Manager) SetOnDisconnectWithReason(cb func(reason string)) {
	m.infoMu.Lock()
	m.onDisconnectWithReason = cb
	m.infoMu.Unlock()
}

func (m *Manager) notifyDisconnect(reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "modem_disconnect"
	}
	m.infoMu.RLock()
	withReason := m.onDisconnectWithReason
	m.infoMu.RUnlock()
	if withReason != nil {
		go withReason(reason)
	}
}

func (m *Manager) resetATTimeoutWatchdog() {
	m.atTimeoutMu.Lock()
	m.atTimeoutStreak = 0
	m.atTimeoutMu.Unlock()
}

func (m *Manager) recordATTimeout(req commandRequest) (int, bool) {
	if req.highPriority {
		return 0, false
	}
	m.atTimeoutMu.Lock()
	defer m.atTimeoutMu.Unlock()
	m.atTimeoutStreak++
	return m.atTimeoutStreak, m.atTimeoutStreak >= atTimeoutWatchdogThreshold
}

func (m *Manager) tripATTimeoutWatchdog(cmd string, failures int) {
	if !m.running {
		return
	}
	logger.Warn(fmt.Sprintf("[%s] AT 连续超时达到阈值，触发控制面恢复", m.cfg.ID),
		"cmd", cmd,
		"port", m.atPort,
		"failures", failures,
		"threshold", atTimeoutWatchdogThreshold)
	m.healthy = false
	m.Stop()
	m.notifyDisconnect("at_timeout_threshold")
}

// Start 启动 AT 管理器的后台协程
func (m *Manager) Start() error {
	if m.pureQMIBackend() {
		logger.Info(fmt.Sprintf("[%s] 纯 QMI 模式，跳过 AT 管理器启动", m.cfg.ID), "at_port", m.atPort)
		m.running = false
		m.markReady()
		return nil
	}
	if m.atPort == "" {
		return errors.New("AT port not configured")
	}

	// 检查并强制接管被占用的端口
	m.forceReleasePort(m.atPort)

	var err error
	for attempt := 0; attempt < 8; attempt++ {
		m.port, err = serial.Open(m.atPort, m.portMode)
		if err == nil {
			break
		}
		if !isRetryableSerialOpenErr(err) {
			break
		}
		time.Sleep(time.Duration(80*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("打开串口 %s 失败: %w", m.atPort, err)
	}

	m.port.SetReadTimeout(100 * time.Millisecond)
	m.running = true

	// 启动读取协程
	m.loopWG.Add(1)
	go func() {
		defer m.loopWG.Done()
		m.readLoop()
	}()

	// 启动主事件循环
	m.loopWG.Add(1)
	go func() {
		defer m.loopWG.Done()
		m.runLoop()
	}()

	// 启动分片清理协程
	m.loopWG.Add(1)
	go func() {
		defer m.loopWG.Done()
		ticker := time.NewTicker(2 * time.Minute)
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				m.cleanupOldFragments()
			}
		}
	}()

	logger.Info(fmt.Sprintf("[%s] AT 管理器已启动", m.cfg.ID), "port", m.atPort)
	return nil
}

func isRetryableSerialOpenErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "resource busy") ||
		strings.Contains(msg, "device or resource busy") ||
		strings.Contains(msg, "temporarily unavailable")
}

func isFatalSerialRuntimeErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "input/output error") ||
		strings.Contains(msg, "no such device") ||
		strings.Contains(msg, "bad file descriptor") ||
		strings.Contains(msg, "device disconnected")
}

func (m *Manager) handleFatalSerialRuntimeErr(err error, phase string, cmd string) {
	if !isFatalSerialRuntimeErr(err) {
		return
	}
	if !m.running {
		return
	}
	logger.Warn(fmt.Sprintf("[%s] AT 串口运行期失效，触发恢复", m.cfg.ID),
		"phase", phase, "cmd", cmd, "port", m.atPort, "err", err)
	m.healthy = false
	m.Stop()
	m.notifyDisconnect("serial_runtime_error")
}

// Stop 停止管理器
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		m.releaseAllAPDULeases("stop")
		close(m.stop)
		if m.port != nil {
			m.port.Close()
		}
		m.running = false
	})
}

func (m *Manager) StopAndWait(timeout time.Duration) bool {
	m.Stop()
	done := make(chan struct{})
	go func() {
		m.loopWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// runLoop 主事件循环，处理命令和 URC
func (m *Manager) runLoop() {
	defer func() {
		if r := recover(); r != nil {
			logger.Error(fmt.Sprintf("[%s] runLoop panic recovered", m.cfg.ID), "err", r)
		}
	}()

	// 异步初始化模组，避免阻塞主循环导致命令执行死锁
	go m.initModem()

	for {
		// 优先级调度逻辑
		select {
		case <-m.stop:
			logger.Info(fmt.Sprintf("[%s] AT 管理器已停止", m.cfg.ID))
			return
		case req := <-m.cmdChanHigh:
			// 优先处理高优先级命令
			m.handleCommand(req)
			continue
		default:
			// 如果没有高优先级命令，则检查普通命令
		}

		select {
		case <-m.stop:
			logger.Info(fmt.Sprintf("[%s] AT 管理器已停止", m.cfg.ID))
			return
		case req := <-m.cmdChanHigh: // 再次检查高优先级，防止饿死
			m.handleCommand(req)
		case req := <-m.cmdChan:
			m.handleCommand(req)
		case msg := <-m.rxChan:
			// 空闲状态下的数据处理（主要是 URC）
			if msg.Err != nil {
				logger.Error(fmt.Sprintf("[%s] 串口读取错误，模块可能已掉线", m.cfg.ID), "err", msg.Err)
				m.Stop()
				m.notifyDisconnect("serial_read_error")
				return
			}
			if m.isURC(msg.Data) {
				m.handleURC(msg.Data)
			}
		}
	}
}

// handleCommand 处理单个 AT 命令
func (m *Manager) handleCommand(req commandRequest) {
	startTime := time.Now()

	// 发送命令
	if _, err := m.port.Write([]byte(req.cmd + "\r\n")); err != nil {
		req.errChan <- err
		m.handleFatalSerialRuntimeErr(err, "write", req.cmd)
		return
	}

	// 等待响应
	fullResponse := []string{}
	timeoutTimer := time.NewTimer(req.timeout)
	defer timeoutTimer.Stop()

RespLoop:
	for {
		select {
		case <-timeoutTimer.C:
			// 超时时尝试发送 ESC (0x1B) 以取消可能的挂起操作（如短信输入）
			m.port.Write([]byte{0x1B})
			logger.Warn(fmt.Sprintf("[%s] 命令执行超时，已发送 ESC 尝试恢复", m.cfg.ID), "port", m.atPort, "cmd", req.cmd, "cost", time.Since(startTime).String())
			req.errChan <- errors.New("命令执行超时")
			if failures, tripped := m.recordATTimeout(req); tripped {
				m.tripATTimeoutWatchdog(req.cmd, failures)
			}
			return

		case msg := <-m.rxChan:
			if msg.Err != nil {
				req.errChan <- msg.Err
				m.handleFatalSerialRuntimeErr(msg.Err, "read", req.cmd)
				return
			}

			line := msg.Data

			if line == "OK" {
				m.resetATTimeoutWatchdog()
				if !req.silent {
					logger.Debug(fmt.Sprintf("[%s] AT 执行成功", m.cfg.ID),
						"cmd", req.cmd,
						"resp", strings.Join(fullResponse, " | "),
						"cost", time.Since(startTime).Truncate(time.Millisecond).String())
				}
				req.respChan <- strings.Join(fullResponse, "\n")
				break RespLoop
			} else if strings.Contains(line, "ERROR") {
				m.resetATTimeoutWatchdog()
				fullResponse = append(fullResponse, line)
				if !req.silent {
					logger.Warn(fmt.Sprintf("[%s] AT 执行失败", m.cfg.ID),
						"cmd", req.cmd,
						"resp", strings.Join(fullResponse, " | "),
						"cost", time.Since(startTime).Truncate(time.Millisecond).String())
				}
				req.errChan <- fmt.Errorf("设备返回错误: %s", strings.Join(fullResponse, "\n"))
				break RespLoop
			} else if strings.Contains(line, ">") {
				m.resetATTimeoutWatchdog()
				fullResponse = append(fullResponse, line)
				if !req.silent {
					logger.Debug(fmt.Sprintf("[%s] AT 收到提示", m.cfg.ID),
						"cmd", req.cmd,
						"resp", ">",
						"cost", time.Since(startTime).Truncate(time.Millisecond).String())
				}

				if req.interactive && req.waitPrompt && req.followUp != "" {
					// 收到提示符，立即发送后续指令
					m.port.Write([]byte(req.followUp))
					// 继续等待最终响应 (OK/ERROR)
					// 重置 waitPrompt 防止重复触发
					req.waitPrompt = false
					continue
				}

				req.respChan <- "> "
				break RespLoop
			} else if m.isURC(line) {
				// 对于确认为 URC 的行，始终分发出去
				m.handleURC(line)

				// 但仅当它是某些明确的、完全异步的事件（如短信通知、来电、USSD、插拔卡）时，才将其从当前命令的 fullResponse 中剔除，以免污染解析器
				// 其余如 +CIMI:, +QCCID:, +CSQ: 实际上既是 URC 也是命令回显，必须被当前命令捕获！
				isPureAsyncURC := func(s string) bool {
					key := urcKey(s)
					switch key {
					case "+CUSD", "+CMTI", "RING", "+CLIP", "+QSIMSTAT", "+QSTKURC", "+QPCMV":
						return true
					}
					return false
				}

				if !isPureAsyncURC(line) {
					fullResponse = append(fullResponse, line)
				}
			} else {
				fullResponse = append(fullResponse, line)
			}
		}
	}
}

// readLoop 专用读取协程
func (m *Manager) readLoop() {
	defer func() {
		if r := recover(); r != nil {
			logger.Error(fmt.Sprintf("[%s] readLoop panic recovered", m.cfg.ID), "err", r)
		}
	}()

	buf := make([]byte, 1024)
	var lineBuf bytes.Buffer

	for {
		select {
		case <-m.stop:
			return
		default:
		}

		n, err := m.port.Read(buf)
		if err != nil {
			errMsg := err.Error()
			// 忽略超时错误和多次读取无数据错误
			if strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "multiple Read calls return no data") {
				continue
			}

			// EOF 处理：连续 EOF 超过阈值则判定为设备已断开
			if err == io.EOF {
				m.eofCount++
				if m.eofCount >= 30 { // 连续 30 次 EOF（约 3 秒）
					select {
					case <-m.stop:
						return
					default:
						logger.Warn(fmt.Sprintf("[%s] 串口连续 %d 次 EOF，判定设备已断开", m.cfg.ID, m.eofCount))
						m.rxChan <- rxMsg{Err: fmt.Errorf("连续 %d 次 EOF，判定设备已断开", m.eofCount)}
						return
					}
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			m.eofCount = 0 // 非 EOF 错误时重置计数

			select {
			case <-m.stop:
				return
			default:
				m.rxChan <- rxMsg{Err: err}
				return
			}
		}

		if n > 0 {
			m.eofCount = 0 // 成功读取数据，重置 EOF 计数
			// 处理读取到的数据
			for i := 0; i < n; i++ {
				b := buf[i]
				lineBuf.WriteByte(b)

				// 遇到换行符，或者遇到 '>' 提示符
				// 判定为行结束 (注意处理 > )
				// 这里的逻辑有点 tricky，因为 "> " 通常是最后两个字符
				// 简化逻辑：遇到 \n 发送；遇到 > 且前一个是 \r 或 \n 或者是行首？
				// 为了稳健，只要遇到 \n 就发送。
			}

			// 重新实现更简单的逻辑：
			// 循环处理所有换行符
			data := lineBuf.String()
			for {
				idx := strings.IndexByte(data, '\n')
				if idx >= 0 {
					// 有换行符，将前面的内容发送出去
					line := strings.TrimSpace(data[:idx+1])
					if line != "" {
						select {
						case m.rxChan <- rxMsg{Data: line}:
						case <-m.stop:
							return
						}
					}
					// 更新数据，继续处理剩余部分
					data = data[idx+1:]
				} else {
					break
				}
			}

			// 重置 buffer 并写入剩余数据
			lineBuf.Reset()
			lineBuf.WriteString(data)

			// 检查剩余部分是否是特殊提示符
			// 注意：有些模组返回 "\r\n> "，上面的循环会处理掉 "\r\n"，剩下 "> "
			if strings.HasSuffix(data, "> ") || data == "> " || strings.HasSuffix(data, ">") {
				// 遇到特殊提示符
				select {
				case m.rxChan <- rxMsg{Data: "> "}:
				case <-m.stop:
					return
				}
				// 清空缓冲区，因为已经消费了提示符
				lineBuf.Reset()
			}
		}
	}
}

// initModem 初始化模组
func (m *Manager) initModem() {
	if m.pureQMIBackend() {
		m.markReady()
		return
	}

	time.Sleep(150 * time.Millisecond)

	// 1. 探测连通性
	_, err := m.ExecuteATSilent("AT", 2*time.Second)
	if err != nil {
		logger.Warn(fmt.Sprintf("[%s] AT 探测失败", m.cfg.ID), "err", err)
		m.markReady()
		return
	}

	// 2. 初始化命令序列
	initCmds := []string{
		"ATE0",              // 关闭回显
		"AT+CMGF=0",         // PDU 模式
		"AT+CNMI=2,1,0,0,0", // 新短信上报 +CMTI
		"AT+CLIP=1",         // 启用来电号码显示 (+CLIP URC)
		"AT+QPCMV=1,2",      // 开启 UAC 语音模式 (PCM → ALSA 桥接必须)
	}

	for _, cmd := range initCmds {
		// 这些初始化命令使用 ExecuteATSilent 降低日志噪音，避免用户误解全在走 AT
		m.ExecuteATSilent(cmd, 2*time.Second)
		time.Sleep(100 * time.Millisecond)
	}

	m.markReady()

	// 3. 采集设备信息
	m.collectDeviceInfo()
	logger.Info(fmt.Sprintf("[%s] 模组初始化完成", m.cfg.ID), "imei", redactIdentifier(m.imei), "iccid", redactIdentifier(m.iccid))
}

// RefreshDeviceInfo 重新采集设备信息（切卡后需要更新缓存）
func (m *Manager) RefreshDeviceInfo() {
	m.collectDeviceInfo()
}

// collectDeviceInfo 采集设备信息 (IMEI, ICCID, IMSI, 运营商, 信号等)
func (m *Manager) collectDeviceInfo() {
	// 1. 无锁阶段：执行所有 AT 命令
	var imei, firmware, iccid, imsi, operator, apn, networkMode, networkDuplex string
	var simInserted bool
	var regStatus, imsStatus int
	var regStatusText, lac, cellID string
	var signalDBM, signalRSRQ, signalRSRP int = -999, 0, 0
	var usbnetMode int = -1

	if v, err := m.QueryIMEI(); err == nil {
		imei = v
	}
	if v, err := m.QueryFirmware(); err == nil {
		firmware = v
	}
	if v, err := m.QuerySIMInserted(); err == nil {
		simInserted = v
	}
	if simInserted {
		if v, err := m.QueryIMSI(); err == nil {
			imsi = v
		}
	}
	if v, err := m.QueryICCID(); err == nil {
		iccid = v
	}
	if v, err := m.QueryOperator(); err == nil {
		operator = v
	}
	if st, text, lacV, cellV, err := m.QueryRegistration(); err == nil {
		regStatus = st
		regStatusText = text
		lac = lacV
		cellID = cellV
	}
	if _, dbm, err := m.QueryCSQ(); err == nil && dbm != -999 {
		signalDBM = dbm
	}
	if rsrp, rsrq, err := m.QueryServingCellLTE(); err == nil {
		signalRSRP = rsrp
		signalRSRQ = rsrq
	}
	if v, err := m.QueryAPN(); err == nil {
		apn = v
	}
	if v, err := m.QueryIMSStatus(); err == nil {
		imsStatus = v
	}
	if mode, duplex, err := m.QueryNetworkModeAndDuplex(); err == nil {
		networkMode = mode
		networkDuplex = duplex
	}
	if v, err := m.QueryUSBNetMode(); err == nil {
		usbnetMode = v
	}

	// 2. 有锁阶段：统一更新状态
	m.infoMu.Lock()
	defer m.infoMu.Unlock()

	if imei != "" {
		m.imei = imei
	}
	if firmware != "" {
		m.firmware = firmware
	}
	if iccid != "" {
		m.iccid = iccid
	}
	if imsi != "" {
		m.imsi = imsi
	}
	if operator != "" {
		m.operator = NormalizeServingOperatorName(operator, imsi)
	}
	m.simInserted = simInserted
	if signalDBM != -999 {
		m.signalDBM = signalDBM
	}
	if signalRSRP != 0 {
		m.signalRSRP = signalRSRP
	}
	if signalRSRQ != 0 {
		m.signalRSRQ = signalRSRQ
	}
	m.regStatus = regStatus
	m.regStatusText = regStatusText
	m.lac = lac
	m.cellID = cellID
	m.apn = apn
	m.imsStatus = imsStatus
	m.networkMode = networkMode
	m.networkDuplex = networkDuplex
	m.usbnetMode = usbnetMode
}

// getRegStatusText 返回网络注册状态文本
func (m *Manager) getRegStatusText(status int) string {
	switch status {
	case 0:
		return "未注册"
	case 1:
		return "已注册(本地)"
	case 2:
		return "搜索中"
	case 3:
		return "注册被拒"
	case 4:
		return "未知"
	case 5:
		return "已注册(漫游)"
	default:
		return "未知"
	}
}

// GetIMEI 返回设备 IMEI
func (m *Manager) GetIMEI() string {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.imei
}

// GetICCID 返回当前 SIM 卡 ICCID
func (m *Manager) GetICCID() string {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.iccid
}

// GetIMSI 返回当前 SIM 卡 IMSI
func (m *Manager) GetIMSI() string {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.imsi
}

// GetOperator 返回运营商名称
func (m *Manager) GetOperator() string {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.operator
}

// GetSignalDBM 返回信号强度 (dBm)
func (m *Manager) GetSignalDBM() int {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.signalDBM
}

// GetFirmware 返回固件版本
func (m *Manager) GetFirmware() string {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.firmware
}

// IsSimInserted 返回是否插入 SIM 卡
func (m *Manager) IsSimInserted() bool {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.simInserted
}

// GetRegStatus 返回网络注册状态
func (m *Manager) GetRegStatus() (int, string) {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.regStatus, m.regStatusText
}

// GetCellInfo 返回小区信息 (LAC, CellID)
func (m *Manager) GetCellInfo() (string, string) {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.lac, m.cellID
}

// GetSignalLTE 返回 LTE 详细信号 (RSRP, RSRQ)
func (m *Manager) GetSignalLTE() (int, int) {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.signalRSRP, m.signalRSRQ
}

// GetAPN 返回当前 APN
func (m *Manager) GetAPN() string {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.apn
}

// GetIMSStatus 返回 IMS 注册状态
func (m *Manager) GetIMSStatus() int {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return m.imsStatus
}

type PNNRecord struct {
	Record    int    `json:"record"`
	FullName  string `json:"full_name,omitempty"`
	ShortName string `json:"short_name,omitempty"`
	RawHex    string `json:"raw_hex,omitempty"`
}

type OPLRecord struct {
	Record    int    `json:"record"`
	PLMN      string `json:"plmn,omitempty"`
	LACStart  uint16 `json:"lac_start,omitempty"`
	LACEnd    uint16 `json:"lac_end,omitempty"`
	PNNRecord int    `json:"pnn_record,omitempty"`
	RawHex    string `json:"raw_hex,omitempty"`
}

type SIMServiceTable struct {
	Kind            string `json:"kind,omitempty"`
	RawHex          string `json:"raw_hex,omitempty"`
	EnabledServices []int  `json:"enabled_services,omitempty"`
}

// GetFullStatus 返回完整状态信息
type DeviceStatus struct {
	IMEI            string           `json:"imei"`
	Firmware        string           `json:"firmware"`
	ICCID           string           `json:"iccid"`
	IMSI            string           `json:"imsi"`
	NativeSPN       string           `json:"native_spn,omitempty"`
	NativeMCC       string           `json:"native_mcc,omitempty"`
	NativeMNC       string           `json:"native_mnc,omitempty"`
	GID1            string           `json:"gid1,omitempty"`
	GID2            string           `json:"gid2,omitempty"`
	PNN             []PNNRecord      `json:"pnn,omitempty"`
	OPL             []OPLRecord      `json:"opl,omitempty"`
	SIMServiceTable *SIMServiceTable `json:"sim_service_table,omitempty"`
	Operator        string           `json:"operator"`
	SimInserted     bool             `json:"sim_inserted"`
	SignalDBM       int              `json:"signal_dbm"`
	SignalRSRP      int              `json:"signal_rsrp"`
	SignalRSRQ      int              `json:"signal_rsrq"`
	SignalSINR      int              `json:"signal_sinr,omitempty"`
	NR5GSignalSINR  int              `json:"nr5g_signal_sinr,omitempty"`
	RadioBand       string           `json:"radio_band,omitempty"`
	RadioChannel    uint32           `json:"radio_channel,omitempty"`
	RegStatus       int              `json:"reg_status"`
	RegStatusText   string           `json:"reg_status_text"`
	PSAttached      bool             `json:"ps_attached"`
	LAC             string           `json:"lac"`
	CellID          string           `json:"cell_id"`
	APN             string           `json:"apn"`
	IMSStatus       int              `json:"ims_status"`
	NetworkMode     string           `json:"network_mode"`
	NetworkDuplex   string           `json:"network_duplex"`
	USBNetMode      int              `json:"usbnet_mode"`
	OperatingMode   *int             `json:"operating_mode,omitempty"`
}

func (m *Manager) GetFullStatus() DeviceStatus {
	m.infoMu.RLock()
	defer m.infoMu.RUnlock()
	return DeviceStatus{
		IMEI:          m.imei,
		Firmware:      m.firmware,
		ICCID:         m.iccid,
		IMSI:          m.imsi,
		Operator:      m.operator,
		SimInserted:   m.simInserted,
		SignalDBM:     m.signalDBM,
		SignalRSRP:    m.signalRSRP,
		SignalRSRQ:    m.signalRSRQ,
		RegStatus:     m.regStatus,
		RegStatusText: m.regStatusText,
		LAC:           m.lac,
		CellID:        m.cellID,
		APN:           m.apn,
		IMSStatus:     m.imsStatus,
		NetworkMode:   m.networkMode,
		NetworkDuplex: m.networkDuplex,
		USBNetMode:    m.usbnetMode,
		OperatingMode: nil,
	}
}

// RefreshStatus 刷新设备状态 (信号、运营商、SIM)，并在发现 SIM 卡掉线时触发告警
func (m *Manager) RefreshStatus(onAlert func(msg string), onRecover func(msg string)) {
	// 1. 在锁外执行耗时的 AT 命令
	var operator, networkMode, networkDuplex string
	var signalDBM, signalRSRP, signalRSRQ int = -999, 0, 0

	// 检查 SIM 卡是否存活
	simInserted, simErr := m.QuerySIMInserted()

	if v, err := m.QueryOperator(); err == nil {
		operator = v
	}
	if _, dbm, err := m.QueryCSQ(); err == nil && dbm != -999 {
		signalDBM = dbm
	}
	if mode, duplex, err := m.QueryNetworkModeFallbackAndDuplex(); err == nil {
		networkMode = mode
		networkDuplex = duplex
	}

	// 2. 获取锁并更新状态
	m.infoMu.Lock()
	defer m.infoMu.Unlock()

	if operator != "" {
		m.operator = NormalizeServingOperatorName(operator, m.imsi)
	}
	if signalDBM != -999 {
		m.signalDBM = signalDBM
	}
	if networkMode != "" {
		m.networkMode = networkMode
		m.networkDuplex = networkDuplex
	}
	if signalRSRP != 0 {
		m.signalRSRP = signalRSRP
	}
	if signalRSRQ != 0 {
		m.signalRSRQ = signalRSRQ
	}

	// 处理 SIM 卡告警逻辑 (连续 3 次探测失败)
	if simErr != nil || !simInserted {
		m.simFailCount++
		if m.simFailCount >= 3 && !m.simAlerting {
			m.simAlerting = true
			errDetail := "SIM 卡未插入或状态异常"
			if simErr != nil {
				errDetail = fmt.Sprintf("AT+CPIN 查询失败: %v", simErr)
			}
			logger.Warn(fmt.Sprintf("[%s] 定时巡检发现 SIM 卡掉线", m.cfg.ID), "err", errDetail)
			if onAlert != nil {
				// 异步发送告警避免阻塞锁内时间
				go onAlert(fmt.Sprintf("⚠️ 设备 %s SIM 卡掉线: %s", m.cfg.ID, errDetail))
			}
		}
	} else {
		if m.simAlerting {
			logger.Info(fmt.Sprintf("[%s] 定时巡检发现 SIM 卡已恢复", m.cfg.ID))
			if onRecover != nil {
				go onRecover(fmt.Sprintf("✅ 设备 %s SIM 卡已恢复正常", m.cfg.ID))
			}
		}
		m.simFailCount = 0
		m.simAlerting = false
	}
}

// isURC 判断是否为 URC
func (m *Manager) isURC(line string) bool {
	s := strings.TrimSpace(line)
	if s == "" {
		return false
	}
	// 排除确认为同步命令的异步回显，避免被 URC 处理函数拦截并报“未分类”
	if strings.HasPrefix(s, "+CSIM:") || strings.HasPrefix(s, "+CGLA:") || strings.HasPrefix(s, "+CCHO:") || strings.HasPrefix(s, "+CMGR:") || strings.HasPrefix(s, "+CMGS:") || strings.HasPrefix(s, "+QENG:") {
		return false
	}
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "^") || strings.HasPrefix(s, "$") {
		return true
	}
	switch s {
	case "RING", "RDY", "SMS Ready", "Call Ready", "NORMAL POWER DOWN", "NO CARRIER", "BUSY", "NO ANSWER":
		return true
	default:
		return false
	}
}

// SubscribeRDY 订阅一次性 RDY 事件。
// 调用方应在发出会触发模组重启的操作 *之前* 先调用本方法，然后等待返回的 channel。
// 模组重启并发出 RDY URC 后，channel 会被关闭（可通过 `<-ch` 或 `select` 接收）。
func (m *Manager) SubscribeRDY() <-chan struct{} {
	ch := make(chan struct{})
	m.rdyMu.Lock()
	m.rdySubs = append(m.rdySubs, ch)
	m.rdyMu.Unlock()
	return ch
}

// dispatchRDY 内部调用：广播 RDY 事件并清空订阅列表
func (m *Manager) dispatchRDY() {
	m.rdyMu.Lock()
	subs := m.rdySubs
	m.rdySubs = nil
	m.rdyMu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
}

func (m *Manager) dispatchSIMStatusURC(inserted *bool, state string) {
	m.infoMu.RLock()
	handler := m.simStatusHandler
	m.infoMu.RUnlock()
	if handler != nil {
		go handler(inserted, state)
	}
}

// handleURC 处理 URC
func (m *Manager) handleURC(line string) {
	s := strings.TrimSpace(line)
	if s == "" {
		return
	}

	fr := m.formatURC(s)
	msg := fmt.Sprintf("[%s] %s", m.cfg.ID, fr.Msg)
	switch fr.Level {
	case urcLogWarn:
		logger.Warn(msg, fr.Fields...)
	case urcLogInfo:
		logger.Info(msg, fr.Fields...)
	default:
		logger.Debug(msg, fr.Fields...)
	}

	// 模组重启信号：广播给所有 SubscribeRDY() 的等待方。
	// 部分 EC20 固件重启后不发 RDY，而是直接发 +CPIN: READY，两者都作为就绪信号处理。
	if fr.Key == "RDY" {
		m.dispatchRDY()
	}
	if fr.Key == "+CPIN" {
		state := ""
		for i := 0; i+1 < len(fr.Fields); i += 2 {
			if k, _ := fr.Fields[i].(string); k == "state" {
				state, _ = fr.Fields[i+1].(string)
				if state == "READY" {
					m.dispatchRDY()
				}
				break
			}
		}
		m.dispatchSIMStatusURC(nil, state)
	}

	if fr.Key == "+QSIMSTAT" {
		var inserted *bool
		for i := 0; i+1 < len(fr.Fields); i += 2 {
			if k, _ := fr.Fields[i].(string); k == "inserted" {
				if v, ok := fr.Fields[i+1].(int); ok && v >= 0 {
					b := v == 1
					inserted = &b
				}
				break
			}
		}
		m.dispatchSIMStatusURC(inserted, "")
	}

	// 分发 +CUSD USSD 响应到等待通道
	if fr.Key == "+CUSD" {
		var n, dcs int
		var text string
		for i := 0; i < len(fr.Fields)-1; i += 2 {
			key, _ := fr.Fields[i].(string)
			switch key {
			case "n":
				n, _ = fr.Fields[i+1].(int)
			case "dcs":
				dcs, _ = fr.Fields[i+1].(int)
			case "text":
				text, _ = fr.Fields[i+1].(string)
			}
		}
		result := USSDResult{Status: n, RawText: text, DCS: dcs}
		result.Text = m.decodeUSSDText(text, dcs)
		select {
		case m.ussdChan <- result:
		default:
			// 没有人在等待，丢弃
			logger.Debug(fmt.Sprintf("[%s] USSD 响应无人等待，已丢弃", m.cfg.ID), "text", result.Text)
		}
	}

	if fr.Key == "+CMTI" && fr.CMTIIndex != "" {
		index := fr.CMTIIndex
		storage := fr.CMTIStorage
		m.infoMu.RLock()
		disabled := m.disableURCRead
		handler := m.newSMSHandler
		m.infoMu.RUnlock()

		if handler != nil {
			go handler(index)
			return
		}
		if !disabled {
			go m.readAndProcessSMSFromStorage(storage, index)
		} else {
			logger.Debug(fmt.Sprintf("[%s] 收到 URC 但已禁用自动读取 (QMI 接管)", m.cfg.ID), "index", index, "storage", storage)
		}
	}

	// 分发 RING 来电事件
	if fr.Key == "RING" {
		m.infoMu.RLock()
		cb := m.ringCallback
		m.infoMu.RUnlock()
		if cb != nil {
			go cb()
		}
	}

	// 分发对方挂断事件 NO CARRIER
	if fr.Key == "NO CARRIER" {
		m.infoMu.RLock()
		cb := m.hangupCallback
		m.infoMu.RUnlock()
		if cb != nil {
			go cb()
		}
	}

	// 分发对方接听外呼事件 (CONNECT)
	if fr.Key == "CONNECT" || fr.Key == "MO CONNECTED" {
		m.infoMu.RLock()
		cb := m.connectCallback
		m.infoMu.RUnlock()
		if cb != nil {
			go cb()
		}
	}

	// 分发 +CLIP 来电号码
	if fr.Key == "+CLIP" {
		for i := 0; i+1 < len(fr.Fields); i += 2 {
			if k, _ := fr.Fields[i].(string); k == "number" {
				if number, _ := fr.Fields[i+1].(string); number != "" {
					m.infoMu.RLock()
					cb := m.clipCallback
					m.infoMu.RUnlock()
					if cb != nil {
						go cb(number)
					}
				}
				break
			}
		}
	}

	// 分发 +QPCMV 流控事件 (0=模块忙, 1=就绪)
	if fr.Key == "+QPCMV" {
		rest := parseURCAfterColon(s)
		if v, ok := parseInt(strings.TrimSpace(rest)); ok {
			if m.qpcmvChan != nil {
				select {
				case m.qpcmvChan <- v:
				default:
				}
			}
		}
	}
}

// readAndProcessSMS 读取并处理短信
func (m *Manager) readAndProcessSMS(index string) {
	// 公开给外部调用的封装 (如果需要)
	m.ReadAndProcessSMS(index)
}

// ReadAndProcessSMS 公开方法：读取并处理短信
func (m *Manager) ReadAndProcessSMS(index string) {
	m.readAndProcessSMSFromStorage("", index)
}

// ReadAndProcessSMSFromStorage 读取指定 AT 短信存储中的短信。
func (m *Manager) ReadAndProcessSMSFromStorage(storage, index string) {
	m.readAndProcessSMSFromStorage(storage, index)
}

func (m *Manager) readAndProcessSMSFromStorage(storage, index string) {
	index, ok := normalizeSMSIndex(index)
	if !ok {
		logger.Warn(fmt.Sprintf("[%s] 短信索引非法，跳过读取", m.cfg.ID), "index", index)
		return
	}

	normalizedStorage, hasStorage := normalizeSMSStorage(storage)
	if hasStorage {
		restore, switched := m.switchSMSStorageForRead(normalizedStorage)
		if !switched {
			logger.Warn(fmt.Sprintf("[%s] 切换短信存储失败，跳过读取以避免误删其他存储短信", m.cfg.ID), "index", index, "storage", normalizedStorage)
			return
		}
		if restore != nil {
			defer restore()
		}
	}

	fields := []any{"index", index}
	if hasStorage {
		fields = append(fields, "storage", normalizedStorage)
	}
	logger.Info(fmt.Sprintf("[%s] 读取短信 (AT)", m.cfg.ID), fields...)

	// 读取短信 PDU
	resp, err := m.ExecuteAT("AT+CMGR="+index, 5*time.Second)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] 读取短信失败", m.cfg.ID), "index", index, "err", err)
		return
	}

	// 解析 PDU
	pduHex, _ := extractSMSPDUAfterPrefix(resp, "+CMGR:")

	if pduHex == "" || pduHex == "OK" {
		logger.Warn(fmt.Sprintf("[%s] 未找到 PDU 数据", m.cfg.ID))
		return
	}

	// 解码 PDU
	sender, content, timestamp := m.decodePDU(pduHex)

	// 如果内容为空（说明是分片且未完成），则不进行回调
	if content == "" {
		// 删除已读分片 (非常重要，否则SIM卡满了)
		m.ExecuteAT("AT+CMGD="+index, 3*time.Second)
		return
	}

	logger.Debug(fmt.Sprintf("[%s] 收到短信", m.cfg.ID), "sender", redactIdentifier(sender), "content_length", len([]rune(content)))

	// 回调通知
	if m.smsCallback != nil {
		m.smsCallback(sender, content, timestamp)
	}

	// 删除已读短信
	m.ExecuteAT("AT+CMGD="+index, 3*time.Second)
}

func normalizeSMSIndex(index string) (string, bool) {
	index = strings.TrimSpace(index)
	if index == "" {
		return "", false
	}
	for _, ch := range index {
		if ch < '0' || ch > '9' {
			return "", false
		}
	}
	return index, true
}

func normalizeSMSStorage(storage string) (string, bool) {
	storage = strings.ToUpper(strings.Trim(strings.TrimSpace(storage), `"`))
	if storage == "" || len(storage) > 8 {
		return "", false
	}
	for _, ch := range storage {
		if (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			continue
		}
		return "", false
	}
	return storage, true
}

func parseCPMSStorages(resp string) []string {
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+CPMS:") {
			continue
		}
		fields := parseCommaFields(parseURCAfterColon(line))
		storages := make([]string, 0, 3)
		for i := 0; i < len(fields); i += 3 {
			if storage, ok := normalizeSMSStorage(fields[i]); ok {
				storages = append(storages, storage)
			}
		}
		return storages
	}
	return nil
}

func cpmsSetCommand(storages ...string) string {
	normalized := make([]string, 0, len(storages))
	for _, storage := range storages {
		if s, ok := normalizeSMSStorage(storage); ok {
			normalized = append(normalized, s)
		}
	}
	if len(normalized) == 0 {
		return ""
	}
	if len(normalized) == 1 {
		normalized = []string{normalized[0], normalized[0], normalized[0]}
	}
	if len(normalized) > 3 {
		normalized = normalized[:3]
	}

	quoted := make([]string, 0, len(normalized))
	for _, storage := range normalized {
		quoted = append(quoted, fmt.Sprintf("%q", storage))
	}
	return "AT+CPMS=" + strings.Join(quoted, ",")
}

func (m *Manager) switchSMSStorageForRead(storage string) (func(), bool) {
	targetCmd := cpmsSetCommand(storage)
	if targetCmd == "" {
		return nil, true
	}

	var previous []string
	if resp, err := m.ExecuteAT("AT+CPMS?", 3*time.Second); err == nil {
		previous = parseCPMSStorages(resp)
		if len(previous) > 0 && strings.EqualFold(previous[0], storage) {
			return nil, true
		}
	} else {
		logger.Warn(fmt.Sprintf("[%s] 查询短信存储失败，将直接尝试切换", m.cfg.ID), "storage", storage, "err", err)
	}

	if _, err := m.ExecuteAT(targetCmd, 5*time.Second); err != nil {
		logger.Warn(fmt.Sprintf("[%s] 切换短信存储失败", m.cfg.ID), "storage", storage, "err", err)
		return nil, false
	}

	restoreCmd := cpmsSetCommand(previous...)
	if restoreCmd == "" || restoreCmd == targetCmd {
		return nil, true
	}
	return func() {
		if _, err := m.ExecuteAT(restoreCmd, 5*time.Second); err != nil {
			logger.Warn(fmt.Sprintf("[%s] 恢复短信存储失败", m.cfg.ID), "storage", previous, "err", err)
		}
	}, true
}

// Reboot 重启模组 (AT+CFUN=1,1)
func (m *Manager) Reboot() error {
	logger.Warn(fmt.Sprintf("[%s] 正在重启模组...", m.cfg.ID))
	_, err := m.ExecuteAT("AT+CFUN=1,1", 5*time.Second)
	return err
}

// cleanupOldFragments 清理过期的短信分片
func (m *Manager) cleanupOldFragments() {
	m.reassembler.Cleanup(10 * time.Minute)
}

// decodePDU 解码 PDU
func (m *Manager) decodePDU(raw string) (sender, content string, timestamp time.Time) {
	timestamp = time.Now()

	b, err := hex.DecodeString(raw)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] PDU 十六进制解码失败", m.cfg.ID), "err", err)
		content = fmt.Sprintf("[解码失败] %s", raw)
		return
	}

	// 跳过 SMSC 头部
	if len(b) > 0 {
		smscLen := int(b[0])
		if len(b) > smscLen+1 {
			b = b[smscLen+1:]
		}
	}

	var concat smscodec.ConcatInfo
	sender, content, msgTime, concat, err := smscodec.DecodeDeliverTPDU(b)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] TPDU 解析失败", m.cfg.ID), "err", err)
		content = fmt.Sprintf("[PDU 解析失败] %s", raw)
		return
	}
	if !msgTime.IsZero() {
		timestamp = msgTime
	}

	if concat.IsConcat {
		logger.Debug(fmt.Sprintf("[%s] 收到短信分片", m.cfg.ID), "ref", concat.Ref, "seq", concat.Seq, "total", concat.Total)
		complete, full := m.reassembler.Add(sender, concat, content)
		if !complete {
			return "", "", time.Time{}
		}
		content = full
		logger.Info(fmt.Sprintf("[%s] 长短信重组完成", m.cfg.ID), "total", concat.Total)
		return sender, content, timestamp
	}

	return sender, content, timestamp
}

// ExecuteAT 执行 AT 命令 (普通优先级)
func (m *Manager) ExecuteAT(cmd string, timeout time.Duration) (string, error) {
	return m.executeAT(cmd, timeout, false, false)
}

// ExecuteATSilent 静默执行 AT 命令 (普通优先级)
func (m *Manager) ExecuteATSilent(cmd string, timeout time.Duration) (string, error) {
	return m.executeAT(cmd, timeout, true, false)
}

// ExecuteATHigh 执行 AT 命令 (高优先级)
func (m *Manager) ExecuteATHigh(cmd string, timeout time.Duration) (string, error) {
	return m.executeAT(cmd, timeout, false, true)
}

// executeAT 内部通用的 AT 命令执行逻辑
func (m *Manager) executeAT(cmd string, timeout time.Duration, silent, highPriority bool) (string, error) {
	if !m.HasATPort() {
		return "", errors.New("当前设备没有可用 AT 端口")
	}
	if !m.CanExecuteAT() {
		return "", errors.New("AT 管理器未启动或不可用")
	}
	if !m.healthy {
		return "", errors.New("设备异常")
	}

	// 从池中获取请求对象
	req := m.reqPool.Get().(*commandRequest)
	// 重置字段
	req.cmd = cmd
	req.timeout = timeout
	req.silent = silent
	req.highPriority = highPriority
	req.interactive = false
	req.waitPrompt = false
	req.followUp = ""

	// 确保回收资源
	defer func() {
		// 清空通道以防万一
		select {
		case <-req.respChan:
		default:
		}
		select {
		case <-req.errChan:
		default:
		}
		m.reqPool.Put(req)
	}()

	// 根据优先级选择通道
	targetChan := m.cmdChan
	if highPriority {
		targetChan = m.cmdChanHigh
	}

	select {
	case targetChan <- *req: // 注意：这里发送的是值拷贝，但这不影响 respChan/errChan 的引用
		select {
		case resp := <-req.respChan:
			return resp, nil
		case err := <-req.errChan:
			return "", err
		case <-m.stop:
			select {
			case err := <-req.errChan:
				return "", err
			case resp := <-req.respChan:
				return resp, nil
			default:
			}
			return "", errors.New("manager stopped")
		}
	case <-time.After(5 * time.Second): // 通道写入超时 (队列满)
		return "", errors.New("command queue full")
	case <-m.stop:
		return "", errors.New("manager stopped")
	}
}

// SetBusy 设置忙碌状态
func (m *Manager) SetBusy(busy bool) {
	m.busyMu.Lock()
	m.busy = busy
	m.busyMu.Unlock()
}

// IsBusy 查询忙碌状态
func (m *Manager) IsBusy() bool {
	m.busyMu.Lock()
	defer m.busyMu.Unlock()
	return m.busy
}

// IsHealthy 返回健康状态
func (m *Manager) IsHealthy() bool {
	return m.healthy && m.running
}

// HasATPort 返回当前管理器是否配置了可用的 AT 端口。
func (m *Manager) HasATPort() bool {
	return strings.TrimSpace(m.atPort) != ""
}

// ATPort 返回配置中的 AT 端口路径。纯 QMI 模式会保留该值供人工 AT 终端使用。
func (m *Manager) ATPort() string {
	return strings.TrimSpace(m.atPort)
}

// CanExecuteAT 返回当前管理器是否已启动，可接受 AT 命令。
func (m *Manager) CanExecuteAT() bool {
	return !m.pureQMIBackend() && m.HasATPort() && m.running
}

func (m *Manager) SetAPDUArbiter(arbiter *apduarbiter.Arbiter) {
	m.apduLeaseMu.Lock()
	defer m.apduLeaseMu.Unlock()
	if m.apduSessions == nil {
		m.apduSessions = make(map[int]apduSessionInfo)
	}
	if m.apduArbiter == arbiter {
		return
	}
	clear(m.apduSessions)
	m.apduArbiter = arbiter
}

func (m *Manager) acquireAPDUTransportLease(timeout time.Duration, owner string, class apduarbiter.APDUClass, channel int) (*apduarbiter.Lease, error) {
	m.apduLeaseMu.Lock()
	arbiter := m.apduArbiter
	m.apduLeaseMu.Unlock()
	if arbiter == nil {
		return nil, nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return arbiter.AcquireTransport(ctx, apduarbiter.Request{
		Owner:   owner,
		Mode:    "AT",
		Class:   class,
		Channel: channel,
	})
}

func (m *Manager) bindAPDUSession(channel int, owner string, class ...apduarbiter.APDUClass) {
	m.apduLeaseMu.Lock()
	defer m.apduLeaseMu.Unlock()
	if m.apduSessions == nil {
		m.apduSessions = make(map[int]apduSessionInfo)
	}
	sessionClass := apduarbiter.APDUClassEUICCWrite
	if len(class) > 0 && class[0] != "" {
		sessionClass = class[0]
	}
	m.apduSessions[channel] = apduSessionInfo{
		Channel:  channel,
		Owner:    strings.TrimSpace(owner),
		Class:    sessionClass,
		OpenedAt: time.Now(),
	}
}

func (m *Manager) getAPDUSession(channel int) (apduSessionInfo, bool) {
	m.apduLeaseMu.Lock()
	defer m.apduLeaseMu.Unlock()
	session, ok := m.apduSessions[channel]
	return session, ok
}

func (m *Manager) hasAPDUSession(channel int) bool {
	m.apduLeaseMu.Lock()
	defer m.apduLeaseMu.Unlock()
	_, ok := m.apduSessions[channel]
	return ok
}

func (m *Manager) takeAPDUSession(channel int) (apduSessionInfo, bool) {
	m.apduLeaseMu.Lock()
	defer m.apduLeaseMu.Unlock()
	session, ok := m.apduSessions[channel]
	delete(m.apduSessions, channel)
	return session, ok
}

func (m *Manager) releaseAllAPDULeases(reason string) {
	m.apduLeaseMu.Lock()
	count := len(m.apduSessions)
	clear(m.apduSessions)
	m.apduLeaseMu.Unlock()

	if count > 0 {
		logger.Warn(fmt.Sprintf("[%s] APDU logical session registry 已清理", m.cfg.ID), "reason", reason, "session_count", count)
	}
}

// Rotate 执行 IP 切换
func (m *Manager) Rotate() error {
	m.SetBusy(true)
	defer m.SetBusy(false)

	logger.Info(fmt.Sprintf("[%s] 开始 IP 切换", m.cfg.ID))

	if err := m.SetAttach(false); err != nil {
		return fmt.Errorf("PS 域脱附失败: %w", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := m.SetAttach(true); err != nil {
		return fmt.Errorf("PS 域附着失败: %w", err)
	}

	logger.Info(fmt.Sprintf("[%s] IP 切换完成", m.cfg.ID))
	return nil
}

// CheckSignal 检查信号强度
func (m *Manager) CheckSignal() (int, error) {
	resp, err := m.ExecuteAT("AT+CSQ", 3*time.Second)
	if err != nil {
		return 0, err
	}

	rssi, _, ok := parseCSQ(resp)
	if ok {
		return rssi, nil
	}

	return 0, errors.New("无法解析信号强度")
}

// Close 关闭管理器
func (m *Manager) Close() error {
	m.Stop()
	return nil
}

// CheckAllSMS 检查所有短信（轮询模式）
func (m *Manager) CheckAllSMS() {
	if m.IsBusy() {
		return
	}

	pdus, err := m.SMSListAllPDU()
	if err != nil {
		logger.Warn(fmt.Sprintf("[%s] 检查短信失败", m.cfg.ID), "err", err)
		return
	}

	if len(pdus) == 0 {
		return
	}

	for _, pduHex := range pdus {
		sender, content, timestamp := m.decodePDU(pduHex)
		if m.smsCallback != nil && content != "" {
			m.smsCallback(sender, content, timestamp)
		}
	}

	// 删除所有短信
	_ = m.SMSDeleteAll()
}

// DeleteSMS 删除指定索引的短信
func (m *Manager) DeleteSMS(index uint32) error {
	_, err := m.ExecuteAT(fmt.Sprintf("AT+CMGD=%d", index), 3*time.Second)
	return err
}

// SendSMS 使用 PDU 模式发送短信
func (m *Manager) SendSMS(phone, message string) error {
	return m.SendSMSWithOptions(phone, message, smscodec.SubmitOptions{})
}

// SendSMSWithOptions 使用 PDU 模式发送短信，并允许调用方指定文本编码策略。
func (m *Manager) SendSMSWithOptions(phone, message string, opts smscodec.SubmitOptions) error {
	m.SetBusy(true)
	defer m.SetBusy(false)

	logger.Info(fmt.Sprintf("[%s] 准备发送短信 (PDU)", m.cfg.ID), "to", redactIdentifier(phone))

	// 确保处于 PDU 模式
	if _, err := m.ExecuteATHigh("AT+CMGF=0", 3*time.Second); err != nil {
		return fmt.Errorf("设置 PDU 模式失败: %w", err)
	}

	// 构建 PDUs
	pduHexList, tpduLenList, err := m.buildSMSPDUsWithOptions(phone, message, opts)
	if err != nil {
		return fmt.Errorf("构建 PDU 失败: %w", err)
	}

	for i, pduHex := range pduHexList {
		tpduLen := tpduLenList[i]
		logger.Debug(fmt.Sprintf("[%s] PDU 编码完成 (分片 %d/%d)", m.cfg.ID, i+1, len(pduHexList)), "pdu", pduHex, "tpdu_len", tpduLen)

		req := commandRequest{
			cmd:          fmt.Sprintf("AT+CMGS=%d", tpduLen), // PDU 长度 (不含 SMSC)
			respChan:     make(chan string, 1),
			errChan:      make(chan error, 1),
			timeout:      20 * time.Second, // 增加超时时间，因为包含两步
			highPriority: true,
			interactive:  true,
			waitPrompt:   true,
			followUp:     pduHex + "\x1A", // PDU + Ctrl+Z
		}

		// 使用高优先级通道原子执行
		select {
		case m.cmdChanHigh <- req:
		case <-time.After(5 * time.Second):
			return errors.New("command queue full")
		}

		// 等待最终响应 (OK)
		select {
		case resp := <-req.respChan:
			if !strings.Contains(resp, "OK") && !strings.Contains(resp, "+CMGS:") {
				return fmt.Errorf("发送分片 %d 失败: %s", i+1, resp)
			}
		case err := <-req.errChan:
			return fmt.Errorf("发送分片 %d 失败: %w", i+1, err)
		case <-time.After(20 * time.Second):
			return errors.New("发送超时")
		}

		// 稍微等待下一段发信，防止模组队列溢出
		if i < len(pduHexList)-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	logger.Info(fmt.Sprintf("[%s] 短信已发送", m.cfg.ID))
	return nil
}

// buildSMSPDUs 构建多段 SMS-SUBMIT PDU
// 返回: PDU 十六进制字符串列表, TPDU 长度列表 (不含 SMSC), 错误
func (m *Manager) buildSMSPDUs(phone, message string) ([]string, []int, error) {
	return m.buildSMSPDUsWithOptions(phone, message, smscodec.SubmitOptions{})
}

func (m *Manager) buildSMSPDUsWithOptions(phone, message string, opts smscodec.SubmitOptions) ([]string, []int, error) {
	tpduBytesList, tpduLenList, err := smscodec.BuildSubmitTPDUsWithOptions(phone, message, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("PDU 编码失败: %w", err)
	}

	// SMSC 使用默认 (长度字节为 00)
	smsc := []byte{0x00}
	var pduHexList []string

	for _, tpduBytes := range tpduBytesList {
		// 完整 PDU = SMSC + TPDU
		fullPDU := append(smsc, tpduBytes...)

		// 转换为十六进制
		pduHex := strings.ToUpper(hex.EncodeToString(fullPDU))
		pduHexList = append(pduHexList, pduHex)
	}

	return pduHexList, tpduLenList, nil
}

// USSDResult USSD 会话响应结果
type USSDResult struct {
	Status  int    `json:"status"`   // 0=无需操作, 1=需要用户回复, 2=会话结束, 5=网络超时
	Text    string `json:"text"`     // 解码后的可读文本
	RawText string `json:"raw_text"` // 原始文本（调试用）
	DCS     int    `json:"dcs"`      // 数据编码方案
}

// decodeUSSDText 根据 DCS (Data Coding Scheme) 解码 USSD 文本
// 参考 3GPP TS 23.038 的编码方案
func (m *Manager) decodeUSSDText(raw string, dcs int) string {
	if raw == "" {
		return ""
	}

	// 判断是否为 Hex 字符串（偶数长度、全 hex 字符）
	isHex := smscodec.IsHexString(raw)

	// DCS 高 4 位判断编码类型
	// 0x00-0x03 (0-3): GSM 7-bit
	// 0x04-0x07 (4-7): 8-bit data
	// 0x08-0x0B (8-11): UCS2
	// 0x0F (15): GSM 7-bit (unspecified)
	// 0x48 (72): UCS2
	codingGroup := (dcs >> 4) & 0x0F
	alphabet := (dcs >> 2) & 0x03

	isUCS2 := false
	if codingGroup == 0x00 || codingGroup == 0x01 {
		// 一般编码组
		isUCS2 = alphabet == 2 // bit3-2 = 10 -> UCS2
	} else if dcs == 72 {
		isUCS2 = true
	} else if dcs >= 0x40 && dcs <= 0x7F {
		// 消息类编码组
		isUCS2 = alphabet == 2
	}

	if isUCS2 && isHex {
		// UCS2: Hex 字符串 -> UTF-16BE -> UTF-8
		b, err := hex.DecodeString(raw)
		if err != nil {
			logger.Debug(fmt.Sprintf("[%s] USSD UCS2 hex 解码失败", m.cfg.ID), "err", err, "raw", raw)
			return raw
		}
		if len(b)%2 != 0 {
			return raw
		}
		u16 := make([]uint16, len(b)/2)
		for i := 0; i < len(b); i += 2 {
			u16[i/2] = uint16(b[i])<<8 | uint16(b[i+1])
		}
		return string(utf16.Decode(u16))
	}

	if isHex {
		// 可能是 GSM 7-bit packed 的 hex 表示
		b, err := hex.DecodeString(raw)
		if err != nil {
			return raw
		}
		unpacked := gsm7.Unpack7BitUSSD(b, 0)
		decoded, err := gsm7.Decode(unpacked)
		if err != nil {
			return raw
		}
		return string(decoded)
	}

	// 非 Hex 字符串，直接返回原文（某些 Modem 已经做了解码）
	return raw
}

// ExecuteUSSD 发送 USSD 指令并等待网络返回结果
// command: USSD 代码，如 "*100#", "*135#"
// timeout: 等待 URC 响应的超时时间
func (m *Manager) ExecuteUSSD(command string, timeout time.Duration) (*USSDResult, error) {
	// 清空可能残留的旧结果
	select {
	case <-m.ussdChan:
	default:
	}

	logger.Info(fmt.Sprintf("[%s] 开始执行 USSD", m.cfg.ID), "code", "[redacted]", "timeout", timeout.String())

	// 设置字符集，避免部分模组因使用非 GSM 的短信格式导致发不出去 USSD
	m.ExecuteATSilent(`AT+CSCS="GSM"`, 2*time.Second)

	// 发送 AT+CUSD=1,"command",15
	cmd := fmt.Sprintf(`AT+CUSD=1,"%s",15`, command)
	_, err := m.ExecuteAT(cmd, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("发送 USSD 指令失败: %w", err)
	}

	logger.Debug(fmt.Sprintf("[%s] USSD 发送成功，等待网络回包 URC (+CUSD)...", m.cfg.ID))

	// 阻塞等待 +CUSD URC 回调
	select {
	case result := <-m.ussdChan:
		logger.Info(fmt.Sprintf("[%s] 收到 USSD 返回", m.cfg.ID), "status", result.Status, "text", result.Text)
		return &result, nil
	case <-time.After(timeout):
		logger.Warn(fmt.Sprintf("[%s] USSD 响应网络超时（无回调），正在自动取消网络等待", m.cfg.ID), "timeout", timeout.String())
		// 超时后取消 USSD 会话
		m.CancelUSSD()
		return nil, errors.New("USSD 响应网络超时（无回调）")
	case <-m.stop:
		return nil, errors.New("设备已停止")
	}
}

// CancelUSSD 取消当前 USSD 会话
func (m *Manager) CancelUSSD() {
	_, err := m.ExecuteATSilent(`AT+CUSD=2`, 3*time.Second)
	if err != nil {
		logger.Debug(fmt.Sprintf("[%s] 取消 USSD 会话(AT+CUSD=2)失败", m.cfg.ID), "err", err)
	} else {
		logger.Debug(fmt.Sprintf("[%s] 已发送 USSD 取消指令 (AT+CUSD=2)", m.cfg.ID))
	}
}

// CheckAndEnableUAC 查询并确保开启 USB Audio Class (UAC) 接口
// 许多 Quectel 模块需要 AT+QCFG="USBCFG" 最后一位为 1 才能在系统枚举出声卡
// 返回 modified(bool) 表示是否发生了配置更改，如果发生了更改，必须重启才能生效
func (m *Manager) CheckAndEnableUAC() (bool, error) {
	resp, err := m.ExecuteAT(`AT+QCFG="USBCFG"?`, 3*time.Second)
	if err != nil {
		return false, err
	}

	// 查找 +QCFG: "usbcfg" 或 +QCFG: "USBCFG"
	idx := strings.Index(strings.ToLower(resp), `+qcfg: "usbcfg",`)
	if idx == -1 {
		return false, nil // 可能不支持该指令或格式不匹配
	}

	start := idx + 7 // Skip "+QCFG: " (7 chars)
	line := resp[start:]
	if end := strings.IndexAny(line, "\r\n"); end != -1 {
		line = line[:end]
	}
	line = strings.TrimSpace(line)

	// line 例: "usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,0
	parts := strings.Split(line, ",")
	if len(parts) < 8 {
		return false, nil // 参数过少跳过
	}

	lastIdx := len(parts) - 1
	lastVal := strings.TrimSpace(parts[lastIdx])

	if lastVal == "0" {
		parts[lastIdx] = "1"
		newArgs := strings.Join(parts, ",")
		newCmd := fmt.Sprintf(`AT+QCFG=%s`, newArgs)
		logger.Info(fmt.Sprintf("[%s] 检测到 UAC 接口未开启，正在通过 %s 执行开启", m.cfg.ID, newCmd))
		_, err := m.ExecuteAT(newCmd, 3*time.Second)
		if err != nil {
			return false, fmt.Errorf("动态开启 UAC 失败: %w", err)
		}
		return true, nil
	} else {
		logger.Debug(fmt.Sprintf("[%s] UAC 接口已处于开启状态 (%s)，无需重启", m.cfg.ID, lastVal))
	}
	return false, nil
}

// EnableUSBAudio 开启 USB Audio UAC模式 (AT+QPCMV=1,2)
// 注意：每次模块重启此设置都会失效，需要在开机后初始化流程或业务需要前调用
func (m *Manager) EnableUSBAudio() error {
	// 查询当前 QPCMV 状态避免重复发送
	enabled, _, err := m.QueryUSBAudioMode()
	if err == nil && enabled {
		logger.Debug(fmt.Sprintf("[%s] USB Audio 此时已经处于开启状态，无需重复下发指令", m.cfg.ID))
		return nil
	}

	_, err = m.ExecuteAT("AT+QPCMV=1,2", 2*time.Second)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] 开启 USB Audio (QPCMV) 失败", m.cfg.ID), "err", err)
		return err
	}
	logger.Info(fmt.Sprintf("[%s] USB Audio (QPCMV) 已配置开启", m.cfg.ID))
	return nil
}

// DisableUSBAudio 关闭 USB Audio 模式 (AT+QPCMV=0)
func (m *Manager) DisableUSBAudio() error {
	_, err := m.ExecuteAT("AT+QPCMV=0", 2*time.Second)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] 关闭 USB Audio 失败", m.cfg.ID), "err", err)
		return err
	}
	logger.Info(fmt.Sprintf("[%s] USB Audio 已配置关闭", m.cfg.ID))
	return nil
}

// QueryUSBAudioMode 查询当前 USB Audio 状态
func (m *Manager) QueryUSBAudioMode() (bool, int, error) {
	resp, err := m.ExecuteAT("AT+QPCMV?", 2*time.Second)
	if err != nil {
		return false, 0, err
	}
	idx := strings.Index(resp, "+QPCMV:")
	if idx == -1 {
		return false, 0, errors.New("查询失败: 响应未包含 +QPCMV")
	}
	parts := strings.Split(strings.TrimSpace(resp[idx+7:]), ",")
	enabled := strings.TrimSpace(parts[0]) == "1"
	mode := 0
	if len(parts) > 1 {
		fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &mode)
	}
	return enabled, mode, nil
}
