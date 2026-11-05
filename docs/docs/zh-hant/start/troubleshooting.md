---
translation-of: docs/docs/start/troubleshooting.md
source-revision: 878809c383cfb140aa2d8e0af56d5d6830e80b03
nav-title: 故障排除
summary: 閘道無法連接、報價過時、守護程序無法啟動，以及解答每個問題的日誌。
description: Canary 以症狀為先的修復方法：閘道探測和握手錯誤、無法啟動的守護程序、延遲或凍結的報價、市場廣度和 Gamma 的狀態、MCP 主機接線，以及日誌的位置。
---

# 故障排除 {#troubleshooting}

更新：2026-11-04 09:23 CET

症狀、原因、修復。先從 `canary status` 開始：它印出四種判斷之一（`READY`、`STARTING`、`ATTENTION`、`OFFLINE`），最多等待 25 秒讓 API 握手完成，並在閘道未連線時以 1 退出。對於缺失或退化的輸入，執行 `canary data health`。

## `Gateway setup` 行說某項設定不同 {#the-gateway-setup-row-says-a-setting-differs}

每次連線後數秒，守護程序會在一個獨立的短暫連線上讀取一次應用程式的 API 設定（與下文 `--gateway-diagnostics` 相同的讀取），而 `canary status` 把結果印為 `Gateway setup` 行。`as expected` 表示 Canary 依賴的每項設定都符合所需。否則該行會為每個差異帶一個短句，各自按 TWS 或 IB Gateway 的標籤命名該設定，並說明它對你的代價：

```text
Gateway setup  Master API client ID is not 15: orders and fills from TWS or
               other apps stay unseen. · read 08:36
               Open orders are not downloaded at connect: orders placed
               elsewhere stay unseen.
```

它檢查的設定，除另有說明外都在 Configure → Settings → API → Settings： `Read-Only API`（當 `[trading].mode` 是 paper 或 live 時必須關閉）、`Master API client ID`（Canary 的客戶端 ID）、`Download open orders on connection` （開啟）、`Reject messages above maximum allowed message rate`（關閉：開啟時三次拒絕就會切斷工作階段）、守護程序撥接迴環位址時的受信任 IP 清單（`Allow connections from localhost only` 開啟，或 `127.0.0.1` 受信任；否則每次重新連線都要等待一次點擊）、API 日誌等級（`Detail` 增長很快）和 `Include market data in API log file`（關閉），以及 Lock and Exit 下的每日 `Auto restart`（登出會讓 Gateway 處於已登出狀態）。經紀商沒有回報的設定不算差異。`not read yet` 表示讀取尚未完成或無法進行；原因跟在後面。Canary 永不更改設定：請在應用程式中修正，該行會在下一次連線或 `canary status --gateway-diagnostics` 時更新。

IB Gateway 以加密方式寫入其日誌和設定檔（`*.ibgzenc`），而 IBKR 沒有記載任何關閉此功能的設定。純文字副本來自 Gateway 的 File 選單（`API Logs` 或 `Gateway Logs` → `Export Today Logs…`）。`Automatic Encryption` 選單項目與日誌無關：它關乎 FIX CTCI 連線，在 API 模式下沒有作用。

## 更改 TWS 或 Gateway 後檢查 API 設定 {#check-api-settings-after-changing-tws-or-gateway}

執行 `canary status --gateway-diagnostics`（加上 `--json` 取得所有欄位）。正常連線維持其已測試的協定上限 203。診斷會對同一端點開啟一個獨立連線，協商至最高 219，讀取 API 設定，並在五秒內關閉。它不會重啟 TWS、更改通訊埠、更新設定、綁定訂單或向經紀商提交指示。

`observed` 表示經紀商回傳了設定。`unsupported` 表示該工作階段無法協商所需的設定讀取協定；請在 TWS 或 Gateway 中手動檢查設定。`unavailable` 或 `rejected` 帶有原因。省略的設定表示**未回報**，而不是關閉。協商得出的協定不是已安裝應用程式的版本號。

輸出包括唯讀 API 模式、自動下載未完成訂單、重新連線時的訂單保留、美股以手數計算成交量的模式、速率限制拒絕、主 API 客戶端 ID、僅限本機模式及迴環位址是否受信任（受信任 IP 清單本身永不保留）、API 訊息日誌及其等級、已開啟的訂單預防措施繞過，以及應用程式所回報的 Lock and Exit 排程（按其自己的時鐘；經紀商不發送時區）。這些都是觀察，不是更改，也不是交易許可。成交量值保留經紀商的小數數量；Canary 不會按數值大小猜測是股數還是手數。保留的經紀商訂單按身份對帳；不確定的發送永不自動重放。

切換應用程式時，請把 Session 端點與所選應用程式中的 socket 通訊埠比較。明確的 `[gateway].port` 固定值仍然具約束力：TWS 通常使用 7496/7497， Gateway 使用 4001/4002。在目標應用程式的登入、API socket 和設定準備好之前，保持使用正常運作的端點。設定診斷不會建立重啟時區，也不會更改操作者宣告的重啟排程。

## Gateway 已就緒，但對帳或 Edge 沒有結單證據 {#gateway-is-ready-but-recon-or-edge-has-no-statement-evidence}

Gateway API 存取與經紀商報表是分開的。新安裝可以成功連線，而每個按結單支援的功能仍然是空的。請完成[設定經紀商報表](reporting.md)：建立八個區段的 XML Activity Flex Query，啟用 Flex Web Service，並設定其 Query ID 和權杖檔案。從 `canary reporting status` 開始診斷；它分辨本機檔案、經紀商可達性、保留的證據、缺失的區段、存在但為空的區段，以及已證明缺失的欄位。

用穩定的原因選擇下一步：

| 原因或狀態 | 該做甚麼 |
| --- | --- |
| `backfilling`、`report_not_ready`、`service_busy`、`response_invalid` | 不要更改查詢，讓 Canary 重試。IBKR 正在準備報表，或回傳了 Canary 無法使用的暫時性回應。 |
| `token_missing`、`token_invalid`、`token_expired` | 更換受保護的權杖檔案；不要把權杖放進 TOML 或 shell 參數。 |
| `query_missing`、`query_invalid` | 檢查已設定的 Query ID，以及完全相同的帳戶選擇在 Client Portal 中仍然顯示該查詢。 |
| `service_inactive`、`ip_restricted` | 在 Client Portal 中重新啟用 Flex Web Service 或修正其 IP 限制。 |
| `absent_sections_unproved` | 在已儲存的查詢中開啟每個指名的區段，選擇所回報的詳細程度和 `Select All`，然後儲存。Canary 會自動核證下一份報表。如果沒有相符的活動，IBKR 可能省略已啟用但為空的區段。 |
| `action_required`、`flex_query_incomplete` | 一個真實的行證明了欄位缺失。編輯指名的區段，選擇 `Select All`，然後儲存。Canary 會自動核證下一份報表；新的 Query ID 是可選的。 |
| 經紀商代碼 `1025` | IBKR 沒有記載此代碼。Canary 只請求以美國交易日結束的範圍，因為曾觀察到以星期六結束的範圍回傳此代碼。如果它在交易日的檢查中出現，請檢視 Flex Web Service/查詢設定；持續出現請聯絡 IBKR。不要按猜測的含義更換正常運作的查詢。 |

缺失的區段是沒有回傳；空的區段是回傳了零行。兩者都不能證明選擇了哪些欄位。永遠不要僅為填充報表區段而落單或調動現金。

## 每個指令都印出指向 `canary status` 的提示 {#every-command-prints-a-hint-pointing-at-canary-status}

一個指令失敗了，而 CLI 附加了這段：

```
  hint: run `canary status` to see whether the daemon is still
        connecting (retry in a few seconds) or the gateway is
        down (start IB Gateway; check ~/.local/state/ibkr/ibkr-daemon.log).
```

每當守護程序的錯誤包含 `gateway_unavailable`，CLI 就會加上這三行。它表示守護程序已啟動並在回應，而它背後的經紀商連線沒有。

`canary status` 分辨這兩種情況。`STARTING` 表示守護程序尚未發佈連線錯誤，所以握手仍在進行中；等幾秒後重新執行。`OFFLINE` 表示它已放棄並發佈了原因，`Last attempt` 行會原文印出。

## `no IBKR listener found on 127.0.0.1 ports [4001 4002 7496 7497]` {#no-ibkr-listener-found-on-127001-ports-4001-4002-7496-7497}

在每個通訊埠 200 毫秒的預算內，四個標準通訊埠（4001 Gateway 真實、4002 Gateway 模擬、7496 TWS 真實、7497 TWS 模擬）都沒有回應 TCP 探測。訊息的其餘部分告訴你屬於哪種情況。

如果有 IBKR 應用程式在運行，錯誤會指出它和它的 PID。對於 TWS 和 IBKR Desktop，它列出從程序外部無法分辨的三個原因：Global Configuration → API → Settings 下的 `'Enable ActiveX and Socket Clients'` 未勾選、尚未完成的登入（2FA 或日終對話框），或非預設的 Socket 通訊埠。IB Gateway 沒有這個勾選框——它的 API 永遠開啟——所以對 Gateway 訊息只指出餘下的兩個：它仍在啟動（通訊埠在登入完成後才開啟），或者它在非預設的 Socket 通訊埠上監聽。那種情況請在 `~/.config/ibkr/config.toml` 的 `[gateway]` 下固定通訊埠。

如果沒有應用程式在運行，訊息會這樣說，並以 `start one and Canary will reconnect automatically` 結尾。它確實會。

探測只檢查你設定中的主機，預設為 `127.0.0.1`。在另一部機器上的閘道需要明確設定 `host`；它永遠不會被探測找到。

## `accepts connections on 127.0.0.1:4001 and resets them before the API handshake` {#accepts-connections-on-1270014001-and-resets-them-before-the-api-handshake}

通訊埠是開啟的，但擁有它的程序在 API 握手交換任何一個位元組之前就結束每個連線。探測的連線在 Canary 發送任何東西之前被關閉（FIN）或重設（RST），而守護程序自己的連線嘗試會在撥接時失敗（`connection reset by peer`）、在第一個 socket 選項時失敗（`failed to set TCP_NODELAY: ... invalid argument`）或在寫入版本描述符時失敗（`broken pipe`），取決於重設落在哪一步。Canary 把這些都回報為一種狀態：`canary status --json` 在 `gateway_phase` 顯示 `port_rejecting`，證據在 `port_rejection` 下，文字的 `TWS` 行陳述這個事實，而守護程序日誌在每次變化時記錄一次判斷，而不是每個週期一個不同的 socket 錯誤。

訊息會指出擁有該通訊埠的應用程式以及要在其中核對甚麼。對於沒有 API 開關的 IB Gateway：登入和初始化已完成（2FA、每日自動重啟、「existing session」對話框）、Gateway 視窗中沒有等待中的連入連線提示，以及在 Configure → Settings → API → Settings 和 Precautions 下，Trusted IPs 涵蓋守護程序的位址、其動作未設為拒絕，且 Socket 通訊埠是被探測的那個。Gateway 前方的作業系統應用程式防火牆可以在 Gateway 看到連線之前，以同樣方式重設已接受的連線（macOS：System Settings → Network → Firewall）。對於 TWS，`'Enable ActiveX and Socket Clients'` 勾選框列在首位。

一個拒絕的通訊埠仍然是候選。當另一個監聽器保持其連線時——例如一個就緒的 TWS 旁邊有一個會中斷的 Gateway——守護程序會先嘗試那一個。

## `not responding to TWS handshake within 12s` {#not-responding-to-tws-handshake-within-12s}

完整文字是 `gateway HOST:PORT not responding to TWS handshake within 12s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on`。TCP 已連線而 API 握手沒有得到回覆，這幾乎總是表示 API socket 被停用而不是不存在。

握手進行中時，`canary status` 印出相同的檢查清單：

```
    Configure → Settings → API → Settings → 'Enable ActiveX and Socket Clients'
    Trusted IPs include 127.0.0.1 (or empty)
    Login fully completed (not paused at 2FA)
```

一則相關訊息 `none of N discovered endpoint(s) completed TWS handshake (tried ...)` 表示有幾個應用程式接受了 TCP 而沒有一個完成握手。它列出嘗試過的每個端點，通常是一個過時的 Gateway 視窗連同一個剛登入的 TWS。退出你不想要的那個。

## `daemon socket did not appear within 5s` {#daemon-socket-did-not-appear-within-5s}

當 socket 不存在時，CLI 會自動啟動 `canary daemon`，並給它 5 秒的預算到達其接受迴圈。此訊息表示它沒有做到，所以守護程序在啟動期間崩潰或卡住了。

錯誤帶有 CLI 能查明的資訊。當鎖定持有者仍在運行時，它加上 `daemon PID N holds PATH but never opened the socket` 和 `if it's stuck, run: kill N`。當守護程序日誌的最後一行可讀時，它附上 `last daemon log: ...`。在終止任何東西之前，先閱讀 `~/.local/state/ibkr/ibkr-daemon.log` 的完整日誌。

如果守護程序日誌以關於 `daemon authority`、`daemon.db` 或預覽金鑰的 `start:` 行結尾，表示啟動完整性檢查失敗，守護程序刻意保持停止；請依照 [從失敗的啟動檢查中復原](../internals/storage.md#recover-from-a-failed-startup-check)。

## 守護程序在啟動時因設定錯誤而停止 {#the-daemon-stops-at-start-with-a-config-error}

守護程序日誌或 `canary daemon` 的輸出顯示 `config ...: ... The [gateway] pins and [trading].mode decide which broker account Canary acts on`。 Canary 無法讀取決定經紀商帳戶的設定：檔案本身、某個 `[gateway]` 固定值（`host`、`port`、`account`、`client_id`、`tls`）或 `[trading].mode`。當 `[gateway]` 或 `[trading]` 帶有它不認識的鍵時它也會停止，因為那可能是拼錯的固定值，或者固定值的名稱放在了另一個區段。訊息會指出鍵和行號。在 `~/.config/ibkr/config.toml` 中修正並執行 `canary restart`。

檔案中任何其他無法讀取的部分永遠不會停止守護程序。它以 Canary 對該部分的預設值運行並如此說明：`canary status` 顯示 `config:degraded` 並列出鍵，簡報帶有一行 `config`，警報收件匣有一則「Config file needs attention」通知。當 `[trading]`、`[auto_trade]` 或 `[rulebook]` 以預設值運行時，預先授權的保護提交會暫停；手動平倉、減倉和讀取繼續。修正檔案並執行 `canary restart` 以清除。

## 第二個守護程序無法啟動 {#a-second-daemon-will-not-start}

這是刻意的。守護程序在觸碰閘道之前，會對 socket 旁邊的 `ibkr.lock` 取得非阻塞的獨佔 `flock`。第二個守護程序發現鎖被佔用，記錄 `Another daemon is already running for socket PATH; exiting cleanly`，並以 0 退出。

崩潰的守護程序遺留的 socket 檔案也不是問題。新的守護程序會撥接探測它：沒有回應表示過時，會自動移除。鎖被持有而仍有即時回應是不可能的情況，它會以 `socket PATH already serving despite holding lock; refusing to evict` 大聲失敗。

要刻意替換運行中的守護程序，使用 `canary restart`。它停止舊程序，從目前的二進位檔啟動新程序，並回報閘道健康狀況。

## `CLI version X does not match daemon version Y` {#cli-version-x-does-not-match-daemon-version-y}

完整警告：

```
Canary: warning: CLI version X does not match daemon version Y — run `canary restart` to pick up the new binary.
```

你安裝了新的二進位檔，而舊的守護程序仍駐留。執行 `canary restart`。

此檢查對 `canary status` 跳過，而當任何一方標記為 `dev` 時它保持沉默，所以工作樹不會每次運行都對自己發出警告。

## 報價顯示 `delayed`、`frozen` 或 `delayed-frozen` {#quotes-say-delayed-frozen-or-delayed-frozen}

通常是正確的行為，不是故障。市場數據在你的 IBKR 市場數據訂閱涵蓋的地方是即時的，在沒有涵蓋的地方則是延遲的。

| `DATA` 值 | 含義 |
| --- | --- |
| `live` | 即時報價；你的訂閱涵蓋該代號 |
| `delayed` | `15-20 min delayed quotes (entitlement-limited)` |
| `frozen` | `markets closed; only the last-recorded quote is available` |
| `delayed-frozen` | `markets closed; showing yesterday's close` |

守護程序在每次連線時刻意向閘道請求市場數據類型 2（支援凍結）：類型 1（純即時）在收市時可能讓快照請求懸而未決，而類型 2 會改為回傳最後已知的價格。如果你預期某個代號是 `live` 卻得到 `delayed`，修復方法是在 IBKR 的帳戶管理中訂閱市場數據，而不是在這裏更改。

閘道在凍結模式下只發送一次快照，永不串流。在 v3 中，請查看相關的持倉或應用程式行及其交易時段背景；獨立的報價監看指令已經停用。

## 持有的名稱在隔夜閘道重設後停止更新 {#a-held-name-stopped-updating-after-an-overnight-gateway-reset}

IBKR 的每夜重設可以讓閘道在一段時間內對所有東西回答「no security definition」，而在該視窗內被請求的名稱會被標記為不活躍。這個標記是快取而不是判斷：它只存在於記憶體中，12 小時後過期，並在每次重新連線時從頭重建。

守護程序也會記住那個回答，所以不會每次讀取都再問一次。連接器會暫停重新解析該名稱一分鐘，倍增至 30 分鐘，直到解析成功為止。報價的收市背景保留拒絕 30 分鐘。分配表在經紀商工作階段的餘下時間保留它，而市場廣度掃描會把該名稱排除在外，直到下一個完成的交易時段。

所以修復方法是 `canary restart`。它重啟守護程序並一次清除所有這些；單靠閘道重新連線只會清除不活躍標記和分配備忘。另一個獨立的 30 分鐘重試視窗涵蓋訂閱權限拒絕，那是症狀相同但不同的故障。

## 市場廣度顯示 `0.0 %`，或停留在 `computing` {#breadth-shows-00--or-stays-computing}

冷的或仍在計算的引擎還沒有讀數。使用 `canary status --json` 和每日簡報分辨 `cold`、`computing`、`ready` 和 `degraded`。

一個 `ready` 讀數是某個特定交易時段的收市值。簡報保留該日期，並在它不再是最新完成的交易時段時標記為過時。守護程序繼續提供最後良好值，而不會發佈部分的替代值；`canary status` 帶有子系統的原因和重試狀態。

在新的守護程序上計算很長時間是預期的。IBKR 的歷史數據節流把 503 個名稱的展開限制在每分鐘約 6 個名稱，所以首次構建大約需要 74 分鐘。一次成份股覆蓋率低於 80% 的掃描不會發佈任何東西，每 12 分鐘重試一次，最多 15 次，這可能把等待時間拉長不少。之後狀態退回 `cold`，由正常的每日一次更新接手，在官方交易時段收市後 35 分鐘進行。

## Gamma 顯示 `no data yet (cold cache)` 或 `computing` {#gamma-says-no-data-yet-cold-cache-or-computing}

兩者都是狀態而不是故障，兩者都以 0 退出。只有 `error` 狀態以 1 退出。

冷表示沒有可用的結果，也沒有東西在計算。守護程序在閘道啟動後預熱 Gamma，並在美國期權常規交易時間內，於 15 分鐘的軟性 TTL 之後在已提供的值背後更新。在這些時間以外，自動更新根本不到期，所以非交易時間的冷快取會刻意保持冷，而不是對已收市的市場運行沉重的期權鏈展開。

`canary status --json` 和應用程式會顯示計算中的狀態、進度和型別化的故障，沒有已停用的強制/不等待指令控制。

在美國期權常規交易時間內，SPY 或 SPX 上的 IBKR 354 會為該標的觸發一次延遲重試。成功的結果在文字中顯示 `15m delayed`，在 JSON 中帶有 `data_type: "delayed"`。Canary 只在現貨和期權模型的 tick 共用該延遲時鐘時才接受它；它不會把延遲報價變成即時數據。如果延遲的期權模型 tick 也失敗， Gamma 保持不可用，而錯誤仍會指出 IBKR 354。

## 以 `canary feedback` 回報錯誤 {#report-a-bug-with-canary-feedback}

```bash
canary feedback --surface cli --error "the message you saw"
```

它印出一份簡短的診斷報告和一個連結。連結開啟 GitHub 上的錯誤回報表單，報告已填好；「Where did this happen?」下拉選單是唯一要你自己選的欄位，因為 GitHub 不會從連結填寫下拉選單。在你閱讀表單並提交之前，甚麼都不會發送。 `--surface` 指出它在哪裏發生（`cli`、`mcp`、`app`、`install`），是要你選的選項；`--error` 是要包含的文字。報告帶有 Canary 版本和構建、你的作業系統和架構、守護程序是否回應、閘道種類和 API 伺服器版本、作為來源覆蓋率的數據健康摘要，以及經過遮蔽的錯誤文字：帳戶 ID、餘額、持倉、訂單參考、主機名稱、使用者名稱和家目錄路徑都會被替換為方括號內的字詞。它在沒有守護程序運行時也能運作，而且不會啟動任何守護程序；報告會如此說明。以 ``run `canary feedback` `` 結尾的錯誤訊息是 Canary 的缺陷，不是設定問題，而這就是它要求的步驟。透過 MCP 連接的 AI 助手以 `canary_feedback` 工具得到相同的報告。私密或涉及經紀商敏感資料的背景請改為[以電郵](https://osauer.dev/canary/feedback/#private) 發送，而不是公開的 issue。

## Claude Desktop 沒有列出工具 {#claude-desktop-does-not-list-the-tools}

完全退出 Claude Desktop 再重新開啟。關閉視窗不等於退出，MCP 伺服器只在真正重新啟動時才會重新產生。這也是重新安裝套件後人們遺漏的步驟。

`canary setup claude-desktop` 會把一個指向運行中二進位檔已核證絕對啟動器路徑的 `mcpServers.canary` 項目，寫入 macOS 上的 `~/Library/Application Support/Claude/claude_desktop_config.json`。它會先備份任何現有設定。它拒絕覆寫不是有效 JSON 的檔案，並印出路徑讓你修正或刪除。[連接 MCP 主機](hosts.md)完整涵蓋每個主機的接線。

如果工具仍然沒有出現，閱讀 Claude Desktop 自己的日誌 `~/Library/Logs/Claude/mcp-server-canary.log`。

MCPB 安裝則不同：它帶有自己嵌入的二進位檔，而 `canary update` 不會觸碰它。[更新](updating.md)涵蓋重新安裝套件。

## 日誌和本機檔案在哪裏 {#where-the-logs-and-local-files-are}

| 路徑 | 內容 |
| --- | --- |
| `~/.local/state/ibkr/ibkr-daemon.log` | 守護程序診斷：探測、握手、重新連線、子系統錯誤。以 `CANARY_LOG` 覆寫 |
| `~/Library/Logs/Claude/mcp-server-canary.log` | Claude Desktop 對它所啟動的 MCP 伺服器的觀察 |
| `$XDG_RUNTIME_DIR/ibkr/ibkr.sock`，否則 `~/.cache/ibkr/ibkr.sock` | 守護程序 IPC socket。以 `CANARY_SOCKET` 覆寫 |
| socket 旁邊的 `ibkr.lock` | 單一實例鎖，持有守護程序的 PID |
| `~/.config/ibkr/config.toml` | 可選的持久設定，包括 `[gateway]` 固定值 |

守護程序日誌是診斷串流，不是稽核記錄。開機時一旦超過 64 MiB 就會被移到 `.1`，並保留一代。擷取的框架和日誌可能帶有帳戶敏感資料；分享之前請先閱讀 [SECURITY.md](https://github.com/osauer/canary/blob/main/SECURITY.md)。
