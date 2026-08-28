import Foundation

struct CallRecord: Codable, Equatable, Sendable, Identifiable {
    let id: String
    let index: Int
    let direction: String
    let state: String
    let number: String?
    let startedAt: Date
    let updatedAt: Date
    let endedAt: Date?
    let missed: Bool

    enum CodingKeys: String, CodingKey {
        case id, index, direction, state, number, missed
        case startedAt = "started_at"
        case updatedAt = "updated_at"
        case endedAt = "ended_at"
    }
}

struct CallStatus: Codable, Sendable {
    let active: CallRecord?
    let history: [CallRecord]?
    let polling: Bool
    let pollIntervalSeconds: Int
    let lastPollError: String

    enum CodingKeys: String, CodingKey {
        case active, history, polling
        case pollIntervalSeconds = "poll_interval_s"
        case lastPollError = "last_poll_error"
    }
}

struct SMSMessage: Codable, Equatable, Sendable {
    let sender: String
    let content: String
    let code: String?
    let timestamp: Date

    var identity: String {
        "\(sender)\u{0}\(timestamp.timeIntervalSince1970)\u{0}\(content)"
    }
}

struct RejectResponse: Codable, Sendable {
    let rejected: Bool
}

struct SMSSendResult: Codable, Sendable {
    let sent: Bool
    let segments: Int?
}

struct CallRecordingResponse: Codable, Sendable {
    let recording: Bool
    let path: String?
}

struct SMSStatus: Codable, Sendable {
    let autoCleanupME: Bool
    enum CodingKeys: String, CodingKey { case autoCleanupME = "auto_cleanup_me" }
}

struct SIMIdentity: Codable, Sendable {
    let phoneNumber: String
    enum CodingKeys: String, CodingKey { case phoneNumber = "phone_number" }
}

struct MaVoAudioHostConfig: Codable, Sendable {
    let vendorID: UInt16
    let productID: UInt16
    let locationID: UInt32
    let routeReady: Bool
    let routeError: String?

    enum CodingKeys: String, CodingKey {
        case vendorID = "vendor_id"
        case productID = "product_id"
        case locationID = "location_id"
        case routeReady = "route_ready"
        case routeError = "route_error"
    }
}

struct GPSStatus: Codable, Sendable {
    let enabled: Bool
    let lastFix: GPSFixSummary?

    enum CodingKeys: String, CodingKey {
        case enabled
        case lastFix = "last_fix"
    }
}

struct GPSFixSummary: Codable, Sendable {
    let latitude: String?
    let longitude: String?
    let hdop: String
    let satellites: String
}

struct NetworkCheckResult: Codable, Sendable {
    let ok: Bool
    let summary: String?
    let detail: String?
}

struct ModemStatus: Codable, Sendable {
    let signalDBM: Int?
    let networkMode: String?
    let operatorName: String?
    let simInserted: Bool?
    let regStatusText: String?
    let imei: String?
    let iccid: String?

    enum CodingKeys: String, CodingKey {
        case signalDBM = "signal_dbm"
        case networkMode = "network_mode"
        case operatorName = "operator"
        case simInserted = "sim_inserted"
        case regStatusText = "reg_status_text"
        case imei, iccid
    }
}

struct ModuleSetupStatus: Codable, Sendable {
    let state: String
    let summary: String
    let detail: String?
    let canInitialize: Bool
    let requiresConfirmation: Bool
    let backupPath: String?

    enum CodingKeys: String, CodingKey {
        case state, summary, detail
        case canInitialize = "can_initialize"
        case requiresConfirmation = "requires_confirmation"
        case backupPath = "backup_path"
    }
}

struct VoiceRuntimeStatus: Codable, Sendable {
    let ready: Bool
    let runtimeInstalled: Bool
    let runtimeSource: String?
    let runtimeDetail: String?
    let lastError: String?

    enum CodingKeys: String, CodingKey {
        case ready
        case runtimeInstalled = "runtime_installed"
        case runtimeSource = "runtime_source"
        case runtimeDetail = "runtime_detail"
        case lastError = "last_error"
    }
}

enum APIError: LocalizedError {
    case invalidResponse
    case http(Int, String?)
    case unreadablePayload(String)

    var errorDescription: String? {
        switch self {
        case .invalidResponse:
            return "DJOneHub 返回了无效响应"
        case let .http(status, message):
            if let message, !message.isEmpty {
                return message
            }
            return "DJOneHub 请求失败（HTTP \(status)）"
        case let .unreadablePayload(message):
            return message
        }
    }
}

private struct APIErrorPayload: Decodable {
    let error: String?
}

struct DJOneHubAPI: Sendable {
    let baseURL: URL

    private static let decoder: JSONDecoder = {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        return decoder
    }()

    func callStatus() async throws -> CallStatus {
        try await get(path: "api/calls/status")
    }

    func messages() async throws -> [SMSMessage] {
        try await get(path: "api/sms")
    }

    func gpsStatus() async throws -> GPSStatus {
        try await get(path: "api/gps")
    }

    func isUsingCellularRoute() async throws -> Bool {
        var request = URLRequest(url: baseURL.appending(path: "api/network/check-4g"))
        request.httpMethod = "POST"
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.timeoutInterval = 5
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw APIError.invalidResponse
        }
        try Self.requireSuccess(http, data: data)
        return try Self.decoder.decode(NetworkCheckResult.self, from: data).ok
    }

    func modemStatus() async throws -> ModemStatus {
        try await get(path: "api/status")
    }

    func moduleSetupStatus() async throws -> ModuleSetupStatus {
        try await get(path: "api/module/setup")
    }

    func voiceRuntimeStatus() async throws -> VoiceRuntimeStatus {
        try await get(path: "api/voice/status")
    }

    func provisionVoiceRuntime() async throws -> VoiceRuntimeStatus {
        try await postDecoded(
            path: "api/voice/provision",
            body: ["confirm": true],
            timeout: 180,
            unreadablePayloadMessage: "语音运行时下载完成，但 DJOneHub 返回的数据无法识别。请更新本机后台服务后重试。"
        )
    }

    func initializeModule() async throws -> ModuleSetupStatus {
        // The endpoint returns as soon as the background state machine starts,
        // but USB AT can be briefly serialized by an SMS/status read. Give that
        // harmless admission check enough room instead of presenting a false
        // timeout while the module has already begun re-enumerating.
        try await postDecoded(path: "api/module/setup", body: ["confirm": true], timeout: 20)
    }

    func rejectCall() async throws -> RejectResponse {
        var request = URLRequest(url: baseURL.appending(path: "api/calls/reject"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw APIError.invalidResponse
        }
        try Self.requireSuccess(http, data: data)
        return try Self.decoder.decode(RejectResponse.self, from: data)
    }

    private func get<T: Decodable>(path: String) async throws -> T {
        var request = URLRequest(url: baseURL.appending(path: path))
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.timeoutInterval = 10
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw APIError.invalidResponse
        }
        try Self.requireSuccess(http, data: data)
        return try Self.decoder.decode(T.self, from: data)
    }

    private static func requireSuccess(_ response: HTTPURLResponse, data: Data) throws {
        guard (200..<300).contains(response.statusCode) else {
            let message = (try? decoder.decode(APIErrorPayload.self, from: data))?.error
            throw APIError.http(response.statusCode, message)
        }
    }
}

// MARK: - 原生 App 接口（拨号 / 接听 / 挂断 / 静音）

extension DJOneHubAPI {
    func smsStatus() async throws -> SMSStatus { try await get(path: "api/sms/status") }
    func setSMSAutoCleanup(_ enabled: Bool) async throws {
        try await patch(path: "api/sms/settings", body: ["auto_cleanup_me": enabled])
    }
    func simIdentity() async throws -> SIMIdentity { try await get(path: "api/sim/identity") }

    func setMaVoAudioHostEnabled(_ enabled: Bool) async throws {
        try await post(path: "api/calls/audio/host/register", body: ["enabled": enabled])
    }

    func maVoAudioHostConfig() async throws -> MaVoAudioHostConfig {
        try await get(path: "api/calls/audio/host/config")
    }

    func dial(number: String) async throws {
        try await post(path: "api/calls/dial", body: ["number": number])
    }

    func answerCall() async throws {
        try await post(path: "api/calls/answer", body: EmptyBody())
    }

    func hangupCall() async throws {
        try await post(path: "api/calls/hangup", body: EmptyBody())
    }

    func sendDTMF(digit: String) async throws {
        try await post(path: "api/calls/dtmf", body: ["digit": digit])
    }

    func setAudioMuted(_ muted: Bool) async throws {
        try await post(path: "api/calls/audio/mute", body: ["muted": muted])
    }

    func setCallRecording(_ recording: Bool) async throws -> CallRecordingResponse {
        try await postDecoded(
            path: "api/calls/audio/record",
            body: ["action": recording ? "start" : "stop"]
        )
    }

    func sendSMS(to phone: String, message: String) async throws -> SMSSendResult {
        var request = URLRequest(url: baseURL.appending(path: "api/sms/send"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(["phone": phone, "message": message])
        request.timeoutInterval = 8
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw APIError.invalidResponse
        }
        try Self.requireSuccess(http, data: data)
        return try Self.decoder.decode(SMSSendResult.self, from: data)
    }

    private func patch<Body: Encodable>(path: String, body: Body) async throws {
        try await send(method: "PATCH", path: path, body: body)
    }

    private func delete<Body: Encodable>(path: String, body: Body) async throws {
        try await send(method: "DELETE", path: path, body: body)
    }

    private func send<Body: Encodable>(method: String, path: String, body: Body) async throws {
        var request = URLRequest(url: baseURL.appending(path: path))
        request.httpMethod = method
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(body)
        request.timeoutInterval = 8
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw APIError.invalidResponse
        }
        try Self.requireSuccess(http, data: data)
    }

    private func post<Body: Encodable>(path: String, body: Body) async throws {
        var request = URLRequest(url: baseURL.appending(path: path))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(body)
        request.timeoutInterval = 5
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw APIError.invalidResponse
        }
        try Self.requireSuccess(http, data: data)
    }

    private func postDecoded<Response: Decodable, Body: Encodable>(
        path: String,
        body: Body,
        timeout: TimeInterval = 5,
        unreadablePayloadMessage: String? = nil
    ) async throws -> Response {
        var request = URLRequest(url: baseURL.appending(path: path))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(body)
        request.timeoutInterval = timeout
        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw APIError.invalidResponse
        }
        try Self.requireSuccess(http, data: data)
        do {
            return try Self.decoder.decode(Response.self, from: data)
        } catch {
            if let unreadablePayloadMessage {
                throw APIError.unreadablePayload(unreadablePayloadMessage)
            }
            throw error
        }
    }
}

private struct EmptyBody: Encodable {}

// MARK: - 更多功能（网络 / 定位 / eSIM / AT）

struct CellularPolicyStatus: Codable, Sendable {
    let forceOff: Bool
    let services: [String]

    enum CodingKeys: String, CodingKey {
        case forceOff = "force_off"
        case services
    }
}

struct NetworkTrafficSnapshot: Codable, Sendable {
    let available: Bool
    let interface: String?
    let rxBytes: UInt64
    let txBytes: UInt64
    let sessionRX: UInt64
    let sessionTX: UInt64
    let sessionTotal: UInt64
    let sampledAtMS: Int64
    let error: String?

    enum CodingKeys: String, CodingKey {
        case available, interface, error
        case rxBytes = "rx_bytes"
        case txBytes = "tx_bytes"
        case sessionRX = "session_rx_bytes"
        case sessionTX = "session_tx_bytes"
        case sessionTotal = "session_total_bytes"
        case sampledAtMS = "sampled_at_ms"
    }
}

struct USBProfileStatus: Codable, Sendable {
    let mode: String
    let uacEnabled: Bool
    let configuration: String
    let needsReconnect: Bool
    let message: String?

    enum CodingKeys: String, CodingKey {
        case mode
        case uacEnabled = "uac_enabled"
        case configuration
        case needsReconnect = "needs_reconnect"
        case message
    }
}

struct GPSControlResponse: Codable, Sendable {
    let enabled: Bool
    let lastFix: GPSFixSummary?

    enum CodingKeys: String, CodingKey {
        case enabled
        case lastFix = "last_fix"
    }
}

struct ATResult: Codable, Sendable {
    let response: String
}

struct ESIMOverview: Codable, Sendable {
    let cardType: String?
    let message: String?
    let chipInfo: ESIMChipInfo?
    let profiles: [ESIMProfileGroup]?

    enum CodingKeys: String, CodingKey {
        case cardType = "card_type"
        case message
        case chipInfo = "chip_info"
        case profiles
    }
}

struct ESIMChipInfo: Codable, Sendable {
    let skuName: String?
    let serialNumber: String?
    let firmware: String?
    let eids: [ESIMEID]?

    enum CodingKeys: String, CodingKey {
        case skuName = "sku_name"
        case serialNumber = "serial_number"
        case firmware
        case eids
    }
}

struct ESIMEID: Codable, Sendable, Identifiable {
    let eid: String?
    let aid: String?
    let freeNvram: String?
    let firmware: String?
    let specGuess: String?

    var id: String { eid ?? aid ?? UUID().uuidString }

    enum CodingKeys: String, CodingKey {
        case eid, aid, firmware
        case freeNvram = "free_nvram"
        case specGuess = "spec_guess"
    }
}

struct ESIMProfileGroup: Codable, Sendable {
    let eid: String?
    let aidHex: String?
    let profiles: [ESIMProfile]?

    enum CodingKeys: String, CodingKey {
        case eid
        case aidHex = "aid_hex"
        case profiles
    }
}

struct ESIMProfile: Codable, Sendable, Identifiable {
    let iccid: String?
    let name: String?
    let serviceProviderName: String?
    let state: Int?
    let stateText: String?

    var id: String { iccid ?? name ?? UUID().uuidString }
    var enabled: Bool { state == 1 }

    enum CodingKeys: String, CodingKey {
        case iccid, name, state
        case serviceProviderName = "service_provider_name"
        case stateText = "state_text"
    }
}

// MARK: - 更多功能接口

extension DJOneHubAPI {
    func cellularPolicy() async throws -> CellularPolicyStatus {
        try await get(path: "api/network/cellular-policy")
    }

    func setCellularPolicy(forceOff: Bool) async throws -> CellularPolicyStatus {
        try await postDecoded(path: "api/network/cellular-policy", body: ["force_off": forceOff])
    }

    func check4GRoute() async throws -> NetworkCheckResult {
        try await postDecoded(path: "api/network/check-4g", body: EmptyBody())
    }

    func checkProxyRoute() async throws -> NetworkCheckResult {
        try await postDecoded(path: "api/network/check-proxy", body: EmptyBody())
    }

    func rebootModule() async throws {
        try await post(path: "api/network/reboot-module", body: EmptyBody())
    }

    func networkTraffic() async throws -> NetworkTrafficSnapshot {
        try await get(path: "api/network/traffic")
    }

    func usbProfile() async throws -> USBProfileStatus {
        try await get(path: "api/usb/profile")
    }

    func setUSBProfile(_ mode: String) async throws -> USBProfileStatus {
        try await postDecoded(path: "api/usb/profile", body: ["mode": mode])
    }

    func gpsStart() async throws -> GPSControlResponse {
        try await postDecoded(path: "api/gps/start", body: EmptyBody())
    }

    func gpsStop() async throws -> GPSControlResponse {
        try await postDecoded(path: "api/gps/stop", body: EmptyBody())
    }

    func gpsRefresh() async throws -> GPSFixSummary {
        try await postDecoded(path: "api/gps/refresh", body: EmptyBody())
    }

    func executeAT(_ command: String) async throws -> ATResult {
        try await postDecoded(path: "api/at", body: ["command": command])
    }

    func refreshSMS() async throws {
        try await post(path: "api/sms/refresh", body: EmptyBody())
    }

    func clearModuleSMS() async throws {
        try await post(path: "api/sms/clear-module", body: EmptyBody())
    }

    func esimOverview() async throws -> ESIMOverview {
        try await get(path: "api/esim")
    }

    func switchESIM(iccid: String) async throws -> ESIMSwitchResult {
        try await postDecoded(path: "api/esim/switch", body: ["iccid": iccid])
    }

    func esimHealth() async throws -> ESIMHealth {
        try await get(path: "api/esim/health")
    }

    func esimNotes() async throws -> [String: ESIMNote] {
        let response: ESIMNotesResponse = try await get(path: "api/esim/notes")
        return response.notes
    }

    func saveESIMNote(iccid: String, label: String, phone: String, tags: String) async throws {
        try await post(
            path: "api/esim/notes",
            body: ["iccid": iccid, "label": label, "phone": phone, "tags": tags]
        )
    }

    func renameESIMProfile(iccid: String, name: String) async throws {
        try await patch(path: "api/esim/profile", body: ["iccid": iccid, "name": name])
    }

    func deleteESIMProfile(iccid: String) async throws {
        try await delete(path: "api/esim/profile", body: ["iccid": iccid])
    }

    func probeESIMPhonebook() async throws -> ESIMPhonebookProbe {
        try await postDecoded(path: "api/esim/phonebook/probe", body: EmptyBody())
    }

    func networkDiagnostic() async throws -> NetworkDiagnostic {
        try await get(path: "api/network")
    }
}


// MARK: - 网络诊断与 eSIM 补充模型

struct NetworkDiagnostic: Codable, Sendable {
    let usbnetMode: String?
    let usbcfg: String?
    let pdpContexts: [PDPContext]?
    let activeContexts: [Int]?
    let pdpAddresses: [String]?
    let macInterfaces: [MacNetInterface]?
    let defaultRoute: MacDefaultRoute?
    let usbNetworkPresent: Bool
    let usbDevice: USBDeviceStatus?
    let errors: [String: String]?

    enum CodingKeys: String, CodingKey {
        case usbcfg, errors
        case usbnetMode = "usbnet_mode"
        case pdpContexts = "pdp_contexts"
        case activeContexts = "active_contexts"
        case pdpAddresses = "pdp_addresses"
        case macInterfaces = "mac_interfaces"
        case defaultRoute = "default_route"
        case usbNetworkPresent = "usb_network_present"
        case usbDevice = "usb_device"
    }
}

struct PDPContext: Codable, Sendable {
    let id: Int?
    let pdn: String?
    let apn: String?
}

struct MacNetInterface: Codable, Sendable {
    let name: String?
    let status: String?
    let ipv4: String?
    let mac: String?
    let kind: String?
}

struct MacDefaultRoute: Codable, Sendable {
    let interface: String?
    let gateway: String?
}

struct USBDeviceStatus: Codable, Sendable {
    let vendor: String?
    let product: String?
    let vendorID: String?
    let productID: String?
    let mode: String?

    enum CodingKeys: String, CodingKey {
        case vendor, product, mode
        case vendorID = "vendor_id"
        case productID = "product_id"
    }
}

struct ESIMSwitchResult: Codable, Sendable {
    let switchAccepted: Bool?
    let phase: String?
    let targetICCID: String?
    let recoveryPending: Bool?
    let moduleRebootRequested: Bool?
    let moduleRebootWarning: String?
    let reconnectWaitSeconds: Int?

    enum CodingKeys: String, CodingKey {
        case phase
        case switchAccepted = "switch_accepted"
        case targetICCID = "target_iccid"
        case recoveryPending = "recovery_pending"
        case moduleRebootRequested = "module_reboot_requested"
        case moduleRebootWarning = "module_reboot_warning"
        case reconnectWaitSeconds = "reconnect_wait_seconds"
    }
}

struct ESIMHealth: Codable, Sendable {
    let ok: Bool?
    let message: String?
    let activeProfile: ESIMProfile?
    let moduleICCID: String?
    let imsi: String?
    let operatorName: String?
    let registration: String?
    let registered: Bool?
    let signalDBM: Int?
    let networkMode: String?

    enum CodingKeys: String, CodingKey {
        case ok, message, registration, registered, imsi
        case activeProfile = "active_profile"
        case moduleICCID = "module_iccid"
        case operatorName = "operator"
        case signalDBM = "signal_dbm"
        case networkMode = "network_mode"
    }
}

struct ESIMNote: Codable, Sendable {
    let label: String?
    let phone: String?
    let tags: String?
}

struct ESIMNotesResponse: Codable, Sendable {
    let notes: [String: ESIMNote]
}

struct ESIMPhonebookProbe: Codable, Sendable {
    let storageSupported: Bool?
    let storageSelected: Bool?
    let readSupported: Bool?
    let writeSupported: Bool?
    let storageStatus: String?

    enum CodingKeys: String, CodingKey {
        case storageSupported = "storage_supported"
        case storageSelected = "storage_selected"
        case readSupported = "read_supported"
        case writeSupported = "write_supported"
        case storageStatus = "storage_status"
    }
}


extension ESIMProfile {
    /// 展示名：与网页端 profileDisplayName 一致
    var displayName: String {
        name ?? serviceProviderName ?? iccid ?? "未命名 Profile"
    }
}

struct ESIMDownloadResult: Codable, Sendable {
    let message: String?
}

extension DJOneHubAPI {
    func downloadESIMProfile(
        smdp: String,
        matchingID: String,
        confirmationCode: String,
        imei: String,
        aid: String
    ) async throws -> ESIMDownloadResult {
        try await postDecoded(
            path: "api/esim/download",
            body: [
                "smdp": smdp,
                "matching_id": matchingID,
                "confirmation_code": confirmationCode,
                "imei": imei,
                "aid": aid,
            ]
        )
    }
}
