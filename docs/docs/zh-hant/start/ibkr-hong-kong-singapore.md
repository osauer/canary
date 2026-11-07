---
translation-of: docs/docs/start/ibkr-hong-kong-singapore.md
source-revision: f6712a54bf113ab00946fb3984075f049c60f86b
nav-title: 香港及新加坡帳戶
summary: 相同的閘道步驟、以港元、新加坡元或日圓計的基礎貨幣、亞洲交易所日曆，以及何時執行簡報。
description: 盈透證券（Interactive Brokers）帳戶設於香港、新加坡、日本或澳洲，持有港股或其他亞洲股票時有何不同：IB Gateway 步驟完全相同、港元、新加坡元與日圓數字如何顯示、港交所、新交所、東京、ASX 與滬深港通的日曆，以及亞洲早上的簡報時段。
---

# 以盈透證券（Interactive Brokers）香港或新加坡帳戶使用 Canary {#canary-with-an-interactive-brokers-hong-kong-or-singapore-account}

更新：2026-11-07 09:15 CET

Canary 透過你自己的 IB Gateway 或 TWS 工作階段，讀取一個盈透證券（Interactive Brokers）帳戶，包括持有港股的帳戶。帳戶由哪個 IBKR 實體持有，無論是 Interactive Brokers 香港、新加坡、日本、澳洲、印度、LLC、愛爾蘭或英國，都不會改變這個連線：API、通訊埠和客戶端 ID 在各地都相同，所以安裝步驟就是[安裝與首次運行](install.md) 所列的那些。守護程序（daemon）無法從閘道分辨實體，也不需要分辨。本頁說明當帳戶的基礎貨幣是港元、新加坡元或日圓，而股票在港交所、新交所、東京證券交易所或 ASX 掛牌時，有哪些地方不同。

## 閘道步驟：完全相同 {#gateway-steps-identical}

- 安裝 IB Gateway 10.37 或更新版本，或 TWS，登入帳戶，並在 Global Configuration → API → Settings 下啟用 API socket 存取。模擬帳戶與真實帳戶都可以。閘道的登入地區並不重要。
- 以[選擇安裝方式](install.md#pick-an-install-path)中的其中一種安裝 Canary，然後執行 `canary status`。探測會像在其他地方一樣，檢查本機的 4001、4002、 7496 和 7497 通訊埠。
- 需要 IBKR Pro 帳戶；IBKR Lite 沒有 API 存取。香港、新加坡和東京上市證券的市場數據，只有在你的帳戶已訂閱該交易所時才是即時的；否則 Canary 會顯示經紀商的延遲數據並加以標示，詳見[市場數據](../understand/market-data.md)。

## 基礎貨幣：按經紀商所報的方式顯示 {#base-currency-shown-as-the-broker-reports-it}

每個帳戶數字都以帳戶的基礎貨幣顯示，用 IBKR 所命名的貨幣，而每一行持倉都帶有自己的貨幣。以下數字皆為虛構。

```text
Account  U1234567 · base=HKD · live
────────────────────────────────────────────
  Net liquidation         HKD 1,842,310.55
  Daily P&L               HKD -4,120.00
```

```text
  LEG                           QTY         AVG        MARK  CHANGE / GREEKS             MKT VALUE     UNREAL P&L
  700
  Stock                      500 sh  HKD 340.40  HKD 388.20  +HKD 1,100.00 (+0.57%)  HKD 194,100.00  HKD 23,900.00
  D05
  Stock                     1000 sh   SGD 41.10   SGD 43.75  -SGD 250.00 (-0.57%)    SGD 43,750.00   SGD 2,650.00
  7203
  Stock                      300 sh     ¥ 2,810     ¥ 2,955  -¥ 9,000 (-1.01%)           ¥ 886,500       ¥ 43,500
```

IBKR 的代號會省去港股編號前面的零，所以 00700（0700.HK）會顯示為 `700`。

港元和新加坡元以 ISO 代碼書寫，日圓以其符號書寫，且永不帶小數單位：以日圓為基礎貨幣的簡報會顯示 `session P&L ok ¥ 15,000,000 · day -¥ 82,500`。配對應用程式會按你裝置的地區設定格式化金額，所以同一個港元數字在那裏可能顯示為 `HK$1,842,310.55`。非基礎貨幣的價值會按經紀商分類帳所報的匯率換算，顯示在 `market_value_base` 和帳戶的貨幣敞口行。

## 查詢港股或新加坡股票 {#looking-up-a-hong-kong-or-singapore-share}

查詢股票時請指明市場。Canary 會把沒有指明市場的代號當作美股，除非你的帳戶已持有該股票。如未持有，`canary technical 700` 會查詢一隻代號為 700 的美股，而盈透證券並無此股票；若代號同時是美股代號，則會得到該美國公司的資料。

```sh
canary technical 700,5,1299,9988 --market hk
canary technical D05,Z74,ES3 --market sg
canary market --symbol 700 --exchange SEHK --currency HKD --range 1M
canary market --symbol D05 --exchange SGX --currency SGD --range 1M
```

在 `canary technical`，`--market hk` 等同 `--exchange SEHK --currency HKD`，`--market sg` 等同 `--exchange SGX --currency SGD`。`canary market` 則使用完整寫法。

盈透證券的自動傳送系統 SMART 拒絕了我們試過的所有 SEHK 和 SGX 上市股份，所以 `--exchange SMART` 會顯示錯誤 `no security definition`。Canary 會直接在 SEHK 或 SGX 讀取這些股票。`--exchange SEHK --currency USD` 也會顯示相同錯誤，因為 700 並無以美元掛牌。東京、悉尼和歐洲的上市股份則可經 SMART 查詢。

相對強度是股票相對基準的走勢，會以股票本身貨幣計算的回報，與美元計價的 SPY 回報比較（可用 `--benchmark` 更改），所以數字包含匯率變動。

## 訂單 {#orders}

SEHK 或 SGX 股份的訂單會送往該股份本身的交易所，因為盈透證券不會經 SMART 為這些上市股份傳送訂單。預覽會列明交易所，並按該市場的交易時段（包括午休）判斷。盈透證券的 Adaptive 演算法只在 SMART 上運作，所以這些訂單以普通限價單傳送：減倉訂單會在條款中說明，手動要求 Adaptive 則會被拒絕並列明原因。其餘事項，例如每手股數和最小價格變動，由盈透證券在預覽回覆中決定。確認前請先閱讀該回覆。

Canary 會以 IDEALPRO（盈透證券的外匯市場）的即時報價，衡量跨貨幣訂單是否超出每筆訂單上限。港元有與 EUR、GBP、AUD、USD、CAD、JPY 和 SGD 的貨幣對，新加坡元有與 EUR、GBP、AUD、USD、JPY 和 HKD 的貨幣對。盈透證券沒有港元或新加坡元對 NZD 或 CHF 的貨幣對，也沒有新加坡元對 CAD 的貨幣對，所以需要這些貨幣對的訂單會被拒絕，預覽會列明原因。Canary 讀取這些貨幣對只為檢查訂單，不會替你兌換港元或新加坡元：貨幣兌換只涵蓋 EUR、GBP、AUD、NZD、USD、CAD、CHF 和 JPY。

## 交易所日曆與簡報的交易時段 {#exchange-calendars-and-the-briefs-session}

`canary calendar` 內置 Canary 所嵌入的官方交易時段日曆，每個都帶有午休、半日市和涵蓋期間：

| `--market` | 交易所 | 本地交易時段 | 涵蓋期間 |
| --- | --- | --- | --- |
| `hk` | 香港現貨股票 | 09:30–12:00、13:00–16:00 HKT | 2026–2027 |
| `sg` | 新加坡證券 | 09:00–12:00、13:00–17:00 SGT | 2026–2027 |
| `jp` | 東京現貨股票 | 09:00–11:30、12:30–15:30 JST | 2026–2027 |
| `au` | ASX 現貨股票 | 10:00–16:00 悉尼時間 | 2026–2027 |
| `cn` | 中國內地 A 股交易時段 | 09:30–11:30、13:00–15:00 CST | 2026 |

```sh
canary calendar --market hk --days 5
canary calendar --market sg --date 2026-02-16
```

香港已不再因颱風休市：自 2024 年 9 月 23 日起，港交所在八號或以上颱風信號、黑色暴雨警告或極端情況下照常交易，所以日曆會把這些日子顯示為正常交易時段。

涵蓋期間以外的日期顯示 `unknown`，永不按星期幾猜測。 [市場日曆](../understand/concepts.md#market-calendars)列出 Canary 嵌入的每個日曆，以及各自涵蓋期間的確實結束日期。A 股透過滬深港通北向進入 IBKR 帳戶，只在香港與內地同時開市的日子交易；在接近假期的日子依賴某個日期之前，請先查閱港交所的滬深港通日曆。

簡報的交易時段行跟隨你的持倉。守護程序讀取持有你股票及期權價值最大份額的交易所的交易時段，所以一個由港交所和新交所股票組成的帳戶，在香港的早上會顯示 `session ok hk_hkex · regular`，而 `since the last close` 和 `off-session marks` 這些字眼都是指該交易所。主要在美國市場的持倉則一如以往維持美國交易時段。

## 亞洲早上的簡報 {#the-brief-at-an-asian-morning}

在你的交易所開市前執行簡報，正如[每日交易台](../operate/daily-desk.md)在美股開市前所做的一樣：

| 交易所 | 開市（本地時間） | 簡報執行時限 |
| --- | --- | --- |
| 新交所 | 09:00 SGT | 08:45 SGT |
| 香港 | 09:30 HKT | 09:15 HKT |
| 東京 | 09:00 JST | 08:45 JST |
| ASX | 10:00 AEDT 或 AEST | 09:45 悉尼時間 |

在那個時刻，美國交易時段已於數小時前收市，所以描述美國的行仍然是美國的行：市場狀態（regime）判讀（來自美國數據源的信貸息差、波動率和利率）、標普 500 市場廣度和 SPX 莊家 Gamma，都是關於剛收市的那個市場的背景資料，而不是你的交易所的狀態。需要業績公布日期的規則，對業績數據供應商未涵蓋的上市證券會顯示 `unknown`，永不顯示 `pass`。組合壓力、集中度、保護覆蓋，以及規則手冊的規模與敞口規則，在任何上市證券上都以你的基礎貨幣運作。

閘道的隔夜診斷遵循同一套日曆。預設情況下，閘道連線中斷在美國、Xetra、倫敦、東京和香港的交易時段內計為警告；持有新交所、ASX 或滬深港通的持倉會自動擴闊這些時數，而[設定參考](../reference/config.md)中的 `[daemon].log_markets` 可以直接宣告它們：

```toml
[daemon]
log_markets = ["hk_hkex", "sg_sgx"]
```

## 結單與對帳 {#statements-and-reconciliation}

[設定經紀商報表](reporting.md)中的 Activity Flex Query 對每個 IBKR 實體都相同；對帳、按結單推算的權益和 Canary Edge 都以你的基礎貨幣讀取它。查詢的區段和欄位不取決於帳戶所在的地區。

## 配合 AI 主機使用 {#using-it-with-an-ai-host}

`canary mcp` 在這些帳戶上的運作方式完全相同，可配合你電腦上任何 MCP 主機使用：見[把盈透證券帳戶接入你慣用的 AI](your-llm.md)及[AI 代理交易](agentic-trading.md)。

你所在的地區未必可以使用 Claude：撰寫本文時（2026 年），Anthropic 的[支援地區列表](https://www.anthropic.com/supported-countries)並未包括香港、澳門或中國內地。CLI 和已配對的應用程式不需要 Claude；其他 MCP 主機也可以使用同一套工具，但所用的模型同樣要在你的地區有提供。

## 下一步 {#next}

[首次使用](first-session.md)逐步介紹各項讀取；[每日交易台](../operate/daily-desk.md) 把它們按一個交易日排序。當某個字眼或數字對你的市場而言顯得不對，請在 GitHub 開一個 issue，並註明交易所和基礎貨幣；日曆和顯示器就是這樣改進的。
