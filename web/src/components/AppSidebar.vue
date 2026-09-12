<script setup>
// 左侧导航:品牌区 + 六页导航 + 底部网关状态与主题切换。
import { computed, ref, onMounted } from 'vue';
import { useRoute } from 'vue-router';
import {
  LayoutDashboard, Users, TimerReset, ScrollText, Settings,
  Sun, Moon, Monitor,
} from '@lucide/vue';
import logoUrl from '../assets/logo.svg';
import { useAppStore } from '../stores/app';
import { applyTheme, getStoredTheme } from '../lib/theme';

const route = useRoute();
const store = useAppStore();

const NAV = [
  { to: '/', label: '概览', icon: LayoutDashboard },
  { to: '/accounts', label: '账号', icon: Users },
  { to: '/automation', label: '自动化', icon: TimerReset },
  { to: '/logs', label: '日志', icon: ScrollText },
  { to: '/settings', label: '设置', icon: Settings },
];

const theme = ref(getStoredTheme());
const THEME_CYCLE = { system: 'light', light: 'dark', dark: 'system' };
const themeIcon = computed(() => (theme.value === 'dark' ? Moon : theme.value === 'light' ? Sun : Monitor));
const themeLabel = computed(() => ({ system: '跟随系统', light: '浅色', dark: '深色' }[theme.value]));
function cycleTheme() {
  theme.value = THEME_CYCLE[theme.value];
  applyTheme(theme.value);
}

const gwState = computed(() => {
  const h = store.gatewayHealth;
  if (!h) return { cls: 'bg-muted-foreground', text: '检测中' };
  if (!h.reachable) return { cls: 'bg-destructive', text: '不可达' };
  if (!h.isWorkbuddy2api && !h.legacyCompatible) return { cls: 'bg-warning', text: '非本网关' };
  if (h.legacyCompatible && !h.isWorkbuddy2api) return { cls: 'bg-success', text: `旧版` };
  if (h.httpStatus !== 200) return { cls: 'bg-warning', text: `可用` };
  // return { cls: 'bg-success', text: `${h.healthy}/${h.total} 健康` };
  return { cls: 'bg-success', text: `已连接` };
});

onMounted(() => store.loadGatewayHealth().catch(() => {}));
</script>

<template>
  <aside class="flex h-full w-56 shrink-0 flex-col border-r border-border bg-card">
    <div class="flex items-center gap-2.5 px-5 pb-5 pt-6">
      <img :src="logoUrl" alt="WorkBuddy" class="h-8 w-8 rounded-lg" />
      <div class="leading-tight">
        <p class="text-sm font-semibold">WorkBuddy2API</p>
        <p class="text-xs text-muted-foreground">账号管理面板</p>
      </div>
    </div>

    <nav class="flex-1 space-y-1 px-3">
      <RouterLink
        v-for="item in NAV"
        :key="item.to"
        :to="item.to"
        custom
        v-slot="{ navigate, isActive }"
      >
        <button
          @click="navigate"
          class="flex w-full items-center gap-2.5 rounded-md px-3 py-2 text-sm font-medium transition-colors"
          :class="isActive
            ? 'bg-accent text-accent-foreground'
            : 'text-muted-foreground hover:bg-accent/60 hover:text-accent-foreground'"
        >
          <component :is="item.icon" :size="16" />
          {{ item.label }}
        </button>
      </RouterLink>
    </nav>

    <div class="space-y-2 border-t border-border p-3">
      <div class="flex items-center gap-2 rounded-md px-3 py-2 text-xs text-muted-foreground" title="workbuddy2api 网关健康(/healthz)">
        <span class="h-2 w-2 rounded-full wb-pulse-dot" :class="gwState.cls" />
        <span>workbuddy2api · {{ gwState.text }}</span>
      </div>
      <button
        @click="cycleTheme"
        class="flex w-full items-center gap-2 rounded-md px-3 py-2 text-xs text-muted-foreground hover:bg-accent/60"
      >
        <component :is="themeIcon" :size="14" />
        <span>主题:{{ themeLabel }}</span>
      </button>
    </div>
  </aside>
</template>
