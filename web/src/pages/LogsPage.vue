<script setup>
// 日志页:任务日志 / 请求日志(网关 docker stdout 采集)/ 网关 stdout 原始行,三页签,5s 自刷。
import { onMounted, onBeforeUnmount, ref, computed } from 'vue';
import { RouterLink } from 'vue-router';
import { RefreshCw, TriangleAlert } from '@lucide/vue';
import PageHeader from '../components/PageHeader.vue';
import { Card, CardHeader, CardTitle, CardDescription, CardContent, Button, Badge, Input } from '../components/ui/primitives';
import { api } from '../api';
import { fmtTimeFull, kindLabel, LOG_STATUS, TASK_KINDS } from '../lib/format';

const tab = ref('tasks'); // tasks | requests | stdout

// ---- 任务日志 ----
const logs = ref([]);
const loading = ref(false);
const filterKind = ref('');
const filterStatus = ref('');
const filterUid = ref('');

async function loadTaskLogs() {
  loading.value = true;
  try {
    const r = await api.listLogs({ kind: filterKind.value || undefined, limit: 300 });
    logs.value = r.logs;
  } finally {
    loading.value = false;
  }
}

const filteredLogs = computed(() =>
  logs.value.filter((l) => {
    if (filterStatus.value && l.status !== filterStatus.value) return false;
    if (filterUid.value) {
      const q = filterUid.value.toLowerCase();
      if (!(l.uid ?? '').toLowerCase().includes(q) && !(l.nickname ?? '').toLowerCase().includes(q)) return false;
    }
    return true;
  }),
);

const KIND_OPTIONS = [{ value: '', label: '全部类型' }, ...Object.entries(TASK_KINDS).map(([value, label]) => ({ value, label }))];
const STATUS_OPTIONS = [
  { value: '', label: '全部状态' },
  ...Object.entries(LOG_STATUS).map(([value, s]) => ({ value, label: s.label })),
];

// ---- 请求日志(网关 /v1/chat/completions 表格行)----
const reqLogs = ref([]);
const reqMeta = ref({ available: true, error: '', container: '' });
const reqModel = ref('');
const reqResult = ref(''); // '' | ok | fail

async function loadRequestLogs() {
  const r = await api.gatewayRequestLogs(400);
  reqMeta.value = { available: r.available, error: r.error ?? '', container: r.container ?? '' };
  reqLogs.value = r.logs ?? [];
}

const filteredReqLogs = computed(() =>
  reqLogs.value.filter((l) => {
    if (reqModel.value && !(l.model ?? '').toLowerCase().includes(reqModel.value.toLowerCase())) return false;
    if (reqResult.value === 'ok' && (l.status < 200 || l.status >= 300)) return false;
    if (reqResult.value === 'fail' && l.status < 400) return false;
    return true;
  }),
);

function statusCls(code) {
  if (code >= 200 && code < 300) return 'text-success';
  if (code >= 400 && code < 500) return 'text-warning';
  if (code >= 500) return 'text-destructive';
  return 'text-muted-foreground';
}

// ---- 网关 stdout 原始行 ----
const stdoutLines = ref([]);
const stdoutMeta = ref({ available: true, error: '' });

async function loadStdout() {
  const r = await api.gatewayStdout(200);
  stdoutMeta.value = { available: r.available, error: r.error ?? '' };
  stdoutLines.value = r.lines ?? [];
}

// ---- 轮询:只刷当前页签 ----
let timer = null;
async function loadActive() {
  try {
    if (tab.value === 'tasks') await loadTaskLogs();
    else if (tab.value === 'requests') await loadRequestLogs();
    else await loadStdout();
  } catch { /* 面板未就绪时静默 */ }
}

const TABS = [
  { value: 'tasks', label: '任务日志' },
  { value: 'requests', label: '请求日志' },
  { value: 'stdout', label: '网关日志' },
];

onMounted(() => {
  loadActive();
  timer = setInterval(loadActive, 5000);
});
onBeforeUnmount(() => clearInterval(timer));
</script>

<template>
  <div class="p-6 lg:p-8">
    <PageHeader title="日志" description="任务执行记录 + 网关请求级表格日志(docker stdout 只读采集)">
      <Button variant="outline" size="sm" :loading="loading" @click="loadActive"><RefreshCw v-if="!loading" />刷新</Button>
    </PageHeader>

    <!-- 页签 -->
    <div class="mb-4 flex overflow-hidden rounded-md border border-input w-fit">
      <button
        v-for="t in TABS"
        :key="t.value"
        @click="tab = t.value; loadActive()"
        class="px-4 py-1.5 text-sm font-medium transition-colors"
        :class="tab === t.value ? 'bg-primary text-primary-foreground' : 'bg-card text-muted-foreground hover:bg-accent'"
      >{{ t.label }}</button>
    </div>

    <!-- ============ 任务日志 ============ -->
    <template v-if="tab === 'tasks'">
      <div class="mb-4 flex flex-wrap items-center gap-2">
        <div class="flex overflow-hidden rounded-md border border-input">
          <button
            v-for="o in KIND_OPTIONS"
            :key="o.value"
            @click="filterKind = o.value; loadTaskLogs()"
            class="px-3 py-1.5 text-xs font-medium transition-colors"
            :class="filterKind === o.value ? 'bg-primary text-primary-foreground' : 'bg-card text-muted-foreground hover:bg-accent'"
          >{{ o.label }}</button>
        </div>
        <div class="flex overflow-hidden rounded-md border border-input">
          <button
            v-for="o in STATUS_OPTIONS"
            :key="o.value"
            @click="filterStatus = o.value"
            class="px-3 py-1.5 text-xs font-medium transition-colors"
            :class="filterStatus === o.value ? 'bg-primary text-primary-foreground' : 'bg-card text-muted-foreground hover:bg-accent'"
          >{{ o.label }}</button>
        </div>
        <Input v-model="filterUid" placeholder="按昵称 / uid 过滤" class="h-8 w-48" />
      </div>

      <Card class="wb-enter">
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-border text-left text-xs text-muted-foreground">
                <th class="px-4 py-3 font-medium">时间</th>
                <th class="px-4 py-3 font-medium">类型</th>
                <th class="px-4 py-3 font-medium">账号</th>
                <th class="px-4 py-3 font-medium">动作</th>
                <th class="px-4 py-3 font-medium">状态</th>
                <th class="px-4 py-3 font-medium">详情</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-border">
              <tr v-for="(l, i) in filteredLogs" :key="l.ts + '-' + i" class="hover:bg-muted/40">
                <td class="whitespace-nowrap px-4 py-2.5 font-mono text-xs text-muted-foreground">{{ fmtTimeFull(l.ts) }}</td>
                <td class="px-4 py-2.5"><Badge variant="secondary">{{ kindLabel(l.kind) }}</Badge></td>
                <td class="max-w-32 truncate px-4 py-2.5">{{ l.nickname || l.uid || '—' }}</td>
                <td class="whitespace-nowrap px-4 py-2.5 text-xs text-muted-foreground">{{ l.action }}</td>
                <td class="whitespace-nowrap px-4 py-2.5 text-xs font-medium" :class="LOG_STATUS[l.status]?.cls">
                  {{ LOG_STATUS[l.status]?.label ?? l.status }}
                </td>
                <td class="max-w-md truncate px-4 py-2.5 text-xs text-muted-foreground" :title="l.detail">{{ l.detail || '—' }}</td>
              </tr>
              <tr v-if="!filteredLogs.length">
                <td colspan="6" class="px-4 py-10 text-center text-muted-foreground">暂无匹配日志</td>
              </tr>
            </tbody>
          </table>
        </div>
      </Card>
    </template>

    <!-- ============ 请求日志 ============ -->
    <template v-else-if="tab === 'requests'">
      <div v-if="!reqMeta.available" class="mb-4 flex items-start gap-3 rounded-lg border border-warning/50 bg-warning/10 p-4 wb-enter">
        <TriangleAlert class="mt-0.5 h-4 w-4 shrink-0 text-warning" />
        <div class="text-sm">
          <p class="font-semibold">请求日志采集不可用</p>
          <p class="text-muted-foreground">{{ reqMeta.error }}</p>
          <p class="mt-1 text-muted-foreground">
            该功能通过 <span class="font-mono">docker logs</span> 只读采集网关容器 stdout。
            请在<RouterLink to="/settings" class="underline underline-offset-2">设置</RouterLink>中确认容器名(当前:{{ reqMeta.container || '未配置' }})。
          </p>
        </div>
      </div>

      <div class="mb-4 flex flex-wrap items-center gap-2">
        <div class="flex overflow-hidden rounded-md border border-input">
          <button
            v-for="o in [{ value: '', label: '全部结果' }, { value: 'ok', label: '成功' }, { value: 'fail', label: '失败' }]"
            :key="o.value"
            @click="reqResult = o.value"
            class="px-3 py-1.5 text-xs font-medium transition-colors"
            :class="reqResult === o.value ? 'bg-primary text-primary-foreground' : 'bg-card text-muted-foreground hover:bg-accent'"
          >{{ o.label }}</button>
        </div>
        <Input v-model="reqModel" placeholder="按模型过滤" class="h-8 w-44" />
        <span class="text-xs text-muted-foreground">容器 {{ reqMeta.container }} · 5s 增量采集</span>
      </div>

      <Card class="wb-enter">
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-border text-left text-xs text-muted-foreground">
                <th class="px-4 py-3 font-medium">#</th>
                <th class="px-4 py-3 font-medium">时间</th>
                <th class="px-4 py-3 font-medium">模型</th>
                <th class="px-4 py-3 font-medium">模式</th>
                <th class="px-4 py-3 font-medium">状态</th>
                <th class="px-4 py-3 font-medium">uid</th>
                <th class="px-4 py-3 font-medium text-right">TTFB</th>
                <th class="px-4 py-3 font-medium text-right">tokens</th>
                <th class="px-4 py-3 font-medium text-right">tok/s</th>
                <th class="px-4 py-3 font-medium text-right">总耗时</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-border">
              <tr v-for="l in filteredReqLogs" :key="l.ts + '-' + l.seq" class="hover:bg-muted/40">
                <td class="px-4 py-2.5 font-mono text-xs text-muted-foreground">{{ l.seq }}</td>
                <td class="whitespace-nowrap px-4 py-2.5 font-mono text-xs text-muted-foreground">{{ fmtTimeFull(l.ts) }}</td>
                <td class="px-4 py-2.5 font-medium">{{ l.model }}</td>
                <td class="px-4 py-2.5"><Badge variant="secondary">{{ l.mode }}</Badge></td>
                <td class="px-4 py-2.5 font-mono text-xs font-semibold" :class="statusCls(l.status)">{{ l.status }}</td>
                <td class="px-4 py-2.5 font-mono text-xs text-muted-foreground">{{ l.uid || '—' }}</td>
                <td class="px-4 py-2.5 text-right font-mono text-xs tabular-nums">{{ l.ttfbMs === null ? '—' : l.ttfbMs + 'ms' }}</td>
                <td class="px-4 py-2.5 text-right font-mono text-xs tabular-nums">{{ l.tokens ?? '—' }}</td>
                <td class="px-4 py-2.5 text-right font-mono text-xs tabular-nums">{{ l.tokPerSec ?? '—' }}</td>
                <td class="px-4 py-2.5 text-right font-mono text-xs tabular-nums">{{ l.totalSec.toFixed(1) }}s</td>
              </tr>
              <tr v-if="!filteredReqLogs.length">
                <td colspan="10" class="px-4 py-10 text-center text-muted-foreground">
                  {{ reqMeta.available ? '暂无请求记录:经网关发起一次 /v1/chat/completions 即会出现(含本页签上方的对话测试)' : '采集不可用' }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </Card>
    </template>

    <!-- ============ 网关 stdout ============ -->
    <template v-else>
      <Card class="wb-enter">
        <CardHeader>
          <CardTitle>网关日志 原始行</CardTitle>
          <CardDescription>非表格行的全部输出(启动 / 调度 / 错误),最新在前,保留最近 300 行</CardDescription>
        </CardHeader>
        <CardContent>
          <div v-if="!stdoutMeta.available" class="py-4 text-sm text-warning">{{ stdoutMeta.error }}</div>
          <div v-else-if="!stdoutLines.length" class="py-4 text-sm text-muted-foreground">暂无输出</div>
          <div v-else class="max-h-[560px] overflow-y-auto rounded-md bg-muted/50 p-3">
            <div v-for="(l, i) in stdoutLines" :key="l.ts + '-' + i" class="flex gap-3 py-0.5">
              <span class="shrink-0 font-mono text-xs text-muted-foreground">{{ fmtTimeFull(l.ts) }}</span>
              <span class="break-all font-mono text-xs">{{ l.raw }}</span>
            </div>
          </div>
        </CardContent>
      </Card>
    </template>
  </div>
</template>
