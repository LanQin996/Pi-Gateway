# 65 項待情境審查記錄

這 65 項 **尚未在 SonarCloud 關閉或獲准豁免**。以下保留每一筆報告位置與理由；應於重分析後確認實際程式與信任邊界，再在 SonarCloud 選擇適當處置，而不是批次標記誤報。行號均為原報告位置，並非修改後位置。

每項先列繁體中文說明，另保留原始逐項證據以利核對。不同追蹤編號即使理由相同，也各自保留；完整其他 776 項見 [findings.csv](findings.csv)。

## SONAR-0419 — S1523

- 原報告位置：`web/check-quota-cost.mjs:20`
- 原報告頁碼：38, 112
- 狀態：**待情境審查，尚未關閉**

此為測試用 TypeScript 載入器，只執行固定倉庫路徑的來源，不接受 HTTP 或使用者輸入；僅允許預期的 import，其他輸入立即失敗。換成其他動態求值介面只會隱藏相同信任邊界，並未降低風險。需確認倉庫及 CI 的來源可信，再作服務端安全審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Security review required, retained: test-only loader executes TypeScript compiled from fixed repository-local source paths, never API/user input; require is restricted to explicit allowlisted imports and unexpected imports throw. Replacing Function with another evaluator would only hide the hotspot.

</details>

## SONAR-0420 — S1523

- 原報告位置：`web/check-usage-billing.mjs:20`
- 原報告頁碼：38, 112
- 狀態：**待情境審查，尚未關閉**

此為測試用 TypeScript 載入器，只執行固定倉庫路徑的來源，不接受 HTTP 或使用者輸入；僅允許預期的 import，其他輸入立即失敗。換成其他動態求值介面只會隱藏相同信任邊界，並未降低風險。需確認倉庫及 CI 的來源可信，再作服務端安全審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Security review required, retained: test-only loader executes TypeScript compiled from fixed repository-local source paths, never API/user input; require is restricted to explicit allowlisted imports and unexpected imports throw. Replacing Function with another evaluator would only hide the hotspot.

</details>

## SONAR-0421 — S1523

- 原報告位置：`web/check-model-catalog.mjs:18`
- 原報告頁碼：38, 112
- 狀態：**待情境審查，尚未關閉**

此為測試用 TypeScript 載入器，只執行固定倉庫路徑的來源，不接受 HTTP 或使用者輸入；僅允許預期的 import，其他輸入立即失敗。換成其他動態求值介面只會隱藏相同信任邊界，並未降低風險。需確認倉庫及 CI 的來源可信，再作服務端安全審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Security review required, retained: test-only loader executes TypeScript compiled from fixed repository-local source paths, never API/user input; require is restricted to explicit allowlisted imports and unexpected imports throw. Replacing Function with another evaluator would only hide the hotspot.

</details>

## SONAR-0429 — S8168

- 原報告位置：`internal/store/dialect.go:149`
- 原報告頁碼：42, 43
- 狀態：**待情境審查，尚未關閉**

BeginTx 工廠必須把活交易所有權交給呼叫端；在此 defer Rollback 會使回傳交易立即失效。已檢視 30 個正式呼叫點：26 處直接 defer rollback、3 處在 deferred closure 回滾，tryBeginRulePublication 在取得失敗時回滾、成功時交付 beginRulePublication；PublishRules／RepairLegacyMiddleware／InitializeRules／RestoreRulesV2Marker 四個最終擁有者均負責 rollback 與明確 commit。已有所有權註解，未壓制規則。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Store.BeginTx is a transaction factory: returned storeTx intentionally transfers ownership to caller, so deferring Rollback here would invalidate every returned transaction. Audited 30 production s.BeginTx sites: 26 direct deferred Rollback, 3 deferred closures calling Rollback, and tryBeginRulePublication rolls back on acquisition failure then transfers to beginRulePublication. Its four owners PublishRules/RepairLegacyMiddleware/InitializeRules/RestoreRulesV2Marker defer Rollback and explicitly commit. Ownership comment preserved; no suppression added.

</details>

## SONAR-0432 — S7739

- 原報告位置：`web/check-rule-graph.mjs:352`
- 原報告頁碼：47, 88
- 狀態：**待情境審查，尚未關閉**

這裡的 then 是不可呼叫的 JSON 分支陣列或雙語標籤陣列，不是 Promise 的 then 方法。更名會破壞既有規則 AST 或翻譯契約；需依欄位實際型別審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review as false positive: then is a non-callable JSON rule-language branch (array) or bilingual field-label entry (array), never a Promise-like then method. Renaming changes the backend wire AST/translation lookup contract.

</details>

## SONAR-0435 — S7739

- 原報告位置：`web/src/utils/ruleLabels.ts:94`
- 原報告頁碼：50, 95
- 狀態：**待情境審查，尚未關閉**

這裡的 then 是不可呼叫的 JSON 分支陣列或雙語標籤陣列，不是 Promise 的 then 方法。更名會破壞既有規則 AST 或翻譯契約；需依欄位實際型別審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review as false positive: then is a non-callable JSON rule-language branch (array) or bilingual field-label entry (array), never a Promise-like then method. Renaming changes the backend wire AST/translation lookup contract.

</details>

## SONAR-0545 — S6819

- 原報告位置：`web/src/components/rules/RuleNodeSearch.vue:102`
- 原報告頁碼：57
- 狀態：**待情境審查，尚未關閉**

這是具搜尋、說明及類別樣式的複合 combobox／ARIA listbox；原生 select 不能等價保留內容及互動。鍵盤與 active-descendant 行為保留，仍需真實瀏覽器及輔助科技驗證。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Rich searchable combobox requires ARIA listbox containing descriptions and category styling; native select cannot preserve the existing rich-option presentation. Keyboard active-descendant behavior retained; manual Sonar disposition required.

</details>

## SONAR-0546 — S6819

- 原報告位置：`web/src/components/rules/RuleNodeSearch.vue:103`
- 原報告頁碼：57
- 狀態：**待情境審查，尚未關閉**

這是富內容 listbox 的 option 按鈕，搭配 ARIA 與鍵盤選取；原生 option 不能等價保留富內容。需驗證選取、焦點及輔助科技行為後作情境審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Custom option buttons belong to the rich ARIA listbox/combobox pattern and support keyboard selection; native option cannot preserve their existing rich content. Manual Sonar disposition required.

</details>

## SONAR-0640 — S2077

- 原報告位置：`internal/store/dialect.go:127`
- 原報告頁碼：65, 105
- 狀態：**待情境審查，尚未關閉**

Store.ExecContext 只對 query 做 Rebind，args 仍獨立交給 database/sql。Rebind 是 placeholder 轉換，不是任意 SQL 的消毒器；已檢視 RecordResult 使用固定 UPDATE，status／error／quota／id／time 均參數綁定。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Store.ExecContext passes Rebind(query) and args... separately to database/sql; Rebind rewrites only static dialect syntax/placeholder positions and never incorporates args. Inspected external production caller accounts/scheduling.go RecordResult: UPDATE text is fixed, status/error/quota/id/time values are bound. Wrapper is not a sanitizer; safety contract is caller-owned SQL templates, not arbitrary user SQL.

</details>

## SONAR-0641 — S2077

- 原報告位置：`internal/store/dialect.go:130`
- 原報告頁碼：65, 105
- 狀態：**待情境審查，尚未關閉**

Store.QueryContext 保留獨立參數；ListKeys 使用固定 SELECT 與常數 keyColumns，篩選條件由固定片段組成，值由 args 傳入。此理由不代表 wrapper 可安全執行任意來源 SQL。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Store.QueryContext forwards Rebind(query), args... separately. Example ListKeys uses literal SELECT plus constant keyColumns; no request strings enter SQL. Dynamic filters elsewhere are assembled from static predicate fragments with values retained in args. This forwarding wrapper itself does not concatenate values, and is not claimed to sanitize arbitrary SQL.

</details>

## SONAR-0642 — S2077

- 原報告位置：`internal/store/dialect.go:133`
- 原報告頁碼：65, 105
- 狀態：**待情境審查，尚未關閉**

Store.QueryRowContext 的 GetKey／GetKeyByValue 經 loadKey 使用固定 SELECT 與 keyColumns，id／value 綁定；GetRule 亦綁定 id，未將請求輸入用作 identifier。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Store.QueryRowContext forwards Rebind(query), args... separately. GetKey/GetKeyByValue call loadKey with static SELECT+constant keyColumns and id/value as a distinct bound arg; GetRule similarly binds id. No identifier or value is derived from a request at this wrapper.

</details>

## SONAR-0643 — S2077

- 原報告位置：`internal/store/dialect.go:156`
- 原報告頁碼：65, 105
- 狀態：**待情境審查，尚未關閉**

storeTx.ExecContext 分開傳送 query／args；CreateKey／UpdateKey 的 name、key、label、strategy 均綁定；DeleteKey 的三個 DELETE 為固定模板並綁定 id。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

storeTx.ExecContext has same separate template/args boundary on an existing transaction. Reviewed CreateKey/UpdateKey bind all name/key/label/strategy values; DeleteKey loops over three constant DELETE templates and binds id. Rebind does not interpolate values.

</details>

## SONAR-0644 — S2077

- 原報告位置：`internal/store/dialect.go:159`
- 原報告頁碼：65, 105
- 狀態：**待情境審查，尚未關閉**

storeTx.QueryContext 的 ListUsageRecords 以固定 usageWhere predicates 建構 SQL，搜尋／篩選值及 LIMIT／OFFSET 均綁定；規則查詢使用常數 ruleColumns／ruleOrder。這不是所有動態 SQL 都安全的通用保證。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

storeTx.QueryContext forwards separate arguments. Reviewed ListUsageRecords: usageWhere builds fixed column/predicate fragments, Search/filter values stay in args, LIMIT/OFFSET are placeholders with appended args; rule-set reads concatenate only constant ruleColumns/ruleOrder. Not a generic arbitrary-SQL safety guarantee.

</details>

## SONAR-0645 — S2077

- 原報告位置：`internal/store/dialect.go:162`
- 原報告頁碼：65, 105
- 狀態：**待情境審查，尚未關閉**

storeTx.QueryRowContext 的 usage count 使用固定 where 片段及獨立參數；規則 metadata 使用 literal SQL。需依實際呼叫端判定，而非將通用 wrapper 誤認成消毒器。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

storeTx.QueryRowContext forwards separate arguments. Reviewed ListUsageRecords count query: where consists of code-owned clauses and filter values remain args; rule metadata reads are literal SQL. No args are interpolated by Rebind or wrapper.

</details>

## SONAR-0646 — S2077

- 原報告位置：`internal/store/dialect.go:272`
- 原報告頁碼：65, 105
- 狀態：**待情境審查，尚未關閉**

migratePostgres 的 ALTER 表名、欄名、definition 皆源自程式內新建的 additiveColumns literal；postgresSchema 僅使用固定型別 token。資料列、請求、環境 DSN 或 HTTP 輸入均不進入 DDL。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

migratePostgres ALTER TABLE receives table/name/definition exclusively by iterating additiveColumns(), a fresh map literal in source. postgresSchema only replaces fixed SQL type tokens. Neither persisted rows, request data, environment DSN nor HTTP parameters supply these DDL identifiers/definitions.

</details>

## SONAR-0647 — S2077

- 原報告位置：`internal/store/key_migration.go:115`
- 原報告頁碼：65, 105
- 狀態：**待情境審查，尚未關閉**

重建 api_keys 的 CREATE 來自編譯期 schemaSQL、固定終止符與固定 api_keys_new 名稱；舊資料列不進入 DDL，複製欄位為常數 keyColumns，id／群組名稱／sequence 均綁定。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Legacy key-table rebuild extracts CREATE TABLE api_keys definition from compile-time schemaSQL, truncates at static terminator, and replaces fixed table name api_keys_new. ExecContext definition cannot contain values from legacy rows. The later row-copy SQL uses compile-time keyColumns; legacy IDs, group names and sequence are bound separately.

</details>

## SONAR-0653 — S9423

- 原報告位置：`web/src/style.css:1157`
- 原報告頁碼：66
- 狀態：**待情境審查，尚未關閉**

prefers-reduced-motion 的 !important 是刻意保護使用者減少動態效果偏好，避免被元件／utility 的動畫樣式蓋過。沒有整體重設 cascade 就移除可能造成可及性退化；需實際瀏覽器驗證並審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Global prefers-reduced-motion safety override deliberately retains !important so component rules and utility animations cannot override the user accessibility preference. Removing it without restructuring all animation cascade layers would regress reduced-motion support; manual Sonar disposition required.

</details>

## SONAR-0654 — S9423

- 原報告位置：`web/src/style.css:1158`
- 原報告頁碼：66
- 狀態：**待情境審查，尚未關閉**

prefers-reduced-motion 的 !important 是刻意保護使用者減少動態效果偏好，避免被元件／utility 的動畫樣式蓋過。沒有整體重設 cascade 就移除可能造成可及性退化；需實際瀏覽器驗證並審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Global prefers-reduced-motion safety override deliberately retains !important so component rules and utility animations cannot override the user accessibility preference. Removing it without restructuring all animation cascade layers would regress reduced-motion support; manual Sonar disposition required.

</details>

## SONAR-0655 — S9423

- 原報告位置：`web/src/style.css:1159`
- 原報告頁碼：66
- 狀態：**待情境審查，尚未關閉**

prefers-reduced-motion 的 !important 是刻意保護使用者減少動態效果偏好，避免被元件／utility 的動畫樣式蓋過。沒有整體重設 cascade 就移除可能造成可及性退化；需實際瀏覽器驗證並審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Global prefers-reduced-motion safety override deliberately retains !important so component rules and utility animations cannot override the user accessibility preference. Removing it without restructuring all animation cascade layers would regress reduced-motion support; manual Sonar disposition required.

</details>

## SONAR-0656 — S9423

- 原報告位置：`web/src/style.css:1160`
- 原報告頁碼：66
- 狀態：**待情境審查，尚未關閉**

prefers-reduced-motion 的 !important 是刻意保護使用者減少動態效果偏好，避免被元件／utility 的動畫樣式蓋過。沒有整體重設 cascade 就移除可能造成可及性退化；需實際瀏覽器驗證並審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Global prefers-reduced-motion safety override deliberately retains !important so component rules and utility animations cannot override the user accessibility preference. Removing it without restructuring all animation cascade layers would regress reduced-motion support; manual Sonar disposition required.

</details>

## SONAR-0684 — S8242

- 原報告位置：`internal/accounts/codex_version_test.go:407`
- 原報告頁碼：71
- 狀態：**待情境審查，尚未關閉**

僅測試用的 Context adapter，為觀察 Done 及 body read 的取消時機而保留／嵌入父 context；不是正式程式將請求 context 長期存在業務結構中。需以 test double 用途判定。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Test-only context adapter intentionally observes Done/body-read cancellation; embedding/preserving parent context is required for the context.Context test double. Not application context storage.

</details>

## SONAR-0685 — S8242

- 原報告位置：`internal/accounts/codex_version_test.go:606`
- 原報告頁碼：71
- 狀態：**待情境審查，尚未關閉**

僅測試用的 Context adapter，為觀察 Done 及 body read 的取消時機而保留／嵌入父 context；不是正式程式將請求 context 長期存在業務結構中。需以 test double 用途判定。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Test-only context adapter intentionally observes Done/body-read cancellation; embedding/preserving parent context is required for the context.Context test double. Not application context storage.

</details>

## SONAR-0694 — S6845

- 原報告位置：`web/src/components/capture/CaptureDiff.vue:57`
- 原報告頁碼：74, 118
- 狀態：**待情境審查，尚未關閉**

此 overflow:auto 區域具可辨識名稱，tabindex=0 讓鍵盤使用者能聚焦並捲動內容。直接移除焦點會損害操作能力；需以真實鍵盤及輔助科技驗證後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

The diff is an overflow:auto keyboard-scrollable region with an accessible name. tabindex=0 is intentionally retained so keyboard-only users can scroll; removing it would regress access. Manual Sonar disposition required.

</details>

## SONAR-0714 — S2245

- 原報告位置：`web/src/utils/editorId.ts:3`
- 原報告頁碼：91
- 狀態：**待情境審查，尚未關閉**

Math.random 僅用於編輯器 ID 的跨 realm 碰撞鹽值，不是驗證、憑證或伺服器識別碼。優先使用 randomUUID／getRandomValues；單純改成時鐘值的 fallback 反而較弱。需依實際非安全用途審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Security hotspot review required: Math.random supplies an editor-only cross-realm collision salt, never credentials/authentication/server-managed IDs. Secure randomUUID/getRandomValues paths are preferred; clock-only replacement would weaken fallback collision resistance.

</details>

## SONAR-0722 — S9382

- 原報告位置：`web/check-quota-cost.mjs:96`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0723 — S9382

- 原報告位置：`web/check-quota-cost.mjs:102`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0724 — S9382

- 原報告位置：`web/check-quota-cost.mjs:114`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0725 — S9382

- 原報告位置：`web/check-rule-components.mjs:216`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0726 — S9382

- 原報告位置：`web/check-rule-components.mjs:313`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0727 — S9382

- 原報告位置：`web/check-rule-components.mjs:316`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0728 — S9382

- 原報告位置：`web/check-quota-cost.mjs:76`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0729 — S9382

- 原報告位置：`web/check-quota-cost.mjs:81`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0730 — S9382

- 原報告位置：`web/check-usage-billing.mjs:68`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0731 — S9382

- 原報告位置：`web/check-usage-billing.mjs:73`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0732 — S9382

- 原報告位置：`web/check-usage-billing.mjs:75`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0733 — S9382

- 原報告位置：`web/check-usage-billing.mjs:80`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0734 — S9382

- 原報告位置：`web/check-usage-billing.mjs:83`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0735 — S9382

- 原報告位置：`web/check-capture-rule-traces.mjs:41`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0736 — S9382

- 原報告位置：`web/check-rule-components.mjs:125`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0737 — S9382

- 原報告位置：`web/check-rule-components.mjs:157`
- 原報告頁碼：100
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0738 — S9382

- 原報告位置：`web/check-rule-components.mjs:212`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0739 — S9382

- 原報告位置：`web/check-rule-components.mjs:220`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0740 — S9382

- 原報告位置：`web/check-rule-components.mjs:231`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0741 — S9382

- 原報告位置：`web/check-rule-components.mjs:255`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0742 — S9382

- 原報告位置：`web/check-rule-components.mjs:261`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0743 — S9382

- 原報告位置：`web/check-rule-components.mjs:305`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0744 — S9382

- 原報告位置：`web/check-rule-components.mjs:308`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0745 — S9382

- 原報告位置：`web/check-rule-components.mjs:318`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0746 — S9382

- 原報告位置：`web/check-rule-components.mjs:319`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0747 — S9382

- 原報告位置：`web/check-rule-components.mjs:327`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0748 — S9382

- 原報告位置：`web/check-rule-components.mjs:328`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0752 — S9382

- 原報告位置：`web/check-model-catalog.mjs:223`
- 原報告頁碼：101
- 狀態：**待情境審查，尚未關閉**

這組檢查有順序相依：Vue locale／render fixture 或同一 mounted component 的變動，需要 nextTick 後檢查中間狀態；遞迴 compileModule 亦依序更新共享依賴快取與來源 import 替換。平行執行會改變測試語意；改寫 Promise chain 只是規避規則。請依本列來源位置確認所屬流程後審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, ordering is intentional: tests mutate shared Vue locale/render fixtures or the same mounted component and assert intermediate states after nextTick; parallel execution changes semantics. Recursive compileModule also updates shared dependency cache/source import replacements in dependency order. No promise-chain rewrite was used merely to evade this diagnostic.

</details>

## SONAR-0794 — S5332

- 原報告位置：`cmd/pi-gateway/main.go:487`
- 原報告頁碼：108
- 狀態：**待情境審查，尚未關閉**

啟動訊息描述實際 HTTP listener；HTTPS 終止由 reverse proxy 負責。只更改印出的 scheme 會錯誤宣稱 listener 已提供 TLS。需審查部署的外部 TLS 邊界。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Only startup display of actual HTTP listener; deployment HTTPS termination must be configured at reverse proxy. Replacing printed scheme would falsely advertise TLS.

</details>

## SONAR-0795 — S5332

- 原報告位置：`cmd/pi-gateway/runtime.go:69`
- 原報告頁碼：108
- 狀態：**待情境審查，尚未關閉**

這是無憑證的本地健康探針；wildcard bind 會解析為 loopback，目標 listener 本身是 HTTP 而非 TLS。需保留實際協定並審查暴露範圍。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Credential-free health probe to configured local listener; wildcard binds mapped to loopback. Listener implements HTTP, not TLS.

</details>

## SONAR-0796 — S5332

- 原報告位置：`internal/config/config.go:645`
- 原報告頁碼：108
- 狀態：**待情境審查，尚未關閉**

operator 指定 HTTP 上游時應對應 WS、HTTPS 時對應 WSS。擅自改成安全 scheme 會破壞既有部署；應審查明文僅用於可信網路／loopback 的設定邊界。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Explicit operator-configured HTTP upstream maps to matching WS; HTTPS maps to WSS. Must not rewrite scheme without changing upstream deployment. Review trusted-network/loopback use of HTTP configuration.

</details>

## SONAR-0797 — S5332

- 原報告位置：`internal/oauth/chatgpt.go:64`
- 原報告頁碼：108
- 狀態：**待情境審查，尚未關閉**

固定 localhost OAuth redirect，空 host 解析到 localhost，並使用隨機 state、PKCE、嚴格 callback origin／path 檢查。改成 HTTPS 會破壞 provider 已註冊的 redirect；需依本地 OAuth 模式審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

OAuth redirect used by flow is fixed localhost loopback (empty host argument resolves localhost), protected by random state + PKCE and strict callback origin/path validation; HTTPS would break provider-registered redirect.

</details>

## SONAR-0813 — S7747

- 原報告位置：`web/src/utils/ruleGraph.ts:447`
- 原報告頁碼：114
- 狀態：**待情境審查，尚未關閉**

graphNodeClosure 刻意快照最初選取的 roots，並用不同 body mode 處理 descendants；改成走訪即時變動集合會改變處理範圍。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review as required snapshot: graphNodeClosure iterates the initial selected roots while recursively adding descendants with distinct body-mode traversal semantics; addValueDependencies iterates original owners while appending new value nodes. Iterating the live collection changes traversal boundaries.

</details>

## SONAR-0814 — S7747

- 原報告位置：`web/src/utils/ruleGraph.ts:623`
- 原報告頁碼：114
- 狀態：**待情境審查，尚未關閉**

addValueDependencies 刻意走訪原始 owners，同時新增 value nodes；改成即時走訪新增節點會擴大迭代邊界與語意。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review as required snapshot: graphNodeClosure iterates the initial selected roots while recursively adding descendants with distinct body-mode traversal semantics; addValueDependencies iterates original owners while appending new value nodes. Iterating the live collection changes traversal boundaries.

</details>

## SONAR-0818 — S7784

- 原報告位置：`web/src/utils/ruleSchema.ts:167`
- 原報告頁碼：116
- 狀態：**待情境審查，尚未關閉**

這裡是 JSON wire 資料的正規化，不是任意物件複製：輸入可能為 Vue reactive proxy，JSON 會移除非 wire 的 undefined／reactive 欄位並產生獨立 AST。structuredClone 不具相同接受範圍及輸出語意，不能直接替換。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, intentional JSON-domain normalization: live Vue reactive proxies cannot be structuredClone-d; JSON serialization removes non-wire undefined properties and produces a detached JSON AST. structuredClone is not behavior-equivalent. The component test normalization likewise strips reactive wrappers/non-JSON fields.

</details>

## SONAR-0819 — S7784

- 原報告位置：`web/src/utils/ruleGraph.ts:220`
- 原報告頁碼：116
- 狀態：**待情境審查，尚未關閉**

這裡是 JSON wire 資料的正規化，不是任意物件複製：輸入可能為 Vue reactive proxy，JSON 會移除非 wire 的 undefined／reactive 欄位並產生獨立 AST。structuredClone 不具相同接受範圍及輸出語意，不能直接替換。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, intentional JSON-domain normalization: live Vue reactive proxies cannot be structuredClone-d; JSON serialization removes non-wire undefined properties and produces a detached JSON AST. structuredClone is not behavior-equivalent. The component test normalization likewise strips reactive wrappers/non-JSON fields.

</details>

## SONAR-0820 — S7784

- 原報告位置：`web/src/utils/ruleEditor.ts:5`
- 原報告頁碼：116
- 狀態：**待情境審查，尚未關閉**

這裡是 JSON wire 資料的正規化，不是任意物件複製：輸入可能為 Vue reactive proxy，JSON 會移除非 wire 的 undefined／reactive 欄位並產生獨立 AST。structuredClone 不具相同接受範圍及輸出語意，不能直接替換。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, intentional JSON-domain normalization: live Vue reactive proxies cannot be structuredClone-d; JSON serialization removes non-wire undefined properties and produces a detached JSON AST. structuredClone is not behavior-equivalent. The component test normalization likewise strips reactive wrappers/non-JSON fields.

</details>

## SONAR-0834 — S4036

- 原報告位置：`web/check-rules.mjs:13`
- 原報告頁碼：129
- 狀態：**待情境審查，尚未關閉**

這是開發／CI 透過可信 PATH 尋找 Go toolchain 的入口，不由正式請求指定可執行檔或參數。保留可攜式工具鏈解析；需要確認 CI／開發環境 PATH 不含不可信目錄。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Security review required: local developer/CI test harness invokes trusted Go compiler via developer-selected PATH; no request or production-server data controls executable/arguments. Portable compiler discovery is intentional; environment must supply a trusted toolchain PATH.

</details>

## SONAR-0835 — S7784

- 原報告位置：`web/check-rule-components.mjs:145`
- 原報告頁碼：131
- 狀態：**待情境審查，尚未關閉**

這裡是 JSON wire 資料的正規化，不是任意物件複製：輸入可能為 Vue reactive proxy，JSON 會移除非 wire 的 undefined／reactive 欄位並產生獨立 AST。structuredClone 不具相同接受範圍及輸出語意，不能直接替換。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, intentional JSON-domain normalization: live Vue reactive proxies cannot be structuredClone-d; JSON serialization removes non-wire undefined properties and produces a detached JSON AST. structuredClone is not behavior-equivalent. The component test normalization likewise strips reactive wrappers/non-JSON fields.

</details>

## SONAR-0836 — S1874

- 原報告位置：`web/src/utils/clipboard.ts:20`
- 原報告頁碼：132
- 狀態：**待情境審查，尚未關閉**

先使用 navigator.clipboard，再以 execCommand(copy) 支援 HTTP LAN 或 Clipboard API 受限的既有環境；finally 仍還原焦點及選取狀態，失敗會拋錯。直接刪除會破壞仍支援的複製功能，需依相容性政策審查。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Review required, compatibility fallback intentionally retained: navigator.clipboard.writeText is tried first; execCommand(copy) supports LAN HTTP/restricted Clipboard API contexts, restores prior focus/selection in finally and throws on failure. Removing it drops explicitly supported copy behavior.

</details>

## SONAR-0837 — S4036

- 原報告位置：`web/e2e/catalog.setup.ts:8`
- 原報告頁碼：133
- 狀態：**待情境審查，尚未關閉**

這是開發／CI 透過可信 PATH 尋找 Go toolchain 的入口，不由正式請求指定可執行檔或參數。保留可攜式工具鏈解析；需要確認 CI／開發環境 PATH 不含不可信目錄。

<details>
<summary>原始逐項處置證據（英文，供核對）</summary>

Security review required: local developer/CI test harness invokes trusted Go compiler via developer-selected PATH; no request or production-server data controls executable/arguments. Portable compiler discovery is intentional; environment must supply a trusted toolchain PATH.

</details>
