//go:build !darwin && !linux

package main

import "fmt"

func renameTrialExclusive(from, to string) error {
	return fmt.Errorf("이 운영체제에서 덮어쓰기 없는 결과 공개를 지원하지 않는다")
}
