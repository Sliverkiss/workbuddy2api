<script setup>
// 概览页:账号/积分指标 + 横向批量操作 + 可用模型/对话测试 + Token统计/积分统计。
import { computed, ref } from 'vue';
import { RouterLink } from 'vue-router';
import {
  Users, Coins, CalendarCheck, HeartPulse, Cat, Radio, RefreshCw, CircleAlert,
} from '@lucide/vue';
import PageHeader from '../components/PageHeader.vue';
import ModelListCard from '../components/ModelListCard.vue';
import ChatTestCard from '../components/ChatTestCard.vue';
import PoolStatsSection from '../components/PoolStatsSection.vue';
import { Card, Button } from '../components/ui/primitives';
import { useAppStore } from '../stores/app';
import { toast } from '../stores/toast';
import { fmtNum, kindLabel } from '../lib/format';

const store = useAppStore();
const batchBusy = ref('');

const stats = computed(() => [
  {
    label: '账号总数',
    value: store.accountsLoaded ? String(store.validAccounts.length) : '—',
    icon: Users,
    hint: ``,
    warn: !store.dirExists || store.accounts.some((a) => a.loadError),
  },
  {
    label: '积分总量',
    value: store.accountsLoaded ? fmtNum(store.totalCredits) : '—',
    icon: Coins,
    hint: ``,
  },
]);

// 横向四键;「刷新积分」上移至标题栏图标按钮。
const QUICK = [
  { kind: 'checkin', label: '一键签到', icon: CalendarCheck, desc: '全部账号签到并刷新积分' },
  { kind: 'keepalive', label: '一键保活', icon: HeartPulse, desc: '刷新全部账号 token' },
  { kind: 'travel', label: '一键旅行', icon: Cat, desc: '推进全部账号猫猫旅行' },
  { kind: 'activity', label: '一键活跃', icon: Radio, desc: '上报活跃点亮连登' },
];

const statsRef = ref(null);

async function runBatch(kind) {
  batchBusy.value = kind;
  try {
    const r = await store.runTask(kind);
    const s = r.summary;
    const parts = [`成功 ${s.ok}`];
    if (s.already) parts.push(`已签 ${s.already}`);
    if (s.skip) parts.push(`跳过 ${s.skip}`);
    if (s.warn) parts.push(`警告 ${s.warn}`);
    if (s.fail) parts.push(`失败 ${s.fail}`);
    toast[s.fail > 0 ? 'warn' : 'success'](`${kindLabel(kind)}完成(${s.total} 个账号):${parts.join(' / ')}`);
  } catch (e) {
    toast.error(e.message, { title: `${kindLabel(kind)}执行失败` });
  } finally {
    batchBusy.value = '';
  }
}

// 标题栏刷新:积分任务 + 统计区重载(Token/积分统计一并更新)。
async function refreshAll() {
  await runBatch('credit');
  statsRef.value?.reload();
}
</script>

<template>
  <div class="p-6 lg:p-8">
    <PageHeader title="概览" description="账号池积分总量统计、Token使用量统计、一键任务">
      <Button
        variant="outline"
        size="sm"
        :loading="batchBusy === 'credit'"
        title="刷新全部积分与统计"
        @click="refreshAll"
      >
        <RefreshCw v-if="batchBusy !== 'credit'" />
      </Button>
    </PageHeader>

    <!-- 凭证目录缺失告警 -->
    <div v-if="store.accountsLoaded && !store.dirExists" class="mb-6 flex items-start gap-3 rounded-lg border border-warning/50 bg-warning/10 p-4 wb-enter">
      <CircleAlert class="mt-0.5 h-4 w-4 shrink-0 text-warning" />
      <div class="text-sm">
        <p class="font-semibold">凭证目录不存在</p>
        <p class="text-muted-foreground break-all">{{ store.authDir }}</p>
        <p class="mt-1 text-muted-foreground">
          请在<RouterLink to="/settings" class="underline underline-offset-2">设置</RouterLink>中把凭证目录指向 workbuddy2api 的 auths 目录。
        </p>
      </div>
    </div>

    <!-- 指标 + 批量操作(占据原两张指标卡宽度) -->
    <div class="grid grid-cols-2 gap-4 xl:grid-cols-4">
      <Card v-for="(s, i) in stats" :key="s.label" class="p-5 wb-enter" :class="`wb-enter-${i}`">
        <div class="flex items-center justify-between">
          <p class="text-sm text-muted-foreground">{{ s.label }}</p>
          <component :is="s.icon" :size="16" :class="s.warn ? 'text-warning' : 'text-muted-foreground'" />
        </div>
        <p class="mt-2 text-2xl font-bold tabular-nums" :class="s.warn ? 'text-warning' : ''">{{ s.value }}</p>
        <p class="mt-1 truncate text-xs text-muted-foreground">{{ s.hint }}</p>
      </Card>

      <Card class="col-span-2 flex flex-col p-5 wb-enter wb-enter-2">
        <p class="text-sm text-muted-foreground">批量任务</p>
        <div class="mt-2 flex flex-1 items-stretch gap-2">
          <button
            v-for="q in QUICK"
            :key="q.kind"
            @click="runBatch(q.kind)"
            :disabled="!!batchBusy"
            :title="q.desc"
            class="flex flex-1 flex-col items-center justify-center gap-1.5 rounded-md border border-border p-2.5 transition-colors hover:bg-accent/60 disabled:opacity-50"
          >
            <span v-if="batchBusy === q.kind" class="h-4 w-4 animate-spin rounded-full border-2 border-border border-t-primary" />
            <component :is="q.icon" v-else :size="16" class="shrink-0 text-primary" />
            <span class="whitespace-nowrap text-xs font-medium">{{ q.label }}</span>
          </button>
        </div>
      </Card>
    </div>

    <!-- 可用模型 + 对话测试(左右等分) -->
    <div class="mt-6 grid gap-6 xl:grid-cols-2">
      <ModelListCard class="wb-enter wb-enter-1" />
      <ChatTestCard class="wb-enter wb-enter-2" />
    </div>

    <!-- Token 统计 + 积分统计(左右等分) -->
    <PoolStatsSection ref="statsRef" class="mt-6" />
  </div>
</template>
