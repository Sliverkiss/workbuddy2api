<script setup>
// 池级统计区:Token 统计(网关请求日志聚合)+ 积分统计(探测结果 + 快照),左右等分。
// 数据:GET /api/stats/overview(server/stats.mjs 聚合)。
import { onMounted, ref } from 'vue';
import { Zap, Coins, AlarmClock } from '@lucide/vue';
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from './ui/primitives';
import { api } from '../api';
import { fmtNum, fmtDate, fmtAgo, fmtTimeFull } from '../lib/format';

const stats = ref(null);
const loading = ref(false);

async function load() {
  loading.value = true;
  try {
    stats.value = await api.statsOverview();
  } catch { /* 面板未就绪时保留空态 */ } finally {
    loading.value = false;
  }
}
onMounted(load);
defineExpose({ reload: load });

function shareOf(part, total) {
  if (!total) return 0;
  return Math.max(0, Math.min(100, (part / total) * 100));
}

function todayDeltaText(d) {
  if (d === null || d === undefined) return '—';
  if (d < 0) return `消耗 ${fmtNum(-d)}`;
  if (d > 0) return `净增 ${fmtNum(d)}`;
  return '持平';
}
</script>

<template>
  <div class="grid gap-6 xl:grid-cols-2">
    <!-- Token 统计:网关 /v1/chat/completions 请求日志聚合 -->
    <Card>
      <CardHeader class="flex-row items-center justify-between space-y-0">
        <div>
          <CardTitle class="flex items-center gap-2"><Zap :size="15" class="text-primary" />Token 统计</CardTitle>
          <!-- <CardDescription>网关请求日志聚合(docker 采集的真实调用)</CardDescription> -->
        </div>
        <span
          v-if="stats"
          class="flex shrink-0 items-center gap-1 text-xs text-muted-foreground"
          :title="`上一次更新时间:${fmtTimeFull(stats.generatedAt)}`"
        >
          <AlarmClock :size="12" />
          {{ fmtAgo(stats.generatedAt) }}
        </span>
      </CardHeader>
      <CardContent v-if="stats">
        <div v-if="!stats.tokens.requests" class="py-6 text-center text-sm text-muted-foreground">
          暂无请求日志 — 网关产生调用后此处自动统计
        </div>
        <template v-else>
          <div class="grid grid-cols-3 gap-3">
            <div>
              <p class="text-xs text-muted-foreground">Token 总量</p>
              <p class="mt-0.5 text-xl font-bold tabular-nums text-primary">{{ fmtNum(stats.tokens.outputTokens) }}</p>
            </div>
            <div>
              <p class="text-xs text-muted-foreground">调用次数</p>
              <p class="mt-0.5 text-xl font-bold tabular-nums">{{ fmtNum(stats.tokens.requests) }}</p>
            </div>
            <div>
              <p class="text-xs text-muted-foreground">平均速率</p>
              <p class="mt-0.5 text-xl font-bold tabular-nums">
                {{ stats.tokens.avgTokPerSec ?? '—' }}<span v-if="stats.tokens.avgTokPerSec" class="text-xs font-normal text-muted-foreground"> tok/s</span>
              </p>
            </div>
          </div>
          <p class="mt-2 text-xs text-muted-foreground tabular-nums">
            今日:{{ stats.tokens.today.requests }} 次调用 · {{ fmtNum(stats.tokens.today.outputTokens) }} tokens
          </p>

          <div v-if="stats.tokens.byModel.length" class="mt-4 space-y-1.5">
            <p class="text-xs text-muted-foreground">模型占比</p>
            <div v-for="m in stats.tokens.byModel" :key="m.key" class="space-y-0.5">
              <div class="flex items-baseline justify-between gap-2 text-sm">
                <span class="truncate font-mono text-xs">{{ m.key }}</span>
                <span class="shrink-0 text-xs tabular-nums text-muted-foreground">{{ m.requests }} 次 · {{ fmtNum(m.tokens) }}</span>
              </div>
              <div class="h-1 overflow-hidden rounded-full bg-muted">
                <div class="h-full rounded-full bg-success/70" :style="{ width: shareOf(m.tokens, stats.tokens.outputTokens) + '%' }" />
              </div>
            </div>
          </div>
        </template>
      </CardContent>
    </Card>

    <!-- 积分统计:探测结果 + 快照 -->
    <Card>
      <CardHeader class="flex-row items-center justify-between space-y-0">
        <div>
          <CardTitle class="flex items-center gap-2"><Coins :size="15" class="text-primary" />积分统计</CardTitle>
          <!-- <CardDescription>账号池积分汇总(只读探测口径)</CardDescription> -->
        </div>
        <span
          v-if="stats"
          class="flex shrink-0 items-center gap-1 text-xs text-muted-foreground"
          :title="`上一次更新时间:${fmtTimeFull(stats.generatedAt)}`"
        >
          <AlarmClock :size="12" />
          {{ fmtAgo(stats.generatedAt) }}
        </span>
      </CardHeader>
      <CardContent v-if="stats">
        <div v-if="!stats.credits.probed" class="py-6 text-center text-sm text-muted-foreground">
          暂无探测数据 — 到账号页执行一次「刷新」
        </div>
        <template v-else>
          <div class="grid grid-cols-3 gap-3">
            <div>
              <p class="text-xs text-muted-foreground">剩余总量</p>
              <p class="mt-0.5 text-xl font-bold tabular-nums text-primary">{{ fmtNum(stats.credits.remain) }}</p>
            </div>
            <div>
              <p class="text-xs text-muted-foreground">已用</p>
              <p class="mt-0.5 text-xl font-bold tabular-nums">{{ fmtNum(stats.credits.used) }}</p>
            </div>
            <div>
              <p class="text-xs text-muted-foreground">总量</p>
              <p class="mt-0.5 text-xl font-bold tabular-nums">{{ fmtNum(stats.credits.total) }}</p>
            </div>
          </div>
          <div class="mt-2 h-1.5 overflow-hidden rounded-full bg-muted">
            <div class="h-full rounded-full bg-success/80" :style="{ width: shareOf(stats.credits.used, stats.credits.total) + '%' }" />
          </div>
          <p class="mt-2 text-xs text-muted-foreground tabular-nums">
            今日变化:{{ todayDeltaText(stats.credits.todayDelta) }}
            <span v-if="stats.credits.expiring7d.amount">
              · 7 天内到期 <span class="text-warning font-medium">{{ fmtNum(stats.credits.expiring7d.amount) }}积分</span>(最早 {{ fmtDate(stats.credits.expiring7d.soonestAt) }})
            </span>
          </p>

          <div v-if="stats.credits.accounts.length" class="mt-4 space-y-1.5">
            <p class="text-xs text-muted-foreground">账号占比</p>
            <div v-for="a in stats.credits.accounts" :key="a.uid" class="space-y-0.5">
              <div class="flex items-baseline justify-between gap-2 text-sm">
                <span class="truncate">{{ a.nickname || a.uid }}</span>
                <span class="shrink-0 text-xs tabular-nums text-muted-foreground">
                  剩 {{ fmtNum(a.remain) }} / {{ fmtNum(a.total) }}
                </span>
              </div>
              <div class="h-1 overflow-hidden rounded-full bg-muted">
                <div class="h-full rounded-full bg-success/70" :style="{ width: shareOf(a.remain, stats.credits.remain) + '%' }" />
              </div>
            </div>
          </div>
        </template>
      </CardContent>
    </Card>
  </div>
</template>
