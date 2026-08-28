import Foundation
import AppKit

/// Go 后端进程管理器
/// 方案 A：DJOneHubNotifier 作为主 App，负责启动 / 停止 Go 后端（djonehub-macos）。
/// 不再依赖 LaunchAgent；Go 后端作为本 App 的子进程运行，App 退出时随之终止。
@MainActor
final class ProcessManager {
    private var backendProcess: Process?

    private let appSupportURL: URL = {
        let paths = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)
        let dir = paths[0].appendingPathComponent("DJOneHub")
        let logsDir = dir.appendingPathComponent("logs")
        try? FileManager.default.createDirectory(at: logsDir, withIntermediateDirectories: true)
        return dir
    }()

    private var logsURL: URL {
        appSupportURL.appendingPathComponent("logs")
    }

    /// 启动 Go 后端（异步，等待 HTTP 就绪后返回）
    func startAll() {
        Task {
            await startBackend()
            await waitForBackendReady()
        }
    }

    private func startBackend() async {
        guard backendProcess == nil || !(backendProcess?.isRunning ?? false) else { return }

        // App Bundle 内嵌的后端可执行文件与 libusb
        let bundleResources = Bundle.main.resourceURL ?? Bundle.main.bundleURL
        let backendExec = bundleResources.appendingPathComponent("djonehub-macos").path
        let libDir = bundleResources.appendingPathComponent("lib").path

        guard FileManager.default.fileExists(atPath: backendExec) else {
            print("[ProcessManager] 未找到后端可执行文件: \(backendExec)")
            return
        }

        let logFile = logsURL.appendingPathComponent("djonehub.log")
        FileManager.default.createFile(atPath: logFile.path, contents: nil)

        let process = Process()
        process.executableURL = URL(fileURLWithPath: backendExec)
        process.arguments = ["-listen", "127.0.0.1:7575"]

        var env = ProcessInfo.processInfo.environment
        env["DYLD_LIBRARY_PATH"] = libDir
        process.environment = env

        if let fileHandle = try? FileHandle(forWritingTo: logFile) {
            fileHandle.seekToEndOfFile()
            process.standardOutput = fileHandle
            process.standardError = fileHandle
        }

        do {
            try process.run()
            self.backendProcess = process
            print("[ProcessManager] Go 后端已启动 (PID: \(process.processIdentifier))")
        } catch {
            print("[ProcessManager] 启动 Go 后端失败: \(error)")
        }
    }

    /// 轮询 127.0.0.1:7575 直到后端就绪，最多等待 10 秒
    private func waitForBackendReady() async {
        let url = URL(string: "http://127.0.0.1:7575/")!
        var attempts = 0
        while attempts < 50 {
            if let process = backendProcess, !process.isRunning {
                print("[ProcessManager] 后端进程提前退出")
                break
            }
            var request = URLRequest(url: url)
            request.timeoutInterval = 1
            do {
                let (_, response) = try await URLSession.shared.data(for: request)
                if let http = response as? HTTPURLResponse, (200..<400).contains(http.statusCode) {
                    print("[ProcessManager] Go 后端 HTTP 服务已就绪")
                    return
                }
            } catch {
                // 等待重试
            }
            attempts += 1
            try? await Task.sleep(nanoseconds: 200_000_000) // 200ms
        }
        print("[ProcessManager] 后端就绪超时，继续运行（后端可能在启动中）")
    }

    /// 优雅停止所有子进程：SIGTERM → 等待 3 秒 → SIGKILL
    func stopAll() {
        print("[ProcessManager] 正在停止 Go 后端...")

        if let process = backendProcess, process.isRunning {
            process.terminate()
        }

        let startTime = Date()
        while backendProcess?.isRunning == true, Date().timeIntervalSince(startTime) < 3.0 {
            Thread.sleep(forTimeInterval: 0.1)
        }

        if let process = backendProcess, process.isRunning {
            kill(process.processIdentifier, SIGKILL)
        }

        backendProcess = nil
        print("[ProcessManager] Go 后端已停止")
    }
}
