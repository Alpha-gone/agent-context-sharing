package contract

import (
	"bytes"
	_ "embed"
)

// toolManifest는 서버의 공개 도구 정의에서 생성한 계약 스냅샷이다.
//
//go:embed tool_manifest.json
var toolManifest []byte

// Manifest는 검증과 호스트 도구 공개에 사용할 도구 계약의 독립 복사본을 반환한다.
func Manifest() []byte { return bytes.Clone(toolManifest) }
