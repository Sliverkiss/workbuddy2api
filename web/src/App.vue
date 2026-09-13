<script setup>
import { onMounted, onBeforeUnmount, ref, watch } from 'vue';
import { useRoute } from 'vue-router';
import AppSidebar from './components/AppSidebar.vue';
import AuthGate from './components/AuthGate.vue';
import UiToaster from './components/ui/UiToaster.vue';
import { useAppStore } from './stores/app';
import { toast } from './stores/toast';

const route = useRoute();
const store = useAppStore();
const gateOpen = ref(false);

watch(
  () => route.meta.title,
  (t) => { 'WorkBuddy2API'; },
  { immediate: true },
);

function onUnauthorized() {
  gateOpen.value = true;
}

async function boot() {
  await store.init();
  if (store.lastError) toast.error(store.lastError, { title: '网关连接异常' });
  store.startPolling();
}

onMounted(async () => {
  window.addEventListener('wb:unauthorized', onUnauthorized);
  await boot();
});
onBeforeUnmount(() => {
  window.removeEventListener('wb:unauthorized', onUnauthorized);
  store.stopPolling();
});
</script>

<template>
  <div class="flex h-full">
    <AppSidebar />
    <main class="min-w-0 flex-1 overflow-y-auto">
      <RouterView />
    </main>
    <AuthGate v-model:open="gateOpen" @unlocked="boot" />
    <UiToaster />
  </div>
</template>
