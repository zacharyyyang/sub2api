package geminicli

// Model represents a selectable Gemini model for UI/testing purposes.
// Keep JSON fields consistent with existing frontend expectations.
type Model struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	DisplayName   string `json:"display_name"`
	CreatedAt     string `json:"created_at"`
	ContextWindow int    `json:"context_window,omitempty"`
}

// DefaultModels is the curated Gemini model list used by the admin UI "test account" flow.
var DefaultModels = []Model{
	{ID: "gemini-2.0-flash", Type: "model", DisplayName: "Gemini 2.0 Flash", CreatedAt: ""},
	{ID: "gemini-2.5-flash", Type: "model", DisplayName: "Gemini 2.5 Flash", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-2.5-flash-image", Type: "model", DisplayName: "Gemini 2.5 Flash Image", CreatedAt: ""},
	{ID: "gemini-2.5-pro", Type: "model", DisplayName: "Gemini 2.5 Pro", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3.5-flash", Type: "model", DisplayName: "Gemini 3.5 Flash", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3-flash-preview", Type: "model", DisplayName: "Gemini 3 Flash Preview", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3-pro-preview", Type: "model", DisplayName: "Gemini 3 Pro Preview", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3.1-pro-preview", Type: "model", DisplayName: "Gemini 3.1 Pro Preview", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3.1-flash-image", Type: "model", DisplayName: "Gemini 3.1 Flash Image", CreatedAt: ""},
	{ID: "gemini-4-argon", Type: "model", DisplayName: "Gemini 4 Argon", CreatedAt: "", ContextWindow: 1000000},
}

// GoogleOneModels is the conservative model set exposed for legacy Google One
// OAuth accounts. Newer subscription models are served through Antigravity
// OAuth rather than the retired consumer Gemini CLI / Code Assist channel.
var GoogleOneModels = []Model{
	{ID: "gemini-2.0-flash", Type: "model", DisplayName: "Gemini 2.0 Flash", CreatedAt: ""},
	{ID: "gemini-2.0-flash-lite", Type: "model", DisplayName: "Gemini 2.0 Flash-Lite", CreatedAt: ""},
	{ID: "gemini-2.5-flash", Type: "model", DisplayName: "Gemini 2.5 Flash", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-2.5-flash-lite", Type: "model", DisplayName: "Gemini 2.5 Flash-Lite", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-2.5-pro", Type: "model", DisplayName: "Gemini 2.5 Pro", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3-flash-preview", Type: "model", DisplayName: "Gemini 3 Flash Preview", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3-pro-preview", Type: "model", DisplayName: "Gemini 3 Pro Preview", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3.1-pro-preview", Type: "model", DisplayName: "Gemini 3.1 Pro Preview", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3.1-pro-preview-customtools", Type: "model", DisplayName: "Gemini 3.1 Pro Preview CustomTools", CreatedAt: "", ContextWindow: 1000000},
	{ID: "gemini-3.5-flash", Type: "model", DisplayName: "Gemini 3.5 Flash", CreatedAt: "", ContextWindow: 1000000},
}

// GoogleOneModelMapping returns a new whitelist map for each account so callers
// cannot mutate the package-level catalog.
func GoogleOneModelMapping() map[string]string {
	mapping := make(map[string]string, len(GoogleOneModels))
	for _, model := range GoogleOneModels {
		mapping[model.ID] = model.ID
	}
	return mapping
}

// DefaultTestModel is the default model to preselect in test flows.
const DefaultTestModel = "gemini-2.0-flash"
