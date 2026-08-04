import Foundation
import ServiceManagement

@MainActor
final class AutoLaunchManager {
    static let shared = AutoLaunchManager()
    
    private init() {}
    
    var isEnabled: Bool {
        if #available(macOS 13.0, *) {
            return SMAppService.mainApp.status == .enabled
        } else {
            return false
        }
    }
    
    func setEnabled(_ enable: Bool) -> Bool {
        if #available(macOS 13.0, *) {
            do {
                if enable {
                    if SMAppService.mainApp.status != .enabled {
                        try SMAppService.mainApp.register()
                    }
                } else {
                    if SMAppService.mainApp.status == .enabled {
                        try SMAppService.mainApp.unregister()
                    }
                }
                return true
            } catch {
                print("设置开机自启动失败: \(error)")
                return false
            }
        } else {
            return false
        }
    }
    
    func toggle() -> Bool {
        return setEnabled(!isEnabled)
    }
}
