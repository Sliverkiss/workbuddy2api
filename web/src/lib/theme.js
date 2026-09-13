// 主题三态:light / dark / system(默认);.dark class 驱动 CSS 变量切换。
const KEY = 'wbweb.theme';

export function getStoredTheme() {
  return localStorage.getItem(KEY) || 'system';
}

export function applyTheme(mode) {
  const dark = mode === 'dark' || (mode === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.classList.toggle('dark', dark);
  localStorage.setItem(KEY, mode);
}

// 跟随系统时监听系统切换
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if (getStoredTheme() === 'system') applyTheme('system');
});
