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
	// MemberLoginIDs는 관리 팀 조회에서만 채우는 현재 구성원의 로그인 아이디다.
	MemberLoginIDs []string
	// GraphGrants는 관리 팀 조회에서만 채우며 그래프 내용·접근 권한을 포함하지 않는다.
	GraphGrants []TeamGraphGrant
}

// TeamGraphGrant는 팀에 보존된 그래프별 부여 등급이다. 삭제된 팀의 상속은 무효다.
type TeamGraphGrant struct {
	GraphID ID
	Grade   GraphGrade
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
	ID         ID
	Source     string
	Kind       string
	Action     string
	ActorID    ID
	OccurredAt time.Time
	Target     string
	Detail     string
}

// AuditFilter는 UTC 시작 포함·끝 제외 기간과 선택 대상의 감사 범위다.
// nil 시각과 영 ID는 해당 조건을 제한하지 않는다.
type AuditFilter struct {
	From, Until *time.Time
	Target      ID
}
