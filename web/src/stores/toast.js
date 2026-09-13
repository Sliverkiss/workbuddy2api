// toast.js — 极简通知:成功/失败/警告,自动消失。
import { reactive } from 'vue';

let seq = 0;
export const toasts = reactive([]);

export function toast(message, { type = 'info', title = '', duration = 3200 } = {}) {
  const id = ++seq;
  toasts.push({ id, message, type, title });
  setTimeout(() => dismiss(id), duration);
  return id;
}

export function dismiss(id) {
  const i = toasts.findIndex((t) => t.id === id);
  if (i >= 0) toasts.splice(i, 1);
}

toast.success = (m, o = {}) => toast(m, { ...o, type: 'success' });
toast.error = (m, o = {}) => toast(m, { ...o, type: 'error', duration: o.duration ?? 5200 });
toast.warn = (m, o = {}) => toast(m, { ...o, type: 'warning' });
