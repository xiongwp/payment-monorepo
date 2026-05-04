// TS port of backend payment-util/money.
//
// Storage convention: backend int64 `storage` = minor_units * 100.
//   USD 100.34 -> minor=10034 -> storage=1003400.
//
// This module provides:
//   - ISO 4217 precision table (digits after decimal point)
//   - Symbol table for common/known currency codes
//   - Strict and banker-rounded storage->minor converters
//   - Human-readable string helpers (formatMinor/formatStorage/display)
//   - A convenience <Money/> component
//
// Banker's rounding = round half to even. Ties (…50 exactly) round toward
// the nearest even integer; non-tie halves round as usual. Negatives flip
// sign for the rounding computation and re-negate the result.
import * as React from "react";

// ISO 4217 precision exceptions (digits of minor units).
// Anything not listed defaults to 2.
export const precisionMinor: Record<string, number> = {
  // 0-digit
  BIF: 0, CLP: 0, DJF: 0, GNF: 0, ISK: 0, JPY: 0, KMF: 0, KRW: 0,
  PYG: 0, RWF: 0, UGX: 0, UYI: 0, VND: 0, VUV: 0, XAF: 0, XOF: 0,
  XPF: 0, XAG: 0, XAU: 0, XPD: 0, XPT: 0, XBA: 0, XBB: 0, XBC: 0,
  XBD: 0, XDR: 0, XSU: 0, XTS: 0, XUA: 0, XXX: 0,
  // 3-digit
  BHD: 3, IQD: 3, JOD: 3, KWD: 3, LYD: 3, OMR: 3, TND: 3,
  // 4-digit
  CLF: 4, UYW: 4,
};

// Commonly-used display symbols. Unlisted codes fall back to `code + " "`.
export const currencySymbol: Record<string, string> = {
  USD: "$", EUR: "€", GBP: "£", JPY: "¥", CNY: "¥", PHP: "₱", THB: "฿",
  INR: "₹", VND: "₫", KRW: "₩", TRY: "₺", RUB: "₽", UAH: "₴", ILS: "₪",
  KZT: "₸", NGN: "₦", GHS: "₵", BDT: "৳", MNT: "₮", LAK: "₭", KHR: "៛",
  AFN: "؋", AZN: "₼", GEL: "₾", PYG: "₲", CRC: "₡",
  HKD: "HK$", SGD: "S$", AUD: "A$", CAD: "C$", TWD: "NT$", NZD: "NZ$",
  MXN: "Mex$", BRL: "R$", ARS: "AR$", CLP: "CLP$", COP: "COL$", UYU: "$U",
  DOP: "RD$", XCD: "EC$",
  PKR: "₨", NPR: "रू", MUR: "₨", SCR: "₨", LKR: "Rs",
  SEK: "kr", NOK: "kr", DKK: "kr", ISK: "kr",
  PLN: "zł", CZK: "Kč", HUF: "Ft", BGN: "лв", RON: "lei",
  MYR: "RM", IDR: "Rp", MMK: "K", BTN: "Nu.", MVR: "Rf",
  ZAR: "R", BWP: "P", ETB: "Br", KES: "KSh", TZS: "TSh", UGX: "USh",
  SAR: "﷼", AED: "د.إ", QAR: "﷼", OMR: "ر.ع.", BHD: "ب.د", KWD: "د.ك",
  JOD: "د.ا", XAF: "FCFA", XOF: "CFA", XPF: "₣",
  CHF: "CHF", ANG: "ƒ", AWG: "ƒ",
};

const DEFAULT_PRECISION = 2;

/** Returns true if we know a precision for this ISO 4217 code. */
export function isSupported(code: string): boolean {
  if (!code) return false;
  if (Object.prototype.hasOwnProperty.call(precisionMinor, code)) return true;
  // Unlisted but well-formed 3-letter code defaults to 2 digits.
  return /^[A-Z]{3}$/.test(code);
}

/** Digits after the decimal for a given currency. Throws on unknown codes. */
export function precision(code: string): number {
  if (!isSupported(code)) {
    throw new Error(`money: unsupported currency code ${JSON.stringify(code)}`);
  }
  if (Object.prototype.hasOwnProperty.call(precisionMinor, code)) {
    return precisionMinor[code];
  }
  return DEFAULT_PRECISION;
}

/** Display symbol. Falls back to `${code} ` for currencies we don't have a glyph for. */
export function symbol(code: string): string {
  if (code && Object.prototype.hasOwnProperty.call(currencySymbol, code)) {
    return currencySymbol[code];
  }
  return (code || "") + " ";
}

/**
 * STRICT storage->minor. Expects storage to be an exact multiple of 100;
 * throws otherwise. Use this when you want a loud failure for any
 * sub-minor-unit residue.
 */
export function storageToMinor(storage: number, code: string): number {
  // code is validated (and used to surface misuse).
  precision(code);
  if (!Number.isFinite(storage)) {
    throw new Error(`money: storage is not finite: ${storage}`);
  }
  if (storage % 100 !== 0) {
    throw new Error(
      `money: storage ${storage} is not a whole number of minor units (residue ${storage % 100})`,
    );
  }
  return storage / 100;
}

/**
 * Banker-rounded storage->minor. Divides storage by 100 and rounds half
 * to even.
 *   1003460 -> 10035
 *   1003450 -> 10034  (tie, 10034 is even)
 *   1003550 -> 10036  (tie, 10036 is even)
 * Negatives: flip sign, round, flip back.
 */
export function storageToMinorBanker(storage: number, code: string): number {
  precision(code);
  if (!Number.isFinite(storage)) {
    throw new Error(`money: storage is not finite: ${storage}`);
  }
  const neg = storage < 0;
  const abs = neg ? -storage : storage;
  const q = Math.trunc(abs / 100);
  const r = abs - q * 100;
  let rounded: number;
  if (r < 50) {
    rounded = q;
  } else if (r > 50) {
    rounded = q + 1;
  } else {
    // exact tie at 50 -> round half to even
    rounded = q % 2 === 0 ? q : q + 1;
  }
  return neg ? -rounded : rounded;
}

/** Format a minor-unit integer as a decimal string using the currency's precision. */
export function formatMinor(minor: number, code: string): string {
  const p = precision(code);
  if (!Number.isFinite(minor)) {
    throw new Error(`money: minor is not finite: ${minor}`);
  }
  const neg = minor < 0;
  const abs = neg ? -minor : minor;
  if (p === 0) {
    return (neg ? "-" : "") + String(Math.trunc(abs));
  }
  const s = String(Math.trunc(abs)).padStart(p + 1, "0");
  const whole = s.slice(0, s.length - p);
  const frac = s.slice(s.length - p);
  return (neg ? "-" : "") + whole + "." + frac;
}

/** Format a storage integer as decimal string via banker's rounding. */
export function formatStorage(storage: number, code: string): string {
  return formatMinor(storageToMinorBanker(storage, code), code);
}

/** Full human-readable display: symbol + formatted amount. */
export function display(storage: number, code: string): string {
  return symbol(code) + formatStorage(storage, code);
}

/** Convenience React component: <Money storage={…} currency={…} />. */
export interface MoneyProps {
  storage: number;
  currency: string;
}
export function Money(props: MoneyProps): React.ReactElement {
  return React.createElement(React.Fragment, null, display(props.storage, props.currency));
}
