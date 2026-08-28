import SwiftUI
import AppKit

// MARK: - 模块（状态 / 网络 / 定位 / eSIM / AT 调试）——已并入「设置」

struct MoreView: View {
    @EnvironmentObject private var calls: CallCenter
    @EnvironmentObject private var settings: AppSettings

    @State private var modem: ModemStatus?
    @State private var traffic: NetworkTrafficSnapshot?
    @State private var policy: CellularPolicyStatus?
    @State private var usbProfile: USBProfileStatus?
    @State private var esim: ESIMOverview?
    @State private var esimHealth: ESIMHealth?
    @State private var esimNotes: [String: ESIMNote] = [:]
    @State private var probeResult: ESIMPhonebookProbe?
    @State private var probeBusy = false
    @State private var renameProfile: ESIMProfile?
    @State private var editNoteProfile: ESIMProfile?
    @State private var deleteProfile: ESIMProfile?
    @State private var showDownload = false
    @State private var networkDiag: NetworkDiagnostic?
    @State private var networkDiagExpanded = false
    @State private var networkDiagBusy = false
    @State private var atCommand = "AT+CSQ"
    @State private var atResponse = ""
    @State private var atBusy = false
    @State private var message = ""
    @State private var busy = false
    @State private var showMobileProfileConfirm = false
    @State private var showMacProfileConfirm = false

    private var profiles: [ESIMProfile] {
        esim?.profiles?.flatMap { $0.profiles ?? [] } ?? []
    }

    private var eidRows: [ESIMEID] {
        esim?.chipInfo?.eids ?? []
    }

    private var signalText: String {
        guard let dbm = modem?.signalDBM else { return "--" }
        return "\(dbm) dBm"
    }

    private var trafficText: String {
        guard let traffic, traffic.available else { return "--" }
        let bytes = Double(traffic.sessionTotal)
        if bytes >= 1_073_741_824 {
            return String(format: "%.1f GB", bytes / 1_073_741_824)
        }
        if bytes >= 1_048_576 {
            return String(format: "%.1f MB", bytes / 1_048_576)
        }
        return "\(Int(bytes / 1024)) KB"
    }

    private var esimCardType: String {
        if let message = esim?.message { return message }
        if let name = esim?.chipInfo?.skuName { return name }
        return L10n.t("查询中…")
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if !message.isEmpty {
                Text(message)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .padding(.horizontal, 14)
            }

            MoreSectionTitle(L10n.t("网络"))
            VStack(spacing: 0) {
                SettingToggleRow(
                    title: L10n.t("允许 4G 上网"),
                    subtitle: L10n.t("关闭后强制禁止 Mac 使用 4G；短信和来电监控不受影响"),
                    isOn: cellularBinding
                )
                Divider().padding(.leading, 14)
                GPSPanel()
                Divider().padding(.leading, 14)
                HStack(spacing: 8) {
                    MoreActionButton(title: L10n.t("检查 4G 出口")) {
                        do {
                            runCheck(try await calls.apiClient.check4GRoute())
                        } catch {
                            message = error.localizedDescription
                        }
                    }
                    MoreActionButton(title: L10n.t("检查代理出口")) {
                        do {
                            runCheck(try await calls.apiClient.checkProxyRoute())
                        } catch {
                            message = error.localizedDescription
                        }
                    }
                    MoreActionButton(title: L10n.t("重启模块"), destructive: true) {
                        await reboot()
                    }
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
                Divider().padding(.leading, 14)
                Button {
                    withAnimation(.easeInOut(duration: 0.18)) {
                        networkDiagExpanded.toggle()
                        if networkDiagExpanded && networkDiag == nil {
                            Task { await loadNetworkDiagnostic() }
                        }
                    }
                } label: {
                    HStack(spacing: 10) {
                        Image(systemName: "network")
                            .font(.body)
                            .foregroundStyle(Color.accentColor)
                        Text(L10n.t("网络端口诊断"))
                            .font(.system(size: 12, weight: .medium))
                        Spacer()
                        if networkDiagBusy {
                            ProgressView().controlSize(.small)
                        }
                        Image(systemName: networkDiagExpanded ? "chevron.up" : "chevron.down")
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.tertiary)
                    }
                    .padding(.horizontal, 14)
                    .padding(.vertical, 11)
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                if networkDiagExpanded {
                    Divider().padding(.leading, 14)
                    if let diag = networkDiag {
                        diagnosticRows(diag)
                    } else if networkDiagBusy {
                        HStack(spacing: 8) {
                            ProgressView().controlSize(.small)
                            Text(L10n.t("正在读取网络诊断…"))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                        .padding(.horizontal, 14)
                        .padding(.vertical, 10)
                    }
                }
            }
            .modifier(PhoneCard())

            MoreSectionTitle("系统")
            VStack(spacing: 0) {
                AutoLaunchToggleRow()
            }
            .modifier(PhoneCard())

            MoreSectionTitle("连接模式")
            VStack(spacing: 0) {
                HStack(spacing: 10) {
                    Image(systemName: usbProfile?.mode == "mobile" ? "iphone.gen3" : "macbook")
                        .font(.system(size: 15, weight: .medium))
                        .foregroundStyle(usbProfile?.mode == "mobile" ? Color.accentColor : .secondary)
                        .frame(width: 20)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(usbProfile?.mode == "mobile" ? "iPhone / iPad 模式" : "Mac 完整模式")
                            .font(.system(size: 13, weight: .medium))
                        Text(usbProfile?.mode == "mobile" ? "已关闭 USB 音频；拔插到移动设备后仅提供上网与短信" : "上网、短信与通话音频均可用")
                            .font(.system(size: 10))
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    Spacer(minLength: 4)
                    Button(usbProfile?.mode == "mobile" ? "恢复 Mac" : "iPhone / iPad") {
                        if usbProfile?.mode == "mobile" { showMacProfileConfirm = true }
                        else { showMobileProfileConfirm = true }
                    }
                    .buttonStyle(.bordered)
                    .controlSize(.small)
                    .disabled(busy || usbProfile == nil)
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
            }
            .modifier(PhoneCard())

            MoreSectionTitle(L10n.t("eSIM / 卡片"))
            VStack(spacing: 0) {
                MoreInfoRow(label: L10n.t("卡片类型"), value: esimCardType)
                if let firmware = esim?.chipInfo?.firmware, !firmware.isEmpty {
                    Divider().padding(.leading, 12)
                    MoreInfoRow(label: L10n.t("固件"), value: firmware)
                }
                if let serial = esim?.chipInfo?.serialNumber, !serial.isEmpty {
                    Divider().padding(.leading, 12)
                    MoreInfoRow(label: L10n.t("序列号"), value: serial)
                }
                ForEach(eidRows) { eid in
                    Divider().padding(.leading, 12)
                    VStack(alignment: .leading, spacing: 2) {
                        HStack(spacing: 8) {
                            Text(L10n.t("EID"))
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                            Spacer()
                            Text(eid.eid ?? "--")
                                .font(.footnote.weight(.medium))
                                .lineLimit(1)
                                .minimumScaleFactor(0.6)
                        }
                        if let detail = eidDetail(eid) {
                            Text(detail)
                                .font(.caption2)
                                .foregroundStyle(.tertiary)
                                .frame(maxWidth: .infinity, alignment: .trailing)
                        }
                    }
                    .padding(.horizontal, 14)
                    .padding(.vertical, 6)
                }
                if let health = esimHealth {
                    if let message = health.message, health.activeProfile == nil {
                        Divider().padding(.leading, 12)
                        Text(message)
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                            .padding(.horizontal, 14)
                            .padding(.vertical, 8)
                    } else if health.activeProfile != nil || health.moduleICCID != nil {
                        Divider().padding(.leading, 12)
                        MoreInfoRow(label: L10n.t("当前启用"), value: health.activeProfile?.displayName ?? "--")
                    Divider().padding(.leading, 12)
                    MoreInfoRow(
                        label: L10n.t("模块实际卡"),
                        value: Self.maskIdentifier(health.moduleICCID) ?? "--",
                        detail: health.imsi.map { "IMSI \($0)" }
                    )
                    Divider().padding(.leading, 12)
                    MoreInfoRow(
                        label: L10n.t("蜂窝注册"),
                        value: {
                            let parts = [OperatorName.display(health.operatorName), health.networkMode, health.registration]
                                .compactMap { $0 }.filter { !$0.isEmpty }
                            return parts.isEmpty ? "--" : parts.joined(separator: " · ")
                        }(),
                        detail: health.registered == true ? nil : L10n.t("等待网络注册")
                    )
                        Divider().padding(.leading, 12)
                        MoreInfoRow(
                            label: L10n.t("信号"),
                            value: health.signalDBM.map { "\($0) dBm" } ?? "--",
                            detail: health.registered == true ? L10n.t("模块已接管当前 Profile") : nil
                        )
                    }
                }
                if !profiles.isEmpty {
                    ForEach(profiles) { profile in
                        Divider().padding(.leading, 12)
                        VStack(alignment: .leading, spacing: 6) {
                            HStack(spacing: 8) {
                                VStack(alignment: .leading, spacing: 1) {
                                    Text(profile.name ?? L10n.t("未命名 Profile"))
                                        .font(.footnote.weight(.medium))
                                    Text(profile.iccid ?? "")
                                        .font(.caption2)
                                        .foregroundStyle(.secondary)
                                        .lineLimit(1)
                                }
                                Spacer()
                                if profile.enabled {
                                    Text(L10n.t("使用中"))
                                        .font(.caption2.weight(.semibold))
                                        .foregroundStyle(Color.accentColor)
                                }
                            }
                            HStack(spacing: 8) {
                                if !profile.enabled {
                                    Button {
                                        Task { await switchProfile(profile) }
                                    } label: {
                                        Text(L10n.t("切换"))
                                            .lineLimit(1)
                                            .minimumScaleFactor(0.7)
                                    }
                                    .buttonStyle(.bordered)
                                    .controlSize(.small)
                                    .disabled(busy)
                                }
                                Button {
                                    renameProfile = profile
                                } label: {
                                    Text(L10n.t("重命名"))
                                        .lineLimit(1)
                                        .minimumScaleFactor(0.7)
                                }
                                .buttonStyle(.bordered)
                                .controlSize(.small)
                                Button {
                                    editNoteProfile = profile
                                } label: {
                                    Text(L10n.t("模块资料"))
                                        .lineLimit(1)
                                        .minimumScaleFactor(0.7)
                                }
                                .buttonStyle(.bordered)
                                .controlSize(.small)
                                Button(role: .destructive) {
                                    deleteProfile = profile
                                } label: {
                                    Text(L10n.t("删除"))
                                        .lineLimit(1)
                                        .minimumScaleFactor(0.7)
                                }
                                .buttonStyle(.bordered)
                                .controlSize(.small)
                                .disabled(profile.enabled)
                                Spacer()
                            }
                        }
                        .padding(.horizontal, 14)
                        .padding(.vertical, 6)
                    }
                }
                Divider().padding(.leading, 14)
                VStack(alignment: .leading, spacing: 6) {
                    HStack(spacing: 8) {
                        MoreActionButton(title: L10n.t("通讯录检测")) {
                            await probePhonebook()
                        }
                        .frame(maxWidth: 130)
                        MoreActionButton(title: L10n.t("下载新 Profile")) {
                            showDownload = true
                        }
                        .frame(maxWidth: 150)
                        Spacer()
                    }
                    if probeBusy {
                        Text(L10n.t("正在检测卡内通讯录能力，不会写入联系人…"))
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    if let probe = probeResult {
                        VStack(alignment: .leading, spacing: 4) {
                            probeRow(L10n.t("SIM 通讯录"), probe.storageSupported, L10n.t("支持 SM 卡内存储"), L10n.t("未发现 SM 卡内存储"))
                            probeRow(L10n.t("当前卡片"), probe.storageSelected, L10n.t("已安全选中 SM 存储"), L10n.t("无法选中 SM 存储"))
                            probeRow(L10n.t("读取能力"), probe.readSupported, L10n.t("模块支持读取卡内联系人"), L10n.t("模块未确认读取命令"))
                            probeRow(L10n.t("写入接口"), probe.writeSupported, L10n.t("模块声明支持写入接口"), L10n.t("模块未确认写入命令"))
                        }
                        .padding(.top, 4)
                    }
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
            }
            .modifier(PhoneCard())
            .sheet(item: $renameProfile) { profile in
                ESIMProfileRenameSheet(profile: profile) { name in
                    Task {
                        do {
                            try await calls.apiClient.renameESIMProfile(iccid: profile.iccid ?? "", name: name)
                            message = L10n.t("Profile 名称已修改")
                            esim = try? await calls.apiClient.esimOverview()
                        } catch {
                            message = error.localizedDescription
                        }
                    }
                }
            }
            .sheet(item: $editNoteProfile) { profile in
                ESIMProfileNoteSheet(profile: profile, note: esimNotes[profile.iccid ?? ""]) { label, phone, tags in
                    Task {
                        do {
                            try await calls.apiClient.saveESIMNote(iccid: profile.iccid ?? "", label: label, phone: phone, tags: tags)
                            message = L10n.t("模块资料已保存")
                            esimNotes = (try? await calls.apiClient.esimNotes()) ?? [:]
                        } catch {
                            message = error.localizedDescription
                        }
                    }
                }
            }
            .alert(L10n.t("确认删除 Profile？"), isPresented: deleteConfirmBinding) {
                Button(L10n.t("删除"), role: .destructive) {
                    if let profile = deleteProfile {
                        Task {
                            do {
                                try await calls.apiClient.deleteESIMProfile(iccid: profile.iccid ?? "")
                                message = L10n.t("Profile 已删除")
                                esim = try? await calls.apiClient.esimOverview()
                            } catch {
                                message = error.localizedDescription
                            }
                        }
                    }
                    deleteProfile = nil
                }
                Button(L10n.t("取消"), role: .cancel) {
                    deleteProfile = nil
                }
            } message: {
                Text(L10n.t("删除不可恢复，确定删除当前 Profile 吗？"))
            }
            .sheet(isPresented: $showDownload) {
                ESIMDownloadSheet { smdp, matchingID, confirmationCode, imei, aid in
                    Task {
                        do {
                            let result = try await calls.apiClient.downloadESIMProfile(
                                smdp: smdp, matchingID: matchingID,
                                confirmationCode: confirmationCode, imei: imei, aid: aid
                            )
                            message = result.message ?? L10n.t("Profile 下载完成")
                            esim = try? await calls.apiClient.esimOverview()
                        } catch {
                            message = error.localizedDescription
                        }
                    }
                }
            }

            MoreSectionTitle(L10n.t("AT 调试"))
            VStack(spacing: 0) {
                HStack(spacing: 8) {
                    TextField(L10n.t("AT 指令"), text: $atCommand)
                        .textFieldStyle(.roundedBorder)
                        .font(.body)
                        .onSubmit { Task { await runAT() } }
                    Button {
                        Task { await runAT() }
                    } label: {
                        Text(L10n.t("发送"))
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.regular)
                    .disabled(atBusy || atCommand.trimmingCharacters(in: .whitespaces).isEmpty)
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 10)
                if !atResponse.isEmpty {
                    Divider()
                    Text(atResponse)
                        .font(.system(size: 11, design: .monospaced))
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 14)
                        .padding(.vertical, 10)
                }
            }
            .modifier(PhoneCard())
        }
        .task {
            await refreshAll()
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 5_000_000_000)
                if let p = try? await calls.apiClient.cellularPolicy() {
                    policy = p
                }
            }
        }
        .confirmationDialog("切换到 iPhone / iPad 模式？", isPresented: $showMobileProfileConfirm, titleVisibility: .visible) {
            Button("关闭 USB 音频并保存") { Task { await applyUSBProfile("mobile") } }
            Button("取消", role: .cancel) {}
        } message: {
            Text("模块会保留上网和短信接口，但不再暴露 USB 音频。设置保存后直接拔出并连接 iPhone 或 iPad 即可。")
        }
        .confirmationDialog("恢复 Mac 完整模式？", isPresented: $showMacProfileConfirm, titleVisibility: .visible) {
            Button("恢复并重新连接") { Task { await applyUSBProfile("mac") } }
            Button("取消", role: .cancel) {}
        } message: {
            Text("模块将恢复 USB 音频并重新连接。完成后可继续使用通话功能。")
        }
    }

    private var cellularBinding: Binding<Bool> {
        Binding(
            get: { policy?.forceOff != true },
            set: { allow in
                busy = true
                Task {
                    do {
                        policy = try await calls.apiClient.setCellularPolicy(forceOff: !allow)
                        message = allow ? L10n.t("已允许 4G 上网") : L10n.t("已禁止 4G 上网")
                    } catch {
                        message = error.localizedDescription
                    }
                    busy = false
                }
            }
        )
    }

    private func refreshAll() async {
        busy = true
        defer { busy = false }
        async let m: ModemStatus? = try? calls.apiClient.modemStatus()
        async let t: NetworkTrafficSnapshot? = try? calls.apiClient.networkTraffic()
        async let p: CellularPolicyStatus? = try? calls.apiClient.cellularPolicy()
        async let u: USBProfileStatus? = try? calls.apiClient.usbProfile()
        async let e: ESIMOverview? = try? calls.apiClient.esimOverview()
        async let h: ESIMHealth? = try? calls.apiClient.esimHealth()
        async let n: [String: ESIMNote]? = try? calls.apiClient.esimNotes()
        modem = await m
        traffic = await t
        policy = await p
        usbProfile = await u
        esim = await e
        esimHealth = await h
        esimNotes = await n ?? [:]
    }

    private func applyUSBProfile(_ mode: String) async {
        busy = true
        defer { busy = false }
        do {
            usbProfile = try await calls.apiClient.setUSBProfile(mode)
            message = usbProfile?.message ?? (mode == "mobile" ? "已保存 iPhone / iPad 模式" : "正在恢复 Mac 完整模式")
        } catch {
            message = error.localizedDescription
        }
    }

    private func runCheck(_ result: NetworkCheckResult) {
        var text = result.summary ?? (result.ok ? L10n.t("检查通过") : L10n.t("检查未通过"))
        if let detail = result.detail, !detail.isEmpty {
            text += "（\(detail)）"
        }
        message = text
    }

    private func reboot() async {
        do {
            try await calls.apiClient.rebootModule()
            message = L10n.t("已发送模块重启指令")
        } catch {
            message = error.localizedDescription
        }
    }

    private func runAT() async {
        atBusy = true
        defer { atBusy = false }
        do {
            let result = try await calls.apiClient.executeAT(atCommand)
            atResponse = result.response
        } catch {
            atResponse = error.localizedDescription
        }
    }

    private func switchProfile(_ profile: ESIMProfile) async {
        guard let iccid = profile.iccid else { return }
        busy = true
        defer { busy = false }
        do {
            _ = try await calls.apiClient.switchESIM(iccid: iccid)
            message = L10n.t("已切换 Profile，模块正在重启并重新读取新卡…")
            esim = try? await calls.apiClient.esimOverview()
        } catch {
            message = error.localizedDescription
        }
    }

    @ViewBuilder
    private func diagnosticRows(_ diag: NetworkDiagnostic) -> some View {
        MoreInfoRow(
            label: L10n.t("USB 网卡"),
            value: diag.usbNetworkPresent ? L10n.t("已识别") : L10n.t("未识别")
        )
        if let route = diag.defaultRoute {
            Divider().padding(.leading, 12)
            MoreInfoRow(
                label: L10n.t("默认出口"),
                value: [route.interface, route.gateway].compactMap { $0 }.filter { !$0.isEmpty }
                    .joined(separator: " -> ").isEmpty ? "--" : [route.interface, route.gateway].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " -> ")
            )
        }
        Divider().padding(.leading, 12)
        MoreInfoRow(label: "usbnet", value: diag.usbnetMode ?? "--")
        Divider().padding(.leading, 12)
        MoreInfoRow(
            label: L10n.t("蜂窝数据"),
            value: diag.activeContexts.map { $0.map(String.init).joined(separator: ", ") }
                .flatMap { $0.isEmpty ? nil : L10n.f("已激活 %@", $0) } ?? L10n.t("未激活")
        )
        Divider().padding(.leading, 12)
        MoreInfoRow(
            label: L10n.t("蜂窝 IP"),
            value: diag.pdpAddresses.map { $0.joined(separator: " · ") }
                .flatMap { $0.isEmpty ? nil : $0 } ?? L10n.t("无")
        )
        Divider().padding(.leading, 12)
        MoreInfoRow(
            label: "APN",
            value: diag.pdpContexts.map { $0.compactMap { ctx in
                [ctx.id.map(String.init), ctx.apn].compactMap { $0 }.joined(separator: ":")
            }.joined(separator: " · ") }.flatMap { $0.isEmpty ? nil : $0 } ?? L10n.t("无")
        )
        if let usb = diag.usbDevice {
            Divider().padding(.leading, 12)
            MoreInfoRow(
                label: L10n.t("USB 枚举"),
                value: [usb.vendor, usb.product, usb.mode].compactMap { $0 }.filter { !$0.isEmpty }
                    .joined(separator: " ").isEmpty ? "--" : [usb.vendor, usb.product, usb.mode].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " "),
                detail: [usb.vendorID, usb.productID].compactMap { $0 }.joined(separator: ":")
            )
        }
        if let interfaces = diag.macInterfaces, !interfaces.isEmpty {
            Divider().padding(.leading, 12)
            ForEach(interfaces, id: \.name) { interface in
                HStack(spacing: 8) {
                    Text(interface.name ?? "--")
                        .font(.footnote.weight(.medium))
                    Text([interface.kind, interface.ipv4].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " · "))
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                    Spacer()
                    Text(interface.status == "active" ? "active" : "inactive")
                        .font(.caption2.weight(.medium))
                        .foregroundStyle(interface.status == "active" ? Color.green : Color.secondary)
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 6)
            }
        }
        if let errors = diag.errors, !errors.isEmpty {
            Divider().padding(.leading, 12)
            Text(L10n.t("读取部分诊断失败：") + errors.values.joined(separator: "；"))
                .font(.caption2)
                .foregroundStyle(.secondary)
                .padding(.horizontal, 14)
                .padding(.vertical, 6)
        }
    }

    private func loadNetworkDiagnostic() async {
        networkDiagBusy = true
        defer { networkDiagBusy = false }
        do {
            networkDiag = try await calls.apiClient.networkDiagnostic()
        } catch {
            message = error.localizedDescription
        }
    }

    private func probePhonebook() async {
        probeBusy = true
        defer { probeBusy = false }
        do {
            probeResult = try await calls.apiClient.probeESIMPhonebook()
        } catch {
            message = error.localizedDescription
        }
    }

    private var deleteConfirmBinding: Binding<Bool> {
        Binding(
            get: { deleteProfile != nil },
            set: { if !$0 { deleteProfile = nil } }
        )
    }

    private func eidDetail(_ eid: ESIMEID) -> String? {
        var parts: [String] = []
        if let aid = eid.aid, !aid.isEmpty { parts.append("AID \(aid)") }
        if let free = eid.freeNvram, !free.isEmpty { parts.append(L10n.f("可用 %@", free)) }
        if let firmware = eid.firmware, !firmware.isEmpty { parts.append(L10n.f("固件 %@", firmware)) }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }

    @ViewBuilder
    private func probeRow(_ label: String, _ ok: Bool?, _ okText: String, _ failText: String) -> some View {
        HStack(spacing: 6) {
            Image(systemName: (ok == true) ? "checkmark.circle.fill" : "xmark.circle")
                .font(.caption)
                .foregroundStyle(ok == true ? Color.green : Color.secondary)
            Text(label)
                .font(.caption)
                .foregroundStyle(.secondary)
            Spacer()
            Text(ok == true ? okText : failText)
                .font(.caption)
                .lineLimit(1)
                .minimumScaleFactor(0.7)
        }
    }

    private static func maskIdentifier(_ value: String?) -> String? {
        guard let value, !value.isEmpty else { return nil }
        if value.count <= 8 { return value }
        return "\(value.prefix(4)) \(String(repeating: "•", count: max(4, value.count - 8))) \(value.suffix(4))"
    }
}

struct MoreSectionTitle: View {
    let title: String

    init(_ title: String) {
        self.title = title
    }

    var body: some View {
        Text(title)
            .font(.body.weight(.semibold))
            .foregroundStyle(.secondary)
    }
}

struct MoreInfoRow: View {
    let label: String
    let value: String
    var detail: String? = nil

    var body: some View {
        HStack(spacing: 10) {
            Text(label)
                .font(.footnote)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .minimumScaleFactor(0.75)
            Spacer()
            VStack(alignment: .trailing, spacing: 1) {
                Text(value)
                    .font(.footnote.weight(.medium))
                    .lineLimit(1)
                    .minimumScaleFactor(0.7)
                if let detail, !detail.isEmpty {
                    Text(detail)
                        .font(.caption2)
                        .foregroundStyle(.tertiary)
                        .lineLimit(1)
                        .minimumScaleFactor(0.7)
                }
            }
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 8)
    }
}

struct MoreActionButton: View {
    let title: String
    var destructive = false
    let action: () async -> Void
    @State private var running = false

    var body: some View {
        Button {
            running = true
            Task {
                await action()
                running = false
            }
        } label: {
            Text(running ? L10n.t("处理中…") : title)
                .font(.system(size: 12, weight: .medium))
                .lineLimit(1)
                .minimumScaleFactor(0.75)
                .frame(maxWidth: .infinity)
        }
        .buttonStyle(.bordered)
        .controlSize(.regular)
        .tint(destructive ? .red : nil)
        .disabled(running)
    }
}

/// 设置行开关：开关与标题同行、与标题对齐（iOS「设置」样式），
/// 说明文字在标题下方；保证多个开关行的开关位置一致。
private struct SettingToggleRow: View {
    let title: String
    let subtitle: String
    @Binding var isOn: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 10) {
                Text(title)
                    .font(.system(size: 12, weight: .medium))
                    .lineLimit(1)
                    .minimumScaleFactor(0.75)
                Spacer(minLength: 8)
                Toggle("", isOn: $isOn)
                    .toggleStyle(.switch)
                    .controlSize(.regular)
                    .labelsHidden()
            }
            Text(subtitle)
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 8)
    }
}

/// 运营商显示名：把模块上报的英文名/代码规范化为中文（与网页端映射一致）。
private enum OperatorName {
    private static let map: [String: String] = [
        "CHN-UNICOM": "中国联通", "CHINA UNICOM": "中国联通", "UNICOM": "中国联通",
        "46001": "中国联通", "46006": "中国联通", "46009": "中国联通",
        "CHINA MOBILE": "中国移动", "CMCC": "中国移动", "CHN-CMCC": "中国移动",
        "46000": "中国移动", "46002": "中国移动", "46004": "中国移动",
        "46007": "中国移动", "46008": "中国移动",
        "CHINA TELECOM": "中国电信", "CHN-CT": "中国电信", "CTCC": "中国电信",
        "46003": "中国电信", "46005": "中国电信", "46011": "中国电信",
        "CBN": "中国广电", "CHN-CBN": "中国广电", "CHINA BROADNET": "中国广电", "46015": "中国广电",
    ]

    static func display(_ raw: String?) -> String? {
        guard let raw, !raw.isEmpty else { return nil }
        let key = raw.trimmingCharacters(in: .whitespaces).uppercased()
        return map[key] ?? raw
    }
}


// MARK: - 设置页顶部状态卡（运营商 / SIM 接入 / 实时上下行速率）

/// 展示性状态卡片：运营商、SIM 接入状态、当前下载/上传速度。
/// 每 2 秒采样一次流量计数，用相邻两次采样计算实时速率（与网页端一致）。
struct DeviceStatusCard: View {
    @EnvironmentObject private var calls: CallCenter
    @EnvironmentObject private var settings: AppSettings

    @State private var status: ModemStatus?
    @State private var traffic: NetworkTrafficSnapshot?
    @State private var rxRate: Double?
    @State private var txRate: Double?
    @State private var previous: NetworkTrafficSnapshot?

    private var simText: String {
        guard let inserted = status?.simInserted else { return "--" }
        return inserted ? L10n.t("已接入") : L10n.t("未接入")
    }

    private var operatorText: String {
        OperatorName.display(status?.operatorName) ?? "--"
    }

    private var signalText: String {
        guard let dbm = status?.signalDBM else { return "--" }
        return "\(dbm) dBm"
    }

    private var trafficText: String {
        guard let traffic, traffic.available else { return "--" }
        let bytes = Double(traffic.sessionTotal)
        if bytes >= 1_073_741_824 {
            return String(format: "%.1f GB", bytes / 1_073_741_824)
        }
        if bytes >= 1_048_576 {
            return String(format: "%.1f MB", bytes / 1_048_576)
        }
        return "\(Int(bytes / 1024)) KB"
    }

    var body: some View {
        VStack(spacing: 0) {
            MoreInfoRow(label: L10n.t("运营商"), value: operatorText)
            Divider().padding(.leading, 12)
            MoreInfoRow(label: L10n.t("SIM 卡接入"), value: simText)
            Divider().padding(.leading, 12)
            MoreInfoRow(label: L10n.t("网络模式"), value: status?.networkMode ?? "--")
            Divider().padding(.leading, 12)
            MoreInfoRow(label: L10n.t("信号强度"), value: signalText)
            Divider().padding(.leading, 12)
            MoreInfoRow(label: L10n.t("下载速度"), value: Self.rateText(rxRate))
            Divider().padding(.leading, 12)
            MoreInfoRow(label: L10n.t("上传速度"), value: Self.rateText(txRate))
            Divider().padding(.leading, 12)
            MoreInfoRow(label: L10n.t("本次流量"), value: trafficText)
            Divider().padding(.leading, 12)
            HStack(spacing: 8) {
                MoreActionButton(title: L10n.t("刷新")) {
                    await sampleOnce()
                }
                .frame(maxWidth: 100)
                Text(L10n.t("每 3 秒自动更新"))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                Spacer(minLength: 0)
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
        }
        .modifier(PhoneCard())
        .task {
            while !Task.isCancelled {
                await sampleOnce()
                try? await Task.sleep(nanoseconds: 3_000_000_000)
            }
        }
    }

    private func sampleOnce() async {
        if let modem = try? await calls.apiClient.modemStatus() {
            if modem.operatorName != nil || status == nil {
                status = modem
            } else if let prev = status {
                status = ModemStatus(
                    signalDBM: modem.signalDBM ?? prev.signalDBM,
                    networkMode: modem.networkMode ?? prev.networkMode,
                    operatorName: modem.operatorName ?? prev.operatorName,
                    simInserted: modem.simInserted ?? prev.simInserted,
                    regStatusText: modem.regStatusText ?? prev.regStatusText,
                    imei: modem.imei ?? prev.imei,
                    iccid: modem.iccid ?? prev.iccid
                )
            }
        }
        guard let current = try? await calls.apiClient.networkTraffic(), current.available else {
            previous = nil
            rxRate = nil
            txRate = nil
            return
        }
        traffic = current
        if let previous, previous.interface == current.interface {
            let elapsed = Double(current.sampledAtMS - previous.sampledAtMS) / 1000.0
            if elapsed > 0 {
                rxRate = max(0, Double(current.rxBytes - previous.rxBytes)) / elapsed
                txRate = max(0, Double(current.txBytes - previous.txBytes)) / elapsed
            }
        }
        previous = current
    }

    private static func rateText(_ rate: Double?) -> String {
        guard let rate, rate >= 0 else { return "--" }
        if rate >= 1_048_576 {
            return String(format: "%.1f MB/s", rate / 1_048_576)
        }
        if rate >= 1024 {
            return String(format: "%.1f KB/s", rate / 1024)
        }
        return String(format: "%.0f B/s", rate)
    }
}


// MARK: - eSIM 弹窗（重命名 / 模块资料 / 下载 Profile）

struct ESIMProfileRenameSheet: View {
    @Environment(\.dismiss) private var dismiss
    let profile: ESIMProfile
    let onSave: (String) -> Void
    @State private var name = ""
    @State private var saving = false

    var body: some View {
        VStack(spacing: 14) {
            Text(L10n.t("重命名 Profile"))
                .font(.title3.weight(.semibold))
            TextField(L10n.t("Profile 名称"), text: $name)
                .textFieldStyle(.roundedBorder)
                .frame(width: 280)
            HStack(spacing: 10) {
                Button(L10n.t("取消")) {
                    dismiss()
                }
                .keyboardShortcut(.cancelAction)
                Button {
                    saving = true
                    onSave(name.trimmingCharacters(in: .whitespacesAndNewlines))
                    dismiss()
                } label: {
                    Text(L10n.t("保存"))
                        .frame(minWidth: 60)
                }
                .buttonStyle(.borderedProminent)
                .disabled(name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || saving)
            }
        }
        .padding(20)
        .onAppear {
            name = profile.name ?? ""
        }
    }
}

struct ESIMProfileNoteSheet: View {
    @Environment(\.dismiss) private var dismiss
    let profile: ESIMProfile
    let note: ESIMNote?
    let onSave: (String, String, String) -> Void
    @State private var label = ""
    @State private var phone = ""
    @State private var tags = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(L10n.t("模块资料"))
                .font(.title3.weight(.semibold))
            Text(L10n.t("资料保存在本机，按 ICCID 与当前 Profile 关联"))
                .font(.caption)
                .foregroundStyle(.secondary)
            TextField(L10n.t("模块内名称（可选）"), text: $label)
                .textFieldStyle(.roundedBorder)
            TextField(L10n.t("模块号码（可选）"), text: $phone)
                .textFieldStyle(.roundedBorder)
            TextField(L10n.t("用途标签（可选）"), text: $tags)
                .textFieldStyle(.roundedBorder)
            HStack(spacing: 10) {
                Spacer()
                Button(L10n.t("取消")) {
                    dismiss()
                }
                .keyboardShortcut(.cancelAction)
                Button {
                    onSave(
                        label.trimmingCharacters(in: .whitespacesAndNewlines),
                        phone.trimmingCharacters(in: .whitespacesAndNewlines),
                        tags.trimmingCharacters(in: .whitespacesAndNewlines)
                    )
                    dismiss()
                } label: {
                    Text(L10n.t("保存"))
                        .frame(minWidth: 60)
                }
                .buttonStyle(.borderedProminent)
            }
        }
        .frame(width: 320)
        .padding(20)
        .onAppear {
            label = note?.label ?? ""
            phone = note?.phone ?? ""
            tags = note?.tags ?? ""
        }
    }
}

struct ESIMDownloadSheet: View {
    @Environment(\.dismiss) private var dismiss
    let onDownload: (String, String, String, String, String) -> Void
    @State private var smdp = ""
    @State private var matchingID = ""
    @State private var confirmationCode = ""
    @State private var imei = ""
    @State private var aid = ""

    private var canSubmit: Bool {
        !smdp.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            && !imei.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(L10n.t("下载新的 Profile"))
                .font(.title3.weight(.semibold))
            Text(L10n.t("将向 SM-DP+ 服务器下载并写入新的 eSIM Profile。写入期间请勿拔出模块。"))
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            TextField(L10n.t("SM-DP+ 地址"), text: $smdp)
                .textFieldStyle(.roundedBorder)
            TextField(L10n.t("Matching ID（可选）"), text: $matchingID)
                .textFieldStyle(.roundedBorder)
            TextField(L10n.t("确认码（可选）"), text: $confirmationCode)
                .textFieldStyle(.roundedBorder)
            TextField(L10n.t("IMEI（必填）"), text: $imei)
                .textFieldStyle(.roundedBorder)
            TextField(L10n.t("AID（可选）"), text: $aid)
                .textFieldStyle(.roundedBorder)
            HStack(spacing: 10) {
                Spacer()
                Button(L10n.t("取消")) {
                    dismiss()
                }
                .keyboardShortcut(.cancelAction)
                Button {
                    onDownload(
                        smdp.trimmingCharacters(in: .whitespacesAndNewlines),
                        matchingID.trimmingCharacters(in: .whitespacesAndNewlines),
                        confirmationCode.trimmingCharacters(in: .whitespacesAndNewlines),
                        imei.trimmingCharacters(in: .whitespacesAndNewlines),
                        aid.trimmingCharacters(in: .whitespacesAndNewlines)
                    )
                    dismiss()
                } label: {
                    Text(L10n.t("开始下载"))
                        .frame(minWidth: 70)
                }
                .buttonStyle(.borderedProminent)
                .disabled(!canSubmit)
            }
        }
        .frame(width: 340)
        .padding(20)
    }
}


// MARK: - GPS 定位开关与详情（并入「通用」板块）

struct GPSPanel: View {
    @EnvironmentObject private var calls: CallCenter
    @State private var gps: GPSStatus?
    @State private var busy = false
    @State private var message = ""

    private var gpsText: String {
        guard let fix = gps?.lastFix,
              let lat = fix.latitude, let lng = fix.longitude else { return L10n.t("等待定位…") }
        return "\(lat), \(lng)"
    }

    var body: some View {
        VStack(spacing: 0) {
            SettingToggleRow(
                title: L10n.t("GPS 定位"),
                subtitle: L10n.t("默认关闭；开启后仅在本机读取定位信息"),
                isOn: gpsBinding
            )
            if gps?.enabled == true {
                Divider().padding(.leading, 14)
                MoreInfoRow(label: L10n.t("坐标"), value: gpsText)
                Divider().padding(.leading, 12)
                HStack(spacing: 0) {
                    MoreInfoRow(label: L10n.t("卫星"), value: gps?.lastFix?.satellites ?? "--")
                        .frame(maxWidth: .infinity)
                    Divider().frame(height: 28)
                    MoreInfoRow(label: "HDOP", value: gps?.lastFix?.hdop ?? "--")
                        .frame(maxWidth: .infinity)
                }
                HStack(spacing: 8) {
                    MoreActionButton(title: L10n.t("立即刷新")) {
                        await refreshGPS()
                    }
                    .frame(maxWidth: 120)
                    if !message.isEmpty {
                        Text(message)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                    }
                    Spacer(minLength: 0)
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 8)
            }
        }
        .task {
            if let current = try? await calls.apiClient.gpsStatus() {
                gps = current
            }
        }
    }

    private var gpsBinding: Binding<Bool> {
        Binding(
            get: { gps?.enabled == true },
            set: { enable in
                busy = true
                Task {
                    defer { busy = false }
                    do {
                        if enable {
                            let resp = try await calls.apiClient.gpsStart()
                            gps = GPSStatus(enabled: resp.enabled, lastFix: resp.lastFix)
                            message = L10n.t("已启动定位")
                        } else {
                            _ = try await calls.apiClient.gpsStop()
                            gps = GPSStatus(enabled: false, lastFix: nil)
                            message = L10n.t("已停止定位")
                        }
                    } catch {
                        message = error.localizedDescription
                    }
                }
            }
        )
    }

    private func refreshGPS() async {
        busy = true
        defer { busy = false }
        do {
            let fix = try await calls.apiClient.gpsRefresh()
            gps = GPSStatus(enabled: true, lastFix: fix)
            message = L10n.t("定位已刷新")
        } catch {
            message = error.localizedDescription
        }
    }
}
