import SwiftUI
import Combine

// MARK: - 外观与语言设置

enum AppAppearance: String, CaseIterable, Identifiable {
    case system
    case light
    case dark

    var id: String { rawValue }

    var displayName: String {
        switch self {
        case .system: return L10n.t("跟随系统")
        case .light: return L10n.t("浅色")
        case .dark: return L10n.t("深色")
        }
    }

    var colorScheme: ColorScheme? {
        switch self {
        case .system: return nil
        case .light: return .light
        case .dark: return .dark
        }
    }
}

enum AppLanguage: String, CaseIterable, Identifiable {
    case system
    case zh
    case en

    var id: String { rawValue }

    var displayName: String {
        switch self {
        case .system: return L10n.t("跟随系统")
        case .zh: return "简体中文"
        case .en: return "English"
        }
    }
}

@MainActor
final class AppSettings: ObservableObject {
    @Published var appearance: AppAppearance {
        didSet {
            UserDefaults.standard.set(appearance.rawValue, forKey: L10n.appearanceKey)
        }
    }

    @Published var language: AppLanguage {
        didSet {
            UserDefaults.standard.set(language.rawValue, forKey: L10n.languageKey)
        }
    }

    init() {
        let defaults = UserDefaults.standard
        appearance = AppAppearance(rawValue: defaults.string(forKey: L10n.appearanceKey) ?? "") ?? .system
        language = AppLanguage(rawValue: defaults.string(forKey: L10n.languageKey) ?? "") ?? .system
    }
}

// MARK: - 轻量本地化（中文 / English）

enum L10n {
    static let appearanceKey = "djonehub.appearance"
    static let languageKey = "djonehub.language"

    static var effectiveLanguage: AppLanguage {
        let saved = AppLanguage(rawValue: UserDefaults.standard.string(forKey: L10n.languageKey) ?? "") ?? .system
        if saved != .system { return saved }
        let preferred = Locale.preferredLanguages.first ?? ""
        return preferred.lowercased().hasPrefix("zh") ? .zh : .en
    }

    static func t(_ key: String) -> String {
        guard effectiveLanguage == .en else { return key }
        return en[key] ?? key
    }

    static func f(_ template: String, _ args: String...) -> String {
        let translated = t(template)
        return args.reduce(translated) { $0.replacingOccurrences(of: "%@", with: $1) }
    }

    static let en: [String: String] = [
        // 通用
        "跟随系统": "Follow System",
        "浅色": "Light",
        "深色": "Dark",
        "外观": "Appearance",
        "语言": "Language",
        "设置": "Settings",
        "取消": "Cancel",
        "发送": "Send",
        "刷新": "Refresh",
        "刷新全部": "Refresh All",
        "立即刷新": "Refresh Now",
        "处理中…": "Working…",
        "复制": "Copy",
        "退出": "Quit",
        "默认": "Default",
        "试听": "Preview",
        "手机": "Mobile",
        "邮箱": "Email",
        "键盘": "Keypad",
        "更多": "More",
        "服务控制": "Service Control",
        "完全退出模块全部服务": "Quit All Module Services",
        "确认完全退出？": "Quit Everything?",
        "将停止 4G 后台、短信守护与通知服务，并退出本应用": "Stops the 4G backend, SMS guard and notification service, then quits this app.",
        "将停止 DJOneHub 后台服务、WiFi/短信守护与本通知程序；需要时请重新运行本应用恢复。": "This stops the DJOneHub backend, WiFi/SMS guard and this notifier. Run the app again when you need them.",
        // 标签页
        "拨号": "Dial",
        "最近通话": "Recents",
        "短信": "Messages",
        "通讯录": "Contacts",
        // 拨号
        "输入号码": "Enter number",
        "正在拨号…": "Dialing…",
        "来电…": "Incoming…",
        "接听": "Accept",
        "拒接": "Decline",
        "挂断": "End",
        "静音": "Mute",
        "取消静音": "Unmute",
        "停止录音": "Stop Recording",
        "通话录音": "Call Recording",
        "通话状态": "Call Status",
        "通话中": "On Call",
        "通话保持": "On Hold",
        "等待对方接听…": "Waiting for answer…",
        "等待接听…": "Waiting…",
        "已拨出": "Outgoing",
        "已接听": "Answered",
        "未接": "Missed",
        "未接来电": "Missed Call",
        "无法连接 DJOneHub 后台：%@": "Cannot reach DJOneHub backend: %@",
        // 最近通话 / 录音
        "暂无通话记录": "No Recents",
        "该通话没有录音": "No recording for this call",
        "暂无录音。通话中点击「录音」按钮开始。": "No recordings yet. Tap Record during a call to start.",
        "录音": "Record",
        "打开录音文件夹": "Open Recording Folder",
        "在 Finder 中显示": "Show in Finder",
        "发短信给 %@": "Message %@",
        "%@ 段录音": "%@ recording(s)",
        "%@ 秒": "%@ sec",
        "%@ 个 · %@": "%@ · %@",
        // 短信
        "新信息": "New Message",
        "撰写新短信": "New Message",
        "本机号码：": "My number: ",
        "收件人：": "To:",
        "输入号码或姓名": "Number or name",
        "从通讯录选择": "Pick from Contacts",
        "短信内容": "Message",
        "需要访问通讯录才能选择联系人": "Allow Contacts access to pick a recipient",
        "短信将通过 4G 模块发送": "Sent via the 4G module",
        "回复短信…": "Reply…",
        "清空模块旧短信": "Clear module SMS",
		"清空全部短信": "Clear all SMS",
		"清空全部短信？": "Clear all SMS?",
		"这会删除 SIM 卡和模块存储中的全部短信，无法恢复。": "This permanently deletes all SMS stored on the SIM card and module.",
        "暂无短信": "No Messages",
        "验证码 %@": "Code %@",
        "%@ 条": "%@ messages",
        // 通讯录
        "搜索姓名或号码": "Search name or number",
        "通讯录为空": "No contacts",
        "刷新同步本机通讯录": "Refresh Contacts from This Mac",
        "刷新自动同步系统通讯录": "Refresh auto-syncs system contacts",
        "没有匹配的联系人": "No matching contacts",
        "授权访问通讯录": "Allow Contacts Access",
        "需要访问通讯录才能显示联系人姓名": "Allow Contacts access to show names",
        "%@ 位联系人": "%@ contacts",
        "呼叫 %@": "Call %@",
        "未知号码": "Unknown",
        // 铃声
        "来电铃声": "Ringtone",
        "添加铃声": "Add Ringtone",
        "打开铃声文件夹": "Open Ringtone Folder",
        "支持 aiff / wav / mp3 / m4a，放入铃声文件夹后自动出现": "Supports aiff / wav / mp3 / m4a; drop into the ringtone folder and it appears automatically",
        // 模块（原「更多」）
        "通用": "General",
        "每 2 秒自动更新": "Auto-refresh every 2s",
        "模块状态": "Module Status",
        "网络模式": "Network Mode",
        "信号强度": "Signal",
        "本次流量": "Session Data",
        "网络": "Network",
        "允许 4G 上网": "Allow 4G Data",
        "关闭后强制禁止 Mac 使用 4G；短信和来电监控不受影响": "When off, Mac is blocked from using 4G; SMS and call monitoring still work",
        "检查 4G 出口": "Check 4G Route",
        "检查代理出口": "Check Proxy Route",
        "重启模块": "Reboot Module",
        "已允许 4G 上网": "4G data allowed",
        "已禁止 4G 上网": "4G data blocked",
        "已发送模块重启指令": "Module reboot sent",
        "检查通过": "Check Passed",
        "检查未通过": "Check Failed",
        "定位": "Location",
        "GPS 定位": "GPS Positioning",
        "默认关闭；开启后仅在本机读取定位信息": "Off by default; reads location on this Mac only when enabled",
        "坐标": "Coordinates",
        "卫星": "Satellites",
        "等待定位…": "Waiting for fix…",
        "已启动定位": "Location started",
        "已停止定位": "Location stopped",
        "定位已刷新": "Location refreshed",
        "eSIM / 卡片": "eSIM / Card",
        "卡片类型": "Card Type",
        "查询中…": "Checking…",
        "未命名 Profile": "Unnamed Profile",
        "使用中": "Active",
        "切换": "Switch",
        "已切换 Profile": "Profile switched",
        "AT 调试": "AT Debug",
        "AT 指令": "AT Command",
        "状态": "Status",
        "运营商": "Operator",
        "SIM 卡接入": "SIM Card",
        "已接入": "Inserted",
        "未接入": "Not Inserted",
        "下载速度": "Download",
        "上传速度": "Upload",
        "网络端口诊断": "Network Diagnostics",
        "正在读取网络诊断…": "Reading network diagnostics…",
        "USB 网卡": "USB NIC",
        "已识别": "Detected",
        "未识别": "Not Detected",
        "默认出口": "Default Route",
        "蜂窝数据": "Cellular Data",
        "蜂窝 IP": "Cellular IP",
        "USB 枚举": "USB Device",
        "读取部分诊断失败：": "Some diagnostics failed: ",
        "固件": "Firmware",
        "序列号": "Serial Number",
        "当前启用": "Active Profile",
        "模块实际卡": "Module Card",
        "蜂窝注册": "Registration",
        "等待网络注册": "Waiting for network registration",
        "模块已接管当前 Profile": "Module is using this profile",
        "重命名": "Rename",
        "模块资料": "Module Notes",
        "通讯录检测": "Phonebook Check",
        "下载新 Profile": "Download New Profile",
        "正在检测卡内通讯录能力，不会写入联系人…": "Checking SIM phonebook capability; no contacts are written…",
        "支持 SM 卡内存储": "Supports SIM storage",
        "未发现 SM 卡内存储": "No SIM storage found",
        "已安全选中 SM 存储": "SIM storage selected",
        "无法选中 SM 存储": "Cannot select SIM storage",
        "模块支持读取卡内联系人": "Module can read SIM contacts",
        "模块未确认读取命令": "Read command unconfirmed",
        "模块声明支持写入接口": "Write interface supported",
        "模块未确认写入命令": "Write command unconfirmed",
        "重命名 Profile": "Rename Profile",
        "Profile 名称": "Profile Name",
        "保存": "Save",
        "资料保存在本机，按 ICCID 与当前 Profile 关联": "Saved on this Mac, linked to this profile's ICCID",
        "模块内名称（可选）": "Name (Optional)",
        "模块号码（可选）": "Phone (Optional)",
        "用途标签（可选）": "Tags (Optional)",
        "下载新的 Profile": "Download New Profile",
        "将向 SM-DP+ 服务器下载并写入新的 eSIM Profile。写入期间请勿拔出模块。": "Downloads and installs a new eSIM profile from the SM-DP+ server. Do not unplug the module while writing.",
        "SM-DP+ 地址": "SM-DP+ Address",
        "Matching ID（可选）": "Matching ID (Optional)",
        "确认码（可选）": "Confirmation Code (Optional)",
        "IMEI（必填）": "IMEI (Required)",
        "AID（可选）": "AID (Optional)",
        "开始下载": "Start Download",
        "确认删除 Profile？": "Delete This Profile?",
        "删除不可恢复，确定删除当前 Profile 吗？": "This cannot be undone. Delete the profile?",
        "Profile 名称已修改": "Profile renamed",
        "Profile 已删除": "Profile deleted",
        "模块资料已保存": "Module notes saved",
        "已切换 Profile，模块正在重启并重新读取新卡…": "Profile switched; module is restarting to load the new card…",
        "可用": "Free",
        "完全退出": "Quit All",
        "停止 4G 后台、短信守护与通知服务": "Stops 4G background, SMS guard and notification services",
        "关闭": "Close",
        "删除": "Delete",
        "已激活 %@": "Active %@",
        "未激活": "Inactive",
        "可用 %@": "Available %@",
        "固件 %@": "Firmware %@",
        "EID": "EID",
        "Profile 下载完成": "Profile Download Complete",
        "SIM 通讯录": "SIM Contacts",
        "信号": "Signal",
        "写入接口": "Write Interface",
        "当前卡片": "Current Card",
        "读取能力": "Read Capability",
        "无": "None",
    ]
}
