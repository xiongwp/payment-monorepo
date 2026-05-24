// LanguageSwitcher — 顶部导航的语言切换下拉。
//
// 用法：直接放 AppLayout 的 Header 右侧。
//   <LanguageSwitcher />
//
// 切换语言时同步：
//   1. i18n.changeLanguage(code) — 触发 react-i18next 全局重渲染
//   2. localStorage["i18nextLng"] = code（i18next-browser-languagedetector 已自动持久化）
//   3. antd ConfigProvider 通过 useLocale hook 拿当前语言 → 切换 antd 内建文案
//
// 不依赖 zustand 等额外状态库；i18n 的 language 本身就是 source of truth。
import { useTranslation } from 'react-i18next';
import { Select } from 'antd';
import { GlobalOutlined } from '@ant-design/icons';
import { SUPPORTED_LANGUAGES, type SupportedLanguage } from './index';

export default function LanguageSwitcher() {
  const { i18n, t } = useTranslation('common');

  const current = (i18n.language?.startsWith('en') ? 'en-US' : 'zh-CN') as SupportedLanguage;

  const handleChange = (value: SupportedLanguage) => {
    void i18n.changeLanguage(value);
  };

  return (
    <Select
      size="small"
      value={current}
      onChange={handleChange}
      style={{ width: 110 }}
      suffixIcon={<GlobalOutlined />}
      title={t('switchLanguage')}
      options={SUPPORTED_LANGUAGES.map((l) => ({ value: l.code, label: l.label }))}
    />
  );
}

// useAntdLocale — 根据当前 i18n 语言返回 antd 对应的 locale 对象。
// 用于 AppLayout 顶层包 ConfigProvider：
//   const antdLocale = useAntdLocale();
//   <ConfigProvider locale={antdLocale}>{children}</ConfigProvider>
import zhCN from 'antd/locale/zh_CN';
import enUS from 'antd/locale/en_US';
import type { Locale } from 'antd/locale';

export function useAntdLocale(): Locale {
  const { i18n } = useTranslation();
  return i18n.language?.startsWith('en') ? enUS : zhCN;
}
