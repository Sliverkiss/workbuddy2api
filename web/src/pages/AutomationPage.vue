<script setup>
// 自动化页(集成版):定时调度由网关自身持有(只读展示),本页提供手动立即执行与近期事件。
import { computed, onMounted, ref } from 'vue';
import { CalendarCheck, Cat, Radio, HeartPulse, Play, Clock } from '@lucide/vue';
import PageHeader from '../components/PageHeader.vue';
import { Card, CardHeader, CardTitle, CardDescription, CardContent, Button, Badge } from '../components/ui/primitives';
import { useAppStore } from '../stores/app';
import { api } from '../api';
import { toast } from '../stores/toast';
import { fmtAgo, kindLabel } from '../lib/format';

const store = useAppStore();
const runningKind = ref('');
const recentLogs = ref([]);

const TASK_DEFS = [
  { kind: 'checkin', icon: CalendarCheck, name: '每日签到', desc: '签到 + 余额查询 + 低分账号解冻;与网关调度器同一签到通道。' },
  { kind: 'travel', icon: Cat, name: '猫猫旅行', desc: '状态机单趟推进:无猫领养 / idle 派出 / arrived 领奖;动作后自动探测刷新。' },
  { kind: 'activity', icon: Radio, name: '活跃上报', desc: 'chat_request_send 上报点亮连登 + 解锁领养,带 streak 回读自检;每号每天 1 次即可。' },
  { kind: 'keepalive', icon: HeartPulse, name: 'token 保活', desc: '全账号刷新 token;12153 连续 3 次判定 session 死亡。' },
];

async function refreshLogs() {
  try {
    const r = await api.listLogs({ limit: 200 });
    recentLogs.value = r.logs ?? [];
  } catch { /* 静默 */ }
}

onMounted(async () => {
  if (!store.config) await store.loadConfig().catch(() => {});
  await refreshLogs();
});

function scheduleText(kind) {
  const s = store.config?.schedule;
  if (!s) return '—';
  if (!s[`${kind}_enabled`]) return '已禁用';
  const hours = s[`${kind}_hours`] ?? [];
  return hours.length ? hours.join(', ') + ' 点' : '—';
}

const lastOf = computed(() => {
  const map = {};
  for (const l of recentLogs.value) {
    if (TASK_DEFS.some((d) => d.kind === l.kind) && (!map[l.kind] || l.ts > map[l.kind].ts)) {
      map[l.kind] = l;
    }
  }
  return map;
});

async function runNow(kind) {
  runningKind.value = kind;
  try {
    const r = await store.runTask(kind);
    const s = r.summary;
    toast[s.fail > 0 ? 'warn' : 'success'](
      `${kindLabel(kind)}完成(${s.total} 个):成功 ${s.ok}` +
      (s.already ? ` / 已签 ${s.already}` : '') +
      (s.skip ? ` / 跳过 ${s.skip}` : '') +
      (s.warn ? ` / 警告 ${s.warn}` : '') +
      (s.fail ? ` / 失败 ${s.fail}` : ''),
    );
    await refreshLogs();
  } catch (e) {
    toast.error(e.message, { title: `${kindLabel(kind)}执行失败` });
  } finally {
    runningKind.value = '';
  }
}
</script>

<template>
  <div class="p-6 lg:p-8">
    <PageHeader title="自动化" description="定时调度由网关持有(config.json 只读);此处可手动立即执行" />

    <div class="grid gap-4 xl:grid-cols-2">
      <Card v-for="(def, i) in TASK_DEFS" :key="def.kind" class="wb-enter" :class="`wb-enter-${i}`">
        <CardHeader>
          <div class="flex items-center justify-between">
            <div class="flex items-center gap-2.5">
              <div class="flex h-8 w-8 items-center justify-center rounded-md bg-secondary">
                <component :is="def.icon" :size="16" />
              </div>
              <div>
                <CardTitle>{{ def.name }}</CardTitle>
                <Badge variant="secondary" class="mt-1"><Clock :size="11" class="mr-1" />{{ scheduleText(def.kind) }}</Badge>
              </div>
            </div>
          </div>
          <CardDescription class="pt-2">{{ def.desc }}</CardDescription>
        </CardHeader>
        <CardContent class="space-y-3">
          <div v-if="lastOf[def.kind]" class="rounded-md bg-muted/50 px-3 py-2 text-xs text-muted-foreground">
            上次:{{ fmtAgo(lastOf[def.kind].ts) }}
            <span class="text-foreground">{{ lastOf[def.kind].nickname || lastOf[def.kind].msg }}</span>
            <span v-if="lastOf[def.kind].status === 'fail'" class="text-destructive">(失败)</span>
          </div>
          <Button variant="secondary" size="sm" class="w-full" :loading="runningKind === def.kind" @click="runNow(def.kind)">
            <Play v-if="runningKind !== def.kind" />立即执行一次
          </Button>
        </CardContent>
      </Card>
    </div>
  </div>
</template>
