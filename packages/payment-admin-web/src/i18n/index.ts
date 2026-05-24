// i18n 入口 — react-i18next + browser LanguageDetector 标准接入。
// 与 accounting-admin-web 保持同款方案，便于团队复用经验。
//
// 命名空间组织（按业务域拆分）：
//   common      通用按钮 / 标签 / 状态枚举
//   menu        左侧菜单 + 顶部导航 + 顶栏 Token Modal
//   dashboard   工作台首页
//   order       订单管理（列表 + 详情）
//   merchant    商户管理（列表 + 详情 + secrets）
//   ledger      总账
//   dispute     Dispute 管理
//   risk        风控运营全套页面
//   channel     渠道（probe / webhook test / PH tester / 出站 webhook 列表）
//   kms         密钥管理
//   ops         运营中心
//   audit       审计日志（order + user/merchant 双源）
//   app         App 下单模拟器
//
// 语言检测优先级（i18next-browser-languagedetector 默认）：
//   1. URL ?lng=
//   2. localStorage["i18nextLng"]
//   3. navigator.language
//   4. fallback zh-CN
import i18n from 'i18next';
import LanguageDetector from 'i18next-browser-languagedetector';
import { initReactI18next } from 'react-i18next';

import zhCommon from './locales/zh/common.json';
import zhMenu from './locales/zh/menu.json';
import zhDashboard from './locales/zh/dashboard.json';
import zhOrder from './locales/zh/order.json';
import zhMerchant from './locales/zh/merchant.json';
import zhLedger from './locales/zh/ledger.json';
import zhDispute from './locales/zh/dispute.json';
import zhRisk from './locales/zh/risk.json';
import zhChannel from './locales/zh/channel.json';
import zhKms from './locales/zh/kms.json';
import zhOps from './locales/zh/ops.json';
import zhAudit from './locales/zh/audit.json';
import zhApp from './locales/zh/app.json';

import enCommon from './locales/en/common.json';
import enMenu from './locales/en/menu.json';
import enDashboard from './locales/en/dashboard.json';
import enOrder from './locales/en/order.json';
import enMerchant from './locales/en/merchant.json';
import enLedger from './locales/en/ledger.json';
import enDispute from './locales/en/dispute.json';
import enRisk from './locales/en/risk.json';
import enChannel from './locales/en/channel.json';
import enKms from './locales/en/kms.json';
import enOps from './locales/en/ops.json';
import enAudit from './locales/en/audit.json';
import enApp from './locales/en/app.json';

export const SUPPORTED_LANGUAGES = [
  { code: 'zh-CN', label: '中文' },
  { code: 'en-US', label: 'English' },
] as const;

export type SupportedLanguage = (typeof SUPPORTED_LANGUAGES)[number]['code'];

i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: {
      'zh-CN': {
        common: zhCommon,
        menu: zhMenu,
        dashboard: zhDashboard,
        order: zhOrder,
        merchant: zhMerchant,
        ledger: zhLedger,
        dispute: zhDispute,
        risk: zhRisk,
        channel: zhChannel,
        kms: zhKms,
        ops: zhOps,
        audit: zhAudit,
        app: zhApp,
      },
      'en-US': {
        common: enCommon,
        menu: enMenu,
        dashboard: enDashboard,
        order: enOrder,
        merchant: enMerchant,
        ledger: enLedger,
        dispute: enDispute,
        risk: enRisk,
        channel: enChannel,
        kms: enKms,
        ops: enOps,
        audit: enAudit,
        app: enApp,
      },
    },
    fallbackLng: 'zh-CN',
    defaultNS: 'common',
    ns: [
      'common',
      'menu',
      'dashboard',
      'order',
      'merchant',
      'ledger',
      'dispute',
      'risk',
      'channel',
      'kms',
      'ops',
      'audit',
      'app',
    ],
    interpolation: { escapeValue: false }, // React 已转义
    detection: {
      order: ['querystring', 'localStorage', 'navigator'],
      lookupQuerystring: 'lng',
      lookupLocalStorage: 'i18nextLng',
      caches: ['localStorage'],
    },
  });

export default i18n;
