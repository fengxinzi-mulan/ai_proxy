<script setup lang="ts">
// 指标卡：小标签 + 大数字 + 可选说明气泡。
//
// 数字用等宽字体对齐，多个卡片横排时小数点不会互相错位。
import { NIcon, NSkeleton, NTooltip } from 'naive-ui'
import { Info } from 'lucide-vue-next'

defineProps<{
  label: string
  value: string
  hint?: string
  accent?: string
  loading?: boolean
}>()
</script>

<template>
  <div class="stat-card">
    <NSkeleton v-if="loading" text :repeat="2" />
    <template v-else>
      <NTooltip v-if="hint" trigger="hover" :delay="200">
        <template #trigger>
          <div class="label">
            <span>{{ label }}</span>
            <NIcon :size="12" class="info"><Info /></NIcon>
          </div>
        </template>
        <span class="hint-text">{{ hint }}</span>
      </NTooltip>
      <div v-else class="label">{{ label }}</div>
      <div class="value num" :style="accent ? { color: accent } : undefined">{{ value }}</div>
    </template>
  </div>
</template>

<style scoped>
.stat-card {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 14px 16px;
  min-width: 0;
}

.label {
  display: flex;
  align-items: center;
  gap: 4px;
  font-size: var(--text-2xs);
  color: var(--text-3);
  cursor: default;
  white-space: nowrap;
}

.info {
  opacity: 0.7;
  flex-shrink: 0;
}

.value {
  /* 大数字用无衬线 + tabular-nums：等宽字体的斜杠零在 26px 下像 Ø，
     而 tabular-nums 已经能保证多位数字左右对齐。 */
  font-family: var(--font-sans);
  font-variant-numeric: tabular-nums;
  font-size: 26px;
  font-weight: 600;
  line-height: 1.25;
  margin-top: 6px;
  letter-spacing: -0.02em;
  color: var(--text);
}

.hint-text {
  display: block;
  max-width: 280px;
  line-height: 1.6;
}
</style>
