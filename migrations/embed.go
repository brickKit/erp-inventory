// Package migrations 只做一件事：把这个目录下的 .sql 文件嵌进迁移二进制
// （backend/cmd/migrate 把 FS 交给 be-sdk-go 的 migrate.Main）。镜像里因此
// 不需要另拷 .sql 文件；进外壳时 brickKit 也用本组件自己的镜像跑这个迁移。
//
// ⚠️ 嵌入必须挨着 .sql 文件本身的目录来做：Go 的 embed 不允许路径里出现
// ".."，在别的包里 //go:embed ../../migrations 编译不过。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
