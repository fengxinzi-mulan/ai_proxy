<script setup lang="ts">
// 套餐余量面板。两种形态共用一套视觉语言，只是详略不同：
//   - 完整态（编辑抽屉的「用量」页）：计划、余额明细、三个窗口、周期统计、原始响应
//   - 紧凑态（供应商卡片）：一行计划 + 一个大号余额 + 三个并排的窗口条
//
// 卡片上刻意压掉了「查询耗时」「订阅状态」这类信息：它们在卡片上属于噪声，
// 但在抽屉里排障时有用，所以只出现在完整态。
//
// 不做任何自动刷新：一次查询要打四个上游接口，由调用方决定什么时候查。
import { computed } from 'vue'
import {
  NAlert,
  NButton,
  NCollapse,
  NCollapseItem,
  NSpin,
  NTag,
  NTooltip,
} from 'naive-ui'
import type { UsageSnapshot, UsageStat, UsageWindow } from '@/types'
import {
  formatCompact,
  formatNumber,
  formatPercent,
  formatRelative,
  formatUntil,
  formatUSD,
  quotaColor,
} from '@/utils/format'

const props = withDefaults(
  defineProps<{
    /** 最近一次查询结果；null 表示还没查过 */
    snapshot?: UsageSnapshot | null
    loading?: boolean
    /** 该供应商是否配了用量查询模板 */
    configured?: boolean
    /** 紧凑形态：给供应商卡片用 */
    compact?: boolean
  }>(),
  { snapshot: null, loading: false, configured: true, compact: false },
)

const emit = defineEmits<{ query: [] }>()

/** 剩余额度合计。上游把额度拆成月度/充值/赠送三份，加总才是可用余额。 */
const remaining = computed(() => {
  const balances = props.snapshot?.balances ?? []
  if (!balances.length) return null
  return balances.reduce((sum, b) => sum + (b.amount || 0), 0)
})

const windows = computed(() => props.snapshot?.windows ?? [])

/** 余额明细一行，用于悬浮提示与完整态。 */
const balanceDetail = computed(() =>
  (props.snapshot?.balances ?? []).map((b) => `${b.label} ${formatUSD(b.amount)}`).join(' · '),
)

/** 把不同单位的统计值格式化成一致口径。token 走短格式：动辄十位数的
    精确值在方格子里既撑破列宽，也不比 2.21B 提供更多决策信息。 */
function statValue(stat: UsageStat): string {
  switch (stat.unit) {
    case 'usd':
      return formatUSD(stat.value)
    case 'percent':
      return formatPercent(stat.value)
    case 'tokens':
      return formatCompact(stat.value)
    case 'count':
    default:
      return formatNumber(stat.value)
  }
}

/** 周期统计按单位分两组：金额/请求数/成功率是一组可以横向对比的量，
    token 是另一个数量级，混在同一张网格里只会把两排都挤窄。 */
const periodGroups = computed(() => {
  const period = props.snapshot?.period ?? []
  return [
    { key: 'usage', title: '本周期用量', stats: period.filter((s) => s.unit !== 'tokens') },
    { key: 'tokens', title: '本周期 token', stats: period.filter((s) => s.unit === 'tokens') },
  ].filter((g) => g.stats.length)
})

/** 计费周期的说明挂在第一组标题行的右侧 —— 它标的是这批数字的时间范围。 */
const periodRange = computed(() => {
  const s = props.snapshot
  if (!s || (!s.periodStart && !s.periodEnd)) return ''
  const from = s.periodStart ? `${formatRelative(s.periodStart)}起` : '—'
  const to = s.periodEnd ? formatUntil(s.periodEnd) : '—'
  return `${from} · 至 ${to}`
})

/** 窗口的悬浮提示：用量、上限、重置时间，以及推导来源（如果有）。 */
function windowTip(w: UsageWindow): string {
  const parts = [`${formatUSD(w.used)} / ${formatUSD(w.cap)}`]
  if (w.resetAt) parts.push(`重置于 ${formatUntil(w.resetAt)}`)
  if (w.hint) parts.push(w.hint)
  return parts.join(' · ')
}

/** 进度条填充宽度。超过 100% 时钉在满格，避免视觉上溢出。 */
function fillWidth(percent: number): string {
  return `${Math.min(Math.max(percent, 0), 100)}%`
}

const hasData = computed(() => {
  const s = props.snapshot
  return Boolean(s && (s.balances.length || s.windows.length || s.period.length || s.planName))
})
</script>

<template>
  <div class="usage" :class="{ compact }">
    <NSpin :show="loading" size="small">
      <!-- 整体失败：模板没配、密钥被拒、地址推导不出来 -->
      <NAlert v-if="snapshot?.error" type="error" :bordered="false" class="alert">{{ snapshot.error }}</NAlert>

      <template v-else-if="hasData">
        <!-- ===== 紧凑态：供应商卡片 ===== -->
        <template v-if="compact">
          <div class="c-top">
            <NTag v-if="snapshot!.planName" size="tiny" :bordered="false" type="info">
              {{ snapshot!.planName }}
            </NTag>
            <span class="c-meta">
              <span class="c-when">{{ formatRelative(snapshot!.fetchedAt) }}</span>
              <NTooltip trigger="hover" placement="top">
                <template #trigger>
                  <button class="c-refresh" :disabled="loading" @click="emit('query')">
                    <svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true">
                      <path
                        d="M13.5 8a5.5 5.5 0 1 1-1.6-3.9M13.5 1.5V5H10"
                        fill="none"
                        stroke="currentColor"
                        stroke-width="1.3"
                        stroke-linecap="round"
                        stroke-linejoin="round"
                      />
                    </svg>
                  </button>
                </template>
                重新查询用量
              </NTooltip>
            </span>
          </div>

          <div class="c-hero">
            <span class="c-amount">{{ remaining === null ? '—' : formatUSD(remaining) }}</span>
            <NTooltip v-if="balanceDetail" trigger="hover" placement="top">
              <template #trigger>
                <span class="c-amount-label">剩余额度</span>
              </template>
              {{ balanceDetail }}
            </NTooltip>
            <span v-else class="c-amount-label">剩余额度</span>
          </div>

          <div v-if="windows.length" class="c-windows">
            <NTooltip v-for="w in windows" :key="w.label" trigger="hover" placement="top">
              <template #trigger>
                <div class="c-window">
                  <div class="c-window-head">
                    <span class="c-window-label" :style="w.exceeded ? { color: '#d03050' } : undefined">
                      {{ w.label }}
                    </span>
                    <span class="c-window-pct" :style="{ color: quotaColor(w.percent) }">
                      {{ formatPercent(w.percent, 0) }}
                    </span>
                  </div>
                  <div class="qbar">
                    <div
                      class="qbar-fill"
                      :style="{ width: fillWidth(w.percent), background: quotaColor(w.percent) }"
                    />
                  </div>
                </div>
              </template>
              {{ w.exceeded ? `已超出该窗口限额 · ${windowTip(w)}` : windowTip(w) }}
            </NTooltip>
          </div>

          <div v-if="snapshot!.warnings.length" class="c-note warn">
            ⚠ {{ snapshot!.warnings.length }} 段不可用
            <NTooltip trigger="hover" placement="top">
              <template #trigger><span class="c-note-more">详情</span></template>
              <div v-for="(w, i) in snapshot!.warnings" :key="i">{{ w }}</div>
            </NTooltip>
          </div>
        </template>

        <!-- ===== 完整态：编辑抽屉 ===== -->
        <template v-else>
          <div class="f-head">
            <div class="f-plan">
              <NTag v-if="snapshot!.planName" size="small" :bordered="false" type="info">
                {{ snapshot!.planName }}
              </NTag>
              <NTag v-if="snapshot!.status" size="tiny" :bordered="false">{{ snapshot!.status }}</NTag>
              <span v-if="snapshot!.account" class="muted small ellipsis">{{ snapshot!.account }}</span>
            </div>
            <span class="muted small">
              {{ formatRelative(snapshot!.fetchedAt) }}查询
              <template v-if="snapshot!.latencyMs"> · {{ snapshot!.latencyMs }} ms</template>
            </span>
          </div>

          <div v-if="remaining !== null" class="f-balance">
            <span class="f-amount">{{ formatUSD(remaining) }}</span>
            <span class="muted small">剩余额度</span>
            <span v-if="snapshot!.balances.length" class="f-parts">
              <span
                v-for="b in snapshot!.balances"
                :key="b.label"
                class="small"
                :class="{ dim: !b.amount }"
              >
                {{ b.label }} {{ formatUSD(b.amount) }}
              </span>
            </span>
          </div>

          <!-- 一行放标签与数字、一行放进度条、一行放重置时间。
               进度条独占一整行是有意的：额度条要横向比较才看得出哪条快满了，
               挤进一行反而把条压短、三根长短难分辨。 -->
          <div v-if="windows.length" class="f-windows">
            <div v-for="w in windows" :key="w.label" class="f-window">
              <div class="f-window-head">
                <span class="f-window-label">
                  {{ w.label }}
                  <NTag v-if="w.exceeded" size="tiny" type="error" :bordered="false" class="ml">
                    已超限
                  </NTag>
                </span>
                <span class="f-window-nums mono">
                  {{ formatUSD(w.used) }} / {{ formatUSD(w.cap) }}
                  <span class="f-window-pct" :style="{ color: quotaColor(w.percent) }">
                    {{ formatPercent(w.percent) }}
                  </span>
                </span>
              </div>
              <div class="qbar">
                <div
                  class="qbar-fill"
                  :style="{ width: fillWidth(w.percent), background: quotaColor(w.percent) }"
                />
              </div>
              <div class="f-window-foot">
                <span v-if="w.resetAt" class="muted small">重置于 {{ formatUntil(w.resetAt) }}</span>
                <NTooltip v-if="w.hint" trigger="hover" placement="top">
                  <template #trigger><span class="f-hint">上限怎么算的</span></template>
                  {{ w.hint }}
                </NTooltip>
              </div>
            </div>
          </div>

          <div v-if="periodGroups.length" class="f-period">
            <div
              v-for="(g, i) in periodGroups"
              :key="g.key"
              class="f-group"
              :class="{ divided: i > 0 }"
            >
              <div class="f-group-head">
                <span class="f-group-title">{{ g.title }}</span>
                <span v-if="i === 0 && periodRange" class="muted small">计费周期 {{ periodRange }}</span>
              </div>
              <div class="f-stats">
                <div v-for="s in g.stats" :key="s.label" class="f-stat">
                  <NTooltip v-if="s.hint" trigger="hover" placement="top">
                    <template #trigger><span class="muted small">{{ s.label }}</span></template>
                    {{ s.hint }}
                  </NTooltip>
                  <span v-else class="muted small">{{ s.label }}</span>
                  <span class="f-stat-value mono">{{ statValue(s) }}</span>
                </div>
              </div>
            </div>
          </div>

          <!-- 部分接口不可用：说明哪一段没读到，而不是显示成 0 -->
          <NAlert v-if="snapshot!.warnings.length" type="warning" :bordered="false" class="alert">
            <div v-for="(w, i) in snapshot!.warnings" :key="i">{{ w }}</div>
          </NAlert>

          <NCollapse style="margin-top: 4px">
            <NCollapseItem title="原始响应" name="raw" class="collapse-title">
              <pre class="f-raw mono">{{ JSON.stringify(snapshot!.raw ?? {}, null, 2) }}</pre>
              <div v-for="u in snapshot!.usedUrls" :key="u" class="muted small mono">{{ u }}</div>
            </NCollapseItem>
          </NCollapse>
        </template>
      </template>

      <!-- 还没查过 / 不能查 -->
      <div v-else class="empty">
        <template v-if="!configured">该供应商未配置用量查询模板</template>
        <template v-else-if="loading">查询中…</template>
        <template v-else>还没有查询过</template>
      </div>
    </NSpin>

    <NButton
      v-if="configured && (compact ? !hasData : true)"
      :size="compact ? 'tiny' : 'small'"
      :loading="loading"
      :class="compact ? 'c-empty-btn' : 'f-refresh'"
      @click="emit('query')"
    >
      {{ hasData ? '刷新用量' : '查询用量' }}
    </NButton>
  </div>
</template>

<style scoped>
.usage {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.alert {
  font-size: 12px;
}

.collapse-title :deep(.n-collapse-item__header-main) {
  font-size: 12px;
}

/* ---------- 共用的细进度条 ---------- */
/* 不用 NProgress：这里要的是「细轨 + 圆角填充」的最小形态，
   卡片上三根并排时它比组件化的进度条更干净，也省一层 DOM。 */
.qbar {
  height: 6px;
  border-radius: 3px;
  background: rgba(128, 128, 128, 0.18);
  overflow: hidden;
}

.qbar-fill {
  height: 100%;
  border-radius: 3px;
  transition: width 0.25s ease;
}

/* ---------- 紧凑态（供应商卡片） ---------- */
.c-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 6px;
}

.c-meta {
  display: flex;
  align-items: center;
  gap: 6px;
}

.c-refresh {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 20px;
  height: 20px;
  padding: 0;
  border: none;
  border-radius: 5px;
  background: transparent;
  color: inherit;
  opacity: 0.45;
  cursor: pointer;
  transition: opacity 0.15s ease, background 0.15s ease;
}

.c-refresh:hover:not(:disabled) {
  opacity: 1;
  background: rgba(128, 128, 128, 0.14);
}

.c-refresh:disabled {
  cursor: default;
  opacity: 0.25;
}

.c-hero {
  display: flex;
  align-items: baseline;
  gap: 6px;
}

.c-amount {
  font-size: 22px;
  font-weight: 600;
  line-height: 1.1;
  font-variant-numeric: tabular-nums;
  letter-spacing: -0.02em;
}

.c-amount-label {
  font-size: 11px;
  opacity: 0.55;
  cursor: default;
}

/* 查询时间跟刷新按钮挨在一起：它回答的是「这个数字有多新」，
   和重新测量是同一件事，不该孤零零挂在金额旁边。 */
.c-when {
  font-size: 10px;
  opacity: 0.45;
  white-space: nowrap;
}

.c-windows {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 12px;
  margin-top: 2px;
}

/* 窗口太窄时宁可挤一点也不要换行：三根并排的对比才是这个卡片的价值 */
.c-window {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
  cursor: default;
}

.c-window-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 3px;
}

.c-window-label {
  font-size: 11px;
  opacity: 0.6;
  white-space: nowrap;
}

.c-window-pct {
  font-size: 11px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.c-note {
  font-size: 11px;
  opacity: 0.75;
}

.c-note.warn {
  color: #f0a020;
}

.c-note-more {
  margin-left: 4px;
  text-decoration: underline;
  cursor: default;
}

.c-empty-btn {
  align-self: flex-start;
}

/* ---------- 完整态（编辑抽屉） ---------- */
.f-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  flex-wrap: wrap;
}

.f-plan {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}

.f-balance {
  display: flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
}

.f-amount {
  font-size: 26px;
  font-weight: 600;
  line-height: 1.1;
  font-variant-numeric: tabular-nums;
  letter-spacing: -0.02em;
}

.f-parts {
  display: flex;
  align-items: baseline;
  gap: 10px;
  border-left: 1px solid rgba(128, 128, 128, 0.25);
  padding-left: 8px;
  flex-wrap: wrap;
}

.f-windows {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.f-window {
  display: flex;
  flex-direction: column;
  gap: 5px;
}

.f-window-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
  font-size: 12px;
}

.f-window-label {
  display: flex;
  align-items: center;
  gap: 4px;
}

.f-window-nums {
  font-variant-numeric: tabular-nums;
  opacity: 0.85;
}

.f-window-pct {
  margin-left: 6px;
  font-weight: 600;
}

.f-window-foot {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 11px;
}

.f-hint {
  opacity: 0.6;
  text-decoration: underline dotted;
  cursor: default;
}

.f-period {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

/* 两组之间画一条细线：不是两个独立区块，但也不用让人自己数格子 */
.f-group.divided {
  border-top: 1px solid rgba(128, 128, 128, 0.18);
  padding-top: 12px;
}

.f-group-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 8px;
}

.f-group-title {
  font-size: 11px;
  opacity: 0.6;
  letter-spacing: 0.02em;
}

.f-stats {
  display: grid;
  /* 150px 是有意的：抽屉内容宽约 672px，这一档正好排下 4 列，
     金额/请求数/成功率/平均每次请求刚好一行，token 两个成一行，
     不会出现 3+1 这种半行悬空。 */
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  gap: 12px 20px;
}

.f-stat {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.f-stat-value {
  font-size: 14px;
  font-variant-numeric: tabular-nums;
}

.f-raw {
  max-height: 260px;
  overflow: auto;
  font-size: 11px;
  line-height: 1.5;
  margin: 0 0 6px;
  padding: 8px;
  border-radius: 6px;
  background: rgba(128, 128, 128, 0.08);
  white-space: pre-wrap;
  word-break: break-all;
}

.f-refresh {
  align-self: flex-start;
}

/* ---------- 通用 ---------- */
.empty {
  font-size: 12px;
  opacity: 0.55;
}

.muted {
  opacity: 0.6;
}

.small {
  font-size: 11px;
}

.ml {
  margin-left: 4px;
}

/* 零额度（没充值、没赠送）照样列出来，但压暗 —— 信息在，噪声不在 */
.dim {
  opacity: 0.4;
}

.ellipsis {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
</style>
