package model

import "time"

// RelationInferenceTask는 판정 작업의 확보 세대와 리스를 보존한다.
type RelationInferenceTask struct {
	ID, GraphID, ContextID, TriggerAccountID ID
	TargetVersion                            int64
	Attempts, OutputAttempts                 int
	CorrelationID                            string
	Generation, LeasedUntil                  time.Time
}
