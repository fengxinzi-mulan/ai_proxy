// Vite 的 emptyOutDir 会先清空 dist，把仓库里用于占位的 .gitkeep 一起删掉，
// 于是每次构建后 git status 都会多出一条「已删除」。构建结束把它补回来。
//
// 有这个文件在，刚克隆下来、还没构建前端时 go build 也能通过，
// 此时访问根路径会看到一个「请先构建前端」的提示页。

import { writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const dist = join(dirname(fileURLToPath(import.meta.url)), '..', 'dist')

writeFileSync(
  join(dist, '.gitkeep'),
  `# 这个文件只用来保证 web/dist 目录在仓库里存在。
#
# go:embed 要求目标目录必须存在，而构建产物不入库；
# 有它在，刚克隆下来、还没构建前端时 go build 也能正常通过，
# 此时访问根路径会看到一个「请先构建前端」的提示页。
# 由 scripts/keep-dist.mjs 在每次构建后重新生成。
`,
)
