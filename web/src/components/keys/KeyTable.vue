<script setup lang="ts">
import { keysApi } from "@/api/keys";
import type { APIKey, Group, GroupConfigOption, KeyStatus } from "@/types/models";
import { appState, triggerSyncOperationRefresh } from "@/utils/app-state";
import { copy } from "@/utils/clipboard";
import { getGroupDisplayName, maskKey } from "@/utils/display";
import {
  Add,
  AddCircleOutline,
  ChevronDown,
  Close,
  CopyOutline,
  EyeOffOutline,
  EyeOutline,
  HelpCircleOutline,
  Pencil,
  Remove,
  RemoveCircleOutline,
  Search,
} from "@vicons/ionicons5";
import {
  NButton,
  NCard,
  NDropdown,
  NEmpty,
  NIcon,
  NInput,
  NInputGroup,
  NInputNumber,
  NModal,
  NSelect,
  NSpace,
  NSpin,
  NSwitch,
  NTooltip,
  useDialog,
  type MessageReactive,
} from "naive-ui";
import { computed, h, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import KeyCreateDialog from "./KeyCreateDialog.vue";
import KeyDeleteDialog from "./KeyDeleteDialog.vue";

const { t } = useI18n();

interface KeyRow extends APIKey {
  is_visible: boolean;
}

interface KeyConfigItem {
  key: string;
  value: number | string | boolean;
}

interface Props {
  selectedGroup: Group | null;
}

const props = defineProps<Props>();

const keys = ref<KeyRow[]>([]);
const loading = ref(false);
const searchText = ref("");
const statusFilter = ref<"all" | "active" | "invalid">("all");
const currentPage = ref(1);
const pageSize = ref(12);
const total = ref(0);
const totalPages = ref(0);
const dialog = useDialog();
const confirmInput = ref("");

const statusOptions = [
  { label: t("common.all"), value: "all" },
  { label: t("keys.valid"), value: "active" },
  { label: t("keys.invalid"), value: "invalid" },
];

const moreOptions = [
  { label: t("keys.exportAllKeys"), key: "copyAll" },
  { label: t("keys.exportValidKeys"), key: "copyValid" },
  { label: t("keys.exportInvalidKeys"), key: "copyInvalid" },
  { type: "divider" },
  { label: t("keys.restoreAllInvalidKeys"), key: "restoreAll" },
  {
    label: t("keys.clearAllInvalidKeys"),
    key: "clearInvalid",
    props: { style: { color: "#d03050" } },
  },
  {
    label: t("keys.clearAllKeys"),
    key: "clearAll",
    props: { style: { color: "red", fontWeight: "bold" } },
  },
  { type: "divider" },
  { label: t("keys.validateAllKeys"), key: "validateAll" },
  { label: t("keys.validateValidKeys"), key: "validateActive" },
  { label: t("keys.validateInvalidKeys"), key: "validateInvalid" },
];

let testingMsg: MessageReactive | null = null;
const isDeling = ref(false);
const isRestoring = ref(false);

const createDialogShow = ref(false);
const deleteDialogShow = ref(false);

const editDialogShow = ref(false);
const editingKey = ref<KeyRow | null>(null);
const editingNotes = ref("");
const editingPriority = ref<number | null>(100);
const configPanelOpen = ref(false);
const probePanelOpen = ref(false);
const editingConfigItems = ref<KeyConfigItem[]>([]);
const editingProbeParamOverrides = ref("");
const configOptions = ref<GroupConfigOption[]>([]);
const configOptionsFetched = ref(false);

const supportedKeyOverrideFields = [
  "blacklist_threshold",
  "key_validation_interval_minutes",
  "key_validation_timeout_seconds",
  "active_probe_enabled",
  "active_probe_interval_seconds",
  "active_probe_timeout_seconds",
  "active_probe_window_minutes",
  "active_probe_failure_rate_limit",
];

const keyConfigOptions = computed(() =>
  configOptions.value.filter(option => supportedKeyOverrideFields.includes(option.key))
);

watch(
  () => props.selectedGroup,
  async newGroup => {
    if (newGroup) {
      await ensureConfigOptionsLoaded();
      const willWatcherTrigger = currentPage.value !== 1 || statusFilter.value !== "all";
      resetPage();
      if (!willWatcherTrigger) {
        await loadKeys();
      }
    }
  },
  { immediate: true }
);

watch([currentPage, pageSize], async () => {
  await loadKeys();
});

watch(statusFilter, async () => {
  if (currentPage.value !== 1) {
    currentPage.value = 1;
  } else {
    await loadKeys();
  }
});

watch(
  () => appState.groupDataRefreshTrigger,
  () => {
    if (appState.lastCompletedTask && props.selectedGroup) {
      const isCurrentGroup = appState.lastCompletedTask.groupName === props.selectedGroup.name;
      const shouldRefresh =
        appState.lastCompletedTask.taskType === "KEY_VALIDATION" ||
        appState.lastCompletedTask.taskType === "KEY_IMPORT" ||
        appState.lastCompletedTask.taskType === "KEY_DELETE";

      if (isCurrentGroup && shouldRefresh) {
        loadKeys();
      }
    }
  }
);

function handleSearchInput() {
  if (currentPage.value !== 1) {
    currentPage.value = 1;
  } else {
    loadKeys();
  }
}

function handleMoreAction(key: string) {
  switch (key) {
    case "copyAll":
      copyAllKeys();
      break;
    case "copyValid":
      copyValidKeys();
      break;
    case "copyInvalid":
      copyInvalidKeys();
      break;
    case "restoreAll":
      restoreAllInvalid();
      break;
    case "validateAll":
      validateKeys("all");
      break;
    case "validateActive":
      validateKeys("active");
      break;
    case "validateInvalid":
      validateKeys("invalid");
      break;
    case "clearInvalid":
      clearAllInvalid();
      break;
    case "clearAll":
      clearAll();
      break;
  }
}

async function ensureConfigOptionsLoaded() {
  if (configOptionsFetched.value) {
    return;
  }

  try {
    const options = await keysApi.getGroupConfigOptions();
    configOptions.value = options || [];
    configOptionsFetched.value = true;
  } catch (error) {
    console.error("Load key config options failed", error);
    window.$message.error(t("keys.loadKeyConfigOptionsFailed"));
    throw error;
  }
}

async function loadKeys() {
  if (!props.selectedGroup?.id) {
    return;
  }

  try {
    loading.value = true;
    const result = await keysApi.getGroupKeys({
      group_id: props.selectedGroup.id,
      page: currentPage.value,
      page_size: pageSize.value,
      status: statusFilter.value === "all" ? undefined : (statusFilter.value as KeyStatus),
      key_value: searchText.value.trim() || undefined,
    });
    keys.value = result.items.map(item => ({
      ...(item as APIKey),
      is_visible: false,
    }));
    total.value = result.pagination.total_items;
    totalPages.value = result.pagination.total_pages;
  } finally {
    loading.value = false;
  }
}

async function handleBatchDeleteSuccess() {
  await loadKeys();
  if (props.selectedGroup) {
    triggerSyncOperationRefresh(props.selectedGroup.name, "BATCH_DELETE");
  }
}

async function copyKey(key: KeyRow) {
  const success = await copy(key.key_value);
  if (success) {
    window.$message.success(t("keys.keyCopied"));
  } else {
    window.$message.error(t("keys.copyFailed"));
  }
}

async function testKey(_key: KeyRow) {
  if (!props.selectedGroup?.id || !_key.key_value || testingMsg) {
    return;
  }

  testingMsg = window.$message.info(t("keys.testingKey"), {
    duration: 0,
  });

  try {
    const response = await keysApi.testKeys(props.selectedGroup.id, _key.key_value);
    const curValid = response.results?.[0] || {};
    if (curValid.is_valid) {
      window.$message.success(
        t("keys.testSuccess", { duration: formatDuration(response.total_duration) })
      );
    } else {
      window.$message.error(curValid.error || t("keys.testFailed"), {
        keepAliveOnHover: true,
        duration: 5000,
        closable: true,
      });
    }
    await loadKeys();
    triggerSyncOperationRefresh(props.selectedGroup.name, "TEST_SINGLE");
  } catch (_error) {
    console.error("Test failed");
  } finally {
    testingMsg?.destroy();
    testingMsg = null;
  }
}

function formatDuration(ms: number): string {
  if (ms < 0) {
    return "0ms";
  }

  const minutes = Math.floor(ms / 60000);
  const seconds = Math.floor((ms % 60000) / 1000);
  const milliseconds = ms % 1000;

  let result = "";
  if (minutes > 0) {
    result += `${minutes}m`;
  }
  if (seconds > 0) {
    result += `${seconds}s`;
  }
  if (milliseconds > 0 || result === "") {
    result += `${milliseconds}ms`;
  }

  return result;
}

function toggleKeyVisibility(key: KeyRow) {
  key.is_visible = !key.is_visible;
}

function getKeyDisplayValue(key: KeyRow): string {
  return key.is_visible ? key.key_value : maskKey(key.key_value);
}

function getNoteText(key: KeyRow): string {
  return key.notes?.trim() || "";
}

function hasKeyOverrides(key: KeyRow): boolean {
  return (
    Object.keys(key.config || {}).length > 0 ||
    Object.keys(key.probe_param_overrides || {}).length > 0
  );
}

function getKeyConfigOption(key: string) {
  return keyConfigOptions.value.find(option => option.key === key);
}

function buildConfigItems(config: Record<string, unknown> | undefined): KeyConfigItem[] {
  const input = config || {};
  return Object.entries(input)
    .filter(([key]) => supportedKeyOverrideFields.includes(key))
    .map(([key, value]) => ({
      key,
      value: value as number | string | boolean,
    }))
    .sort((a, b) => {
      const aIndex = keyConfigOptions.value.findIndex(option => option.key === a.key);
      const bIndex = keyConfigOptions.value.findIndex(option => option.key === b.key);
      return aIndex - bIndex;
    });
}

function buildConfigPayload(items: KeyConfigItem[]): Record<string, unknown> {
  return items.reduce<Record<string, unknown>>((accumulator, item) => {
    if (!item.key) {
      return accumulator;
    }
    accumulator[item.key] = item.value;
    return accumulator;
  }, {});
}

function stableObjectText(input: Record<string, unknown> | undefined): string {
  const source = input || {};
  const normalized = Object.keys(source)
    .sort()
    .reduce<Record<string, unknown>>((accumulator, key) => {
      accumulator[key] = source[key];
      return accumulator;
    }, {});

  return JSON.stringify(normalized);
}

function addKeyConfigItem() {
  editingConfigItems.value.push({
    key: "",
    value: "",
  });
}

function removeKeyConfigItem(index: number) {
  editingConfigItems.value.splice(index, 1);
}

function handleKeyConfigKeyChange(index: number, key: string) {
  const option = getKeyConfigOption(key);
  if (!option) {
    return;
  }
  editingConfigItems.value[index].value = option.default_value;
}

function getEditingOverrideSummary(): string {
  const configCount = editingConfigItems.value.filter(item => item.key).length;
  let probeCount = 0;

  const probeText = editingProbeParamOverrides.value.trim();
  if (probeText && probeText !== "{}") {
    try {
      probeCount = Object.keys(JSON.parse(probeText) as Record<string, unknown>).length;
    } catch {
      probeCount = editingKey.value ? Object.keys(editingKey.value.probe_param_overrides || {}).length : 0;
    }
  }

  if (configCount === 0 && probeCount === 0) {
    return t("keys.keyInheritSummary");
  }

  return t("keys.keyOverrideSummary", {
    configCount,
    probeCount,
  });
}

async function openKeyEditor(key: KeyRow) {
  try {
    await ensureConfigOptionsLoaded();
  } catch {
    return;
  }
  editingKey.value = key;
  editingNotes.value = key.notes || "";
  editingPriority.value = key.priority || 100;
  editingConfigItems.value = buildConfigItems(key.config);
  editingProbeParamOverrides.value = JSON.stringify(key.probe_param_overrides || {}, null, 2);
  configPanelOpen.value = false;
  probePanelOpen.value = false;
  editDialogShow.value = true;
}

async function saveKeyMeta() {
  if (!editingKey.value || !editingPriority.value || editingPriority.value <= 0) {
    return;
  }

  try {
    const nextNotes = editingNotes.value.trim();
    const nextPriority = editingPriority.value;
    const nextProbeOverridesText = editingProbeParamOverrides.value.trim();
    const currentNotes = editingKey.value.notes || "";
    const currentPriority = editingKey.value.priority || 100;
    const nextConfig = buildConfigPayload(editingConfigItems.value);
    const currentConfig = editingKey.value.config || {};
    const currentProbeOverridesText = JSON.stringify(
      editingKey.value.probe_param_overrides || {},
      null,
      2
    );

    const notesChanged = nextNotes !== currentNotes;
    const priorityChanged = nextPriority !== currentPriority;
    const configChanged = stableObjectText(nextConfig) !== stableObjectText(currentConfig);
    const probeOverridesChanged = nextProbeOverridesText !== currentProbeOverridesText;

    if (!notesChanged && !priorityChanged && !configChanged && !probeOverridesChanged) {
      editDialogShow.value = false;
      return;
    }

    let nextProbeParamOverrides: Record<string, unknown> = editingKey.value.probe_param_overrides || {};

    if (probeOverridesChanged) {
      if (!nextProbeOverridesText) {
        nextProbeParamOverrides = {};
      } else {
        try {
          nextProbeParamOverrides = JSON.parse(nextProbeOverridesText) as Record<string, unknown>;
        } catch {
          window.$message.error(t("keys.invalidKeyProbeParamOverridesJson"));
          return;
        }
      }
    }

    const updated = await keysApi.updateKey(editingKey.value.id, {
      notes: nextNotes,
      priority: nextPriority,
      config: nextConfig,
      probe_param_overrides: nextProbeParamOverrides,
    });
    editingKey.value.notes = updated.notes || "";
    editingKey.value.priority = updated.priority;
    editingKey.value.config = updated.config || {};
    editingKey.value.probe_param_overrides = updated.probe_param_overrides || {};

    editDialogShow.value = false;
    window.$message.success(t("keys.keyMetaUpdated"));

    if (priorityChanged || configChanged || probeOverridesChanged) {
      await loadKeys();
    }
  } catch (error) {
    console.error("Update key meta failed", error);
  }
}

async function restoreKey(key: KeyRow) {
  if (!props.selectedGroup?.id || !key.key_value || isRestoring.value) {
    return;
  }

  const d = dialog.warning({
    title: t("keys.restoreKey"),
    content: t("keys.confirmRestoreKey", { key: maskKey(key.key_value) }),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      isRestoring.value = true;
      d.loading = true;

      try {
        await keysApi.restoreKeys(props.selectedGroup.id, key.key_value);
        await loadKeys();
        triggerSyncOperationRefresh(props.selectedGroup.name, "RESTORE_SINGLE");
      } catch (_error) {
        console.error("Restore failed");
      } finally {
        d.loading = false;
        isRestoring.value = false;
      }
    },
  });
}

async function deleteKey(key: KeyRow) {
  if (!props.selectedGroup?.id || !key.key_value || isDeling.value) {
    return;
  }

  const d = dialog.warning({
    title: t("keys.deleteKey"),
    content: t("keys.confirmDeleteKey", { key: maskKey(key.key_value) }),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      d.loading = true;
      isDeling.value = true;

      try {
        await keysApi.deleteKeys(props.selectedGroup.id, key.key_value);
        await loadKeys();
        triggerSyncOperationRefresh(props.selectedGroup.name, "DELETE_SINGLE");
      } catch (_error) {
        console.error("Delete failed");
      } finally {
        d.loading = false;
        isDeling.value = false;
      }
    },
  });
}

function formatRelativeTime(date: string) {
  if (!date) {
    return t("keys.never");
  }
  const now = new Date();
  const target = new Date(date);
  const diffSeconds = Math.floor((now.getTime() - target.getTime()) / 1000);
  const diffMinutes = Math.floor(diffSeconds / 60);
  const diffHours = Math.floor(diffMinutes / 60);
  const diffDays = Math.floor(diffHours / 24);

  if (diffDays > 0) {
    return t("keys.daysAgo", { days: diffDays });
  }
  if (diffHours > 0) {
    return t("keys.hoursAgo", { hours: diffHours });
  }
  if (diffMinutes > 0) {
    return t("keys.minutesAgo", { minutes: diffMinutes });
  }
  if (diffSeconds > 0) {
    return t("keys.secondsAgo", { seconds: diffSeconds });
  }
  return t("keys.justNow");
}

function getStatusClass(status: KeyStatus): string {
  switch (status) {
    case "active":
      return "status-valid";
    case "invalid":
      return "status-invalid";
    default:
      return "status-unknown";
  }
}

function formatProbeRate(rate: number): string {
  return `${rate.toFixed(1)}%`;
}

function getProbeRateSummary(key: KeyRow): string {
  if (key.probe_sample_count <= 0) {
    return "--";
  }
  return `${formatProbeRate(key.probe_failure_rate)} / ${key.probe_sample_count}`;
}

function getLastUsedText(key: KeyRow): string {
  return key.last_used_at ? formatRelativeTime(key.last_used_at) : t("keys.unused");
}

function getLastProbeText(key: KeyRow): string {
  return key.last_probe_at ? formatRelativeTime(key.last_probe_at) : t("keys.probeNeverShort");
}

async function copyAllKeys() {
  if (!props.selectedGroup?.id) {
    return;
  }

  keysApi.exportKeys(props.selectedGroup.id, "all");
}

async function copyValidKeys() {
  if (!props.selectedGroup?.id) {
    return;
  }

  keysApi.exportKeys(props.selectedGroup.id, "active");
}

async function copyInvalidKeys() {
  if (!props.selectedGroup?.id) {
    return;
  }

  keysApi.exportKeys(props.selectedGroup.id, "invalid");
}

async function restoreAllInvalid() {
  if (!props.selectedGroup?.id || isRestoring.value) {
    return;
  }

  const d = dialog.warning({
    title: t("keys.restoreKeys"),
    content: t("keys.confirmRestoreAllInvalid"),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      isRestoring.value = true;
      d.loading = true;
      try {
        await keysApi.restoreAllInvalidKeys(props.selectedGroup.id);
        await loadKeys();
        triggerSyncOperationRefresh(props.selectedGroup.name, "RESTORE_ALL_INVALID");
      } catch (_error) {
        console.error("Restore failed");
      } finally {
        d.loading = false;
        isRestoring.value = false;
      }
    },
  });
}

async function validateKeys(status: "all" | "active" | "invalid") {
  if (!props.selectedGroup?.id || testingMsg) {
    return;
  }

  let statusText = t("common.all");
  if (status === "active") {
    statusText = t("keys.valid");
  } else if (status === "invalid") {
    statusText = t("keys.invalid");
  }

  testingMsg = window.$message.info(t("keys.validatingKeysMsg", { type: statusText }), {
    duration: 0,
  });

  try {
    await keysApi.validateGroupKeys(props.selectedGroup.id, status === "all" ? undefined : status);
    localStorage.removeItem("last_closed_task");
    appState.taskPollingTrigger++;
  } catch (_error) {
    console.error("Test failed");
  } finally {
    testingMsg?.destroy();
    testingMsg = null;
  }
}

async function clearAllInvalid() {
  if (!props.selectedGroup?.id || isDeling.value) {
    return;
  }

  const d = dialog.warning({
    title: t("keys.clearKeys"),
    content: t("keys.confirmClearInvalidKeys"),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      isDeling.value = true;
      d.loading = true;
      try {
        const { data } = await keysApi.clearAllInvalidKeys(props.selectedGroup.id);
        window.$message.success(data?.message || t("keys.clearSuccess"));
        await loadKeys();
        triggerSyncOperationRefresh(props.selectedGroup.name, "CLEAR_ALL_INVALID");
      } catch (_error) {
        console.error("Delete failed");
      } finally {
        d.loading = false;
        isDeling.value = false;
      }
    },
  });
}

async function clearAll() {
  if (!props.selectedGroup?.id || isDeling.value) {
    return;
  }

  dialog.warning({
    title: t("keys.clearAllKeys"),
    content: t("keys.confirmClearAllKeys"),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: () => {
      confirmInput.value = "";
      dialog.create({
        title: t("keys.enterGroupNameToConfirm"),
        content: () =>
          h("div", null, [
            h("p", null, [
              t("keys.dangerousOperationWarning1"),
              h("strong", null, t("common.all")),
              t("keys.dangerousOperationWarning2"),
              h("strong", { style: { color: "#d03050" } }, props.selectedGroup?.name),
              t("keys.toConfirm"),
            ]),
            h(NInput, {
              value: confirmInput.value,
              "onUpdate:value": v => {
                confirmInput.value = v;
              },
              placeholder: t("keys.enterGroupName"),
            }),
          ]),
        positiveText: t("keys.confirmClear"),
        negativeText: t("common.cancel"),
        onPositiveClick: async () => {
          if (confirmInput.value !== props.selectedGroup?.name) {
            window.$message.error(t("keys.incorrectGroupName"));
            return false;
          }

          if (!props.selectedGroup?.id) {
            return;
          }

          isDeling.value = true;
          try {
            await keysApi.clearAllKeys(props.selectedGroup.id);
            window.$message.success(t("keys.clearAllKeysSuccess"));
            await loadKeys();
            triggerSyncOperationRefresh(props.selectedGroup.name, "CLEAR_ALL");
          } catch (_error) {
            console.error("Clear all failed", _error);
          } finally {
            isDeling.value = false;
          }
        },
      });
    },
  });
}

function changePage(page: number) {
  currentPage.value = page;
}

function changePageSize(size: number) {
  pageSize.value = size;
  currentPage.value = 1;
}

function resetPage() {
  currentPage.value = 1;
  searchText.value = "";
  statusFilter.value = "all";
}
</script>

<template>
  <div class="key-table-container">
    <div class="toolbar">
      <div class="toolbar-left">
        <n-button type="success" size="small" @click="createDialogShow = true">
          <template #icon>
            <n-icon :component="AddCircleOutline" />
          </template>
          {{ t("keys.addKey") }}
        </n-button>
        <n-button type="error" size="small" @click="deleteDialogShow = true">
          <template #icon>
            <n-icon :component="RemoveCircleOutline" />
          </template>
          {{ t("keys.deleteKey") }}
        </n-button>
      </div>

      <div class="toolbar-right">
        <n-space :size="12" align="center">
          <n-select
            v-model:value="statusFilter"
            :options="statusOptions"
            size="small"
            style="width: 120px"
            :placeholder="t('keys.allStatus')"
          />

          <n-input-group>
            <n-input
              v-model:value="searchText"
              :placeholder="t('keys.keyExactMatch')"
              size="small"
              style="width: 220px"
              clearable
              @keyup.enter="handleSearchInput"
            >
              <template #prefix>
                <n-icon :component="Search" />
              </template>
            </n-input>
            <n-button
              type="primary"
              ghost
              size="small"
              :disabled="loading"
              @click="handleSearchInput"
            >
              {{ t("common.search") }}
            </n-button>
          </n-input-group>

          <n-dropdown :options="moreOptions" trigger="click" @select="handleMoreAction">
            <n-button size="small" tertiary>
              <template #icon>
                <span class="toolbar-more-icon">⋯</span>
              </template>
            </n-button>
          </n-dropdown>
        </n-space>
      </div>
    </div>

    <div class="keys-grid-container">
      <n-spin :show="loading">
        <div v-if="keys.length === 0 && !loading" class="empty-container">
          <n-empty :description="t('keys.noMatchingKeys')" />
        </div>

        <div v-else class="keys-grid">
          <article
            v-for="key in keys"
            :key="key.id"
            class="key-card"
            :class="getStatusClass(key.status)"
          >
            <!-- Row 1: Status + Key + Note + Actions -->
            <div class="row-main">
              <span class="priority-num">{{ key.priority }}</span>
              <span class="status-dot" :class="getStatusClass(key.status)" />
              <code class="key-mono" :title="key.key_value">{{ getKeyDisplayValue(key) }}</code>
              <span v-if="hasKeyOverrides(key)" class="override-tag">{{ t("keys.overrideShort") }}</span>
              <span v-if="getNoteText(key)" class="note-tag" :title="getNoteText(key)">{{ getNoteText(key) }}</span>
              <span class="row-spacer" />
              <span class="icon-actions">
                <n-button size="tiny" text @click="openKeyEditor(key)" :title="t('keys.editKeyMeta')">
                  <template #icon><n-icon :component="Pencil" /></template>
                </n-button>
                <n-button size="tiny" text @click="toggleKeyVisibility(key)" :title="t('keys.showHide')">
                  <template #icon><n-icon :component="key.is_visible ? EyeOffOutline : EyeOutline" /></template>
                </n-button>
                <n-button size="tiny" text @click="copyKey(key)" :title="t('common.copy')">
                  <template #icon><n-icon :component="CopyOutline" /></template>
                </n-button>
              </span>
            </div>

            <!-- Row 2: Metrics -->
            <div class="row-metrics">
              <span class="metric">{{ t('keys.requestsShort') }} <strong>{{ key.request_count }}</strong></span>
              <span class="metric-sep" />
              <span class="metric">{{ t('keys.failuresShort') }} <strong>{{ key.failure_count }}</strong></span>
              <span class="metric-sep" />
              <span class="metric">{{ t('keys.rateShort') }} <strong>{{ getProbeRateSummary(key) }}</strong></span>
              <span class="metric-sep" />
              <span class="metric">{{ t('keys.probeLastShort') }} <strong :title="key.last_probe_error || ''">{{ getLastProbeText(key) }}</strong></span>
              <span class="metric-sep" />
              <span class="metric">{{ t('keys.usedShort') }} <strong>{{ getLastUsedText(key) }}</strong></span>
            </div>

            <!-- Row 3: Action buttons -->
            <div class="row-actions">
              <n-button tertiary type="info" size="tiny" @click="testKey(key)">{{ t('keys.testShort') }}</n-button>
              <n-button v-if="key.status !== 'active'" tertiary size="tiny" type="warning" @click="restoreKey(key)">{{ t('keys.restoreShort') }}</n-button>
              <n-button tertiary size="tiny" type="error" @click="deleteKey(key)">{{ t('common.deleteShort') }}</n-button>
            </div>
          </article>
        </div>
      </n-spin>
    </div>

    <div class="pagination-container">
      <div class="pagination-info">
        <span>{{ t("keys.totalRecords", { total }) }}</span>
        <n-select
          v-model:value="pageSize"
          :options="[
            { label: t('keys.recordsPerPage', { count: 12 }), value: 12 },
            { label: t('keys.recordsPerPage', { count: 24 }), value: 24 },
            { label: t('keys.recordsPerPage', { count: 60 }), value: 60 },
            { label: t('keys.recordsPerPage', { count: 120 }), value: 120 },
          ]"
          size="small"
          style="width: 100px; margin-left: 12px"
          @update:value="changePageSize"
        />
      </div>
      <div class="pagination-controls">
        <n-button size="small" :disabled="currentPage <= 1" @click="changePage(currentPage - 1)">
          {{ t("common.previousPage") }}
        </n-button>
        <span class="page-info">
          {{ t("keys.pageInfo", { current: currentPage, total: totalPages }) }}
        </span>
        <n-button
          size="small"
          :disabled="currentPage >= totalPages"
          @click="changePage(currentPage + 1)"
        >
          {{ t("common.nextPage") }}
        </n-button>
      </div>
    </div>

    <key-create-dialog
      v-if="selectedGroup?.id"
      v-model:show="createDialogShow"
      :group-id="selectedGroup.id"
      :group-name="getGroupDisplayName(selectedGroup!)"
      @success="loadKeys"
    />

    <key-delete-dialog
      v-if="selectedGroup?.id"
      v-model:show="deleteDialogShow"
      :group-id="selectedGroup.id"
      :group-name="getGroupDisplayName(selectedGroup!)"
      @success="handleBatchDeleteSuccess"
    />
  </div>

  <n-modal :show="editDialogShow" @update:show="editDialogShow = false" class="key-editor-modal">
    <n-card class="key-editor-card" :bordered="false" size="huge" role="dialog" aria-modal="true">
      <template #header>
        <div class="editor-header">
          <div>
            <div class="editor-eyebrow">{{ t("keys.singleKeySettings") }}</div>
            <div class="editor-title">{{ t("keys.editKeyMeta") }}</div>
          </div>
          <n-button quaternary circle @click="editDialogShow = false">
            <template #icon>
              <n-icon :component="Close" />
            </template>
          </n-button>
        </div>
      </template>

      <div v-if="editingKey" class="editor-content">
        <section class="editor-hero">
          <div class="editor-hero-main">
            <div class="editor-hero-label">{{ t("keys.keyFingerprint") }}</div>
            <div class="editor-hero-value">{{ maskKey(editingKey.key_value) }}</div>
            <div class="editor-hero-meta">
              <span class="editor-hero-chip" :class="getStatusClass(editingKey.status)">
                {{ editingKey.status === "active" ? t("keys.valid") : t("keys.invalid") }}
              </span>
              <span class="editor-hero-chip neutral">
                {{ t("keys.priority") }} {{ editingPriority || editingKey.priority }}
              </span>
              <span class="editor-hero-chip neutral">{{ getEditingOverrideSummary() }}</span>
            </div>
          </div>
          <div class="editor-hero-side">
            <div class="editor-side-label">{{ t("keys.group") }}</div>
            <div class="editor-side-value">
              {{ selectedGroup ? getGroupDisplayName(selectedGroup) : "--" }}
            </div>
          </div>
        </section>

        <section class="editor-panel">
          <div class="editor-panel-head">
            <div>
              <div class="editor-panel-title">{{ t("keys.basicSettings") }}</div>
            </div>
          </div>

          <div class="editor-basic-row">
            <div class="edit-field-inline">
              <div class="edit-label">{{ t("keys.priority") }}</div>
              <n-input-number v-model:value="editingPriority" :min="1" size="small" style="width: 100px" />
            </div>
            <div class="edit-field-inline" style="flex: 1; min-width: 0">
              <div class="edit-label">{{ t("keys.notes") }}</div>
              <n-input
                v-model:value="editingNotes"
                :placeholder="t('keys.enterNotes')"
                maxlength="255"
                size="small"
                style="flex: 1"
              />
            </div>
          </div>
        </section>

        <section class="editor-panel collapsible">
          <div class="editor-panel-head clickable" @click="configPanelOpen = !configPanelOpen">
            <div>
              <div class="editor-panel-title">{{ t("keys.keyConfigOverrides") }}</div>
              <div class="editor-panel-desc">
                {{ t("keys.keyConfigOverridesHint") }}
              </div>
            </div>
            <n-icon :component="ChevronDown" class="collapse-icon" :class="{ open: configPanelOpen }" />
          </div>

          <div v-if="configPanelOpen" class="collapsible-body">
            <div v-if="editingConfigItems.length > 0" class="config-items">
              <div
                v-for="(configItem, index) in editingConfigItems"
                :key="`${configItem.key}-${index}`"
                class="key-config-row"
              >
                <div class="config-select">
                  <n-select
                    v-model:value="configItem.key"
                    :options="
                      keyConfigOptions.map(option => ({
                        label: option.name,
                        value: option.key,
                        disabled:
                          editingConfigItems
                            .map(item => item.key)
                            .includes(option.key) && option.key !== configItem.key,
                      }))
                    "
                    :placeholder="t('keys.selectConfigParam')"
                    @update:value="value => handleKeyConfigKeyChange(index, value)"
                    clearable
                  />
                </div>
                <div class="config-value">
                  <n-tooltip trigger="hover" placement="top">
                    <template #trigger>
                      <n-input-number
                        v-if="typeof configItem.value === 'number'"
                        v-model:value="configItem.value"
                        :placeholder="t('keys.paramValue')"
                        :precision="0"
                        style="width: 100%"
                      />
                      <div v-else-if="typeof configItem.value === 'boolean'" class="config-switch">
                        <n-switch v-model:value="configItem.value" size="small" />
                        <span>{{ configItem.value ? t("keys.switchOn") : t("keys.switchOff") }}</span>
                      </div>
                      <n-input
                        v-else
                        v-model:value="configItem.value"
                        :placeholder="t('keys.paramValue')"
                      />
                    </template>
                    {{ getKeyConfigOption(configItem.key)?.description || t("keys.setConfigValue") }}
                  </n-tooltip>
                </div>
                <div class="config-actions">
                  <n-button
                    @click="removeKeyConfigItem(index)"
                    type="error"
                    quaternary
                    circle
                    size="small"
                  >
                    <template #icon>
                      <n-icon :component="Remove" />
                    </template>
                  </n-button>
                </div>
              </div>
            </div>
            <div v-else class="editor-empty">
              <div class="editor-empty-title">{{ t("keys.keyInheritSummary") }}</div>
              <div class="editor-empty-desc">{{ t("keys.keyConfigEmptyHint") }}</div>
            </div>

            <n-button
              @click="addKeyConfigItem"
              dashed
              class="editor-add-button"
              :disabled="editingConfigItems.length >= keyConfigOptions.length"
            >
              <template #icon>
                <n-icon :component="Add" />
              </template>
              {{ t("keys.addConfigParam") }}
            </n-button>
          </div>
        </section>

        <section class="editor-panel collapsible">
          <div class="editor-panel-head clickable" @click="probePanelOpen = !probePanelOpen">
            <div>
              <div class="editor-panel-title">{{ t("keys.keyProbeParamOverrides") }}</div>
              <div class="editor-panel-desc">{{ t("keys.keyProbeParamOverridesHint") }}</div>
            </div>
            <div class="panel-head-right">
              <n-tooltip trigger="hover" placement="top">
                <template #trigger>
                  <n-icon :component="HelpCircleOutline" class="editor-help-icon" @click.stop />
                </template>
                {{ t("keys.keyProbeParamOverridesDescription") }}
              </n-tooltip>
              <n-icon :component="ChevronDown" class="collapse-icon" :class="{ open: probePanelOpen }" />
            </div>
          </div>

          <div v-if="probePanelOpen" class="collapsible-body">
            <n-input
              v-model:value="editingProbeParamOverrides"
              type="textarea"
              :placeholder="t('keys.keyProbeParamOverridesPlaceholder')"
              :rows="5"
            />
          </div>
        </section>
      </div>

      <template #footer>
        <div class="editor-footer">
          <n-button @click="editDialogShow = false">{{ t("common.cancel") }}</n-button>
          <n-button type="primary" @click="saveKeyMeta">{{ t("common.save") }}</n-button>
        </div>
      </template>
    </n-card>
  </n-modal>
</template>

<style scoped>
.key-table-container {
  background: var(--card-bg-solid);
  border-radius: 10px;
  box-shadow: var(--shadow-md);
  border: 1px solid var(--border-color);
  overflow: hidden;
  height: 100%;
  display: flex;
  flex-direction: column;
}

.toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 16px;
  background: var(--card-bg-solid);
  border-bottom: 1px solid var(--border-color);
  flex-shrink: 0;
  gap: 16px;
}

.toolbar-left {
  display: flex;
  gap: 8px;
  flex-shrink: 0;
}

.toolbar-right {
  display: flex;
  gap: 12px;
  align-items: center;
  flex: 1;
  justify-content: flex-end;
  min-width: 0;
}

.toolbar-more-icon {
  font-size: 16px;
  font-weight: bold;
}

.keys-grid-container {
  flex: 1;
  overflow-y: auto;
  padding: 14px;
  min-height: 0;
  scrollbar-width: none;
  -ms-overflow-style: none;
  background:
    linear-gradient(180deg, rgba(15, 23, 42, 0.02), transparent 28%),
    var(--bg-secondary, transparent);
}

.keys-grid-container::-webkit-scrollbar {
  width: 0;
  height: 0;
}

.keys-grid-container :deep(.n-spin-container),
.keys-grid-container :deep(.n-spin-content) {
  min-height: 0;
  height: 100%;
}

.keys-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(380px, 1fr));
  gap: 10px;
}

.key-card {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border-radius: 8px;
  border: 1px solid rgba(15, 23, 42, 0.07);
  background: var(--card-bg-solid);
  transition: border-color 0.15s ease;
}

.key-card:hover {
  border-color: rgba(15, 23, 42, 0.15);
}

.key-card.status-valid {
  border-color: rgba(24, 160, 88, 0.18);
}

.key-card.status-invalid {
  border-color: rgba(208, 48, 80, 0.35);
  background: rgba(208, 48, 80, 0.055);
  opacity: 0.78;
}

/* Row 1 */
.row-main {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.priority-num {
  flex-shrink: 0;
  font-size: 10px;
  font-weight: 700;
  color: #1f2937;
  background: rgba(107, 114, 128, 0.15);
  min-width: 20px;
  height: 18px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 4px;
  padding: 0 4px;
}

.status-dot {
  flex-shrink: 0;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: #999;
}

.status-dot.status-valid {
  background: #18a058;
}

.status-dot.status-invalid {
  background: #d03050;
}

.key-mono {
  font-family: 'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, Courier, monospace;
  font-size: 12px;
  color: var(--text-primary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  min-width: 0;
  flex-shrink: 1;
}

.override-tag,
.note-tag {
  flex-shrink: 0;
  font-size: 11px;
  line-height: 18px;
  height: 18px;
  border-radius: 999px;
  padding: 0 7px;
  max-width: 140px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.override-tag {
  color: #8a5a00;
  background: rgba(240, 160, 32, 0.12);
}

.note-tag {
  color: #1f2937;
  background: rgba(107, 114, 128, 0.15);
}

.key-editor-modal {
  width: 640px;
}

.key-editor-card {
  --n-border-radius: 14px;
  --n-color: var(--card-bg-solid, #fff);
  box-shadow:
    0 24px 48px -12px rgba(0, 0, 0, 0.18),
    0 0 0 1px rgba(0, 0, 0, 0.05);
  max-height: calc(100vh - 80px);
  display: flex;
  flex-direction: column;
}

.key-editor-card :deep(.n-card-header) {
  padding: 16px 20px 12px;
  border-bottom: none;
  flex-shrink: 0;
}

.key-editor-card :deep(.n-card__content) {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 0 20px 16px;
  scrollbar-width: none;
  -ms-overflow-style: none;
}

.key-editor-card :deep(.n-card__content)::-webkit-scrollbar {
  display: none;
}

.key-editor-card :deep(.n-card__footer) {
  padding: 12px 20px 16px;
  border-top: 1px solid var(--border-color, rgba(0, 0, 0, 0.06));
  flex-shrink: 0;
}

.editor-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.editor-eyebrow {
  font-size: 10px;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--text-secondary, #6b7280);
  margin-bottom: 2px;
  font-weight: 500;
}

.editor-title {
  font-size: 17px;
  line-height: 1.3;
  font-weight: 600;
  color: var(--text-primary, #111827);
  letter-spacing: -0.01em;
}

.editor-content {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.editor-hero {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  padding: 12px 14px;
  border-radius: 10px;
  background: var(--bg-secondary, #f9fafb);
  border: 1px solid var(--border-color, rgba(0, 0, 0, 0.06));
}

.editor-hero-main {
  min-width: 0;
}

.editor-hero-label,
.editor-side-label {
  font-size: 10px;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--text-secondary, #6b7280);
  font-weight: 500;
}

.editor-hero-value {
  margin-top: 4px;
  font-family: 'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, Courier, monospace;
  font-size: 13px;
  line-height: 1.4;
  color: var(--text-primary, #111827);
  word-break: break-all;
}

.editor-hero-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 8px;
}

.editor-hero-chip {
  display: inline-flex;
  align-items: center;
  height: 22px;
  padding: 0 8px;
  border-radius: 6px;
  font-size: 11px;
  font-weight: 500;
  border: none;
}

.editor-hero-chip.status-valid {
  color: #15803d;
  background: rgba(22, 163, 74, 0.1);
}

.editor-hero-chip.status-invalid {
  color: #b91c1c;
  background: rgba(220, 38, 38, 0.08);
}

.editor-hero-chip.neutral {
  color: var(--text-secondary, #475569);
  background: var(--bg-tertiary, rgba(0, 0, 0, 0.04));
}

.editor-hero-side {
  padding-left: 16px;
  border-left: 1px solid var(--border-color, rgba(0, 0, 0, 0.06));
  flex-shrink: 0;
}

.editor-side-value {
  margin-top: 4px;
  font-size: 13px;
  line-height: 1.4;
  color: var(--text-primary, #0f172a);
  word-break: break-word;
}

.editor-panel {
  padding: 12px 14px;
  border-radius: 10px;
  border: 1px solid var(--border-color, rgba(0, 0, 0, 0.06));
  background: var(--card-bg-solid, #fff);
}

.editor-panel-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 10px;
}

.editor-panel-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--text-primary, #111827);
}

.editor-panel-desc {
  margin-top: 2px;
  font-size: 11px;
  line-height: 1.5;
  color: var(--text-secondary, #6b7280);
}

.editor-help-icon {
  margin-top: 2px;
  font-size: 14px;
  color: var(--text-secondary, #94a3b8);
  cursor: help;
}

.editor-basic-row {
  display: flex;
  align-items: center;
  gap: 12px;
}

.edit-field-inline {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}

.edit-label {
  font-size: 12px;
  font-weight: 500;
  color: var(--text-secondary, #6b7280);
  white-space: nowrap;
}

.editor-panel.collapsible {
  padding: 0;
}

.editor-panel-head.clickable {
  cursor: pointer;
  padding: 10px 14px;
  border-radius: 10px;
  user-select: none;
  transition: background 0.15s;
}

.editor-panel-head.clickable:hover {
  background: var(--bg-secondary, rgba(0, 0, 0, 0.02));
}

.panel-head-right {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
}

.collapse-icon {
  font-size: 16px;
  color: var(--text-secondary, #94a3b8);
  transition: transform 0.2s ease;
  flex-shrink: 0;
}

.collapse-icon.open {
  transform: rotate(180deg);
}

.collapsible-body {
  padding: 0 14px 12px;
}

.editor-notes-field {
  min-width: 0;
}

.config-items {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.key-config-row {
  display: grid;
  grid-template-columns: minmax(0, 1.1fr) minmax(0, 0.9fr) 28px;
  gap: 8px;
  align-items: center;
}

.config-select,
.config-value {
  min-width: 0;
}

.config-switch {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  min-height: 34px;
  padding: 0 10px;
  border-radius: 8px;
  border: 1px solid var(--border-color, rgba(0, 0, 0, 0.06));
  background: var(--bg-secondary, #f9fafb);
  color: var(--text-secondary, #475569);
  font-size: 12px;
  font-weight: 500;
}

.config-actions {
  display: flex;
  justify-content: center;
}

.editor-empty {
  padding: 14px;
  border-radius: 8px;
  border: 1px dashed var(--border-color, rgba(0, 0, 0, 0.1));
  background: var(--bg-secondary, #f9fafb);
}

.editor-empty-title {
  font-size: 12px;
  font-weight: 600;
  color: var(--text-primary, #0f172a);
}

.editor-empty-desc {
  margin-top: 4px;
  font-size: 11px;
  line-height: 1.5;
  color: var(--text-secondary, #6b7280);
}

.editor-add-button {
  width: 100%;
  margin-top: 8px;
}

.editor-footer {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}

.row-spacer {
  flex: 1;
}

.icon-actions {
  display: flex;
  align-items: center;
  gap: 2px;
  flex-shrink: 0;
}

.icon-actions :deep(.n-button) {
  color: var(--text-secondary);
}

/* Row 2 */
.row-metrics {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  font-size: 11px;
  color: var(--text-secondary);
  padding: 4px 0;
}

.metric {
  white-space: nowrap;
}

.metric strong {
  font-weight: 600;
  color: var(--text-primary);
}

.metric-sep {
  width: 1px;
  height: 10px;
  background: rgba(120, 120, 120, 0.2);
  flex-shrink: 0;
}

/* Row 3 */
.row-actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 6px;
}

.empty-container {
  min-height: 220px;
  display: flex;
  align-items: center;
  justify-content: center;
}

.pagination-container {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 12px 16px;
  background: var(--card-bg-solid);
  border-top: 1px solid var(--border-color);
  flex-shrink: 0;
}

.pagination-info {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 12px;
  color: var(--text-secondary);
}

.pagination-controls {
  display: flex;
  align-items: center;
  gap: 12px;
}

.page-info {
  font-size: 12px;
  color: var(--text-secondary);
}

@media (max-width: 960px) {
  .toolbar {
    flex-direction: column;
    align-items: stretch;
  }

  .toolbar-left,
  .toolbar-right {
    width: 100%;
  }

  .toolbar-right :deep(.n-space) {
    width: 100%;
    justify-content: space-between;
    flex-wrap: wrap;
  }

  .key-editor-modal {
    width: calc(100vw - 24px);
  }

  .editor-hero {
    flex-direction: column;
  }

  .editor-hero-side {
    padding-left: 0;
    padding-top: 12px;
    border-left: none;
    border-top: 1px solid rgba(15, 23, 42, 0.08);
  }

  .editor-basic-row {
    flex-direction: column;
    align-items: stretch;
  }

  .edit-field-inline {
    width: 100%;
  }

  .key-config-row {
    grid-template-columns: 1fr;
  }

  .config-actions {
    justify-content: flex-end;
  }
}

@media (max-width: 768px) {
  .keys-grid-container {
    padding: 12px;
  }

  .keys-grid {
    grid-template-columns: 1fr;
  }

  .pagination-container {
    flex-direction: column;
    align-items: stretch;
    gap: 10px;
  }

  .pagination-controls {
    justify-content: space-between;
  }

  .key-editor-card :deep(.n-card__content) {
    padding: 0 14px 14px;
  }

  .editor-panel,
  .editor-hero {
    padding: 10px 12px;
  }
}

@media (max-width: 520px) {
  .row-main {
    flex-wrap: wrap;
  }

  .row-metrics {
    gap: 4px;
  }
}
</style>
