package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
)

func validateModeFlags(convert string, adaptive, consistency, adversarial bool) error {
	var invalid bool
	flag.Visit(func(f *flag.Flag) {
		switch {
		case strings.HasPrefix(f.Name, "convert-"):
			invalid = invalid || convert == ""
		case strings.HasPrefix(f.Name, "consistency-"):
			invalid = invalid || !consistency
		case strings.HasPrefix(f.Name, "adaptive-") || f.Name == "direct-thresholds" || f.Name == "direct-margins":
			invalid = invalid || !adaptive
		case f.Name == "attacks":
			invalid = invalid || (!adversarial && convert != convertOwnAdversarial)
		}
	})
	if invalid {
		return fmt.Errorf("평가 모드와 전용 옵션이 맞지 않는다")
	}
	return nil
}

func parseIntegerOptions(raw string, minimum int) ([]int, error) {
	var values []int
	for part := range strings.SplitSeq(raw, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || value < minimum {
			return nil, fmt.Errorf("일관성 정수 목록은 각 값이 %d 이상이어야 한다", minimum)
		}
		values = append(values, value)
	}
	return values, nil
}
