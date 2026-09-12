// api.js — 面板后端 fetch 封装:统一错误信封、可选 accessToken(与后端 accessToken 配置对应)。
let token = localStorage.getItem('wbweb.token') || '';

export function setToken(t) {
  token = t || '';
  localStorage.setItem('wbweb.token', token);
}

export function getToken() {
  return token;
}

export class ApiError extends Error {
  constructor(message, code, status) {
    super(message);
    this.code = code;
    this.status = status;
  }
}

async function request(path, { method = 'GET', body } = {}) {
  let resp;
  try {
    resp = await fetch(path, {
      method,
      headers: {
        ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
        ...(token ? { Authorization: 'Bearer ' + token } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    throw new ApiError(`面板服务不可达: ${e.message}(请先 npm start 或用 npm run dev)`, 'PANEL_UNREACHABLE', 0);
  }
  let json = null;
  try {
    json = await resp.json();
  } catch {
    throw new ApiError(`响应解析失败(http ${resp.status})`, 'BAD_JSON', resp.status);
  }
  if (!resp.ok) {
    const err = new ApiError(json?.error?.message ?? `http ${resp.status}`, json?.error?.code ?? 'error', resp.status);
    if (resp.status === 401) {
      // 网关 api_key 校验失败:通知 AuthGate 弹出密钥输入
      window.dispatchEvent(new CustomEvent('wb:unauthorized'));
    }
    throw err;
  }
  return json;
}

export const api = {
  getConfig: () => request('/api/config'),

  listAccounts: () => request('/api/accounts'),
  deleteAccount: (uid) => request(`/api/accounts/${encodeURIComponent(uid)}`, { method: 'DELETE' }),
  probeAccount: (uid) => request(`/api/accounts/${encodeURIComponent(uid)}/probe`, { method: 'POST' }),
  probeAll: () => request('/api/probe/all', { method: 'POST' }),
  statsOverview: () => request('/api/stats/overview'),

  runTask: (kind) => request(`/api/tasks/${kind}/run`, { method: 'POST' }),
  runTaskOne: (uid, kind) => request(`/api/accounts/${encodeURIComponent(uid)}/tasks/${kind}`, { method: 'POST' }),

  listLogs: (params = {}) => {
    const q = new URLSearchParams(Object.entries(params).filter(([, v]) => v));
    return request('/api/logs' + (q.size ? '?' + q : ''));
  },

  gatewayHealth: () => request('/api/gateway/health'),
  gatewayStatus: () => request('/api/gateway/status'),
  gatewayModels: () => request('/api/gateway/models'),
  gatewayRequestLogs: (limit = 300) => request(`/api/gateway/request-logs?limit=${limit}`),
  gatewayStdout: (limit = 200) => request(`/api/gateway/stdout?limit=${limit}`),
  gatewayChatTest: (model, prompt) => request('/api/gateway/chat-test', { method: 'POST', body: { model, prompt } }),

  oauthBegin: () => request('/api/oauth/begin', { method: 'POST' }),
  oauthPoll: (state) => request('/api/oauth/poll', { method: 'POST', body: { state } }),
  oauthCancel: (state) => request('/api/oauth/cancel', { method: 'POST', body: { state } }),
};
