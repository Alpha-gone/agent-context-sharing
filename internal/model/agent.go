package model

// ValidateRegistrationKey는 자격 증명이 아닌 등록 재조회 키의 고정 ASCII 계약을 확인한다.
func ValidateRegistrationKey(key string) error {
	valid := len(key) >= 16 && len(key) <= 128
	for i := range len(key) {
		c := key[i]
		valid = valid && (c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-')
	}
	if !valid {
		return FieldError{Field: "registration_key", Message: "등록 키는 16~128자의 영문·숫자·밑줄·하이픈이어야 한다"}
	}
	return nil
}
