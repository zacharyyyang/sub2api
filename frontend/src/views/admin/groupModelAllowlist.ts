export interface ModelAllowlistConfig {
  enabled: boolean
  models: string[]
  // 显示名映射：模型 id 原文 → 客户端模型选择器里展示的名字。
  display_names?: Record<string, string>
}

export interface ModelAllowlistItem {
  id: string
  selected: boolean
}

export interface ModelAllowlistDisplayNameRow {
  id: string
  displayName: string
}

export interface ModelAllowlistState {
  enabled: boolean
  savedModels: string[]
  // 当前有效显示名映射：显示名表与保存提交的唯一取值来源。
  // 本会话的增 / 改 / 删只写它，切换白名单开关不搬运值；无原型对象防止 id 命中原型属性。
  draftDisplayNames: Record<string, string>
  items: ModelAllowlistItem[]
  // 是否载入自既有分组配置（新建分组为 false）。
  loadedConfig: boolean
  // 本会话是否动过白名单选择（勾选 / 全选 / 反选 / 手工添加 / 排序）。
  selectionEdited: boolean
}

// 自定义条目校验错误码，由视图映射为 i18n 提示。
export type ModelAllowlistAddError = 'empty' | 'duplicate'

export const createModelAllowlistState = (
  config?: Partial<ModelAllowlistConfig> | null,
): ModelAllowlistState => ({
  enabled: config?.enabled ?? false,
  savedModels: normalizeModels(config?.models ?? []),
  draftDisplayNames: normalizeDisplayNames(config?.display_names),
  items: [],
  loadedConfig: config !== undefined && config !== null,
  selectionEdited: false,
})

export const hydrateModelAllowlistState = (
  config: Partial<ModelAllowlistConfig> | null | undefined,
  candidates: string[],
): ModelAllowlistState => {
  const state = createModelAllowlistState(config)
  setModelAllowlistCandidates(state, candidates)
  return state
}

// applyModelAllowlistConfig 用一份配置就地替换状态的全部字段（含载入的显示名）。
// 整体替换而不是逐字段拷贝：状态新增字段时逐字段拷贝会漏拷，配置就会静默丢失。
export const applyModelAllowlistConfig = (
  state: ModelAllowlistState,
  config?: Partial<ModelAllowlistConfig> | null,
) => {
  Object.assign(state, createModelAllowlistState(config))
}

export const setModelAllowlistCandidates = (
  state: ModelAllowlistState,
  candidates: string[],
) => {
  const normalizedCandidates = normalizeModels(candidates)
  const currentSelected = new Set(
    state.items.filter(item => item.selected).map(item => item.id),
  )
  const currentKnown = new Set(state.items.map(item => item.id))
  const savedSelected = new Set(state.savedModels)
  const hasExistingItems = state.items.length > 0
  const selectionOrder = normalizeModels([
    ...state.items.map(item => item.id),
    ...state.savedModels,
    ...normalizedCandidates,
  ])

  state.items = selectionOrder.map(id => {
    const selected = hasExistingItems
      ? currentSelected.has(id)
      : state.savedModels.length > 0
        ? savedSelected.has(id)
        : normalizedCandidates.includes(id)

    return {
      id,
      selected: selected && (currentKnown.has(id) || savedSelected.has(id) || state.savedModels.length === 0),
    }
  })
}

export const toggleModelAllowlistItem = (
  state: ModelAllowlistState,
  modelID: string,
) => {
  const item = state.items.find(item => item.id === modelID)
  if (item) {
    item.selected = !item.selected
    state.selectionEdited = true
  }
}

export const selectAllModelAllowlistItems = (state: ModelAllowlistState) => {
  state.items.forEach(item => {
    item.selected = true
  })
  state.selectionEdited = true
}

export const invertModelAllowlistSelection = (state: ModelAllowlistState) => {
  state.items.forEach(item => {
    item.selected = !item.selected
  })
  state.selectionEdited = true
}

export const moveModelAllowlistItem = (
  state: ModelAllowlistState,
  fromIndex: number,
  toIndex: number,
) => {
  if (
    fromIndex === toIndex ||
    fromIndex < 0 ||
    toIndex < 0 ||
    fromIndex >= state.items.length ||
    toIndex >= state.items.length
  ) {
    return
  }
  const [item] = state.items.splice(fromIndex, 1)
  state.items.splice(toIndex, 0, item)
  state.selectionEdited = true
}

// addCustomModelAllowlistItem 把手工输入的条目追加到白名单末尾（选中状态）。
// 去重；`*` 可出现在任意位置。返回错误码或 null（成功）。
// 显示名不随条目新增而创建：已有显示名留在 draftDisplayNames 里按当前值提交。
export const addCustomModelAllowlistItem = (
  state: ModelAllowlistState,
  raw: string,
): ModelAllowlistAddError | null => {
  const entry = raw.trim()
  if (!entry) {
    return 'empty'
  }
  if (
    state.items.some(item => item.id.toLowerCase() === entry.toLowerCase()) ||
    state.savedModels.some(model => model.toLowerCase() === entry.toLowerCase())
  ) {
    return 'duplicate'
  }
  state.items.push({ id: entry, selected: true })
  state.selectionEdited = true
  return null
}

// setModelAllowlistDisplayName 写「当前有效映射」。trim 后为空 = 显式删除该键（「不填就没有」）；
// 非空值原样保存，提交时再 trim。
export const setModelAllowlistDisplayName = (
  state: ModelAllowlistState,
  modelID: string,
  rawValue: string,
) => {
  const id = modelID.trim()
  if (!id) {
    return
  }
  if (!rawValue.trim()) {
    delete state.draftDisplayNames[id]
    return
  }
  state.draftDisplayNames[id] = rawValue
}

// removeModelAllowlistDisplayName 从「当前有效映射」删除该键；切换开关不复活。
export const removeModelAllowlistDisplayName = (
  state: ModelAllowlistState,
  modelID: string,
) => {
  delete state.draftDisplayNames[modelID.trim()]
}

// displayNameTableRows 显示名表的行：读 draftDisplayNames，行 = 其键与当前值（与白名单开关无关）。
export const displayNameTableRows = (
  state: ModelAllowlistState,
): ModelAllowlistDisplayNameRow[] => {
  return Object.entries(state.draftDisplayNames).map(([id, displayName]) => ({
    id,
    displayName,
  }))
}

// buildModelAllowlistConfig 生成提交给分组接口的 model_allowlist。
// 显示名映射取 draftDisplayNames 逐键 trim 后的非空值（空值丢弃 = 显式删除），
// 取值只认 draft，不按「该行有没有输入框」分流。
// models：关闭态且未动过白名单选择时原样回传载入值——只改显示名的保存不得改动 models
// （载入值为空时也不得被候选默认全选改写）；其它情形沿用既有逻辑（选中行 / 候选未载入时回传载入值）。
export const buildModelAllowlistConfig = (
  state: ModelAllowlistState,
): ModelAllowlistConfig => {
  const displayNames = Object.create(null) as Record<string, string>
  for (const [rawID, rawValue] of Object.entries(state.draftDisplayNames)) {
    const id = rawID.trim()
    const value = (rawValue ?? '').trim()
    if (!id || !value) {
      continue
    }
    displayNames[id] = value
  }

  const config: ModelAllowlistConfig = {
    enabled: state.enabled,
    models: resolveSubmittedModels(state),
  }
  if (Object.keys(displayNames).length > 0) {
    config.display_names = displayNames
  }
  return config
}

const resolveSubmittedModels = (state: ModelAllowlistState): string[] => {
  if (!state.enabled && state.loadedConfig && !state.selectionEdited) {
    return [...state.savedModels]
  }
  return state.items.length > 0
    ? state.items.filter(item => item.selected).map(item => item.id)
    : [...state.savedModels]
}

export const selectedModelAllowlistCount = (state: ModelAllowlistState): number =>
  state.items.filter(item => item.selected).length

const normalizeModels = (models: string[]): string[] => {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of models) {
    const model = raw.trim()
    if (!model || seen.has(model)) {
      continue
    }
    seen.add(model)
    out.push(model)
  }
  return out
}

// normalizeDisplayNames 与后端归一化同形：键与值都 trim，空键 / 空值丢弃。
// 用无原型字典：否则 id 为 constructor / __proto__ 时会读到（或覆盖）继承属性。
const normalizeDisplayNames = (
  displayNames: Record<string, string> | undefined,
): Record<string, string> => {
  const out = Object.create(null) as Record<string, string>
  for (const [rawKey, rawValue] of Object.entries(displayNames ?? {})) {
    const key = rawKey.trim()
    const value = (rawValue ?? '').trim()
    if (!key || !value) {
      continue
    }
    out[key] = value
  }
  return out
}
