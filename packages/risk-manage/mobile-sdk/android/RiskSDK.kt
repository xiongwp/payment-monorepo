// RiskSDK.kt — Android 客户端风控 SDK 参考实现。
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
import android.os.Build
import android.provider.Settings
import android.view.MotionEvent
import android.view.View
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.GlobalScope
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL
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

    val sessionId: String
        get() = sessionRef.get()

    /**
     * 启动；endpoint 是后端 POST /v1/risk/session 全 URL。
     * 在 Application.onCreate 调一次即可。失败 fail-open（sessionId 留空）。
     */
    fun initialize(ctx: Context, endpoint: String) {
        this.endpoint = endpoint
        this.startTime = System.currentTimeMillis()
        val payload = buildFingerprintPayload(ctx)
        GlobalScope.launch(Dispatchers.IO) {
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
        obj.put("sdk_version", "0.1.0")
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
        obj.put("isEmulator", isEmulator())
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

    private fun isRooted(): Boolean {
        // simple su / Magisk 路径检测；生产配 SafetyNet attestation
        val suspicious = listOf(
            "/system/bin/su", "/system/xbin/su",
            "/system/app/Superuser.apk",
            "/sbin/su"
        )
        return suspicious.any { java.io.File(it).exists() }
    }

    private fun isEmulator(): Boolean {
        val fp = Build.FINGERPRINT.lowercase()
        return fp.contains("generic") || fp.contains("vbox") || fp.contains("emulator") ||
                Build.MODEL.contains("sdk", true) || Build.PRODUCT.contains("sdk", true)
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
