// format.js — 展示层格式化小工具。
export function fmtNum(n) {
  if (n === null || n === undefined) return '—';
  return Number(n).toLocaleString('zh-CN');
}

export function fmtTime(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function fmtTimeFull(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export function fmtDate(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

// 相对时间:刚刚 / N 分钟前 / N 小时前 / N 天前
export function fmtAgo(ts) {
  if (!ts) return '—';
  const diff = Date.now() - ts;
  if (diff < 60_000) return '刚刚';
  if (diff < 3600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff < 86400_000) return `${Math.floor(diff / 3600_000)} 小时前`;
  return `${Math.floor(diff / 86400_000)} 天前`;
}

// token 剩余时长:Xd Yh / Xh Ym / Xm / 已过期
export function fmtDurationSec(sec) {
  if (sec === null || sec === undefined) return '未知';
  if (sec <= 0) return '已过期';
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d} 天 ${h} 小时`;
  if (h > 0) return `${h} 小时 ${m} 分`;
  return `${m} 分钟`;
}

export function fmtCooldown(sec) {
  if (!sec || sec <= 0) return '—';
  const h = Math.floor(sec / 3600);
  const m = Math.ceil((sec % 3600) / 60);
  if (h > 0) return `${h}h${m}m`;
  return `${m}m`;
}

// 倒计时:Xh Ym / Xm Ys / Xs;<=0 返回 null
export function fmtCountdown(ms) {
  if (ms === null || ms === undefined || ms <= 0) return null;
  const sec = Math.floor(ms / 1000);
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

// 到期天数:<0 已过期;0 今天;N 天后
export function daysUntil(tsMs) {
  if (!tsMs) return null;
  return Math.ceil((tsMs - Date.now()) / 86400_000);
}

export const TASK_KINDS = {
  checkin: '签到',
  keepalive: '保活',
  activity: '活跃上报',
  travel: '猫猫旅行',
  credit: '积分查询',
  oauth: 'OAuth 登录',
  gateway: '网关',
  probe: '状态探测',
  system: '系统',
};

export const LOG_STATUS = {
  ok: { label: '成功', cls: 'text-success' },
  already: { label: '已签到', cls: 'text-muted-foreground' },
  skip: { label: '跳过', cls: 'text-muted-foreground' },
  warn: { label: '警告', cls: 'text-warning' },
  fail: { label: '失败', cls: 'text-destructive' },
};

export function kindLabel(kind) {
  return TASK_KINDS[kind] ?? kind;
}
