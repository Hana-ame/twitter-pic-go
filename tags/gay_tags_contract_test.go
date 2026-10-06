package tags

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 图站前端（gallery/static/home.js）的默认 Gay 词表，与 Flutter 客户端
// lib/services/storage_service.dart 的 kDefaultGayTags 属于**同一份跨端契约**。
//
// 背景：知识库里曾记着词表是 gay/yaoi/futanari/男同/基/bl，而线上代码里
// 实际是 男性/男娘/人妖/露屌/阳痿/男同。那 5 个英文/单字词在真实的
// account_tags 里**一条记录都没有** —— 所以知识库那份若按字面执行，是把线上
// 正在过滤的 341 个账号**全部放行**，不是「多过滤几个」。这类「文档描述的
// 能力和代码里的不是一回事」，靠改文档防不住，必须把字面量钉住。
//
// 判据可证伪：改 home.js 的 DEFAULT_GAY_TAGS 或 Dart 侧的 kDefaultGayTags
// 而不同步另一边，本测试立刻变红。
const webDefaultGayTagsLine = `const DEFAULT_GAY_TAGS = ["男性", "男娘", "人妖", "露屌", "阳痿", "男同"];`

// homeJSPath 相对于本包（tags/）定位：../gallery/static/home.js。
// 测试的 cwd 是包目录，所以不能用仓库根的相对路径。
var homeJSPath = "../gallery/static/home.js"

// extractTagsFromJSList 从 JS 数组字面量里逐个取出中英文词。
func extractTagsFromJSList(line string) []string {
	re := regexp.MustCompile(`"([^"]+)"`)
	m := re.FindAllStringSubmatch(line, -1)
	out := make([]string, 0, len(m))
	for _, g := range m {
		out = append(out, g[1])
	}
	return out
}

func TestWebGalleryGayTagsUnchanged(t *testing.T) {
	b, err := os.ReadFile(homeJSPath)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", homeJSPath, err)
	}
	var found string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "DEFAULT_GAY_TAGS") && strings.Contains(line, "[") {
			found = strings.TrimSpace(line)
			break
		}
	}
	if found == "" {
		t.Fatalf("%s 里找不到 DEFAULT_GAY_TAGS 的定义行", homeJSPath)
	}
	if found != webDefaultGayTagsLine {
		t.Fatalf("图站 Gay 词表被改了，但 Flutter 客户端还停在旧值（跨端契约漂移）:\n			现在: %s\n			期望: %s\n			请同步修改 lib/services/storage_service.dart 的 kDefaultGayTags",
			found, webDefaultGayTagsLine)
	}
	tags := extractTagsFromJSList(found)
	if len(tags) != 6 {
		t.Fatalf("期望 6 个词，实际解析出 %d 个: %v", len(tags), tags)
	}
}