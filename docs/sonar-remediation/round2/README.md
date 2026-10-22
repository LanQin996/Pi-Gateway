# 第二輪 SonarCloud 修復、審查與影響記錄

## PR #2 追加報告補修

最新 PR 範圍 PDF 為 A／C／A、8 項（6 項可維護性＋2 項安全），並非整體主分支清單。見[完整補修、相容性影響與驗證](pr2-followup.md)及[8 項追蹤表](pr2-followup-findings.csv)。六項維護性補修已實作；兩項安全仍待使用者方案決策，未宣稱全 A。

## 報告與處置範圍

本目錄記錄第二份 **2026-10-22** SonarCloud PDF（48 頁），保留[第一輪記錄](../README.md)不覆寫。第二輪報告標示 `main`、Quality Gate **FAILED**，整體評級為 **Reliability C／Security D／Maintainability A**；新程式碼評級為 C／C／A。目標是改善至 A，但**程式修改及本地測試無法保證服務端評級**。

第二輪共有 189 個品質影響列（Security 22、Reliability 7、Maintainability 160），跨品質去重後保留 **169 項**。本輪以 `4767880425bde4b35174d54286e0b47c2735f0d1` 為差異基準，對這 169 項完成逐項處置：

| 範圍 | 本地實作完成，待重分析 | 待服務端情境審查 | 合計 |
|---|---:|---:|---:|
| Go 布林／命名／測試 context | 91 | 2 | 93 |
| 資料儲存與 SQL／交易 | 1 | 10 | 11 |
| 其他後端、測試代理、probe | 4 | 4 | 8 |
| 前端與檢查工具 | 17 | 40 | 57 |
| **總計** | **113** | **56** | **169** |

- [169 項完整追蹤表](findings.csv)：保留規則、原報告位置、品質影響、頁碼、描述及逐項處置證據。
- [56 項待審查理由](reviews.md)：每項獨立列出，不以批次標記誤報代替審查。

`SONAR2-xxxx` 是本地整理編號，不是 Sonar issue key；PDF 缺少 issue key 與精確字元欄號（column），同一行的不同項目仍分開保留。描述／原始處置證據保留原文便於核對。**113 項不等於服務端已關閉，56 項也不是已核准豁免。**

## 如何解讀 841 → 169

第一輪 PDF 的 841 項與第二輪的 169 項為各自去重後的報告清單；品質影響列由 884 降為 189，Go 認知複雜度列由 315 降為 1，第二輪未列 TypeScript 複雜度問題。

但兩份報告沒有 issue key，不能直接宣稱「精確關閉 672 項」：問題消失也可能與規則、分析範圍或版本差異有關。第二輪還新增 **89 項 Go S1940**（可簡化的否定比較），主要出現在先前拆分 assertion 後的 `!(x != y)`／`!(x == y)` 形式；本輪改成直接比較，沒有刪除測試或將失敗條件反轉成通過。另有命名、Python probe 與前端旗標參數等新增類別。

## 本輪主要變更

### 1. 安全邊界

- **Compose probe**：API 回傳及狀態檔的 entity ID 僅接受正 int64；在任何 authenticated request 之前驗證；固定可用路徑集合，拒絕 traversal、encoded separator、query、fragment 及外部 origin。
- **redirect／proxy**：loopback base URL 正規化，拒絕 userinfo；停用環境 proxy 並拒絕 redirect，避免 disposable bearer credential 被重新導向。
- **CONNECT 測試 fixture**：連線目的地固定為 setup 提供的 loopback listener；請求 authority 只決定接受或拒絕，不能決定 dial 目的地。保留 TLS wrong-host、認證隔離及取消測試原意。
- **SQLite schema inspection**：`PRAGMA table_info` 的字串拼接改成 `pragma_table_info(?)` 綁定參數，仍先驗證遷移白名單。
- **Go compiler 解析**：先解析 canonical absolute executable；Unix 檢查檔案及全部祖先的 root／目前使用者所有權與 writable bits，處理可信 sticky directory。相對路徑、不可信 owner 或可替換 ancestor 不接受；Windows 仍依賴作業系統 ACL，不能宣稱 Unix 權限驗證適用於 Windows。

### 2. 前端、測試載入與複製

- 三個檢查工具不再以 `new Function` 執行轉譯字串，改用限定 `web/src`、限定來源副檔名及顯式依賴的 native ESM loader；暫存輸出限制權限並於完成後清理。這是可信倉庫來源的測試工具，不是任意程式碼 sandbox。
- 新增 restricted JSON-wire clone，支援 nested Vue proxy、undefined／array holes／非有限數字正規化、保留鍵、循環偵測；不再把 `structuredClone` 當成 reactive JSON 的直接替代。
- 正式 clone 與測試預期值保持獨立：component 測試保留 `JSON.parse(JSON.stringify(...))` 作 reference oracle，該項留待審查。
- label 字典改成 Map，讓 `then` 僅作查找 key，避免物件外觀與 thenable 混淆；210 對原標籤與新 entries 獨立核對完全一致。真正 wire AST 的 `then` 分支名稱不更動。
- 拆分 capture key-order 策略，保留原排序；graph 仍只處理初始 owners，不因 append 新節點擴大遍歷。
- editor ID 優先採用 crypto；無 crypto 舊環境保留非安全用途隨機 salt，以免相同／降精度時鐘的獨立 realm 產生相同 ID。此 fallback 仍需情境審查，不宣稱密碼學保證。

### 3. Go 可維護性與測試

- 89 項否定比較改成直接相反運算子；保持 operand 求值次數及順序。排序比較僅針對 int64 時間值，不任意套用會改變 NaN 語意的浮點排序反轉。
- 整理重複型別驗證訊息、測試 helper 複雜度及命名。
- 新增 SQL-looking bound values 的六個 wrapper contract，以及交易 early return／panic／cancel rollback 檢查。
- Context test adapter 明確保留父 context 及 body context，新增完整 delegation contract；不把 context 藏進 method value／closure 來假裝消除保存。兩項 S8242 正確保留待審查。

## 獨立審查修正記錄

審查曾重現 Go compiler resolver 僅驗證直接父目錄、卻接受可寫祖先的缺口；已改成完整 canonical ancestor／owner 檢查，原負向案例現在拒絕。另重現無 crypto、相同時鐘的兩份 editor module 會碰撞，已恢復非安全 entropy salt 並新增兩份 module 的固定時鐘回歸。

同時撤回以 production clone 作測試 oracle、以及以 method values 隱藏 context 的做法；相關項目保留 review。這些調整以維持真實行為及可驗證性為優先，不為追求 A 而壓制規則。

## 遠端 CI 相容性修正

首次提交的 hosted runner 在 `check:rules` 拒絕 setup-go 的共用 toolcache 信任鏈。CI 現在將官方 setup-go 安裝複製到本次 job 專屬、目前使用者擁有的 `/tmp` 隨機私有目錄，移除 group/world 寫入權限，並明確設定 `GOROOT`／`PI_GO_BINARY`；不放寬 resolver 的 owner／全部 ancestor 驗證。此目錄隨 disposable runner 銷毀，額外成本為一次本機工具鏈複製，不涉及部署。

使用相同複製流程重新執行 `check:rules`／`check:sonar-regressions` 通過；遠端最終結果以 PR checks 為準。

首個遠端分析（`a981cb788c99cb9e13a8d6ffd87ae09c4f1b100f`）仍因新程式碼 Security C 未通過 Quality Gate。PR 的兩項安全 review comments 為：`editorId.ts` 非 credential 的 PRNG fallback（`AaFh_hhMwr3G9Zas3V9V`），以及 `compose_smoke.py` 固定 loopback HTTP（`AaFh_h1swr3G9Zas3V9Y`）。這些情境審查仍未獲服務端核准；不以改寫字串或刪除防碰撞行為規避規則。維持未解決狀態，不能宣稱全 A。

## 驗證狀態

**第二輪完整 Go vet／race 及最後 accounts race 均通過。** 使用 Go 1.26.0；完整 `go vet ./...` 與 `go test -race -json -count=1 -timeout=30m ./...` 通過 21 個測試套件（API 603.809 秒）。PostgreSQL 17.11／Redis 7.4.11 實機環境的 9 個必跑整合項目全部通過且無 skip，包含四種 backend 組合。

聚合執行期間，最後審查僅再調整 accounts 的一個測試檔，恢復明確 context 欄位；其餘 Go 來源雜湊維持一致。該套件已以最終來源補跑完整 vet／race，70.377 秒通過。前端最後 build 後亦補跑真實 gateway probe 及 embedded webui／command race，確保使用最終資產。

已取得本輪本地證據：

- 前端 typecheck、production build、i18n、rules、capture、quota、billing、model-catalog、靜態 accessibility、sonar-regressions 通過。
- rules 檢查包含 51 graph roundtrip、141 field edits、181 enum options、28 真實 Vue roundtrip；i18n 798 keys；model-catalog 13 cases。
- Python probe 7 項完整測試通過，包含實際 gateway seed→重啟→verify→清理、惡意 ID／路徑、回環 URL、redirect 與 state 檔案邊界；獨立複核其中 5 項邊界測試亦通過。
- SQLite shared contract／transaction ownership focused tests，以及完整 store race＋實際 PostgreSQL 通過；最終 webui／command race 亦通過。
- Go compiler writable-ancestor 負向測試、可信 sticky ancestor、固定時鐘雙 module ID 回歸，以及原／新 label pairs 比對通過。

尚未證實：

- **第二輪瀏覽器 E2E 未重跑**。第一輪遠端 55 項通過僅是基準證據，不是本輪測試結果。
- **本輪 Docker Compose runtime／多架構 image build 未驗證**；probe focused test 不等於容器測試。
- **本輪修改後 SonarCloud 尚未重新分析**；113／56 是本地處置分類，不能保證 Quality Gate 或全部 A。

## 影響、相容性與合併條件

- Probe 只接受目前實際使用的 API／資產路徑；若未來路由或資產命名改變，須同步 allowlist，不能一律恢復任意路徑。
- Go 工具鏈在不安全共用安裝目錄、相對 PATH 或外部使用者擁有的目錄可能被拒絕；可透過受信的絕對 `PI_GO_BINARY`／`GOROOT` 指定，但不繞過 Unix ownership／ancestor 檢查。
- JSON clone 僅對 wire 資料設計；Date、自訂 prototype／toJSON 不視為支援輸入。跨 realm fallback ID 為機率性防碰撞，非 credential。
- SQL／HTTP／可及性／順序 await 等保留項目需逐項確認信任邊界與契約；不把通用 SQL wrapper 當 sanitizer，不將 HTTP 字串改 HTTPS 偽裝 TLS，不移除鍵盤焦點或 reduced-motion 保護以通過掃描。
- [x] 確認最後完整 Go aggregate、必跑整合無 skip、accounts 與 embedded webui／command race 結果。
- [ ] 重跑本輪瀏覽器與容器 CI，補實際可及性驗證。
- [ ] SonarCloud 重分析，核對原問題、新問題、Quality Gate 及評級。
- [ ] 逐項審查 56 項並在服務端記錄決策；無權限時保持未解決，不預先宣告 A。

本輪不刻意改變公開 HTTP／SSE／WS wire contract，不宣稱有整體效能量化提升；仍應在 staging 驗證規則編輯、串流／取消及資料升級。原始碼回退不等同資料庫回復，部署前仍應備份。
