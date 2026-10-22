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

這次 **沒有修改 Go 正式服務邏輯**；沒有刪除測試、降低 assertion、改寫預期結果或新增規則排除。Python 僅重構 disposable CI probe 的允許路徑驗證，不改產品 HTTP/TLS 協定。效能差異限於 probe 的固定集合與最多兩次短正則匹配，不影響 gateway 請求資料面。

## 驗證

- 最終 `internal/accounts`（87 個頂層測試，70.569 秒）與 `internal/store`（73 個頂層測試，最後重跑 33.438 秒）完整 `go vet`／`go test -race -count=1` 通過；真實 PostgreSQL 17.11／Redis 7.4.11 fixture 下沒有 skip。
- 原 4 個指定 Go 測試以 `-race -count=1` 通過；原子性／目前策略／affinity freshness／catalog persistence 場景保持原測試名稱。
- Python 完整 10 項測試通過，包括實際 gateway seed→restart→verify、redirect 與新的 ASCII／Unicode／資產檔名／ID 邊界。
- 使用固定 seed，將新路徑驗證與補修前原始正則比對 **100,016 個輸入，0 差異**。這是補充證據，不是全輸入空間形式證明。
- 本地 gocognit 對四個場景的主函式及新增 helper 最大值分別為 **9、6、7、8**；所有新增 helper 均不超過 10。gocognit 與 Sonar 計分並非完全等價（原 catalog 場景為 21 vs PDF 19），因此以足夠餘量重構，仍等待服務端分析確認，不能據此宣稱 Sonar 已關閉。

獨立差異審查核對了六個程式／測試檔：未發現確定的行為回歸；特別確認正則分支與 ASCII 邊界、synctest／子測試邊界、清理順序、原 assertions，以及 converter 計數傳址保持即時值。

## 尚未處置的兩項安全問題

- **S2245 `editorId.ts`**：現有 crypto 優先；無 crypto fallback 的相容性／跨 realm 防碰撞設計尚待使用者選擇。
- **S5332 `compose_smoke.py`**：現有 probe 僅允許 loopback HTTP，proxy／redirect 仍禁止；真正 TLS 改造需要明確 endpoint 與信任設定決策。

兩項仍未解決，未在服務端標記 Safe／Accepted／False Positive。**本次六項維護性補修不應被宣稱已讓 Security 升 A 或 Quality Gate 通過。** 新提交的最終分析與 CI 結果以 PR checks 為準。
