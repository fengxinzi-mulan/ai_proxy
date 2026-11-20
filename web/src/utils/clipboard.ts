// 复制到剪贴板。
//
// 不能直接用 navigator.clipboard：它只在安全上下文（https / localhost）里存在。
// 这个工具最常被复制的场景恰恰是「用局域网 IP 打开管理界面」（http://192.168.x.x:8080），
// 那里 navigator.clipboard 是 undefined，直接调会抛 TypeError，用户点了没反应。
// 所以先走现代 API，不可用时降级到 textarea + execCommand('copy')。

export async function copyText(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return
    } catch {
      // 权限被拒（非安全上下文、iframe 无授权）时继续走降级路径。
    }
  }
  if (fallbackCopy(text)) return
  throw new Error('当前环境不支持复制，请手动选中文本')
}

/**
 * 老掉牙但兼容性最好的做法：临时 textarea + execCommand。
 *
 * 必须把元素挂进文档、并显式选中，否则 iOS Safari 和部分桌面浏览器
 * 会把 execCommand('copy') 当成无选区操作而返回 false。
 */
function fallbackCopy(text: string): boolean {
  if (typeof document === 'undefined') return false
  const el = document.createElement('textarea')
  el.value = text
  // 放在视口外，避免页面跳动；readonly 防止移动端弹出键盘。
  el.setAttribute('readonly', '')
  el.style.position = 'fixed'
  el.style.top = '-9999px'
  el.style.opacity = '0'
  document.body.appendChild(el)
  try {
    el.select()
    el.setSelectionRange(0, el.value.length)
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    document.body.removeChild(el)
  }
}
