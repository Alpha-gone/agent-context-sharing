package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// webFiles는 템플릿·서체·시각화 자산과 재배포 고지를 바이너리에 보존한다.
// HTTP로 제공하는 파일은 loadAssets의 허용 목록으로 제한한다.
//
//go:embed assets/* templates/*.html
var webFiles embed.FS

const assetCacheControl = "public, max-age=86400"

type staticAsset struct {
	content     []byte
	etag        string
	contentType string
}

func loadAssets() (map[string]staticAsset, error) {
	contentTypes := map[string]string{
		"app.css":                  "text/css; charset=utf-8",
		"PretendardVariable.woff2": "font/woff2",
		"cytoscape.min.js":         "text/javascript; charset=utf-8",
		"CYTOSCAPE-LICENSE.txt":    "text/plain; charset=utf-8",
		"PRETENDARD-LICENSE.txt":   "text/plain; charset=utf-8",
	}
	assets := make(map[string]staticAsset, len(contentTypes))
	for name, contentType := range contentTypes {
		content, err := webFiles.ReadFile("assets/" + name)
		if err != nil {
			return nil, fmt.Errorf("웹 자산 읽기: %w", err)
		}
		digest := sha256.Sum256(content)
		assets["/assets/"+name] = staticAsset{
			content: content, etag: `"` + hex.EncodeToString(digest[:]) + `"`, contentType: contentType,
		}
	}
	return assets, nil
}

// serveAsset은 수정 시각이 없는 내장 파일을 내용 해시로 재검증한다.
// ServeContent가 조건부 요청·HEAD·범위 요청을 처리하며 파일 시스템은 노출하지 않는다.
func serveAsset(writer http.ResponseWriter, request *http.Request, asset staticAsset) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "허용되지 않는 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}
	writer.Header().Set("Content-Type", asset.contentType)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("ETag", asset.etag)
	writer.Header().Set("Cache-Control", assetCacheControl)
	http.ServeContent(writer, request, strings.TrimPrefix(request.URL.Path, "/assets/"), time.Time{}, bytes.NewReader(asset.content))
}
