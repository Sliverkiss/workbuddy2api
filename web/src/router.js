import { createRouter, createWebHashHistory } from 'vue-router';

const DashboardPage = () => import('./pages/DashboardPage.vue');
const AccountsPage = () => import('./pages/AccountsPage.vue');
const AutomationPage = () => import('./pages/AutomationPage.vue');
const LogsPage = () => import('./pages/LogsPage.vue');
const SettingsPage = () => import('./pages/SettingsPage.vue');

export const router = createRouter({
  // hash 模式:静态托管任意路径可达
  history: createWebHashHistory(),
  routes: [
    { path: '/', name: 'dashboard', component: DashboardPage, meta: { title: '概览' } },
    { path: '/accounts', name: 'accounts', component: AccountsPage, meta: { title: '账号' } },
    { path: '/automation', name: 'automation', component: AutomationPage, meta: { title: '自动化' } },
    { path: '/logs', name: 'logs', component: LogsPage, meta: { title: '日志' } },
    { path: '/settings', name: 'settings', component: SettingsPage, meta: { title: '设置' } },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
});
