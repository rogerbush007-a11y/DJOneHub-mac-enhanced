import AppKit
import Foundation

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private var statusItem: NSStatusItem!
    private var processManager: ProcessManager!
    private let webURL = URL(string: "http://127.0.0.1:7575")!
    
    func applicationDidFinishLaunching(_ notification: Notification) {
        // 1. 初始化状态栏
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        if let button = statusItem.button {
            button.image = loadStatusIcon()
            button.target = self
            button.action = #selector(statusBarButtonClicked(_:))
            button.sendAction(on: [.leftMouseUp, .rightMouseUp])
            button.toolTip = "DJOneHub (左键打开管理页面，右键显示菜单)"
        }
        
        // 2. 启动核心进程
        processManager = ProcessManager()
        processManager.startAll()
    }
    
    private func loadStatusIcon() -> NSImage? {
        let bundleResources = Bundle.main.resourceURL ?? Bundle.main.bundleURL
        let iconPath = bundleResources.appendingPathComponent("StatusIcon.png").path
        if let image = NSImage(contentsOfFile: iconPath) {
            image.isTemplate = true
            return image
        }
        // Fallback: 动态生成 18x18 的点表示状态栏图标
        let fallback = NSImage(size: NSSize(width: 18, height: 18))
        fallback.lockFocus()
        NSColor.black.setFill()
        NSBezierPath(ovalIn: NSRect(x: 4, y: 4, width: 10, height: 10)).fill()
        fallback.unlockFocus()
        fallback.isTemplate = true
        return fallback
    }
    
    @objc private func statusBarButtonClicked(_ sender: NSStatusBarButton) {
        guard let event = NSApp.currentEvent else { return }
        if event.type == .rightMouseUp || event.modifierFlags.contains(.control) {
            showContextMenu()
        } else {
            openWeb()
        }
    }
    
    private func showContextMenu() {
        let menu = NSMenu()
        
        let openItem = NSMenuItem(title: "打开 DJOneHub", action: #selector(openWeb), keyEquivalent: "")
        openItem.target = self
        menu.addItem(openItem)
        
        menu.addItem(NSMenuItem.separator())
        
        let autoLaunchItem = NSMenuItem(title: "开机自动启动", action: #selector(toggleAutoLaunch(_:)), keyEquivalent: "")
        autoLaunchItem.target = self
        autoLaunchItem.state = AutoLaunchManager.shared.isEnabled ? .on : .off
        menu.addItem(autoLaunchItem)
        
        menu.addItem(NSMenuItem.separator())
        
        let quitItem = NSMenuItem(title: "退出 DJOneHub", action: #selector(quitApp), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)
        
        statusItem.menu = menu
        statusItem.button?.performClick(nil)
        statusItem.menu = nil
    }
    
    @objc private func openWeb() {
        NSWorkspace.shared.open(webURL)
    }
    
    @objc private func toggleAutoLaunch(_ sender: NSMenuItem) {
        let success = AutoLaunchManager.shared.toggle()
        if success {
            sender.state = AutoLaunchManager.shared.isEnabled ? .on : .off
        }
    }
    
    @objc private func quitApp() {
        processManager.stopAll()
        NSApp.terminate(nil)
    }
    
    func applicationWillTerminate(_ notification: Notification) {
        processManager.stopAll()
    }
}
