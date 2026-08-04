import Foundation
import AppKit

@MainActor
final class ProcessManager {
    private var backendProcess: Process?
    private var notifierProcess: Process?
    
    private let appSupportURL: URL = {
        let paths = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)
        let dir = paths[0].appendingPathComponent("DJOneHub")
        let logsDir = dir.appendingPathComponent("logs")
        try? FileManager.default.createDirectory(at: logsDir, withIntermediateDirectories: true)
        return dir
    }()
    
    private var logsURL: URL {
        return appSupportURL.appendingPathComponent("logs")
    }
    
    func startAll() {
        Task {
            await startBackend()
            await waitForBackendReady()
            startNotifier()
        }
    }
    
    private func startBackend() async {
        guard backendProcess == nil || !backendProcess!.isRunning else { return }
        
        let bundleResources = Bundle.main.resourceURL ?? Bundle.main.bundleURL
        let backendExec = bundleResources.appendingPathComponent("djonehub-macos").path
        let libDir = bundleResources.appendingPathComponent("lib").path
        
        guard FileManager.default.fileExists(atPath: backendExec) else {
            print("未找到后端可执行文件: \(backendExec)")
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
            print("Go 后端已启动 (PID: \(process.processIdentifier))")
        } catch {
            print("启动 Go 后端失败: \(error)")
        }
    }
    
    private func waitForBackendReady() async {
        let url = URL(string: "http://127.0.0.1:7575/")!
        var attempts = 0
        while attempts < 50 {
            if let process = backendProcess, !process.isRunning {
                print("后端进程提前退出")
                break
            }
            var request = URLRequest(url: url)
            request.timeoutInterval = 1
            do {
                let (_, response) = try await URLSession.shared.data(for: request)
                if let http = response as? HTTPURLResponse, (200..<400).contains(http.statusCode) {
                    print("Go 后端 HTTP 服务已就绪")
                    return
                }
            } catch {
                // 等待重试
            }
            attempts += 1
            try? await Task.sleep(nanoseconds: 200_000_000) // 200ms
        }
    }
    
    private func startNotifier() {
        guard notifierProcess == nil || !notifierProcess!.isRunning else { return }
        
        let bundleResources = Bundle.main.resourceURL ?? Bundle.main.bundleURL
        let notifierExec = bundleResources
            .appendingPathComponent("DJOneHubNotifier.app")
            .appendingPathComponent("Contents")
            .appendingPathComponent("MacOS")
            .appendingPathComponent("DJOneHubNotifier").path
        
        guard FileManager.default.fileExists(atPath: notifierExec) else {
            print("未找到通知助手可执行文件: \(notifierExec)")
            return
        }
        
        let logFile = logsURL.appendingPathComponent("notifier.log")
        FileManager.default.createFile(atPath: logFile.path, contents: nil)
        
        let process = Process()
        process.executableURL = URL(fileURLWithPath: notifierExec)
        
        if let fileHandle = try? FileHandle(forWritingTo: logFile) {
            fileHandle.seekToEndOfFile()
            process.standardOutput = fileHandle
            process.standardError = fileHandle
        }
        
        do {
            try process.run()
            self.notifierProcess = process
            print("通知助手已启动 (PID: \(process.processIdentifier))")
        } catch {
            print("启动通知助手失败: \(error)")
        }
    }
    
    func stopAll() {
        print("正在停止所有子进程...")
        
        if let process = notifierProcess, process.isRunning {
            process.terminate()
        }
        
        if let process = backendProcess, process.isRunning {
            process.terminate()
        }
        
        let startTime = Date()
        while (notifierProcess?.isRunning == true || backendProcess?.isRunning == true) && Date().timeIntervalSince(startTime) < 3.0 {
            Thread.sleep(forTimeInterval: 0.1)
        }
        
        if let process = notifierProcess, process.isRunning {
            kill(process.processIdentifier, SIGKILL)
        }
        if let process = backendProcess, process.isRunning {
            kill(process.processIdentifier, SIGKILL)
        }
        
        notifierProcess = nil
        backendProcess = nil
        print("所有子进程已停止")
    }
}
