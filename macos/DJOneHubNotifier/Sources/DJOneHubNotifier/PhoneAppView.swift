import SwiftUI
import AppKit

// MARK: - 标签页

private enum PhoneTab: String, CaseIterable, Identifiable {
    case dial = "拨号"
    case recents = "最近通话"
    case messages = "短信"
    case contacts = "通讯录"
    case settings = "设置"

    var id: String { rawValue }

    var icon: String {
        switch self {
        case .dial: return "circle.grid.3x3.fill"
        case .recents: return "clock.fill"
        case .messages: return "message.fill"
        case .contacts: return "person.crop.circle.fill"
        case .settings: return "gearshape.fill"
        }
    }
}

enum PhoneStyle {
    static let green = Color.green
    /// 界面打底：浅色纯白、深色近黑，去掉灰色底。
    static let appBackgroundNS = NSColor(name: nil) { appearance in
        appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
            ? NSColor(white: 0.09, alpha: 1)
            : NSColor.white
    }
    static let appBackground = Color(nsColor: appBackgroundNS)
}

// MARK: - 主视图

struct PhoneAppView: View {
    @EnvironmentObject private var calls: CallCenter
    @EnvironmentObject private var settings: AppSettings
    @State private var tab: PhoneTab = .dial
    @State private var pendingSMSRecipient: String?

    var body: some View {
        ZStack {
            VStack(spacing: 0) {
                Group {
                    switch tab {
                    case .dial:
                        DialPadView()
                    case .recents:
                        RecentsView()
                    case .messages:
                        MessagesView(pendingRecipient: $pendingSMSRecipient)
                    case .contacts:
                        ContactsView(onComposeSMS: { phone in
                            pendingSMSRecipient = phone
                            tab = .messages
                        })
                    case .settings:
                        SettingsView()
                    }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                TabBar(selection: $tab)
            }
            .id(settings.language.rawValue)
            if let call = calls.activeCall {
                CallActiveView(call: call)
                    // macOS 26 透明窗口下 move 过渡会偶发触发内容颠倒渲染，
                    // 改用纯淡入避免翻转，保持接通动画平滑。
                    .transition(.opacity)
            }
        }
        .animation(.easeInOut(duration: 0.22), value: calls.activeCall?.id)
        .frame(minWidth: 370, minHeight: 600)
        // 主体：系统连续圆角，与玻璃窗口形状一致（macOS 26 窗口主体圆角 20pt）
        .background(
            RoundedRectangle(cornerRadius: 20, style: .continuous)
                .fill(PhoneStyle.appBackground)
        )
        .clipShape(RoundedRectangle(cornerRadius: 20, style: .continuous))
        .tint(PhoneStyle.green)
        .preferredColorScheme(settings.appearance.colorScheme)
    }
}

struct PhoneCard: ViewModifier {
    func body(content: Content) -> some View {
        content
            .padding(.horizontal, 12)
            .padding(.vertical, 10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(
                RoundedRectangle(cornerRadius: 12, style: .continuous)
                    .fill(.thinMaterial)
                    .overlay(
                        RoundedRectangle(cornerRadius: 12, style: .continuous)
                            .strokeBorder(Color.primary.opacity(0.1), lineWidth: 0.5)
                    )
                    .shadow(color: .black.opacity(0.08), radius: 8, x: 0, y: 2)
            )
    }
}

private struct TabBar: View {
    @Binding var selection: PhoneTab

    var body: some View {
        HStack(spacing: 4) {
            ForEach(PhoneTab.allCases) { tab in
                Button {
                    selection = tab
                } label: {
                    VStack(spacing: 2) {
                        Image(systemName: tab.icon)
                            .font(.system(size: 17, weight: .medium))
                        Text(L10n.t(tab.rawValue))
                            .font(.caption2.weight(.semibold))
                    }
                    .foregroundStyle(selection == tab ? Color.white : Color.secondary)
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 6)
                    .background {
                        if selection == tab {
                            Capsule()
                                .fill(PhoneStyle.green)
                                .shadow(color: .black.opacity(0.15), radius: 4, x: 0, y: 1)
                        }
                    }
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
            }
        }
        .padding(5)
        .background(Capsule().fill(.ultraThinMaterial))
        .overlay(Capsule().strokeBorder(.white.opacity(0.4), lineWidth: 0.5))
        .shadow(color: .black.opacity(0.12), radius: 12, x: 0, y: 4)
        .padding(.horizontal, 12)
        .padding(.bottom, 10)
    }
}

// MARK: - 拨号键盘

struct DialPadView: View {
    @EnvironmentObject private var calls: CallCenter
    @EnvironmentObject private var contacts: ContactStore

    /// 完整/后缀匹配到的联系人（显示头像与姓名）
    private var matchedContact: ContactStore.Contact? {
        let query = calls.numberInput.trimmingCharacters(in: .whitespaces)
        guard !query.isEmpty else { return nil }
        return contacts.contact(for: query)
    }

    /// 前缀匹配的联系人建议（最多 3 个，点选自动填入号码）
    private var dialSuggestions: [ContactStore.Contact] {
        let query = calls.numberInput.trimmingCharacters(in: .whitespaces)
        guard matchedContact == nil, query.count >= 2 else { return [] }
        let target = ContactStore.normalized(query)
        guard !target.isEmpty else { return [] }
        return Array(
            contacts.contacts.filter { contact in
                contact.phones.contains { $0.hasPrefix(target) }
            }.prefix(3)
        )
    }

    private struct DialKeySpec: Hashable {
        let digit: String
        let letters: String
    }

    private static let rows: [[DialKeySpec]] = [
        [DialKeySpec(digit: "1", letters: ""), DialKeySpec(digit: "2", letters: "ABC"), DialKeySpec(digit: "3", letters: "DEF")],
        [DialKeySpec(digit: "4", letters: "GHI"), DialKeySpec(digit: "5", letters: "JKL"), DialKeySpec(digit: "6", letters: "MNO")],
        [DialKeySpec(digit: "7", letters: "PQRS"), DialKeySpec(digit: "8", letters: "TUV"), DialKeySpec(digit: "9", letters: "WXYZ")],
        [DialKeySpec(digit: "*", letters: ""), DialKeySpec(digit: "0", letters: "+"), DialKeySpec(digit: "#", letters: "")]
    ]

    var body: some View {
        VStack(spacing: 16) {
            Spacer(minLength: 6)

            Text(calls.numberInput.isEmpty ? L10n.t("输入号码") : calls.numberInput)
                .font(.system(size: 34, weight: .light, design: .rounded))
                .foregroundStyle(calls.numberInput.isEmpty ? Color.secondary : Color.primary)
                .lineLimit(1)
                .minimumScaleFactor(0.5)
                .frame(height: 46)
                .padding(.horizontal, 24)
                .contentTransition(.numericText())

            // 联系人识别：号码下方小字蓝色显示，固定高度避免挤占拨号键布局
            Group {
                if let matched = matchedContact {
                    Text(matched.name)
                        .contentTransition(.opacity)
                } else if !dialSuggestions.isEmpty {
                    Button {
                        let first = dialSuggestions[0]
                        calls.numberInput = first.phones.first ?? first.name
                    } label: {
                        Text(dialSuggestions.map(\.name).joined(separator: "、"))
                            .lineLimit(1)
                    }
                    .buttonStyle(.plain)
                    .contentShape(Rectangle())
                } else {
                    Text(" ")
                }
            }
            .font(.system(size: 12))
            .foregroundStyle(Color.blue)
            .lineLimit(1)
            .frame(height: 20)
            .padding(.horizontal, 24)

            VStack(spacing: 12) {
                ForEach(Self.rows, id: \.self) { row in
                    HStack(spacing: 22) {
                        ForEach(row, id: \.digit) { key in
                            DialKey(digit: key.digit, letters: key.letters) {
                                calls.numberInput.append(key.digit)
                            }
                        }
                    }
                }
                HStack(spacing: 22) {
                    Color.clear.frame(width: 76, height: 76)
                    Button {
                        calls.dial()
                    } label: {
                        ZStack {
                            Circle().fill(Color.green).frame(width: 76, height: 76)
                            Image(systemName: "phone.fill")
                                .font(.system(size: 30, weight: .semibold))
                                .foregroundStyle(.white)
                        }
                    }
                    .buttonStyle(.plain)
                    .disabled(calls.numberInput.trimmingCharacters(in: .whitespaces).isEmpty || calls.isDialing)
                    .opacity(calls.numberInput.trimmingCharacters(in: .whitespaces).isEmpty ? 0.5 : 1)

                    Button {
                        if !calls.numberInput.isEmpty {
                            calls.numberInput.removeLast()
                        }
                    } label: {
                        Image(systemName: "delete.left")
                            .font(.system(size: 22, weight: .regular))
                            .foregroundStyle(.secondary)
                            .frame(width: 76, height: 76)
                    }
                    .buttonStyle(.plain)
                }
            }
            .padding(.top, 10)

            if let error = calls.lastActionError ?? (!calls.isOnline ? calls.lastError : nil) {
                Text(calls.lastActionError == nil ? L10n.f("无法连接 DJOneHub 后台：%@", error) : error)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .padding(.horizontal, 20)
                    .multilineTextAlignment(.center)
            }
            Spacer(minLength: 6)
        }
        .padding(.bottom, 16)
        .modifier(DialKeyboardMonitor(onKey: handleDialKey))
    }

    private func handleDialKey(_ key: String) {
        switch key {
        case "0"..."9", "*", "#", "+":
            calls.numberInput.append(key)
        case "\u{7F}", "\u{08}":
            if !calls.numberInput.isEmpty {
                calls.numberInput.removeLast()
            }
        case "\r", "\n":
            calls.dial()
        default:
            break
        }
    }
}

/// 拨号页专用：系统键盘直接输入数字（仅在拨号页可见时生效）
private struct DialKeyboardMonitor: ViewModifier {
    let onKey: (String) -> Void
    @State private var monitor: Any?

    func body(content: Content) -> some View {
        content
            .onAppear {
                guard monitor == nil else { return }
                monitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { event in
                    guard event.modifierFlags.intersection([.command, .control, .option]).isEmpty else {
                        return event
                    }
                    guard let window = event.window, window == NSApp.keyWindow else { return event }
                    if let responder = window.firstResponder, responder is NSTextView {
                        return event
                    }
                    guard let chars = event.charactersIgnoringModifiers else { return event }
                    for ch in chars {
                        onKey(String(ch))
                    }
                    return nil
                }
            }
            .onDisappear {
                if let monitor {
                    NSEvent.removeMonitor(monitor)
                    self.monitor = nil
                }
            }
    }
}

private struct DialKey: View {
    let digit: String
    let letters: String
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            ZStack {
                Circle()
                    .fill(Color(nsColor: .controlBackgroundColor))
                    .shadow(color: .black.opacity(0.08), radius: 4, x: 0, y: 2)
                    .frame(width: 66, height: 66)
                VStack(spacing: 0) {
                    Text(digit)
                        .font(.system(size: 27, weight: .regular, design: .rounded))
                        .foregroundStyle(.primary)
                    if !letters.isEmpty {
                        Text(letters)
                            .font(.caption2.weight(.semibold))
                            .foregroundStyle(.secondary)
                    }
                }
            }
            .frame(width: 74, height: 74)
            .contentShape(Circle())
        }
        .buttonStyle(.plain)
        .onHover { inside in
            NSCursor.pointingHand.push()
            if !inside { NSCursor.pop() }
        }
    }
}

// MARK: - 最近通话

struct RecentsView: View {
    @EnvironmentObject private var calls: CallCenter
    @State private var recordings: [RecordingFile] = []
    @State private var expanded: Set<String> = []
    @State private var playingID: String?
    @State private var sound: NSSound?

    private func recordings(for call: CallRecord) -> [RecordingFile] {
        let start = call.startedAt.addingTimeInterval(-90).timeIntervalSince1970
        let end = (call.endedAt ?? call.startedAt.addingTimeInterval(600))
            .addingTimeInterval(90).timeIntervalSince1970
        return recordings
            .filter { $0.date.timeIntervalSince1970 >= start && $0.date.timeIntervalSince1970 <= end }
            .sorted { $0.date < $1.date }
    }

    private func refreshRecordings() {
        let fm = FileManager.default
        let base = fm.urls(for: .applicationSupportDirectory, in: .userDomainMask).first!
        let dir = base.appendingPathComponent("DJOneHub/recordings", isDirectory: true)
        let files = (try? fm.contentsOfDirectory(
            at: dir,
            includingPropertiesForKeys: [.fileSizeKey, .creationDateKey],
            options: [.skipsHiddenFiles]
        )) ?? []
        recordings = files
            .filter { $0.pathExtension.lowercased() == "wav" }
            .compactMap { url -> RecordingFile? in
                let values = try? url.resourceValues(forKeys: [.fileSizeKey, .creationDateKey])
                return RecordingFile(
                    id: url.lastPathComponent,
                    url: url,
                    size: Int64(values?.fileSize ?? 0),
                    date: values?.creationDate ?? .distantPast
                )
            }
            .sorted { $0.date > $1.date }
    }

    private func togglePlay(_ file: RecordingFile) {
        if playingID == file.id {
            sound?.stop()
            sound = nil
            playingID = nil
            return
        }
        sound?.stop()
        let newSound = NSSound(contentsOf: file.url, byReference: true)
        newSound?.play()
        sound = newSound
        playingID = file.id
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text(L10n.t("最近通话"))
                    .font(.title3.weight(.semibold))
                Spacer()
                Button {
                    refreshRecordings()
                    Task { await calls.refreshCalls() }
                } label: {
                    Image(systemName: "arrow.clockwise")
                        .font(.body.weight(.semibold))
                }
                .buttonStyle(.plain)
                .help(L10n.t("刷新通话记录与录音"))
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)

            Divider()

            if calls.history.isEmpty {
                Spacer()
                VStack(spacing: 8) {
                    Image(systemName: "phone.arrow.up.right")
                        .font(.system(size: 28))
                        .foregroundStyle(.secondary)
                    Text(L10n.t("暂无通话记录"))
                        .font(.body)
                        .foregroundStyle(.secondary)
                }
                Spacer()
            } else {
                ScrollView {
                    LazyVStack(spacing: 8) {
                        ForEach(calls.history) { call in
                            VStack(spacing: 8) {
                                RecentsRow(
                                    call: call,
                                    expanded: expanded.contains(call.id),
                                    recordingCount: recordings(for: call).count,
                                    onToggle: {
                                        if expanded.contains(call.id) {
                                            expanded.remove(call.id)
                                        } else {
                                            expanded.insert(call.id)
                                        }
                                    },
                                    onDial: {
                                        if let number = call.number, !number.isEmpty {
                                            calls.dialNumber(number)
                                        }
                                    }
                                )
                                .modifier(PhoneCard())
                                if expanded.contains(call.id) {
                                    RecentsRecordingsSection(
                                        recordings: recordings(for: call),
                                        playingID: playingID,
                                        onPlay: togglePlay
                                    )
                                    .modifier(PhoneCard())
                                    .transition(.opacity)
                                }
                            }
                        }
                    }
                    .padding(12)
                }
                .onAppear(perform: refreshRecordings)
                .onChange(of: calls.history) { _ in
                    refreshRecordings()
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

private struct RecentsRow: View {
    @EnvironmentObject private var contacts: ContactStore
    let call: CallRecord
    var expanded = false
    var recordingCount = 0
    let onToggle: () -> Void
    let onDial: () -> Void

    private var icon: (name: String, color: Color, label: String) {
        if call.missed {
            return ("phone.down.fill", .red, L10n.t("未接来电"))
        }
        switch call.direction {
        case "outgoing":
            return ("phone.arrow.up.right.fill", .green, L10n.t("已拨出"))
        default:
            return ("phone.arrow.down.left.fill", Color.blue, L10n.t("已接听"))
        }
    }

    private var timeText: String {
        if let endedAt = call.endedAt {
            let seconds = max(0, Int(endedAt.timeIntervalSince(call.startedAt)))
            if seconds >= 60 {
                return String(format: "%d:%02d", seconds / 60, seconds % 60)
            }
            return L10n.f("%@ 秒", String(seconds))
        }
        return ""
    }

    var body: some View {
        HStack(spacing: 10) {
            Button(action: onToggle) {
                HStack(spacing: 12) {
                    Image(systemName: icon.name)
                        .font(.system(size: 14))
                        .foregroundStyle(icon.color)
                        .frame(width: 24)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(contacts.displayName(for: call.number))
                            .font(.system(size: 14, weight: .medium))
                            .foregroundStyle(.primary)
                        Text(RecentsRow.dateText(call.startedAt))
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                    }
                    Spacer(minLength: 6)
                    VStack(alignment: .trailing, spacing: 2) {
                        if call.missed {
                            Text(L10n.t("未接"))
                                .font(.caption.weight(.semibold))
                                .foregroundStyle(.red)
                        } else {
                            Text(timeText)
                                .font(.footnote.monospaced())
                                .foregroundStyle(.secondary)
                        }
                        if recordingCount > 0 {
                            Label(L10n.f("%@ 段录音", String(recordingCount)), systemImage: "waveform")
                                .font(.caption2.weight(.medium))
                                .foregroundStyle(.secondary)
                        }
                    }
                    Image(systemName: expanded ? "chevron.down" : "chevron.right")
                        .font(.caption2.weight(.semibold))
                        .foregroundStyle(.tertiary)
                        .frame(width: 8)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)

            Button(action: onDial) {
                Image(systemName: "phone.fill")
                    .font(.system(size: 12, weight: .semibold))
                    .foregroundStyle(.white)
                    .frame(width: 26, height: 26)
                    .background(Circle().fill(PhoneStyle.green))
            }
            .buttonStyle(.plain)
            .disabled(call.number?.isEmpty ?? true)
            .opacity(call.number?.isEmpty ?? true ? 0.4 : 1)
            .help(L10n.t("拨号"))
        }
        .padding(.vertical, 5)
    }

    private static func dateText(_ date: Date) -> String {
        let formatter = DateFormatter()
        let isEN = L10n.effectiveLanguage == .en
        formatter.locale = Locale(identifier: isEN ? "en_US" : "zh_CN")
        if Calendar.current.isDateInToday(date) {
            formatter.dateFormat = isEN ? "HH:mm" : "今天 HH:mm"
        } else if Calendar.current.isDateInYesterday(date) {
            formatter.dateFormat = isEN ? "HH:mm" : "昨天 HH:mm"
        } else {
            formatter.dateFormat = isEN ? "M/d HH:mm" : "M月d日 HH:mm"
        }
        return formatter.string(from: date)
    }
}

// MARK: - 通话记录下的录音

private struct RecentsRecordingsSection: View {
    let recordings: [RecordingFile]
    let playingID: String?
    let onPlay: (RecordingFile) -> Void

    var body: some View {
        VStack(spacing: 0) {
            if recordings.isEmpty {
                Text(L10n.t("该通话没有录音"))
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .padding(.vertical, 4)
            } else {
                ForEach(Array(recordings.enumerated()), id: \.element.id) { index, file in
                    HStack(spacing: 10) {
                        Button {
                            onPlay(file)
                        } label: {
                            Image(systemName: playingID == file.id ? "stop.fill" : "play.fill")
                                .font(.caption.weight(.semibold))
                                .frame(width: 22)
                                .foregroundStyle(Color.accentColor)
                        }
                        .buttonStyle(.plain)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(L10n.t("通话录音"))
                                .font(.footnote.weight(.medium))
                            Text(recordingMeta(file))
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                        }
                        Spacer()
                        Button {
                            NSWorkspace.shared.activateFileViewerSelecting([file.url])
                        } label: {
                            Image(systemName: "folder")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        .buttonStyle(.plain)
                        .help(L10n.t("在 Finder 中显示"))
                    }
                    .padding(.vertical, 4)
                    if index < recordings.count - 1 {
                        Divider().padding(.leading, 40)
                    }
                }
            }
        }
    }

    private func recordingMeta(_ file: RecordingFile) -> String {
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyy-MM-dd HH:mm"
        let size: String
        if file.size < 1_048_576 {
            size = "\(file.size / 1024) KB"
        } else {
            size = String(format: "%.1f MB", Double(file.size) / 1_048_576)
        }
        return "\(formatter.string(from: file.date)) · \(size)"
    }
}

// MARK: - 短信会话

private struct SentSMS: Identifiable {
    let id = UUID()
    let phone: String
    let content: String
    let sentAt: Date
}

private struct Conversation: Identifiable {
    let sender: String
    let lastContent: String
    let lastTime: Date
    var id: String { sender }
}

private enum ThreadItem: Identifiable {
    case received(SMSMessage)
    case sent(SentSMS)

    var id: String {
        switch self {
        case let .received(message): return message.identity
        case let .sent(message): return message.id.uuidString
        }
    }

    var date: Date {
        switch self {
        case let .received(message): return message.timestamp
        case let .sent(message): return message.sentAt
        }
    }

    var content: String {
        switch self {
        case let .received(message): return message.content
        case let .sent(message): return message.content
        }
    }

    var isSent: Bool {
        if case .sent = self { return true }
        return false
    }
}

private struct MessagesView: View {
    @EnvironmentObject private var calls: CallCenter
    @EnvironmentObject private var settings: AppSettings
    @Binding var pendingRecipient: String?
    @State private var received: [SMSMessage] = []
    @State private var sent: [SentSMS] = []
    @State private var selected: String?
    @State private var loading = false
    @State private var error: String?
    @State private var showingComposer = false
    @State private var showingClearSMSConfirmation = false
    @State private var composerRecipient = ""
    @State private var ownNumber = ""
    @State private var autoCleanupME = true

    private var conversations: [Conversation] {
        var latest: [String: (content: String, date: Date)] = [:]
        for message in received {
            let sender = message.sender.isEmpty ? "未知号码" : message.sender
            if let current = latest[sender] {
                if message.timestamp > current.date {
                    latest[sender] = (message.content, message.timestamp)
                }
            } else {
                latest[sender] = (message.content, message.timestamp)
            }
        }
        for message in sent {
            let sender = message.phone.isEmpty ? "未知号码" : message.phone
            if let current = latest[sender] {
                if message.sentAt > current.date {
                    latest[sender] = (message.content, message.sentAt)
                }
            } else {
                latest[sender] = (message.content, message.sentAt)
            }
        }
        return latest
            .map { Conversation(sender: $0.key, lastContent: $0.value.content, lastTime: $0.value.date) }
            .sorted { $0.lastTime > $1.lastTime }
    }

    private func threadItems(for sender: String) -> [ThreadItem] {
        var items: [ThreadItem] = []
        items += received
            .filter { $0.sender == sender || ($0.sender.isEmpty && sender == "未知号码") }
            .map { .received($0) }
        items += sent
            .filter { $0.phone == sender }
            .map { .sent($0) }
        return items.sorted { $0.date < $1.date }
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                Text(L10n.t("短信"))
                    .font(.title3.weight(.semibold))
                if !ownNumber.isEmpty {
                    Text(ownNumber)
                        .font(.caption.monospacedDigit())
                        .foregroundStyle(.secondary)
                }
                Spacer()
                if let error {
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(.red)
                        .lineLimit(1)
                }
                Button {
                    Task { await load() }
                } label: {
                    if loading {
                        ProgressView().controlSize(.small)
                    } else {
                        Image(systemName: "arrow.clockwise")
                            .font(.body.weight(.semibold))
                    }
                }
                .buttonStyle(.plain)
                .disabled(loading)
                .help(L10n.t("刷新短信列表"))
                Button {
                    composerRecipient = ""
                    showingComposer = true
                } label: {
                    Image(systemName: "square.and.pencil")
                        .font(.body.weight(.semibold))
                }
                .buttonStyle(.plain)
                .help(L10n.t("撰写新短信"))
                Button {
                    showingClearSMSConfirmation = true
                } label: {
                    Image(systemName: "trash")
                        .font(.system(size: 12, weight: .semibold))
                }
                .buttonStyle(.plain)
                .help(L10n.t("清空全部短信"))
                Button {
                    Task { await load() }
                } label: {
                    Image(systemName: "arrow.clockwise")
                        .font(.body.weight(.semibold))
                }
                .buttonStyle(.plain)
                .disabled(loading)
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)

            Divider()

            Toggle(L10n.t("读取后自动清理模块短信"), isOn: $autoCleanupME)
                .font(.caption)
                .padding(.horizontal, 14)
                .padding(.vertical, 7)
                .onChange(of: autoCleanupME) { enabled in
                    Task { try? await calls.apiClient.setSMSAutoCleanup(enabled) }
                }

            Divider()

            if let sender = selected {
                ThreadView(
                    sender: sender,
                    items: threadItems(for: sender),
                    onBack: { selected = nil },
                    onSend: send
                )
            } else {
                ScrollView {
                    LazyVStack(spacing: 8) {
                        ForEach(conversations) { conversation in
                            Button {
                                selected = conversation.sender
                            } label: {
                                ConversationRow(conversation: conversation)
                                    .modifier(PhoneCard())
                            }
                            .buttonStyle(.plain)
                        }
                    }
                    .padding(12)
                }
                .overlay {
                    if conversations.isEmpty {
                        VStack(spacing: 8) {
                            Image(systemName: "message.fill")
                                .font(.system(size: 28))
                                .foregroundStyle(.secondary)
                            Text(L10n.t("暂无短信"))
                                .font(.body)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
        .task {
            if let identity = try? await calls.apiClient.simIdentity() { ownNumber = identity.phoneNumber }
            if let status = try? await calls.apiClient.smsStatus() { autoCleanupME = status.autoCleanupME }
            await load()
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 6_000_000_000)
                await load()
            }
        }
        .overlay {
            if showingComposer {
                ZStack {
                    Color.black.opacity(0.25)
                        .ignoresSafeArea()
                        .onTapGesture {
                            showingComposer = false
                            pendingRecipient = nil
                        }
                    ComposeMessageView(
                        initialRecipient: composerRecipient,
                        onSend: { phone, content in
                            let trimmed = content.trimmingCharacters(in: .whitespacesAndNewlines)
                            sent.append(SentSMS(phone: phone, content: trimmed, sentAt: Date()))
                            selected = phone
                            showingComposer = false
                            pendingRecipient = nil
                        },
                        onClose: {
                            showingComposer = false
                            pendingRecipient = nil
                        }
                    )
                    .transition(.scale(scale: 0.96).combined(with: .opacity))
                }
                .animation(.easeInOut(duration: 0.18), value: showingComposer)
            }
        }
        .task(id: pendingRecipient) {
            if let pendingRecipient, !pendingRecipient.isEmpty {
                composerRecipient = pendingRecipient
                showingComposer = true
            }
        }
        .alert(L10n.t("清空全部短信？"), isPresented: $showingClearSMSConfirmation) {
            Button(L10n.t("删除"), role: .destructive) {
                Task { await clearModuleSMS() }
            }
            Button(L10n.t("取消"), role: .cancel) {}
        } message: {
            Text(L10n.t("这会删除 SIM 卡和模块存储中的全部短信，无法恢复。"))
        }
    }

    private func load() async {
        loading = true
        defer { loading = false }
        do {
            received = try await calls.apiClient.messages()
            self.error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func clearModuleSMS() async {
        do {
            try await calls.apiClient.clearModuleSMS()
            self.error = nil
            received = []
            selected = nil
            await load()
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func send(to sender: String, content: String) {
        let trimmed = content.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        Task {
            do {
                _ = try await calls.apiClient.sendSMS(to: sender, message: trimmed)
                sent.append(SentSMS(phone: sender, content: trimmed, sentAt: Date()))
            } catch {
                self.error = error.localizedDescription
            }
        }
    }
}


/// 新短信（iMessage 风格）：取消 / 新信息 / 发送导航，收件人支持从通讯录直接选取
private struct ComposeMessageView: View {
    @EnvironmentObject private var calls: CallCenter
    @EnvironmentObject private var contacts: ContactStore
    @State private var recipient: String
    @State private var content = ""
    @State private var sending = false
    @State private var errorText: String?
    @State private var showingContacts = false
    @State private var contactSearch = ""
    @State private var phoneChooser: ContactStore.Contact?
    let onSend: (String, String) -> Void
    let onClose: () -> Void

    init(
        initialRecipient: String = "",
        onSend: @escaping (String, String) -> Void,
        onClose: @escaping () -> Void
    ) {
        self._recipient = State(initialValue: initialRecipient)
        self.onSend = onSend
        self.onClose = onClose
    }

    private var suggestions: [ContactStore.Contact] {
        let query = recipient.trimmingCharacters(in: .whitespaces)
        guard !query.isEmpty else { return [] }
        let lowered = query.lowercased()
        return Array(
            contacts.contacts.filter { contact in
                contact.name.lowercased().contains(lowered)
                    || contact.phones.contains { $0.lowercased().contains(lowered) || $0.hasSuffix(lowered) }
            }.prefix(5)
        )
    }

    private var filteredContacts: [ContactStore.Contact] {
        let query = contactSearch.trimmingCharacters(in: .whitespaces)
        guard !query.isEmpty else { return contacts.contacts }
        let lowered = query.lowercased()
        return contacts.contacts.filter {
            $0.name.lowercased().contains(lowered)
                || $0.phones.contains { $0.contains(lowered) }
        }
    }

    private var canSend: Bool {
        !recipient.trimmingCharacters(in: .whitespaces).isEmpty
            && !content.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            && !sending
    }

    var body: some View {
        VStack(spacing: 0) {
            // 顶部导航：取消 / 新信息 / 发送（仿 iMessage）
            HStack(spacing: 12) {
                Button(L10n.t("取消")) {
                    onClose()
                }
                .buttonStyle(.plain)
                .font(.system(size: 14))
                .foregroundStyle(Color.accentColor)
                Spacer()
                Text(L10n.t("新信息"))
                    .font(.system(size: 14, weight: .semibold))
                Spacer()
                Button(L10n.t("发送")) {
                    Task { await sendNow() }
                }
                .buttonStyle(.plain)
                .font(.system(size: 14, weight: .semibold))
                .foregroundStyle(canSend ? Color.accentColor : Color.secondary)
                .disabled(!canSend)
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 12)

            Divider()

            // 收件人行：+ 通讯录 / 收件人 / 输入框
            HStack(spacing: 8) {
                Button {
                    showingContacts = true
                    contactSearch = ""
                } label: {
                    Image(systemName: "plus.circle.fill")
                        .font(.system(size: 15))
                        .foregroundStyle(Color.accentColor)
                }
                .buttonStyle(.plain)
                .help(L10n.t("从通讯录选择"))
                Text(L10n.t("收件人："))
                    .font(.system(size: 14))
                    .foregroundStyle(.secondary)
                    .layoutPriority(0)
                TextField(L10n.t("输入号码或姓名"), text: $recipient)
                    .textFieldStyle(.plain)
                    .font(.system(size: 14))
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 9)

            if !suggestions.isEmpty {
                Divider()
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 8) {
                        ForEach(suggestions) { contact in
                            Button {
                                recipient = contact.phones.first ?? contact.name
                            } label: {
                                HStack(spacing: 6) {
                                    ContactAvatarView(photoData: contact.photoData, name: contact.name, size: 20)
                                    Text(contact.name)
                                        .font(.footnote.weight(.medium))
                                        .lineLimit(1)
                                    Text(contact.phones.first ?? "")
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                                .padding(.horizontal, 10)
                                .padding(.vertical, 4)
                                .background(Capsule().fill(Color(nsColor: .controlBackgroundColor)))
                            }
                            .buttonStyle(.plain)
                        }
                    }
                    .padding(.horizontal, 16)
                    .padding(.vertical, 8)
                }
            }

            Divider()

            // 中间对话内容区：新信息时留空（仿 iMessage）
            Spacer(minLength: 0)

            if let errorText {
                Text(errorText)
                    .font(.caption)
                    .foregroundStyle(.red)
                    .padding(.horizontal, 16)
                    .padding(.bottom, 6)
            }

            Divider()

            // 底部输入栏：圆角输入框 + 蓝色圆形发送键（仿 iMessage）
            VStack(spacing: 4) {
                HStack(alignment: .bottom, spacing: 8) {
                    TextField(L10n.t("短信内容"), text: $content, axis: .vertical)
                        .textFieldStyle(.plain)
                        .font(.system(size: 14))
                        .lineLimit(1...4)
                        .padding(.horizontal, 12)
                        .padding(.vertical, 8)
                        .background(
                            RoundedRectangle(cornerRadius: 16, style: .continuous)
                                .fill(Color(nsColor: .textBackgroundColor))
                        )
                        .onSubmit {
                            Task { await sendNow() }
                        }
                    Button {
                        Task { await sendNow() }
                    } label: {
                        Image(systemName: "arrow.up")
                            .font(.system(size: 14, weight: .semibold))
                            .foregroundStyle(.white)
                            .frame(width: 30, height: 30)
                            .background(Circle().fill(canSend ? Color.accentColor : Color.secondary.opacity(0.45)))
                    }
                    .buttonStyle(.plain)
                    .disabled(!canSend)
                    .help(L10n.t("发送"))
                }
                Text(L10n.t("短信将通过 4G 模块发送"))
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 9)
        }
        .frame(width: 400, height: 440)
        .background(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(PhoneStyle.appBackground)
                .overlay(
                    RoundedRectangle(cornerRadius: 16, style: .continuous)
                        .strokeBorder(.white.opacity(0.4), lineWidth: 0.5)
                )
                .shadow(color: .black.opacity(0.28), radius: 24, x: 0, y: 10)
        )
        .clipShape(RoundedRectangle(cornerRadius: 16, style: .continuous))
        .padding(24)
        .overlay {
            if showingContacts {
                contactPicker
            }
        }
    }

    /// 通讯录选择器（仿 iMessage「+」弹层）：搜索 + 列表，选中后填入收件人
    private var contactPicker: some View {
        ZStack {
            Color.black.opacity(0.22)
                .ignoresSafeArea()
                .onTapGesture { closeContactPicker() }
            VStack(spacing: 0) {
                if let chooser = phoneChooser {
                    phoneChooserList(chooser)
                } else {
                    HStack(spacing: 8) {
                        Text(L10n.t("通讯录"))
                            .font(.system(size: 14, weight: .semibold))
                        Spacer()
                        if contacts.contacts.isEmpty && contacts.isAuthorized {
                            Button(L10n.t("刷新")) {
                                Task { await contacts.load() }
                            }
                            .buttonStyle(.plain)
                            .font(.system(size: 13))
                            .foregroundStyle(Color.accentColor)
                        }
                        Button {
                            closeContactPicker()
                        } label: {
                            Image(systemName: "xmark.circle.fill")
                                .font(.system(size: 16))
                                .foregroundStyle(.tertiary)
                        }
                        .buttonStyle(.plain)
                    }
                    .padding(.horizontal, 14)
                    .padding(.vertical, 10)
                    Divider()
                    HStack {
                        Spacer(minLength: 0)
                        ContactSearchField(placeholder: L10n.t("搜索姓名或号码"), text: $contactSearch)
                        Spacer(minLength: 0)
                    }
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                    Divider()
                    if !contacts.isAuthorized {
                        Spacer()
                        VStack(spacing: 8) {
                            Text(L10n.t("需要访问通讯录才能选择联系人"))
                                .font(.body)
                                .foregroundStyle(.secondary)
                            Button(L10n.t("授权访问通讯录")) {
                                Task { await contacts.requestAccess() }
                            }
                            .buttonStyle(.borderedProminent)
                            .controlSize(.regular)
                        }
                        Spacer()
                    } else if filteredContacts.isEmpty {
                        Spacer()
                        Text(contacts.contacts.isEmpty ? L10n.t("通讯录为空") : L10n.t("没有匹配的联系人"))
                            .font(.body)
                            .foregroundStyle(.secondary)
                        Spacer()
                    } else {
                        ScrollView {
                            LazyVStack(spacing: 0) {
                                ForEach(filteredContacts) { contact in
                                    Button {
                                        pickContact(contact)
                                    } label: {
                                        HStack(spacing: 10) {
                                            ContactAvatarView(photoData: contact.photoData, name: contact.name, size: 32)
                                            VStack(alignment: .leading, spacing: 1) {
                                                Text(contact.name)
                                                    .font(.system(size: 13, weight: .medium))
                                                Text(contact.phones.first ?? "")
                                                    .font(.caption)
                                                    .foregroundStyle(.secondary)
                                            }
                                            Spacer()
                                            if contact.phones.count > 1 {
                                                Image(systemName: "chevron.right")
                                                    .font(.system(size: 10, weight: .semibold))
                                                    .foregroundStyle(.tertiary)
                                            }
                                        }
                                        .padding(.horizontal, 14)
                                        .padding(.vertical, 7)
                                        .contentShape(Rectangle())
                                    }
                                    .buttonStyle(.plain)
                                    Divider().padding(.leading, 56)
                                }
                            }
                            .padding(.vertical, 4)
                        }
                    }
                }
            }
            .frame(width: 360, height: 400)
            .background(
                RoundedRectangle(cornerRadius: 14, style: .continuous)
                    .fill(PhoneStyle.appBackground)
                    .overlay(
                        RoundedRectangle(cornerRadius: 14, style: .continuous)
                            .strokeBorder(.white.opacity(0.35), lineWidth: 0.5)
                    )
                    .shadow(color: .black.opacity(0.25), radius: 20, x: 0, y: 8)
            )
            .clipShape(RoundedRectangle(cornerRadius: 14, style: .continuous))
        }
        .transition(.opacity)
    }

    private func phoneChooserList(_ contact: ContactStore.Contact) -> some View {
        VStack(spacing: 0) {
            HStack(spacing: 8) {
                Button {
                    phoneChooser = nil
                } label: {
                    Image(systemName: "chevron.left")
                        .font(.system(size: 12, weight: .semibold))
                }
                .buttonStyle(.plain)
                .foregroundStyle(Color.accentColor)
                Text(contact.name)
                    .font(.system(size: 14, weight: .semibold))
                    .lineLimit(1)
                Spacer()
                Button {
                    closeContactPicker()
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .font(.system(size: 16))
                        .foregroundStyle(.tertiary)
                }
                .buttonStyle(.plain)
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
            Divider()
            ScrollView {
                LazyVStack(spacing: 0) {
                    ForEach(Array(contact.phones.enumerated()), id: \.offset) { index, phone in
                        Button {
                            recipient = phone
                            closeContactPicker()
                        } label: {
                            HStack(spacing: 10) {
                                Image(systemName: "iphone")
                                    .font(.system(size: 13))
                                    .foregroundStyle(Color.accentColor)
                                    .frame(width: 32, height: 32)
                                    .background(Circle().fill(Color.accentColor.opacity(0.12)))
                                VStack(alignment: .leading, spacing: 1) {
                                    Text(phone)
                                        .font(.system(size: 13, weight: .medium))
                                    Text(L10n.t("手机"))
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                                Spacer()
                                Image(systemName: "plus.circle.fill")
                                    .font(.system(size: 15))
                                    .foregroundStyle(Color.accentColor)
                            }
                            .padding(.horizontal, 14)
                            .padding(.vertical, 8)
                            .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                        if index < contact.phones.count - 1 {
                            Divider().padding(.leading, 56)
                        }
                    }
                }
                .padding(.vertical, 4)
            }
        }
    }

    private func pickContact(_ contact: ContactStore.Contact) {
        if contact.phones.count == 1 {
            recipient = contact.phones[0]
            closeContactPicker()
        } else {
            phoneChooser = contact
        }
    }

    private func closeContactPicker() {
        showingContacts = false
        phoneChooser = nil
        contactSearch = ""
    }

    private func sendNow() async {
        let phone = recipient.trimmingCharacters(in: .whitespaces)
        let text = content.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !phone.isEmpty, !text.isEmpty else { return }
        sending = true
        defer { sending = false }
        do {
            _ = try await calls.apiClient.sendSMS(to: phone, message: text)
            onSend(phone, text)
        } catch {
            errorText = error.localizedDescription
        }
    }
}

private struct ConversationRow: View {
    @EnvironmentObject private var contacts: ContactStore
    let conversation: Conversation

    var body: some View {
        HStack(spacing: 10) {
            ContactAvatarView(
                photoData: contacts.contact(for: conversation.sender)?.photoData,
                name: contacts.displayName(for: conversation.sender),
                size: 38
            )
            VStack(alignment: .leading, spacing: 3) {
                Text(contacts.displayName(for: conversation.sender))
                    .font(.body.weight(.semibold))
                Text(conversation.lastContent)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
            }
            Spacer(minLength: 8)
            Text(smsShortTime(conversation.lastTime))
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .padding(.vertical, 2)
    }
}

private struct ThreadView: View {
    @EnvironmentObject private var contacts: ContactStore
    let sender: String
    let items: [ThreadItem]
    let onBack: () -> Void
    let onSend: (String, String) -> Void
    @State private var draft = ""

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                Button(action: onBack) {
                    Image(systemName: "chevron.left")
                        .font(.system(size: 14, weight: .semibold))
                }
                .buttonStyle(.plain)
                ContactAvatarView(
                    photoData: contacts.contact(for: sender)?.photoData,
                    name: contacts.displayName(for: sender),
                    size: 26
                )
                Text(contacts.displayName(for: sender))
                    .font(.system(size: 14, weight: .semibold))
                    .lineLimit(1)
                Spacer()
                Text(L10n.f("%@ 条", String(items.count)))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)

            Divider()

            ScrollView {
                LazyVStack(spacing: 7) {
                    ForEach(items) { item in
                        MessageBubble(item: item)
                    }
                }
                .padding(12)
            }

            Divider()

            HStack(spacing: 8) {
                TextField(L10n.t("回复短信…"), text: $draft, axis: .vertical)
                    .textFieldStyle(.plain)
                    .font(.body)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                    .background(
                        RoundedRectangle(cornerRadius: 12, style: .continuous)
                            .fill(Color(nsColor: .textBackgroundColor))
                    )
                    .lineLimit(1...4)
                    .onSubmit { send() }
                Button(action: send) {
                    Image(systemName: "arrow.up.circle.fill")
                        .font(.system(size: 25))
                        .foregroundStyle(
                            draft.trimmingCharacters(in: .whitespaces).isEmpty
                                ? Color.secondary : Color.accentColor
                        )
                }
                .buttonStyle(.plain)
                .disabled(draft.trimmingCharacters(in: .whitespaces).isEmpty)
            }
            .padding(10)
        }
    }

    private func send() {
        let content = draft
        draft = ""
        onSend(sender, content)
    }
}

/// iMessage 风格气泡：主体圆角矩形 + 底部小尾巴连成一个整体，
/// 尾巴一侧的圆角被替换为尖角，再由三角形尾巴自然衔接，不留缝隙。
private struct MessageBubbleShape: Shape {
    let sent: Bool

    func path(in rect: CGRect) -> Path {
        let radius: CGFloat = 18
        let tail: CGFloat = 12      // 尾巴沿边收拢的长度
        let tip: CGFloat = 4        // 尾巴尖端超出气泡主体的距离
        let w = rect.width
        let h = rect.height
        var path = Path()

        if sent {
            // 尾巴在右下角
            path.move(to: CGPoint(x: radius, y: 0))
            path.addLine(to: CGPoint(x: w - radius, y: 0))
            path.addArc(tangent1End: CGPoint(x: w, y: 0), tangent2End: CGPoint(x: w, y: radius), radius: radius)
            path.addLine(to: CGPoint(x: w, y: h - tail))
            path.addLine(to: CGPoint(x: w + tip, y: h + tip))       // 尾巴尖端
            path.addLine(to: CGPoint(x: w - tail, y: h))
            path.addLine(to: CGPoint(x: radius, y: h))
            path.addArc(tangent1End: CGPoint(x: 0, y: h), tangent2End: CGPoint(x: 0, y: h - radius), radius: radius)
            path.addLine(to: CGPoint(x: 0, y: radius))
            path.addArc(tangent1End: CGPoint(x: 0, y: 0), tangent2End: CGPoint(x: radius, y: 0), radius: radius)
        } else {
            // 尾巴在左下角（镜像）
            path.move(to: CGPoint(x: w - radius, y: 0))
            path.addLine(to: CGPoint(x: radius, y: 0))
            path.addArc(tangent1End: CGPoint(x: 0, y: 0), tangent2End: CGPoint(x: 0, y: radius), radius: radius)
            path.addLine(to: CGPoint(x: 0, y: h - tail))
            path.addLine(to: CGPoint(x: -tip, y: h + tip))          // 尾巴尖端
            path.addLine(to: CGPoint(x: tail, y: h))
            path.addLine(to: CGPoint(x: w - radius, y: h))
            path.addArc(tangent1End: CGPoint(x: w, y: h), tangent2End: CGPoint(x: w, y: h - radius), radius: radius)
            path.addLine(to: CGPoint(x: w, y: radius))
            path.addArc(tangent1End: CGPoint(x: w, y: 0), tangent2End: CGPoint(x: w - radius, y: 0), radius: radius)
        }
        path.closeSubpath()
        return path
    }
}

private struct MessageBubble: View {
    let item: ThreadItem

    var body: some View {
        HStack {
            if item.isSent { Spacer(minLength: 46) }
            VStack(alignment: item.isSent ? .trailing : .leading, spacing: 4) {
                Text(item.content)
                    .font(.body)
                    .textSelection(.enabled)
                if case let .received(message) = item, let code = message.code {
                    HStack(spacing: 8) {
                        Text(L10n.f("验证码 %@", code))
                            .font(.caption.weight(.semibold))
                        Button(L10n.t("复制")) {
                            NSPasteboard.general.clearContents()
                            NSPasteboard.general.setString(code, forType: .string)
                        }
                        .buttonStyle(.plain)
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(Color.accentColor)
                    }
                    .padding(.top, 1)
                }
                Text(smsShortTime(item.date))
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .background(
                MessageBubbleShape(sent: item.isSent)
                    .fill(item.isSent ? Color.accentColor : Color(nsColor: .controlBackgroundColor))
            )
            .foregroundStyle(item.isSent ? .white : .primary)
            if !item.isSent { Spacer(minLength: 46) }
        }
    }
}

private func smsShortTime(_ date: Date) -> String {
    let formatter = DateFormatter()
    let isEN = L10n.effectiveLanguage == .en
    formatter.locale = Locale(identifier: isEN ? "en_US" : "zh_CN")
    if Calendar.current.isDateInToday(date) {
        formatter.dateFormat = "HH:mm"
    } else if Calendar.current.isDate(date, equalTo: Date(), toGranularity: .year) {
        formatter.dateFormat = "M/d HH:mm"
    } else {
        formatter.dateFormat = "yy/M/d"
    }
    return formatter.string(from: date)
}

// MARK: - 通讯录

/// 通讯录搜索框：窄而高，带放大镜图标
private struct ContactSearchField: View {
    let placeholder: String
    @Binding var text: String

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: "magnifyingglass")
                .font(.system(size: 12, weight: .medium))
                .foregroundStyle(.secondary)
            TextField(placeholder, text: $text)
                .textFieldStyle(.plain)
                .font(.system(size: 13))
        }
        .padding(.horizontal, 9)
        .frame(width: 250, height: 34)
        .background(
            RoundedRectangle(cornerRadius: 9, style: .continuous)
                .fill(Color(nsColor: .textBackgroundColor))
        )
    }
}

/// 联系人头像：有照片显示照片，否则显示首字母圆形占位
struct ContactAvatarView: View {
    let photoData: Data?
    let name: String
    var size: CGFloat = 36

    var body: some View {
        if let photoData, let image = NSImage(data: photoData) {
            Image(nsImage: image)
                .resizable()
                .scaledToFill()
                .frame(width: size, height: size)
                .clipShape(Circle())
        } else {
            Circle()
                .fill(Color.accentColor.opacity(0.14))
                .frame(width: size, height: size)
                .overlay {
                    Text(String(name.prefix(1)).uppercased())
                        .font(.system(size: size * 0.38, weight: .semibold))
                        .foregroundStyle(Color.accentColor)
                }
        }
    }
}

private struct ContactsView: View {
    @EnvironmentObject private var calls: CallCenter
    @EnvironmentObject private var contacts: ContactStore
    let onComposeSMS: (String) -> Void
    @State private var search = ""
    @State private var selected: ContactStore.Contact?
    @State private var refreshing = false

    private var filtered: [ContactStore.Contact] {
        let query = search.trimmingCharacters(in: .whitespaces)
        guard !query.isEmpty else { return contacts.contacts }
        return contacts.contacts.filter {
            $0.name.localizedCaseInsensitiveContains(query)
                || $0.phones.contains { $0.contains(query) }
        }
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text(L10n.t("通讯录"))
                    .font(.title3.weight(.semibold))
                Spacer()
                if contacts.isAuthorized {
                    Text(L10n.f("%@ 位联系人", String(contacts.contacts.count)))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Button {
                    Task { await refresh() }
                } label: {
                    if refreshing {
                        ProgressView()
                            .controlSize(.small)
                    } else {
                        Image(systemName: "arrow.clockwise")
                            .font(.body.weight(.semibold))
                    }
                }
                .buttonStyle(.plain)
                .disabled(refreshing)
                .help(L10n.t("刷新同步本机通讯录"))
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)

            Text(L10n.t("刷新自动同步系统通讯录"))
                .font(.caption2)
                .foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 14)
                .padding(.bottom, 8)

            Divider()

            if let selected {
                ContactDetailView(contact: selected, onBack: { self.selected = nil }, onComposeSMS: onComposeSMS)
            } else if !contacts.isAuthorized {
                Spacer()
                VStack(spacing: 10) {
                    Image(systemName: "person.crop.circle.badge.questionmark")
                        .font(.system(size: 30))
                        .foregroundStyle(.secondary)
                    Text(L10n.t("需要访问通讯录才能显示联系人姓名"))
                        .font(.body)
                        .foregroundStyle(.secondary)
                    if let error = contacts.authError {
                        Text(error)
                            .font(.caption)
                            .foregroundStyle(.red)
                    }
                    Button(L10n.t("授权访问通讯录")) {
                        Task { await contacts.requestAccess() }
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.regular)
                }
                Spacer()
            } else {
                HStack {
                    Spacer(minLength: 0)
                    ContactSearchField(placeholder: L10n.t("搜索姓名或号码"), text: $search)
                    Spacer(minLength: 0)
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 8)
                if filtered.isEmpty {
                    Spacer()
                    Text(contacts.contacts.isEmpty ? L10n.t("通讯录为空") : L10n.t("没有匹配的联系人"))
                        .font(.body)
                        .foregroundStyle(.secondary)
                    Spacer()
                } else {
                    ScrollView {
                        LazyVStack(spacing: 8) {
                            ForEach(filtered) { contact in
                                Button {
                                    selected = contact
                                } label: {
                                    HStack(spacing: 10) {
                                        ContactAvatarView(photoData: contact.photoData, name: contact.name, size: 36)
                                        VStack(alignment: .leading, spacing: 2) {
                                            Text(contact.name)
                                                .font(.body.weight(.medium))
                                            Text(contact.phones.first ?? "")
                                                .font(.footnote)
                                                .foregroundStyle(.secondary)
                                        }
                                        Spacer()
                                        Image(systemName: "chevron.right")
                                            .font(.system(size: 12, weight: .semibold))
                                            .foregroundStyle(.tertiary)
                                    }
                                    .modifier(PhoneCard())
                                    .contentShape(Rectangle())
                                }
                                .buttonStyle(.plain)
                            }
                        }
                        .padding(12)
                    }
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func refresh() async {
        refreshing = true
        defer { refreshing = false }
        if !contacts.isAuthorized {
            await contacts.requestAccess()
        } else {
            await contacts.load()
        }
    }
}

// MARK: - 联系人详情

private struct ContactDetailView: View {
    @EnvironmentObject private var calls: CallCenter
    let contact: ContactStore.Contact
    let onBack: () -> Void
    let onComposeSMS: (String) -> Void

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 8) {
                Button(action: onBack) {
                    Image(systemName: "chevron.left")
                        .font(.body.weight(.semibold))
                    Text(L10n.t("通讯录"))
                        .font(.system(size: 14, weight: .medium))
                }
                .buttonStyle(.plain)
                .foregroundStyle(Color.accentColor)
                Spacer()
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)

            Divider()

            ScrollView {
                VStack(spacing: 16) {
                    VStack(spacing: 8) {
                        ContactAvatarView(photoData: contact.photoData, name: contact.name, size: 76)
                        Text(contact.name)
                            .font(.system(size: 18, weight: .semibold))
                    }
                    .padding(.top, 16)

                    VStack(spacing: 0) {
                        ForEach(Array(contact.phones.enumerated()), id: \.offset) { index, phone in
                            HStack(spacing: 12) {
                                Button {
                                    calls.dialNumber(phone)
                                } label: {
                                    Image(systemName: "phone.fill")
                                        .font(.system(size: 14, weight: .semibold))
                                        .foregroundStyle(.white)
                                        .frame(width: 34, height: 34)
                                        .background(Circle().fill(PhoneStyle.green))
                                }
                                .buttonStyle(.plain)
                                .help(L10n.f("呼叫 %@", phone))

                                VStack(alignment: .leading, spacing: 2) {
                                    Text(phone)
                                        .font(.system(size: 14, weight: .medium))
                                    Text(L10n.t("手机"))
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                                Spacer()

                                Button {
                                    onComposeSMS(phone)
                                } label: {
                                    Image(systemName: "message.fill")
                                        .font(.system(size: 14, weight: .semibold))
                                        .foregroundStyle(.white)
                                        .frame(width: 34, height: 34)
                                        .background(Circle().fill(Color.accentColor))
                                }
                                .buttonStyle(.plain)
                                .help(L10n.f("发短信给 %@", phone))
                            }
                            .padding(.horizontal, 14)
                            .padding(.vertical, 10)
                            if index < contact.phones.count - 1 {
                                Divider().padding(.leading, 60)
                            }
                        }
                    }
                    .modifier(PhoneCard())

                    if !contact.emails.isEmpty {
                        VStack(spacing: 0) {
                            ForEach(Array(contact.emails.enumerated()), id: \.offset) { index, email in
                                HStack(spacing: 12) {
                                    Image(systemName: "envelope.fill")
                                        .font(.system(size: 14, weight: .semibold))
                                        .foregroundStyle(.white)
                                        .frame(width: 34, height: 34)
                                        .background(Circle().fill(Color.accentColor))
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(email)
                                            .font(.system(size: 14, weight: .medium))
                                            .lineLimit(1)
                                        Text(L10n.t("邮箱"))
                                            .font(.caption)
                                            .foregroundStyle(.secondary)
                                    }
                                    Spacer()
                                }
                                .padding(.horizontal, 14)
                                .padding(.vertical, 10)
                                if index < contact.emails.count - 1 {
                                    Divider().padding(.leading, 60)
                                }
                            }
                        }
                        .modifier(PhoneCard())
                    }

                }
                .padding(14)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

/// 通用卡片统一行：左侧固定宽标题列，右侧内容列对齐
private struct GeneralRow<Content: View>: View {
    let title: String
    @ViewBuilder var content: Content

    var body: some View {
        HStack(spacing: 10) {
            Text(title)
                .font(.system(size: 12))
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .minimumScaleFactor(0.8)
                .frame(width: 80, alignment: .leading)
            Spacer(minLength: 8)
            content
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 8)
    }
}

// MARK: - 设置（铃声）

private struct SettingsView: View {
    @EnvironmentObject private var ringtones: RingtoneStore
    @EnvironmentObject private var settings: AppSettings
    @State private var showQuitConfirm = false

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text(L10n.t("设置"))
                    .font(.title3.weight(.semibold))
                Spacer()
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)

            Divider()

            ScrollView {
                VStack(alignment: .leading, spacing: 10) {
                    MoreSectionTitle(L10n.t("状态"))
                    DeviceStatusCard()

                    MoreSectionTitle("通话支持")
                    ModuleSetupCard()

                    MoreSectionTitle("语音运行时")
                    VoiceRuntimeCard()

                    MoreSectionTitle(L10n.t("通用"))
                    VStack(spacing: 0) {
                        GeneralRow(title: L10n.t("外观")) {
                            Picker("", selection: $settings.appearance) {
                                ForEach(AppAppearance.allCases) { mode in
                                    Text(mode.displayName).tag(mode)
                                }
                            }
                            .pickerStyle(.segmented)
                            .labelsHidden()
                            .frame(width: 190)
                        }
                        Divider().padding(.leading, 14)
                        GeneralRow(title: L10n.t("语言")) {
                            Picker("", selection: $settings.language) {
                                ForEach(AppLanguage.allCases) { lang in
                                    Text(lang.displayName).tag(lang)
                                }
                            }
                            .pickerStyle(.segmented)
                            .labelsHidden()
                            .frame(width: 190)
                        }
                        Divider().padding(.leading, 14)
                        RingtoneDropdown()
                    }
                    .modifier(PhoneCard())

                    MoreView()

                    MoreSectionTitle(L10n.t("服务控制"))
                    VStack(spacing: 0) {
                        HStack(spacing: 10) {
                            Button(role: .destructive) {
                                showQuitConfirm = true
                            } label: {
                                Label(L10n.t("完全退出"), systemImage: "power")
                            }
                            .buttonStyle(.bordered)
                            .tint(.red)
                            .controlSize(.regular)
                            Text(L10n.t("停止 4G 后台与 DJOneHub 应用"))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                            Spacer()
                        }
                        .padding(.horizontal, 14)
                        .padding(.vertical, 10)
                    }
                    .modifier(PhoneCard())
                }
                .padding(14)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .alert(L10n.t("确认完全退出？"), isPresented: $showQuitConfirm) {
            Button(L10n.t("退出"), role: .destructive) {
                NSApp.terminate(nil)
            }
            Button(L10n.t("取消"), role: .cancel) {}
        } message: {
            Text(L10n.t("将停止 DJOneHub 后台进程与本应用程序。"))
        }
    }
}

private struct ModuleSetupCard: View {
    @State private var status: ModuleSetupStatus?
    @State private var isLoading = false
    @State private var showConfirm = false
    @State private var errorText: String?
    private let api = DJOneHubAPI(baseURL: URL(string: "http://127.0.0.1:7575/")!)

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .top, spacing: 10) {
                Image(systemName: icon)
                    .foregroundStyle(tint)
                    .font(.system(size: 16, weight: .semibold))
                    .frame(width: 22)
                VStack(alignment: .leading, spacing: 3) {
                    Text(status?.summary ?? "正在检查模块状态")
                        .font(.footnote.weight(.semibold))
                    Text(detail)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                Spacer(minLength: 0)
                if isLoading {
                    ProgressView().controlSize(.small)
                } else {
                    Button {
                        refresh()
                    } label: {
                        Image(systemName: "arrow.clockwise")
                    }
                    .buttonStyle(.plain)
                    .foregroundStyle(.secondary)
                }
            }
            if status?.canInitialize == true {
                Button {
                    showConfirm = true
                } label: {
                    Label("启用通话支持", systemImage: "wand.and.stars")
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.small)
                .disabled(isLoading)
            }
            if let backup = status?.backupPath, !backup.isEmpty {
                Text("已备份原始模块配置")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
            if let errorText {
                Text(errorText)
                    .font(.caption)
                    .foregroundStyle(.red)
            }
        }
        .padding(12)
        .modifier(PhoneCard())
        .task { refresh() }
        .alert("启用通话支持？", isPresented: $showConfirm) {
            Button("启用", role: .destructive) { initialize() }
            Button("取消", role: .cancel) {}
        } message: {
            Text("兼容原始模块、旧 UAC 配置及其他工具留下的完整 USB 配置。将先备份当前配置，再补齐通话所需接口并重启模块；过程中 4G 会短暂断开。验证失败会自动恢复原始配置，并显示具体原因。")
        }
    }

    private var icon: String {
        switch status?.state {
        case "ready": return "checkmark.circle.fill"
        case "needs_initialization": return "sparkles"
        case "initializing", "restarting", "verifying", "reconnecting": return "arrow.triangle.2.circlepath"
        case "failed", "unsupported": return "exclamationmark.triangle.fill"
        default: return "antenna.radiowaves.left.and.right"
        }
    }

    private var tint: Color {
        switch status?.state {
        case "ready": return .green
        case "failed", "unsupported": return .orange
        default: return .accentColor
        }
    }

    private var detail: String {
        if let detail = status?.detail, !detail.isEmpty { return detail }
        return "新模块只需初始化一次；之后直接插入即可使用。"
    }

    private func refresh() {
        guard !isLoading else { return }
        isLoading = true
        errorText = nil
        Task {
            do {
                let next = try await api.moduleSetupStatus()
                await MainActor.run {
                    status = next
                    isLoading = next.state == "reconnecting"
                }
                if next.state == "reconnecting" {
                    await waitForModuleSetupCompletion()
                }
            } catch {
                await MainActor.run { errorText = error.localizedDescription; isLoading = false }
            }
        }
    }

    private func initialize() {
        isLoading = true
        errorText = nil
        Task {
            do {
                let next = try await api.initializeModule()
                await MainActor.run { status = next }
            } catch {
                // A module reboot can close the local request while the
                // accepted setup continues. Inspect its state before calling
                // that a failure, so the UI never encourages a duplicate
                // initialization write.
                let recovered = try? await api.moduleSetupStatus()
                if let recovered, isModuleSetupPending(recovered.state) {
                    await MainActor.run { status = recovered }
                } else {
                    await MainActor.run {
                        status = recovered
                        errorText = error.localizedDescription
                        isLoading = false
                    }
                    return
                }
            }
            await waitForModuleSetupCompletion()
        }
    }

    private func waitForModuleSetupCompletion() async {
        // The module restart normally completes within a minute. During that
        // interval keep the action disabled and render the server state rather
        // than a client-side timeout.
        for _ in 0..<50 {
            try? await Task.sleep(for: .seconds(2))
            guard let next = try? await api.moduleSetupStatus() else { continue }
            await MainActor.run { status = next }
            if !isModuleSetupPending(next.state) {
                await MainActor.run { isLoading = false }
                return
            }
        }
        await MainActor.run {
            isLoading = false
            errorText = "模块仍在重新连接，请点击刷新查看状态。"
        }
    }

    private func isModuleSetupPending(_ state: String) -> Bool {
        ["initializing", "restarting", "verifying", "reconnecting"].contains(state)
    }
}

private struct VoiceRuntimeCard: View {
    @State private var status: VoiceRuntimeStatus?
    @State private var isLoading = false
    @State private var showConfirm = false
    @State private var errorText: String?
    private let api = DJOneHubAPI(baseURL: URL(string: "http://127.0.0.1:7575/")!)

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .top, spacing: 10) {
                Image(systemName: status?.runtimeInstalled == true ? "waveform.circle.fill" : "arrow.down.circle")
                    .foregroundStyle(status?.runtimeInstalled == true ? .green : .accentColor)
                    .font(.system(size: 16, weight: .semibold))
                    .frame(width: 22)
                VStack(alignment: .leading, spacing: 3) {
                    Text(status?.runtimeInstalled == true ? "语音运行时已就绪" : "尚未安装语音运行时")
                        .font(.footnote.weight(.semibold))
                    Text(status?.runtimeDetail ?? "首次确认后从上游固定版本下载并校验，下载可能需要数分钟；模块重启后将从本机缓存自动恢复。")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                Spacer(minLength: 0)
                if isLoading {
                    ProgressView().controlSize(.small)
                } else {
                    Button { refresh() } label: { Image(systemName: "arrow.clockwise") }
                        .buttonStyle(.plain)
                        .foregroundStyle(.secondary)
                }
            }
            if status?.runtimeInstalled != true {
                Button { showConfirm = true } label: {
                    Label("确认并启用通话", systemImage: "phone.badge.checkmark")
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.small)
                .disabled(isLoading)
            }
            if let errorText {
                Text(errorText)
                    .font(.caption)
                    .foregroundStyle(.red)
            }
        }
        .padding(12)
        .modifier(PhoneCard())
        .task { refresh() }
        .alert("下载并启用语音运行时？", isPresented: $showConfirm) {
            Button("确认并启用", role: .destructive) { provision() }
            Button("取消", role: .cancel) {}
        } message: {
            Text("将从 MaVo 官方固定版本直接下载模块侧语音运行时，校验 SHA-256 后保存到本机。DJOneHub 不随 App 分发该运行时。首次通话时会临时部署到当前模块；不刷写固件。")
        }
    }

    private func refresh() {
        guard !isLoading else { return }
        isLoading = true
        errorText = nil
        Task {
            do {
                let next = try await api.voiceRuntimeStatus()
                await MainActor.run { status = next; isLoading = false }
            } catch {
                await MainActor.run { errorText = error.localizedDescription; isLoading = false }
            }
        }
    }

    private func provision() {
        isLoading = true
        errorText = nil
        Task {
            do {
                let next = try await api.provisionVoiceRuntime()
                await MainActor.run { status = next; isLoading = false }
            } catch {
                let message: String
                if (error as? URLError)?.code == .timedOut {
                    message = "语音运行时下载超过 3 分钟未完成，请检查网络后重试。"
                } else {
                    message = error.localizedDescription
                }
                await MainActor.run { errorText = message; isLoading = false }
            }
        }
    }
}

private struct RingtoneDropdown: View {
    @EnvironmentObject private var ringtones: RingtoneStore
    @State private var expanded = false

    var body: some View {
        VStack(spacing: 0) {
            Button {
                withAnimation(.easeInOut(duration: 0.18)) { expanded.toggle() }
            } label: {
                HStack(spacing: 10) {
                    Text(L10n.t("来电铃声"))
                        .font(.system(size: 12))
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .minimumScaleFactor(0.8)
                        .frame(width: 80, alignment: .leading)
                    Spacer(minLength: 8)
                    Text(ringtones.selected?.name ?? L10n.t("默认"))
                        .font(.system(size: 12))
                        .foregroundStyle(.primary)
                        .lineLimit(1)
                    Image(systemName: expanded ? "chevron.up" : "chevron.down")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(.tertiary)
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 8)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)

            if expanded {
                Divider().padding(.leading, 40)
                ForEach(ringtones.all) { ringtone in
                    RingtoneRow(ringtone: ringtone)
                        .padding(.horizontal, 14)
                    Divider().padding(.leading, 40)
                }
                HStack(spacing: 8) {
                    Button {
                        ringtones.pickAndAdd()
                    } label: {
                        Label(L10n.t("添加铃声"), systemImage: "plus.circle")
                            .font(.footnote.weight(.medium))
                    }
                    .buttonStyle(.bordered)
                    .controlSize(.small)
                    Button {
                        ringtones.revealFolder()
                    } label: {
                        Label(L10n.t("打开铃声文件夹"), systemImage: "folder")
                            .font(.footnote.weight(.medium))
                    }
                    .buttonStyle(.bordered)
                    .controlSize(.small)
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
                Text(L10n.t("支持 aiff / wav / mp3 / m4a，放入铃声文件夹后自动出现"))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 14)
                    .padding(.bottom, 10)
            }
        }
    }
}

private struct RingtoneRow: View {
    @EnvironmentObject private var ringtones: RingtoneStore
    let ringtone: Ringtone

    var body: some View {
        HStack(spacing: 10) {
            Button {
                ringtones.select(ringtone)
            } label: {
                HStack(spacing: 8) {
                    Image(systemName: ringtones.selectedID == ringtone.id ? "checkmark.circle.fill" : "circle")
                        .font(.system(size: 14))
                        .foregroundStyle(ringtones.selectedID == ringtone.id ? Color.accentColor : Color.secondary)
                    Text(ringtone.name)
                        .font(.system(size: 12))
                        .foregroundStyle(.primary)
                    Spacer()
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            Button {
                ringtones.preview(ringtone)
            } label: {
                Image(systemName: "speaker.wave.2.fill")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
            .buttonStyle(.plain)
            .help(L10n.t("试听"))
        }
        .padding(.vertical, 3)
    }
}

// MARK: - 通话录音（设置页）

private struct RecordingFile: Identifiable {
    let id: String
    let url: URL
    let size: Int64
    let date: Date
}

private struct RecordingSection: View {
    @State private var recordings: [RecordingFile] = []
    @State private var playingID: String?
    @State private var sound: NSSound?

    private static var directory: URL {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first!
        return base.appendingPathComponent("DJOneHub/recordings", isDirectory: true)
    }

    private var totalSize: String {
        let bytes = recordings.reduce(Int64(0)) { $0 + $1.size }
        if bytes < 1_048_576 { return "\(bytes / 1024) KB" }
        return String(format: "%.1f MB", Double(bytes) / 1_048_576)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text(L10n.t("通话录音"))
                    .font(.body.weight(.semibold))
                    .foregroundStyle(.secondary)
                Spacer()
                Text(L10n.f("%@ 个 · %@", String(recordings.count), totalSize))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if recordings.isEmpty {
                Text(L10n.t("暂无录音。通话中点击「录音」按钮开始。"))
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            } else {
                ForEach(recordings) { file in
                    HStack(spacing: 8) {
                        Button {
                            togglePlay(file)
                        } label: {
                            Image(systemName: playingID == file.id ? "stop.fill" : "play.fill")
                                .font(.caption.weight(.semibold))
                                .frame(width: 22)
                                .foregroundStyle(Color.accentColor)
                        }
                        .buttonStyle(.plain)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(file.url.deletingPathExtension().lastPathComponent)
                                .font(.footnote)
                                .lineLimit(1)
                            Text(RecordingSection.fileMeta(file))
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                        }
                        Spacer()
                        Button {
                            NSWorkspace.shared.activateFileViewerSelecting([file.url])
                        } label: {
                            Image(systemName: "folder")
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        .buttonStyle(.plain)
                        .help(L10n.t("在 Finder 中显示"))
                    }
                    .padding(.vertical, 2)
                }
            }
            HStack(spacing: 8) {
                Button {
                    refresh()
                } label: {
                    Label(L10n.t("刷新"), systemImage: "arrow.clockwise")
                        .font(.footnote.weight(.medium))
                }
                .buttonStyle(.bordered)
                .controlSize(.small)
                Button {
                    let dir = Self.directory
                    try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
                    NSWorkspace.shared.open(dir)
                } label: {
                    Label(L10n.t("打开录音文件夹"), systemImage: "folder")
                        .font(.footnote.weight(.medium))
                }
                .buttonStyle(.bordered)
                .controlSize(.small)
            }
        }
        .onAppear(perform: refresh)
    }

    private func refresh() {
        if CommandLine.arguments.contains("--review-safe") {
            recordings = []
            return
        }
        let fm = FileManager.default
        let dir = Self.directory
        try? fm.createDirectory(at: dir, withIntermediateDirectories: true)
        let files = (try? fm.contentsOfDirectory(
            at: dir,
            includingPropertiesForKeys: [.fileSizeKey, .creationDateKey],
            options: [.skipsHiddenFiles]
        )) ?? []
        recordings = files
            .filter { $0.pathExtension.lowercased() == "wav" }
            .compactMap { url -> RecordingFile? in
                let values = try? url.resourceValues(forKeys: [.fileSizeKey, .creationDateKey])
                return RecordingFile(
                    id: url.lastPathComponent,
                    url: url,
                    size: Int64(values?.fileSize ?? 0),
                    date: values?.creationDate ?? .distantPast
                )
            }
            .sorted { $0.date > $1.date }
    }

    private func togglePlay(_ file: RecordingFile) {
        if playingID == file.id {
            sound?.stop()
            sound = nil
            playingID = nil
            return
        }
        sound?.stop()
        let newSound = NSSound(contentsOf: file.url, byReference: true)
        newSound?.play()
        sound = newSound
        playingID = file.id
    }

    private static func fileMeta(_ file: RecordingFile) -> String {
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyy-MM-dd HH:mm"
        let size: String
        if file.size < 1_048_576 {
            size = "\(file.size / 1024) KB"
        } else {
            size = String(format: "%.1f MB", Double(file.size) / 1_048_576)
        }
        return "\(formatter.string(from: file.date)) · \(size)"
    }
}

// MARK: - 通话界面

struct CallActiveView: View {
    @EnvironmentObject private var calls: CallCenter
    @EnvironmentObject private var contacts: ContactStore
    let call: CallRecord
    @State private var now = Date()
    @State private var showKeypad = false

    private var ringing: Bool {
        call.state == "incoming" || call.state == "waiting"
    }

    private var inCall: Bool {
        ["active", "dialing", "alerting", "held"].contains(call.state)
    }

    private var statusText: String {
        switch call.state {
        case "incoming": return L10n.t("来电…")
        case "waiting": return L10n.t("等待接听…")
        case "active": return L10n.t("通话中")
        case "dialing": return L10n.t("正在拨号…")
        case "alerting": return L10n.t("等待对方接听…")
        case "held": return L10n.t("通话保持")
        default: return L10n.t("通话状态")
        }
    }

    private var durationText: String {
        let seconds = Int(calls.callDuration(now: now))
        let hours = seconds / 3600
        let minutes = (seconds % 3600) / 60
        let secs = seconds % 60
        if hours > 0 {
            return String(format: "%d:%02d:%02d", hours, minutes, secs)
        }
        return String(format: "%02d:%02d", minutes, secs)
    }

    var body: some View {
        VStack(spacing: 18) {
            Spacer()
            ZStack {
                Circle()
                    .fill(Color.primary.opacity(0.08))
                    .frame(width: 96, height: 96)
                Image(systemName: "person.fill")
                    .font(.system(size: 40))
                    .foregroundStyle(.primary.opacity(0.85))
            }
            Text(contacts.displayName(for: call.number))
                .font(.system(size: 30, weight: .semibold))
                .lineLimit(1)
                .minimumScaleFactor(0.6)
                .foregroundStyle(.primary)
            Text(statusText)
                .font(.system(size: 14))
                .foregroundStyle(.secondary)
            if inCall {
                Text(durationText)
                    .font(.system(size: 42, weight: .light, design: .rounded))
                    .monospacedDigit()
                    .foregroundStyle(.primary)
                    .padding(.top, 6)
            }
            Spacer()

            if !ringing && showKeypad {
                dtmfKeypad
                    .padding(.bottom, 6)
            }

            if ringing {
                HStack(spacing: 44) {
                    CallRoundButton(icon: "phone.down.fill", tint: .red, label: L10n.t("拒接"), filled: true) {
                        calls.reject()
                    }
                    CallRoundButton(icon: "phone.fill", tint: PhoneStyle.green, label: L10n.t("接听"), filled: true) {
                        calls.answer()
                    }
                }
            } else {
                HStack(spacing: 12) {
                    CallRoundButton(
                        icon: calls.isMuted ? "mic.slash.fill" : "mic.fill",
                        tint: calls.isMuted ? PhoneStyle.green : Color.primary,
                        label: calls.isMuted ? L10n.t("取消静音") : L10n.t("静音")
                    ) {
                        calls.toggleMute()
                    }
                    CallRoundButton(
                        icon: calls.isRecording ? "stop.circle.fill" : "record.circle",
                        tint: calls.isRecording ? Color.red : Color.primary,
                        label: calls.isRecording ? L10n.t("停止录音") : L10n.t("录音")
                    ) {
                        calls.toggleRecording()
                    }
                    CallRoundButton(icon: "phone.down.fill", tint: .red, label: L10n.t("挂断"), filled: true) {
                        calls.hangup()
                    }
                    CallRoundButton(icon: "number", tint: showKeypad ? PhoneStyle.green : Color.primary, label: L10n.t("键盘")) {
                        showKeypad.toggle()
                    }
                }
            }
        }
        .padding(26)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        // 通话界面跟随系统外观（浅色/深色），不再硬编码深色
        .background(PhoneStyle.appBackground)
        .onReceive(Timer.publish(every: 1, on: .main, in: .common).autoconnect()) { _ in
            now = Date()
        }
    }

    /// 通话中 DTMF 键盘：点按数字直接发送双音多频到运营商线路
    private var dtmfKeypad: some View {
        VStack(spacing: 10) {
            ForEach(["123", "456", "789", "*0#"], id: \.self) { row in
                HStack(spacing: 26) {
                    ForEach(Array(row), id: \.self) { ch in
                        Button {
                            calls.sendDTMF(String(ch))
                        } label: {
                            Text(String(ch))
                                .font(.system(size: 26, weight: .medium, design: .rounded))
                                .foregroundStyle(.primary)
                                .frame(width: 54, height: 54)
                                .background(Circle().fill(Color.primary.opacity(0.08)))
                                .contentShape(Circle())
                        }
                        .buttonStyle(.plain)
                    }
                }
            }
        }
    }
}

private struct CallRoundButton: View {
    let icon: String
    let tint: Color
    let label: String
    var filled = false
    let action: () -> Void

    var body: some View {
        VStack(spacing: 6) {
            Button(action: action) {
                ZStack {
                    Circle()
                        .fill(filled ? tint : tint.opacity(0.18))
                        .frame(width: 54, height: 54)
                    Image(systemName: icon)
                        .font(.system(size: 20, weight: .semibold))
                        .foregroundStyle(filled ? Color.white : tint)
                }
            }
            .buttonStyle(.plain)
            Text(label)
                .font(.caption)
                .foregroundStyle(.primary.opacity(0.85))
        }
    }
}
