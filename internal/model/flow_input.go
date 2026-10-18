package model

import (
	"strings"
	"unicode/utf8"
)

// Validate는 DB·제공자에 전달하기 전에 검색 입력의 논리 계약을 검사한다.
// 미지정 as_of·scope의 기본값과 계정별 예산 한도 적용은 호출 경계가 소유한다.
func (input FlowInput) Validate() error {
	if err := validateID("graph_id", input.GraphID); err != nil {
		return err
	}
	if err := validateText("work_context", input.WorkContext); err != nil {
		return err
	}
	if strings.TrimSpace(input.WorkContext) == "" || utf8.RuneCountInString(input.WorkContext) > 8000 {
		return fieldErrorf("work_context", "현재 작업 컨텍스트는 공백만인 값을 제외한 1자 이상 8000자 이하여야 한다")
	}
	if !input.AsOf.IsZero() && !isUTC(input.AsOf) {
		return fieldErrorf("as_of", "검색 기준 시각은 UTC여야 한다")
	}
	switch input.Scope {
	case "", "auto", "local", "global":
	default:
		return fieldErrorf("scope", "검색 범위는 auto, local, global 중 하나여야 한다")
	}
	// 0은 계정 플랜의 한도 없음이다. 호출자의 상한 적용은 권한 경계에서 수행한다.
	if input.Budget < 0 {
		return fieldErrorf("budget", "검색 문자 예산은 음수일 수 없다")
	}
	if input.MaxHops < 0 {
		return fieldErrorf("max_hops", "검색 홉 깊이는 음수일 수 없다")
	}
	if input.MaxHopNodes < 0 {
		return fieldErrorf("max_hop_nodes", "검색 홉 결과 상한은 음수일 수 없다")
	}
	return nil
}
