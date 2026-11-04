---
translation-of: docs/docs/start/first-session.md
source-revision: 5c365add89daed36f8045f77db8ba108eb2aa3b6
nav-title: 首次使用
summary: 有指引的半小時：讀取帳戶、執行簡報、查看規則手冊，並解讀回傳的內容。
description: Canary 首次使用的七步導覽：閘道健康、帳戶、持倉、每日簡報、規則手冊與政策讀取、指定代號的技術分析，以及透過代理進行相同的讀取。
---

# 首次使用 {#your-first-session}

更新：2026-11-04 09:23 CET

本導覽只使用唯讀指令。它假設 Canary 已安裝，並且有一個本機 IB Gateway 或 TWS 工作階段可用。[安裝與首次運行](install.md)涵蓋設定。

## 1. 證明授權來源 {#1-prove-the-authority}

```sh
canary status
```

先看已連線的帳戶模式和閘道端點，然後讀取儲存、子系統、數據農場和背景工作的健康狀況。閒置的守護程序是中性的；普通指令會按需啟動它。缺失或過時的來源證據會保持為警告或不可用狀態，而不會變成乾淨的結果。`Executions` 行說明你在 TWS 輸入的成交是否會到達 Canary：只有當 TWS 的 Master API client ID 是 Canary 的客戶端 ID 時才會，詳見[安裝與首次運行](install.md#check-it-worked-canary-status)。

一個就緒的 Gateway 並不會設定 IBKR 結單。在依賴對帳、按結單推算的權益或 Canary Edge 之前，請先完成[設定經紀商報表](reporting.md)。它為這三個使用方建立一個共用的 Activity Flex Query。然後核對獨立的報表通道：

```sh
canary reporting status
```

這會分辨本機憑證、經紀商可達性、報表新鮮度、觀察到的欄位、未回傳的區段，以及回傳但為空、因而尚無法證明其欄位選擇的區段。

## 2. 讀取帳戶和持倉 {#2-read-the-account-and-book}

```sh
canary account
canary positions --by underlying
```

兩項讀取都識別一個選定的帳戶和模式。每個持倉都帶有 IBKR 回報的上市場所。缺失的值保持缺失，而無法解析的多帳戶登入會被拒絕，不會被合併。在依賴總敞口之前，先檢查持倉的新鮮度和 Greeks 覆蓋率。

## 3. 讀取交易台簡報 {#3-read-the-desk-brief}

```sh
canary brief
```

預設檢視先顯示評估的完整度，然後是需要審視的發現、帶日期的背景資料，以及分組的覆蓋缺口。缺失的觀察時間會明確標示；一項保留的發現不會僅因為簡報是現在產生而變成最新。

使用 `canary brief --details` 查看完整的 Review 和 Ready 證據及來源診斷。應用程式提供相同的概覽並可展開詳情。`canary regime --explain` 展開全部八項市場指標、它們的觀察時間、閾值和各自獨立的來源故障。`canary stress --details` 展開組合風險證據和診斷。JSON 和 MCP 保留完整的型別化證據；精簡的人類可讀檢視不會改變風險決策。

## 4. 查看規則手冊 {#4-inspect-the-rulebook}

```sh
canary rules
canary policy show
canary recon show
```

`rules` 回報最嚴重的諮詢性發現和每個未知的輸入。`policy show` 識別已核准的風險憲章。`recon show` 把保留的經紀商結單與已申報的資本分類帳比較。讀取指令不會確認、覆寫、駁回或更改任何狀態。

## 5. 查看目前的工作 {#5-inspect-current-work}

```sh
canary proposals list
canary opportunities list
canary orders open
```

建議（proposals）是只限平倉或減倉的保護候選項目，以及在你啟用時現金清掃的國庫券買入。機會（opportunities）是期權行使的候選項目。訂單讀取查看本機的生命週期日誌。這些記錄是證據，不是向經紀商寫入的授權；任何動作都需要獨立的交易版本、一份新的精確審視契約、守護程序重新核證，以及針對該筆交易的明確指示。

## 6. 分析一個明確指定的代號 {#6-analyze-an-explicitly-named-symbol}

```sh
canary technical SPY,QQQ
```

技術分析讀取為指定的股票或 ETF 批量整理每日趨勢、相對強度、ATR 和流動性證據。它會回報已退化的歷史數據，而不會無聲地為不完整的行排名。

## 7. 透過 MCP 主機提問 {#7-ask-through-an-mcp-host}

隨附的 MCP 伺服器是唯讀的。直接提出問題，而不是指定工具：

| 提問 | 通常使用的工具 |
| --- | --- |
| 「今天有甚麼需要注意？」 | `canary_brief` |
| 「我的帳戶情況如何？」 | `canary_account`，然後 `canary_positions` |
| 「我按標的資產的敞口是多少？」 | `canary_positions` |
| 「經紀商報表是否已為對帳和 Edge 準備好？」 | `canary_reporting` |
| 「我過去的股票、ETF 和期權活動實際帶來了甚麼？」 | `canary_edge` |
| 「我過去的哪些建倉、加倉、減倉或平倉在 20 個交易日內的價格影響最大？」 | `canary_edge` |
| 「規則手冊的哪些輸入是未知的？」 | `canary_rules` |
| 「有沒有保護或行使的候選項目？」 | `canary_proposals` 或 `canary_opportunities` |
| 「為甚麼一張本機訂單仍然未完成？」 | `canary_orders_open`，然後 `canary_order_status` |

MCP 沒有資源訂閱、預覽工具、設定寫入、治理寫入或向經紀商寫入的工具。 [與代理協作](../operate/agents.md)有主機設定和證據規則。

## 下一步去哪裏 {#where-to-go-next}

- 如果按結單支援的行缺失或仍需要第一份報表，看[設定經紀商報表](reporting.md)。
- [每日交易台](../operate/daily-desk.md)了解每日的工作流程。
- [訂單與交易安全](../operate/orders.md)了解受約束的審視邊界。
- [感測器](../understand/sensors.md)了解數據類型、新鮮度、來源健康、最後良好值和警告的語義。
- [更新](updating.md)了解穩定主要版本的更新行為。
