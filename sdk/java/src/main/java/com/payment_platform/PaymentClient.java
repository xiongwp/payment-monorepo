package com.payment_platform;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.security.SecureRandom;
import java.time.Duration;
import java.util.HashMap;
import java.util.Map;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

/**
 * Java SDK for Payment Platform API.
 *
 * <pre>
 *   PaymentClient pay = new PaymentClient("pk_test_xxx");
 *   Map&lt;String, Object&gt; params = Map.of("amount", 1999L, "currency", "USD", "source", "tok_xxx");
 *   Map&lt;String, Object&gt; charge = pay.charges().create(params);
 *
 *   // Webhook 验签 (Spring / Spark / JAX-RS 都 OK):
 *   Map&lt;String, Object&gt; event = pay.webhooks().constructEvent(
 *       rawBody, request.getHeader("X-Webhook-Signature"),
 *       System.getenv("WEBHOOK_SECRET"), 300);
 * </pre>
 *
 * 零外部依赖 — 用 JDK 11+ 内置 java.net.http.
 */
public class PaymentClient {

    public static final String API_VERSION = "2026-05-01";
    public static final String SDK_VERSION = "0.1.0";

    private final String apiKey;
    private final String baseUrl;
    private final String mode;
    private final HttpClient http;
    private final int maxRetries;

    public final ChargesAPI charges;
    public final RefundsAPI refunds;
    public final PayoutsAPI payouts;
    public final CustomersAPI customers;
    public final WebhooksAPI webhooks;

    public PaymentClient(String apiKey) { this(apiKey, null, 30, 3); }

    public PaymentClient(String apiKey, String baseUrl, int timeoutSec, int maxRetries) {
        if (apiKey == null || apiKey.isEmpty())
            throw new IllegalArgumentException("apiKey required (pk_test_... or pk_live_...)");
        this.apiKey = apiKey;
        this.mode = apiKey.startsWith("pk_test_") ? "test"
                  : apiKey.startsWith("pk_live_") ? "live" : "live";
        this.baseUrl = (baseUrl != null) ? baseUrl
                     : ("test".equals(this.mode) ? "https://api-sandbox.payment.example.com"
                                                 : "https://api.payment.example.com");
        this.http = HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(timeoutSec))
                .build();
        this.maxRetries = maxRetries;

        this.charges = new ChargesAPI(this);
        this.refunds = new RefundsAPI(this);
        this.payouts = new PayoutsAPI(this);
        this.customers = new CustomersAPI(this);
        this.webhooks = new WebhooksAPI(this);
    }

    public String mode() { return mode; }

    /**
     * 内部 HTTP 调用. 自动: idempotency / 重试 / 4xx-pass / 5xx-retry.
     */
    Map<String, Object> request(String method, String path, Map<String, Object> body, String idempotencyKey)
            throws PaymentException {
        String url = this.baseUrl + path;
        HttpRequest.Builder rb = HttpRequest.newBuilder()
                .uri(URI.create(url))
                .timeout(Duration.ofSeconds(30))
                .header("Authorization", "Bearer " + this.apiKey)
                .header("User-Agent", "payment_platform-java/" + SDK_VERSION)
                .header("Accept", "application/json")
                .header("X-API-Version", API_VERSION);

        boolean isWrite = !"GET".equalsIgnoreCase(method) && !"HEAD".equalsIgnoreCase(method);
        if (isWrite) {
            if (idempotencyKey == null) idempotencyKey = "idem_" + randHex(16);
            rb.header("Idempotency-Key", idempotencyKey);
        }
        if (body != null) {
            rb.header("Content-Type", "application/json");
            rb.method(method.toUpperCase(), HttpRequest.BodyPublishers.ofString(MiniJSON.write(body)));
        } else {
            rb.method(method.toUpperCase(), HttpRequest.BodyPublishers.noBody());
        }
        HttpRequest req = rb.build();

        PaymentException lastErr = null;
        for (int attempt = 0; attempt < this.maxRetries; attempt++) {
            try {
                HttpResponse<String> resp = http.send(req, HttpResponse.BodyHandlers.ofString());
                int status = resp.statusCode();
                Map<String, Object> parsed = (resp.body() != null && !resp.body().isEmpty())
                        ? MiniJSON.parse(resp.body()) : new HashMap<>();
                if (status < 300) return parsed;
                // 4xx (除 429) 不重试
                if (status >= 400 && status < 500 && status != 429) {
                    throw new PaymentException(
                            (String) parsed.getOrDefault("message", "HTTP " + status),
                            status, (String) parsed.get("error"));
                }
                lastErr = new PaymentException("HTTP " + status, status, null);
            } catch (PaymentException e) {
                if (e.statusCode < 500 && e.statusCode != 429) throw e;
                lastErr = e;
            } catch (Exception e) {
                lastErr = new PaymentException("network: " + e.getMessage(), 0, null);
            }
            try { Thread.sleep(100L * (1L << attempt)); } catch (InterruptedException ie) { /* */ }
        }
        if (lastErr != null) throw lastErr;
        throw new PaymentException("unknown", 0, null);
    }

    static String randHex(int bytes) {
        byte[] b = new byte[bytes];
        new SecureRandom().nextBytes(b);
        StringBuilder sb = new StringBuilder(bytes * 2);
        for (byte x : b) sb.append(String.format("%02x", x));
        return sb.toString();
    }

    // ── 资源 API ──

    public static class ChargesAPI {
        private final PaymentClient c;
        ChargesAPI(PaymentClient c) { this.c = c; }
        public Map<String, Object> create(Map<String, Object> params) throws PaymentException {
            return c.request("POST", "/v1/charges", params, null);
        }
        public Map<String, Object> retrieve(String id) throws PaymentException {
            return c.request("GET", "/v1/charges/" + id, null, null);
        }
    }

    public static class RefundsAPI {
        private final PaymentClient c;
        RefundsAPI(PaymentClient c) { this.c = c; }
        public Map<String, Object> create(Map<String, Object> params) throws PaymentException {
            return c.request("POST", "/v1/refunds", params, null);
        }
    }

    public static class PayoutsAPI {
        private final PaymentClient c;
        PayoutsAPI(PaymentClient c) { this.c = c; }
        public Map<String, Object> retrieve(String id) throws PaymentException {
            return c.request("GET", "/v1/payouts/" + id, null, null);
        }
    }

    public static class CustomersAPI {
        private final PaymentClient c;
        CustomersAPI(PaymentClient c) { this.c = c; }
        public Map<String, Object> create(Map<String, Object> params) throws PaymentException {
            return c.request("POST", "/v1/customers", params, null);
        }
    }

    public static class WebhooksAPI {
        private final PaymentClient c;
        WebhooksAPI(PaymentClient c) { this.c = c; }

        /**
         * Verify webhook signature and parse event (Stripe-style).
         *
         * @param rawBody         raw request body (NOT parsed JSON)
         * @param sigHeader       X-Webhook-Signature header value
         * @param endpointSecret  secret from merchant dashboard
         * @param toleranceSec    timestamp tolerance window, default 300
         */
        public Map<String, Object> constructEvent(String rawBody, String sigHeader,
                                                  String endpointSecret, int toleranceSec)
                throws PaymentException {
            long timestamp = 0;
            java.util.List<String> sigs = new java.util.ArrayList<>();
            for (String part : sigHeader.split(",")) {
                part = part.trim();
                if (part.startsWith("t=")) {
                    try { timestamp = Long.parseLong(part.substring(2)); }
                    catch (NumberFormatException e) { throw new PaymentException("bad timestamp", 0, null); }
                } else if (part.startsWith("v1=")) {
                    sigs.add(part.substring(3));
                }
            }
            if (timestamp == 0 || sigs.isEmpty())
                throw new PaymentException("malformed signature header", 0, null);

            long delta = System.currentTimeMillis() / 1000 - timestamp;
            if (Math.abs(delta) > toleranceSec)
                throw new PaymentException("timestamp out of tolerance (delta=" + delta + "s)", 0, null);

            String expected;
            try {
                Mac mac = Mac.getInstance("HmacSHA256");
                mac.init(new SecretKeySpec(endpointSecret.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
                byte[] hash = mac.doFinal((timestamp + "." + rawBody).getBytes(StandardCharsets.UTF_8));
                StringBuilder sb = new StringBuilder();
                for (byte b : hash) sb.append(String.format("%02x", b));
                expected = sb.toString();
            } catch (Exception e) {
                throw new PaymentException("hmac error: " + e.getMessage(), 0, null);
            }
            boolean ok = false;
            for (String s : sigs) {
                if (constantTimeEq(s, expected)) { ok = true; break; }
            }
            if (!ok) throw new PaymentException("signature mismatch", 0, null);
            return MiniJSON.parse(rawBody);
        }

        private static boolean constantTimeEq(String a, String b) {
            if (a.length() != b.length()) return false;
            int r = 0;
            for (int i = 0; i < a.length(); i++) r |= a.charAt(i) ^ b.charAt(i);
            return r == 0;
        }
    }
}
