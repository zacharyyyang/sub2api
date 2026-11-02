import { computed, reactive, ref } from 'vue';

import { claudeModels } from '@/composables/useModelWhitelist';
import type { ModelCatalogEntry } from '@/types';

// 分组模型目录（壳 ID + 显示名 + 真实模型）的前端状态机。
//
// 与旧机制模块 groupModelAllowlist.ts 并列、互不引用：目录的行集、自动发号、行校验与
// 提交构造都在本文件，旧显示名表仍由旧模块负责。壳 ID 规则与后端同一套（设计 §2.1 /
// §2.2 的归一化与占位集合），两边不一致时表现为保存 400，不会静默落错数据。

// 目录条目的线上形状由 types/index.ts 唯一定义，这里只借类型、不重复声明。
export type ModelCatalogConfigEntry = ModelCatalogEntry;

// 目录表的一行（界面态）：键稳定用于 v-for；三个字段都是输入框原文，提交时才 trim。
export interface GroupModelCatalogRow {
  key: number;
  shellID: string;
  displayName: string;
  realModel: string;
  supports1M: boolean;
}

// 目录校验与发号的对照面。按结构取用旧机制的状态（提交态白名单条目、旧显示名键），
// 不 import 旧模块的类型——两个机制各自演进，目录只读取它需要的字段。
export interface CatalogOverviewSource {
  enabled?: boolean;
  items?: { id: string; selected: boolean }[];
  savedModels?: string[];
  loadedConfig?: boolean;
  selectionEdited?: boolean;
  draftDisplayNames?: Record<string, string>;
  // 平台候选模型（管理端候选接口原样返回，anthropic 平台含真 Claude 默认模型名）。
  candidates?: string[];
}

export type CatalogRowError =
  | 'shellRequired'
  | 'shellFormat'
  | 'shellDuplicate'
  | 'shellReserved'
  | 'shellTakenByRealModel'
  | 'displayNameRequired'
  | 'realModelRequired'
  | 'realModelWildcard'
  | 'realModelNotAllowed';

// 壳 ID 形态：CCD 只认 sonnet / opus / haiku / fable / mythos 六个家族，
// 家族外的 id 客户端根本不显示（后端同一条正则，比较前转小写）。
const CATALOG_SHELL_ID_PATTERN = /^claude-(?:sonnet|opus|haiku|fable|mythos)-[a-z0-9][a-z0-9._-]*$/;

// 自动壳 ID：claude-mythos-<n>。mythos 在真 Claude 里空闲，数字后缀让后端
// NormalizeInboundModelID 判为原生名而不剥前缀（设计 §2.2）。
export const CATALOG_AUTO_SHELL_PREFIX = 'claude-mythos-';

export const isValidCatalogShellID = (raw: string): boolean =>
  CATALOG_SHELL_ID_PATTERN.test(raw.trim().toLowerCase());

export interface CatalogShellIDContext {
  // 本组目录已占用的壳 ID。
  occupied?: string[];
  // 本组存量真实模型 ID（白名单提交态条目 ∪ 旧显示名映射键）。
  realModelIDs?: string[];
  // 真 Claude 模型名。
  reservedModelIDs?: string[];
}

const lowerSet = (values: Iterable<string> = []): Set<string> => {
  const out = new Set<string>();
  for (const value of values) {
    const normalized = (value ?? '').trim().toLowerCase();
    if (normalized) {
      out.add(normalized);
    }
  }
  return out;
};

// catalogShellIDPlaceholders = 生成时跳过、保存时拒绝的同一套占位集合：
// 真 Claude 模型名 ∪ 本组存量真实模型 ID ∪ 本组目录已占用的壳 ID。
// 生成与保存都只经本函数求并，不各写一遍。
export const catalogShellIDPlaceholders = (
  context: CatalogShellIDContext = {},
): Set<string> =>
  new Set([
    ...lowerSet(context.occupied),
    ...lowerSet(context.realModelIDs),
    ...lowerSet(context.reservedModelIDs),
  ]);

// nextCatalogShellID 取最小的未被占用正整数编号（从 1 起，无上限）。
export const nextCatalogShellID = (context: CatalogShellIDContext = {}): string => {
  const placeholders = catalogShellIDPlaceholders(context);
  for (let n = 1; ; n += 1) {
    const candidate = `${CATALOG_AUTO_SHELL_PREFIX}${n}`;
    if (!placeholders.has(candidate)) {
      return candidate;
    }
  }
};

// resolveSubmittedModels 与旧机制提交规则同形（groupModelAllowlist.buildModelAllowlistConfig）：
// 关闭态且未动过选择时原样回传载入值，否则取选中行；选中行为空时回退载入值。
const resolveSubmittedModels = (source: CatalogOverviewSource): string[] => {
  if (!source.enabled && source.loadedConfig && !source.selectionEdited) {
    return [...(source.savedModels ?? [])];
  }
  const items = source.items ?? [];
  return items.length > 0
    ? items.filter(item => item.selected).map(item => item.id)
    : [...(source.savedModels ?? [])];
};

// 真实模型候选：白名单启用时取自提交态 models（看到什么 = 能调什么），
// 关闭时给平台候选当输入提示。原文大小写保留，只按小写去重。
export const catalogRealModelOptions = (source: CatalogOverviewSource = {}): string[] => {
  const values = source.enabled ? resolveSubmittedModels(source) : (source.candidates ?? []);
  const seen = new Set<string>();
  const options: string[] = [];
  for (const value of values) {
    const trimmed = (value ?? '').trim();
    const key = trimmed.toLowerCase();
    // 通配条目是模式不是真实模型，不放进候选（输入框仍可自由输入具体模型）。
    if (!trimmed || trimmed.includes('*') || seen.has(key)) {
      continue;
    }
    seen.add(key);
    options.push(trimmed);
  }
  return options;
};

// 白名单模式匹配：与后端 groupAllowlistPatternMatches 同规则——只认 * 通配、
// 锚定、大小写不敏感、* 可出现在任意位置。
export const catalogPatternMatches = (pattern: string, model: string): boolean => {
  const p = (pattern ?? '').trim().toLowerCase();
  const m = (model ?? '').trim().toLowerCase();
  if (!p) {
    return false;
  }
  const parts = p.split('*');
  if (parts.length === 1) {
    return p === m;
  }
  const head = parts[0];
  if (!m.startsWith(head)) {
    return false;
  }
  let rest = m.slice(head.length);
  for (const part of parts.slice(1, -1)) {
    const index = rest.indexOf(part);
    if (index < 0) {
      return false;
    }
    rest = rest.slice(index + part.length);
  }
  return rest.endsWith(parts[parts.length - 1]);
};

// 真 Claude 模型名 = 仓内既有清单 claudeModels（覆盖后端 claude.DefaultModelIDs 的全部条目）。
// 平台候选**不进**这个集合：候选是输入提示（按平台过滤，可能一条 Claude 名都没有），
// 只有真 Claude 名才属于「壳 ID 不得占用」那一类。
export const CATALOG_RESERVED_CLAUDE_MODEL_IDS: Set<string> = lowerSet(claudeModels);

// 存量真实模型 ID：白名单提交态条目 ∪ 旧显示名映射键。目录壳 ID 不得抢占它们，
// 否则入站反查会把今天指向该真实模型的请求静默改道（后端 KDR-9 的同一理由）。
export const catalogTakenByRealModelIDs = (
  source: CatalogOverviewSource = {},
): Set<string> =>
  lowerSet([...resolveSubmittedModels(source), ...Object.keys(source.draftDisplayNames ?? {})]);

// catalogShellIDContext 汇总生成用的三个来源，经 catalogShellIDPlaceholders 求并；
// 保存校验走同一个 Placeholders 函数（occupied 维由 shellDuplicate 单独点名）。
const catalogShellIDContext = (
  rows: GroupModelCatalogRow[],
  source: CatalogOverviewSource = {},
): CatalogShellIDContext => ({
  occupied: rows.map(row => row.shellID),
  realModelIDs: [...catalogTakenByRealModelIDs(source)],
  reservedModelIDs: [...CATALOG_RESERVED_CLAUDE_MODEL_IDS],
});

const nextRowKey = (rows: GroupModelCatalogRow[]): number =>
  rows.reduce((max, row) => (row.key > max ? row.key : max), 0) + 1;

// catalogRowsFrom 把后端配置转成界面行（字段 trim、1M 开关转布尔）。
export const catalogRowsFrom = (
  catalog?: Partial<ModelCatalogConfigEntry>[] | null,
): GroupModelCatalogRow[] =>
  (catalog ?? []).map((entry, index) => ({
    key: index + 1,
    shellID: (entry.shell_id ?? '').trim(),
    displayName: (entry.display_name ?? '').trim(),
    realModel: (entry.real_model ?? '').trim(),
    supports1M: entry.supports_1m === true,
  }));

// appendCatalogRow 追加一行：壳 ID 预填下一个可用值，显示名与真实模型留空（留空即不能保存）。
export const appendCatalogRow = (
  rows: GroupModelCatalogRow[],
  source: CatalogOverviewSource = {},
): GroupModelCatalogRow[] => [
  ...rows,
  {
    key: nextRowKey(rows),
    shellID: nextCatalogShellID(catalogShellIDContext(rows, source)),
    displayName: '',
    realModel: '',
    supports1M: false,
  },
];

// convertDisplayNameToCatalogRow 「转为目录行」：新增一行目录（壳 ID 自动、显示名取旧值、
// 真实模型取该行模型 id）。旧 display_names 键不动——非 CC 客户端只吃旧机制，
// 删键是管理员的独立一步（设计 §8）。
export const convertDisplayNameToCatalogRow = (
  rows: GroupModelCatalogRow[],
  source: CatalogOverviewSource = {},
  modelID: string,
  displayName: string,
): GroupModelCatalogRow[] => [
  ...rows,
  {
    key: nextRowKey(rows),
    shellID: nextCatalogShellID(catalogShellIDContext(rows, source)),
    displayName: (displayName ?? '').trim(),
    realModel: (modelID ?? '').trim(),
    supports1M: false,
  },
];

export const updateCatalogRow = (
  rows: GroupModelCatalogRow[],
  key: number,
  patch: Partial<Omit<GroupModelCatalogRow, 'key'>>,
): GroupModelCatalogRow[] => rows.map(row => (row.key === key ? { ...row, ...patch } : row));

export const removeCatalogRow = (
  rows: GroupModelCatalogRow[],
  key: number,
): GroupModelCatalogRow[] => rows.filter(row => row.key !== key);

// catalogRowErrors 逐行给出阻断保存的原因（按行 key 索引）。判定线与后端 §2.1 的
// 归一化规则同一套：格式、重复、占位集合、必填、白名单成员（启用时）。
export const catalogRowErrors = (
  rows: GroupModelCatalogRow[],
  source: CatalogOverviewSource = {},
): Record<number, CatalogRowError[]> => {
  const whitelistEnabled = source.enabled === true;
  const submittedModels = resolveSubmittedModels(source);
  const reservedClaudeIDs = CATALOG_RESERVED_CLAUDE_MODEL_IDS;
  const takenByRealModel = catalogTakenByRealModelIDs(source);
  // 与生成共用一个占位集合（catalogShellIDPlaceholders）；本行自身不算占用，
  // 同行重复由 shellDuplicate 单独点名。
  const placeholders = catalogShellIDPlaceholders({
    realModelIDs: [...takenByRealModel],
    reservedModelIDs: [...reservedClaudeIDs],
  });

  const shellCounts = new Map<string, number>();
  for (const row of rows) {
    const shell = row.shellID.trim().toLowerCase();
    if (shell) {
      shellCounts.set(shell, (shellCounts.get(shell) ?? 0) + 1);
    }
  }

  const errors: Record<number, CatalogRowError[]> = {};
  const push = (key: number, error: CatalogRowError) => {
    errors[key] = [...(errors[key] ?? []), error];
  };

  for (const row of rows) {
    const shell = row.shellID.trim().toLowerCase();
    const displayName = row.displayName.trim();
    const realModel = row.realModel.trim();

    if (!shell) {
      push(row.key, 'shellRequired');
    } else if (!isValidCatalogShellID(shell)) {
      push(row.key, 'shellFormat');
    } else if ((shellCounts.get(shell) ?? 0) > 1) {
      push(row.key, 'shellDuplicate');
    } else if (placeholders.has(shell)) {
      push(row.key, reservedClaudeIDs.has(shell) ? 'shellReserved' : 'shellTakenByRealModel');
    }

    if (!displayName) {
      push(row.key, 'displayNameRequired');
    }

    if (!realModel) {
      push(row.key, 'realModelRequired');
    } else if (realModel.includes('*')) {
      // 通配字面是白名单的匹配模式，不是模型名（设计 §2.1 规则 8）：存成 real_model 后，
      // 入站反查会把它原样发给上游，失败落在请求期、管理员拿不到原因。就地拦住，
      // 与白名单开关无关（后端在保存期同样 400，前端只是把报错提前到点击保存之前）。
      push(row.key, 'realModelWildcard');
    } else if (
      whitelistEnabled &&
      !submittedModels.some(pattern => catalogPatternMatches(pattern, realModel))
    ) {
      push(row.key, 'realModelNotAllowed');
    }
  }

  return errors;
};

export const catalogHasErrors = (
  rows: GroupModelCatalogRow[],
  source: CatalogOverviewSource = {},
): boolean => Object.keys(catalogRowErrors(rows, source)).length > 0;

// catalogRowErrorMessageKey 把行错误码映射成 i18n 键；文案落在 <prefix>.errors 下。
export const catalogRowErrorMessageKey = (prefix: string, error: CatalogRowError): string =>
  `${prefix}.errors.${error}`;

let catalogTableSequence = 0;

// nextGroupModelCatalogListID 给每条目录表的 datalist 一个页面内唯一 id：
// 新建与编辑两个抽屉的目录表可能同时在 DOM 里，id 撞车会让 datalist 串台。
export const nextGroupModelCatalogListID = (): string => {
  catalogTableSequence += 1;
  return `group-model-catalog-real-models-${catalogTableSequence}`;
};

// buildCatalogEntries 提交构造：逐字段 trim，整行空的不提交（不制造空条目），
// 顺序按当前行序；supports_1m 只在勾选时出现（与后端 omitempty 同形）。
export const buildCatalogEntries = (
  rows: GroupModelCatalogRow[],
): ModelCatalogConfigEntry[] => {
  const entries: ModelCatalogConfigEntry[] = [];
  for (const row of rows) {
    const shellID = row.shellID.trim();
    const displayName = row.displayName.trim();
    const realModel = row.realModel.trim();
    if (!shellID && !displayName && !realModel) {
      continue;
    }
    const entry: ModelCatalogConfigEntry = {
      shell_id: shellID,
      display_name: displayName,
      real_model: realModel,
    };
    if (row.supports1M) {
      entry.supports_1m = true;
    }
    entries.push(entry);
  }
  return entries;
};

// catalogAllowlistPatch 供保存时并入 model_allowlist：目录为空时键不出现，
// 与后端 omitempty 的存量分组输出逐字一致。
export const catalogAllowlistPatch = (
  rows: GroupModelCatalogRow[],
): { catalog?: ModelCatalogConfigEntry[] } => {
  const entries = buildCatalogEntries(rows);
  return entries.length > 0 ? { catalog: entries } : {};
};

// createGroupModelCatalogSession 一个抽屉一份的目录会话：行集、平台候选、校验与提交
// 的编排都在这里，视图只保留 v-model 绑定与生命周期调用（load / setCandidates / convert /
// allowlistPatch / invalid）。legacy 传旧机制的 reactive 状态，按结构取用（不 import 旧模块）。
// 返回值用 reactive 包一层：ref / computed 在模板里自动解包，v-model:rows 可直接写回。
export const createGroupModelCatalogSession = (legacy: CatalogOverviewSource) => {
  const rows = ref<GroupModelCatalogRow[]>([]);
  const candidates = ref<string[]>([]);
  const source = computed<CatalogOverviewSource>(() => ({
    ...legacy,
    candidates: candidates.value,
  }));
  return reactive({
    rows,
    candidates,
    source,
    // 任一目录行不合法 ⇒ 保存按钮置灰（后端仍兜底 400）。
    invalid: computed(() => catalogHasErrors(rows.value, source.value)),
    // setCandidates 写入平台候选；先清空再写，切换平台 / 分组失败时不会残留上一份提示。
    setCandidates(models: string[]) {
      candidates.value = [...models];
    },
    // load 载入某个分组的目录；不传 = 清空（新建抽屉、切换平台）。
    load(catalog?: Partial<ModelCatalogConfigEntry>[] | null) {
      rows.value = catalogRowsFrom(catalog);
    },
    // convert 「转为目录行」：只新增行，旧键保留。
    convert(modelID: string, displayName: string) {
      rows.value = convertDisplayNameToCatalogRow(
        rows.value,
        source.value,
        modelID,
        displayName,
      );
    },
    // allowlistPatch 保存时并入 model_allowlist 的那一部分。
    allowlistPatch(): { catalog?: ModelCatalogConfigEntry[] } {
      return catalogAllowlistPatch(rows.value);
    },
  });
};