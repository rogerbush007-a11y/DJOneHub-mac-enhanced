import Foundation
import Combine

/// 原生主窗口的数据中心：轮询后台 /api/calls/status，驱动拨号、通话与最近通话界面。
@MainActor
final class CallCenter: ObservableObject {
    @Published var activeCall: CallRecord?
    @Published var history: [CallRecord] = []
    @Published var isOnline = false
    @Published var lastError: String?
    @Published var lastActionError: String?
    @Published var isMuted = false
    @Published var isRecording = false
    @Published var numberInput = ""
    @Published var isDialing = false

    private let api: DJOneHubAPI
    var apiClient: DJOneHubAPI { api }
    private var pollTask: Task<Void, Never>?
    private var lastActiveID: String?
    private var pollInFlight = false
    private let maVoAudio = VoiceAudioService()
    private var maVoAudioStarting = false
    private var maVoAudioCallID: String?
    private var maVoHostRegistered = false

    /// 新来电（呼入且响铃/等待）时回调，用于弹窗/聚焦主窗口。
    var onIncoming: ((CallRecord) -> Void)?

    init(api: DJOneHubAPI) {
        self.api = api
    }

    func start() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.pollOnce()
                let interval: UInt64
                if !(self?.isOnline ?? false) {
                    interval = 5_000_000_000
                } else if self?.activeCall != nil {
                    interval = 1_000_000_000
                } else {
                    interval = 3_000_000_000
                }
                try? await Task.sleep(nanoseconds: interval)
            }
        }
    }

    func stop() {
        pollTask?.cancel()
        pollTask = nil
        maVoAudio.stop()
        maVoHostRegistered = false
        Task { _ = try? await api.setMaVoAudioHostEnabled(false) }
    }

    func refreshCalls() async {
        await pollOnce()
    }

    private func pollOnce() async {
        guard !pollInFlight else { return }
        pollInFlight = true
        defer { pollInFlight = false }
        do {
            let status = try await api.callStatus()
            isOnline = true
            if !maVoHostRegistered {
                do {
                    try await api.setMaVoAudioHostEnabled(true)
                    maVoHostRegistered = true
                } catch {
                    // The backend can take tens of seconds to enumerate USB
                    // after a restart. Keep the old route until registration
                    // succeeds, then retry on the next poll.
                    lastError = error.localizedDescription
                }
            }
            lastError = status.lastPollError
            let previousID = activeCall?.id
            activeCall = status.active
            history = status.history ?? []

            if status.active == nil, previousID != nil {
                if isRecording { maVoAudio.stopRecording { _ in } }
                isRecording = false
                maVoAudio.stop()
                maVoAudioCallID = nil
                maVoAudioStarting = false
            }
            if status.active == nil, previousID != nil, isRecording {
                _ = try? await api.setCallRecording(false)
                isRecording = false
            }

            if let active = status.active,
               active.direction == "incoming",
               active.state == "incoming" || active.state == "waiting",
               active.id != lastActiveID {
                lastActiveID = active.id
                onIncoming?(active)
            } else if status.active == nil {
                lastActiveID = nil
            }
            if status.active?.id != previousID, status.active == nil || previousID == nil {
                isMuted = false
            }
            if let active = status.active, active.state == "active" {
                startMaVoAudioIfNeeded(for: active)
            }
        } catch {
            isOnline = false
            lastError = error.localizedDescription
        }
    }

    func dial() {
        let number = numberInput.trimmingCharacters(in: .whitespaces)
        guard !number.isEmpty else { return }
        isDialing = true
        Task {
            do {
                try await api.dial(number: number)
                await MainActor.run {
                    self.isDialing = false
                    self.lastActionError = nil
                }
            } catch {
                await MainActor.run {
                    self.isDialing = false
                    self.lastActionError = "拨号失败：\(error.localizedDescription)"
                }
            }
        }
    }

    func answer() {
        Task {
            do {
                try await api.answerCall()
                lastActionError = nil
            } catch {
                lastActionError = "接听失败：\(error.localizedDescription)"
            }
        }
    }

    func reject() {
        Task {
            do {
                _ = try await api.rejectCall()
                lastActionError = nil
            } catch {
                lastActionError = "拒接失败：\(error.localizedDescription)"
            }
        }
    }

    func hangup() {
        isMuted = false
        if isRecording { maVoAudio.stopRecording { _ in } }
        isRecording = false
        maVoAudio.stop()
        Task {
            do {
                try await api.hangupCall()
                lastActionError = nil
            } catch {
                lastActionError = "挂断失败：\(error.localizedDescription)"
            }
        }
    }

    func sendDTMF(_ digit: String) {
        Task { _ = try? await api.sendDTMF(digit: digit) }
    }

    func toggleMute() {
        let muted = !isMuted
        isMuted = muted
        maVoAudio.setMuted(muted)
    }

    func toggleRecording() {
        guard maVoAudio.isRunning else {
            lastActionError = "通话音频尚未连接，无法开始录音。"
            return
        }
        if isRecording {
            maVoAudio.stopRecording { [weak self] _ in self?.isRecording = false }
            return
        }
        do {
            _ = try maVoAudio.startRecording()
            isRecording = true
            lastActionError = nil
        } catch {
            lastActionError = "无法创建录音：\(error.localizedDescription)"
        }
    }

    private func startMaVoAudioIfNeeded(for call: CallRecord) {
        guard !maVoAudio.isRunning, !maVoAudioStarting, maVoAudioCallID != call.id else { return }
        maVoAudioStarting = true
        maVoAudioCallID = call.id
        // Keep MaVo's ordering: acquire microphone permission before creating
        // the engine or opening the UAC media bridge. AVAudioEngine startup is
        // intentionally all-or-nothing in the upstream service.
        maVoAudio.requestMicrophoneAccess { [weak self] granted in
            guard let self else { return }
            guard granted else {
                self.maVoAudioStarting = false
                self.maVoAudioCallID = nil
                self.lastActionError = "需要麦克风权限才能进行双向通话。"
                return
            }
            Task {
                do {
                    let config = try await self.waitForMaVoRoute()
                    self.maVoAudio.startUAC(
                        vendorID: config.vendorID,
                        productID: config.productID,
                        matchingLocationID: config.locationID
                    ) { [weak self] result in
                        guard let self else { return }
                        self.maVoAudioStarting = false
                        switch result {
                        case .success:
                            self.maVoAudio.setMediaEnabled(true)
                            self.lastActionError = nil
                        case .failure(let message):
                            self.lastActionError = "MaVo 音频启动失败：\(message)"
                            self.maVoAudioCallID = nil
                        }
                    }
                } catch {
                    self.maVoAudioStarting = false
                    self.maVoAudioCallID = nil
                    self.lastActionError = "MaVo 音频准备失败：\(error.localizedDescription)"
                }
            }
        }
    }

    private func waitForMaVoRoute() async throws -> MaVoAudioHostConfig {
        let deadline = Date().addingTimeInterval(40)
        while Date() < deadline {
            let config = try await api.maVoAudioHostConfig()
            if config.routeReady {
                return config
            }
            if let routeError = config.routeError, !routeError.isEmpty {
                throw APIError.http(409, "模块语音路由准备失败：\(routeError)")
            }
            try await Task.sleep(for: .milliseconds(500))
        }
        throw APIError.http(409, "等待模块 UAC 语音路由超过 40 秒。请在设置中查看初始化诊断。")
    }

    func callDuration(now: Date = Date()) -> TimeInterval {
        guard let call = activeCall else { return 0 }
        let end = call.endedAt ?? now
        return max(0, end.timeIntervalSince(call.startedAt))
    }

    func dialNumber(_ number: String) {
        numberInput = number
        dial()
    }
}
