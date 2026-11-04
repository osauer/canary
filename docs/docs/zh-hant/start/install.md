---
translation-of: docs/docs/start/install.md
source-revision: b2211b4bca2bd59bd138c40d6d0bb2ebb8a78893
nav-title: 安裝與首次運行
summary: 先決條件、各種安裝方式，以及證明閘道可達的第一批指令。
description: Canary 連線前必須運行甚麼、在 macOS 或 Linux 上選擇哪種安裝方式、如何解讀狀態健康檢查，以及二進位檔、設定、狀態和日誌寫到哪裏。
---

# 安裝與首次運行 {#install-and-first-run}

更新：2026-11-04 09:23 CET

`canary` 是一個 Go 二進位檔。它包含終端機 CLI、本機 stdio MCP 伺服器，以及保持與你自己的 IB Gateway 或 TWS 工作階段連線的背景守護程序（daemon）。沒有任何部分是託管的，也不涉及 Python 或 Java 執行環境。

## 開始之前 {#before-you-start}

- **一個運行中並已登入的閘道。** IB Gateway 10.37 或更新版本，或 TWS。模擬帳戶或真實帳戶都可以。
- **已在該應用程式中啟用 API socket 存取**，位置在 Global Configuration → API → Settings。沒有啟用的話，通訊埠會接受 TCP 連線但永遠不會完成握手。
- **一個 IBKR Pro 帳戶。** IBKR Lite 不包含 TWS API 存取。
- **macOS 或 Linux，arm64 或 amd64。** 發行版本為這四種組合構建。
- **與閘道同一部機器**，除非你固定了 `[gateway].host`。探測只檢查本機迴環位址。
- **任何 IBKR 實體。** 由 Interactive Brokers 香港、新加坡、日本、澳洲、愛爾蘭、英國或 LLC 持有的帳戶，連線方式完全相同；[香港及新加坡帳戶](ibkr-hong-kong-singapore.md)說明基礎貨幣和交易所日曆。

沒有 Windows 版本。守護程序使用 `setsid`、`flock` 和 AF_UNIX socket，所以程式碼完全無法為 Windows 編譯，而安裝程式在偵測到 MSYS、MinGW 或 Cygwin shell 時會停止並顯示訊息。WSL 是 Windows 上的途徑：在 WSL 內，`uname` 回報 Linux，普通的 Linux 安裝方式即適用。

在 Linux 上，`canary stop`、`canary restart` 以及 `canary update` 之後的重啟都從 `/proc` 讀取程序身份，所以不需要 `ps`，在 Alpine 等 BusyBox 系統上也一樣運作。Canary 向程序發送訊號之前，會檢查守護程序鎖定檔中的 PID 是否正在運行 `canary daemon`，對於無關或無法讀取的程序會拒絕而不發送任何訊號。透過 qemu-user 模擬在 arm64 主機上運行的 amd64 二進位檔會按其解譯器被識別。其他模擬器，例如 Rosetta for Linux、FEX 或 box64，則不會：`stop` 會列出它所看到的指令並讓守護程序繼續運行，請自行用 `kill` 停止。

### 從 v3.26.2 或更早版本升級 {#upgrading-from-v3262-or-earlier}

如果你的應用程式使用含空格的路徑，或者你運行獨立的 HOME/XDG 設定檔，請先使用 `canary update --no-restart`。然後從應用程式原本的設定檔環境呼叫新安裝的 `canary restart`。較舊的更新程式即使替換了檔案，仍以自己的舊重啟程式碼執行啟用，所以其 `--restart` 無法套用這些修正。交易版本的使用者應先透過現有的安裝程序安裝新的交易版本構件，再重啟。

從 v3.26.3 起，應用程式生命週期指令在停止它之前會核對應用程式實際生效的 HOME、socket、設定和狀態路徑。不同或無法讀取的設定檔環境會被拒絕而不停止任何程序；請使用應用程式原本的環境。相對的設定檔路徑和相對的應用程式 `--state-dir`/`--log` 值需要改為絕對路徑。帶有自訂環境或工作目錄的自訂 launchd 工作需要明確的監督程式管理；自動生命週期指令不會觸碰它們。較舊的已儲存監督程式復原記錄沒有設定檔身份：請從其原本的設定檔環境以 `canary setup app` 復原，而不是在目前環境下自動恢復。

## 選擇安裝方式 {#pick-an-install-path}

| 方式 | 做法 | 適用情況 |
| --- | --- | --- |
| Claude Desktop 套件 | 以 Claude Desktop 開啟 [`canary.mcpb`](https://github.com/osauer/canary/releases/latest/download/canary.mcpb) | Claude Desktop 是你唯一的主機。套件自帶 macOS 和 Linux 二進位檔。 |
| Shell 安裝程式 | `curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh \| sh` | 你想要一個供終端機和每個 MCP 主機共用的二進位檔。 |
| 公開 Go 模組 / v2 CLI | `go install github.com/osauer/canary/v2/cmd/canary@latest` | 你刻意要使用維護中的 v2 Go 系列。這不會安裝產品 v3。 |
| 發行版 tarball | 下載 `canary-vX.Y.Z-<os>-<arch>.tar.gz`，並對照 `SHA256SUMS` 核對 | 你手動核對下載檔案，或安裝到受管理的位置。 |
| 複製並構建 | `make install GO_TAGS=""` | 你正在開發這份程式碼。 |

純 `make install` 和 `make restart-daemon` 使用此儲存庫的實驗性交易版本預設值。傳入 `GO_TAGS=""` 以保留標準的唯讀能力。

[Claude Desktop 安裝頁面](../../claude-desktop-interactive-brokers/index.html)逐步講解套件的安裝。[打包與發佈](../internals/packaging.md)描述每個構件包含甚麼。

產品 v3 及之後的版本使用已簽署的安裝程式、MCPB 或發行版 tarball。公開 Go 模組維持為 `github.com/osauer/canary/v2`：Go 會忽略該模組路徑的根 v3 標籤，所以 `/v2/...@latest` 會繼續解析為最新的有效 v2 標籤。這讓現有的函式庫使用者和從原始碼構建的 v2 安裝留在其維護中的主要版本上，而不會無聲地跨越產品邊界。

## 目前原始碼與已簽署的發行版 {#current-source-and-signed-releases}

說明文件和 MCP 探索目錄跟隨 `main`。FX 貢獻和借出費用報表已包含在 v3.16.0 的已簽署二進位檔和 MCPB 中。未來的指令和工具可能在已簽署的發行版之前出現在這裏。請核對你已安裝的 `canary --help` 和[最新發行版](https://github.com/osauer/canary/releases/latest)。

要以唯讀版本試用目前的原始碼：

```sh
git clone https://github.com/osauer/canary.git
cd canary
make build GO_TAGS=""
./bin/canary --help
```

這會構建一個獨立的可執行檔；它不會取代已安裝的 Canary。需要 Go、Make 以及儲存庫中宣告的版本。

## Shell 安裝程式做甚麼 {#what-the-shell-installer-does}

1. 偵測你的作業系統和架構，並拒絕 Darwin 或 Linux 上 arm64 或 amd64 以外的任何組合。
2. 透過跟隨 `/releases/latest` 的重新導向解析最新發行版，所以它不會呼叫 API，也不可能觸及速率限制。
3. 下載 tarball、`SHA256SUMS` 和 `SHA256SUMS.asc`。從 v1.0.0 起，缺少簽章會中止安裝，而不會退回到只核對校驗和的路徑。
4. 從已加標籤的原始碼樹把發行版簽署金鑰匯入一個臨時鑰匙圈，對照 `D98426D48FED85EFA33904694D922A4F922B7D7D` 核對其指紋，並核對簽章。指紋或簽章不符會中止。v1.0.0 及之後的版本需要 `gpg`。
5. 核對 SHA-256 校驗和。
6. 在解壓前列出壓縮檔內容。`canary`、`LICENSE` 和 `README.md` 以外的任何東西都會被拒絕，符號連結形式的 `canary` 也會被拒絕。
7. 以 0755 模式把二進位檔安裝到 `~/.local/bin`，或者你設定了 `CANARY_INSTALL_DIR` 的話，安裝到那裏。
8. 清除 macOS 的 `com.apple.quarantine` 屬性，讓 Gatekeeper 不會阻擋首次運行。
9. 當 `~/.local/bin` 不在 `PATH` 中時把它加入。對 Bash，它更新 `.bashrc` 和第一個存在的登入檔（`.bash_profile`、`.bash_login` 或 `.profile`），若都不存在則建立 `.bash_profile`。對 Zsh，它更新 `${ZDOTDIR:-$HOME}/.zshrc`；fish 使用 `${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish`；其他 shell 使用 `.profile`。之後請開啟新的終端機。自訂的 `CANARY_INSTALL_DIR` 會把 shell 設定留給你處理。如果啟動檔無法寫入，安裝會繼續並印出需要手動加入的那一行。當 PATH 上有較舊的可執行檔優先時，安裝程式會警告並顯示已安裝版本的絕對指令。
10. 執行 `canary version`，如果已安裝的二進位檔回報的版本與剛才取得的不同，則發出警告。

建議先閱讀它：`curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh -o install.sh`，然後 `less install.sh`，再 `sh install.sh`。

## 檢查是否成功：`canary status` {#check-it-worked-canary-status}

`canary status` 是唯一值得記住的指令。它回答守護程序是否已啟動、閘道握手是否已完成、它連上了哪個端點和帳戶，以及哪個子系統仍在預熱。

```sh
canary status
```

一個健康的安裝會印出接近以下的內容。以下數值皆為虛構：

```text
IBKR Gateway  READY

Session        DU0000000 via 127.0.0.1:4001 (tls=false, discovered), client 15
Market data    Live
Daemon         v1.0.0, up 30m42s
TWS            API server 178
Gateway setup  as expected · read 09:02
Executions     other-client fills not yet observed · check TWS Master API client
               ID is 15
SPX members    cache:2026-05-22, 503 names
```

第一行的標記是 `READY`、`STARTING`、`ATTENTION` 或 `OFFLINE`。Session 行上的 `discovered` 表示通訊埠來自探測；`pinned` 表示它來自你的設定檔。數據農場、市場存取、背景工作、子系統、交易和數據品質的行只在有話要說時才出現，而 `Next concern` 行只在有事項時才出現。

`Executions` 行回報來自其他客戶端的成交證據。在觀察到一筆之前，即使 TWS 已正確設定，可見性仍未被證明。TWS 只向其主客戶端回報手動輸入或由其他 API 客戶端輸入的成交，所以執行成本報表和決策記分卡只有在你把 TWS 的 **Global Configuration → API → Settings → Master API client ID** 設為 Canary 的客戶端 ID 之後才會看到它們；看到第一筆這類成交後，該行會顯示 `all clients (proven HH:MM)`，而 `canary status --json` 在 `executions` 下帶有相同的事實。

`Market access` 行是缺少 IBKR 市場數據訂閱唯一會按名稱顯示的地方，而不是在每個需要該數據的功能中分別浮現：

```text
Market access  SPX not subscribed (IBKR 354, seen 14:03, retry 14:33)
```

它列出閘道拒絕了甚麼、何時拒絕，以及 Canary 何時會再次請求——IBKR 代碼 354 表示帳戶未訂閱該數據。沒有任何東西會因此被關閉：列出的代號只表示此刻重新擷取會被拒絕，而持有快取結果的功能會繼續提供它。該行是一個 30 分鐘視窗內的觀察，不是你訂閱權限的記錄，所以視窗內沒有任何功能請求過的代號永遠不會出現，而數據農場故障也可能把你帳戶確實持有的代號放到那裏。訂閱在 IBKR Client Portal 管理，不在 Canary 內。

首次運行而沒有開啟任何 IBKR 應用程式時，標記是 `OFFLINE`，而 `Last attempt` 行說明守護程序發現了甚麼以及接下來該做甚麼。以下數值皆為虛構：

```text
IBKR Gateway  OFFLINE

Session        auto-detect via 127.0.0.1 (tls=false), client 15
Market data    unavailable until connected
Daemon         v1.0.0, just started
TWS            not connected
Last attempt   no IBKR listener found on 127.0.0.1 ports [4001 4002 7496 7497]
               (probe timeout 200ms); no TWS / IB Gateway / IBKR Desktop process
               found — start one and Canary will reconnect automatically
Next concern   Broker data connection unavailable

  Daemon log: ~/.local/state/ibkr/ibkr-daemon.log
```

`Last attempt` 行是守護程序對其上一次連線嘗試的判斷，原文照印：沒有運行中的 IBKR 應用程式、應用程式運行中但通訊埠拒絕連線（附上要檢查的設定，或要在 `config.toml` 中固定的 `[gateway]` 通訊埠）、沒有完成的握手，或者接受連線後又中斷的通訊埠。[故障排除](troubleshooting.md)解釋每一則訊息。`Next concern` 行保留守護程序的整體判斷。

當守護程序尚未從閘道得到判斷時，`status` 會每 500 毫秒輪詢一次，最多 25 秒後放棄，然後印出守護程序日誌路徑，以及在守護程序尚未說明握手為何停滯時印出 API 檢查清單。閘道未連線時，指令以 1 退出；`canary status --json` 在 `verdict` 下帶有相同的判斷，在 `last_error` 下帶有連線錯誤，所以可在指令碼中使用。

這一切都不需要設定檔。守護程序以每個 200 毫秒的逾時並行探測 `4001`（Gateway 真實）、`4002`（Gateway 模擬）、`7496`（TWS 真實）和 `7497`（TWS 模擬），按該順序取第一個回應者，把其他的列為備選，從 `managedAccounts` 自動偵測帳戶，並使用客戶端 ID `15`。只有當你想固定某項設定時才建立 `~/.config/ibkr/config.toml`；[設定參考](../reference/config.md)中的每個鍵都是可選的。

## 兩件令人意外的事 {#two-things-that-surprise-people}

**守護程序會自行啟動。** 任何由守護程序支援的指令都會撥接 Unix socket，而當 socket 不存在時，會在自己的工作階段中啟動 `canary daemon`，最多等待 5 秒讓它出現。你永遠不需要手動啟動它。一個守護程序同時服務 CLI 和每個 MCP 主機，所以它們共用一個閘道連線和客戶端 ID。當 15 分鐘內沒有客戶端連線且沒有進行中的背景工作時它會退出，由 `[daemon].idle_timeout` 控制。

**首次市場廣度構建大約需要 74 分鐘。** 標普 500 市場廣度在本機由成份股的每日 K 線計算，因為 IBKR 不會在零售訂閱中轉發 S&P DJI 的廣度指數。共用的歷史數據限速器允許最初 60 個請求，然後以每秒 0.1 個的速度補充，所以其餘 443 個名稱在冷快取上大約需要 74 分鐘。這段期間沒有任何東西壞了：`canary status` 顯示 `breadth:computing`，而 `Background` 行顯示 `refreshing rolling SPX breadth`。快照只有在覆蓋率達到成份股數目的 80% 時才會發佈；低於此值時，引擎保留它已擷取的各名稱視窗，每 12 分鐘重試一次，15 次後放棄，直到下一次收市後更新。閒置計時器不會中斷構建：市場廣度計為背景工作，所以守護程序會保持運行，直到更新完成或重試預算用盡。之後的更新很快，而且快取在重啟後仍然保留。

## 檔案放在哪裏 {#where-things-land}

| 路徑 | 內容 |
| --- | --- |
| `~/.local/bin/canary` | 二進位檔，除非你設定了 `CANARY_INSTALL_DIR` |
| `~/.config/ibkr/config.toml` | 可選的固定設定。預設不存在 |
| `~/.config/ibkr/policies/` | 安裝程式從 Canary 預設值寫入的政策檔，每個都以 `Canary defaults, not yet reviewed` 開頭：`rulebook-policy.toml`、`protection-policy.toml`、`opportunity-policy.toml` 和 `risk-policy.toml`，後者的資本數字是留給你填寫的佔位值，其 `[order_limits]` 載有每筆訂單的上限。永不被取代：守護程序在下一次啟動時先備份再就地遷移現有檔案，並保留你寫入的每項設定；見[交易政策](../understand/policy.md#policy-files-canary-writes-for-you) |
| `~/.local/state/ibkr/daemon.db` | 守護程序擁有的狀態：設定、日誌記錄、成份股、證據 |
| `~/.local/state/ibkr/ibkr-daemon.log` | 守護程序日誌。有東西無法啟動時第一個要看的地方 |
| `~/.cache/ibkr/ibkr.sock` | 守護程序 socket；設定了 `$XDG_RUNTIME_DIR` 時位於 `$XDG_RUNTIME_DIR/ibkr/` |
| `~/.cache/ibkr/` | 快取和更新暫存空間 |

`XDG_CONFIG_HOME` 重新定位 `config.toml`；`XDG_STATE_HOME` 重新定位守護程序狀態和日誌，而 `XDG_CACHE_HOME` 重新定位快取和更新暫存空間。socket 在設定了 `XDG_RUNTIME_DIR` 時使用它，否則使用 `HOME/.cache/ibkr`，與 `XDG_CACHE_HOME` 無關。`CANARY_CONFIG`、`CANARY_SOCKET` 和 `CANARY_LOG` 各自覆寫對應的檔案。

政策預設值維持在 `HOME/.config/ibkr/policies` 下，即使設定了 `XDG_CONFIG_HOME` 或 `CANARY_CONFIG`。規則手冊、保護和機會政策在 `[rulebook]`、`[auto_trade]` 和 `[opportunities]` 中有明確的 `policy_file` 覆寫；風險憲章使用其固定的預設路徑。更改設定路徑不會重新定位政策。

## 下一步 {#next}

在依賴對帳、按結單推算的權益或 Canary Edge 之前，先在[設定經紀商報表](reporting.md)中設定共用的 IBKR 結單來源。然後在[連接 MCP 主機](hosts.md)中把代理接到伺服器。以港元、新加坡元或日圓計的帳戶，請閱讀[香港及新加坡帳戶](ibkr-hong-kong-singapore.md)了解日曆和簡報的時間。以[更新](updating.md) 保持二進位檔為最新。在對它能向經紀商發送甚麼、不能發送甚麼作出任何假設之前，先閱讀 [受約束的訂單與交易版本](../operate/orders.md)。
