# RTM — 集成用例索引（Requirements Traceability Matrix）

- **用途**：一条业务故事一节；一节内每行 = 一条集成用例。行号先在此固定，用例文件与标题随后按行号命名。
- **判定范围**：只登记**面向业务可观察结果**的集成用例（跨组件的交互结果），单元层断言不入此表。
- **当前状态**：本仓尚无集成测试套件（`test/` 目录此前不存在，本文件为首个基线）。套件建设作为欠账登记在项目台账（缺集成基线与 RTM）。本表先行固定行号与用例设计；**「用例设计是否完整」与「套件何时可执行」分列记录**，后者不替代前者。

## 计数

| 节 | 用例行数 |
|---|---|
| 成本账（账号统计成本） | 2 |
| 图片按张计费（分组开关） | 1 |
| **合计** | **3 / 50–100（窗口下限远未达到；套件可执行性仍欠账）** |

---

## 1. 成本账按时段计价（账号统计成本）

**业务故事**：管理员为渠道配置「每时段一个价」后，同一模型在不同时段发生的调用，其**账号统计成本**（`usage_logs.account_stats_cost`）按该时段自己的价格记账；后台用量页与用量浮窗显示的成本随之变化，而用户扣费（`actual_cost`）与售价（`total_cost`）不变。

### 时序基准（全节共用，与设计 §2.4 同源）

```
请求进入 → applyAccountStatsCost 内计算成本（计时点即请求发生时刻）
         → 用量落库，usage_logs 一并写入 account_stats_cost / total_cost / actual_cost
         → 展示与聚合读取已落库的值（UsageTable / /api/v1/admin/usage/stats）
```

成本在**写入之前**算好、随用量行一起落库；展示与统计只读已落库值，不做二次计算。

### 1.1 用例行

| 行号 | 业务入口 | 组件交互 | 业务结果 | 需求追溯 | 验收证据（通过判据） |
|---|---|---|---|---|---|
| **RTM-COST-01** | 后台「渠道 → 账号统计定价」配置两条不同价的时段；客户端在高峰与空闲各发一次真实调用；管理员在用量页与浮窗查看 | 渠道配置（`channels` / `channel_account_stats_pricing_rules` / 时段子表）→ 请求进入后 `resolveAccountStatsCost` / `calculateStatsCostAt` 选段算成本 → 用量行连同三列成本落库 → 后台 `UsageTable` 与统计接口读取 | 两次调用各自 `account_stats_cost` = **本方段价 × 本次实际 tokens**（误差 < 1e-9） | F1 / F2 / F4；AC-1 / AC-2 / AC-5 | ① 两次调用的 `usage_logs` 行：`account_stats_cost` 按各自 tokens 反算等于段价 × tokens（不依赖两次用量相等）；② 同两行的 `total_cost` / `actual_cost` 与「**相同售价与扣费配置、相同实际用量、且未启用成本时段价**」的对照结果相等（基准是“未启用时段价”，不是笼统的“目录价”）；③ 用量页显示值 = `(account_stats_cost ?? total_cost) × account_rate_multiplier`；④ 统计接口 `total_account_cost` 增量 = 两次 `account_stats_cost` 之和 |
| **RTM-COST-02** | 后台建**第二个渠道**（时段划分与价格均与第一个不同），把另一账号绑到它；两个渠道各自发一次真实调用（同一模型、同一时刻亦可） | 两套渠道配置并存 → 各请求按自身分组解析到所属渠道 → 各自命中自己渠道的规则与时段 → 各自落库 | 两个渠道的调用**互不串价**：每笔成本等于其所属渠道该时刻段价 × 本次 tokens | F1；AC-3 | ① 两笔 `usage_logs` 行的 `account_stats_cost` 分别等于各自渠道段价 × tokens；② 改动渠道 A 的价格后，渠道 B 的**新调用**按「B 自身未变的配置 × 请求时刻 × 实际用量」算出的预期成本不变（**不是**比新旧成本数值大小）；③ 渠道 A 的**历史行**不被回写——与②作为**两个独立断言**；④ 两渠道的定价行 id 与渠道 id 对应关系可从 `channel_account_stats_pricing_rules` 查得 |

### 未覆盖（缺口，随套件建设补入）

- AC-4（未命中时段兜底）与 AC-7（时段边界时刻）：由单元用例覆盖（设计 §6 CASE-06 / CASE-07），按本表判定范围不入集成行。
- 套件可执行性：本仓尚无集成运行入口；上两行的用例设计已完整，**实现载体待套件建设**（台账欠账）。

---

## 2. 图片按张计费（分组开关）

**业务故事**：管理员在分组编辑页打开「按张计费」后，专用图片端点的出图调用按**上游实际返回的张数**计费；关闭开关后同一调用回到 token 计价。

### 2.1 用例行

| 行号 | 业务入口 | 组件交互 | 业务结果 | 需求追溯 | 验收证据（通过判据） |
|---|---|---|---|---|---|
| **RTM-IMG-01** | 管理员在分组编辑页打开「按张计费」（composite/openai 分组且已开图片生成），客户端经专用图片端点生成 2 张图；随后关闭开关，再发一次同参数请求 | 分组配置（`groups.image_count_billing`）→ 网关图片端点判定（`imageCountBillingApplies`）→ `calculateImageCost` 取价（`CalculateImageCost`：单价先 `ToCNYAmount` 再 × 张数 × 倍率）→ 用量行落库（`image_count` / `billing_mode` / `total_cost` / `actual_cost`）→ 后台用量页读取 | 开启：`billing_mode='image'`、`image_count=2`、`total_cost` = 2 × 该分组该尺寸单价（人民币口径）、`actual_cost` = `total_cost` × `rate_multiplier`。关闭：`billing_mode='token'`、成本由 token 分项构成、`image_count` 仍记录上游报的 2（不计按张） | F3 / AC-4 / AC-7 | ① 两笔 `usage_logs` 行逐字段比对（同一模型、尺寸档、倍率、币种；单价取 `channel_model_pricing` / 分组图片价，不得以「默认价猜测」代替）；② 开启行 `total_cost ÷ image_count` = 配置单价经汇率换算后的值（误差 < 1e-9）；③ 关闭行 `billing_mode='token'` 且 `total_cost` = `input_cost + output_cost + cache_creation_cost + cache_read_cost`；④ 不得以「关闭后 `image_count` 为 0」作为判据——该列记录上游报的张数与计费模式无关 |

### 未覆盖（缺口）

- 「混合请求（图 + 文本）走开张计费分组不按张」由单元用例覆盖（`image_count_billing_switch_test.go`），按本表判定范围不入集成行。
- 套件可执行性：同 §1 末尾，实现载体待套件建设。