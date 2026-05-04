// Human-readable money formatting utilities.
//
// Backend services store monetary amounts as an int64 `storage` value where
// `storage = minor_units * 100`. For example, USD 100.34 (which is 10034 minor
// units / cents) is stored as 1003400. This module ports the semantics of the
// server-side payment-util/money package into TypeScript so the admin web UI
// can render amounts as human-readable strings like "$100.34".
//
// int64 → JSON:
//   gRPC/protojson encodes int64 fields as JSON strings by default (to avoid
//   2^53 precision loss in JavaScript). So API payloads arrive with storage
//   values like "1003400" (string) rather than 1003400 (number). The public
//   functions here accept `string | number | bigint` and convert to bigint
//   internally; callers don't need to parse the API value themselves.
//
// Rounding:
//   - `storageToMinor` is STRICT: it rejects any storage value that is not an
//     exact multiple of 100 by throwing.
//   - `storageToMinorBanker` uses banker's rounding (round half to even) on
//     the storage -> minor_units conversion. All display helpers
//     (`formatStorage`, `display`, `<Money />`) use banker's rounding because
//     UI should never crash on unexpected sub-minor-unit dust.

import React from "react";

// ISO 4217 decimal-digit table. Any currency not listed here defaults to 2.
// Source: ISO 4217 + common backend reference tables.
export const precisionMinor: Record<string, number> = {
  // Zero-decimal currencies
  BIF: 0, CLP: 0, DJF: 0, GNF: 0, ISK: 0, JPY: 0, KMF: 0, KRW: 0,
  PYG: 0, RWF: 0, UGX: 0, UYI: 0, VND: 0, VUV: 0,
  XAF: 0, XOF: 0, XPF: 0, XAG: 0, XAU: 0, XPD: 0, XPT: 0,
  XBA: 0, XBB: 0, XBC: 0, XBD: 0, XDR: 0, XSU: 0, XTS: 0, XUA: 0, XXX: 0,
  // Three-decimal currencies
  BHD: 3, IQD: 3, JOD: 3, KWD: 3, LYD: 3, OMR: 3, TND: 3,
  // Four-decimal currencies
  CLF: 4, UYW: 4,
};

// Currency symbol lookup. Values are intentionally pre-composed glyphs (e.g.
// "HK$", "₨") so the caller can simply concatenate symbol + formatted number.
export const currencySymbol: Record<string, string> = {
  USD: "$", EUR: "€", GBP: "£", JPY: "¥", CNY: "¥",
  PHP: "₱", THB: "฿", INR: "₹", VND: "₫", KRW: "₩",
  TRY: "₺", RUB: "₽", UAH: "₴", ILS: "₪", KZT: "₸",
  NGN: "₦", GHS: "₵", BDT: "৳", MNT: "₮", LAK: "₭",
  KHR: "៛", AFN: "؋", AZN: "₼", GEL: "₾", PYG: "₲", CRC: "₡",
  HKD: "HK$", SGD: "S$", AUD: "A$", CAD: "C$", TWD: "NT$",
  NZD: "NZ$", MXN: "Mex$", BRL: "R$", ARS: "AR$", CLP: "CLP$",
  COP: "COL$", UYU: "$U", DOP: "RD$", XCD: "EC$",
  PKR: "₨", NPR: "रू", MUR: "₨", SCR: "₨", LKR: "Rs",
  SEK: "kr", NOK: "kr", DKK: "kr", ISK: "kr",
  PLN: "zł", CZK: "Kč", HUF: "Ft", BGN: "лв", RON: "lei",
  MYR: "RM", IDR: "Rp", MMK: "K", BTN: "Nu.", MVR: "Rf",
  ZAR: "R", BWP: "P", ETB: "Br", KES: "KSh", TZS: "TSh", UGX: "USh",
  SAR: "﷼", AED: "د.إ", QAR: "﷼", OMR: "ر.ع.", BHD: "ب.د",
  KWD: "د.ك", JOD: "د.ا",
  XAF: "FCFA", XOF: "CFA", XPF: "₣",
  CHF: "CHF", ANG: "ƒ", AWG: "ƒ",
};

// storage = minor_units * STORAGE_SCALE.
const STORAGE_SCALE = 100;

/** The canonical input type for an int64 amount coming off a JSON API.
 *  Accepts the three representations that show up in practice:
 *    - `string`  — preferred for int64 fields (protojson default)
 *    - `number`  — safe for values < 2^53
 *    - `bigint`  — if the caller already parsed it
 */
export type StorageInput = string | number | bigint;

function normalize(code: string): string {
  return (code || "").toUpperCase();
}

/** True if the currency is recognised in either the precision table or the
 *  symbol table. Anything else is still renderable (via fallback), but callers
 *  that care about display quality can gate on this. */
export function isSupported(code: string): boolean {
  const c = normalize(code);
  if (!c) return false;
  return c in precisionMinor || c in currencySymbol;
}

/** Number of fractional digits for the given ISO 4217 currency code.
 *  Defaults to 2 for unknown codes (matches backend behaviour). */
export function precision(code: string): number {
  const c = normalize(code);
  const p = precisionMinor[c];
  return p === undefined ? 2 : p;
}

/** Display symbol for the currency. Falls back to `"<CODE> "` for unknown
 *  codes so the amount is still unambiguous (e.g. "XYZ 1.00"). */
export function symbol(code: string): string {
  const c = normalize(code);
  const s = currencySymbol[c];
  return s === undefined ? `${c} ` : s;
}

// Parse any of the three input representations into an exact BigInt. Accepts
// integer-like strings ("1003400", "-1003400"), finite integer numbers, and
// bigints. Throws on anything else so call sites that MUST be valid
// (storageToMinor) surface the problem rather than silently rounding.
function toBigInt(n: StorageInput): bigint {
  if (typeof n === "bigint") return n;
  if (typeof n === "number") {
    if (!Number.isFinite(n)) {
      throw new Error(`money: non-finite storage value ${n}`);
    }
    if (!Number.isInteger(n)) {
      throw new Error(`money: non-integer storage value ${n}`);
    }
    return BigInt(n);
  }
  // string: accept optional leading sign, then digits. Rejects "1.5", "abc",
  // scientific notation, etc. Trim to tolerate the occasional whitespace.
  const s = n.trim();
  if (!/^-?\d+$/.test(s)) {
    throw new Error(`money: non-integer storage string ${JSON.stringify(n)}`);
  }
  return BigInt(s);
}

/** STRICT conversion: storage must be an exact multiple of STORAGE_SCALE.
 *  Throws otherwise. Use this on trusted numeric paths where silently
 *  rounding sub-minor-unit dust would hide a bug. */
export function storageToMinor(storage: StorageInput, _code: string): number {
  const s = toBigInt(storage);
  const scale = BigInt(STORAGE_SCALE);
  if (s % scale !== 0n) {
    throw new Error(
      `money: storage ${s.toString()} is not a multiple of ${STORAGE_SCALE}`,
    );
  }
  return Number(s / scale);
}

/** Banker's rounding (round half to even) from storage to minor units. Never
 *  throws on sub-minor-unit dust — this is what display paths should use. */
export function storageToMinorBanker(
  storage: StorageInput,
  _code: string,
): number {
  const scale = BigInt(STORAGE_SCALE);
  const s = toBigInt(storage);
  const neg = s < 0n;
  const abs = neg ? -s : s;
  const quot = abs / scale;
  const rem = abs % scale;
  const half = scale / 2n; // STORAGE_SCALE is even (100).
  let out = quot;
  if (rem > half) {
    out = quot + 1n;
  } else if (rem === half) {
    // round half to even
    if (quot % 2n !== 0n) {
      out = quot + 1n;
    }
  }
  const signed = neg ? -out : out;
  return Number(signed);
}

/** Format a minor-units amount as "<symbol><integer>.<fraction>". The
 *  fraction is zero-padded / omitted according to the currency's precision
 *  (e.g. JPY → "¥1000", USD → "$1,000.34"). Integer part is grouped with
 *  thousands separators. */
export function formatMinor(minor: StorageInput, code: string): string {
  const digits = precision(code);
  const sym = symbol(code);
  const m = toBigInt(minor);
  const neg = m < 0n;
  const abs = neg ? -m : m;

  let intPart: bigint;
  let fracStr = "";
  if (digits === 0) {
    intPart = abs;
  } else {
    const scale = 10n ** BigInt(digits);
    intPart = abs / scale;
    const frac = abs % scale;
    fracStr = frac.toString().padStart(digits, "0");
  }

  // Group integer part with commas.
  const intStr = intPart.toString();
  const grouped = intStr.replace(/\B(?=(\d{3})+(?!\d))/g, ",");

  const body = digits === 0 ? grouped : `${grouped}.${fracStr}`;
  return `${neg ? "-" : ""}${sym}${body}`;
}

// Detect whether a value is already a "formatted major units" decimal string
// (e.g. "200.00", "-1,234.56"). accounting-system's gRPC responses run
// balances / amounts through currency.FormatAmount, so admin-web pages that
// talk to the gRPC-backed handlers receive strings like this directly rather
// than raw int64 storage. JPY / zero-decimal currencies still print without
// a dot ("100"), so this predicate intentionally favours the stricter signal
// (presence of a "."); plain-integer strings fall through to the storage path.
function looksLikeDecimalMajor(v: StorageInput): boolean {
  if (typeof v !== "string") return false;
  const s = v.trim();
  // "1,234.56" or "200.00" — allow comma grouping and optional sign
  return /^-?[\d,]+\.\d+$/.test(s);
}

// formatDecimalMajor takes an already-formatted decimal string (major units,
// e.g. "200.00") and returns "<sign><symbol><grouped>.<fraction>". Trailing
// fraction digits are left exactly as the server sent them so precision is
// preserved verbatim (never more than the currency's ISO precision in
// practice, because currency.FormatAmount uses StringFixed).
function formatDecimalMajor(v: string, code: string): string {
  const raw = v.trim().replace(/,/g, "");
  const neg = raw.startsWith("-");
  const abs = neg ? raw.slice(1) : raw;
  const [intPart, fracPart] = abs.split(".");
  const grouped = intPart.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const sym = symbol(code);
  const body = fracPart !== undefined ? `${grouped}.${fracPart}` : grouped;
  return `${neg ? "-" : ""}${sym}${body}`;
}

/** Banker's-rounded formatter. Use for untrusted / display paths. */
export function formatStorage(storage: StorageInput, code: string): string {
  if (looksLikeDecimalMajor(storage)) {
    return formatDecimalMajor(storage as string, code);
  }
  const minor = storageToMinorBanker(storage, code);
  return formatMinor(minor, code);
}

/** Parse any supported money input into BigInt minor units. Handles:
 *    - storage int (number / bigint / digit string) → divides by STORAGE_SCALE
 *      with banker's rounding on sub-minor-unit dust
 *    - formatted decimal major string like "200.00" → multiplies by 10^precision
 *      with banker's rounding if the fraction has more digits than precision
 *  Useful for client-side summation across a list where different rows may
 *  arrive in either representation (accounting-system mixes int64 HTTP
 *  responses and StringFixed gRPC responses). */
export function toMinorBigInt(input: StorageInput, code: string): bigint {
  if (looksLikeDecimalMajor(input)) {
    return decimalMajorToMinor(input as string, code);
  }
  // Storage int path: reuse the banker-rounded minor (as number is safe only
  // for < 2^53, but the result is an integer count of minor units which is
  // fine for the value ranges we handle here — UI totals, not settlement).
  return BigInt(storageToMinorBanker(input, code));
}

function decimalMajorToMinor(v: string, code: string): bigint {
  const digits = precision(code);
  const raw = v.trim().replace(/,/g, "");
  const neg = raw.startsWith("-");
  const abs = neg ? raw.slice(1) : raw;
  const [intPart, fracPart = ""] = abs.split(".");
  if (!/^\d+$/.test(intPart) || (fracPart && !/^\d+$/.test(fracPart))) {
    throw new Error(`money: not a decimal major string: ${v}`);
  }
  const scale = 10n ** BigInt(digits);
  let intBig: bigint;
  try {
    intBig = BigInt(intPart || "0") * scale;
  } catch {
    throw new Error(`money: decimal intPart parse failed: ${intPart}`);
  }
  let fracBig = 0n;
  if (digits > 0 && fracPart.length > 0) {
    if (fracPart.length <= digits) {
      fracBig = BigInt(fracPart.padEnd(digits, "0"));
    } else {
      // More fraction digits than the currency precision → banker rounding.
      const keep = fracPart.slice(0, digits);
      const restDigits = fracPart.slice(digits);
      let base = BigInt(keep);
      const half = 5n * 10n ** BigInt(restDigits.length - 1);
      const rest = BigInt(restDigits);
      if (rest > half) {
        base += 1n;
      } else if (rest === half) {
        if (base % 2n !== 0n) base += 1n;
      }
      fracBig = base;
    }
  }
  const out = intBig + fracBig;
  return neg ? -out : out;
}

/** Format a BigInt minor-unit total back to "<symbol><grouped>[.<fraction>]".
 *  Use this after summing via toMinorBigInt. */
export function formatMinorBigInt(minor: bigint, code: string): string {
  return formatMinor(minor, code);
}

/** Convenience alias: "$100.34"-style rendering of a storage value. */
export function display(storage: StorageInput, code: string): string {
  return formatStorage(storage, code);
}

export interface MoneyProps {
  /** int64 storage amount (minor_units * 100). Accepts number, bigint, or
   *  a decimal string — strings are parsed as integers (no decimal point). */
  storage: StorageInput | null | undefined;
  /** ISO 4217 currency code. Case-insensitive. */
  currency: string | null | undefined;
  /** Optional className forwarded to the wrapping <span>. */
  className?: string;
}

/** React component rendering a storage amount as a human-readable money
 *  string. Gracefully renders an em-dash for null / undefined / unparseable
 *  inputs instead of crashing the page. */
export const Money: React.FC<MoneyProps> = ({ storage, currency, className }) => {
  if (storage === null || storage === undefined || currency == null) {
    return React.createElement("span", { className }, "—");
  }
  try {
    // formatStorage handles both formatted-decimal strings and raw storage
    // ints. Don't pre-parse to BigInt — that would reject "200.00".
    return React.createElement("span", { className }, formatStorage(storage, currency));
  } catch {
    return React.createElement("span", { className }, "—");
  }
};

export default Money;
