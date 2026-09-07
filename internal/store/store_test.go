package store

import "testing"

// sharedGraphNamePattern은 config, migrate와 store가 함께 쓰기로 한 AGE 그래프 이름 경계다.
// 세 패키지가 각자 이 리터럴과의 일치를 확인하므로, 한 곳만 바꾸면 그 패키지의 테스트가
// 깨져 값이 갈라지는 것을 막는다. 값을 바꿀 때는 세 곳을 함께 고친다.
const sharedGraphNamePattern = `^[a-z_][a-z0-9_]*$`

// TestGraphNamePattern은 저장소의 그래프 이름 경계가 약속된 정규식과 같고 그 정규식이
// 기대한 이름을 가르는지 확인한다.
func TestGraphNamePattern(t *testing.T) {
	if got := graphNamePattern.String(); got != sharedGraphNamePattern {
		t.Fatalf("store의 그래프 이름 정규식 = %s, want %s", got, sharedGraphNamePattern)
	}
	tests := []struct {
		name  string
		valid bool
	}{
		{name: "agent_context", valid: true},
		{name: "_agent_context", valid: true},
		{name: "1agent_context", valid: false},
		{name: "agent-context", valid: false},
		{name: "AgentContext", valid: false},
	}
	for _, test := range tests {
		if got := graphNamePattern.MatchString(test.name); got != test.valid {
			t.Errorf("그래프 이름 %q 허용 결과 = %t, want %t", test.name, got, test.valid)
		}
	}
}
