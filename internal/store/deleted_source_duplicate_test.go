package store

import (
	"errors"
	"testing"

	"agent_context_sharing/internal/model"
)

func TestDeletedSourceDuplicateRetainsDeletionIntegration(t *testing.T) {
	for _, channel := range []string{"web", "mcp"} {
		t.Run(channel, func(t *testing.T) {
			database := newSmallPoolStore(t)
			source := restoreChannelTarget(t, database, model.LayerSource)
			var deleted model.Context
			var err error
			if channel == "web" {
				deleted, err = database.SetContextDeleted(t.Context(), source.GraphID, source.ID, source.CreatedBy, true, 0)
			} else {
				deleted, err = database.DiscardContext(t.Context(), source.GraphID, source.ID, testMCPDiscardOperation(source), WriteLimits{})
			}
			if err != nil || deleted.DeletedAt == nil {
				t.Fatalf("delete: %v", err)
			}
			before := registrationStateFingerprint(t, database)
			duplicate := testSourceContext(t, source.GraphID, source.CreatedBy, source.Source.Reference.Locator)
			duplicate.Body = "recreation must not replace the source"
			got, err := database.CreateContextWithOperation(t.Context(), source.GraphID, duplicate, nil, registrationOperation(duplicate, OperationAdd), WriteLimits{})
			if err != nil || got.ID != source.ID || got.Body != source.Body || got.DeletedAt == nil || !got.DeletedAt.Equal(*deleted.DeletedAt) {
				t.Fatalf("deleted duplicate: %+v/%v", got, err)
			}
			if before != registrationStateFingerprint(t, database) {
				t.Fatal("duplicate changed data, queue, activity or audit")
			}
			restore := testMCPDiscardOperation(source)
			restore.Kind = OperationUpdate
			_, err = database.RestoreContext(t.Context(), source.GraphID, source.ID, restore, WriteLimits{})
			if channel == "web" {
				if !errors.Is(err, ErrRestoreChannel) {
					t.Fatalf("web deletion bypassed: %v", err)
				}
			} else if err != nil {
				t.Fatalf("MCP restoration: %v", err)
			}
		})
	}
}
