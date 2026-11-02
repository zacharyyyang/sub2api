import { describe, expect, it } from "vitest";

import {
  appendCatalogRow,
  buildCatalogEntries,
  catalogAllowlistPatch,
  catalogHasErrors,
  catalogPatternMatches,
  catalogRealModelOptions,
  catalogRowErrors,
  catalogRowsFrom,
  catalogShellIDPlaceholders,
  CATALOG_RESERVED_CLAUDE_MODEL_IDS,
  convertDisplayNameToCatalogRow,
  createGroupModelCatalogSession,
  nextCatalogShellID,
  removeCatalogRow,
  updateCatalogRow,
  type GroupModelCatalogRow,
} from "../groupModelCatalog";

// 一条合法目录行的基线：各用例只改需要触发判定的字段。
const validRow = (overrides: Partial<GroupModelCatalogRow> = {}): GroupModelCatalogRow => ({
  key: 1,
  shellID: "claude-mythos-7",
  displayName: "DeepSeek V4",
  realModel: "deepseek-v4",
  supports1M: false,
  ...overrides,
});

describe("groupModelCatalog · 自动壳 ID（AC-7）", () => {
  it("空目录从 claude-mythos-1 起", () => {
    expect(nextCatalogShellID({})).toBe("claude-mythos-1");
  });

  it("已有 1 与 3 时取空缺的 2", () => {
    expect(
      nextCatalogShellID({ occupied: ["claude-mythos-1", "claude-mythos-3"] }),
    ).toBe("claude-mythos-2");
  });

  it("2 落在占位集合内时跳到 4", () => {
    expect(
      nextCatalogShellID({
        occupied: ["claude-mythos-1", "claude-mythos-3"],
        realModelIDs: ["claude-mythos-2"],
      }),
    ).toBe("claude-mythos-4");
  });

  it("白名单条目与真 Claude 默认模型名同样占位", () => {
    expect(
      nextCatalogShellID({
        realModelIDs: ["claude-mythos-1"],
        reservedModelIDs: ["claude-mythos-2"],
      }),
    ).toBe("claude-mythos-3");
  });

  it("生成与保存共用同一个占位集合函数", () => {
    const placeholders = catalogShellIDPlaceholders({
      occupied: ["claude-mythos-1"],
      realModelIDs: ["deepseek-v4"],
      reservedModelIDs: ["claude-opus-4-6"],
    });
    expect([...placeholders].sort()).toEqual([
      "claude-mythos-1",
      "claude-opus-4-6",
      "deepseek-v4",
    ]);
  });

  it("大小写不同的同一个壳 ID 视为已占用", () => {
    expect(nextCatalogShellID({ occupied: ["CLAUDE-MYTHOS-1"] })).toBe("claude-mythos-2");
  });

  it("appendCatalogRow 预填下一个可用壳 ID，显示名与真实模型留空", () => {
    const rows: GroupModelCatalogRow[] = [validRow({ shellID: "claude-mythos-1" })];
    const next = appendCatalogRow(rows, { draftDisplayNames: { "deepseek-v4": "旧名" } });
    expect(next).toHaveLength(2);
    expect(next[1]).toMatchObject({
      key: 2,
      shellID: "claude-mythos-2",
      displayName: "",
      realModel: "",
      supports1M: false,
    });
  });
});

describe("groupModelCatalog · 行校验", () => {
  it("整行合法时没有错误", () => {
    expect(catalogRowErrors([validRow()], {})).toEqual({});
    expect(catalogHasErrors([validRow()], {})).toBe(false);
  });

  it("三个必填字段为空时各报一次", () => {
    const rows = [validRow({ shellID: "", displayName: "", realModel: "" })];
    expect(catalogRowErrors(rows, {})[1]).toEqual([
      "shellRequired",
      "displayNameRequired",
      "realModelRequired",
    ]);
  });

  it("壳 ID 家族不在六家族内时判格式错", () => {
    expect(catalogRowErrors([validRow({ shellID: "foo-1" })], {})[1]).toEqual([
      "shellFormat",
    ]);
    expect(
      catalogRowErrors([validRow({ shellID: "claude-mythos-" })], {})[1],
    ).toEqual(["shellFormat"]);
  });

  it("六家族内的大小写壳 ID 通过格式校验", () => {
    expect(
      catalogRowErrors([validRow({ shellID: "CLAUDE-OPUS-2" })], {}),
    ).toEqual({});
  });

  it("两行壳 ID 重复时两行都被点名", () => {
    const rows = [
      validRow({ key: 1, shellID: "claude-mythos-3" }),
      validRow({ key: 2, shellID: "CLAUDE-MYTHOS-3", realModel: "kimi-k2.6" }),
    ];
    const errors = catalogRowErrors(rows, {});
    expect(errors[1]).toEqual(["shellDuplicate"]);
    expect(errors[2]).toEqual(["shellDuplicate"]);
  });

  it("与真 Claude 模型名重名 ⇒ shellReserved（判据是仓内 Claude 名清单，不靠平台候选）", () => {
    // 候选接口按平台过滤，gemini 平台的候选一条 Claude 名都没有；后端规则 3 不分平台。
    expect([...CATALOG_RESERVED_CLAUDE_MODEL_IDS]).toContain("claude-opus-4-6");
    expect(
      catalogRowErrors([validRow({ shellID: "claude-opus-4-6" })], {
        candidates: ["gemini-3.8-flash"],
      })[1],
    ).toEqual(["shellReserved"]);
  });

  it("只出现在平台候选里的 id 不算占位（不给后端会放行的值加罚款）", () => {
    // claude-mythos-ops 在候选里，但不在本组 models / 旧键 / 真 Claude 名里 ⇒
    // 后端规则 3 放行，前端也不得拦住。
    expect(
      catalogRowErrors([validRow({ shellID: "claude-mythos-ops" })], {
        candidates: ["claude-mythos-ops"],
      }),
    ).toEqual({});
  });

  it("与存量真实模型 ID 重名 ⇒ shellTakenByRealModel（白名单条目与旧键都算）", () => {
    expect(
      catalogRowErrors([validRow({ shellID: "claude-mythos-ops" })], {
        items: [{ id: "claude-mythos-ops", selected: true }],
      })[1],
    ).toEqual(["shellTakenByRealModel"]);
    expect(
      catalogRowErrors([validRow({ shellID: "claude-mythos-2" })], {
        draftDisplayNames: { "claude-mythos-2": "旧名" },
      })[1],
    ).toEqual(["shellTakenByRealModel"]);
  });

  it("白名单启用时真实模型必须命中条目，通配按后端规则匹配", () => {
    const source = {
      enabled: true,
      items: [{ id: "deepseek-*", selected: true }],
    };
    expect(catalogRowErrors([validRow()], source)).toEqual({});
    expect(
      catalogRowErrors([validRow({ realModel: "gpt-5.4" })], source)[1],
    ).toEqual(["realModelNotAllowed"]);
  });

  it("真实模型写成通配 ⇒ realModelWildcard，保存被阻断（与白名单开关无关）", () => {
    // 设计 §2.1 规则 8：通配字面是白名单的模式、不是模型名；存成 real_model 会在请求期
    // 被原样发给上游，后端保存期也会 400。前端两态都得拦住。
    const wildcardRow = validRow({ realModel: "deepseek-*" });
    for (const source of [{}, { enabled: true, items: [{ id: "deepseek-*", selected: true }] }]) {
      expect(catalogRowErrors([wildcardRow], source)[1]).toEqual(["realModelWildcard"]);
      expect(catalogHasErrors([wildcardRow], source)).toBe(true);
    }

    const session = createGroupModelCatalogSession({});
    session.load([
      { shell_id: "claude-mythos-7", display_name: "DeepSeek V4", real_model: "deepseek-*" },
    ]);
    // 会话 invalid ⇒ 保存按钮置灰（提交被阻断）。
    expect(session.invalid).toBe(true);
  });

  it("白名单关闭时不做成员校验", () => {
    expect(
      catalogRowErrors([validRow({ realModel: "absent-model" })], { enabled: false }),
    ).toEqual({});
  });

  it("catalogPatternMatches 锚定且大小写不敏感，* 可出现在任意位置", () => {
    expect(catalogPatternMatches("deepseek-*", "DeepSeek-V4")).toBe(true);
    expect(catalogPatternMatches("gpt-*-codex", "gpt-5.5-codex")).toBe(true);
    expect(catalogPatternMatches("gpt-*-codex", "gpt-5.5-codex-mini")).toBe(false);
    expect(catalogPatternMatches("deepseek", "deepseek-v4")).toBe(false);
    expect(catalogPatternMatches("", "deepseek-v4")).toBe(false);
  });

  it("updateCatalogRow / removeCatalogRow 只动目标行", () => {
    const rows = [validRow({ key: 1 }), validRow({ key: 2, shellID: "claude-mythos-8" })];
    const updated = updateCatalogRow(rows, 2, { displayName: "Kimi" });
    expect(updated[1].displayName).toBe("Kimi");
    expect(updated[0]).toBe(rows[0]);
    expect(removeCatalogRow(rows, 1).map(row => row.key)).toEqual([2]);
  });
});

describe("groupModelCatalog · 提交构造", () => {
  it("逐字段 trim、保序、整行空的条目不提交", () => {
    const rows: GroupModelCatalogRow[] = [
      validRow({ key: 1, shellID: " claude-mythos-7 ", displayName: " DeepSeek V4 " }),
      validRow({
        key: 2,
        shellID: "",
        displayName: "",
        realModel: "",
        supports1M: true,
      }),
      validRow({ key: 3, shellID: "claude-mythos-8", realModel: "kimi-k2.6", supports1M: true }),
    ];
    expect(buildCatalogEntries(rows)).toEqual([
      { shell_id: "claude-mythos-7", display_name: "DeepSeek V4", real_model: "deepseek-v4" },
      { shell_id: "claude-mythos-8", display_name: "DeepSeek V4", real_model: "kimi-k2.6", supports_1m: true },
    ]);
  });

  it("目录为空时 catalog 键不出现", () => {
    expect(catalogAllowlistPatch([])).toEqual({});
    expect(catalogAllowlistPatch([validRow({ shellID: "", displayName: "", realModel: "" })])).toEqual({});
  });

  it("目录非空时 catalog 带完整条目", () => {
    expect(catalogAllowlistPatch([validRow()])).toEqual({
      catalog: [
        { shell_id: "claude-mythos-7", display_name: "DeepSeek V4", real_model: "deepseek-v4" },
      ],
    });
  });

  it("catalogRowsFrom 把后端配置转成界面行", () => {
    expect(
      catalogRowsFrom([
        {
          shell_id: " claude-mythos-1 ",
          display_name: "甲",
          real_model: "deepseek-v4",
          supports_1m: true,
        },
      ]),
    ).toEqual([
      {
        key: 1,
        shellID: "claude-mythos-1",
        displayName: "甲",
        realModel: "deepseek-v4",
        supports1M: true,
      },
    ]);
    expect(catalogRowsFrom(undefined)).toEqual([]);
  });
});

describe("groupModelCatalog · 转为目录行（AC-12 前端半）", () => {
  it("只新增一行，旧显示名映射不被改动", () => {
    const rows: GroupModelCatalogRow[] = [];
    const source = {
      draftDisplayNames: { "deepseek-v4": "旧名" },
    };
    const next = convertDisplayNameToCatalogRow(rows, source, "deepseek-v4", "旧名");

    expect(next).toHaveLength(1);
    expect(next[0]).toMatchObject({
      shellID: "claude-mythos-1",
      displayName: "旧名",
      realModel: "deepseek-v4",
      supports1M: false,
    });
    // 转换不删旧键：非 CC 客户端只吃旧机制，删除是管理员的独立一步。
    expect(source.draftDisplayNames).toEqual({ "deepseek-v4": "旧名" });
    expect(rows).toEqual([]);
  });

  it("已占用的壳 ID 不重复发放", () => {
    const rows: GroupModelCatalogRow[] = [validRow({ shellID: "claude-mythos-1" })];
    const next = convertDisplayNameToCatalogRow(rows, {}, "kimi-k2.6", "Kimi");
    expect(next[1].shellID).toBe("claude-mythos-2");
  });
});

describe("groupModelCatalog · 真实模型候选", () => {
  it("白名单启用时只给提交态条目", () => {
    expect(
      catalogRealModelOptions({
        enabled: true,
        items: [
          { id: "deepseek-v4", selected: true },
          { id: "gpt-5.4", selected: false },
        ],
      }),
    ).toEqual(["deepseek-v4"]);
  });

  it("白名单关闭时给平台候选并去重", () => {
    expect(
      catalogRealModelOptions({ enabled: false, candidates: ["gpt-5.4", "GPT-5.4", "gpt-5.5"] }),
    ).toEqual(["gpt-5.4", "gpt-5.5"]);
  });

  it("白名单通配条目只当校验规则，不进真实模型候选", () => {
    expect(
      catalogRealModelOptions({
        enabled: true,
        items: [
          { id: "deepseek-*", selected: true },
          { id: "deepseek-v4", selected: true },
        ],
      }),
    ).toEqual(["deepseek-v4"]);
  });
});

describe("groupModelCatalog · 抽屉会话", () => {
  it("载入分组目录并按行集导出提交补丁", () => {
    const session = createGroupModelCatalogSession({});
    expect(session.rows).toEqual([]);
    expect(session.invalid).toBe(false);

    session.load([
      {
        shell_id: "claude-mythos-7",
        display_name: "DeepSeek V4",
        real_model: "deepseek-v4",
        supports_1m: true,
      },
    ]);
    expect(session.rows).toHaveLength(1);
    expect(session.invalid).toBe(false);
    expect(session.allowlistPatch()).toEqual({
      catalog: [
        {
          shell_id: "claude-mythos-7",
          display_name: "DeepSeek V4",
          real_model: "deepseek-v4",
          supports_1m: true,
        },
      ],
    });

    session.load();
    expect(session.rows).toEqual([]);
    expect(session.allowlistPatch()).toEqual({});
  });

  it("候选只当提示，不参与壳 ID 占位；转换保留旧键", () => {
    const legacy = { draftDisplayNames: { "deepseek-v4": "旧名" } };
    const session = createGroupModelCatalogSession(legacy);

    session.setCandidates(["gpt-5.4"]);
    expect(session.source.candidates).toEqual(["gpt-5.4"]);

    session.convert("deepseek-v4", "旧名");
    expect(session.rows[0]).toMatchObject({
      shellID: "claude-mythos-1",
      displayName: "旧名",
      realModel: "deepseek-v4",
    });
    expect(legacy.draftDisplayNames).toEqual({ "deepseek-v4": "旧名" });
    expect(session.invalid).toBe(false);
  });

  it("行不完整时会话报 invalid（保存钮据此置灰）", () => {
    const session = createGroupModelCatalogSession({});
    session.convert("deepseek-v4", "");
    expect(session.invalid).toBe(true);
  });
});