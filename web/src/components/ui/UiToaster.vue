<script setup>
// 右上角通知堆叠,源自 stores/toast.js。
import { toasts, dismiss } from '../../stores/toast';
import { CircleCheck, TriangleAlert, CircleX, Info, X } from '@lucide/vue';

const ICONS = {
  success: CircleCheck,
  error: CircleX,
  warning: TriangleAlert,
  info: Info,
};
const CLS = {
  success: 'text-success',
  error: 'text-destructive',
  warning: 'text-warning',
  info: 'text-primary',
};
</script>

<template>
  <Teleport to="body">
    <div class="fixed right-4 top-4 z-[100] flex w-[min(92vw,360px)] flex-col gap-2">
      <div
        v-for="t in toasts"
        :key="t.id"
        class="flex items-start gap-2.5 rounded-lg border border-border bg-card p-3.5 shadow-lg wb-enter"
      >
        <component :is="ICONS[t.type] ?? Info" class="mt-0.5 h-4 w-4 shrink-0" :class="CLS[t.type] ?? CLS.info" />
        <div class="min-w-0 flex-1">
          <p v-if="t.title" class="text-sm font-semibold">{{ t.title }}</p>
          <p class="text-sm text-muted-foreground break-words">{{ t.message }}</p>
        </div>
        <button class="opacity-60 hover:opacity-100 shrink-0" @click="dismiss(t.id)">
          <X class="h-3.5 w-3.5" />
        </button>
      </div>
    </div>
  </Teleport>
</template>
