---
translation-of: docs/docs/start/your-llm.md
source-revision: f175807d5016642f9d3581051d006d5d1e8ee9b4
nav-title: 接入你慣用的 AI
summary: 把盈透證券帳戶接入 Claude、Cursor 或其他 MCP 主機，在對話中直接查詢持倉和風險。
description: 如何把盈透證券（Interactive Brokers）帳戶接入你慣用的大型語言模型：Claude Desktop、Claude Code、Cursor、Zed 或其他 MCP 主機，帳戶資料會去哪裏，以及可以問甚麼。
---

# 把盈透證券帳戶接入你慣用的 AI {#connect-your-interactive-brokers-account-to-the-llm-you-already-use}

更新：2026-11-05 08:40 CET

Canary 不附帶模型，也不需要模型。它在你的電腦上運行一個小型伺服器，同一部電腦上的任何 MCP 主機都可以啟動它，用的是哪個模型，就看主機怎樣設定。接上之後，你可以在該應用程式裏直接問自己的持倉。

## 你需要甚麼 {#what-you-need}

- 已安裝 Canary，並已連接閘道。詳見[安裝與首次運行](install.md)。
- 同一部電腦上有一個支援 stdio MCP 的桌面或終端機應用程式。

瀏覽器分頁裏的對話做不到這點，因為主機要與這個伺服器在同一部電腦上運行。

## Claude Desktop 和 Claude Code {#claude-desktop-and-claude-code}

**Claude Desktop。** 用 Claude Desktop 打開 [`canary.mcpb`](https://github.com/osauer/canary/releases/latest/download/canary.mcpb)。[Claude Desktop 安裝頁](../../claude-desktop-interactive-brokers/index.html)有逐步說明。

**Claude Code。** 在終端機執行以下指令。外掛程式不包含 `canary` 二進位檔，所以也要安裝它，否則外掛程式雖然裝得上，工具卻無法使用。

```sh
claude plugin marketplace add osauer/canary
claude plugin install canary@canary
```

你所在的地區未必可以使用 Claude：撰寫本文時（2026 年），Anthropic 的[支援地區列表](https://www.anthropic.com/supported-countries)並未包括香港、澳門或中國內地。CLI 和已配對的應用程式不需要 Claude；其他 MCP 主機也可以使用同一套工具，但所用的模型同樣要在你的地區有提供。

## Cursor、Continue、Zed 及其他主機 {#cursor-continue-zed-and-other-hosts}

把以下項目加入主機的 MCP 設定，然後完全退出並重新打開主機：

```json
{
  "mcpServers": {
    "canary": {
      "command": "/ABSOLUTE/PATH/TO/canary",
      "args": ["mcp"]
    }
  }
}
```

`which canary` 會顯示路徑。路徑必須是絕對路徑，因為主機不會搜尋你的 `PATH`。設定檔的位置和外層的鍵名因主機而異，請查看該主機的 MCP 說明文件；當中必須有 `command` 和 `args`。其餘內容可參閱[連接 MCP 主機](hosts.md)，包括如何分辨是連接設定問題還是閘道問題。

## 你的帳戶資料會去哪裏 {#where-your-account-data-goes}

Canary 不用註冊，也不傳送遙測資料，伺服器就在你的電腦上運行。工具的結果會交給主機及其所用的模型，所以如果用雲端模型，你的持倉和結餘會按該供應商的條款傳送給對方。模型在你自己的電腦上運行，就不會有模型供應商收到這些資料，但仍要查看主機本身會傳送甚麼。Canary 亦會按股票代號向 Nasdaq 查詢業績日期，對方可由此推斷你可能持有該股。[私隱說明](https://github.com/osauer/canary/blob/main/PRIVACY.md)列出所有連線。

## 可以問甚麼 {#what-to-ask}

- 「今天有甚麼需要我留意？」Canary 會以每日簡報回答，並指出任何過時或缺失的數據。
- 「我哪些持倉超出了限額？」這會按你的限額讀取規則手冊；如果仍是未經你審閱的 Canary 預設值，也會說明。
- 「按相關資產計，我的風險敞口是多少？」這會讀取按相關資產分組的持倉，與 `canary positions --by underlying` 顯示的相同。
- 「港股現在開市嗎？午市幾點開？」這會讀取官方交易所日曆。
- 「過去一年，我的交易之後價格怎樣走？」設定好[經紀商報表](reporting.md)後，Canary Edge 會從你自己的交易紀錄回答。它只描述過去，不作預測。

## 訂單 {#orders}

MCP 工具只會讀取。在任何版本中，它們都不能預覽或發出訂單。[AI 代理交易](agentic-trading.md)說明代理可以做甚麼，以及訂單如何發出。
