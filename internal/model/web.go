package model

import "time"

// GrantSubject는 그래프 등급을 직접 받는 계정 또는 팀을 표시한다.
type GrantSubject struct {
	Type      string
	ID        ID
	Name      string
	Grade     GraphGrade
	Inherited bool
	CanRevoke bool
}

// Team은 웹 관리 화면에 표시하는 팀의 메타데이터다.
type Team struct {
	ID               ID
	Name             string
	ManagerAccountID ID
	CreatedAt        time.Time
	DeletedAt        *time.Time
}

// DeletionImpact는 삭제 전에 안내할 연결된 대상의 수를 담는다.
type DeletionImpact struct {
	Contexts  int
	Relations int
	Accounts  int
	Derived   int
	Events    int
}

// RestoreRequest는 처리되지 않은 자동 삭제 그래프의 복구 요청이다.
type RestoreRequest struct {
	GraphID     ID
	GraphName   string
	RequestedBy ID
	RequestedAt time.Time
}

// AuditEntry는 컨텍스트 관리와 웹 관리 기록을 시간순으로 함께 표시하는 값이다.
type AuditEntry struct {
	Kind       string
	Action     string
	ActorID    ID
	OccurredAt time.Time
	Target     string
	Detail     string
}
