<script setup>
// AuthGate 访问密钥门:/api 返回 401 时弹出,输入网关 api_key 后重试。
// 密钥仅存 localStorage(wbweb.token),随每个请求以 Bearer 注入。
import { ref } from 'vue';
import { KeyRound } from '@lucide/vue';
import { Button, Input } from './ui/primitives';
import { setToken } from '../api';

const props = defineProps({ open: { type: Boolean, default: false } });
const emit = defineEmits(['update:open', 'unlocked']);

const key = ref('');
const saving = ref(false);

async function submit() {
  if (!key.value.trim()) return;
  saving.value = true;
  setToken(key.value.trim());
  emit('update:open', false);
  emit('unlocked');
  saving.value = false;
  key.value = '';
}
</script>

<template>
  <Teleport to="body">
    <div v-if="props.open" class="fixed inset-0 z-50 flex items-center justify-center bg-background/80 backdrop-blur-sm">
      <form class="w-[380px] rounded-xl border border-border bg-card p-6 shadow-lg wb-enter" @submit.prevent="submit">
        <div class="flex items-center gap-2.5">
          <div class="flex h-9 w-9 items-center justify-center rounded-lg bg-primary/10">
            <KeyRound :size="17" class="text-primary" />
          </div>
          <div>
            <h2 class="text-sm font-semibold">需要访问密钥</h2>
            <p class="text-xs text-muted-foreground">管理接口与网关 /v1 共用同一把 api_key</p>
          </div>
        </div>
        <Input
          v-model="key"
          type="password"
          placeholder="输入 config.json 中的 api_key"
          class="mt-4"
          autofocus
        />
        <Button type="submit" class="mt-4 w-full" :loading="saving" :disabled="!key.trim()">解锁</Button>
        <p class="mt-3 text-center text-xs text-muted-foreground">密钥仅保存在本机浏览器 localStorage</p>
      </form>
    </div>
  </Teleport>
</template>
