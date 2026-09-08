package server

import (
	"errors"
	"net/http"
	"os"

	"workbuddy2api/internal/credentials"
)

func (h *Handler) adminPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(adminHTML))
}

func (h *Handler) adminAccounts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.cfg.Pool.List()})
}

func (h *Handler) adminLoginStart(w http.ResponseWriter, r *http.Request) {
	start, err := h.cfg.Credentials.Start(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "oauth_start_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, start)
}

func (h *Handler) adminLoginPoll(w http.ResponseWriter, r *http.Request) {
	a, err := h.cfg.Credentials.Poll(r.Context(), r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, credentials.ErrPending):
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
		case errors.Is(err, credentials.ErrNotFound):
			writeOpenAIError(w, http.StatusNotFound, "login_not_found", err.Error())
		default:
			writeOpenAIError(w, http.StatusBadGateway, "oauth_poll_failed", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "complete", "uid": a.UID, "nickname": a.Nickname})
}

func (h *Handler) adminAccountDelete(w http.ResponseWriter, r *http.Request) {
	err := h.cfg.Credentials.Delete(r.PathValue("uid"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeOpenAIError(w, http.StatusNotFound, "account_not_found", "account not found")
			return
		}
		writeOpenAIError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

const adminHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>WorkBuddy2API 管理</title><style>
:root{color-scheme:dark;font-family:Inter,ui-sans-serif,system-ui;background:#0b1020;color:#e8ecf4}body{max-width:980px;margin:0 auto;padding:32px 20px}h1{margin:0 0 8px}.muted{color:#98a2b3}.bar,.card{background:#151b2d;border:1px solid #293149;border-radius:14px;padding:16px;margin:18px 0}.bar{display:flex;gap:10px;flex-wrap:wrap}input{flex:1;min-width:250px;background:#0b1020;color:#fff;border:1px solid #39425d;border-radius:9px;padding:10px}button{border:0;border-radius:9px;padding:10px 14px;background:#6d5dfc;color:#fff;cursor:pointer}button.danger{background:#b42318}button:disabled{opacity:.55}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:11px;border-bottom:1px solid #293149}.ok{color:#47cd89}.bad{color:#f97066}a{color:#9b8afb}#message{white-space:pre-wrap}
</style></head><body><h1>WorkBuddy2API</h1><div class="muted">凭证与账号池管理</div>
<div class="bar"><input id="key" type="password" placeholder="API Key（仅保存在当前浏览器）"><button onclick="saveKey()">保存并刷新</button><button onclick="startLogin()">添加账号</button></div>
<div id="login" class="card" hidden><div id="loginText"></div><p><a id="authLink" target="_blank" rel="noopener">打开 WorkBuddy 授权页面</a></p><button id="pollBtn" onclick="pollLogin()">我已完成登录，检查状态</button></div>
<div id="message" class="muted"></div><div class="card"><table><thead><tr><th>UID</th><th>昵称</th><th>状态</th><th>积分</th><th></th></tr></thead><tbody id="accounts"></tbody></table></div>
<script>
let loginId=''; const key=document.getElementById('key'); key.value=localStorage.getItem('wb2api_key')||'';
function headers(){return {'Authorization':'Bearer '+key.value,'Content-Type':'application/json'}}
function saveKey(){localStorage.setItem('wb2api_key',key.value);loadAccounts()}
async function api(path,opt={}){opt.headers={...headers(),...(opt.headers||{})};const r=await fetch(path,opt);const b=await r.json().catch(()=>({}));if(!r.ok&&r.status!==202)throw new Error(b.error?.message||('HTTP '+r.status));return [r,b]}
async function loadAccounts(){try{const[,b]=await api('/admin/api/accounts');accounts.innerHTML=b.accounts.map(a=>'<tr><td>'+esc(a.uid)+'</td><td>'+esc(a.nickname||'')+'</td><td class="'+(a.disabled||a.cooling?'bad':'ok')+'">'+(a.disabled?'禁用':a.cooling?'冷却':'可用')+'</td><td>'+(a.credits??'-')+'</td><td><button class="danger" onclick="del(\''+encodeURIComponent(a.uid)+'\')">删除</button></td></tr>').join('');message.textContent=''}catch(e){message.textContent=e.message}}
async function startLogin(){try{const[,b]=await api('/admin/api/login/start',{method:'POST'});loginId=b.id;authLink.href=b.auth_url;loginText.textContent='授权流程已创建，请在新窗口完成登录。';login.hidden=false;authLink.click()}catch(e){message.textContent=e.message}}
async function pollLogin(){try{pollBtn.disabled=true;const[r,b]=await api('/admin/api/login/'+loginId+'/poll',{method:'POST'});if(r.status===202){loginText.textContent='登录尚未完成，请完成授权后重试。'}else{loginText.textContent='账号 '+(b.nickname||b.uid)+' 已保存并热加载。';await loadAccounts()}}catch(e){loginText.textContent=e.message}finally{pollBtn.disabled=false}}
async function del(uid){if(!confirm('确定删除这个凭证？'))return;try{await api('/admin/api/accounts/'+uid,{method:'DELETE'});await loadAccounts()}catch(e){message.textContent=e.message}}
function esc(v){const d=document.createElement('div');d.textContent=String(v);return d.innerHTML}
loadAccounts();
</script></body></html>`
