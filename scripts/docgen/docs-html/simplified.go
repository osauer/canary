package main

// Simplified Chinese is derived from the Traditional (Hong Kong) master at
// build time, never written beside it. Two steps: the glossary first, for
// vocabulary that differs by region rather than by script (Hong Kong says
// 帳戶 and 甚麼 where Singapore and the mainland say 账户 and 什么), then
// OpenCC's hk2s profile for the script itself. The glossary is applied to
// the Traditional text, so its keys are Traditional and its values are
// already Simplified; the script conversion leaves them alone.
//
// OpenCC is the pure-Go port github.com/longbridgeapp/opencc, pinned in this
// module's go.mod with its dictionaries embedded, so the build needs no
// binary on PATH. Owner decision 2026-11-04 12:34 CET: ship Simplified
// generated from Traditional from the first publication.

import (
	"strings"
	"sync"

	"github.com/longbridgeapp/opencc"
)

// glossary maps Hong Kong vocabulary to the Singapore and mainland word.
// Order matters: strings.Replacer tries pairs in this order at each
// position, so a longer phrase sits before the word it contains, and the
// process sense of 程序 is renamed only after the program sense of 程式 has
// become 程序. A single pass means a value is never re-matched as a key.
var glossary = []string{
	// Software terms. Hong Kong (and Taiwan) say 程式 for program and 程序
	// for process; the mainland says 程序 for program and 进程 for process.
	"守護程序", "守护进程",
	"安裝程序", "安装流程",
	"指令碼", "脚本",
	"應用程式", "应用程序",
	"程式碼", "代码",
	"原始碼", "源代码",
	"程式", "程序",
	"程序", "进程",
	"說明文件", "文档",
	"設定檔", "配置文件",
	"設定參考", "配置参考",
	"設定", "设置",
	"預設", "默认",
	"登入", "登录",
	"工作階段", "会话",
	"通訊埠", "端口",
	"迴環位址", "回环地址",
	"位址", "地址",
	"鎖定檔", "锁文件",
	"二進位檔", "二进制文件",
	"檔案", "文件",
	"遙距存取", "远程访问",
	"遙距", "远程",
	"存取", "访问",
	"意見回饋", "意见反馈",
	"回饋", "反馈",
	"私隱", "隐私",
	"終端機", "终端",
	"位元組", "字节",
	"權杖", "令牌",
	"鑰匙圈", "密钥环",
	"金鑰", "密钥",
	"簽章", "签名",
	"簽署", "签名",
	"快取", "缓存",
	"伺服器", "服务器",
	"稽核", "审计",
	"節流", "限速",
	"視窗", "窗口",
	"選單", "菜单",
	"勾選框", "复选框",
	"解譯器", "解释器",
	"執行環境", "运行时",
	"支援", "支持",
	"覆寫", "覆盖",
	"連結", "链接",
	"重新導向", "重定向",
	"退化", "降级",
	"判讀", "解读",
	"感測器", "传感器",
	"顯示器", "渲染器",
	"使用者", "用户",
	"家目錄", "主目录",
	"電郵", "电子邮件",
	"透過", "通过",
	"型別化", "类型化",
	"線路協定", "线路协议",
	"協定", "协议",
	"儲存庫", "仓库",
	"儲存", "存储",
	"構件", "制品",
	"發行版", "发布版",
	"函式庫", "库",
	"記憶體", "内存",
	"指令", "命令",
	"閘道", "网关",
	"核證", "验证",
	"回報", "报告",
	"撥接", "连接",
	"甚麼", "什么",
	// Broker and market terms.
	"戶口", "账户",
	"帳戶", "账户",
	"按金", "保证金",
	"沽空", "卖空",
	"沽出", "卖出",
	"對帳", "对账",
	"分類帳", "分类账",
	"帳", "账",
	"半日市", "半日交易",
	"開市", "开盘",
	"收市", "收盘",
	"莊家", "做市商",
	"業績公布", "财报发布",
	"業績", "财报",
	"信貸息差", "信用利差",
	"成份股", "成分股",
	"代號", "代码",
}

var glossaryReplacer = strings.NewReplacer(glossary...)

// converter turns Hong Kong Traditional text into Simplified text.
type converter struct {
	cc *opencc.OpenCC
}

var (
	converterOnce sync.Once
	converterInst *converter
	converterErr  error
)

// newConverter loads the hk2s dictionaries once per process.
func newConverter() (*converter, error) {
	converterOnce.Do(func() {
		cc, err := opencc.New("hk2s")
		if err != nil {
			converterErr = err
			return
		}
		converterInst = &converter{cc: cc}
	})
	return converterInst, converterErr
}

// convert applies the glossary, then the script conversion. Markdown
// structure, links, anchors and code survive unchanged: the dictionaries map
// Chinese characters and phrases only.
func (c *converter) convert(s string) (string, error) {
	return c.cc.Convert(glossaryReplacer.Replace(s))
}

// deriveSimplifiedUI fills zhHans's chrome strings from zhHant's, except the
// two notes under a translated title, which say what the page is: a
// conversion of the Traditional translation, not a translation of its own.
func deriveSimplifiedUI(c *converter) error {
	if zhHans.ui.Documentation != "" {
		return nil
	}
	ui := zhHant.ui
	conv := func(s *string) error {
		out, err := c.convert(*s)
		*s = out
		return err
	}
	for _, field := range []*string{
		&ui.Documentation, &ui.Overview, &ui.OnThisPage, &ui.NotWrittenYet, &ui.DimmedNote,
		&ui.HubLead, &ui.HubDescription, &ui.FooterDocs, &ui.FooterPrivacy, &ui.FooterSecurity,
		&ui.Fineprint, &ui.ReadEnglish,
	} {
		if err := conv(field); err != nil {
			return err
		}
	}
	nav := make([]string, len(ui.Nav))
	for i := range ui.Nav {
		out, err := c.convert(ui.Nav[i])
		if err != nil {
			return err
		}
		nav[i] = out
	}
	ui.Nav = nav
	sections := make(map[string]sectionSpec, len(ui.Sections))
	for slug, section := range ui.Sections {
		title, err := c.convert(section.Title)
		if err != nil {
			return err
		}
		blurb, err := c.convert(section.Blurb)
		if err != nil {
			return err
		}
		sections[slug] = sectionSpec{Slug: slug, Title: title, Blurb: blurb}
	}
	ui.Sections = sections
	ui.Translated = "本页由繁体中文译本自动转换而成；与英文原文不一致时以英文版为准。"
	ui.Stale = "英文原文在本译文之后已有更新，本页内容可能落后。"
	zhHans.ui = ui
	return nil
}

// deriveTranslation builds the Simplified page from a Traditional one. It
// carries the Traditional translation's provenance, so the stale gate judges
// both pages by the same English revision.
func deriveTranslation(c *converter, t *translation) (*translation, error) {
	body, err := c.convert(string(t.body))
	if err != nil {
		return nil, err
	}
	navTitle, err := c.convert(t.navTitle)
	if err != nil {
		return nil, err
	}
	summary, err := c.convert(t.summary)
	if err != nil {
		return nil, err
	}
	description, err := c.convert(t.description)
	if err != nil {
		return nil, err
	}
	return &translation{
		lang:           zhHans,
		source:         translationSource(zhHans, t.page.Source),
		page:           t.page,
		navTitle:       navTitle,
		summary:        summary,
		description:    description,
		sourceRevision: t.sourceRevision,
		stale:          t.stale,
		derivedFrom:    t,
		body:           []byte(body),
	}, nil
}
