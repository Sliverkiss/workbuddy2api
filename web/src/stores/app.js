// app.js — 面板共享状态:账号 / 网关 / 配置,带轻量轮询。
// 集成版:任务为同步调用(无 running 状态机),调度配置只读自网关 /api/config。
import { defineStore } from 'pinia';
import { api } from '../api';

export const useAppStore = defineStore('app', {
  state: () => ({
    authDir: '',
    dirExists: true,
    accounts: [],
    accountsLoaded: false,
    gatewayHealth: null,
    config: null,
    pollingTimer: null,
    lastError: '',
  }),
  getters: {
    validAccounts: (s) => s.accounts.filter((a) => !a.loadError),
    totalCredits: (s) => s.accounts.reduce((sum, a) => sum + (a.lastCredit?.remain ?? 0), 0),
    expiredTokens: (s) => s.accounts.filter((a) => !a.loadError && a.expired),
    sessionDeadWarned: (s) => s.accounts.filter((a) => (a.sessionDeadFails ?? 0) > 0),
  },
  actions: {
    async loadAccounts() {
      const r = await api.listAccounts();
      this.authDir = r.authDir;
      this.dirExists = r.dirExists;
      this.accounts = r.accounts;
      this.accountsLoaded = true;
    },
    async loadGatewayHealth() {
      this.gatewayHealth = await api.gatewayHealth();
    },
    async loadConfig() {
      this.config = await api.getConfig();
    },
    async init() {
      const results = await Promise.allSettled([
        this.loadAccounts(),
        this.loadGatewayHealth(),
        this.loadConfig(),
      ]);
      const failed = results.find((r) => r.status === 'rejected');
      this.lastError = failed?.reason?.message ?? '';
    },
    // 账号数据 8s 慢轮询 + 网关健康 32s
    startPolling() {
      this.stopPolling();
      let tick = 0;
      this.pollingTimer = setInterval(async () => {
        tick++;
        try {
          await this.loadAccounts();
          if (tick % 4 === 1) await this.loadGatewayHealth();
        } catch { /* 网关不可达时静默,下个周期再试 */ }
      }, 8000);
    },
    stopPolling() {
      if (this.pollingTimer) clearInterval(this.pollingTimer);
      this.pollingTimer = null;
    },
    async runTask(kind) {
      const r = await api.runTask(kind);
      await this.loadAccounts();
      return r;
    },
    async runTaskOne(uid, kind) {
      const r = await api.runTaskOne(uid, kind);
      await this.loadAccounts();
      return r;
    },
    async probeAccount(uid) {
      const r = await api.probeAccount(uid);
      await this.loadAccounts(); // liveStatus 挂在账号视图上,重拉一次
      return r;
    },
    async probeAll() {
      const r = await api.probeAll();
      await this.loadAccounts();
      return r;
    },
  },
});
