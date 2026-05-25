// i18n 入口 — react-i18next + browser LanguageDetector 标准接入。
//
// 命名空间组织（避免单 JSON 膨胀）：
//   common      通用按钮 / 标签 / 状态枚举
//   menu        左侧菜单 + 顶部导航
//   account     账户管理 / 平台账户
//   transaction 交易流水
//   rotation    fleet × rotation 管理
//   snapshot    余额快照
//   tcc         TCC 监控
//   daycut      日切管理
//   trial       试算平衡
//   config      系统配置 / 缓冲 / 热账户
//   business    业务类型管理
//   redis       Redis 重建
//   instances   服务实例
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
import zhAccount from './locales/zh/account.json';
import zhTransaction from './locales/zh/transaction.json';
import zhRotation from './locales/zh/rotation.json';
import zhSnapshot from './locales/zh/snapshot.json';
import zhTcc from './locales/zh/tcc.json';
import zhDaycut from './locales/zh/daycut.json';
import zhTrial from './locales/zh/trial.json';
import zhConfig from './locales/zh/config.json';
import zhBusiness from './locales/zh/business.json';
import zhRedis from './locales/zh/redis.json';
import zhInstances from './locales/zh/instances.json';

import enCommon from './locales/en/common.json';
import enMenu from './locales/en/menu.json';
import enAccount from './locales/en/account.json';
import enTransaction from './locales/en/transaction.json';
import enRotation from './locales/en/rotation.json';
import enSnapshot from './locales/en/snapshot.json';
import enTcc from './locales/en/tcc.json';
import enDaycut from './locales/en/daycut.json';
import enTrial from './locales/en/trial.json';
import enConfig from './locales/en/config.json';
import enBusiness from './locales/en/business.json';
import enRedis from './locales/en/redis.json';
import enInstances from './locales/en/instances.json';

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
        account: zhAccount,
        transaction: zhTransaction,
        rotation: zhRotation,
        snapshot: zhSnapshot,
        tcc: zhTcc,
        daycut: zhDaycut,
        trial: zhTrial,
        config: zhConfig,
        business: zhBusiness,
        redis: zhRedis,
        instances: zhInstances,
      },
      'en-US': {
        common: enCommon,
        menu: enMenu,
        account: enAccount,
        transaction: enTransaction,
        rotation: enRotation,
        snapshot: enSnapshot,
        tcc: enTcc,
        daycut: enDaycut,
        trial: enTrial,
        config: enConfig,
        business: enBusiness,
        redis: enRedis,
        instances: enInstances,
      },
    },
    fallbackLng: 'zh-CN',
    defaultNS: 'common',
    ns: [
      'common',
      'menu',
      'account',
      'transaction',
      'rotation',
      'snapshot',
      'tcc',
      'daycut',
      'trial',
      'config',
      'business',
      'redis',
      'instances',
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
