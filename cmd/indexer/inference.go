package main

import (
	"agent_context_sharing/internal/config"
	"agent_context_sharing/internal/model"
	"agent_context_sharing/internal/relinfer"
)

func inferenceRuntimeConfig(cfg config.Config, embeddingModel string) relinfer.RuntimeConfig {
	value := cfg.RelationInference
	result := relinfer.RuntimeConfig{Mode: value.Mode, Provider: value.Provider, Model: value.Model, APIKey: value.APIKey, EmbeddingModel: embeddingModel, AcceptedProfile: value.AcceptedProfile, Timeout: value.Timeout, IndexWait: value.IndexWait, AdjacencyWindow: cfg.RelationAdjacencyWindow, CandidateLimit: value.CandidateLimit, TargetChars: value.TargetChars, CandidateChars: value.CandidateChars, InputChars: value.InputChars}
	if value.BaseURL != nil {
		result.BaseURL = value.BaseURL.String()
	}
	for _, kind := range value.AutoConfirmTypes {
		result.AutoConfirmTypes = append(result.AutoConfirmTypes, model.RelationType(kind))
	}
	return result
}
