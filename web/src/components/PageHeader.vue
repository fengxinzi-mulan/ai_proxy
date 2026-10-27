<script setup lang="ts">
// 页面标题区：标题 + 一句话说明 + 右侧操作。
//
// 五个页面原来各写一份 .page-head（字号、间距、边距各不相同），统一到这里。
defineProps<{
  title: string
  description?: string
}>()
</script>

<template>
  <div class="page-head">
    <div class="page-head-text">
      <h2 class="page-title">{{ title }}</h2>
      <p v-if="description" class="page-desc">{{ description }}</p>
    </div>
    <div v-if="$slots.actions" class="page-actions">
      <slot name="actions" />
    </div>
  </div>
</template>

<style scoped>
.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-4);
  margin-bottom: var(--space-4);
}

.page-head-text {
  min-width: 0;
}

.page-title {
  font-size: var(--text-xl);
  font-weight: 600;
  line-height: 1.3;
  letter-spacing: -0.01em;
}

.page-desc {
  margin: 4px 0 0;
  font-size: var(--text-xs);
  color: var(--text-3);
  line-height: 1.5;
}

.page-actions {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-shrink: 0;
}
</style>
