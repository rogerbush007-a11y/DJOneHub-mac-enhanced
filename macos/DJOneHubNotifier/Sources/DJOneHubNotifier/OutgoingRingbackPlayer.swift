import AppKit
import Foundation

/// Local progress tone for outgoing calls. The modem UAC media route remains
/// closed until CLCC reports an active call, so network ringback is unavailable
/// during dialing and alerting.
@MainActor
final class OutgoingRingbackPlayer {
    private var timer: Timer?
    private var sound: NSSound?

    nonisolated static let toneData = makeToneWAV()

    func start() {
        guard timer == nil else { return }
        playTone()
        timer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated {
                self?.playTone()
            }
        }
        NSLog("DJOneHub outgoing ringback started")
    }

    func stop() {
        guard timer != nil || sound != nil else { return }
        timer?.invalidate()
        timer = nil
        sound?.stop()
        sound = nil
        NSLog("DJOneHub outgoing ringback stopped")
    }

    private func playTone() {
        sound?.stop()
        let next = NSSound(data: Self.toneData)
        next?.volume = 0.7
        next?.play()
        sound = next
    }

    nonisolated private static func makeToneWAV() -> Data {
        let sampleRate: UInt32 = 44_100
        let duration = 1.0
        let sampleCount = Int(Double(sampleRate) * duration)
        let dataSize = UInt32(sampleCount * MemoryLayout<Int16>.size)
        var data = Data()

        data.append(contentsOf: "RIFF".utf8)
        appendLittleEndian(UInt32(36) + dataSize, to: &data)
        data.append(contentsOf: "WAVEfmt ".utf8)
        appendLittleEndian(UInt32(16), to: &data)
        appendLittleEndian(UInt16(1), to: &data)
        appendLittleEndian(UInt16(1), to: &data)
        appendLittleEndian(sampleRate, to: &data)
        appendLittleEndian(sampleRate * 2, to: &data)
        appendLittleEndian(UInt16(2), to: &data)
        appendLittleEndian(UInt16(16), to: &data)
        data.append(contentsOf: "data".utf8)
        appendLittleEndian(dataSize, to: &data)

        let fadeSamples = Double(sampleRate) * 0.015
        for index in 0..<sampleCount {
            let position = Double(index)
            let remaining = Double(sampleCount - 1 - index)
            let envelope = min(1, position / fadeSamples, remaining / fadeSamples)
            let phase = 2 * Double.pi * 450 * position / Double(sampleRate)
            let sample = Int16(sin(phase) * 5_500 * max(0, envelope))
            appendLittleEndian(UInt16(bitPattern: sample), to: &data)
        }
        return data
    }

    nonisolated private static func appendLittleEndian<T: FixedWidthInteger>(_ value: T, to data: inout Data) {
        var littleEndian = value.littleEndian
        withUnsafeBytes(of: &littleEndian) { bytes in
            data.append(contentsOf: bytes)
        }
    }
}
