package domain

// GroupModelCatalogEntry 是分组模型目录的一行：客户端看到的壳 id → 真实模型。
// 目录与白名单是两个职责——目录决定「客户端看到哪些壳 id、叫什么」，
// 白名单决定「允许调哪些真实模型」；两者同域存储，随分组一次读写。
type GroupModelCatalogEntry struct {
	ShellID     string `json:"shell_id"`
	DisplayName string `json:"display_name"`
	RealModel   string `json:"real_model"`
	Supports1M  bool   `json:"supports_1m,omitempty"`
}

// GroupModelAllowlist 是分组级模型白名单：开启后既过滤模型列表类接口的返回内容，
// 也约束分组实际可调用的模型（网关准入在合成路由改写与调度之前完成）。
type GroupModelAllowlist struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models,omitempty"`
	// DisplayNames 是「模型 id 原文 → 展示文本」的显式映射，只影响 Claude 形状模型
	// 列表的 display_name；与 Enabled / Models 相互独立。
	DisplayNames map[string]string `json:"display_names,omitempty"`
	// Catalog 是分组模型目录：客户端可见的壳 id 及其真实模型。
	// 与 Enabled / Models / DisplayNames 相互独立；无目录时键不出现，
	// 存量分组读出 nil，行为与建立目录机制之前逐字一致。
	Catalog []GroupModelCatalogEntry `json:"catalog,omitempty"`
}
