import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

import { describe, expect, it } from "vitest";

const currentDir = dirname(fileURLToPath(import.meta.url));
const groupsViewSource = readFileSync(
  resolve(currentDir, "../GroupsView.vue"),
  "utf8",
);

describe("groups model allowlist layout", () => {
  it("keeps the toolbar outside of the scrolling list content", () => {
    expect(groupsViewSource).toContain("overflow-hidden rounded-lg border");
    expect(groupsViewSource).toContain("max-h-64 space-y-2 overflow-y-auto p-2");
    expect(groupsViewSource).not.toContain("sticky top-0");
  });

  it("uses a wide dialog and keeps model pricing controls responsive", () => {
    expect(groupsViewSource).toContain('width="wide"');
    expect(groupsViewSource).toContain(
      "btn btn-secondary shrink-0 whitespace-nowrap",
    );
  });
});

// 显示名表与白名单开关无关的模板级检查：两处抽屉 × 开关两态 × 空表。
// 函数行集相等断言查不出「模板仍条件隐藏」或「勾选行残留逐行输入框」，这组断言覆盖那两处。
describe("groups display name table layout", () => {
  const drawers = [
    {
      state: "createModelAllowlistState",
      rows: "createDisplayNameRows",
      entryID: "createDisplayNameEntryID",
      entryName: "createDisplayNameEntryName",
      inputHandler: "handleCreateDisplayNameInput",
    },
    {
      state: "editModelAllowlistState",
      rows: "editDisplayNameRows",
      entryID: "editDisplayNameEntryID",
      entryName: "editDisplayNameEntryName",
      inputHandler: "handleEditDisplayNameInput",
    },
  ];

  it("keeps the display name table outside the whitelist switch branch in both drawers", () => {
    for (const drawer of drawers) {
      const branchStart = groupsViewSource.indexOf(`<template v-if="${drawer.state}.enabled">`);
      expect(branchStart).toBeGreaterThan(-1);
      const tableTitle = groupsViewSource.indexOf(
        't("admin.groups.modelAllowlist.displayNameTableTitle")',
        branchStart,
      );
      expect(tableTitle).toBeGreaterThan(branchStart);

      // 抽屉这一段 = v-if 开口到表标题之间（勾选半区 + 分支收口）。
      // 断言只落在这段内，不整档禁止某个形态：别的区块将来加 <template v-else> 不该把这里判红。
      const drawerRegion = groupsViewSource
        .slice(branchStart, tableTitle)
        .replace(/<!--[\s\S]*?-->/g, "");

      // 看“表标题处的 template 嵌套深度”而不是数收口个数：
      // 个数是文本启发式——勾选半区将来嵌一层 <template> 且表被挪回分支内时，
      // 区间内仍只有 1 个收口，个数法会假通过；嵌套配对后深度仍 > 0。
      const depth = (drawerRegion.match(/<\/?template\b/g) ?? []).reduce(
        (acc, tag) => acc + (tag === "</template" ? -1 : 1),
        0,
      );
      // 深度 0 ⇒ v-if 分支已在表标题之前收口，开关两种状态下都渲染同一张表。
      expect(depth).toBe(0);

      // 旧的 v-else 半区（表包在 else 里）与勾选行逐行显示名输入框都不该在这段内。
      expect(drawerRegion).not.toContain("template v-else");
      expect(drawerRegion).not.toContain(`${drawer.inputHandler}(item.id`);
    }
  });

  it("has no displayNameInputVisible consumer left in the view", () => {
    // 验收条目 5 判的是“仓内无定义、无引用”，所以这里是整档检查，不是单抽屉检查。
    expect(groupsViewSource).not.toContain("displayNameInputVisible");
  });

  it("keeps one row list, the empty hint and the add row in both drawers", () => {
    const count = (needle: string) => groupsViewSource.split(needle).length - 1;
    expect(count(`v-for="row in ${drawers[0].rows}"`)).toBe(1);
    expect(count(`v-for="row in ${drawers[1].rows}"`)).toBe(1);
    expect(count('t("admin.groups.modelAllowlist.displayNameEmpty")')).toBe(2);
    expect(count('t("admin.groups.modelAllowlist.addDisplayName")')).toBe(2);
  });

  it("greys out the add button until both the model id and the name are filled", () => {
    for (const drawer of drawers) {
      expect(groupsViewSource).toContain(
        `:disabled="!${drawer.entryID}.trim() || !${drawer.entryName}.trim()"`,
      );
    }
  });
});

// 目录表与旧显示名表的落位：目录表在上、不被白名单开关包裹（它就是 CC 客户端
// 看到的那张清单），旧显示名表在下并点明「非 Claude Code 客户端使用」，两处抽屉同形。
describe("groups model catalog table layout", () => {
  const catalogDrawers = [
    {
      state: "createModelAllowlistState",
      rows: "createCatalog.rows",
      convert: "createCatalog",
    },
    {
      state: "editModelAllowlistState",
      rows: "editCatalog.rows",
      convert: "editCatalog",
    },
  ];
  const count = (needle: string) => groupsViewSource.split(needle).length - 1;
  // 标签内的属性顺序 / 换行都属于排版，不是约束；只认「同一个标签里有这条绑定」。
  const catalogTagIndex = (rowsBinding: string, from = 0) => {
    const pattern = new RegExp(
      `<GroupModelCatalogTable\\b[^>]*v-model:rows="${rowsBinding}"`,
      "g",
    );
    pattern.lastIndex = from;
    const match = pattern.exec(groupsViewSource);
    return match ? match.index : -1;
  };

  it("registers one catalog table per drawer with its own row set", () => {
    for (const drawer of catalogDrawers) {
      expect(catalogTagIndex(drawer.rows)).toBeGreaterThan(-1);
    }
    expect(count("<GroupModelCatalogTable")).toBe(2);
  });

  it("keeps the catalog table outside the whitelist switch branch and above the legacy table", () => {
    for (const drawer of catalogDrawers) {
      const branchStart = groupsViewSource.indexOf(
        `<template v-if="${drawer.state}.enabled">`,
      );
      expect(branchStart).toBeGreaterThan(-1);
      const catalogStart = catalogTagIndex(drawer.rows, branchStart);
      expect(catalogStart).toBeGreaterThan(branchStart);
      // 区间内 template 深度回到 0 ⇒ 目录表在开关分支之外，开关两种状态下都渲染。
      const depth = (
        groupsViewSource
          .slice(branchStart, catalogStart)
          .replace(/<!--[\s\S]*?-->/g, "")
          .match(/<\/?template\b/g) ?? []
      ).reduce((acc, tag) => acc + (tag === "</template" ? -1 : 1), 0);
      expect(depth).toBe(0);
      const legacyTitle = groupsViewSource.indexOf(
        't("admin.groups.modelAllowlist.displayNameTableTitle")',
        branchStart,
      );
      expect(legacyTitle).toBeGreaterThan(catalogStart);
    }
  });

  it("names the legacy section as the non-Claude-Code surface and keeps its entries", () => {
    expect(count('t("admin.groups.modelAllowlist.displayNameSectionHint")')).toBe(2);
    expect(count('t("admin.groups.modelAllowlist.addDisplayName")')).toBe(2);
    expect(count('t("admin.groups.modelAllowlist.displayNameEmpty")')).toBe(2);
  });

  it("offers one convert-to-catalog action per legacy row in both drawers", () => {
    for (const drawer of catalogDrawers) {
      expect(count(`${drawer.convert}.convert(row.id, row.displayName)`)).toBe(1);
    }
  });
});
