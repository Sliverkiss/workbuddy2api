<script setup>
// OAuth 设备登录对话框:发起 → 打开授权页 → 轮询 → 落盘 auths/。
import { ref, watch, onBeforeUnmount } from 'vue';
import { ExternalLink, Copy, CircleCheck } from '@lucide/vue';
import UiDialog from './ui/UiDialog.vue';
import { Button } from './ui/primitives';
import { api } from '../api';
import { toast } from '../stores/toast';

const props = defineProps({ open: { type: Boolean, default: false } });
const emit = defineEmits(['close', 'done']);

const phase = ref('idle'); // idle | waiting | done | expired | error
const authUrl = ref('');
const state = ref('');
const account = ref(null);
const errorMsg = ref('');
let pollTimer = null;

function stopPoll() {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = null;
}

async function begin() {
  phase.value = 'idle';
  errorMsg.value = '';
  try {
    const r = await api.oauthBegin();
    state.value = r.state;
    authUrl.value = r.authUrl;
    phase.value = 'waiting';
    window.open(r.authUrl, '_blank', 'noopener');
    pollTimer = setInterval(poll, 2000);
  } catch (e) {
    errorMsg.value = e.message;
    phase.value = 'error';
  }
}

async function poll() {
  try {
    const r = await api.oauthPoll(state.value);
    if (r.status === 'authorized') {
      stopPoll();
      account.value = r.saved;
      phase.value = 'done';
      emit('done');
    } else if (r.status === 'timeout') {
      stopPoll();
      phase.value = 'expired';
    } else if (r.status === 'error') {
      throw new Error(r.error || '授权失败');
    }
    // 'pending' 继续轮询
  } catch (e) {
    stopPoll();
    errorMsg.value = e.message;
    phase.value = 'error';
  }
}

function copyUrl() {
  navigator.clipboard?.writeText(authUrl.value);
  toast.success('授权链接已复制');
}

function openUrl() {
  window.open(authUrl.value, '_blank', 'noopener');
}

function close() {
  stopPoll();
  if (state.value && phase.value === 'waiting') api.oauthCancel(state.value).catch(() => {});
  emit('close');
}

watch(() => props.open, (v) => {
  if (v) {
    state.value = '';
    authUrl.value = '';
    account.value = null;
    begin();
  } else {
    stopPoll();
  }
});
onBeforeUnmount(stopPoll);
</script>

<template>
  <UiDialog :open="open" title="扫码 / 浏览器登录" description="OAuth 设备授权流程:在打开的页面完成登录,凭证自动写入 auths 目录" @close="close">
    <div v-if="phase === 'idle'" class="flex items-center gap-2 text-sm text-muted-foreground">
      <span class="h-4 w-4 animate-spin rounded-full border-2 border-border border-t-primary" />
      正在获取授权链接…
    </div>

    <div v-else-if="phase === 'waiting'" class="space-y-4">
      <div class="flex items-center gap-2 text-sm">
        <span class="h-4 w-4 animate-spin rounded-full border-2 border-border border-t-primary" />
        等待浏览器中完成登录…(10 分钟内有效)
      </div>
      <div class="rounded-md border border-border bg-muted/50 p-3">
        <p class="mb-2 text-xs text-muted-foreground">若浏览器未自动打开,请复制链接:</p>
        <p class="break-all font-mono text-xs">{{ authUrl }}</p>
      </div>
      <div class="flex gap-2">
        <Button variant="outline" size="sm" @click="copyUrl"><Copy />复制链接</Button>
        <Button variant="outline" size="sm" @click="openUrl"><ExternalLink />重新打开</Button>
      </div>
    </div>

    <div v-else-if="phase === 'done'" class="flex items-center gap-3 rounded-md border border-success/40 bg-success/10 p-4">
      <CircleCheck class="h-5 w-5 text-success" />
      <div class="text-sm">
        <p class="font-semibold">登录成功</p>
        <p class="text-muted-foreground">{{ account?.nickname || account?.uid }} 已加入账号池</p>
      </div>
    </div>

    <div v-else-if="phase === 'expired'" class="space-y-3">
      <p class="text-sm text-warning">授权已过期(10 分钟),请重新发起。</p>
      <Button size="sm" @click="begin">重新登录</Button>
    </div>

    <div v-else-if="phase === 'error'" class="space-y-3">
      <p class="text-sm text-destructive">{{ errorMsg }}</p>
      <Button size="sm" variant="outline" @click="begin">重试</Button>
    </div>

    <template #footer>
      <Button variant="secondary" @click="close">{{ phase === 'done' ? '完成' : '取消' }}</Button>
    </template>
  </UiDialog>
</template>
