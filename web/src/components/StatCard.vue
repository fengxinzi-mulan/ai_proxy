<script setup lang="ts">
import { NCard, NSkeleton, NTooltip } from 'naive-ui'

defineProps<{
  label: string
  value: string
  hint?: string
  accent?: string
  loading?: boolean
}>()
</script>

<template>
  <NCard size="small" class="stat-card" :bordered="false">
    <NSkeleton v-if="loading" text :repeat="2" />
    <template v-else>
      <NTooltip v-if="hint" trigger="hover">
        <template #trigger>
          <div class="label">
            {{ label }}
            <span class="info">ⓘ</span>
          </div>
        </template>
        {{ hint }}
      </NTooltip>
      <div v-else class="label">{{ label }}</div>
      <div class="value num" :style="accent ? { color: accent } : undefined">{{ value }}</div>
    </template>
  </NCard>
</template>

<style scoped>
.stat-card {
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04), 0 4px 16px rgba(16, 24, 40, 0.06);
}

.label {
  font-size: 12px;
  opacity: 0.62;
  display: flex;
  align-items: center;
  gap: 4px;
  cursor: default;
  white-space: nowrap;
}

.info {
  opacity: 0.5;
  font-size: 11px;
}

.value {
  font-size: 24px;
  font-weight: 600;
  line-height: 1.3;
  margin-top: 6px;
  letter-spacing: -0.01em;
}
</style>
