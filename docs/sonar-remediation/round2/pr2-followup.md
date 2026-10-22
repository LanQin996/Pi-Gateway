# PR #2 最新報告補修

## 範圍與評級解讀

使用者追加的 PDF 明確標示 **Pull Request 2**、分析日期 **2026-10-22**，共有 **8 項**：6 項 Maintainability、2 項 Security；該 PR 的 Reliability／Security／Maintainability 為 **A／C／A**，Quality Gate FAILED。**這是 PR 範圍，不能解讀為整個主分支只剩 8 項。** PDF 未提供 SHA；本次依規則、路徑、行號與具體函式核對 `85e88a8d0d3c30ad9c5c98e57138835326e0458b` 的程式內容。

[8 項逐項記錄](pr2-followup-findings.csv)與[原第二輪 169 項](findings.csv)分開保存；不把不同分析範圍直接相減，亦不覆寫原始審查記錄。

## 六項修改與影響

| 規則／位置 | 修改 | 保留的行為與驗證 |
|---|---|---|
| Go S3776 `store/rules_test.go` | 按 legacy source、metadata、刪除後重初始化拆分具名 helper | migration 原子性、converter 呼叫計數、兩次初始化不復活、版本與 legacy 資料完整性 |
| Go S3776 `accounts/acquire_authorization_test.go` | synctest 改呼叫具名場景函式，reservation 驗證獨立 | 原 synctest bubble、排隊時策略更新、第二帳號選擇、所有 reservation／release |
| Go S3776 `accounts/affinity_test.go` | 每個 cache get/set 場景使用具名 helper | 子測試名稱與數量、更新 token／proxy 的時序、freshness assertions 與 deferred release |
| Go S3776 `store/model_catalog_test.go` | catalog snapshot 與 persisted upstream 的語意驗證分離 | 所有 models／timestamps／error／source／row-count assertions、原持久化及讀取順序 |
| Python S5843 `compose_smoke.py` | 固定路徑集合與 entity／asset 小模式分開 | fullmatch、原允許路徑集合、禁止外部 origin／traversal／query／fragment |
| Python S6353 同處 | `[0-9]` 改 `\d` 且明確 `re.ASCII` | 不擴大為 Unicode 數字，仍限制無前導 0 的 1–19 位 ASCII ID；真正 API/state ID 仍另驗證正 int64 |

上述六項維護性補修 **沒有修改 Go 正式服務邏輯**；沒有刪除測試、降低 assertion、改寫預期結果或新增規則排除。Python 僅重構 disposable CI probe 的允許路徑驗證，不改產品 HTTP/TLS 協定。效能差異限於 probe 的固定集合與最多兩次短正則匹配，不影響 gateway 請求資料面。

## 驗證

- 最終 `internal/accounts`（87 個頂層測試，70.569 秒）與 `internal/store`（73 個頂層測試，最後重跑 33.438 秒）完整 `go vet`／`go test -race -count=1` 通過；真實 PostgreSQL 17.11／Redis 7.4.11 fixture 下沒有 skip。
- 原 4 個指定 Go 測試以 `-race -count=1` 通過；原子性／目前策略／affinity freshness／catalog persistence 場景保持原測試名稱。
- Python 完整 10 項測試通過，包括實際 gateway seed→restart→verify、redirect 與新的 ASCII／Unicode／資產檔名／ID 邊界。
- 使用固定 seed，將新路徑驗證與補修前原始正則比對 **100,016 個輸入，0 差異**。這是補充證據，不是全輸入空間形式證明。
- 本地 gocognit 對四個場景的主函式及新增 helper 最大值分別為 **9、6、7、8**；所有新增 helper 均不超過 10。gocognit 與 Sonar 計分並非完全等價（原 catalog 場景為 21 vs PDF 19），因此以足夠餘量重構，仍等待服務端分析確認，不能據此宣稱 Sonar 已關閉。

獨立差異審查核對了六個程式／測試檔：未發現確定的行為回歸；特別確認正則分支與 ASCII 邊界、synctest／子測試邊界、清理順序、原 assertions，以及 converter 計數傳址保持即時值。

## 安全問題與後續決策

### S2245：依批准移除非密碼學亂數備援

`editorId.ts` 現在只以 `crypto.getRandomValues(new Uint8Array(16))` 的熵建立 UUIDv4，保留既有 UUID 格式與版本／variant 位元。完全移除 `Math.random`、session、時鐘與 counter 備援，也不再依賴 `randomUUID`。

- **HTTP 相容性**：`getRandomValues` 不要求 secure context；一般 HTTP 的現代瀏覽器仍可使用。這不代表任意舊瀏覽器或受限嵌入環境必定提供此 API，也不表示 HTTP 傳輸已加密。
- **明確的契約變更**：完全沒有 crypto、缺少 getRandomValues 或呼叫拋錯時，不再產生弱亂數 ID，而在要求 ID 時拋出可辨識錯誤。模組載入不取亂數，不會因 crypto 缺失在 import 時崩潰。這項相容性改變經使用者批准；沒有默默以排除或接受議題取代修復。
- **影響範圍**：編輯器本機識別碼；不改伺服器 rule ID、credential、HTTP/TLS、認證或儲存協定。每個 ID 取 16 bytes 熵，不新增全域可變計數器。
- **回歸**：共用測試涵蓋 HTTP-like 僅有 getRandomValues、正確 receiver、兩個獨立模組的 10,000 個 UUID 樣本無碰撞、固定輸入的 version／variant 位元、crypto getter／API 缺失與拋錯，以及 import 不請求熵。樣本無碰撞不是數學上的碰撞不可能保證。
- **瀏覽器測試**：保留無 randomUUID 的 HTTP 新增／追加／複製與未連線節點禁止保存流程；以三個明確錯誤案例取代舊的無 crypto 仍成功契約。完整 Playwright 清單為 57 項，真正瀏覽器結果以新提交 CI 為準，不把清單解析算作測試通過。

`79193674d37b332328659e9e8f2ce5dc19f53e85` 的 Sonar check `119680305731`（06:41:50 UTC）已不再列出 S2245，只剩 S5332；當次新程式碼 Security B、Quality Gate 仍失敗，並非全 A。

### S5332：經批准改為測試專用、驗證憑證的 TLS

`compose_smoke.py` 現在只接受 loopback HTTPS；CLI 必須提供本次 fixture 的 CA。使用系統的憑證鏈／IP SAN 驗證，最低 TLS 1.2，不使用 insecure context、不關閉驗證，並保留禁止 redirect、停用環境 proxy 與路徑／ID allowlist。這是真實 TLS endpoint，不是將字串或 scheme 隱藏。

- **隔離方式**：新增 `.github/compose.tls.yaml`，只在 CI 作為最後一份 override。TLS sidecar 使用同一個已測試的 gateway image，加掛本次編譯的 Go 標準函式庫 fixture；不新增外部代理 image、不修改正式 Dockerfile。sidecar 與 gateway 共用 `network_mode: service:pi-gateway`。gateway 透過既有 `PI_GATEWAY_HOST` 僅綁 `127.0.0.1:8317`；主機僅公開 `127.0.0.1:18443` 的 TLS 入口。
- **最後一段的真實邊界**：代理向固定 `http://127.0.0.1:8317` 轉送，仍是明文，只在共享的 network namespace 內回環；不聲稱端到端加密，也不把此測試 fixture 當正式環境 TLS 部署指引。正式 Compose、產品 HTTP／SSE／WS 支援與公開設定不變。
- **憑證與私鑰**：每次 job 產生短效 ECDSA CA／server certificate，SAN 為 `127.0.0.1` 與 `::1`；CA 私鑰只存在產生流程的記憶體。新建目錄為 0700、檔案 0600，拒絕覆寫既有目錄／檔案；server key 不提交、不上傳 artifact。sidecar 使用 runner 相同 UID/GID、唯讀 mount、唯讀根檔案系統、移除 capabilities；沒有為讀取私鑰放寬權限。EXIT trap 刪除本次臨時目錄，runner 銷毀亦不保留憑證。
- **Compose 合併防護**：要求 Compose >= 2.24.4，使用 `!override` 取代而不是附加 ports。啟動前驗證合併 JSON 的 loopback bind、單一 TLS port、共享 namespace、同 image／UID 與唯讀 mounts；若舊 HTTP port 仍公開、代理落在其他網路或配置不符即失敗。
- **保持測試內容**：四種 SQLite／PostgreSQL × memory／Redis 組合仍執行 seed→移除 containers→重建→verify→清理，Redis 的跨重建資料斷言不變。backend 的真實 gateway 測試也經同一個 TLS fixture，而不是使用僅為測試建立的不同代理實作。
- **新增成本／限制**：CI 多一個輕量 sidecar、一次靜態 fixture build、每組一次憑證產生及 TLS 握手；不宣稱整體效能提升。fixture 僅供目前 Linux amd64 Compose runtime matrix；正式 image 的 amd64／arm64 build 仍照舊驗證。這不是擴大為正式環境憑證管理功能。

本地最終 17 項 Python 測試通過：保留原 10 項，新增未信任／錯誤 CA、錯誤 IP SAN、ambient proxy、Compose 版本／port／namespace／owner／mount guard。錯誤 CA／SAN 會在發出 bearer request 前被拒絕。Go fixture `go vet` 與 6 項 `-race` 測試通過，另涵蓋憑證過期、0600／拒絕覆寫、明文送往 TLS port 被拒絕、轉送方法／路徑／body／認證保持，以及 healthcheck 不接受錯誤 CA 或 redirect。新增 Go 函式的本地 gocognit 均不超過 10，仍非 Sonar 計分保證。

本地沒有 Docker daemon，YAML parse 與合併 guard 單元測試不冒充 Docker runtime；四組真實 Compose 與新提交 Sonar 結果由遠端 CI 確認。沒有在服務端標記 Safe／Accepted／False Positive，**新提交完成分析前仍不宣稱全 A 或 Quality Gate 已通過。**

## 服務端重分析回饋

`38e9f61d294054171feb1f8bd32343bb89c27660` 於 06:04 UTC 的 Sonar Code Analysis，GitHub checks 實際頁面列出 3 個 annotations：原有 2 個安全問題，以及新固定路徑清單令 `/api/auth/logout` 達到三次重複的字面值問題（issue `AaFiNgeASWJBroeFAkbt`）。該次 annotations 已不再列出原六項維護性問題；這是 PR check 的證據，不是主分支全部問題清單。

後續將 logout 路徑抽為 `AUTH_LOGOUT_PATH`，清單與兩次 logout request 共用同一常數；不改路徑值、請求方法或次數，再次跑完整 10 項 probe 測試。該次 logout 常數補修沒有改變兩項安全契約；後續 S2245 的批准與變更另見上節。每個新提交仍需以自己的 CI／Sonar 結果判定。
