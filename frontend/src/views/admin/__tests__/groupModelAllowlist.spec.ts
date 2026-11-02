import { describe, expect, it } from "vitest";

import {
  applyModelAllowlistConfig,
  addCustomModelAllowlistItem,
  buildModelAllowlistConfig,
  createModelAllowlistState,
  displayNameTableRows,
  hydrateModelAllowlistState,
  invertModelAllowlistSelection,
  moveModelAllowlistItem,
  removeModelAllowlistDisplayName,
  selectAllModelAllowlistItems,
  setModelAllowlistCandidates,
  setModelAllowlistDisplayName,
  toggleModelAllowlistItem,
  type ModelAllowlistAddError,
} from "../groupModelAllowlist";

describe("groupModelAllowlist", () => {
  it("selects all default candidates for a new disabled config", () => {
    const state = createModelAllowlistState();

    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4"]);

    expect(state.enabled).toBe(false);
    expect(state.items).toEqual([
      { id: "gpt-5.5", selected: true },
      { id: "gpt-5.4", selected: true },
    ]);
  });

  it("keeps saved selections and marks new candidates as unselected when editing", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5", "gpt-5.4"],
    });

    setModelAllowlistCandidates(state, ["gpt-5.4", "legacy-gpt", "gpt-5.5"]);

    expect(state.enabled).toBe(true);
    expect(state.items.map(item => [item.id, item.selected])).toEqual([
      ["gpt-5.5", true],
      ["gpt-5.4", true],
      ["legacy-gpt", false],
    ]);
  });

  it("preserves explicitly unselected saved candidates when candidates refresh", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5"],
    });

    setModelAllowlistCandidates(state, ["gpt-5.5", "gpt-5.4"]);

    expect(state.items).toEqual([
      { id: "gpt-5.5", selected: true },
      { id: "gpt-5.4", selected: false },
    ]);
  });

  it("builds config with selected models in current display order", () => {
    const state = hydrateModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5", "gpt-5.4", "legacy-gpt"],
    }, ["gpt-5.5", "gpt-5.4", "legacy-gpt"]);

    toggleModelAllowlistItem(state, "legacy-gpt");
    moveModelAllowlistItem(state, 1, 0);

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: true,
      models: ["gpt-5.4", "gpt-5.5"],
    });
  });

  it("keeps selected models in payload even when disabled so reopening can restore choices", () => {
    const state = hydrateModelAllowlistState({
      enabled: false,
      models: ["gpt-5.5"],
    }, ["gpt-5.5", "gpt-5.4"]);

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: false,
      models: ["gpt-5.5"],
    });
  });

  it("preserves saved models when candidates have not loaded yet", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5", "gpt-5.4"],
    });

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: true,
      models: ["gpt-5.5", "gpt-5.4"],
    });
  });

  it("selects all candidate models from the toolbar action", () => {
    const state = hydrateModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5"],
    }, ["gpt-5.5", "gpt-5.4", "gpt-5.4-mini"]);

    selectAllModelAllowlistItems(state);

    expect(state.items.map(item => [item.id, item.selected])).toEqual([
      ["gpt-5.5", true],
      ["gpt-5.4", true],
      ["gpt-5.4-mini", true],
    ]);
  });

  it("inverts selected models from the toolbar action", () => {
    const state = hydrateModelAllowlistState({
      enabled: true,
      models: ["gpt-5.5"],
    }, ["gpt-5.5", "gpt-5.4", "gpt-5.4-mini"]);

    invertModelAllowlistSelection(state);

    expect(state.items.map(item => [item.id, item.selected])).toEqual([
      ["gpt-5.5", false],
      ["gpt-5.4", true],
      ["gpt-5.4-mini", true],
    ]);
  });
});

describe("addCustomModelAllowlistItem", () => {
  const state = () => createModelAllowlistState({ enabled: true, models: ["gpt-5.4"] });

  it("appends a trimmed entry as selected", () => {
    const s = state();
    expect(addCustomModelAllowlistItem(s, "  claude-sonnet-4.5  ")).toBeNull();
    expect(s.items.at(-1)).toEqual({ id: "claude-sonnet-4.5", selected: true });
  });

  it("accepts trailing wildcard entries", () => {
    const s = state();
    expect(addCustomModelAllowlistItem(s, "gpt-5.5-*")).toBeNull();
    expect(s.items.at(-1)?.id).toBe("gpt-5.5-*");
  });

  it("rejects blank input", () => {
    expect(addCustomModelAllowlistItem(state(), "   ")).toBe<ModelAllowlistAddError>("empty");
  });

  it("accepts wildcards at any position", () => {
    const s = state();
    expect(addCustomModelAllowlistItem(s, "gpt-*-5.4")).toBeNull();
    expect(addCustomModelAllowlistItem(s, "*codex")).toBeNull();
    expect(addCustomModelAllowlistItem(s, "gpt-*-codex-*")).toBeNull();
    expect(buildModelAllowlistConfig(s).models).toEqual(["gpt-*-5.4", "*codex", "gpt-*-codex-*"]);
  });

  it("rejects duplicates case-insensitively", () => {
    const s = state();
    expect(addCustomModelAllowlistItem(s, "GPT-5.4")).toBe<ModelAllowlistAddError>("duplicate");
  });
});

describe("groupModelAllowlist display names", () => {
  const savedState = () => createModelAllowlistState({
    enabled: true,
    models: ["claude-a", "claude-b"],
    display_names: { "claude-a": "甲" },
  });

  it("loads model_allowlist.display_names into the draft map", () => {
    const state = savedState();
    setModelAllowlistCandidates(state, ["claude-a", "claude-b"]);

    expect(state.draftDisplayNames).toEqual({ "claude-a": "甲" });
    expect(state.items).toEqual([
      { id: "claude-a", selected: true },
      { id: "claude-b", selected: true },
    ]);
  });

  it("resets every state field when a saved config is reloaded", () => {
    const state = createModelAllowlistState();
    setModelAllowlistCandidates(state, ["claude-a"]);

    applyModelAllowlistConfig(state, {
      enabled: true,
      models: ["claude-a", "claude-b"],
      display_names: { "claude-a": "甲" },
    });
    setModelAllowlistCandidates(state, ["claude-a", "claude-b"]);

    expect(state.draftDisplayNames).toEqual({ "claude-a": "甲" });
    expect(buildModelAllowlistConfig(state).display_names).toEqual({ "claude-a": "甲" });
  });

  it("submits edited display names with the allowlist config", () => {
    const state = savedState();
    setModelAllowlistCandidates(state, ["claude-a", "claude-b"]);

    setModelAllowlistDisplayName(state, "claude-b", "  Claude B  ");

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: true,
      models: ["claude-a", "claude-b"],
      display_names: { "claude-a": "甲", "claude-b": "Claude B" },
    });
  });

  it("keeps the saved display name after the model is unselected", () => {
    const state = savedState();
    setModelAllowlistCandidates(state, ["claude-a", "claude-b"]);

    toggleModelAllowlistItem(state, "claude-a");

    const config = buildModelAllowlistConfig(state);
    expect(config.models).toEqual(["claude-b"]);
    expect(config.display_names).toEqual({ "claude-a": "甲" });
  });

  it("keeps the saved display name on a wildcard entry", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["*"],
      display_names: { "*": "X" },
    });
    setModelAllowlistCandidates(state, ["*"]);

    expect(buildModelAllowlistConfig(state).display_names).toEqual({ "*": "X" });
  });

  it("treats an emptied input as an explicit delete", () => {
    const state = savedState();
    setModelAllowlistCandidates(state, ["claude-a", "claude-b"]);

    setModelAllowlistDisplayName(state, "claude-a", "   ");

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: true,
      models: ["claude-a", "claude-b"],
    });
  });

  it("drops empty keys and values from the loaded config", () => {
    const state = createModelAllowlistState({
      enabled: false,
      models: [],
      display_names: { "": "x", "claude-a": "   " },
    });

    expect(state.draftDisplayNames).toEqual({});
    expect(buildModelAllowlistConfig(state)).toEqual({ enabled: false, models: [] });
  });

  it("keeps the saved display name when that model is added by hand", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["claude-b"],
      display_names: { "claude-a": "甲" },
    });
    setModelAllowlistCandidates(state, ["claude-b"]);

    expect(addCustomModelAllowlistItem(state, "claude-a")).toBeNull();
    expect(displayNameTableRows(state)).toEqual([{ id: "claude-a", displayName: "甲" }]);

    expect(buildModelAllowlistConfig(state).display_names).toEqual({ "claude-a": "甲" });
  });

  it("treats prototype key names as plain model ids", () => {
    // JSON.parse 造出真正的自有 "__proto__" 键（对象字面量里 "__proto__" 会改原型）。
    const savedDisplayNames: Record<string, string> = JSON.parse('{"__proto__":"守卫"}');
    const state = createModelAllowlistState({
      enabled: true,
      models: ["constructor"],
      display_names: savedDisplayNames,
    });

    setModelAllowlistCandidates(state, ["constructor"]);

    expect(state.items).toEqual([
      { id: "constructor", selected: true },
    ]);
    const names = buildModelAllowlistConfig(state).display_names ?? {};
    expect(Object.keys(names)).toEqual(["__proto__"]);
    expect(names["__proto__"]).toBe("守卫");
  });
});

describe("groupModelAllowlist display name table", () => {
  const disabledState = (displayNames?: Record<string, string>) =>
    createModelAllowlistState({
      enabled: false,
      models: [],
      display_names: displayNames,
    });

  it("lists loaded display names with an editable input while disabled", () => {
    const state = disabledState({ "claude-mythos-ds": "DeepSeek · V4.1 Flash" });

    expect(displayNameTableRows(state)).toEqual([
      { id: "claude-mythos-ds", displayName: "DeepSeek · V4.1 Flash" },
    ]);
  });

  it("edits and saves a display name without touching enabled or models", () => {
    const state = disabledState({ "claude-mythos-ds": "DeepSeek · V4.1 Flash" });

    setModelAllowlistDisplayName(state, "claude-mythos-ds", "DeepSeek V4.1");

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: false,
      models: [],
      display_names: { "claude-mythos-ds": "DeepSeek V4.1" },
    });
  });

  it("adds a display name row while disabled and keeps models untouched", () => {
    const state = disabledState();

    setModelAllowlistDisplayName(state, "claude-9x", "X");

    expect(displayNameTableRows(state)).toEqual([{ id: "claude-9x", displayName: "X" }]);
    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: false,
      models: [],
      display_names: { "claude-9x": "X" },
    });
  });

  it("keeps one row when the same model id is added again", () => {
    const state = disabledState({ "claude-a": "甲" });

    setModelAllowlistDisplayName(state, "claude-a", "乙");

    expect(displayNameTableRows(state)).toEqual([{ id: "claude-a", displayName: "乙" }]);
    expect(buildModelAllowlistConfig(state).display_names).toEqual({ "claude-a": "乙" });
  });

  it("deletes a display name row while disabled", () => {
    const state = disabledState({ "claude-a": "甲" });

    removeModelAllowlistDisplayName(state, "claude-a");

    expect(displayNameTableRows(state)).toEqual([]);
    expect(buildModelAllowlistConfig(state)).toEqual({ enabled: false, models: [] });
  });

  it("keeps renamed values after the switch is turned on before saving", () => {
    const state = disabledState({ "claude-a": "甲" });

    setModelAllowlistDisplayName(state, "claude-a", "乙");
    state.enabled = true;
    setModelAllowlistCandidates(state, ["claude-a", "claude-b"]);
    expect(displayNameTableRows(state)).toEqual([{ id: "claude-a", displayName: "乙" }]);
    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: true,
      models: ["claude-a", "claude-b"],
      display_names: { "claude-a": "乙" },
    });
  });

  it("keeps unsaved edits when the toggle is turned off", () => {
    const state = createModelAllowlistState({
      enabled: true,
      models: ["a", "b"],
      display_names: { a: "甲" },
    });
    setModelAllowlistCandidates(state, ["a", "b"]);

    setModelAllowlistDisplayName(state, "a", "乙");
    toggleModelAllowlistItem(state, "b");
    state.enabled = false;

    expect(displayNameTableRows(state)).toEqual([{ id: "a", displayName: "乙" }]);
    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: false,
      models: ["a"],
      display_names: { a: "乙" },
    });
  });

  it("keeps a value added while disabled after enabling and selecting another model", () => {
    const state = disabledState();

    setModelAllowlistDisplayName(state, "a", "甲");
    state.enabled = true;
    setModelAllowlistCandidates(state, ["a", "b"]);
    toggleModelAllowlistItem(state, "a");

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: true,
      models: ["b"],
      display_names: { a: "甲" },
    });
  });

  it("keeps a value edited while disabled after enabling with its row unselected", () => {
    const state = disabledState({ a: "甲" });

    setModelAllowlistDisplayName(state, "a", "乙");
    state.enabled = true;
    setModelAllowlistCandidates(state, ["a", "b"]);
    toggleModelAllowlistItem(state, "a");

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: true,
      models: ["b"],
      display_names: { a: "乙" },
    });
  });

  it("keeps a deletion made while disabled after the model is selected again", () => {
    const state = disabledState({ a: "甲" });

    removeModelAllowlistDisplayName(state, "a");
    state.enabled = true;
    setModelAllowlistCandidates(state, ["a"]);
    expect(state.draftDisplayNames.a).toBeUndefined();
    expect(buildModelAllowlistConfig(state)).toEqual({ enabled: true, models: ["a"] });
  });

  it("keeps an empty models list when only display names are edited while disabled", () => {
    // 真实编辑抽屉会先载入候选（空 models ⇒ 候选默认全选）；仅改显示名的保存不得把它变成全部候选。
    const state = disabledState({ "claude-a": "甲" });
    setModelAllowlistCandidates(state, ["claude-a", "claude-b"]);

    setModelAllowlistDisplayName(state, "claude-a", "乙");

    expect(buildModelAllowlistConfig(state)).toEqual({
      enabled: false,
      models: [],
      display_names: { "claude-a": "乙" },
    });
  });

  it("keeps the loaded models when only display names are edited while disabled", () => {
    const state = createModelAllowlistState({
      enabled: false,
      models: ["claude-a"],
      display_names: { "claude-a": "甲" },
    });
    setModelAllowlistCandidates(state, ["claude-a", "claude-b"]);

    removeModelAllowlistDisplayName(state, "claude-a");

    expect(buildModelAllowlistConfig(state)).toEqual({ enabled: false, models: ["claude-a"] });
  });

  it("uses the current selection once the whitelist selection is edited", () => {
    const state = disabledState();
    setModelAllowlistCandidates(state, ["a", "b"]);

    toggleModelAllowlistItem(state, "a");

    expect(buildModelAllowlistConfig(state)).toEqual({ enabled: false, models: ["b"] });
  });

  it("keeps the candidate default selection for a new group", () => {
    const state = createModelAllowlistState();
    setModelAllowlistCandidates(state, ["a", "b"]);

    expect(buildModelAllowlistConfig(state)).toEqual({ enabled: false, models: ["a", "b"] });
  });

  it("returns the same table rows whether the whitelist switch is on or off", () => {
    const state = disabledState({ "claude-a": "甲", "*": "X" });
    const rowsWhileDisabled = displayNameTableRows(state);

    state.enabled = true;
    setModelAllowlistCandidates(state, ["claude-a", "claude-b"]);

    expect(displayNameTableRows(state)).toEqual(rowsWhileDisabled);
    expect(displayNameTableRows(state)).toEqual([
      { id: "claude-a", displayName: "甲" },
      { id: "*", displayName: "X" },
    ]);
  });
});