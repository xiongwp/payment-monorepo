// RiskSDK.kt — Android 客户端风控 SDK 参考实现。
//
// v0.3 attestation + 反 hook 检测：
//   - Play Integrity API：requestIntegrityToken(nonce) → integrityToken；
//     服务端用 Google Play Developer API（decode） 验出 deviceIntegrity /
//     appIntegrity / accountDetails。
//   - Root 检测：10 个独立信号（路径 / busybox / magisk / supersu APKs /
//     test-keys / TracerPid）
//   - 模拟器检测：FINGERPRINT 子串 + 传感器缺失 + null SIM
//   - Frida / Xposed：/proc/self/maps 扫 frida-agent / 安装包名扫 LSPosed
//
// 用法：
//
//     // Application.onCreate
//     RiskSDK.initialize(this, endpoint = "https://api.example.com/v1/risk/session")
//
//     // CheckoutActivity
//     class CheckoutActivity : AppCompatActivity() {
//         override fun onResume() {
//             super.onResume()
//             RiskSDK.attach(this)
//         }
//
//         private fun onSubmit() {
//             val sid = RiskSDK.sessionId
//             // sid 塞进 PaymentIntent.metadata.risk_session_id
//             lifecycleScope.launch { RiskSDK.finalize() }
//         }
//     }
//
// 文本输入场景：caller 自己在 EditText 上挂 TextWatcher 调
// RiskSDK.recordKeystroke()；从粘贴板贴卡号时调 RiskSDK.recordPaste(fieldName)。

package com.example.risksdk

import android.app.Activity
import android.app.Application
import android.content.Context
import android.content.pm.PackageManager
import android.hardware.Sensor
import android.hardware.SensorManager
import android.os.Build
import android.provider.Settings
import android.telephony.TelephonyManager
import android.view.MotionEvent
import android.view.View
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.GlobalScope
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.io.BufferedReader
import java.io.File
import java.io.FileReader
import java.net.HttpURLConnection
import java.net.URL
import java.security.MessageDigest
import java.util.Locale
import java.util.TimeZone
import java.util.concurrent.atomic.AtomicReference
import kotlin.math.sqrt

object RiskSDK {

    private val sessionRef = AtomicReference("")
    private var endpoint: String = ""
    private var startTime = System.currentTimeMillis()

    private val clickTimes = mutableListOf<Long>()
    private val keystrokeTimes = mutableListOf<Long>()
    private val pastedFields = mutableSetOf<String>()
    private val lock = Object()

    // attestation cache：Play Integrity token（initialize 时拉一次）
    @Volatile private var playIntegrityToken: String = ""

    val sessionId: String
        get() = sessionRef.get()

    /**
     * 启动；endpoint 是后端 POST /v1/risk/session 全 URL。
     * 在 Application.onCreate 调一次即可。失败 fail-open（sessionId 留空）。
     */
    fun initialize(ctx: Context, endpoint: String) {
        this.endpoint = endpoint
        this.startTime = System.currentTimeMillis()
        // 异步拉 Play Integrity token；初始化 fingerprint payload 时 token 为空也无所谓
        // （finalize 时 attestation 字段会再带一次）。
        GlobalScope.launch(Dispatchers.IO) {
            refreshPlayIntegrity(ctx)
            val payload = buildFingerprintPayload(ctx)
            val id = postSession(payload)
            sessionRef.set(id)
        }
    }

    /**
     * 绑定到 Activity：拦截 dispatchTouchEvent 收集触摸事件。
     * Activity 通过自定义 BaseActivity 转发或重写 dispatchTouchEvent 调
     * RiskSDK.recordTouch(ev) 即可。
     */
    fun attach(activity: Activity) {
        val root: View = activity.window.decorView
        root.setOnTouchListener { _, ev -> recordTouch(ev); false }
    }

    fun recordTouch(ev: MotionEvent) {
        if (ev.action == MotionEvent.ACTION_DOWN) {
            synchronized(lock) { clickTimes.add(System.currentTimeMillis()) }
        }
    }

    fun recordKeystroke() {
        synchronized(lock) { keystrokeTimes.add(System.currentTimeMillis()) }
    }

    fun recordPaste(fieldName: String) {
        synchronized(lock) { pastedFields.add(fieldName.lowercase()) }
    }

    /** Submit 时调；上报 behavior 快照。失败 fail-open。 */
    suspend fun finalize() {
        val sid = sessionRef.get()
        if (sid.isEmpty() || endpoint.isEmpty()) return
        val body = buildBehaviorPayload(sid)
        withContext(Dispatchers.IO) {
            postJSON("$endpoint/finalize", body)
        }
    }

    // ─── payload builders ──────────────────────────────────────────

    private fun buildFingerprintPayload(ctx: Context): JSONObject {
        val obj = JSONObject()
        obj.put("sdk_version", "0.3.0")
        obj.put("platform", "android")
        obj.put("userAgent", "RiskSDK-Android/${appVersion(ctx)}")
        obj.put("fingerprintHash", persistentDeviceIdentifier(ctx))
        obj.put("screenWxH", "${ctx.resources.displayMetrics.widthPixels}x${ctx.resources.displayMetrics.heightPixels}")
        obj.put("timezone", TimeZone.getDefault().id)
        obj.put("language", Locale.getDefault().toString())
        obj.put("hardwareConcurrency", Runtime.getRuntime().availableProcessors())
        obj.put("osVersion", "Android ${Build.VERSION.RELEASE}")
        obj.put("appVersion", appVersion(ctx))
        obj.put("deviceModel", "${Build.MANUFACTURER} ${Build.MODEL}")
        obj.put("isJailbroken", isRooted())
        obj.put("isEmulator", isEmulator(ctx))
        obj.put("fridaDetected", isFridaPresent())
        obj.put("xposedDetected", isXposedPresent(ctx))
        obj.put("debuggerAttached", isDebuggerAttached())
        if (playIntegrityToken.isNotEmpty()) {
            obj.put("playIntegrityToken", playIntegrityToken)
            obj.put("attestationKind", "play_integrity")
        }
        return obj
    }

    private fun buildBehaviorPayload(sid: String): JSONObject {
        synchronized(lock) {
            val now = System.currentTimeMillis()
            val timeToCheckoutMs = now - startTime
            val clickIntervalMs = avgIntervalMs(clickTimes)
            val typingRhythmCV = cv(keystrokeTimes)
            val obj = JSONObject()
            obj.put("session_id", sid)
            obj.put("timeToCheckoutMs", timeToCheckoutMs)
            obj.put("mouseMovementEntropy", 0.0) // 移动端无鼠标
            obj.put("clickIntervalMs", clickIntervalMs)
            obj.put("scrollSpeedPxPerSec", 0.0)
            obj.put("typingRhythmCV", typingRhythmCV)
            obj.put("keystrokeCount", keystrokeTimes.size)
            obj.put("mouseMoves", 0)
            val pasted = JSONArray()
            pastedFields.forEach { pasted.put(it) }
            obj.put("pastedFields", pasted)
            // attestation 再带一次给 finalize（防 fingerprint 上报时 token 还没回来）
            if (playIntegrityToken.isNotEmpty()) {
                obj.put("playIntegrityToken", playIntegrityToken)
                obj.put("attestationKind", "play_integrity")
            }
            return obj
        }
    }

    @Suppress("DEPRECATION")
    private fun persistentDeviceIdentifier(ctx: Context): String {
        // ANDROID_ID 在 Android 8+ 是 per-app + per-user，重装会变；用作
        // fingerprint 兜底足够。不用 IMEI / MAC（高敏感 + Google Play 限制）。
        return Settings.Secure.getString(ctx.contentResolver, Settings.Secure.ANDROID_ID) ?: ""
    }

    private fun appVersion(ctx: Context): String =
        try {
            ctx.packageManager.getPackageInfo(ctx.packageName, 0).versionName ?: "0"
        } catch (_: Exception) {
            "0"
        }

    // ─── Root detection (10 signals) ───────────────────────────────

    /** Root 检测：10 个独立 OR 信号；任一命中即认为 root（fail-loud）。 */
    private fun isRooted(): Boolean {
        // 1-7: 常见 su / busybox / magisk 二进制路径
        val suPaths = listOf(
            "/system/bin/su",
            "/system/xbin/su",
            "/sbin/su",
            "/system/app/Superuser.apk",
            "/system/xbin/busybox",
            "/data/local/xbin/su",
            "/data/local/bin/su",
        )
        if (suPaths.any { File(it).exists() }) return true
        // 8: Magisk / SuperSU package
        if (File("/sbin/.magisk").exists() || File("/data/adb/magisk").exists()) return true
        // 9: test-keys（开发版镜像 / 自签 ROM 标志）
        if (Build.TAGS?.contains("test-keys") == true) return true
        // 10: /proc/self/status TracerPid != 0（被 ptrace 附加 ≈ debug/hook）
        if (readTracerPid() > 0) return true
        return false
    }

    private fun readTracerPid(): Int {
        return try {
            BufferedReader(FileReader("/proc/self/status")).use { br ->
                var line: String?
                while (br.readLine().also { line = it } != null) {
                    val l = line ?: continue
                    if (l.startsWith("TracerPid:")) {
                        return l.substringAfter(":").trim().toIntOrNull() ?: 0
                    }
                }
                0
            }
        } catch (_: Exception) { 0 }
    }

    // ─── Emulator detection ────────────────────────────────────────

    private fun isEmulator(ctx: Context): Boolean {
        // 1) FINGERPRINT / MODEL / PRODUCT / MANUFACTURER 子串
        val fp = (Build.FINGERPRINT ?: "").lowercase()
        val model = (Build.MODEL ?: "").lowercase()
        val manuf = (Build.MANUFACTURER ?: "").lowercase()
        val product = (Build.PRODUCT ?: "").lowercase()
        val brand = (Build.BRAND ?: "").lowercase()
        val needles = listOf(
            "generic", "vbox", "sdk", "emulator", "google_sdk", "test-keys",
            "genymotion", "andy", "droid4x", "bluestacks", "nox", "memu", "ldplayer"
        )
        if (needles.any { fp.contains(it) || model.contains(it) || product.contains(it) ||
                          manuf.contains(it) || brand.contains(it) }) {
            return true
        }
        // 2) 传感器缺失（真机一般至少有 accelerometer + gyroscope）
        try {
            val sm = ctx.getSystemService(Context.SENSOR_SERVICE) as? SensorManager
            val accel = sm?.getDefaultSensor(Sensor.TYPE_ACCELEROMETER)
            if (accel == null) return true
        } catch (_: Exception) {}
        // 3) SIM / network operator 全空（多数模拟器没 telephony 实现）
        try {
            val tm = ctx.getSystemService(Context.TELEPHONY_SERVICE) as? TelephonyManager
            if (tm != null) {
                val op = tm.networkOperatorName ?: ""
                val sim = tm.simOperatorName ?: ""
                if (op.equals("android", ignoreCase = true) && sim.isEmpty()) {
                    return true
                }
            }
        } catch (_: Exception) {}
        return false
    }

    // ─── Frida / Xposed detection ──────────────────────────────────

    /** Frida 注入会把 frida-agent.so / libfrida-gadget.so map 进 /proc/self/maps。 */
    private fun isFridaPresent(): Boolean {
        // 扫 /proc/self/maps（仅本进程内存映射；无需 root 权限）
        try {
            BufferedReader(FileReader("/proc/self/maps")).use { br ->
                var line: String?
                while (br.readLine().also { line = it } != null) {
                    val l = (line ?: continue).lowercase()
                    if (l.contains("frida") || l.contains("gum-js-loop") ||
                        l.contains("gmain") || l.contains("linjector")) {
                        return true
                    }
                }
            }
        } catch (_: Exception) {}
        // 扫常见 Frida server 端口 27042 listen（fast probe）
        try {
            val sock = java.net.Socket()
            sock.connect(java.net.InetSocketAddress("127.0.0.1", 27042), 100)
            sock.close()
            return true
        } catch (_: Exception) {}
        return false
    }

    /** Xposed / LSPosed / Magisk Manager 安装包扫描。 */
    private fun isXposedPresent(ctx: Context): Boolean {
        val suspicious = listOf(
            "de.robv.android.xposed.installer",
            "io.github.lsposed.manager",
            "org.lsposed.manager",
            "com.topjohnwu.magisk",
            "eu.chainfire.supersu",
            "com.koushikdutta.superuser",
        )
        val pm = ctx.packageManager
        for (pkg in suspicious) {
            try {
                pm.getPackageInfo(pkg, 0)
                return true
            } catch (_: PackageManager.NameNotFoundException) {
                continue
            } catch (_: Exception) {
                continue
            }
        }
        // Xposed 注入痕迹：classloader 含 XposedBridge
        try {
            Class.forName("de.robv.android.xposed.XposedBridge")
            return true
        } catch (_: ClassNotFoundException) {}
        return false
    }

    /** 调试器附加：TracerPid != 0（同 isRooted 的复用，独立暴露给规则）。 */
    private fun isDebuggerAttached(): Boolean = readTracerPid() > 0

    // ─── Play Integrity API ────────────────────────────────────────
    //
    // 真接入需要在 build.gradle 里加：
    //   implementation 'com.google.android.play:integrity:1.4.0'
    // 然后：
    //   val integrityManager = IntegrityManagerFactory.create(context)
    //   integrityManager.requestIntegrityToken(
    //       IntegrityTokenRequest.builder().setNonce(serverNonce).build()
    //   ).addOnSuccessListener { resp -> token = resp.token() }
    //
    // 这里走 stub 反射：编译期不强依赖 Play Integrity SDK，运行时若没集成依
    // 然能编（token 留空，后端见空 token 走 fail-soft）。

    private fun refreshPlayIntegrity(ctx: Context) {
        // 1) 拿 server nonce：真接入应先 POST 后端 /v1/risk/attestation/nonce
        //    拿 16 byte random base64（防重放）；此处用本地随机演示。
        val nonce = ByteArray(16).also { java.security.SecureRandom().nextBytes(it) }
        val b64Nonce = android.util.Base64.encodeToString(nonce, android.util.Base64.NO_WRAP or android.util.Base64.URL_SAFE)
        // 2) 反射调 IntegrityManagerFactory.create() — 没集成 SDK 时静默失败
        try {
            val factoryCls = Class.forName("com.google.android.play.core.integrity.IntegrityManagerFactory")
            val mgr = factoryCls.getMethod("create", Context::class.java).invoke(null, ctx)
            val requestBuilderCls = Class.forName("com.google.android.play.core.integrity.IntegrityTokenRequest")
            val builderMethod = requestBuilderCls.getMethod("builder")
            val builder = builderMethod.invoke(null)
            val setNonce = builder.javaClass.getMethod("setNonce", String::class.java)
            setNonce.invoke(builder, b64Nonce)
            val buildMethod = builder.javaClass.getMethod("build")
            val request = buildMethod.invoke(builder)
            val requestTokenMethod = mgr.javaClass.getMethod("requestIntegrityToken", requestBuilderCls)
            // Task<IntegrityTokenResponse>：用反射拿同步结果（生产用真 SDK 时建议 addOnSuccessListener）
            val task = requestTokenMethod.invoke(mgr, request)
            // 等任务完成
            val tasksCls = Class.forName("com.google.android.gms.tasks.Tasks")
            val awaitMethod = tasksCls.getMethod("await", Class.forName("com.google.android.gms.tasks.Task"))
            val resp = awaitMethod.invoke(null, task)
            val tokenMethod = resp.javaClass.getMethod("token")
            val token = tokenMethod.invoke(resp) as? String
            if (!token.isNullOrEmpty()) {
                playIntegrityToken = token
            }
        } catch (_: Throwable) {
            // SDK 未集成 / device 不支持 / 网络失败：留空，服务端 fail-soft
        }
    }

    // ─── network ──────────────────────────────────────────────────

    private fun postSession(body: JSONObject): String {
        val resp = postJSON(endpoint, body) ?: return ""
        return resp.optString("session_id", "")
    }

    private fun postJSON(url: String, body: JSONObject): JSONObject? = try {
        val u = URL(url)
        val conn = u.openConnection() as HttpURLConnection
        conn.requestMethod = "POST"
        conn.doOutput = true
        conn.setRequestProperty("Content-Type", "application/json")
        conn.connectTimeout = 5_000
        conn.readTimeout = 5_000
        conn.outputStream.bufferedWriter(Charsets.UTF_8).use { it.write(body.toString()) }
        if (conn.responseCode in 200..299) {
            JSONObject(conn.inputStream.bufferedReader(Charsets.UTF_8).readText())
        } else null
    } catch (_: Exception) {
        null
    }

    // ─── stats ────────────────────────────────────────────────────

    private fun avgIntervalMs(times: List<Long>): Int {
        if (times.size < 2) return 0
        val sorted = times.sorted()
        var total = 0L
        for (i in 1 until sorted.size) total += sorted[i] - sorted[i - 1]
        return (total / (sorted.size - 1)).toInt()
    }

    private fun cv(times: List<Long>): Double {
        if (times.size < 3) return 0.0
        val sorted = times.sorted()
        val deltas = mutableListOf<Double>()
        for (i in 1 until sorted.size) deltas.add((sorted[i] - sorted[i - 1]).toDouble())
        val mean = deltas.average()
        if (mean == 0.0) return 0.0
        val variance = deltas.sumOf { (it - mean) * (it - mean) } / deltas.size
        return sqrt(variance) / mean
    }
}
