package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

// GroupModelCatalogEntry 是分组模型目录的一行：客户端看到的壳 id → 真实模型。
//
// 目录只负责「客户端看到哪些壳 id、叫什么」，白名单负责「允许调哪些真实模型」；
// 两者同域存储，随分组一次读写（KDR-1）。存储里永远只有壳 id 原文，
// `[1m]` 是发布期追加、不落库（KDR-8）。
type GroupModelCatalogEntry struct {
	ShellID     string `json:"shell_id"`
	DisplayName string `json:"display_name"`
	RealModel   string `json:"real_model"`
	Supports1M  bool   `json:"supports_1m,omitempty"`
}

// catalogShellIDPattern 是壳 id 的格式约束：Claude Code Desktop 只认
// sonnet / opus / haiku / fable / mythos 六个家族（UR unified-router.mjs:645-647），
// 家族外的 id 客户端根本不显示，所以不合格式即 400 而不是静默丢弃。
var catalogShellIDPattern = regexp.MustCompile(`^claude-(sonnet|opus|haiku|fable|mythos)-[a-z0-9][a-z0-9._-]*$`)

// catalogShellIDFamily 是自动发号使用的家族：mythos 在真 Claude 里空闲、专给
// 非 Claude 厂商（UR unified-router.mjs:647），避开与真 Claude 混淆。
const catalogShellIDFamily = "claude-mythos-"

// catalogDiscoveryCreatedAt 与 writeModelsList 的既有条目保持一致，
// 目录行不引入新的时间语义。
const catalogDiscoveryCreatedAt = "2024-01-01T00:00:00Z"

// catalogEntryErrorMessage 组装一条可指认的 400：点名行下标与被拒的值。
// 静默丢行等于替管理员删配置，所以缺字段与非法值一律报错。
func catalogEntryErrorMessage(index int, format string, args ...any) string {
	return fmt.Sprintf("model catalog row %d: %s", index, fmt.Sprintf(format, args...))
}

// catalogShellIDPlaceholderSet 返回目录壳 id 不得占用的 id 集合：
// 真 Claude 名（claude.DefaultModelIDs）∪ 本组白名单条目 ∪ 本组旧 display_names 的键，
// 比较前两边都 trim、转小写（不做通配展开）。
//
// 两个理由：与真 Claude 名重名会被 Claude Code 强制重命名（UR unified-router.mjs:566）；
// 与本组存量真实模型 id 重名会让入站反查把今天指向该真实模型的请求静默改道（KDR-9）。
// 生成（NextCatalogShellID）与保存校验（normalizeGroupModelCatalog 规则 3）共用本函数，
// 不存在「生成避开、手改放行」两套占位规则。
func catalogShellIDPlaceholderSet(cfg GroupModelAllowlist) map[string]struct{} {
	reserved := make(map[string]struct{}, len(cfg.Models)+len(cfg.DisplayNames)+len(claude.DefaultModels))
	for _, modelID := range claude.DefaultModelIDs() {
		reserved[strings.ToLower(strings.TrimSpace(modelID))] = struct{}{}
	}
	for _, entry := range cfg.Models {
		reserved[strings.ToLower(strings.TrimSpace(entry))] = struct{}{}
	}
	for key := range cfg.DisplayNames {
		reserved[strings.ToLower(strings.TrimSpace(key))] = struct{}{}
	}
	return reserved
}

// NextCatalogShellID 返回下一个可用壳 id：claude-mythos-<n>，n 为最小可用正整数。
//
// 数字后缀而非 UR 的 route-<n>：NormalizeInboundModelID 对 claude-mythos-<词> 会剥前缀，
// 但数字开头后缀判为原生 mythos 名、不剥（model_context_window.go:220-238）——
// 反查漏了就会响亮失败成未知模型，而不是被剥成别的名字继续跑（KDR-2）。
func NextCatalogShellID(cfg GroupModelAllowlist) string {
	reserved := catalogShellIDPlaceholderSet(cfg)
	for _, entry := range cfg.Catalog {
		reserved[strings.ToLower(strings.TrimSpace(entry.ShellID))] = struct{}{}
	}
	for n := 1; ; n++ {
		candidate := catalogShellIDFamily + strconv.Itoa(n)
		if _, taken := reserved[candidate]; !taken {
			return candidate
		}
	}
}

// normalizeGroupModelCatalog 归一化管理端提交的目录（设计 §2.1 规则 1-6）：
// 三个字符串字段 trim；壳 id 转小写并按六家族格式校验；壳 id 不得落在占位集合内；
// 组内壳 id 不得重复；白名单启用时真实模型必须命中白名单条目。条目顺序与 1M 开关原样保留。
func normalizeGroupModelCatalog(cfg GroupModelAllowlist, catalog []GroupModelCatalogEntry) ([]GroupModelCatalogEntry, error) {
	if len(catalog) == 0 {
		return nil, nil
	}
	out := make([]GroupModelCatalogEntry, 0, len(catalog))
	placeholders := catalogShellIDPlaceholderSet(cfg)
	seen := make(map[string]struct{}, len(catalog))
	for i, entry := range catalog {
		shellID := strings.ToLower(strings.TrimSpace(entry.ShellID))
		displayName := strings.TrimSpace(entry.DisplayName)
		realModel := strings.TrimSpace(entry.RealModel)
		switch {
		case shellID == "":
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_CATALOG", catalogEntryErrorMessage(i, "shell_id is required"))
		case displayName == "":
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_CATALOG", catalogEntryErrorMessage(i, "display_name is required"))
		case realModel == "":
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_CATALOG", catalogEntryErrorMessage(i, "real_model is required"))
		case strings.Contains(realModel, "*"):
			// 白名单里的 `deepseek-*` 是匹配模式，不是模型名：存下来反查后会把 `deepseek-*`
			// 原样发给上游，失败点在请求期、不在保存期，管理员看不到原因。
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_CATALOG", catalogEntryErrorMessage(i, "real_model %q must be a concrete model id, not an allowlist pattern", realModel))
		}
		if !catalogShellIDPattern.MatchString(shellID) {
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_CATALOG", catalogEntryErrorMessage(i, "shell id %q must match claude-(sonnet|opus|haiku|fable|mythos)-<name>", shellID))
		}
		if _, taken := placeholders[shellID]; taken {
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_CATALOG", catalogEntryErrorMessage(i, "shell id %q is already used by a model id of this group", shellID))
		}
		if _, duplicate := seen[shellID]; duplicate {
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_CATALOG", catalogEntryErrorMessage(i, "shell id %q is duplicated in this catalog", shellID))
		}
		seen[shellID] = struct{}{}
		if cfg.Enabled && !catalogRealModelAllowed(cfg.Models, realModel) {
			return nil, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_CATALOG", catalogEntryErrorMessage(i, "real model %q is not allowed by the model allowlist", realModel))
		}
		out = append(out, GroupModelCatalogEntry{
			ShellID:     shellID,
			DisplayName: displayName,
			RealModel:   realModel,
			Supports1M:  entry.Supports1M,
		})
	}
	return out, nil
}

// catalogRealModelAllowed 报告真实模型是否命中白名单条目（沿用 groupAllowlistPatternMatches：
// `*` 通配、锚定、大小写不敏感）。白名单关闭时由调用方跳过本校验。
func catalogRealModelAllowed(models []string, realModel string) bool {
	candidates := groupModelAllowlistCandidates(realModel)
	for _, entry := range models {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		for _, candidate := range candidates {
			if groupAllowlistPatternMatches(entry, candidate) {
				return true
			}
		}
	}
	return false
}

// catalogLookupKey 把入站模型名折算成目录查找键：trim → 反复剥尾部 `[1m]`
// （大小写不敏感）→ 小写。剥到后缀时报告 1M 意图，供调用方决定改写值。
func catalogLookupKey(modelID string) (key string, requested1M bool) {
	trimmed, stripped := stripTrailing1MSuffixes(modelID)
	if trimmed == "" {
		return "", false
	}
	return strings.ToLower(trimmed), stripped
}

// CatalogRealModelFor 反查入站模型名命中的目录行（设计 §2.4 步骤 1-2）。
//
// 查找与命中判定只有这一处，请求体与路径参数两条来源共用：
// 查找键 = trim → 反复剥尾部 `[1m]`（大小写不敏感）→ 小写；
// 命中条件 = 目录里存在该 shell_id（存储即小写），且「请求未带 [1m]」或「该行 supports_1m=true」。
// 返回值是裸真实模型 + 「请求带了 1M」这一事实，**不含改写值**——改写值由两条来源各自规定
// （请求体拼回后缀复用既有 Context1MRequested 机制，路径参数恒为裸真实模型，KDR-8）。
//
// 未命中（含「带 [1m] 但该行未授予 1M」）返回 ok=false：入站该字段保持原文，
// 准入按原文判、失败时点名原文，不会静默换成任何真实模型。
func (a GroupModelAllowlist) CatalogRealModelFor(modelID string) (realModel string, requested1M bool, ok bool) {
	if len(a.Catalog) == 0 {
		return "", false, false
	}
	key, requested1M := catalogLookupKey(modelID)
	if key == "" {
		return "", false, false
	}
	for _, entry := range a.Catalog {
		if strings.ToLower(strings.TrimSpace(entry.ShellID)) != key {
			continue
		}
		realModel = strings.TrimSpace(entry.RealModel)
		if realModel == "" {
			continue
		}
		if requested1M && !entry.Supports1M {
			continue
		}
		return realModel, requested1M, true
	}
	return "", false, false
}

// CatalogPathParamRealModel 反查路径参数（`GET /v1/models/:model` 与其根别名）命中的裸真实模型，
// 供准入判定使用。
//
// 与请求体分支共用同一查找键与命中判定（CatalogRealModelFor：剥 `[1m]`、按行授予判定），
// 但路径参数**不改写**：详情入口要的是列表里存在的那个 id（CC 形状列表发布的是壳 id，
// 详情 handler 按它匹配并原样返回），把参数换成真实模型只会让详情 404
// （设计 §2.4 路径参数分支）。白名单只认真实模型，所以准入按本函数的返回值判。
func (a GroupModelAllowlist) CatalogPathParamRealModel(modelID string) (string, bool) {
	realModel, _, ok := a.CatalogRealModelFor(modelID)
	if !ok {
		return "", false
	}
	return realModel, true
}

// catalogInboundRouteEligible 报告该入口是否参与目录反查：只有 Claude 形状
// （/messages 家族）参与。OpenAI chat、Gemini 原生与 Grok 入口维持现状（设计 §2.4 末段）。
func catalogInboundRouteEligible(routePath string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(routePath)), "/messages")
}

// catalogBodyModelCandidate 是请求体里一个可被下游解析器绑定、且可以原样改写的模型字段：
// 字段当前值 + 它在原请求体里的字节位置（gjson `Result.Index` / `Result.Raw`，绝对偏移）。
//
// 用位置而不是「键路径」定位改写点：同一键名可以重复出现，sjson 之类按路径写入只改首个出现
// （实测 `{"model":"a","model":"b"}` 改写的是 `a`），命中后一个时会把壳 id 留在体内、同时
// 改掉前面那个未命中的值——正好是「命中与改写落点必须同源」要禁止的改道（设计 §2.4 :151）。
type catalogBodyModelCandidate struct {
	value string
	index int
	raw   string
}

// catalogBodyModelCandidates 按 requestmodel.FromBodyCandidates 的同一条候选顺序，
// 返回请求体（JSON）里的模型字段及其出现位置：顶层 `model`（含大小写变体、重复键，按出现顺序）
// 优先；顶层没有值时才回落各 `session` 对象的 `model`（设计 §2.4 步骤 1）。
//
// 值与位置同源交付：反查用这里的值，改写写回这里的位置。
func catalogBodyModelCandidates(body []byte) []catalogBodyModelCandidate {
	var topLevel, session []catalogBodyModelCandidate
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		if value.Type == gjson.String && strings.EqualFold(key.String(), "model") {
			if trimmed := strings.TrimSpace(value.String()); trimmed != "" {
				topLevel = append(topLevel, catalogBodyModelCandidate{value: trimmed, index: value.Index, raw: value.Raw})
			}
			return true
		}
		if !strings.EqualFold(key.String(), "session") {
			return true
		}
		value.ForEach(func(sessionKey, sessionValue gjson.Result) bool {
			if sessionValue.Type == gjson.String && strings.EqualFold(sessionKey.String(), "model") {
				if trimmed := strings.TrimSpace(sessionValue.String()); trimmed != "" {
					session = append(session, catalogBodyModelCandidate{
						value: trimmed,
						index: sessionValue.Index,
						raw:   sessionValue.Raw,
					})
				}
			}
			return true
		})
		return true
	})
	if len(topLevel) > 0 {
		return topLevel
	}
	return session
}

// catalogRewriteJSONString 把原请求体 offset 处那一段原始 JSON（raw）换成 value 的 JSON 字符串字面量。
// offset / raw 来自 gjson 对同一份 body 的解析，改动的字节位置因此就是被反查的那个字段本身；
// 位置对不上（body 已被改动）即放弃改写，让请求按原文走既有的响亮准入。
func catalogRewriteJSONString(body []byte, offset int, raw, value string) ([]byte, bool) {
	if offset < 0 || offset+len(raw) > len(body) || string(body[offset:offset+len(raw)]) != raw {
		return nil, false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	out := make([]byte, 0, len(body)+len(encoded))
	out = append(out, body[:offset]...)
	out = append(out, encoded...)
	out = append(out, body[offset+len(raw):]...)
	return out, true
}

// RewriteCatalogModelInBody 把请求体里命中目录的壳 id 改写成真实模型，返回改写后的请求体。
//
// 改写落点 = 命中的那个字段本身：顶层 `model` 命中就写顶层，只有 `session.model` 命中就写
// `session.model`；多个候选同时命中不同壳 id 时以候选顺序的第一个为准，其余字段不动
// （设计 §2.4 步骤 2-3 / AC-18）。请求带 1M 时把后缀拼回改写值，1M 意图继续交给既有
// NormalizeInboundModelID / Context1MRequested 机制，不另起字段（KDR-8）。
//
// 未命中、非 /messages 入口、multipart 与非法 JSON 一律原样返回 false——
// 这些情况下请求按原文走准入，失败是响亮的（404 点名原文），不会被静默换成别的模型。
func RewriteCatalogModelInBody(cfg GroupModelAllowlist, routePath, contentType string, body []byte) ([]byte, bool) {
	if len(cfg.Catalog) == 0 || len(body) == 0 {
		return body, false
	}
	if !catalogInboundRouteEligible(routePath) || catalogMultipartBody(contentType) || !gjson.ValidBytes(body) {
		return body, false
	}
	for _, candidate := range catalogBodyModelCandidates(body) {
		realModel, requested1M, hit := cfg.CatalogRealModelFor(candidate.value)
		if !hit {
			continue
		}
		rewriteValue := realModel
		if requested1M {
			rewriteValue += ContextWindow1MSuffix
		}
		rewritten, ok := catalogRewriteJSONString(body, candidate.index, candidate.raw, rewriteValue)
		if !ok {
			return body, false
		}
		return rewritten, true
	}
	return body, false
}

// CatalogMultipartShellIDHit 报告 multipart 请求体里是否出现目录壳 id，返回命中的那个候选原文。
//
// multipart 的模型值在表单分段里，改写需重新编码整个表单体，本设计不重编码；
// 而「不改写」本身不构成拒绝——白名单未启用时 Allows 恒为 true，壳 id 会原样送到上游必然失败。
// 所以命中即由准入层显式 400（设计 §2.4 步骤 6 / §7）：业务结果不变（该请求本就不该成功），
// 失败点从上游前移到网关，文案点名壳 id。
func CatalogMultipartShellIDHit(cfg GroupModelAllowlist, routePath, contentType string, body []byte) (string, bool) {
	if len(cfg.Catalog) == 0 || len(body) == 0 {
		return "", false
	}
	if !catalogMultipartBody(contentType) || !catalogInboundRouteEligible(routePath) {
		return "", false
	}
	for _, candidate := range requestmodel.FromBodyCandidates(routePath, contentType, body) {
		if _, _, hit := cfg.CatalogRealModelFor(candidate); hit {
			return candidate, true
		}
	}
	return "", false
}

// catalogMultipartBody 报告请求体声明为 multipart/form-data（目录改写只覆盖 JSON 请求体）。
func catalogMultipartBody(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "multipart/form-data")
}

// FormatCatalogEntryForDiscovery 把一条目录行格式化成发布值（设计 §2.3）：
// id = shell_id；display_name 照配；supports_1m=true 时 id 追加 `[1m]`、显示名追加 ` 1M`
// （已有 ` 1M` 不重复）、context_window = 1000000；否则 context_window 省略。
//
// `[1m]` 只在发布期追加，catalog 里永远只有壳 id 原文（KDR-5 / KDR-8）。
func FormatCatalogEntryForDiscovery(entry GroupModelCatalogEntry) (id, displayName string, contextWindow int) {
	id = strings.ToLower(strings.TrimSpace(entry.ShellID))
	displayName = strings.TrimSpace(entry.DisplayName)
	if displayName == "" {
		displayName = id
	}
	if !entry.Supports1M {
		return id, displayName, 0
	}
	if !has1MDisplayLabel(displayName) {
		displayName += " 1M"
	}
	return id + ContextWindow1MSuffix, displayName, ContextWindow1MTokens
}

// catalogRealModelEquivalent 报告两个模型名是否等价：沿用准入侧既有的模型名等价规则
// （大小写、`models/` 前缀、-thinking 宽容、OpenAI 推理后缀、壳 id 剥离），
// 保证「列表里看得到」与「准入放得过」用同一套比较。
func catalogRealModelEquivalent(left, right string) bool {
	leftCandidates := groupModelAllowlistCandidates(left)
	for _, candidate := range groupModelAllowlistCandidates(right) {
		for _, existing := range leftCandidates {
			if existing == candidate {
				return true
			}
		}
	}
	return false
}

// catalogEntryPublishable 报告该目录行是否进入发布集合：
// 真实模型必须属于可见集合（看到什么 = 能调什么），且白名单放行该真实模型。
// 白名单关闭时 Allows 恒为 true，目录照常发布（目录是客户端可见面）。
func catalogEntryPublishable(cfg GroupModelAllowlist, entry GroupModelCatalogEntry, visibleModels []string) bool {
	realModel := strings.TrimSpace(entry.RealModel)
	if realModel == "" || !cfg.Allows(realModel) {
		return false
	}
	for _, model := range visibleModels {
		if catalogRealModelEquivalent(realModel, model) {
			return true
		}
	}
	return false
}

// CatalogDiscoveryEntries 返回 Claude 形状出口要追加的目录行（已做发布期 1M 装饰、
// 按目录顺序），以及这批行覆盖到的可见模型（键 = visibleModels 里的原值）。
// 调用方先输出目录行，再跳过 covered 里的模型走既有自动包装逻辑。
//
// 非 Claude Code 客户端只吃旧机制：目录不发布，两个返回值都为 nil（设计 §2.3）。
func CatalogDiscoveryEntries(
	cfg GroupModelAllowlist,
	visibleModels []string,
	isCCClient bool,
) ([]claude.Model, map[string]struct{}) {
	if !isCCClient || len(cfg.Catalog) == 0 || len(visibleModels) == 0 {
		return nil, nil
	}
	entries := make([]claude.Model, 0, len(cfg.Catalog))
	covered := make(map[string]struct{}, len(cfg.Catalog))
	for _, entry := range cfg.Catalog {
		if !catalogEntryPublishable(cfg, entry, visibleModels) {
			continue
		}
		id, displayName, contextWindow := FormatCatalogEntryForDiscovery(entry)
		entries = append(entries, claude.Model{
			ID:            id,
			Type:          "model",
			DisplayName:   displayName,
			CreatedAt:     catalogDiscoveryCreatedAt,
			ContextWindow: contextWindow,
		})
		for _, model := range visibleModels {
			if catalogRealModelEquivalent(entry.RealModel, model) {
				covered[model] = struct{}{}
			}
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return entries, covered
}

// DefaultDiscoveryDisplayName 返回默认列表条目发布时使用的显示名：旧 display_names 命中即用配置值，
// 否则用条目自带值。上一批的显示名机制（KDR-6）在两个默认列表出口上照旧生效。
func DefaultDiscoveryDisplayName(cfg GroupModelAllowlist, modelID, fallback string) string {
	if configured, ok := cfg.DisplayNameFor(modelID); ok {
		return configured
	}
	return fallback
}

// GeminiCLIDefaultModelIDs 返回 Gemini CLI 默认模型列表的 id（顺序一致），
// 供 Claude 形状出口判定「目录行的真实模型是否属于可见集合」。
func GeminiCLIDefaultModelIDs() []string {
	ids := make([]string, len(geminicli.DefaultModels))
	for i, model := range geminicli.DefaultModels {
		ids[i] = model.ID
	}
	return ids
}

// domainGroupModelCatalog 把 service 目录转成 domain 类型（逐字段拷贝）。
func domainGroupModelCatalog(entries []GroupModelCatalogEntry) []domain.GroupModelCatalogEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]domain.GroupModelCatalogEntry, len(entries))
	for i, entry := range entries {
		out[i] = domain.GroupModelCatalogEntry{
			ShellID:     entry.ShellID,
			DisplayName: entry.DisplayName,
			RealModel:   entry.RealModel,
			Supports1M:  entry.Supports1M,
		}
	}
	return out
}

// groupModelCatalogFromDomain 把 ent 读出的 domain 目录转成 service 类型（逐字段拷贝）。
func groupModelCatalogFromDomain(entries []domain.GroupModelCatalogEntry) []GroupModelCatalogEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]GroupModelCatalogEntry, len(entries))
	for i, entry := range entries {
		out[i] = GroupModelCatalogEntry{
			ShellID:     entry.ShellID,
			DisplayName: entry.DisplayName,
			RealModel:   entry.RealModel,
			Supports1M:  entry.Supports1M,
		}
	}
	return out
}
