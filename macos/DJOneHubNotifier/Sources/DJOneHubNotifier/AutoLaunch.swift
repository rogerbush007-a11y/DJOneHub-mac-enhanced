import SwiftUI
import ServiceManagement

/// 开机自启管理器（使用 macOS 13+ SMAppService.mainApp）
@MainActor
final class AutoLaunchManager: ObservableObject {
    static let shared = AutoLaunchManager()

    @Published private(set) var isEnabled: Bool = false

    private init() {
        refresh()
    }

    private func refresh() {
        isEnabled = SMAppService.mainApp.status == .enabled
    }

    func setEnabled(_ enabled: Bool) {
        do {
            if enabled {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
            refresh()
        } catch {
            print("[AutoLaunch] 设置开机自启失败: \(error.localizedDescription)")
        }
    }
}

/// 开机自启设置行（在「设置」MoreView 中使用）
struct AutoLaunchToggleRow: View {
    @StateObject private var manager = AutoLaunchManager.shared

    var body: some View {
        HStack(spacing: 10) {
            Image(systemName: "power.circle")
                .font(.system(size: 15, weight: .medium))
                .foregroundStyle(.secondary)
                .frame(width: 20)
            VStack(alignment: .leading, spacing: 2) {
                Text("开机自动启动")
                    .font(.system(size: 13, weight: .medium))
                Text("登录后自动启动 DJOneHub，监听来电与短信")
                    .font(.system(size: 10))
                    .foregroundStyle(.secondary)
            }
            Spacer(minLength: 4)
            Toggle("", isOn: Binding(
                get: { manager.isEnabled },
                set: { manager.setEnabled($0) }
            ))
            .toggleStyle(.switch)
            .labelsHidden()
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
    }
}
