<script setup lang="ts">
// 问号说明气泡：把「常驻在界面上的整段解释」收进悬停提示。
//
// 这些说明原先是一行或几行 muted 文字 / info 提示块，直接贴在控件旁边。
// 抽屉窄的时候它们会折成好几行，把真正要操作的开关挤走；内容一多，
// 界面上就只剩解释、看不见配置。统一收进问号气泡后，布局只占一个图标。
import { NTooltip, NIcon } from 'naive-ui'
import { CircleHelp } from 'lucide-vue-next'

withDefaults(
  defineProps<{
    /** 纯文本说明。需要内联 code / strong 时改用默认插槽。 */
    text?: string
    /** 气泡最大宽度（px）。长说明默认给宽一点，避免频繁折行。 */
    width?: number
    placement?: 'top' | 'bottom' | 'left' | 'right'
  }>(),
  { width: 360, placement: 'top' },
)
</script>

<template>
  <NTooltip trigger="hover" :placement="placement" :delay="150" :style="{ maxWidth: `${width}px` }">
    <template #trigger>
      <NIcon :size="15" class="help-tip" role="img" aria-label="说明">
        <CircleHelp />
      </NIcon>
    </template>
    <div class="help-tip-body">
      <slot>{{ text }}</slot>
    </div>
  </NTooltip>
</template>

<style scoped>
.help-tip {
  margin-left: 6px;
  vertical-align: -3px;
  opacity: 0.55;
  cursor: help;
}

.help-tip-body {
  font-size: 12px;
  line-height: 1.75;
}

/* 气泡内容会被 teleport 到 body，但插槽节点仍带本组件的 scope id，
   所以这里用 :deep 补一下 code 的底色，和页面里其它说明保持一致。 */
.help-tip-body :deep(code) {
  padding: 0 3px;
  border-radius: 3px;
  background: rgba(127, 127, 127, 0.16);
}
</style>
