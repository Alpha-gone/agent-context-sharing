# 내장 외부 자산

WEB-01에서 공식 고정 태그의 배포 파일을 그대로 확인했다. 런타임에 CDN이나 외부 서체를 요청하지 않는다. HTTP 제공 목록은 `../assets.go`가 소유하며 이 문서는 제공하지 않는다.

- Cytoscape.js **3.34.3**: https://github.com/cytoscape/cytoscape.js/tree/v3.34.3
  - `cytoscape.min.js`: 공식 `dist/cytoscape.min.js`와 바이트 단위 일치.
  - `CYTOSCAPE-LICENSE.txt`: 공식 `LICENSE` 전문, MIT.
- Pretendard **1.3.9**: https://github.com/orioncactus/pretendard/tree/v1.3.9
  - `PretendardVariable.woff2`: 공식 `packages/pretendard/dist/web/variable/woff2/PretendardVariable.woff2` 원본.
  - `PRETENDARD-LICENSE.txt`: 공식 `LICENSE` 전문, SIL OFL 1.1과 예약 글꼴 이름 고지.
  - `app.css`는 공식 가변 서체의 이름·45~920 굵기·normal·swap을 유지하고 같은 출처의 원본 파일을 참조한다.

## SHA-256

- `cytoscape.min.js`: `5f3b5b529546d5af1fc5628590af033b74511a5b6f789f5f4682845863228b91`
- `CYTOSCAPE-LICENSE.txt`: `eb319c6e6f233607f71e8e2f450391751883cfc0eeb3ca7ef574c13d1d9c2203`
- `PretendardVariable.woff2`: `9599f12fd42fc0bce1cd50b47a0c022e108d7aa64dd0d1bb0ed44f3282d900b4`
- `PRETENDARD-LICENSE.txt`: `d31ddd9f2bed32fd7e302a205cf2380ba0de6529152d239ef99cfb6f261bfc04`

판올림 시 공식 배포·라이선스·위 해시와 `../foundation_test.go`의 고정 자산 검증을 함께 갱신한다. 글꼴을 수정하거나 부분 집합을 새로 생성하지 않았다.
