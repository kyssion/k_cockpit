package logging

import (
	"regexp"
)

// 脱敏发生在**写入时**，不是读取时（F-9-02）。
//
// 这是本包最要紧的一条设计决定，理由值得写清楚：
//
//	读取时脱敏意味着**文件里已经是明文**。任何能看到那个文件的路径——
//	宿主机上的运维登录、备份系统、日志采集代理、磁盘镜像——都能拿到
//	完整内容。而"我们提供了脱敏的查看接口"会让人以为日志是安全的，
//	于是更不会去管文件的权限与去向。
//
// 写入时脱敏则只有一份风险面，而且它**不可绕过**：绕过界面的查看方式
// 拿到的仍然是脱敏后的内容。
//
// 代价是原始值不可恢复——但日志本就不该用来还原密码。排查需要的是
// "这里有一个密码字段、它被设置了"，而不是它的内容。

// redactPlaceholder 是替换后的文本。
//
// 用固定占位符而不是等长的星号：**长度本身也是信息**。一串 32 个星号
// 与一串 8 个星号能让人推断出密钥长度，而在有些系统上那足以缩小暴力
// 搜索的范围。
const redactPlaceholder = "[已脱敏]"

// keyValueRule 匹配「键名 + 可选引号 + 分隔符 + 值」。
//
// 分组刻意拆成四段——**键名、收尾引号、分隔符、值**：
//
//   - 键名必须留下（排查时要能看出这里原本是什么字段）
//   - 分隔符必须留下（否则变成 "password[已脱敏]" 这种读不通的文本）
//   - 收尾引号单独一组，才能正确处理 "password":"x" 这种形式
//
// 早期版本把「键名 + 值」直接替换掉，结果是"脱敏成功了但日志从此失去
// 排查价值"——一行 [已脱敏] 看不出那是什么字段。
var keyValueRule = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|apikey|auth[_-]?token|access[_-]?token|refresh[_-]?token|private[_-]?key|cookie|session[_-]?id|client[_-]?secret)\b("?)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}\]]+)`)

// authorizationRule 单独处理 Authorization。
//
// 它的值**到行尾为止**——`Authorization: Bearer eyJ...` 里的抬头与令牌之间
// 有空格，而通用的「值到空白为止」会在 `Bearer` 处停下，把真正的令牌留在
// 后面（这个漏洞在早期版本的测试里被抓到过）。
//
// 整个尾部一起替换是刻意的过度脱敏：Authorization 的值是不透明的，把它
// 之后的内容一并抹掉，代价是这一行后面若还有别的字段会丢，而收益是**不
// 可能漏掉令牌**。在这个取舍上，少一条日志字段远好过一次凭据泄漏。
var authorizationRule = regexp.MustCompile(`(?i)\bauthorization\b\s*[:=]\s*.+`)

// 其余规则按**从具体到宽泛**的顺序执行——顺序是有意的，见 Redact。
var otherRules = []*regexp.Regexp{
	// Bearer / Basic / Digest 抬头之后的凭据。
	regexp.MustCompile(`(?i)\b(bearer|basic|digest)\s+[A-Za-z0-9._~+/=-]{8,}`),
	// JWT：三段点分的 base64url。它经常被直接拼进消息而没有任何键名。
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	// 连接串里的口令：scheme://user:pass@host
	regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^:/@\s]+:)[^@\s]+(@)`),
	// TOTP 恢复码形态（XXXX-XXXX-XXXX）。它出现在日志里通常意味着某处把
	// 它回显了，而那本身就是个问题——但先脱敏再说。
	regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4}\b`),
}

// Redact 对一行文本做脱敏。
//
// **没有短路判断。** 早期版本先做一次廉价的"这行可能含敏感词"检查，命中
// 才跑正则——理由是绝大多数日志行不含敏感词，跳过正则能省下开销。
//
// 那个优化被删掉了，因为它的代价是一个**必须与规则手工保持同步的清单**：
// 加规则时忘了往清单里加关键词，那一类就永远不会被脱敏，而失败方式是静默
// 的——日志看上去一切正常，直到某天有人在里面看到明文密码。写这段代码时
// 就真的漏了 `session_id`。
//
// 正则本身是线性的（字符类 + 有限重复，没有嵌套量词），跑一遍的开销在
// 微秒量级。用一次静默泄漏的风险换这点开销，不划算。
func Redact(line string) string {
	if line == "" {
		return line
	}

	// **顺序有讲究**：先处理凭据形态，再处理键值对。
	//
	// 反过来的话，`Authorization: Bearer eyJ...` 会被键值对规则先匹配到
	// ——它的值模式在空白处停下，于是只吃掉 `Bearer`，把真正的令牌留在
	// 后面，而整行看上去"已经脱敏过了"。
	out := line
	out = authorizationRule.ReplaceAllString(out, redactPlaceholder)
	for _, r := range otherRules {
		out = r.ReplaceAllString(out, "${1}"+redactPlaceholder+"${2}")
	}
	return keyValueRule.ReplaceAllStringFunc(out, redactKeyValue)
}

// redactKeyValue 处理一处键值对，**保留值外面的引号**。
//
// 不保留的话，JSON 日志会变成 `"password":[已脱敏]` ——那是无效 JSON，
// 而日志里的 JSON 体经常被复制到工具里查看。脱敏的代价不该包括"让人没法
// 用工具读这一行"。
func redactKeyValue(m string) string {
	sub := keyValueRule.FindStringSubmatch(m)
	if len(sub) < 5 {
		return redactPlaceholder
	}
	key, quote, sep, val := sub[1], sub[2], sub[3], sub[4]
	if len(val) >= 2 {
		first, last := val[0], val[len(val)-1]
		if (first == '"' || first == '\'') && first == last {
			return key + quote + sep + string(first) + redactPlaceholder + string(last)
		}
	}
	return key + quote + sep + redactPlaceholder
}

// **脱敏必须逐行独立。** 一行的判定不能依赖上一行——否则把两行拼起来看就
// 不再等效，而日志恰恰经常被分段传输与并发写入。
