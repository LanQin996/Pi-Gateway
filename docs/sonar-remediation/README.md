# SonarCloud 修復記錄與影響說明

## 1. 範圍與結論

本次變更依據 **2026-10-19 的 SonarCloud PDF 報告（137 頁）**，以 `master` 的 `ae06a55825ecd654d3eca6d56c0e132af73394ea` 為修復基準。報告標示的分支為 `main`，但倉庫實際基準為 `master`；原始行號已對照基準程式，不應直接當成重構後行號。

報告包含 884 個品質影響列（Security 38、Reliability 62、Maintainability 784）。同一問題可能影響多個品質面向，去除跨品質重複後為 **841 項**：

| 範圍 | 已完成本地修復、待重分析 | 保留行為、待情境審查 | 合計 |
|---|---:|---:|---:|
| 前端與前端檢查 | 323 | 50 | 373 |
| API／admin／upstream／Codex bridge | 215 | 0 | 215 |
| 帳號管理 | 46 | 2 | 48 |
| 規則引擎 | 57 | 0 | 57 |
| 其他後端、CI、容器設定 | 84 | 4 | 88 |
| 資料儲存與遷移 | 51 | 9 | 60 |
| **總計** | **776** | **65** | **841** |

**這不是「841 項已關閉」的宣告。** SonarCloud 尚未重跑，65 項尚需在服務端逐項判定；Quality Gate 是否通過仍未知。沒有以 `NOSONAR`、排除檔案或停用規則來隱藏報告問題。

- [完整 841 項追蹤表](findings.csv)：原始規則、位置、描述、處置證據與狀態。
- [65 項待審查理由](reviews.md)：每一項的具體安全／相容性邊界，以及保留既有行為的理由。

`SONAR-0001` 等編號是本次報告整理的追蹤編號，**不是 SonarCloud issue key**。PDF 未提供完整 issue key／欄位位置；同一檔案同一行的不同列仍分開保留。CSV 中的描述與原始證據保留英文以利核對，繁體中文總結及審查理由見本目錄文件。

## 2. 安全性與執行環境修復

- **靜態資產路徑**：在正規化之前拒絕路徑穿越片段、反斜線及 NUL，並以 `fs.ValidPath` 限制允許的資產路徑；補上穿越及正常路由回歸測試。
- **CI 狀態檔**：限制 smoke probe 的檔案位置及 symlink 行為，避免測試狀態寫到預期目錄之外。
- **SQL 遷移**：`ensureColumns` 的表名、欄名與定義必須符合程式內的遷移登錄白名單。SQL 值仍以參數綁定傳入，不把參數拼接進查詢。
- **測試代理**：CONNECT fixture 在撥號前拒絕非 loopback 位址及主機名稱，避免測試代理被用來連任意目的地；這不等同於變更正式環境的 egress 政策。
- **遷移群組名稱雜湊**：舊 API key 群組遷移的確定性名稱由 SHA-1 改為 SHA-256，並更新對應測試。
- **供應鏈設定**：GitHub Actions 改用固定 commit SHA；npm 安裝使用 `--ignore-scripts`，Playwright 使用已安裝的本地執行檔而不是隱式取得其他版本。
- **HTTP 情境**：沒有把實際 HTTP listener、健康探針、operator 指定的 WS 上游或已註冊 localhost OAuth callback，僅靠改字串偽裝成 HTTPS。四項情境仍需審查部署邊界，見 [reviews.md](reviews.md)。

## 3. API、串流與請求生命週期

- 將請求正規化、舊版 compact 請求整理、continuation 解析、key admission、帳號路由及回應交付拆成具名步驟。
- SSE 解析分離 frame 狀態與實體行讀取，保留 byte limit、部分 EOF、idle timeout、錯誤標頭、capture 順序與「已送出事件後不可任意重試」的界線。
- WebSocket exchange 保留 watcher 結束等待及 pool release 的生命週期；失敗寫入仍標記連線不可再用，失敗／取消／過期 exchange 不回收至可用池。
- continuation 的共享狀態提交仍先於終止事件交付，本地 baseline 則在 callback 成功後提交；明確 continuation 不會靜默重撥或重播。
- 429 同帳號重試保留初始 rate-limit 事件緩衝、跨重試共用的等待期限，以及先釋放 slot 再取得的順序；取消仍優先於待回傳錯誤。
- SSE 回應標頭仍延遲提交：第一個事件之前的錯誤回傳 JSON，提交之後以 SSE 表示；WebSocket retry metadata 仍僅附於錯誤事件。
- Codex bridge 拆分 copy-on-write 轉換與 namespace／工具形狀檢查，不放寬原本接受的加密或工具資料格式。

**預期效果**：降低分支與狀態交錯的維護難度，同時保留取消、釋放資源、錯誤分類及重試邊界。此為大範圍重構，測試通過不能替代部署後對串流中斷、continuation 及 429 行為的觀察。

## 4. 帳號選擇、資料庫與規則引擎

### 帳號與目錄

- 拆分 eligible／excluded／pinned／retry／specific 帳號選擇，保留取得 slot 後重新驗證、affinity cache I/O 後再次驗證、取消時釋放及 FIFO 等待位置。
- 拆分模型目錄項目解析、重複 ID 合併、來源更新摘要與 credential 目的地檢查；保留第一個來源的明確 `null`／`false`／空值優先權。
- 拆分 Codex 版本查詢、quota 與官方用量同步；單一請求取消不會安裝全域失敗快取。

### 資料儲存

- 拆分 SQL placeholder tokenizer，保留單／雙引號、巢狀註解、dollar quote、`??` 逃脫與 PostgreSQL 參數編號語意。
- 拆分設定載入、帳號管理 patch、模型目錄合併、規則發佈及遷移；維持交易原子性、revision、排序與回滾責任。
- 過長參數清單改用具名結構，例如 `AccountManagementFields` 與 `ChatGPTCredential`，同步更新倉庫內呼叫端。
- `BeginTx` 是交付活交易所有權的 factory，不能在回傳前 `defer Rollback`。已檢視實際呼叫端的 rollback／commit 管理，這一項保留為待審查，不以錯誤修復破壞交易。

### 規則引擎

- 將規則執行、action／condition／expression、巢狀 flow、JSON clone／pointer／diff、舊規則展開與驗證拆成責任明確的函式。
- 移除 interpreter 的 `context.Context` 結構欄位，改為沿執行路徑明確傳遞；新增五個直接檢查取消傳遞的 runtime 邊界測試。
- 保留每條規則的候選狀態隔離、整條規則回滾、`skip_rule`／abort 差異、trace 省略預算、受保護欄位及 request model 不變條件。
- 所有 **315 項 Go 複雜度目標**在本地精確遞迴分析均 ≤15，新增輔助函式亦未超標。此結果是本地分析證據，不是 SonarCloud 的權威結果。

## 5. 前端、可及性與 capture

- 重構規則編輯器、graph、schema、模擬、capture 檢視與模型目錄的複雜條件、巢狀模板及型別處理；整理重複字串、排序、正規表示式與不必要暫存變數。
- 修正表單標籤及可及性問題，補充靜態 template 檢查；保留 ARIA 複合選項、可捲動區域鍵盤焦點及 reduced-motion 覆寫。保留項目需要真實瀏覽器與輔助科技驗證，不能只依靜態檢查判定可用。
- 保留 JSON wire AST 的 `then` 欄位及 Vue reactive proxy 的 JSON 正規化語意；不為滿足靜態規則改用不等價的 Promise 名稱或 `structuredClone`。
- 保留有相依副作用的順序 `await`、走訪快照及 HTTP LAN 剪貼簿 fallback；完整理由逐項列於審查清單。
- capture 的反射複製、secret redaction、來源 metadata 與 byte／node／trace 預算拆分後維持原有順序。
- 差異審查曾發現 SSE 前綴擷取重構會在小輸出預算下掃描完整長行；已改成依剩餘預算限制掃描，新增 1 MiB 長行邊界測試。**不主張整體吞吐量或延遲已有量化改善**；此修正主要避免重構引入額外 CPU 成本。

## 6. 修復與審查歷程

1. 將 PDF 的 884 個品質影響列核對為 841 個追蹤項目，保留原始頁碼、規則及行號；以基準程式對照問題位置。
2. 分模組完成安全修復及複雜度重構，將測試 setup／assertion／migration 階段拆成具名函式；保留原有斷言，不刪除測試以降低複雜度。
3. 完成跨模組編譯、常數與呼叫端整合，清理重構新增的非必要 context 欄位及參數。
4. 進行獨立差異審查，修正 SSE 長行掃描退化，檢視資源釋放、交易、排序、credential 邊界及 redaction 行為。
5. 啟動隔離的實際 PostgreSQL／Redis，完成最終完整 race 測試及前端檢查；最後資料庫測試調整後再次執行 store race。
6. 將不能以等價改碼消除的 65 項逐項記錄為待審查；交由 SonarCloud 重分析與人工判定，不預先宣告關閉。

## 7. 驗證記錄

| 驗證 | 結果與範圍 |
|---|---|
| `go vet ./...` | 通過 |
| `go test -race -count=1 -timeout=30m ./...` | 實際 PostgreSQL 17.11／Redis 7.4.11；21 個有測試套件全部通過，3 個套件無測試 |
| 完整 race 的代表耗時 | API 607.122 秒、admin 147.338 秒、accounts 89.231 秒、store 49.379 秒 |
| 最後 store race | 最後一次測試常數整理後，以實際 PostgreSQL 再跑通過，37.979 秒 |
| 前端 | typecheck、build、i18n、rules、capture、quota、billing、simulation、model-catalog、靜態 accessibility 全部通過 |
| 前端部分覆蓋數量 | 798 雙語鍵；51 graph AST roundtrip、141 field edit、181 enum option、28 真實 Vue roundtrip；capture 另含 Unicode 排序及 250 組隨機案例 |
| 本地 smoke | 3 個 Compose probe／state 路徑測試通過；gateway seed／reopen／verify smoke 通過，但不等同容器 runtime 驗證 |
| 差異檢查 | `git diff --check` 通過 |

先前重複執行的一次 API race 因預設 10 分鐘期限終止，另一次重複執行取消，**均未計入通過**；最終完整執行使用 30 分鐘期限並成功。

### 尚未驗證

- **55 項瀏覽器 E2E 未執行**：Chromium 在測試啟動前受執行環境限制，沒有完成瀏覽器斷言，不能記為 E2E 通過。
- **Docker Compose runtime／多架構 image build 未執行**：本地沒有可用 Docker runtime；本地 smoke 不能替代這些檢查。
- **SonarCloud 未重新分析**：Quality Gate、服務端新增問題與 65 項 review 的最終處置仍未知。

## 8. 相容性與導入風險

| 變更／邊界 | 影響與注意事項 |
|---|---|
| 路徑與 SQL 白名單 | 原本容忍的不合法資產路徑或未登錄遷移定義現在會拒絕；新增遷移欄位必須同步更新登錄。 |
| SHA-256 遷移名稱 | 尚未執行舊 key 群組遷移的資料庫會產生不同的 `migrated:` 名稱；已遷移資料不以本次變更全面重新命名。部署前仍應備份並驗證舊資料升級流程，避免外部工具依賴舊 SHA-1 名稱。 |
| Go 內部方法簽章 | 倉庫內呼叫端已更新；維護私有 fork／同模組擴充的使用者需同步具名參數結構，不能只替換單一檔案。沒有以此宣告公開 HTTP API 版本變更。 |
| npm 停用安裝腳本 | 降低安裝時任意腳本執行面，但未來新增需要 lifecycle script 的依賴可能無法直接安裝；應明確審核並建立必要的建置步驟，不應無條件恢復全部腳本。 |
| Actions 固定 SHA | 提高執行版本可追溯性；安全更新仍需定期更新固定版本，不會自動追隨 tag。 |
| 串流／取消／交易重構 | 改動範圍大；雖已通過含真實服務的 race 測試，仍須檢視部署流量下的重試、continuation、取消、pool reuse 及資料回滾。 |
| 前端可及性與剪貼簿 | 靜態驗證不保證實際瀏覽器、螢幕閱讀器及 HTTP LAN 環境行為；必須補足 E2E／人工驗證。 |
| HTTP 保留情境 | 不代表對外明文傳輸安全。正式部署仍須使用適當 TLS 終止、可信網路邊界及正確 operator 設定。 |

## 9. 合併前檢查與回退

- [ ] 在正常 CI 執行 55 項瀏覽器 E2E，補做鍵盤／螢幕閱讀器與 reduced-motion QA。
- [ ] 完成 Docker Compose runtime 與 `linux/amd64`／`linux/arm64` 建置。
- [ ] 重新執行 SonarCloud，核對每個原報告問題、新增問題及 Quality Gate。
- [ ] 對 [65 項清單](reviews.md)逐項確認資料來源、信任邊界與相容性理由，再於 SonarCloud 記錄適當處置；不要批次當成 false positive。
- [ ] 以資料備份驗證舊 SQLite／PostgreSQL 升級及回退方案後，再決定正式部署。

原始碼可以透過回退 PR commit 還原；**程式碼回退不會自動撤銷已完成的資料遷移或還原資料庫內容**。涉及既有資料時應使用部署前備份及已驗證的還原流程。本修復文件與草稿 PR 本身不代表已合併或部署。
