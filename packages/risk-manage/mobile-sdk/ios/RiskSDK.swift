// RiskSDK.swift — iOS 客户端风控 SDK 参考实现。
//
// 用法：
//
//     // App 启动时
//     RiskSDK.shared.initialize(endpoint: URL(string: "https://api.example.com/v1/risk/session")!)
//
//     // checkout 页面
//     class CheckoutVC: UIViewController {
//         override func viewDidAppear(_ animated: Bool) {
//             super.viewDidAppear(animated)
//             RiskSDK.shared.attach(to: self)
//         }
//
//         func submitTapped() {
//             let sid = RiskSDK.shared.sessionID
//             // sid 塞进 PaymentIntent.metadata.risk_session_id
//             // 同时调一下 finalize 上报 behavior
//             Task { await RiskSDK.shared.finalize() }
//         }
//     }

import UIKit
import Foundation

public final class RiskSDK {
    public static let shared = RiskSDK()

    private(set) public var sessionID: String = ""
    private var endpoint: URL?
    private var startTime: Date = Date()
    private var clickTimes: [Date] = []
    private var keystrokeTimes: [Date] = []
    private var pastedFields: Set<String> = []
    private let queue = DispatchQueue(label: "risksdk.serial")

    /// 启动。endpoint 是后端 POST /v1/risk/session 的全 URL。
    /// 在 App 启动时调一次即可；后续 attach() 复用同一 sessionID。
    public func initialize(endpoint: URL) {
        self.endpoint = endpoint
        self.startTime = Date()
        Task {
            let payload = self.buildFingerprintPayload()
            self.sessionID = await self.postSession(payload) ?? ""
        }
    }

    /// 绑定到一个 view controller，开始采集 touch / 输入事件。
    /// 重复 attach 不同 vc 共享同一收集器（多个 checkout 页串起来）。
    public func attach(to viewController: UIViewController) {
        let pan = UIPanGestureRecognizer(target: self, action: #selector(onPan(_:)))
        pan.cancelsTouchesInView = false
        viewController.view.addGestureRecognizer(pan)
        let tap = UITapGestureRecognizer(target: self, action: #selector(onTap(_:)))
        tap.cancelsTouchesInView = false
        viewController.view.addGestureRecognizer(tap)
        // 文本输入需要 caller 自己在 UITextFieldDelegate 里调 RiskSDK.shared.recordKeystroke()
        // 和 RiskSDK.shared.recordPaste(fieldName:) — 这两个方法见下方。
    }

    /// UITextField 输入回调里调一次。
    public func recordKeystroke() {
        queue.async { self.keystrokeTimes.append(Date()) }
    }

    /// 用户从粘贴板贴入卡号 / CVC 时调；fieldName 用 input 的 accessibilityIdentifier 或固定 key。
    public func recordPaste(fieldName: String) {
        queue.async { self.pastedFields.insert(fieldName.lowercased()) }
    }

    /// submit 时调一次：把行为快照 PUT 到 /finalize；失败 fail-open。
    public func finalize() async {
        guard !sessionID.isEmpty, let endpoint = endpoint else { return }
        let snap = buildBehaviorPayload()
        let url = endpoint.appendingPathComponent("finalize")
        try? await postJSON(url: url, body: snap)
    }

    // MARK: - Internal

    @objc private func onTap(_ g: UITapGestureRecognizer) {
        queue.async { self.clickTimes.append(Date()) }
    }

    @objc private func onPan(_ g: UIPanGestureRecognizer) {
        // 仅记录"有 pan 事件"；entropy 在 finalize 时由间隔计算。
        queue.async { self.clickTimes.append(Date()) }
    }

    private func buildFingerprintPayload() -> [String: Any] {
        let dev = UIDevice.current
        let info = ProcessInfo.processInfo
        let screen = UIScreen.main
        let locale = Locale.current
        let tz = TimeZone.current
        let scale = screen.scale
        let nativeBounds = screen.nativeBounds
        let modelHash = persistentDeviceIdentifier()

        return [
            "sdk_version": "0.1.0",
            "platform": "ios",
            "userAgent": "RiskSDK-iOS/\(Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "?")",
            "fingerprintHash": modelHash,
            "screenWxH": "\(Int(nativeBounds.width))x\(Int(nativeBounds.height))",
            "timezone": tz.identifier,
            "language": locale.identifier,
            "hardwareConcurrency": info.activeProcessorCount,
            "osVersion": "iOS \(dev.systemVersion)",
            "appVersion": Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "",
            "deviceModel": deviceModelCode(),
            "isJailbroken": isJailbroken(),
            "isEmulator": false,
            "screenScale": scale,
        ]
    }

    private func buildBehaviorPayload() -> [String: Any] {
        let now = Date()
        let timeToCheckoutMs = Int(now.timeIntervalSince(startTime) * 1000)
        let clickIntervalMs = avgIntervalMs(clickTimes)
        let typingRhythmCV = cv(intervals: keystrokeTimes)
        return [
            "session_id": sessionID,
            "timeToCheckoutMs": timeToCheckoutMs,
            "mouseMovementEntropy": 0.0, // 移动端无鼠标
            "clickIntervalMs": clickIntervalMs,
            "scrollSpeedPxPerSec": 0.0,  // 简化版：scroll 速度 caller 自己上报
            "typingRhythmCV": typingRhythmCV,
            "keystrokeCount": keystrokeTimes.count,
            "mouseMoves": 0,
            "pastedFields": Array(pastedFields),
        ]
    }

    private func persistentDeviceIdentifier() -> String {
        // IDFV：per-app per-vendor，重装 app 会变；用作 fingerprint 兜底足够。
        // 不用 IDFA（ATT 同意要求），不存 keychain（隐私 + 合规复杂）。
        return UIDevice.current.identifierForVendor?.uuidString ?? UUID().uuidString
    }

    private func deviceModelCode() -> String {
        var sysinfo = utsname()
        uname(&sysinfo)
        let machine = withUnsafePointer(to: &sysinfo.machine) {
            $0.withMemoryRebound(to: CChar.self, capacity: 1) { String(cString: $0) }
        }
        return machine
    }

    private func isJailbroken() -> Bool {
        // 简单 heuristic：检测越狱常见路径 / Cydia URL scheme。生产应集成
        // DeviceCheck / SafetyNet 之类的 attestation。
        let suspicious = ["/Applications/Cydia.app", "/private/var/lib/apt"]
        for p in suspicious where FileManager.default.fileExists(atPath: p) { return true }
        return false
    }

    // MARK: - Network helpers

    private func postSession(_ body: [String: Any]) async -> String? {
        guard let endpoint = endpoint else { return nil }
        do {
            let resp = try await postJSON(url: endpoint, body: body)
            if let id = resp["session_id"] as? String { return id }
        } catch {}
        return nil
    }

    @discardableResult
    private func postJSON(url: URL, body: [String: Any]) async throws -> [String: Any] {
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.addValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: body)
        let (data, _) = try await URLSession.shared.data(for: req)
        if let dict = try JSONSerialization.jsonObject(with: data) as? [String: Any] { return dict }
        return [:]
    }

    // MARK: - Stats helpers

    private func avgIntervalMs(_ times: [Date]) -> Int {
        guard times.count >= 2 else { return 0 }
        let sorted = times.sorted()
        var total: TimeInterval = 0
        for i in 1..<sorted.count { total += sorted[i].timeIntervalSince(sorted[i - 1]) }
        return Int(total * 1000 / Double(sorted.count - 1))
    }

    private func cv(intervals times: [Date]) -> Double {
        guard times.count >= 3 else { return 0 }
        let sorted = times.sorted()
        var deltas: [Double] = []
        for i in 1..<sorted.count {
            deltas.append(sorted[i].timeIntervalSince(sorted[i - 1]) * 1000)
        }
        let mean = deltas.reduce(0, +) / Double(deltas.count)
        if mean == 0 { return 0 }
        let variance = deltas.map { ($0 - mean) * ($0 - mean) }.reduce(0, +) / Double(deltas.count)
        return sqrt(variance) / mean
    }
}
