package com.payment_platform;

import java.util.*;

/**
 * Mini JSON parser/serializer. 不依赖 Jackson/Gson, 保持 SDK 零依赖.
 *
 * 限制:
 *   - 数字统一返 Long / Double (无 BigDecimal); 高精度场景商户自己用 String
 *   - 不处理 unicode 转义 / 控制字符 (\u00xx) — 业务 99% 不需要
 *
 * 真生产建议: 升级到 Jackson/Gson, 或换 JEP 198 (JDK preview).
 */
final class MiniJSON {

    private MiniJSON() {}

    static String write(Object v) {
        StringBuilder sb = new StringBuilder();
        writeValue(sb, v);
        return sb.toString();
    }

    @SuppressWarnings("unchecked")
    static Map<String, Object> parse(String s) {
        Parser p = new Parser(s);
        Object v = p.value();
        p.skipWs();
        return (Map<String, Object>) v;
    }

    // ── write ──
    private static void writeValue(StringBuilder sb, Object v) {
        if (v == null) { sb.append("null"); return; }
        if (v instanceof String) { writeString(sb, (String) v); return; }
        if (v instanceof Boolean) { sb.append(((Boolean) v) ? "true" : "false"); return; }
        if (v instanceof Number) { sb.append(v.toString()); return; }
        if (v instanceof Map) {
            sb.append('{');
            boolean first = true;
            for (Map.Entry<?,?> e : ((Map<?,?>) v).entrySet()) {
                if (!first) sb.append(',');
                first = false;
                writeString(sb, String.valueOf(e.getKey()));
                sb.append(':');
                writeValue(sb, e.getValue());
            }
            sb.append('}');
            return;
        }
        if (v instanceof Collection) {
            sb.append('[');
            boolean first = true;
            for (Object o : (Collection<?>) v) {
                if (!first) sb.append(',');
                first = false;
                writeValue(sb, o);
            }
            sb.append(']');
            return;
        }
        // 兜底 toString
        writeString(sb, v.toString());
    }

    private static void writeString(StringBuilder sb, String s) {
        sb.append('"');
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"':  sb.append("\\\""); break;
                case '\\': sb.append("\\\\"); break;
                case '\n': sb.append("\\n");  break;
                case '\r': sb.append("\\r");  break;
                case '\t': sb.append("\\t");  break;
                default:
                    if (c < 0x20) sb.append(String.format("\\u%04x", (int) c));
                    else          sb.append(c);
            }
        }
        sb.append('"');
    }

    // ── parse ──
    private static final class Parser {
        private final String s;
        private int i;
        Parser(String s) { this.s = s; this.i = 0; }

        Object value() {
            skipWs();
            if (i >= s.length()) throw new RuntimeException("MiniJSON: empty");
            char c = s.charAt(i);
            if (c == '"') return parseString();
            if (c == '{') return parseObject();
            if (c == '[') return parseArray();
            if (c == 't' || c == 'f') return parseBool();
            if (c == 'n') { expect("null"); return null; }
            return parseNumber();
        }

        Map<String, Object> parseObject() {
            Map<String, Object> m = new LinkedHashMap<>();
            i++; // skip {
            skipWs();
            if (i < s.length() && s.charAt(i) == '}') { i++; return m; }
            while (i < s.length()) {
                skipWs();
                String key = parseString();
                skipWs();
                if (s.charAt(i) != ':') throw new RuntimeException("MiniJSON: expected ':'");
                i++;
                Object val = value();
                m.put(key, val);
                skipWs();
                char c = s.charAt(i++);
                if (c == '}') return m;
                if (c != ',') throw new RuntimeException("MiniJSON: expected ',' or '}'");
            }
            throw new RuntimeException("MiniJSON: unterminated object");
        }

        List<Object> parseArray() {
            List<Object> l = new ArrayList<>();
            i++;
            skipWs();
            if (i < s.length() && s.charAt(i) == ']') { i++; return l; }
            while (i < s.length()) {
                l.add(value());
                skipWs();
                char c = s.charAt(i++);
                if (c == ']') return l;
                if (c != ',') throw new RuntimeException("MiniJSON: expected ',' or ']'");
            }
            throw new RuntimeException("MiniJSON: unterminated array");
        }

        String parseString() {
            if (s.charAt(i) != '"') throw new RuntimeException("MiniJSON: expected '\"'");
            i++;
            StringBuilder sb = new StringBuilder();
            while (i < s.length()) {
                char c = s.charAt(i++);
                if (c == '"') return sb.toString();
                if (c == '\\') {
                    char e = s.charAt(i++);
                    switch (e) {
                        case '"':  sb.append('"');  break;
                        case '\\': sb.append('\\'); break;
                        case '/':  sb.append('/');  break;
                        case 'n':  sb.append('\n'); break;
                        case 't':  sb.append('\t'); break;
                        case 'r':  sb.append('\r'); break;
                        case 'b':  sb.append('\b'); break;
                        case 'f':  sb.append('\f'); break;
                        case 'u':  // \uXXXX
                            int cp = Integer.parseInt(s.substring(i, i + 4), 16);
                            sb.append((char) cp);
                            i += 4;
                            break;
                        default: sb.append(e);
                    }
                } else sb.append(c);
            }
            throw new RuntimeException("MiniJSON: unterminated string");
        }

        Boolean parseBool() {
            if (s.startsWith("true", i)) { i += 4; return true; }
            if (s.startsWith("false", i)) { i += 5; return false; }
            throw new RuntimeException("MiniJSON: expected boolean");
        }

        Object parseNumber() {
            int start = i;
            if (s.charAt(i) == '-') i++;
            while (i < s.length() && (Character.isDigit(s.charAt(i)) || s.charAt(i) == '.'
                    || s.charAt(i) == 'e' || s.charAt(i) == 'E' || s.charAt(i) == '+' || s.charAt(i) == '-')) {
                i++;
            }
            String num = s.substring(start, i);
            if (num.contains(".") || num.contains("e") || num.contains("E")) return Double.parseDouble(num);
            return Long.parseLong(num);
        }

        void expect(String word) {
            if (!s.startsWith(word, i)) throw new RuntimeException("MiniJSON: expected " + word);
            i += word.length();
        }

        void skipWs() {
            while (i < s.length() && Character.isWhitespace(s.charAt(i))) i++;
        }
    }
}
