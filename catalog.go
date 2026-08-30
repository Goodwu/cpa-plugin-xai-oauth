package main

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// catalogJSON is a trimmed copy of the repository's static xAI model catalog
// (internal/registry/models/models.json "xai" section) used to backfill
// metadata onto discovered models. Media-only builtin entries are omitted.
//
//go:embed catalog.json
var catalogJSON []byte

// catalogEntry mirrors one catalog.json item.
type catalogEntry struct {
	ID                        string           `json:"id"`
	Object                    string           `json:"object"`
	Created                   int64            `json:"created"`
	OwnedBy                   string           `json:"owned_by"`
	Type                      string           `json:"type"`
	DisplayName               string           `json:"display_name"`
	Name                      string           `json:"name"`
	Description               string           `json:"description"`
	ContextLength             int64            `json:"context_length"`
	MaxCompletionTokens       int64            `json:"max_completion_tokens"`
	Thinking                  *catalogThinking `json:"thinking"`
	SupportedInputModalities  []string         `json:"supported_input_modalities"`
	SupportedOutputModalities []string         `json:"supported_output_modalities"`
}

type catalogThinking struct {
	ZeroAllowed bool     `json:"zero_allowed"`
	Levels      []string `json:"levels"`
}

var (
	catalogOnce  sync.Once
	catalogCache []wireModelInfo
)

// catalogModels returns the embedded static catalog as wire model metadata.
func catalogModels() []wireModelInfo {
	catalogOnce.Do(func() {
		var entries []catalogEntry
		if err := json.Unmarshal(catalogJSON, &entries); err != nil {
			return
		}
		models := make([]wireModelInfo, 0, len(entries))
		for _, entry := range entries {
			model := wireModelInfo{
				ID:                        entry.ID,
				Object:                    entry.Object,
				Created:                   entry.Created,
				OwnedBy:                   entry.OwnedBy,
				Type:                      entry.Type,
				DisplayName:               entry.DisplayName,
				Name:                      entry.Name,
				Description:               entry.Description,
				ContextLength:             entry.ContextLength,
				MaxCompletionTokens:       entry.MaxCompletionTokens,
				SupportedInputModalities:  entry.SupportedInputModalities,
				SupportedOutputModalities: entry.SupportedOutputModalities,
			}
			if entry.Thinking != nil {
				model.Thinking = &wireThinkingSupport{
					ZeroAllowed: entry.Thinking.ZeroAllowed,
					Levels:      append([]string(nil), entry.Thinking.Levels...),
				}
			}
			models = append(models, model)
		}
		catalogCache = models
	})
	return catalogCache
}
