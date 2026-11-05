---
translation-of: docs/docs/start/agentic-trading.md
source-revision: 88efceda3ccfc037268cedbe9bb9e308489cc3b0
nav-title: AI 代理交易
summary: AI 代理在盈透證券帳戶上可以做甚麼、不可以做甚麼，以及一張訂單要過哪些關才會發出。
description: 讓 AI 代理協助看管盈透證券（Interactive Brokers）帳戶：代理可以讀取甚麼、如何全日留意市場，以及落單前需要交易版本、Canary 的檢查和你對該筆交易的指示。
---

# 以 AI 代理在盈透證券帳戶交易 {#agentic-trading-on-an-interactive-brokers-account}

更新：2026-11-05 08:40 CET

Canary 讓 AI 代理協助處理你的盈透證券（Interactive Brokers）帳戶，而代理發出的每張訂單仍由你決定。代理可以讀取你的持倉和市場數據，如果主機能定時喚醒它，還可以全日留意，並向你提出交易構思。要真正落單，需要交易版本和 Canary 的檢查，而代理必須先有你對該筆交易的指示。

## 標準版本下代理可以做甚麼 {#what-an-agent-can-do-with-the-standard-build}

- 透過 MCP 工具讀取每日簡報、持倉、風險敞口、投資組合壓力測試、規則手冊和訂單紀錄。結果會說明數字的來源和時效，數據缺失或過時都會標明。
- 按官方交易所日曆安排工作，包括香港和東京的午休時段。
- 用 `canary mcp --profile monitor` 做低成本的定時檢查，這個設定只開放簡報和狀態兩個工具。
- 讀取 Canary 草擬的保護方案，並向你解釋。
- 擬定一筆交易供你審閱，並列出所用的依據。這只是計劃，未發出之前不算訂單。

以上都不能落單。標準版本根本沒有落單的程式碼，而 MCP 在任何版本都沒有落單工具。

## 落單需要甚麼 {#what-it-takes-to-send-an-order}

1. 下載交易版本。它是獨立的下載項目 `canary-trading`，屬實驗性質，按現狀提供。
2. 把 `[trading].mode` 設為 `paper`，並在設定中指定帳戶和客戶端 ID。先用模擬帳戶。
3. 檢視風險政策中 `[order_limits]` 的每張訂單限額。Canary 會預先寫入起始限額，在你修改並提高 `policy_version` 之前，一直以這些限額為準。缺少必要項目時，Canary 會拒絕所有訂單預覽。
4. 在最新一則訊息中，明確指示代理落那一張單。方案、計劃或較早前的指示都不算數。
5. 代理使用設有關卡的 CLI：先預覽，由經紀商試算一次（WhatIf），然後才落單。Canary 會檢查你的限額，以及帳戶和客戶端 ID 是否與設定相符，並在送交經紀商之前，記錄這個請求來自代理。

Canary 看不到你的對話，所以這項指示是代理必須遵守的規則，並非 Canary 執行的檢查。交易版本一旦可以落單，能執行 shell 的代理不問你也可以運行 CLI。它發出的訂單仍要通過 Canary 的檢查，所以只應把這個權限交給你信任的代理。

如果安裝了 Claude Code 外掛程式，還有一個 Hook（攔截腳本）會檢查代理執行的每個落單指令：除非 `canary trading status` 顯示可以落單，否則一律攔下；與其他指令串連的落單指令也會被拒絕。代理亦不能經此更改規則手冊限額或安全設定。所有訂單類型和檢查，可參閱[設有關卡的訂單與交易版本](../operate/orders.md)。

MCP 沒有可以更改限額的工具，更改限額的指令只供你本人使用。

## 不經代理的訂單 {#orders-that-run-without-an-agent}

在交易版本中，你可以在自己編輯的政策裏，預先授權部分平倉和減倉操作，以及現金清掃的國庫券訂單。單靠這份清單不會發出任何訂單：要在已配對的裝置上簽署常設授權之後，守護程序才會發出，而目前只有 Canary Desk 可以簽署。過程不涉及代理。安裝 Canary 不會開啟這項功能。詳情可參閱[保護與減低風險](../operate/protection.md)。

## 建立你自己的代理 {#building-your-own-agent}

把 `canary mcp` 作為代理的子程序運行，讓它自行發現工具。喚醒時間、開支上限和模型上下文由你的程式碼負責。經紀商連線、市場數據、風險計算和訂單狀態由 Canary 負責，所以應重用它的結果，不要在提示詞中重新計算。新聞和其他外來文字只能當作證據，當中的指示不可照做。詳情可參閱[持續運行的代理](hosts.md#continuously-running-agents)。

## 香港及新加坡帳戶 {#accounts-in-hong-kong-and-singapore}

基礎貨幣為港元或新加坡元的帳戶，可以預覽以該貨幣計價的訂單。涉及港元或新加坡元的跨貨幣訂單會被預覽拒絕，並說明原因。其餘內容可參閱[香港及新加坡指南](ibkr-hong-kong-singapore.md)。
