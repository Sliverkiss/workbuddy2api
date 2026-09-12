<script setup>
// 可用模型卡片:/v1/models(面板侧透传,网关侧缓存 1h)。概览页与网关页共用。
import { onMounted, ref } from 'vue';
import { Card, CardHeader, CardTitle, CardDescription, CardContent, Badge } from './ui/primitives';
import { api } from '../api';

const models = ref([]);
const loaded = ref(false);

onMounted(async () => {
  try {
    const r = await api.gatewayModels();
    models.value = r.ok ? r.models : [];
  } catch { /* 网关不可达时空态展示 */ } finally {
    loaded.value = true;
  }
});
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>可用模型</CardTitle>
      <!-- <CardDescription>/v1/models(动态拉取,网关侧缓存 1h)</CardDescription> -->
    </CardHeader>
    <CardContent>
      <div v-if="!models.length" class="py-4 text-sm text-muted-foreground">
        {{ loaded ? '暂无模型数据(网关不可达或未配置 api_key)' : '加载中…' }}
      </div>
      <div v-else class="flex flex-wrap gap-2">
        <Badge v-for="m in models" :key="m.id" variant="secondary" class="px-2.5 py-1">{{ m.id }}</Badge>
      </div>
    </CardContent>
  </Card>
</template>
