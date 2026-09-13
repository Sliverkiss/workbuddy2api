<script setup>
// 账号页:OAuth 添加 + 批量操作 + 卡片网格 + 删除确认。
import { computed, ref, onMounted } from 'vue';
import { RouterLink } from 'vue-router';
import { Plus, RefreshCw, CircleAlert, FolderOpen } from '@lucide/vue';
import PageHeader from '../components/PageHeader.vue';
import AccountCard from '../components/AccountCard.vue';
import OauthDialog from '../components/OauthDialog.vue';
import UiDialog from '../components/ui/UiDialog.vue';
import { Button, Card, Skeleton, Badge } from '../components/ui/primitives';
import { useAppStore } from '../stores/app';
import { api } from '../api';
import { toast } from '../stores/toast';

const store = useAppStore();

const oauthOpen = ref(false);
const deleteTarget = ref(null);
const deleting = ref(false);
const refreshing = ref(false);

// 只读探测(静默档供进场自动触发与合并刷新复用)。
async function probeAll({ silent = false } = {}) {
  try {
    const r = await store.probeAll();
    if (!silent) toast[r.warn > 0 ? 'warn' : 'success'](`状态探测完成:${r.ok}/${r.total} 个账号全部成功${r.warn ? `,${r.warn} 个部分失败` : ''}`);
    return r;
  } catch (e) {
    if (!silent) toast.error(e.message, { title: '状态探测失败' });
    throw e;
  }
}

// 进场自动探测一次:有账号且(从未探测 或 最旧探测超过 10 分钟)时触发,避免每次进页都打上游。
onMounted(async () => {
  await store.loadAccounts();
  const valids = store.validAccounts;
  if (!valids.length) return;
  const staleMs = 10 * 60 * 1000;
  const oldest = Math.min(...valids.map((a) => a.liveStatus?.ts ?? 0));
  if (Date.now() - oldest > staleMs) probeAll({ silent: true }).catch(() => {});
});

// 合并刷新:状态探测(签到/旅行/积分明细) + 积分任务刷新,一路并发,汇总提示。
async function refreshAll() {
  refreshing.value = true;
  try {
    const [probeR, creditR] = await Promise.allSettled([store.probeAll(), store.runTask('credit')]);
    const probe = probeR.status === 'fulfilled' ? probeR.value : null;
    const credit = creditR.status === 'fulfilled' ? creditR.value : null;
    if (probe && credit) {
      toast[probe.warn > 0 ? 'warn' : 'success'](`刷新完成:状态 ${probe.ok}/${probe.total} · 积分 ${credit.summary.ok}/${credit.summary.total}`);
    } else {
      const errs = [probeR, creditR].filter((r) => r.status === 'rejected').map((r) => r.reason?.message).join(';');
      const done = probe ? `状态 ${probe.ok}/${probe.total}` : credit ? `积分 ${credit.summary.ok}/${credit.summary.total}` : '';
      toast.error(`${done ? done + ' · ' : ''}部分失败:${errs}`, { title: '刷新未全部成功' });
    }
  } finally {
    refreshing.value = false;
  }
}

async function confirmDelete() {
  if (!deleteTarget.value) return;
  deleting.value = true;
  try {
    await api.deleteAccount(deleteTarget.value.uid);
    toast.success(`已删除 ${deleteTarget.value.nickname || deleteTarget.value.uid} 的凭证文件`);
    deleteTarget.value = null;
    await store.loadAccounts();
  } catch (e) {
    toast.error(e.message, { title: '删除失败' });
  } finally {
    deleting.value = false;
  }
}

const brokenFiles = computed(() => store.accounts.filter((a) => a.loadError));
const validAccounts = computed(() =>
  [...store.validAccounts].sort((x, y) => {
    // 排序:有过期 token / 12153 警告的靠前,其余按积分从少到多(最需关注的在前)
    const score = (a) => (a.expiresInSec !== null && a.expiresInSec <= 0 ? 0 : a.sessionDeadFails > 0 ? 1 : 2);
    const sx = score(x), sy = score(y);
    if (sx !== sy) return sx - sy;
    return (x.lastCredit?.remain ?? Infinity) - (y.lastCredit?.remain ?? Infinity);
  }),
);
</script>

<template>
  <div class="p-6 lg:p-8">
    <PageHeader title="账号">
      <Button variant="outline" size="sm" :loading="refreshing" title="刷新全部状态与积分" @click="refreshAll">
        <RefreshCw v-if="!refreshing" />
      </Button>
      <Button variant="success" size="sm" @click="oauthOpen = true"><Plus />添加账号</Button>
    </PageHeader>

    <!-- 目录缺失 -->
    <div v-if="store.accountsLoaded && !store.dirExists" class="flex flex-col items-center gap-3 rounded-lg border border-dashed border-border py-16 wb-enter">
      <FolderOpen class="h-8 w-8 text-muted-foreground" />
      <p class="text-sm text-muted-foreground">凭证目录不存在</p>
      <p class="max-w-md break-all text-center font-mono text-xs text-muted-foreground">{{ store.authDir }}</p>
      <RouterLink to="/settings"><Button size="sm" variant="outline">去设置凭证目录</Button></RouterLink>
    </div>

    <template v-else>
      <!-- 加载骨架 -->
      <div v-if="!store.accountsLoaded" class="grid gap-4 md:grid-cols-2 2xl:grid-cols-3">
        <Skeleton v-for="i in 3" :key="i" class="h-56" />
      </div>

      <!-- 空态 -->
      <div v-else-if="!store.accounts.length" class="flex flex-col items-center gap-3 rounded-lg border border-dashed border-border py-16 wb-enter">
        <p class="text-sm text-muted-foreground">账号池为空</p>
        <p class="max-w-md text-center text-xs text-muted-foreground">
          点击「添加账号」走 OAuth 设备授权登录;或直接把 workbuddy2api 的 auths/*.json 放进凭证目录。
        </p>
        <Button variant="success" size="sm" @click="oauthOpen = true"><Plus />添加账号</Button>
      </div>

      <template v-else>
        <!-- 坏文件告警 -->
        <div v-if="brokenFiles.length" class="mb-4 flex items-start gap-3 rounded-lg border border-destructive/40 bg-destructive/10 p-4 wb-enter">
          <CircleAlert class="mt-0.5 h-4 w-4 shrink-0 text-destructive" />
          <div class="text-sm">
            <p class="font-semibold">{{ brokenFiles.length }} 个凭证文件解析失败(不会参与任务)</p>
            <p v-for="b in brokenFiles" :key="b.loadError" class="mt-0.5 font-mono text-xs text-muted-foreground">{{ b.loadError }}</p>
          </div>
        </div>

        <div class="grid gap-4 md:grid-cols-2 2xl:grid-cols-3">
          <AccountCard
            v-for="(a, i) in validAccounts"
            :key="a.uid"
            :account="a"
            :class="`wb-enter wb-enter-${Math.min(i, 3)}`"
            @refresh="store.loadAccounts()"
            @delete="deleteTarget = $event"
          />
        </div>
      </template>
    </template>

    <!-- OAuth 登录 -->
    <OauthDialog
      :open="oauthOpen"
      @close="oauthOpen = false"
      @done="store.loadAccounts()"
    />

    <!-- 删除确认 -->
    <UiDialog :open="!!deleteTarget" title="删除账号凭证" @close="deleteTarget = null">
      <p class="text-sm text-muted-foreground">
        将删除凭证文件 <span class="font-mono text-foreground">{{ deleteTarget?.file }}</span>
        ({{ deleteTarget?.nickname || deleteTarget?.uid }})。该操作只移除本目录中的凭证文件,不影响已登录的 CodeBuddy 客户端。
      </p>
      <template #footer>
        <Button variant="secondary" @click="deleteTarget = null">取消</Button>
        <Button variant="destructive" :loading="deleting" @click="confirmDelete">确认删除</Button>
      </template>
    </UiDialog>
  </div>
</template>
