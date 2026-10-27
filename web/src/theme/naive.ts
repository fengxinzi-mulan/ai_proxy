/* Naive UI 主题覆盖。
 *
 * 与 tokens.css 是同一套值：这里负责组件内部（Naive 用 JS 计算样式，
 * 认不了 CSS 变量），CSS 变量负责自定义元素。改配色时两处一起改。
 *
 * 亮暗两套分别给出：暗色不是简单反色，边框和表面要单独调，
 * 否则 Naive 默认的暗色会比自绘卡片更黑，出现「两种暗」。
 */

import type { GlobalThemeOverrides } from 'naive-ui'

const blue = {
  primary: '#2563eb',
  primaryHover: '#1d4ed8',
  primaryPressed: '#1e40af',
  primarySuppl: '#2563eb',
}

const blueDark = {
  primary: '#4f8cff',
  primaryHover: '#6b9dff',
  primaryPressed: '#3b7ae8',
  primarySuppl: '#4f8cff',
}

export function themeOverrides(dark: boolean): GlobalThemeOverrides {
  const c = dark
    ? {
        ...blueDark,
        bg: '#0f1115',
        surface: '#16191f',
        surface2: '#1a1e25',
        hover: '#1f242c',
        border: '#262b33',
        borderStrong: '#343b45',
        text: '#e6e8ec',
        text2: '#a4acba',
        text3: '#6f7887',
        success: '#34d399',
        warning: '#fbbf24',
        danger: '#f87171',
        info: '#2dd4bf',
      }
    : {
        ...blue,
        bg: '#f5f6f8',
        surface: '#ffffff',
        surface2: '#fafbfc',
        hover: '#f2f4f7',
        border: '#e4e7ec',
        borderStrong: '#d0d5dd',
        text: '#101828',
        text2: '#475467',
        text3: '#8a94a6',
        success: '#16a34a',
        warning: '#d97706',
        danger: '#dc2626',
        info: '#0e9384',
      }

  return {
    common: {
      primaryColor: c.primary,
      primaryColorHover: c.primaryHover,
      primaryColorPressed: c.primaryPressed,
      primaryColorSuppl: c.primarySuppl,
      successColor: c.success,
      successColorHover: c.success,
      successColorPressed: c.success,
      warningColor: c.warning,
      warningColorHover: c.warning,
      warningColorPressed: c.warning,
      errorColor: c.danger,
      errorColorHover: c.danger,
      errorColorPressed: c.danger,
      infoColor: c.info,
      infoColorHover: c.info,
      infoColorPressed: c.info,

      bodyColor: c.bg,
      cardColor: c.surface,
      modalColor: c.surface,
      popoverColor: c.surface,
      tableColor: c.surface,
      tableHeaderColor: c.surface2,
      inputColor: c.surface,
      actionColor: c.surface2,

      textColorBase: c.text,
      textColor1: c.text,
      textColor2: c.text2,
      textColor3: c.text3,
      textColorDisabled: c.text3,
      placeholderColor: c.text3,
      borderColor: c.border,
      dividerColor: c.border,
      hoverColor: c.hover,

      borderRadius: '8px',
      borderRadiusSmall: '6px',
      fontSize: '13px',
      fontSizeMini: '11px',
      fontSizeTiny: '12px',
      fontSizeSmall: '12px',
      fontSizeMedium: '13px',
      fontSizeLarge: '14px',
      fontSizeHuge: '16px',
      fontFamily:
        "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', 'Noto Sans SC', sans-serif",
      fontFamilyMono: "ui-monospace, SFMono-Regular, 'JetBrains Mono', Menlo, Consolas, monospace",
      heightMini: '24px',
      heightTiny: '26px',
      heightSmall: '30px',
      heightMedium: '34px',
      heightLarge: '38px',
      heightHuge: '42px',
      boxShadow1: '0 1px 2px rgba(16, 24, 40, 0.06)',
      boxShadow2: '0 4px 12px rgba(16, 24, 40, 0.10)',
      boxShadow3: '0 8px 24px rgba(16, 24, 40, 0.14)',
    },

    Layout: {
      color: c.bg,
      siderColor: c.surface,
      headerColor: c.surface,
      siderBorderColor: c.border,
      headerBorderColor: c.border,
      siderToggleButtonColor: c.surface2,
      siderToggleButtonBorder: `1px solid ${c.border}`,
    },
    Menu: {
      color: 'transparent',
      itemColorHover: c.hover,
      itemColorActive: c.primary + '14',
      itemColorActiveHover: c.primary + '1f',
      itemColorActiveCollapsed: c.primary + '14',
      itemTextColor: c.text2,
      itemTextColorHover: c.text,
      itemTextColorActive: c.primary,
      itemTextColorActiveHover: c.primary,
      itemIconColor: c.text3,
      itemIconColorHover: c.text2,
      itemIconColorActive: c.primary,
      itemIconColorActiveHover: c.primary,
      itemBorderRadius: '6px',
      itemHeight: '36px',
      itemMargin: '2px 8px',
      groupTextColor: c.text3,
    },
    Card: {
      color: c.surface,
      borderColor: c.border,
      borderRadius: '8px',
      titleFontSizeMedium: '13px',
      titleFontWeight: '600',
      paddingMedium: '16px',
    },
    DataTable: {
      thColor: c.surface2,
      thTextColor: c.text2,
      thFontWeight: '500',
      thBorderColor: c.border,
      tdColor: c.surface,
      tdColorHover: c.hover,
      tdColorStriped: c.surface2,
      tdTextColor: c.text,
      borderColor: c.border,
      borderRadius: '8px',
      fontSizeSmall: '12px',
      fontSizeMedium: '13px',
    },
    Input: {
      color: c.surface,
      colorFocus: c.surface,
      border: `1px solid ${c.border}`,
      borderHover: `1px solid ${c.borderStrong}`,
      borderFocus: `1px solid ${c.primary}`,
      boxShadowFocus: `0 0 0 2px ${c.primary}26`,
      textColor: c.text,
      placeholderColor: c.text3,
      caretColor: c.primary,
      borderRadius: '6px',
    },
    InputNumber: {
      peers: {
        Input: {
          color: c.surface,
          border: `1px solid ${c.border}`,
          borderHover: `1px solid ${c.borderStrong}`,
          borderFocus: `1px solid ${c.primary}`,
          boxShadowFocus: `0 0 0 2px ${c.primary}26`,
          textColor: c.text,
          borderRadius: '6px',
        },
      },
    },
    Select: {
      peers: {
        InternalSelection: {
          color: c.surface,
          border: `1px solid ${c.border}`,
          borderHover: `1px solid ${c.borderStrong}`,
          borderActive: `1px solid ${c.primary}`,
          borderFocus: `1px solid ${c.primary}`,
          boxShadowActive: `0 0 0 2px ${c.primary}26`,
          boxShadowFocus: `0 0 0 2px ${c.primary}26`,
          textColor: c.text,
          placeholderColor: c.text3,
          borderRadius: '6px',
        },
        InternalSelectMenu: {
          color: c.surface,
          optionTextColor: c.text,
          optionTextColorActive: c.primary,
          optionColorPending: c.hover,
          optionColorActive: c.primary + '14',
          borderRadius: '8px',
          optionHeightMedium: '32px',
        },
      },
    },
    Button: {
      borderRadiusMedium: '6px',
      borderRadiusSmall: '6px',
      fontSizeMedium: '13px',
      fontSizeSmall: '12px',
      fontWeightStrong: '500',
      textColor: c.text,
      textColorHover: c.text,
      border: `1px solid ${c.border}`,
      borderHover: `1px solid ${c.borderStrong}`,
      borderPressed: `1px solid ${c.borderStrong}`,
      color: c.surface,
      colorHover: c.hover,
      colorPressed: c.surface2,
      colorFocus: c.surface,
    },
    Tag: {
      borderRadius: '4px',
      fontSizeSmall: '11px',
      fontSizeMedium: '12px',
      heightSmall: '20px',
      heightMedium: '24px',
    },
    Drawer: {
      color: c.surface,
      borderRadius: '0',
      titleFontSize: '16px',
      titleFontWeight: '600',
    },
    Dialog: {
      color: c.surface,
      borderRadius: '10px',
      titleFontSize: '16px',
    },
    Message: {
      color: c.surface,
      borderRadius: '8px',
      boxShadow: '0 8px 24px rgba(16, 24, 40, 0.14)',
    },
    Notification: {
      color: c.surface,
      borderRadius: '10px',
    },
    Tooltip: {
      color: dark ? '#242a33' : '#101828',
      textColor: '#ffffff',
      borderRadius: '6px',
      padding: '6px 10px',
    },
    // 不要再单独覆盖 Popover.color：Tooltip 内部复用 Popover，全局 Popover
    // 覆盖的优先级高于 Tooltip 自带颜色，会把气泡底盖成 surface（浅色下白底白字）。
    // Popover 自身的底色由 common.popoverColor 提供，这里无需重复设置。
    Tabs: {
      tabFontWeightActive: '600',
      tabFontSizeMedium: '13px',
      tabTextColorLine: c.text2,
      tabTextColorActiveLine: c.text,
      tabTextColorHoverLine: c.text,
      barColor: c.primary,
    },
    Progress: {
      railColor: c.border,
      fillColor: c.primary,
    },
    Skeleton: {
      color: c.surface2,
      colorEnd: c.hover,
    },
    Empty: {
      textColor: c.text3,
      iconColor: c.text3,
    },
    Scrollbar: {
      color: c.borderStrong,
      colorHover: c.text3,
    },
    Divider: {
      color: c.border,
    },
    Alert: {
      borderRadius: '8px',
    },
  }
}
