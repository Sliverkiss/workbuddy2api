<script setup>
// 账号卡片:昵称 + 上次刷新时间 + 签到日历点 + 更多菜单(全部操作)/ 猫猫旅行状态机(实时倒计时)/ 积分三段 / 近期到期两条。
// 数据来源:只读探测(probe)落库的 liveStatus;探测无副作用(不签到/不派出/不领奖)。
import { computed, ref, onMounted, onBeforeUnmount, watch } from 'vue';
import {
  CalendarCheck, HeartPulse, Cat, Radio, RefreshCw, Trash2, Coins,
  MapPin, Gift, CircleAlert, AlarmClock, EllipsisVertical,
} from '@lucide/vue';
import { Card, Badge, Button } from './ui/primitives';
import { api } from '../api';
import { toast } from '../stores/toast';
import { fmtNum, fmtAgo, fmtDate, fmtTimeFull, fmtCountdown, kindLabel } from '../lib/format';

const props = defineProps({
  account: { type: Object, required: true },
});
const emit = defineEmits(['refresh', 'delete']);

const a = computed(() => props.account);
const live = computed(() => a.value.liveStatus ?? null);

// ---------------- 操作(更多菜单内) ----------------
const busyKind = ref('');
async function run(kind) {
  busyKind.value = kind;
  try {
    const r = await api.runTaskOne(props.account.uid, kind);
    const res = r.result;
    const label = kindLabel(kind);
    if (res.status === 'ok') {
      const extra = res.remain !== undefined && res.remain !== null ? `,积分 ${fmtNum(res.remain)}`
        : res.reward ? `,奖励 ${res.reward} 积分`
        : res.streakDays ? `,连登 ${res.streakDays} 天` : '';
      toast.success(`${props.account.nickname || props.account.uid}:${label}成功${extra}`);
    } else if (res.status === 'already') {
      toast(`${props.account.nickname || props.account.uid}:今日已签到`, { type: 'info' });
    } else if (res.status === 'skip') {
      toast.warn(`${props.account.nickname || props.account.uid}:${label}跳过 — ${res.detail}`);
    } else if (res.status === 'warn') {
      toast.warn(`${props.account.nickname || props.account.uid}:${label}警告 — ${res.detail}`);
    } else {
      toast.error(`${props.account.nickname || props.account.uid}:${label}失败 — ${res.detail}`);
    }
    emit('refresh');
    if (kind === 'travel') await probe(); // 旅行状态机已变化,立即重探
  } catch (e) {
    toast.error(e.message, { title: `${kindLabel(kind)}执行失败` });
  } finally {
    busyKind.value = '';
  }
}

const probing = ref(false);
async function probe() {
  probing.value = true;
  try {
    const r = await api.probeAccount(props.account.uid);
    const errs = Object.keys(r.status?.errors ?? {});
    if (errs.length) toast.warn(`${props.account.nickname || props.account.uid}:探测部分失败(${errs.join('/')})`);
    emit('refresh');
  } catch (e) {
    toast.error(e.message, { title: '状态探测失败' });
  } finally {
    probing.value = false;
  }
}

// ---------------- 更多菜单 ----------------
const menuOpen = ref(false);
const menuRoot = ref(null);
function onDocClick(e) {
  if (menuRoot.value && !menuRoot.value.contains(e.target)) menuOpen.value = false;
}
function onDocKey(e) {
  if (e.key === 'Escape') menuOpen.value = false;
}
watch(menuOpen, (v) => {
  if (v) {
    document.addEventListener('mousedown', onDocClick);
    document.addEventListener('keydown', onDocKey);
  } else {
    document.removeEventListener('mousedown', onDocClick);
    document.removeEventListener('keydown', onDocKey);
  }
});
onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocClick);
  document.removeEventListener('keydown', onDocKey);
});

async function menuRun(kind) {
  menuOpen.value = false;
  await run(kind);
}
function menuDelete() {
  menuOpen.value = false;
  emit('delete', a.value);
}

// 合并刷新:只读探测(签到/旅行/积分明细) + 积分任务刷新,并发执行,汇总提示。
const refreshingOne = ref(false);
async function menuRefresh() {
  menuOpen.value = false;
  refreshingOne.value = true;
  const nick = props.account.nickname || props.account.uid;
  try {
    const [p, c] = await Promise.allSettled([
      api.probeAccount(props.account.uid),
      api.runTaskOne(props.account.uid, 'credit'),
    ]);
    const problems = [];
    let remain = null;
    if (p.status === 'fulfilled') {
      const failed = Object.keys(p.value.status?.errors ?? {});
      if (failed.length) problems.push('探测:' + failed.join('/'));
      remain = p.value.status?.credits?.remain ?? null;
    } else problems.push('探测失败:' + p.reason.message);
    if (c.status === 'fulfilled') {
      if (c.value.result.status === 'fail') problems.push('积分:' + c.value.result.detail);
      remain = c.value.result.remain ?? remain;
    } else problems.push('积分刷新失败:' + c.reason.message);
    if (problems.length) toast.warn(`${nick}:刷新部分失败(${problems.join(';')})`);
    else toast.success(`${nick}:刷新完成${remain !== null ? `,积分 ${fmtNum(remain)}` : ''}`);
    emit('refresh');
  } finally {
    refreshingOne.value = false;
  }
}

const MENU_ACTIONS = [
  { kind: 'checkin', label: '签到', icon: CalendarCheck },
  { kind: 'keepalive', label: '保活', icon: HeartPulse },
  { kind: 'travel', label: '旅行', icon: Cat },
  { kind: 'activity', label: '活跃', icon: Radio },
];

// ---------------- 签到状态(日历点) ----------------
const checkinState = computed(() => {
  if (!live.value) return { cls: 'text-muted-foreground/40', tip: '签到状态未探测' };
  if (live.value.errors?.checkin) return { cls: 'text-warning', tip: '签到状态查询失败' };
  return live.value.checkin?.checked
    ? { cls: 'text-success', tip: '今日已签到' }
    : { cls: 'text-muted-foreground/40', tip: '今日未签到' };
});

// ---------------- 猫猫旅行(实时倒计时) ----------------
// 服务器时钟偏移:serverNowMs 与探测时刻本地钟对齐,倒计时用偏移校正。
const clockOffset = computed(() => {
  const t = live.value?.travel;
  if (!t?.serverNowMs || !live.value?.ts) return 0;
  return t.serverNowMs - live.value.ts;
});
const nowMs = ref(Date.now());
let tickTimer = null;
onMounted(() => { tickTimer = setInterval(() => { nowMs.value = Date.now(); }, 1000); });
onBeforeUnmount(() => clearInterval(tickTimer));

const travelRemainMs = computed(() => {
  const t = live.value?.travel;
  if (!t?.arriveAtMs) return null;
  return t.arriveAtMs - (nowMs.value + clockOffset.value);
});

// 真实行程进度:depart → arrive 区间上的已行进比例。
const travelPct = computed(() => {
  const t = live.value?.travel;
  if (!t?.arriveAtMs || !t?.departAtMs || t.arriveAtMs <= t.departAtMs) return null;
  const p = ((nowMs.value + clockOffset.value - t.departAtMs) / (t.arriveAtMs - t.departAtMs)) * 100;
  return Math.max(0, Math.min(100, p));
});

const travelView = computed(() => {
  if (!live.value) return { kind: 'unknown' };
  if (live.value.errors?.travel && live.value.errors?.buddy) return { kind: 'error', text: '旅行状态查询失败' };
  if (live.value.buddy && !live.value.buddy.has) return { kind: 'no-buddy' };
  const t = live.value.travel;
  if (!t) return { kind: 'unknown' };
  switch (t.state) {
    case 'traveling':
      return {
        kind: 'traveling',
        location: t.locationName || '未知地点',
        buddy: live.value.buddy?.name ?? '',
        remainMs: travelRemainMs.value,
        reward: t.rewardCredit || null,
      };
    case 'arrived':
      return { kind: 'arrived', location: t.locationName || '', reward: t.rewardCredit || null, recordId: t.recordId };
    case 'idle':
      return t.dailyLimitReached ? { kind: 'finished' } : { kind: 'not-started' };
    default:
      return t.dailyLimitReached ? { kind: 'finished' } : { kind: 'not-started' };
  }
});

// 倒计时归零:旅行中 → 自动重探(应翻为 arrived 可领取)
let arrivalProbing = false;
watch(travelRemainMs, async (ms) => {
  if (travelView.value.kind !== 'traveling') return;
  if (ms !== null && ms <= 0 && !arrivalProbing && !probing.value) {
    arrivalProbing = true;
    try { await probe(); } finally { arrivalProbing = false; }
  }
});

// ---------------- 积分三段 + 近期到期 ----------------
const credits = computed(() => live.value?.credits ?? null);
const creditRemain = computed(() => credits.value?.remain ?? a.value.lastCredit?.remain ?? null);
const usedPct = computed(() => {
  const c = credits.value;
  if (!c || !c.total) return 0;
  return Math.min(100, Math.round((c.used / c.total) * 100));
});

// 近期到期:未过期、有余量、有到期时间,最近两条「近期到期」口径)。
const expiringPackages = computed(() =>
  (credits.value?.packages ?? [])
    .filter((p) => !p.expired && p.expireAtMs && p.remaining > 0)
    .sort((x, y) => x.expireAtMs - y.expireAtMs)
    .slice(0, 2),
);

// 该条积分余量在总积分中的占比(进度条用)。
function sharePct(p) {
  const t = credits.value?.total ?? 0;
  if (!t) return 0;
  return Math.max(0, Math.min(100, (p.remaining / t) * 100));
}
</script>

<template>
  <Card class="flex flex-col gap-4 p-5 transition-shadow hover:shadow-md">
    <!-- 头行:昵称 + 上次刷新 + 签到日历点 + 更多菜单 -->
    <div class="flex items-center gap-3">
      <p class="truncate text-sm font-semibold">{{ a.nickname || '未命名账号' }}</p>
      <span
        class="flex shrink-0 items-center gap-1 text-xs text-muted-foreground"
        :title="`上一次刷新时间:${fmtTimeFull(a.lastRefreshMs)}`"
      >
        <AlarmClock :size="12" />
        {{ a.lastRefreshMs ? fmtAgo(a.lastRefreshMs) : '—' }}
      </span>
      <Badge v-if="a.sessionDeadFails > 0" variant="destructive">12153 ×{{ a.sessionDeadFails }}</Badge>
      <CircleAlert
        v-if="live?.errors && Object.keys(live.errors).length"
        :size="14"
        class="shrink-0 text-warning"
      />
      <div class="ml-auto flex shrink-0 items-center gap-1.5">
        <span :title="checkinState.tip" class="flex items-center">
          <CalendarCheck :size="16" :class="checkinState.cls" />
        </span>
        <div ref="menuRoot" class="relative">
          <button
            class="flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
            title="更多操作"
            @click="menuOpen = !menuOpen"
          >
            <EllipsisVertical :size="16" />
          </button>
          <Transition name="wb-menu">
            <div
              v-if="menuOpen"
              class="absolute right-0 top-full z-20 mt-1 w-40 overflow-hidden rounded-md border border-border bg-popover py-1 shadow-lg"
            >
              <button
                v-for="act in MENU_ACTIONS"
                :key="act.kind"
                class="flex w-full items-center gap-2.5 px-3 py-2 text-sm transition-colors hover:bg-accent disabled:opacity-50"
                :disabled="!!busyKind"
                @click="menuRun(act.kind)"
              >
                <component :is="act.icon" :size="14" class="text-muted-foreground" />
                {{ act.label }}
                <span v-if="busyKind === act.kind" class="ml-auto h-3 w-3 animate-spin rounded-full border border-border border-t-primary" />
              </button>
              <button
                class="flex w-full items-center gap-2.5 px-3 py-2 text-sm transition-colors hover:bg-accent disabled:opacity-50"
                :disabled="refreshingOne"
                title="同时执行状态探测与积分刷新"
                @click="menuRefresh"
              >
                <RefreshCw :size="14" class="text-muted-foreground" />
                刷新
                <span v-if="refreshingOne" class="ml-auto h-3 w-3 animate-spin rounded-full border border-border border-t-primary" />
              </button>
              <div class="my-1 border-t border-border" />
              <button
                class="flex w-full items-center gap-2.5 px-3 py-2 text-sm text-destructive transition-colors hover:bg-destructive/10"
                @click="menuDelete"
              >
                <Trash2 :size="14" />
                删除凭证
              </button>
            </div>
          </Transition>
        </div>
      </div>
    </div>

    <!-- 积分三段 -->
    <div class="space-y-1.5">
      <div class="flex items-baseline gap-2">
        <Coins :size="14" class="text-success translate-y-0.5" />
        <span class="text-2xl font-bold tabular-nums">{{ creditRemain === null ? '—' : fmtNum(creditRemain) }}</span>
        <span class="text-xs text-muted-foreground">剩余积分</span>
        <span v-if="credits" class="ml-auto text-xs text-muted-foreground tabular-nums">
          总量 {{ fmtNum(credits.total) }} · 已用 {{ fmtNum(credits.used) }}
        </span>
      </div>
      <div v-if="credits && credits.total > 0" class="h-1.5 overflow-hidden rounded-full bg-muted">
        <div class="h-full rounded-full bg-success/80 transition-all" :style="{ width: usedPct + '%' }" />
      </div>
    </div>

    <!-- 近期到期(最近两条;进度条 = 该条余量占总积分比例) -->
    <div v-if="expiringPackages.length" class="space-y-2 rounded-md border border-border px-3 py-2">
      <p class="text-xs text-muted-foreground">近期到期</p>
      <div v-for="(p, i) in expiringPackages" :key="p.code || i" class="space-y-1">
        <div class="flex items-baseline justify-between gap-2">
          <p class="text-sm font-semibold text-warning tabular-nums">{{ fmtNum(p.remaining) }}积分</p>
          <p class="shrink-0 text-xs text-warning tabular-nums">{{ fmtDate(p.expireAtMs) }} 到期</p>
        </div>
        <div class="h-1 overflow-hidden rounded-full bg-muted">
          <div class="h-full rounded-full bg-warning/80 transition-all" :style="{ width: sharePct(p) + '%' }" />
        </div>
      </div>
    </div>

    <!-- 猫猫旅行状态机 -->
    <div class="rounded-md border border-border p-3">
      <div class="flex items-center gap-2 text-xs text-muted-foreground mb-1.5">
        <Cat :size="13" />
        <span>猫猫旅行</span>
        <span v-if="live" class="ml-auto">{{ fmtAgo(live.ts) }}探测</span>
      </div>

      <div v-if="travelView.kind === 'unknown'" class="text-sm text-muted-foreground">
        未探测 — 更多菜单中选「刷新」
      </div>
      <div v-else-if="travelView.kind === 'error'" class="text-sm text-warning">{{ travelView.text }}</div>

      <div v-else-if="travelView.kind === 'no-buddy'" class="flex items-center justify-between gap-2">
        <p class="text-sm">还没领养猫猫</p>
        <Button size="xs" variant="outline" :loading="busyKind === 'travel'" @click="run('travel')">去领养</Button>
      </div>

      <div v-else-if="travelView.kind === 'traveling'" class="space-y-1">
        <p class="text-sm">
          <span v-if="travelView.buddy" class="font-medium">{{ travelView.buddy }}</span>
          正在
          <span class="font-medium text-primary"><MapPin class="inline h-3.5 w-3.5 -translate-y-px" />{{ travelView.location }}</span>
          旅行
        </p>
        <p class="text-xs text-muted-foreground tabular-nums">
          剩余
          <span class="font-semibold text-foreground">{{ fmtCountdown(travelView.remainMs) ?? '即将到达…' }}</span>
          <span v-if="travelView.reward"> · 预计奖励 {{ travelView.reward }} 积分</span>
        </p>
        <div v-if="travelPct !== null" class="h-1 overflow-hidden rounded-full bg-muted">
          <div class="h-full rounded-full bg-primary/70 transition-[width] duration-1000 ease-linear" :style="{ width: travelPct + '%' }" />
        </div>
      </div>

      <div v-else-if="travelView.kind === 'arrived'" class="flex items-center justify-between gap-2">
        <p class="text-sm">
          <Gift class="inline h-3.5 w-3.5 -translate-y-px text-success" />
          已到达<span v-if="travelView.location"> {{ travelView.location }}</span>
          <span v-if="travelView.reward" class="font-semibold text-success"> · 可领 {{ travelView.reward }} 积分</span>
        </p>
        <Button size="xs" variant="success" :loading="busyKind === 'travel'" @click="run('travel')">领取奖励</Button>
      </div>

      <div v-else-if="travelView.kind === 'finished'" class="text-sm text-muted-foreground">
        今日旅行已结束,明天再来吧
      </div>

      <div v-else class="flex items-center justify-between gap-2">
        <p class="text-sm text-muted-foreground">今日未开始旅行</p>
        <Button size="xs" variant="outline" :loading="busyKind === 'travel'" @click="run('travel')">派出猫猫</Button>
      </div>
    </div>
  </Card>
</template>

<style scoped>
.wb-menu-enter-active,
.wb-menu-leave-active {
  transition: opacity 0.12s ease, transform 0.12s ease;
}
.wb-menu-enter-from,
.wb-menu-leave-to {
  opacity: 0;
  transform: translateY(-4px) scale(0.98);
}
</style>
