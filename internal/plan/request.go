package plan

// CheckHop은 요청 홉 수를 검사하고 요청 계정의 결과 노드 상한을 반환한다.
// 요청 0홉은 시작점만 뜻하며, 플랜의 노드 상한 0은 결과 수를 제한하지 않는다.
func (limits Limits) CheckHop(hops int) (int, error) {
	if err := CheckRequest("max_hops", int64(hops), int64(limits.MaxHops)); err != nil {
		return 0, err
	}
	if limits.MaxHopNodes < 0 {
		return 0, validationError("max_hop_nodes", "노드 상한은 음수일 수 없다", nil)
	}
	return limits.MaxHopNodes, nil
}

// Resolve는 생략한 페이지 크기에 기본값을 적용하고 양수·플랜 상한을 검사한다.
// name은 graph_page 또는 relation_page처럼 호출 경계가 소유하는 고정 항목명이다.
func (page PageSize) Resolve(name string, requested *int) (int, error) {
	if page.Default <= 0 || page.Maximum < 0 || (page.Maximum > 0 && page.Default > page.Maximum) {
		return 0, validationError(name, "페이지 기본값과 상한이 올바르지 않다", nil)
	}
	value := page.Default
	if requested != nil {
		value = *requested
	}
	if value <= 0 {
		return 0, validationError(name, "요청 페이지 크기는 양수여야 한다", nil)
	}
	if err := CheckRequest(name, int64(value), int64(page.Maximum)); err != nil {
		return 0, err
	}
	return value, nil
}

// ResolveBudget은 생략한 예산에 계정 플랜을 적용하고 호출자의 양수 예산을 검사한다.
// 플랜의 0은 무제한이지만 호출자가 0으로 유한한 플랜을 우회할 수는 없다.
func (limits Limits) ResolveBudget(requested *int) (int, error) {
	if limits.ContextBudget < 0 {
		return 0, validationError("context_budget", "예산 한도는 음수일 수 없다", nil)
	}
	if requested == nil {
		return limits.ContextBudget, nil
	}
	if *requested <= 0 {
		return 0, validationError("context_budget", "호출자가 지정한 예산은 양수여야 한다", nil)
	}
	if err := CheckRequest("context_budget", int64(*requested), int64(limits.ContextBudget)); err != nil {
		return 0, err
	}
	return *requested, nil
}
