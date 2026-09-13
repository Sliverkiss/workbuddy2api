<script setup>
// 设置页(集成版):访问密钥 / 主题 / 网关只读信息。
// 网关 config.json 容器内只读挂载,运行参数改 = 编辑宿主机文件 + 重启容器,故全部只读展示。
import { onMounted, ref } from 'vue';
import { Sun, Moon, Monitor, Check } from '@lucide/vue';
import PageHeader from '../components/PageHeader.vue';
import { Card, CardHeader, CardTitle, CardDescription, CardContent, Button, Input, Label, Separator } from '../components/ui/primitives';
import { getToken, setToken } from '../api';
import { toast } from '../stores/toast';
import { useAppStore } from '../stores/app';
import { applyTheme, getStoredTheme } from '../lib/theme';

const store = useAppStore();
const theme = ref(getStoredTheme());
const key = ref('');
const keySaved = ref(false);

onMounted(() => {
  key.value = getToken();
  if (!store.config) store.loadConfig().catch(() => {});
});

function saveKey() {
  setToken(key.value.trim());
  keySaved.value = true;
  setTimeout(() => (keySaved.value = false), 2000);
  store.init(); // 用新密钥重拉
  toast.success('访问密钥已保存');
}

function setTheme(mode) {
  theme.value = mode;
  applyTheme(mode);
}

const THEME_OPTIONS = [
  { value: 'system', label: '跟随系统', icon: Monitor },
  { value: 'light', label: '浅色', icon: Sun },
  { value: 'dark', label: '深色', icon: Moon },
];

function hoursText(h) {
  return Array.isArray(h) && h.length ? h.join(', ') + ' 点' : '—';
}
</script>

<template>
  <div class="p-6 lg:p-8">
    <PageHeader title="设置" description="访问密钥、主题与网关运行信息" />

    <div class="max-w-3xl space-y-6">
      <!-- 访问密钥 -->
      <Card class="wb-enter">
        <CardHeader>
          <CardTitle>访问密钥</CardTitle>
          <CardDescription>管理接口与网关 /v1 共用 config.json 的 api_key;密钥仅存本机浏览器</CardDescription>
        </CardHeader>
        <CardContent class="space-y-4">
          <div class="space-y-1.5">
            <Label for="apikey">api_key{{ store.config?.apiKeyRequired ? '' : '(网关未设置,可留空)' }}</Label>
            <div class="flex gap-2">
              <Input id="apikey" v-model="key" type="password" placeholder="config.json 中的 api_key" />
              <Button variant="outline" @click="saveKey"><Check v-if="!keySaved" :size="15" />{{ keySaved ? '已保存' : '保存' }}</Button>
            </div>
          </div>
        </CardContent>
      </Card>

      <!-- 主题 -->
      <Card class="wb-enter wb-enter-1">
        <CardHeader>
          <CardTitle>主题</CardTitle>
        </CardHeader>
        <CardContent>
          <div class="flex gap-2">
            <Button
              v-for="o in THEME_OPTIONS"
              :key="o.value"
              :variant="theme === o.value ? 'default' : 'outline'"
              size="sm"
              @click="setTheme(o.value)"
            >
              <component :is="o.icon" />{{ o.label }}
            </Button>
          </div>
        </CardContent>
      </Card>

      <!-- 网关信息(只读) -->
      <Card class="wb-enter wb-enter-2">
        <CardHeader>
          <CardTitle>网关信息</CardTitle>
          <CardDescription>来自网关进程(config.json 只读挂载;修改 = 编辑宿主机文件 + 重启容器)</CardDescription>
        </CardHeader>
        <CardContent class="space-y-3 text-sm">
          <div class="flex justify-between gap-4">
            <span class="text-muted-foreground">凭证目录</span>
            <span class="font-mono text-xs break-all text-right">{{ store.config?.authDir || '—' }}</span>
          </div>
          <div class="flex justify-between gap-4">
            <span class="text-muted-foreground">鉴权</span>
            <span>{{ store.config?.apiKeyRequired ? '已启用 api_key' : '未启用' }}</span>
          </div>
          <Separator />
          <div class="flex justify-between gap-4">
            <span class="text-muted-foreground">签到排程</span>
            <span>{{ store.config?.schedule?.checkin_enabled ? hoursText(store.config?.schedule?.checkin_hours) : '已禁用' }}</span>
          </div>
          <div class="flex justify-between gap-4">
            <span class="text-muted-foreground">保活排程</span>
            <span>{{ store.config?.schedule?.keepalive_enabled ? hoursText(store.config?.schedule?.keepalive_hours) : '已禁用' }}</span>
          </div>
          <div class="flex justify-between gap-4">
            <span class="text-muted-foreground">旅行排程</span>
            <span>{{ store.config?.schedule?.travel_enabled ? hoursText(store.config?.schedule?.travel_hours) : '已禁用' }}</span>
          </div>
          <div class="flex justify-between gap-4">
            <span class="text-muted-foreground">活跃排程</span>
            <span>{{ store.config?.schedule?.activity_enabled ? hoursText(store.config?.schedule?.activity_hours) : '已禁用' }}</span>
          </div>
        </CardContent>
      </Card>

      <!-- 数据说明 -->
      <Card class="wb-enter wb-enter-3">
        <CardHeader>
          <CardTitle>数据与边界</CardTitle>
        </CardHeader>
        <CardContent class="space-y-2 text-sm text-muted-foreground">
          <p>· 管理台内嵌于网关进程,账号池 / 上游客户端 / 调度器与 /v1 完全同源。</p>
          <p>· 管理台数据(探测缓存 / 积分快照 / 任务事件)存放于 <span class="font-mono">data/web-state.json</span>;网关池状态在 <span class="font-mono">data/state.json</span>。</p>
          <p>· 定时自动化(签到 / 保活 / 旅行 / 活跃)由网关调度器持有,管理台只读展示;「一键任务」为手动同步触发。</p>
        </CardContent>
      </Card>
    </div>
  </div>
</template>
