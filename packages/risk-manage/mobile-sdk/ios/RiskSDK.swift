// RiskSDK.swift — iOS 客户端风控 SDK 参考实现。
//
// v0.3 attestation + 反 hook 检测：
//   - DeviceCheck（iOS 11+）：generateToken → 服务端用 Apple Server-to-Server API
//     验，可附 2 bit per-device 持久标记。
//   - App Attest（iOS 14+）：generateKey + attestKey → 服务端验 X.509 链 +
//     CBOR attestation object，是 DeviceCheck 的强化版，能验证"运行中的二进制
//     真的是 Apple 签名的合法 app + 装在合法设备上"。
//   - 越狱深度检测：8 个信号（路径 / sandbox escape / cydia:// URL scheme / dyld）
//   - 模拟器检测：targetEnvironment(simulator) + utsname machine
//   - Frida/debugger：扫 dyld 镜像名 + ptrace 自检
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
#if canImport(DeviceCheck)
import DeviceCheck
#endif
#if canImport(MachO)
import MachO
#endif
import Darwin

public final class RiskSDK {
    public static let shared = RiskSDK()

    private(set) public var sessionID: String = ""
    private var endpoint: URL?
    private var startTime: Date = Date()
    private var clickTimes: [Date] = []
    private var keystrokeTimes: [Date] = []
    private var pastedFields: Set<String> = []
    private let queue = DispatchQueue(label: "risksdk.serial")

    // attestation tokens cached after initialize → finalize 时 piggyback 上报
    private var deviceCheckToken: String = ""
    private var appAttestKeyID: String = ""
    private var appAttestObject: String = ""   // base64 attestation object
    private var appAttestAssertion: String = "" // 后续启动用 assertion

    // App Attest keyID 持久化用的 Keychain key
    private let kAppAttestKeyIDStorageKey = "com.risksdk.appattest.keyId"

    /// 启动。endpoint 是后端 POST /v1/risk/session 的全 URL。
    /// 在 App 启动时调一次即可；后续 attach() 复用同一 sessionID。
    public func initialize(endpoint: URL) {
        self.endpoint = endpoint
        self.startTime = Date()
        Task {
            // 先尝试拿 attestation token（异步，失败不阻塞 fingerprint 上报）
            await self.refreshAttestation()
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

        var payload: [String: Any] = [
            "sdk_version": "0.3.0",
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
            "isEmulator": isSimulator(),
            "screenScale": scale,
            "fridaDetected": isFridaPresent(),
            "debuggerAttached": isDebuggerAttached(),
        ]
        // attestation 字段拍平上报（直接匹配 Snapshot json tag）
        if !deviceCheckToken.isEmpty {
            payload["deviceCheckToken"] = deviceCheckToken
            payload["attestationKind"] = "device_check"
        }
        if !appAttestKeyID.isEmpty && !appAttestObject.isEmpty {
            payload["appAttestKeyId"] = appAttestKeyID
            payload["appAttestToken"] = appAttestObject
            payload["attestationKind"] = "app_attest" // App Attest 比 DeviceCheck 强，覆盖 kind
        }
        return payload
    }

    private func buildBehaviorPayload() -> [String: Any] {
        let now = Date()
        let timeToCheckoutMs = Int(now.timeIntervalSince(startTime) * 1000)
        let clickIntervalMs = avgIntervalMs(clickTimes)
        let typingRhythmCV = cv(intervals: keystrokeTimes)
        var payload: [String: Any] = [
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
        // App Attest assertion 在 finalize 时上报（每次提交新算一次更安全）
        if !appAttestAssertion.isEmpty {
            payload["appAttestAssertion"] = appAttestAssertion
            payload["appAttestKeyId"] = appAttestKeyID
        }
        return payload
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

    // MARK: - Jailbreak / Emulator / Anti-hook detection

    /// 越狱检测：8 个独立信号取 OR。任一命中即认为越狱（fail-loud；
    /// 误报由后端 attestation 二次校验，不直接在客户端拒交易）。
    private func isJailbroken() -> Bool {
        // 1-6: 常见越狱后存在的文件 / 目录路径
        let suspicious = [
            "/Applications/Cydia.app",
            "/Library/MobileSubstrate/MobileSubstrate.dylib",
            "/bin/bash",
            "/usr/sbin/sshd",
            "/etc/apt",
            "/private/var/lib/apt/",
        ]
        for p in suspicious where FileManager.default.fileExists(atPath: p) {
            return true
        }
        // 7: sandbox escape — 尝试 fopen 写沙盒外文件
        let escapePath = "/private/jailbreak.txt"
        if let fp = fopen(escapePath, "w") {
            fclose(fp)
            try? FileManager.default.removeItem(atPath: escapePath)
            return true
        }
        // 8: cydia:// URL scheme（越狱设备装 Cydia 后系统会注册）
        if let url = URL(string: "cydia://package/com.example.package"),
           UIApplication.shared.canOpenURL(url) {
            return true
        }
        return false
    }

    /// 模拟器检测：编译宏 + 设备型号字符串。
    private func isSimulator() -> Bool {
        #if targetEnvironment(simulator)
        return true
        #else
        // 真机型号都形如 "iPhone14,3" / "iPad13,1"；x86_64 / arm64 裸名 = simulator
        let m = deviceModelCode()
        return m == "x86_64" || m == "i386" || m == "arm64"
        #endif
    }

    /// Frida / FridaGadget 检测：扫 dyld 加载镜像名。Frida 注入会产生：
    ///   - /usr/lib/frida/frida-agent.dylib
    ///   - FridaGadget / libfrida-gadget.dylib（self-injected）
    private func isFridaPresent() -> Bool {
        let count = _dyld_image_count()
        for i in 0..<count {
            guard let raw = _dyld_get_image_name(i) else { continue }
            let name = String(cString: raw).lowercased()
            if name.contains("frida") || name.contains("cycript") || name.contains("substrate") {
                return true
            }
        }
        return false
    }

    /// debugger / ptrace 附加检测：sysctl P_TRACED flag。
    /// 注意：这是被动检测；主动 ptrace(DENY_ATTACH) 反调试由 caller 在二进制
    /// 入口决定（影响 Xcode 调试体验，不建议 SDK 自己开）。
    private func isDebuggerAttached() -> Bool {
        var info = kinfo_proc()
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, getpid()]
        var size = MemoryLayout<kinfo_proc>.stride
        let rc = sysctl(&mib, UInt32(mib.count), &info, &size, nil, 0)
        if rc != 0 { return false }
        return (info.kp_proc.p_flag & P_TRACED) != 0
    }

    // MARK: - Attestation

    /// 刷新 DeviceCheck + App Attest token。失败仅 log，不影响 init 流程。
    /// 真接入需在 Apple Developer 后台启用 DeviceCheck capability + .p8 私钥。
    private func refreshAttestation() async {
        await refreshDeviceCheckToken()
        await refreshAppAttest()
    }

    /// DeviceCheck（iOS 11+）：每次启动 generateToken → base64 字符串。
    /// 服务端拿这 token POST 到 https://api.development.devicecheck.apple.com/v1/validate_device_token
    /// 配 .p8 私钥的 JWT。验通过的设备可写 2 bit per-device 持久标记。
    private func refreshDeviceCheckToken() async {
        #if canImport(DeviceCheck)
        if #available(iOS 11.0, *) {
            guard DCDevice.current.isSupported else { return }
            await withCheckedContinuation { (cont: CheckedContinuation<Void, Never>) in
                DCDevice.current.generateToken { token, err in
                    if let t = token, err == nil {
                        self.deviceCheckToken = t.base64EncodedString()
                    }
                    cont.resume()
                }
            }
        }
        #endif
    }

    /// App Attest（iOS 14+）：首次启动 generateKey，把 keyId 存 Keychain；
    /// 服务端发 challenge → 算 clientDataHash → attestKey 返回 attestation object（CBOR）。
    /// 后续启动用 generateAssertion(对 nonce + body) 替代 attestation（attestation 一辈子一次）。
    private func refreshAppAttest() async {
        #if canImport(DeviceCheck)
        if #available(iOS 14.0, *) {
            let service = DCAppAttestService.shared
            guard service.isSupported else { return }
            // 1. 从 Keychain 取已有 keyId；没有则 generateKey 一个
            let keyID: String
            if let cached = readKeychainKeyID() {
                keyID = cached
            } else {
                do {
                    let newKey: String = try await withCheckedThrowingContinuation { cont in
                        service.generateKey { kid, err in
                            if let kid = kid { cont.resume(returning: kid) }
                            else { cont.resume(throwing: err ?? NSError(domain: "appattest", code: -1)) }
                        }
                    }
                    writeKeychainKeyID(newKey)
                    keyID = newKey
                } catch {
                    return
                }
            }
            self.appAttestKeyID = keyID
            // 2. 拿后端 challenge（POST /attestation/challenge → 16 字节 nonce）。
            //    TODO: 真接入时调后端拿 server nonce；此处用本地随机演示。
            let challenge = Data((0..<16).map { _ in UInt8.random(in: 0...255) })
            let clientDataHash = sha256(challenge)
            // 3. attestKey 一次（同一 keyId 整个 app 生命周期只做一次 attest）
            //    后端验通过后客户端可以转为只发 assertion。
            if appAttestObject.isEmpty {
                if let att = try? await attestKey(service: service, keyID: keyID, hash: clientDataHash) {
                    self.appAttestObject = att.base64EncodedString()
                }
            }
            // 4. generateAssertion 每次启动 / 每次 finalize 都算
            if let assertion = try? await generateAssertion(service: service, keyID: keyID, hash: clientDataHash) {
                self.appAttestAssertion = assertion.base64EncodedString()
            }
        }
        #endif
    }

    #if canImport(DeviceCheck)
    @available(iOS 14.0, *)
    private func attestKey(service: DCAppAttestService, keyID: String, hash: Data) async throws -> Data {
        return try await withCheckedThrowingContinuation { cont in
            service.attestKey(keyID, clientDataHash: hash) { att, err in
                if let att = att { cont.resume(returning: att) }
                else { cont.resume(throwing: err ?? NSError(domain: "appattest.attestKey", code: -1)) }
            }
        }
    }

    @available(iOS 14.0, *)
    private func generateAssertion(service: DCAppAttestService, keyID: String, hash: Data) async throws -> Data {
        return try await withCheckedThrowingContinuation { cont in
            service.generateAssertion(keyID, clientDataHash: hash) { a, err in
                if let a = a { cont.resume(returning: a) }
                else { cont.resume(throwing: err ?? NSError(domain: "appattest.assert", code: -1)) }
            }
        }
    }
    #endif

    private func sha256(_ data: Data) -> Data {
        var digest = [UInt8](repeating: 0, count: 32)
        data.withUnsafeBytes { buf in
            CC_SHA256_RiskSDK(buf.baseAddress, CC_LONG(data.count), &digest)
        }
        return Data(digest)
    }

    // MARK: - Keychain helpers for App Attest keyId

    private func readKeychainKeyID() -> String? {
        let q: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrAccount as String: kAppAttestKeyIDStorageKey,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var item: CFTypeRef?
        if SecItemCopyMatching(q as CFDictionary, &item) == errSecSuccess,
           let data = item as? Data,
           let s = String(data: data, encoding: .utf8) {
            return s
        }
        return nil
    }

    private func writeKeychainKeyID(_ s: String) {
        let q: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrAccount as String: kAppAttestKeyIDStorageKey,
            kSecValueData as String: s.data(using: .utf8) ?? Data(),
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        SecItemDelete(q as CFDictionary)
        SecItemAdd(q as CFDictionary, nil)
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

// SHA256 桥接：避免引入 CryptoKit 编译条件复杂化，直接调 CommonCrypto C 函数。
// 通过 @_silgen_name 把符号映射，避免 import CommonCrypto 的 module map 问题。
@_silgen_name("CC_SHA256")
private func CC_SHA256_RiskSDK(_ data: UnsafeRawPointer?, _ len: CC_LONG, _ md: UnsafeMutablePointer<UInt8>?) -> UnsafeMutablePointer<UInt8>?

private typealias CC_LONG = UInt32
