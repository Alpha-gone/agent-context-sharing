package model

import "fmt"

// ValidateGraphUpdate는 이름·설명과 다음 판만 바뀌었는지 확인한다.
// 삭제·보관 유예·저장량·활동 시각의 시스템 전이는 별도 저장 계약이 담당한다.
func ValidateGraphUpdate(previous, next Graph) error {
	if err := previous.Validate(); err != nil {
		return fmt.Errorf("기존 그래프: %w", err)
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("다음 그래프: %w", err)
	}
	if previous.ID != next.ID || previous.CreatedBy != next.CreatedBy || !previous.CreatedAt.Equal(next.CreatedAt) {
		return fieldErrorf("graph_id", "그래프 식별자와 생성 메타데이터는 바꿀 수 없다")
	}
	if !previous.LastActivityAt.Equal(next.LastActivityAt) || previous.StoredChars != next.StoredChars || !sameOptionalTime(previous.DeletedAt, next.DeletedAt) || !sameOptionalTime(previous.GraceStartedAt, next.GraceStartedAt) {
		return fieldErrorf("graph_id", "그래프 정보 수정은 시스템 속성을 바꿀 수 없다")
	}
	if next.Version != previous.Version+1 {
		return fieldErrorf("expected_version", "그래프 수정은 판 번호를 하나 증가시켜야 한다")
	}
	return nil
}

// ValidateDerivedSources는 저장소가 조회한 근거들의 식별자·계층·그래프를 확인한다.
// 근거 파생의 다른 참조를 재귀 조회하지 않고 전달된 속성만 검사한다.
func ValidateDerivedSources(derived Context, sources []Context) error {
	if err := derived.Validate(); err != nil {
		return err
	}
	if derived.Layer != LayerDerived {
		return fieldErrorf("layer", "파생 근거 검증에는 파생 컨텍스트가 필요하다")
	}
	if len(sources) != len(derived.Derived.DerivedFrom) {
		return fieldErrorf("derived_from", "조회된 근거 개수가 참조 개수와 다르다")
	}
	byID := make(map[ID]Context, len(sources))
	for _, source := range sources {
		if err := source.ValidateProperties(); err != nil {
			return fmt.Errorf("파생 근거: %w", err)
		}
		if source.GraphID != derived.GraphID || source.Layer == LayerEvent || source.DeletedAt != nil {
			return fieldErrorf("derived_from", "근거는 같은 그래프의 활성 원천 또는 파생이어야 한다")
		}
		byID[source.ID] = source
	}
	for _, id := range derived.Derived.DerivedFrom {
		if _, found := byID[id]; !found {
			return fieldErrorf("derived_from", "조회 결과에 지정한 근거가 없다")
		}
	}
	return nil
}

// ValidateSupersedes는 저장소가 조회한 대체 대상이 같은 그래프·같은 계열의 파생인지 확인한다.
func ValidateSupersedes(derived, previous Context) error {
	if err := derived.Validate(); err != nil {
		return err
	}
	if err := previous.Validate(); err != nil {
		return fmt.Errorf("이전 파생: %w", err)
	}
	if derived.Layer != LayerDerived || previous.Layer != LayerDerived {
		return fieldErrorf("supersedes", "대체의 양 끝은 파생이어야 한다")
	}
	if derived.Derived.Supersedes == nil || *derived.Derived.Supersedes != previous.ID || derived.GraphID != previous.GraphID || previous.DeletedAt != nil {
		return fieldErrorf("supersedes", "대체 대상은 지정한 같은 그래프의 활성 파생이어야 한다")
	}
	if derived.Derived.Kind != previous.Derived.Kind || derived.Derived.SummaryScope != previous.Derived.SummaryScope {
		return fieldErrorf("supersedes", "대체 대상은 같은 파생 종류와 요약 범위여야 한다")
	}
	return nil
}
