<script setup>
// 对话测试卡片:/v1/chat/completions 非流式,端到端验证网关链路。概览页与网关页共用。
import { onMounted, ref } from 'vue';
import { Send } from '@lucide/vue';
import { Card, CardHeader, CardTitle, CardDescription, CardContent, Button, Input, Label, Separator } from './ui/primitives';
import { api } from '../api';
import { toast } from '../stores/toast';

const models = ref([]);
const chatModel = ref('');
const chatPrompt = ref('你好,用一句话介绍你自己。');
const chatLoading = ref(false);
const chatResult = ref(null);

onMounted(async () => {
  try {
    const r = await api.gatewayModels();
    models.value = r.ok ? r.models : [];
  } catch { /* 网关不可达时选择框为空 */ }
});

async function sendChat() {
  if (!chatModel.value || !chatPrompt.value.trim()) {
    toast.warn('请选择模型并输入内容');
    return;
  }
  chatLoading.value = true;
  chatResult.value = null;
  try {
    const r = await api.gatewayChatTest(chatModel.value, chatPrompt.value.trim());
    if (r.ok) {
      chatResult.value = r;
    } else {
      toast.error(r.error, { title: '对话测试失败' });
    }
  } catch (e) {
    toast.error(e.message, { title: '对话测试失败' });
  } finally {
    chatLoading.value = false;
  }
}
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>对话测试</CardTitle>
      <!-- <CardDescription>/v1/chat/completions 非流式,端到端验证网关链路</CardDescription> -->
    </CardHeader>
    <CardContent class="space-y-3">
      <div class="grid grid-cols-3 gap-2">
        <div class="col-span-1 space-y-1.5">
          <Label>模型</Label>
          <select
            v-model="chatModel"
            class="flex h-9 w-full rounded-md border border-input bg-background px-2 text-sm text-foreground shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
          >
            <option value="" disabled>选择模型</option>
            <option v-for="m in models" :key="m.id" :value="m.id">{{ m.id }}</option>
          </select>
        </div>
        <div class="col-span-2 space-y-1.5">
          <Label>内容</Label>
          <div class="flex gap-2">
            <Input v-model="chatPrompt" placeholder="输入测试内容" @keydown.enter="sendChat" />
            <Button :loading="chatLoading" @click="sendChat"><Send v-if="!chatLoading" /></Button>
          </div>
        </div>
      </div>
      <template v-if="chatResult">
        <Separator />
        <div class="rounded-md bg-muted/50 p-3 text-sm">
          <p class="whitespace-pre-wrap">{{ chatResult.content || '(空响应)' }}</p>
          <p v-if="chatResult.usage" class="mt-2 text-xs text-muted-foreground">
            tokens: {{ chatResult.usage.total_tokens ?? '—' }}(输入 {{ chatResult.usage.prompt_tokens ?? '—' }} / 输出 {{ chatResult.usage.completion_tokens ?? '—' }})
          </p>
        </div>
      </template>
    </CardContent>
  </Card>
</template>
