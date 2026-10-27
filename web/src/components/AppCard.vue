<script setup lang="ts">
// 通用卡片：统一边框、圆角、内边距与卡片头。
//
// 目的是消除各页面重复的 box-shadow + border 组合（原来 6 个文件各写一份，
// 暗色下阴影常量还都没换）。需要强调时用 accent 变体，而不是加粗描边。
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    title?: string
    /** 标题右侧的说明文字 */
    description?: string
    /** 是否高亮（当前生效项） */
    accent?: boolean
    /** 紧凑内边距，用于侧栏里的信息块 */
    dense?: boolean
    /** 内容区是否自带横向滚动 */
    bodyClass?: string
  }>(),
  { accent: false, dense: false, bodyClass: '' },
)

const cls = computed(() => ({
  card: true,
  accent: props.accent,
  dense: props.dense,
}))
</script>

<template>
  <section :class="cls">
    <header v-if="title || $slots.actions" class="card-head">
      <div class="card-head-text">
        <h3 v-if="title" class="card-title">{{ title }}</h3>
        <p v-if="description" class="card-desc">{{ description }}</p>
      </div>
      <div v-if="$slots.actions" class="card-actions">
        <slot name="actions" />
      </div>
    </header>
    <div :class="['card-body', bodyClass]">
      <slot />
    </div>
  </section>
</template>

<style scoped>
.card {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  min-width: 0;
}

/* 当前生效/选中态：整圈细描边 + 左侧强调条，比粗绿框克制。 */
.card.accent {
  border-color: var(--accent-border);
  box-shadow: inset 3px 0 0 var(--accent);
}

.card-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
  padding: var(--space-4) var(--space-4) 0;
}

.card-head-text {
  min-width: 0;
}

.card-title {
  font-size: var(--text-sm);
  font-weight: 600;
  line-height: 1.4;
}

.card-desc {
  margin: 2px 0 0;
  font-size: var(--text-xs);
  color: var(--text-3);
  line-height: 1.5;
}

.card-actions {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-shrink: 0;
}

.card-body {
  padding: var(--space-4);
}

.card.dense .card-head {
  padding: var(--space-3) var(--space-3) 0;
}

.card.dense .card-body {
  padding: var(--space-3);
}
</style>
