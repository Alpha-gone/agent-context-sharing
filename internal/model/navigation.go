package model

// ValidateNavigationReferences는 요약의 탐색 참조 수·식별자·중복·자기 참조를 검사한다.
func ValidateNavigationReferences(contextID ID, kind DerivationKind, references []ID) error {
	if len(references) > 100 {
		return fieldErrorf("navigation_refs", "탐색 참조는 100개 이하여야 한다")
	}
	if len(references) > 0 && kind != DerivationKindSummary {
		return fieldErrorf("navigation_refs", "탐색 참조는 요약 파생에만 허용한다")
	}
	seen := make(map[ID]struct{}, len(references))
	for _, id := range references {
		if !id.IsV7() || id == contextID {
			return fieldErrorf("navigation_refs", "탐색 대상은 자신이 아닌 UUIDv7이어야 한다")
		}
		if _, found := seen[id]; found {
			return fieldErrorf("navigation_refs", "탐색 대상을 중복해서 지정할 수 없다")
		}
		seen[id] = struct{}{}
	}
	return nil
}
