/**
 * API 快速测试脚本
 * 在浏览器控制台或 Node 环境中调用，验证后端连通性
 *
 * 使用方式（浏览器 console）:
 *   import { runAllTests } from './src/api/test'
 *   runAllTests()
 */

import {
  createAccount,
  createTransaction,
  doubleEntryBooking,
  getAccount,
  getBalanceSnapshot,
  healthCheck,
  retryTransaction,
} from './accounting';
import {
  AccountBusinessType,
  AccountCategory,
  AccountType,
  BusinessType,
  PartyType,
} from '../types/accounting';

const log = (tag: string, data: unknown) =>
  console.log(`[${tag}]`, JSON.stringify(data, null, 2));

const err = (tag: string, e: unknown) =>
  console.error(`[${tag} FAILED]`, e instanceof Error ? e.message : e);

// ── TC01: 健康检查 ────────────────────────────────────────────────────────────
async function tc01_health() {
  try {
    const res = await healthCheck();
    log('TC01 health', res);
  } catch (e) { err('TC01 health', e); }
}

// ── TC02: 创建用户账户 ────────────────────────────────────────────────────────
async function tc02_createUserAccount(): Promise<string | null> {
  try {
    const acc = await createAccount({
      user_id: 10001,
      account_type: AccountType.USER,
      category: AccountCategory.ASSET,
      account_business_type: AccountBusinessType.USER_BALANCE,
      currency: 'PHP',
    });
    log('TC02 createUserAccount', acc);
    return acc.account_no;
  } catch (e) { err('TC02 createUserAccount', e); return null; }
}

// ── TC03: 创建平台账户 ────────────────────────────────────────────────────────
async function tc03_createPlatformAccount(): Promise<string | null> {
  try {
    const acc = await createAccount({
      user_id: 0,
      account_type: AccountType.PLATFORM,
      category: AccountCategory.EQUITY,
      account_business_type: AccountBusinessType.USER_BALANCE,
      currency: 'PHP',
    });
    log('TC03 createPlatformAccount', acc);
    return acc.account_no;
  } catch (e) { err('TC03 createPlatformAccount', e); return null; }
}

// ── TC04: 创建商户账户 ────────────────────────────────────────────────────────
async function tc04_createMerchantAccount(): Promise<string | null> {
  try {
    const acc = await createAccount({
      user_id: 20001,
      account_type: AccountType.MERCHANT,
      category: AccountCategory.ASSET,
      account_business_type: AccountBusinessType.MERCHANT_BALANCE,
      currency: 'PHP',
    });
    log('TC04 createMerchantAccount', acc);
    return acc.account_no;
  } catch (e) { err('TC04 createMerchantAccount', e); return null; }
}

// ── TC05: 查询账户 ────────────────────────────────────────────────────────────
async function tc05_getAccount(accountNo: string) {
  try {
    const acc = await getAccount(accountNo);
    log('TC05 getAccount', acc);
  } catch (e) { err('TC05 getAccount', e); }
}

// ── TC06: 用户充值（复式记账） ────────────────────────────────────────────────
async function tc06_deposit(userAccountNo: string, platformAccountNo: string) {
  try {
    const result = await doubleEntryBooking({
      business_no: `DEP_${Date.now()}`,
      business_type: BusinessType.DEPOSIT,
      currency: 'PHP',
      description: '用户充值 500 元',
      entries: [
        { account_no: userAccountNo,     debit_amount: '500.00', description: '充值借方' },
        { account_no: platformAccountNo, credit_amount: '500.00', description: '充值贷方' },
      ],
    });
    log('TC06 deposit', result);
  } catch (e) { err('TC06 deposit', e); }
}

// ── TC07: 用户提现 ────────────────────────────────────────────────────────────
async function tc07_withdraw(userAccountNo: string, platformAccountNo: string) {
  try {
    const result = await doubleEntryBooking({
      business_no: `WD_${Date.now()}`,
      business_type: BusinessType.WITHDRAW,
      currency: 'PHP',
      entries: [
        { account_no: platformAccountNo, debit_amount: '80.00' },
        { account_no: userAccountNo,    credit_amount: '80.00' },
      ],
    });
    log('TC07 withdraw', result);
  } catch (e) { err('TC07 withdraw', e); }
}

// ── TC08: 用户支付商户（Transaction API，自动查规则）──────────────────────────
async function tc08_payment() {
  try {
    const result = await createTransaction({
      order_no: `PAY_${Date.now()}`,
      product_code: 'PAYMENT',
      event_code: 'CHECKOUT_PAY',
      from_party_id: 10001,
      from_party_type: PartyType.USER,
      to_party_id: 20001,
      to_party_type: PartyType.MERCHANT,
      amount: '200.00',
      currency: 'PHP',
    });
    log('TC08 payment', result);
    return result.order_no;
  } catch (e) { err('TC08 payment', e); return null; }
}

// ── TC09: 幂等测试（相同 order_no 重复提交）──────────────────────────────────
async function tc09_idempotency(orderNo: string) {
  try {
    const r1 = await createTransaction({
      order_no: orderNo,
      product_code: 'DEPOSIT',
      event_code: 'USER_DEPOSIT',
      from_party_id: 0,
      from_party_type: PartyType.PLATFORM,
      to_party_id: 10001,
      to_party_type: PartyType.USER,
      amount: '100.00',
    });
    const r2 = await createTransaction({
      order_no: orderNo,   // 相同订单号
      product_code: 'DEPOSIT',
      event_code: 'USER_DEPOSIT',
      from_party_id: 0,
      from_party_type: PartyType.PLATFORM,
      to_party_id: 10001,
      to_party_type: PartyType.USER,
      amount: '100.00',
    });
    const same = r1.voucher_no === r2.voucher_no;
    log('TC09 idempotency', { r1, r2, voucher_same: same });
  } catch (e) { err('TC09 idempotency', e); }
}

// ── TC10: 重试失败订单 ────────────────────────────────────────────────────────
async function tc10_retry(orderNo: string) {
  try {
    const result = await retryTransaction(orderNo);
    log('TC10 retry', result);
  } catch (e) { err('TC10 retry', e); }
}

// ── TC11: 查询余额快照 ────────────────────────────────────────────────────────
async function tc11_snapshot(accountNo: string) {
  try {
    const snap = await getBalanceSnapshot(accountNo);
    log('TC11 snapshot', snap);
  } catch (e) { err('TC11 snapshot', e); }
}

// ─── 主测试入口 ───────────────────────────────────────────────────────────────

export async function runAllTests() {
  console.log('====== Accounting API Tests ======');

  await tc01_health();

  const userAccNo      = await tc02_createUserAccount();
  const platformAccNo  = await tc03_createPlatformAccount();
  const merchantAccNo  = await tc04_createMerchantAccount();

  if (userAccNo)     await tc05_getAccount(userAccNo);
  if (userAccNo && platformAccNo) {
    await tc06_deposit(userAccNo, platformAccNo);
    await tc07_withdraw(userAccNo, platformAccNo);
  }

  await tc08_payment();

  const idemOrderNo = `IDEM_${Date.now()}`;
  await tc09_idempotency(idemOrderNo);

  // TC10: 用一个已有订单测试重试（实际测试时替换为 FAILED 状态的订单号）
  // await tc10_retry('your-failed-order-no');

  if (userAccNo) await tc11_snapshot(userAccNo);

  console.log('====== Tests Done ======');
}
