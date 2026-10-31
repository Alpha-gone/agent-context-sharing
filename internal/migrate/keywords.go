package migrate

import "slices"

// unquotedKeywords는 PostgreSQL 18의 예약어(R)와 타입·함수 이름 키워드(T)다.
// 마이그레이션 파일 일부는 그래프 이름과 콜드 tablespace를 따옴표 없이 치환하고, 적용된 SQL은
// 바꿀 수 없으므로 이 단어들을 식별자 자리에서 거부한다. 열 이름 키워드(C)와 비예약어(U)는
// 스키마·tablespace 자리에서 따옴표 없이 쓸 수 있다. 목록은 pg_get_keywords()와 대조한다.
var unquotedKeywords = []string{
	"all", "analyse", "analyze", "and", "any", "array", "as", "asc", "asymmetric",
	"authorization", "binary", "both", "case", "cast", "check", "collate", "collation",
	"column", "concurrently", "constraint", "create", "cross", "current_catalog",
	"current_date", "current_role", "current_schema", "current_time", "current_timestamp",
	"current_user", "default", "deferrable", "desc", "distinct", "do", "else", "end",
	"except", "false", "fetch", "for", "foreign", "freeze", "from", "full", "grant", "group",
	"having", "ilike", "in", "initially", "inner", "intersect", "into", "is", "isnull",
	"join", "lateral", "leading", "left", "like", "limit", "localtime", "localtimestamp",
	"natural", "not", "notnull", "null", "offset", "on", "only", "or", "order", "outer",
	"overlaps", "placing", "primary", "references", "returning", "right", "select",
	"session_user", "similar", "some", "symmetric", "system_user", "table", "tablesample",
	"then", "to", "trailing", "true", "union", "unique", "user", "using", "variadic",
	"verbose", "when", "where", "window", "with",
}

// unquotedKeyword는 name이 따옴표 없는 식별자 자리에서 구문 오류가 되는 키워드인지 확인한다.
func unquotedKeyword(name string) bool {
	_, found := slices.BinarySearch(unquotedKeywords, name)
	return found
}
