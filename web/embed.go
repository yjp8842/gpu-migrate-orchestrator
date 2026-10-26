// Package web 은 화면(React) 빌드 결과를 실행 파일에 담는다.
//
// npm run build 가 dist/ 를 채운다. 빌드하지 않았으면 dist/.gitkeep 만 있어 화면 없이 API 만 뜬다.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist 는 dist/ 안의 파일이다. index.html 이 없으면 nil (화면을 빌드하지 않은 실행 파일).
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
