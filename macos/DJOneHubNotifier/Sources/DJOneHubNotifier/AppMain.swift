import AppKit
import Foundation
import SwiftUI

@main
enum DJOneHubNotifierMain {
    @MainActor
    static func main() {
        if CommandLine.arguments.contains("--self-test") {
            SelfTest.run()
            return
        }
        let app = NSApplication.shared
        let delegate = AppDelegate(arguments: CommandLine.arguments)
        app.delegate = delegate
        app.setActivationPolicy(.regular)
        app.run()
    }
}

enum SelfTest {
    static func run() {
        precondition(NotificationText.displayNumber("  ") == "未知号码")
        let codeMessage = SMSMessage(
            sender: "10086",
            content: "您的验证码是 482913",
            code: "482913",
            timestamp: Date()
        )
        precondition(NotificationText.smsPreview(codeMessage) == "验证码 482913")
        let longMessage = SMSMessage(
            sender: "10086",
            content: "第一行\n第二行以及一段很长很长的短信正文",
            code: nil,
            timestamp: Date()
        )
        precondition(NotificationText.smsPreview(longMessage, limit: 8) == "第一行 第二行以…")
        print("DJOneHubNotifier self-test passed")
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private let api: DJOneHubAPI
    private let panel: NotifierPanel
    private let gpsMapPanel = GPSMapPanel()
    private let previewMode: String?
    private let snapshotPath: String?
    private let callCenter: CallCenter
    private let contactStore: ContactStore
    private let ringtoneStore: RingtoneStore
    private let appSettings: AppSettings
    private var mainWindow: NSWindow?
    private let healthCheck: Bool
    private let reviewSafe: Bool
    // Go 后端进程管理器（方案 A：DJOneHubNotifier 作为主 App，负责管理后端生命周期）
    private let processManager = ProcessManager()

    private var callTimer: Timer?
    private var smsTimer: Timer?
    private var gpsTimer: Timer?
    private var cellularTimer: Timer?
    private var gpsAnimationTimer: Timer?
    private var gpsSearchTimeoutTimer: Timer?
    private var gpsStatusItem: NSStatusItem?
    private var cellularStatusItem: NSStatusItem?
    private var gpsWasEnabled = false
    private var gpsSearchTimedOut = false
    private var gpsStartupFramesRemaining = 0
    private var gpsAnimationFrame = 0
    private var lastActiveCallID: String?
    private var seenCallHistoryIDs = Set<String>()
    private var seenMessageIDs = Set<String>()
    private var initializedCalls = false
    private var initializedMessages = false
    private var consecutiveErrors = 0
    // URLSession may take longer than the timer interval while the module is
    // handling an AT command. Never let an older response overwrite a newer
    // incoming-call state and hide the panel.
    private var callPollInFlight = false
    private var smsPollInFlight = false
    private var wasCallConnected = false

    init(arguments: [String]) {
        let baseURL = Self.argumentValue("--base-url", in: arguments)
            .flatMap(URL.init(string:))
            ?? URL(string: "http://127.0.0.1:7575/")!
        api = DJOneHubAPI(baseURL: baseURL)
        previewMode = Self.argumentValue("--preview", in: arguments)
        snapshotPath = Self.argumentValue("--snapshot", in: arguments)
        healthCheck = arguments.contains("--health-check")
        reviewSafe = arguments.contains("--review-safe")
        callCenter = CallCenter(api: api)
        contactStore = ContactStore()
        panel = NotifierPanel()
        panel.contactStore = contactStore
        ringtoneStore = RingtoneStore()
        appSettings = AppSettings()
        panel.appSettings = appSettings
        super.init()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        installMainMenu()
        if healthCheck {
            Task { await runHealthCheck() }
            return
        }
        if let previewMode {
            showPreview(previewMode)
            if let snapshotPath {
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { [weak self] in
                    guard let self else { return }
                    try? self.panel.saveSnapshot(to: URL(fileURLWithPath: snapshotPath))
                    NSApplication.shared.terminate(nil)
                }
            }
            return
        }
        let demoCall = CommandLine.arguments.contains("--show-call")
        if !demoCall {
            // 启动 Go 后端（方案 A：由本 App 直接管理后端进程，不依赖 LaunchAgent）
            processManager.startAll()
            smsTimer = Timer.scheduledTimer(withTimeInterval: 10, repeats: true) { [weak self] _ in
                Task { @MainActor in await self?.pollMessages() }
            }
            gpsTimer = Timer.scheduledTimer(withTimeInterval: 15, repeats: true) { [weak self] _ in
                Task { @MainActor in await self?.pollGPSStatus() }
            }
            cellularTimer = Timer.scheduledTimer(withTimeInterval: 30, repeats: true) { [weak self] _ in
                Task { @MainActor in await self?.pollCellularStatus() }
            }
            NSWorkspace.shared.notificationCenter.addObserver(
                self,
                selector: #selector(systemDidWake),
                name: NSWorkspace.didWakeNotification,
                object: nil
            )
            Task {
                // 等待 Go 后端启动就绪后（延时 2 秒），发起初始探测刷新
                try? await Task.sleep(nanoseconds: 2_000_000_000)
                await pollMessages()
                await pollGPSStatus()
                await pollCellularStatus()
            }
            callCenter.start()
            if !reviewSafe {
                Task { await contactStore.requestAccess() }
            }
        }
        if demoCall {
            let demo = CallRecord(
                id: "demo-call",
                index: 0,
                direction: "incoming",
                state: "active",
                number: "189 •••• ••••",
                startedAt: Date().addingTimeInterval(-75),
                updatedAt: Date(),
                endedAt: nil,
                missed: false
            )
            callCenter.activeCall = demo
            showMainWindow()
        }
        if CommandLine.arguments.contains("--show-window") {
            showMainWindow()
        }
        if previewMode == nil, let snapshotPath {
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.8) { [weak self] in
                guard let self else { return }
                try? self.saveMainWindowSnapshot(to: URL(fileURLWithPath: snapshotPath))
                NSApplication.shared.terminate(nil)
            }
        }
    }

    private func runHealthCheck() async {
        do {
            let calls = try await api.callStatus()
            let messages = try await api.messages()
            print(
                "health-check passed: callPolling=\(calls.polling) " +
                "callHistory=\(calls.history?.count ?? 0) smsCount=\(messages.count)"
            )
            NSApplication.shared.terminate(nil)
        } catch {
            fputs("health-check failed: \(error.localizedDescription)\n", stderr)
            exit(1)
        }
    }

    func applicationWillTerminate(_ notification: Notification) {
        callTimer?.invalidate()
        smsTimer?.invalidate()
        gpsTimer?.invalidate()
        cellularTimer?.invalidate()
        stopGPSAnimation()
        cancelGPSSearchTimeout()
        removeGPSStatusItem()
        removeCellularStatusItem()
        // 优雅停止 Go 后端（SIGTERM → 等待 3s → SIGKILL）
        processManager.stopAll()
    }

    private func pollCalls() async {
        guard !callPollInFlight else {
            return
        }
        callPollInFlight = true
        defer { callPollInFlight = false }

        do {
            let status = try await api.callStatus()
            consecutiveErrors = 0
            let history = status.history ?? []

            if !initializedCalls {
                initializedCalls = true
                seenCallHistoryIDs = Set(history.map(\.id))
            }

            if let active = status.active,
               active.direction == "incoming",
               active.state == "incoming" || active.state == "waiting" {
                if active.id != lastActiveCallID {
                    showIncoming(active)
                }
                lastActiveCallID = active.id
            } else {
                if lastActiveCallID != nil {
                    panel.hide()
                }
                ringtoneStore.stopRinging()
                lastActiveCallID = nil
            }

            if let active = status.active {
                if active.state == "active" || active.state == "held" {
                    if !wasCallConnected, !(mainWindow?.isVisible ?? false) {
                        showMainWindow()
                    }
                    wasCallConnected = true
                } else {
                    wasCallConnected = false
                }
            } else {
                if wasCallConnected, !(mainWindow?.isVisible ?? false) {
                    showMainWindow()
                }
                wasCallConnected = false
            }

            if let missed = history.first(where: { $0.missed && !seenCallHistoryIDs.contains($0.id) }) {
                seenCallHistoryIDs.insert(missed.id)
                showMissed(missed)
            }
            seenCallHistoryIDs.formUnion(history.map(\.id))
        } catch {
            consecutiveErrors += 1
        }
    }

    /// 系统从休眠唤醒后，等待 USB 设备重新枚举稳定，然后恢复轮询与菜单栏状态，
    /// 避免 4G 模块/网络在唤醒后无法自动找回。
    @objc private func systemDidWake(_ notification: Notification) {
        Task { @MainActor in
            // 等 USB 设备重新枚举、网络栈稳定后再恢复轮询
            try? await Task.sleep(nanoseconds: 4_000_000_000)
            consecutiveErrors = 0
            try? await Task.sleep(nanoseconds: 1_000_000_000)
            await pollCalls()
            await pollMessages()
            await pollGPSStatus()
            await pollCellularStatus()
        }
    }

    private func pollMessages() async {
        guard !smsPollInFlight else {
            return
        }
        smsPollInFlight = true
        defer { smsPollInFlight = false }

        do {
            let messages = try await api.messages()
            if !initializedMessages {
                initializedMessages = true
                seenMessageIDs = Set(messages.map(\.identity))
                return
            }
            guard let newest = messages.first(where: { !seenMessageIDs.contains($0.identity) }) else {
                return
            }
            seenMessageIDs.formUnion(messages.map(\.identity))
            showMessage(newest)
        } catch {
            // Call polling owns the offline warning to avoid duplicate banners.
        }
    }

    private func pollGPSStatus() async {
        guard let status = try? await api.gpsStatus() else { return }
        if status.enabled {
            let signalLevel = Self.gpsSignalLevel(for: status.lastFix)
            if !gpsWasEnabled {
                gpsWasEnabled = true
                gpsSearchTimedOut = false
                startGPSAnimation()
                scheduleGPSSearchTimeout()
                gpsMapPanel.show()
            }
            gpsMapPanel.update(with: status.lastFix)
            if let signalLevel {
                stopGPSAnimation()
                cancelGPSSearchTimeout()
                showGPSStatusItem(signalLevel: signalLevel)
            } else if gpsSearchTimedOut {
                stopGPSAnimation()
                showGPSStatusItem(signalLevel: nil)
            } else if gpsAnimationTimer == nil {
                startGPSAnimation()
            }
        } else {
            gpsWasEnabled = false
            gpsSearchTimedOut = false
            stopGPSAnimation()
            cancelGPSSearchTimeout()
            removeGPSStatusItem()
            gpsMapPanel.hide()
        }
    }

    private func pollCellularStatus() async {
        guard (try? await api.isUsingCellularRoute()) == true,
              let modem = try? await api.modemStatus(),
              let signalDBM = modem.signalDBM,
              Self.isCellularNetwork(modem.networkMode)
        else {
            removeCellularStatusItem()
            return
        }
        showCellularStatusItem(signalLevel: Self.cellularSignalLevel(signalDBM))
    }

    private func showCellularStatusItem(signalLevel: Int) {
        if cellularStatusItem == nil {
            cellularStatusItem = NSStatusBar.system.statusItem(withLength: 48)
            cellularStatusItem?.button?.target = self
            cellularStatusItem?.button?.action = #selector(openDJOneHubFromCellularMenuBar)
        }
        cellularStatusItem?.button?.image = Self.cellularStatusImage(signalLevel: signalLevel)
        cellularStatusItem?.button?.toolTip = "DJOneHub 4G 正在接管网络；点击打开控制面板"
    }

    private func removeCellularStatusItem() {
        guard let cellularStatusItem else { return }
        NSStatusBar.system.removeStatusItem(cellularStatusItem)
        self.cellularStatusItem = nil
    }

    private func showGPSStatusItem(signalLevel: Int?, scanPhase: Int? = nil) {
        if gpsStatusItem == nil {
            gpsStatusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
            gpsStatusItem?.button?.target = self
            gpsStatusItem?.button?.action = #selector(openDJOneHubFromMenuBar)
        }
        gpsStatusItem?.button?.image = Self.gpsStatusImage(signalLevel: signalLevel, scanPhase: scanPhase)
        if signalLevel != nil {
            gpsStatusItem?.button?.toolTip = "DJOneHub GPS 定位已开启；点击展开或收起地图"
        } else if gpsSearchTimedOut {
            gpsStatusItem?.button?.toolTip = "DJOneHub GPS 定位已开启：暂未找到卫星信号；点击展开或收起地图"
        } else {
            gpsStatusItem?.button?.toolTip = "DJOneHub GPS 定位已开启：正在搜索卫星；点击展开或收起地图"
        }
    }

    private func removeGPSStatusItem() {
        guard let gpsStatusItem else { return }
        NSStatusBar.system.removeStatusItem(gpsStatusItem)
        self.gpsStatusItem = nil
    }

    private func startGPSAnimation() {
        guard gpsAnimationTimer == nil else { return }
        gpsStartupFramesRemaining = 8
        gpsAnimationFrame = 0
        renderGPSAnimationFrame()
        gpsAnimationTimer = Timer.scheduledTimer(withTimeInterval: 0.8, repeats: true) { [weak self] _ in
            Task { @MainActor in
                self?.renderGPSAnimationFrame()
            }
        }
    }

    private func stopGPSAnimation() {
        gpsAnimationTimer?.invalidate()
        gpsAnimationTimer = nil
        gpsStartupFramesRemaining = 0
    }

    private func scheduleGPSSearchTimeout() {
        cancelGPSSearchTimeout()
        gpsSearchTimeoutTimer = Timer.scheduledTimer(withTimeInterval: 120, repeats: false) { [weak self] _ in
            Task { @MainActor in
                guard let self, self.gpsWasEnabled else { return }
                self.gpsSearchTimedOut = true
                self.stopGPSAnimation()
                self.showGPSStatusItem(signalLevel: nil)
            }
        }
    }

    private func cancelGPSSearchTimeout() {
        gpsSearchTimeoutTimer?.invalidate()
        gpsSearchTimeoutTimer = nil
    }

    private func renderGPSAnimationFrame() {
        gpsAnimationFrame += 1
        if gpsStartupFramesRemaining > 0 {
            // First show a neutral, rising four-bar search animation. It is a
            // UI transition only; no position or signal result is fabricated.
            let level = ((gpsAnimationFrame - 1) % 4) + 1
            showGPSStatusItem(signalLevel: level)
            gpsStartupFramesRemaining -= 1
            return
        }
        // Until the module reports a valid fix, use a red rotating scan arc.
        showGPSStatusItem(signalLevel: nil, scanPhase: gpsAnimationFrame % 8)
    }

    @objc private func openDJOneHubFromMenuBar() {
        gpsMapPanel.toggle()
    }

    @objc private func openDJOneHubFromCellularMenuBar() {
        openDJOneHub()
    }

    private static func isCellularNetwork(_ mode: String?) -> Bool {
        let normalized = mode?.uppercased() ?? ""
        return normalized.contains("LTE") || normalized.contains("4G")
    }

    private static func cellularSignalLevel(_ dbm: Int) -> Int {
        if dbm >= -65 { return 4 }
        if dbm >= -75 { return 3 }
        if dbm >= -85 { return 2 }
        return 1
    }

    // A compact stepped mobile-signal indicator that mirrors the iPhone
    // status bar: four signal bars on the left, a "4G" label on the right.
    // The label is vertically centered and sized to the same height as the
    // tallest signal bar (13.8pt). Drawn as a template image so macOS renders
    // it white on a dark menu bar and black on a light one, like iOS.
    private static func cellularStatusImage(signalLevel: Int) -> NSImage {
        let image = NSImage(size: NSSize(width: 48, height: 18))
        image.lockFocus()
        // 模板色：菜单栏深色时系统渲染为白色，浅色时自动变黑，与 iPhone 状态栏一致
        let color = NSColor.black
        let active = color
        let inactive = color.withAlphaComponent(0.30)
        for (index, height) in [4.2, 7.4, 10.6, 13.8].enumerated() {
            (index < signalLevel ? active : inactive).setFill()
            let bar = NSBezierPath(
                roundedRect: NSRect(x: CGFloat(index) * 5.2, y: 1, width: 3.6, height: height),
                xRadius: 0.9,
                yRadius: 0.9
            )
            bar.fill()
        }
        // "4G"：与菜单栏电量百分比同字号（menuBarFont 13pt、SF Pro 常规字重不加粗），垂直居中于信号条
        let label = "4G" as NSString
        label.draw(
            at: NSPoint(x: 22, y: 0.5),
            withAttributes: [
                .font: NSFont.systemFont(ofSize: 13, weight: .regular),
                .foregroundColor: color,
            ]
        )
        image.unlockFocus()
        image.isTemplate = true
        image.accessibilityDescription = "DJOneHub 4G 信号 \(signalLevel) 格"
        return image
    }

    private static func gpsSignalLevel(for fix: GPSFixSummary?) -> Int? {
        guard let fix,
              let satellites = Int(fix.satellites),
              let hdop = Double(fix.hdop),
              satellites >= 4,
              hdop.isFinite
        else { return nil }
        if satellites >= 10 && hdop <= 1.2 { return 4 }
        if satellites >= 8 && hdop <= 2 { return 3 }
        if satellites >= 6 && hdop <= 3.5 { return 2 }
        if hdop <= 5 { return 1 }
        return nil
    }

    // `satellite` is not present in every macOS SF Symbols release. Draw a
    // compact vector icon so the menu-bar indicator is reliable on macOS 13.
    // A missing or weak fix is deliberately red and omits signal bars.
    private static func gpsStatusImage(signalLevel: Int?, scanPhase: Int? = nil) -> NSImage {
        let image = NSImage(size: NSSize(width: 18, height: 18))
        image.lockFocus()
        let weakSignal = signalLevel == nil
        let color: NSColor = weakSignal ? .systemRed : .black
        color.setFill()
        color.setStroke()

        NSBezierPath(ovalIn: NSRect(x: 7, y: 7, width: 4, height: 4)).fill()

        let upperPanel = NSBezierPath()
        upperPanel.move(to: NSPoint(x: 2, y: 12))
        upperPanel.line(to: NSPoint(x: 6.5, y: 10.5))
        upperPanel.line(to: NSPoint(x: 7.5, y: 12.5))
        upperPanel.line(to: NSPoint(x: 3, y: 14))
        upperPanel.close()
        upperPanel.fill()

        let lowerPanel = NSBezierPath()
        lowerPanel.move(to: NSPoint(x: 10.5, y: 5.5))
        lowerPanel.line(to: NSPoint(x: 15, y: 4))
        lowerPanel.line(to: NSPoint(x: 16, y: 6))
        lowerPanel.line(to: NSPoint(x: 11.5, y: 7.5))
        lowerPanel.close()
        lowerPanel.fill()

        let antenna = NSBezierPath()
        antenna.move(to: NSPoint(x: 10, y: 10))
        antenna.line(to: NSPoint(x: 14.5, y: 14.5))
        antenna.lineWidth = 1.3
        antenna.stroke()

        if let signalLevel {
            for level in 1...signalLevel {
                let radius = CGFloat(2) + CGFloat(level) * 1.65
                let wave = NSBezierPath()
                wave.appendArc(withCenter: NSPoint(x: 10, y: 10), radius: radius, startAngle: 30, endAngle: 72, clockwise: false)
                wave.lineWidth = 1.15
                wave.stroke()
            }
        } else if let scanPhase {
            let scan = NSBezierPath()
            let startAngle = CGFloat(scanPhase * 45)
            scan.appendArc(
                withCenter: NSPoint(x: 9, y: 9),
                radius: 7,
                startAngle: startAngle,
                endAngle: startAngle + 78,
                clockwise: false
            )
            scan.lineWidth = 1.35
            scan.stroke()
        }

        image.unlockFocus()
        image.isTemplate = !weakSignal
        image.accessibilityDescription = weakSignal
            ? "DJOneHub GPS 信号弱"
            : "DJOneHub GPS 定位正常"
        return image
    }

    private func showIncoming(_ call: CallRecord) {
        ringtoneStore.startRinging()
        panel.show(
            .incoming(
                number: NotificationText.displayNumber(call.number),
                startedAt: call.startedAt
            ),
            onReject: { [weak self] in
                Task { @MainActor in
                    await self?.rejectCall()
                    self?.showMainWindow()
                }
            },
            onAnswer: { [weak self] in
                Task { @MainActor in
                    self?.callCenter.answer()
                    self?.showMainWindow()
                }
            },
            onOpen: openDJOneHub
        )
    }

    private func showMissed(_ call: CallRecord) {
        ringtoneStore.stopRinging()
        panel.show(
            .missed(
                number: NotificationText.displayNumber(call.number),
                startedAt: call.startedAt
            ),
            onReject: {},
            onAnswer: {},
            onOpen: openDJOneHub
        )
    }

    private func showMessage(_ message: SMSMessage) {
        NSSound(named: "Glass")?.play()
        panel.show(
            .sms(
                sender: message.sender.isEmpty ? "未知发送方" : message.sender,
                preview: NotificationText.smsPreview(message),
                code: message.code
            ),
            onReject: {},
            onAnswer: {},
            onOpen: openDJOneHub
        )
    }

    private func rejectCall() async {
        do {
            _ = try await api.rejectCall()
            panel.hide()
        } catch {
            panel.show(
                .error(message: "拒接失败：\(error.localizedDescription)"),
                onReject: {},
                onAnswer: {},
                onOpen: openDJOneHub
            )
        }
    }

    private func openDJOneHub() {
        showMainWindow()
    }

    private func showPreview(_ mode: String) {
        switch mode {
        case "sms":
            panel.show(
                .sms(sender: "10086", preview: "验证码 482913", code: "482913"),
                onReject: {},
                onAnswer: {},
                onOpen: openDJOneHub
            )
        default:
            panel.show(
                .incoming(number: "189 •••• ••••", startedAt: Date()),
                onReject: {},
                onAnswer: {},
                onOpen: openDJOneHub
            )
        }
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showMainWindow()
        return true
    }

    private func showMainWindow() {
        if mainWindow == nil {
            let rootView = PhoneAppView()
                .environmentObject(callCenter)
                .environmentObject(contactStore)
                .environmentObject(ringtoneStore)
                .environmentObject(appSettings)
            let hosting = NSHostingController(rootView: rootView)
            let window = NSWindow(contentViewController: hosting)
            window.title = "DJOneHub"
            window.styleMask = [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView]
            window.titlebarAppearsTransparent = true
            window.titleVisibility = .hidden
            // macOS 26 透明 NSWindow + SwiftUI 存在已知渲染 bug（内容偶发 180° 颠倒），
            // 主窗口改为不透明，圆角与外观全部由 SwiftUI 主体自绘，规避系统级问题。
            window.isOpaque = true
            window.backgroundColor = PhoneStyle.appBackgroundNS
            window.isMovableByWindowBackground = true
            window.setContentSize(NSSize(width: 400, height: 700))
            window.center()
            window.isReleasedWhenClosed = false
            window.tabbingMode = .disallowed
            mainWindow = window
        }
        mainWindow?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func saveMainWindowSnapshot(to url: URL) throws {
        guard let view = mainWindow?.contentView else { return }
        view.layoutSubtreeIfNeeded()
        guard let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return }
        view.cacheDisplay(in: view.bounds, to: bitmap)
        guard let data = bitmap.representation(using: .png, properties: [:]) else { return }
        try data.write(to: url, options: .atomic)
    }

    private func installMainMenu() {
        let mainMenu = NSMenu()

        let appItem = NSMenuItem()
        mainMenu.addItem(appItem)
        let appMenu = NSMenu()
        appMenu.addItem(
            withTitle: "关于 DJOneHub",
            action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)),
            keyEquivalent: ""
        )
        appMenu.addItem(.separator())
        appMenu.addItem(
            withTitle: "隐藏 DJOneHub",
            action: #selector(NSApplication.hide(_:)),
            keyEquivalent: "h"
        )
        appMenu.addItem(.separator())
        appMenu.addItem(
            withTitle: "退出 DJOneHub",
            action: #selector(NSApplication.terminate(_:)),
            keyEquivalent: "q"
        )
        appItem.submenu = appMenu

        let editItem = NSMenuItem()
        mainMenu.addItem(editItem)
        let editMenu = NSMenu(title: "编辑")
        editMenu.addItem(withTitle: "拷贝", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        editMenu.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = editMenu

        NSApp.mainMenu = mainMenu
    }

    private static func argumentValue(_ flag: String, in arguments: [String]) -> String? {
        guard let index = arguments.firstIndex(of: flag), arguments.indices.contains(index + 1) else {
            return nil
        }
        return arguments[index + 1]
    }
}
