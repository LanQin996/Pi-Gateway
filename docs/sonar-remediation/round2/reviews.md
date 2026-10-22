# 第二輪 56 項待情境審查

本文件逐項保留 **56 項尚未關閉** 的報告列。中文說明不取代個別原始證據與實際來源檢視；請在 SonarCloud 重分析後逐項記錄適當處置，不要批次宣告誤報或已達 A。原始行號對應第二份 PDF，非重構後行號。

[第二輪總覽](README.md) · [完整 169 項 CSV](findings.csv)

保留情境包含 SQL 綁定／靜態 DDL、交易所有權、測試 context、wire then 欄位、順序 await、HTTP 部署、可及性、獨立測試 oracle、非 credential 隨機 salt 與剪貼簿 fallback。

## SONAR2-0006 — S8168

- 原報告位置：`internal/store/dialect.go:176`
- 報告頁碼：13, 14
- 狀態：**待情境審查，尚未關閉**

BeginTx 是交付活交易所有權的 factory；在 factory 內 defer rollback 會使回傳的交易立即失效。需確認所有最終消費端負責 commit／deferred rollback，再進行服務端審查。新增 early return／panic／cancel 邊界測試，而非破壞所有權契約。

<details>
<summary>本項原始處置證據（供核對）</summary>

BeginTx is an ownership-transfer factory: after successful database/sql BeginTx it immediately returns a storeTx embedding the live sql.Tx. Deferring rollback inside the factory would invalidate the returned transaction. Exhaustive AST/source audit found 38 production acquisition sites: 35 immediate deferred-rollback consumers, 3 ownership-transfer factories (BeginTx, tryBeginRulePublication, beginRulePublication). Publication lock failure explicitly rolls back before retry/return; all 5 final publication consumers defer rollback. No uncovered transaction lifetime observed. New real-database regressions cover commit, early-error return, panic unwinding and cancellation. This is a Reliability finding requiring a Sonar false-positive decision, NOT a code-fixed leak.

</details>

## SONAR2-0007 — S7739

- 原報告位置：`web/check-rule-graph.mjs:369`
- 報告頁碼：15, 24
- 狀態：**待情境審查，尚未關閉**

這裡的 then 是不可呼叫的 JSON 分支陣列或雙語標籤陣列，不是 Promise 的 then 方法。更名會破壞既有規則 AST 或翻譯契約；需依欄位實際型別審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Required backend wire JSON branch then is an array, never callable. Retained literal plus Array.isArray contract assertion; renaming breaks serialization and relocating it merely to hide the rule was rejected. Existing nested workflow roundtrips validate exact then/else AST. Needs Sonar false-positive disposition, not a behavioral change.

</details>

## SONAR2-0010 — S2077

- 原報告位置：`internal/store/db.go:531`
- 報告頁碼：17, 36
- 狀態：**待情境審查，尚未關閉**

此處為 SQL wrapper 或固定 schema／projection 拼接。已依實際呼叫端檢查資料值與 args 分離，以及表名／欄名／DDL 來源限制；下面逐項證據記錄具體路徑。這不是任意 SQL 均安全的宣告，必須依個別來源及信任邊界作服務端安全審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

ALTER TABLE identifiers/type syntax cannot be bound as values. ensureColumns calls validateMigrationColumns before database access: table must exist in freshly constructed additiveColumns registry, every supplied column must exist there, and its entire definition must exactly equal the registered literal. Invalid table/column/definition injection tests reject before touching a nil connection; registered-column migration/default/idempotence tests pass. Remaining formatting accepts only static registered DDL, not caller text.

</details>

## SONAR2-0011 — S2077

- 原報告位置：`internal/store/dialect.go:151`
- 報告頁碼：17, 36
- 狀態：**待情境審查，尚未關閉**

此處為 SQL wrapper 或固定 schema／projection 拼接。已依實際呼叫端檢查資料值與 args 分離，以及表名／欄名／DDL 來源限制；下面逐項證據記錄具體路徑。這不是任意 SQL 均安全的宣告，必須依個別來源及信任邊界作服務端安全審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Store.ExecContext passes s.Rebind(query) and args... separately to database/sql. Rebind receives no argument values and cannot interpolate them. Current external production caller internal/accounts/scheduling.go supplies a literal UPDATE; store updates build SET fragments from fixed field names (accountManagementUpdate/appendAccountCapacityUpdate), placing names, credentials, URLs, IDs and other data only in args. New SQLite/PostgreSQL contract inserts SQL-looking bound strings and confirms exact values and preserved table.

</details>

## SONAR2-0012 — S2077

- 原報告位置：`internal/store/dialect.go:154`
- 報告頁碼：17, 36
- 狀態：**待情境審查，尚未關閉**

此處為 SQL wrapper 或固定 schema／projection 拼接。已依實際呼叫端檢查資料值與 args 分離，以及表名／欄名／DDL 來源限制；下面逐項證據記錄具體路徑。這不是任意 SQL 均安全的宣告，必須依個別來源及信任邊界作服務端安全審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Store.QueryContext preserves the separate args slice. Current usageWhere/CaptureFilter.sqlWhere/proxy filters construct clauses from literals and bind all filter/search values. Capture projection is selected between two fixed projections; UsageTrend interpolates only int64 bucket sizes chosen by hour/day switch; usageDimensions chooses fixed columns by switch; percentileSQL callers supply literal ttft/tps and 0.5/0.9/0.95. No user-provided identifier/order/query text crosses this API in inspected production callers. New SQLite/PostgreSQL rows regression verifies SQL-looking values do not broaden predicates.

</details>

## SONAR2-0013 — S2077

- 原報告位置：`internal/store/dialect.go:157`
- 報告頁碼：17, 36
- 狀態：**待情境審查，尚未關閉**

此處為 SQL wrapper 或固定 schema／projection 拼接。已依實際呼叫端檢查資料值與 args 分離，以及表名／欄名／DDL 來源限制；下面逐項證據記錄具體路徑。這不是任意 SQL 均安全的宣告，必須依個別來源及信任邊界作服務端安全審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Store.QueryRowContext preserves args through placeholder rewriting, including search/count queries assembled from fixed predicates and column constants. insertID adds literal RETURNING id to internal static INSERT statements and still passes values separately. New SQLite/PostgreSQL query-row contract round-trips SQL-looking bound values unchanged; no string escaping is used as a substitute for binding.

</details>

## SONAR2-0014 — S2077

- 原報告位置：`internal/store/dialect.go:185`
- 報告頁碼：17, 36
- 狀態：**待情境審查，尚未關閉**

此處為 SQL wrapper 或固定 schema／projection 拼接。已依實際呼叫端檢查資料值與 args 分離，以及表名／欄名／DDL 來源限制；下面逐項證據記錄具體路徑。這不是任意 SQL 均安全的宣告，必須依個別來源及信任邊界作服務端安全審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

storeTx.ExecContext passes tx.store.Rebind(query) and original args separately. Transactional mutation callsites use literal INSERT/UPDATE/DELETE or static SET lists; accountManagementUpdate appends only fixed field clauses and binds patch values. Delete loops iterate literal statements and bind IDs. New SQLite/PostgreSQL transactional UPDATE with SQL-looking replacement data verifies argument binding and subsequent commit.

</details>

## SONAR2-0015 — S2077

- 原報告位置：`internal/store/dialect.go:188`
- 報告頁碼：17, 36
- 狀態：**待情境審查，尚未關閉**

此處為 SQL wrapper 或固定 schema／projection 拼接。已依實際呼叫端檢查資料值與 args 分離，以及表名／欄名／DDL 來源限制；下面逐項證據記錄具體路徑。這不是任意 SQL 均安全的宣告，必須依個別來源及信任邊界作服務端安全審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

storeTx.QueryContext keeps placeholders and values separate. Rule/usage count-page queries concatenate only static column/order constants and filters selected from fixed clauses; search, phase, enabled flags, limits and offsets are bound args. New SQLite/PostgreSQL transactional QueryContext validates exactly one matching SQL-looking value and unchanged contents.

</details>

## SONAR2-0016 — S2077

- 原報告位置：`internal/store/dialect.go:191`
- 報告頁碼：17, 36
- 狀態：**待情境審查，尚未關閉**

此處為 SQL wrapper 或固定 schema／projection 拼接。已依實際呼叫端檢查資料值與 args 分離，以及表名／欄名／DDL 來源限制；下面逐項證據記錄具體路徑。這不是任意 SQL 均安全的宣告，必須依個別來源及信任邊界作服務端安全審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

storeTx.QueryRowContext keeps original args separate from Rebind. Snapshot/count/existence/ID read paths use constant projections/predicates and bind IDs, search values and other data. New SQLite/PostgreSQL transactional QueryRowContext verifies SQL-looking strings remain values and are returned exactly, not executed.

</details>

## SONAR2-0017 — S2077

- 原報告位置：`internal/store/dialect.go:301`
- 報告頁碼：17, 36
- 狀態：**待情境審查，尚未關閉**

此處為 SQL wrapper 或固定 schema／projection 拼接。已依實際呼叫端檢查資料值與 args 分離，以及表名／欄名／DDL 來源限制；下面逐項證據記錄具體路徑。這不是任意 SQL 均安全的宣告，必須依個別來源及信任邊界作服務端安全審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

PostgreSQL additive schema loop ranges over additiveColumns(), which constructs a fresh literal table/column/definition registry with no input parameters or runtime configuration. postgresSchema applies fixed SQLite-to-PostgreSQL type replacements to those literal definitions. DDL identifiers/types cannot be bound, but no request/config/database value is used in this formatted ALTER TABLE. Existing real PostgreSQL fresh migration/reopen/idempotence tests exercise it.

</details>

## SONAR2-0018 — S2077

- 原報告位置：`internal/store/key_migration.go:115`
- 報告頁碼：17, 36
- 狀態：**待情境審查，尚未關閉**

此處為 SQL wrapper 或固定 schema／projection 拼接。已依實際呼叫端檢查資料值與 args 分離，以及表名／欄名／DDL 來源限制；下面逐項證據記錄具體路徑。這不是任意 SQL 均安全的宣告，必須依個別來源及信任邊界作服務端安全審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

The concatenated INSERT/SELECT projection is a compile-time constant: keyColumns is a const literal in internal/store/keys.go, and all other operands are SQL literals. No data or identifiers are supplied by requests/config/database rows. Retaining the shared canonical projection avoids duplicated column lists silently diverging during legacy rebuilds. Existing legacy key migration tests verify migrated data and sequence high-water mark preservation.

</details>

## SONAR2-0019 — S9423

- 原報告位置：`web/src/style.css:1160`
- 報告頁碼：18
- 狀態：**待情境審查，尚未關閉**

prefers-reduced-motion 的 !important 是刻意保護使用者減少動態效果偏好，避免被元件／utility 的動畫樣式蓋過。沒有整體重設 cascade 就移除可能造成可及性退化；需實際瀏覽器驗證並審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Global prefers-reduced-motion override intentionally wins over scoped component/utility and inline motion declarations; removing important or raising only selector specificity cannot override inline durations or future component rules. Retained documented accessibility safety override; needs reviewed exception.

</details>

## SONAR2-0020 — S9423

- 原報告位置：`web/src/style.css:1161`
- 報告頁碼：18
- 狀態：**待情境審查，尚未關閉**

prefers-reduced-motion 的 !important 是刻意保護使用者減少動態效果偏好，避免被元件／utility 的動畫樣式蓋過。沒有整體重設 cascade 就移除可能造成可及性退化；需實際瀏覽器驗證並審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Global prefers-reduced-motion override intentionally wins over scoped component/utility and inline motion declarations; removing important or raising only selector specificity cannot override inline durations or future component rules. Retained documented accessibility safety override; needs reviewed exception.

</details>

## SONAR2-0021 — S9423

- 原報告位置：`web/src/style.css:1162`
- 報告頁碼：18
- 狀態：**待情境審查，尚未關閉**

prefers-reduced-motion 的 !important 是刻意保護使用者減少動態效果偏好，避免被元件／utility 的動畫樣式蓋過。沒有整體重設 cascade 就移除可能造成可及性退化；需實際瀏覽器驗證並審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Global prefers-reduced-motion override intentionally wins over scoped component/utility and inline motion declarations; removing important or raising only selector specificity cannot override inline durations or future component rules. Retained documented accessibility safety override; needs reviewed exception.

</details>

## SONAR2-0022 — S9423

- 原報告位置：`web/src/style.css:1163`
- 報告頁碼：18
- 狀態：**待情境審查，尚未關閉**

prefers-reduced-motion 的 !important 是刻意保護使用者減少動態效果偏好，避免被元件／utility 的動畫樣式蓋過。沒有整體重設 cascade 就移除可能造成可及性退化；需實際瀏覽器驗證並審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Global prefers-reduced-motion override intentionally wins over scoped component/utility and inline motion declarations; removing important or raising only selector specificity cannot override inline durations or future component rules. Retained documented accessibility safety override; needs reviewed exception.

</details>

## SONAR2-0023 — S6819

- 原報告位置：`web/src/components/rules/RuleNodeSearch.vue:106`
- 報告頁碼：19
- 狀態：**待情境審查，尚未關閉**

搜尋型複合選擇器使用 combobox／listbox active-descendant，包含標題、說明、badge 與方向鍵／Home／End／Enter／Escape 操作。原生 select／option 不等價；需真實瀏覽器與輔助科技確認後審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Rich searchable module chooser follows combobox/listbox active-descendant pattern with title/help/badges, arrow/Home/End/Enter/Escape handling. Native select/options cannot preserve its rich cross-device layout and search behavior without feature redesign. Existing first-round remote browser tests passed; second-round browser rerun awaits approval/CI. Needs accessibility review exception.

</details>

## SONAR2-0024 — S6819

- 原報告位置：`web/src/components/rules/RuleNodeSearch.vue:107`
- 報告頁碼：19
- 狀態：**待情境審查，尚未關閉**

搜尋型複合選擇器使用 combobox／listbox active-descendant，包含標題、說明、badge 與方向鍵／Home／End／Enter／Escape 操作。原生 select／option 不等價；需真實瀏覽器與輔助科技確認後審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Rich searchable module chooser follows combobox/listbox active-descendant pattern with title/help/badges, arrow/Home/End/Enter/Escape handling. Native select/options cannot preserve its rich cross-device layout and search behavior without feature redesign. Existing first-round remote browser tests passed; second-round browser rerun awaits approval/CI. Needs accessibility review exception.

</details>

## SONAR2-0025 — S6845

- 原報告位置：`web/src/components/rules/RuleCanvas.vue:563`
- 報告頁碼：20, 40
- 狀態：**待情境審查，尚未關閉**

焦點是刻意的鍵盤操作設計：canvas 接收 Space／undo／duplicate 並恢復 Escape 焦點；diff overflow 區域則必須可聚焦捲動。不是裝飾性 tab stop，移除可能損害可及性。請依本列檔案及下方證據確認。

<details>
<summary>本項原始處置證據（供核對）</summary>

Focus is intentional accessibility, not a decorative tab stop. Canvas focus restores Escape destination and receives Space/undo/duplicate keyboard shortcuts (explicit existing E2E focus assertions). Overflowing payload diff must remain keyboard-scrollable even without interactive children. Removing tabindex breaks keyboard behavior; needs reviewed false-positive/exception.

</details>

## SONAR2-0026 — S6845

- 原報告位置：`web/src/components/capture/CaptureDiff.vue:74`
- 報告頁碼：20, 40
- 狀態：**待情境審查，尚未關閉**

焦點是刻意的鍵盤操作設計：canvas 接收 Space／undo／duplicate 並恢復 Escape 焦點；diff overflow 區域則必須可聚焦捲動。不是裝飾性 tab stop，移除可能損害可及性。請依本列檔案及下方證據確認。

<details>
<summary>本項原始處置證據（供核對）</summary>

Focus is intentional accessibility, not a decorative tab stop. Canvas focus restores Escape destination and receives Space/undo/duplicate keyboard shortcuts (explicit existing E2E focus assertions). Overflowing payload diff must remain keyboard-scrollable even without interactive children. Removing tabindex breaks keyboard behavior; needs reviewed false-positive/exception.

</details>

## SONAR2-0027 — S8242

- 原報告位置：`internal/accounts/codex_version_test.go:349`
- 報告頁碼：21
- 狀態：**待情境審查，尚未關閉**

測試 adapter 明確保留 test-scoped context，用於觀察 Done／body read 取消時機並保留完整 context contract。method value／closure 同樣會延長 context lifetime，故不以隱藏欄位當成修復；保留結構並要求服務端情境審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

獨立審查後撤回以method values／closure隱藏context欄位的改寫：那不會改變保留期間，不能冒稱生命週期修復。此測試限定adapter需要保留parent context以觀察Done被讀取，body observer需要觀察實際request context的deadline/cancellation。保留明確欄位及原取消測試，新增完整context契約測試；需情境審查，不是已修復資源洩漏。

</details>

## SONAR2-0028 — S8242

- 原報告位置：`internal/accounts/codex_version_test.go:497`
- 報告頁碼：21
- 狀態：**待情境審查，尚未關閉**

測試 adapter 明確保留 test-scoped context，用於觀察 Done／body read 取消時機並保留完整 context contract。method value／closure 同樣會延長 context lifetime，故不以隱藏欄位當成修復；保留結構並要求服務端情境審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

獨立審查後撤回以method values／closure隱藏context欄位的改寫：那不會改變保留期間，不能冒稱生命週期修復。此測試限定adapter需要保留parent context以觀察Done被讀取，body observer需要觀察實際request context的deadline/cancellation。保留明確欄位及原取消測試，新增完整context契約測試；需情境審查，不是已修復資源洩漏。

</details>

## SONAR2-0032 — S2245

- 原報告位置：`web/src/utils/editorId.ts:4`
- 報告頁碼：26
- 狀態：**待情境審查，尚未關閉**

Math.random 僅用於編輯器 ID 的跨 realm 碰撞鹽值，不是驗證、憑證或伺服器識別碼。優先使用 randomUUID／getRandomValues；單純改成時鐘值的 fallback 反而較弱。需依實際非安全用途審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Secure random namespace is preferred when crypto exists. No-crypto legacy contexts retain Math.random solely as an editor identity collision salt, never credentials or server authorization. Removing this entropy caused equal/coarsened-clock cross-realm collisions and was rejected during audit. Two independent module realms under identical clocks plus distinct injected entropy now differ;2000 fixed-clock local IDs remain unique. Requires security hotspot safe-use review; no universal uniqueness guarantee.

</details>

## SONAR2-0125 — S9382

- 原報告位置：`web/check-quota-cost.mjs:96`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by all render helpers; assertions depend on that locale. Awaited renders stay serial to avoid language races.

</details>

## SONAR2-0126 — S9382

- 原報告位置：`web/check-quota-cost.mjs:102`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by all render helpers; assertions depend on that locale. Awaited renders stay serial to avoid language races.

</details>

## SONAR2-0127 — S9382

- 原報告位置：`web/check-quota-cost.mjs:114`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by all render helpers; assertions depend on that locale. Awaited renders stay serial to avoid language races.

</details>

## SONAR2-0128 — S9382

- 原報告位置：`web/check-rule-components.mjs:219`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0129 — S9382

- 原報告位置：`web/check-rule-components.mjs:324`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0130 — S9382

- 原報告位置：`web/check-rule-components.mjs:327`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0131 — S9382

- 原報告位置：`web/check-quota-cost.mjs:76`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by all render helpers; assertions depend on that locale. Awaited renders stay serial to avoid language races.

</details>

## SONAR2-0132 — S9382

- 原報告位置：`web/check-quota-cost.mjs:81`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by all render helpers; assertions depend on that locale. Awaited renders stay serial to avoid language races.

</details>

## SONAR2-0133 — S9382

- 原報告位置：`web/check-usage-billing.mjs:68`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by imported money/i18n helpers. Intermediate badge/billing assertions stay serialized; parallelism can mix labels/rates.

</details>

## SONAR2-0134 — S9382

- 原報告位置：`web/check-usage-billing.mjs:73`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by imported money/i18n helpers. Intermediate badge/billing assertions stay serialized; parallelism can mix labels/rates.

</details>

## SONAR2-0135 — S9382

- 原報告位置：`web/check-usage-billing.mjs:75`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by imported money/i18n helpers. Intermediate badge/billing assertions stay serialized; parallelism can mix labels/rates.

</details>

## SONAR2-0136 — S9382

- 原報告位置：`web/check-usage-billing.mjs:80`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by imported money/i18n helpers. Intermediate badge/billing assertions stay serialized; parallelism can mix labels/rates.

</details>

## SONAR2-0137 — S9382

- 原報告位置：`web/check-usage-billing.mjs:83`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

逐語言檢查會變動共享 locale，money／i18n／render helper 讀取同一 closure。必須先完成該語言的 render 及中間斷言，不能平行改動造成語系競態。

<details>
<summary>本項原始處置證據（供核對）</summary>

Per-language loop mutates shared locale closure used by imported money/i18n helpers. Intermediate badge/billing assertions stay serialized; parallelism can mix labels/rates.

</details>

## SONAR2-0138 — S9382

- 原報告位置：`web/check-capture-rule-traces.mjs:41`
- 報告頁碼：34
- 狀態：**待情境審查，尚未關閉**

遞迴 component loader 按順序解析依賴、更新共享 module cache 並替換來源 imports。平行化需要另行建立具循環安全性的 promise-cache 設計；不能直接改 Promise.all 或換寫法隱藏規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Recursive component-module loader resolves dependency imports into shared module cache and rewrites source imports in order; concurrent compilation would need cycle-safe promise-cache redesign, not mechanical Promise.all.

</details>

## SONAR2-0139 — S9382

- 原報告位置：`web/check-rule-components.mjs:125`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0140 — S9382

- 原報告位置：`web/check-rule-components.mjs:157`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0141 — S9382

- 原報告位置：`web/check-rule-components.mjs:212`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0142 — S9382

- 原報告位置：`web/check-rule-components.mjs:223`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0143 — S9382

- 原報告位置：`web/check-rule-components.mjs:237`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0144 — S9382

- 原報告位置：`web/check-rule-components.mjs:263`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0145 — S9382

- 原報告位置：`web/check-rule-components.mjs:269`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0146 — S9382

- 原報告位置：`web/check-rule-components.mjs:316`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0147 — S9382

- 原報告位置：`web/check-rule-components.mjs:319`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0148 — S9382

- 原報告位置：`web/check-rule-components.mjs:329`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0149 — S9382

- 原報告位置：`web/check-rule-components.mjs:330`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0150 — S9382

- 原報告位置：`web/check-rule-components.mjs:338`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0151 — S9382

- 原報告位置：`web/check-rule-components.mjs:339`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

真實 Vue 測試依序修改同一 mounted control，等待 nextTick 後驗證中間 mode／value／options 狀態，並更新共享 coverage counters。平行化會改變測試語意，不以 Promise chain 換寫法迴避規則。

<details>
<summary>本項原始處置證據（供核對）</summary>

Real Vue tests edit shared mounted controls through nextTick and assert intermediate mode/value/options states; also update shared coverage counters. Source/encoding/field/roundtrip loops deliberately preserve interaction order.

</details>

## SONAR2-0152 — S9382

- 原報告位置：`web/check-model-catalog.mjs:231`
- 報告頁碼：35
- 狀態：**待情境審查，尚未關閉**

SSR cases 會變動共享 reactive locale 與同一 selector 的 fixtures，必須等待 render 結束後再改下一組測試資料。

<details>
<summary>本項原始處置證據（供核對）</summary>

SSR cases change shared reactive locale and fixtures used by the same compiled selector. Awaited renders must finish before the next mutation.

</details>

## SONAR2-0153 — S5332

- 原報告位置：`cmd/pi-gateway/main.go:421`
- 報告頁碼：37
- 狀態：**待情境審查，尚未關閉**

此為實際 HTTP listener、健康探針、配置的 WS 上游或本地 OAuth redirect。不能僅更改 scheme 偽裝 TLS；需依本列部署／回環／憑證／provider 契約判定。自訂非回環設定並非普遍安全背書。

<details>
<summary>本項原始處置證據（供核對）</summary>

此處僅記錄目前原生 HTTP listener 的真實位址，不應謊報 HTTPS。原生預設回環；Compose 對外映射或自訂非回環部署仍必須以防火牆／HTTPS反向代理保護。這不是任何部署均安全的宣告，需部署者確認傳輸邊界後審查 hotspot。

</details>

## SONAR2-0154 — S5332

- 原報告位置：`cmd/pi-gateway/runtime.go:69`
- 報告頁碼：37
- 狀態：**待情境審查，尚未關閉**

此為實際 HTTP listener、健康探針、配置的 WS 上游或本地 OAuth redirect。不能僅更改 scheme 偽裝 TLS；需依本列部署／回環／憑證／provider 契約判定。自訂非回環設定並非普遍安全背書。

<details>
<summary>本項原始處置證據（供核對）</summary>

唯讀 healthcheck預設／wildcard listen address轉回環，只請求 /healthz、不攜帶認證，停用環境proxy及redirect且2秒timeout。自訂具體host仍由受信部署配置控制；不可無憑證就改成HTTPS。

</details>

## SONAR2-0155 — S5332

- 原報告位置：`internal/config/config.go:570`
- 報告頁碼：37
- 狀態：**待情境審查，尚未關閉**

此為實際 HTTP listener、健康探針、配置的 WS 上游或本地 OAuth redirect。不能僅更改 scheme 偽裝 TLS；需依本列部署／回環／憑證／provider 契約判定。自訂非回環設定並非普遍安全背書。

<details>
<summary>本項原始處置證據（供核對）</summary>

HTTP→WS和HTTPS→WSS是配置上游的等價協定轉換，不會將HTTPS降為WS。預設上游HTTPS；本機測試／受信代理仍需支援HTTP。非受信網路部署必須使用HTTPS/WSS，不能單靠此converter保證安全。

</details>

## SONAR2-0156 — S5332

- 原報告位置：`internal/oauth/chatgpt.go:64`
- 報告頁碼：37
- 狀態：**待情境審查，尚未關閉**

此為實際 HTTP listener、健康探針、配置的 WS 上游或本地 OAuth redirect。不能僅更改 scheme 偽裝 TLS；需依本列部署／回環／憑證／provider 契約判定。自訂非回環設定並非普遍安全背書。

<details>
<summary>本項原始處置證據（供核對）</summary>

OAuth原生應用回環callback依RFC8252§7.3/§8.3使用HTTP；production flow以預設127.0.0.1建立redirect URI且使用PKCE/state，不能改HTTPS破壞註冊redirect。自訂host並非普遍安全背書，僅回環情境符合此理由。

</details>

## SONAR2-0167 — S7784

- 原報告位置：`web/check-rule-components.mjs:145`
- 報告頁碼：46
- 狀態：**待情境審查，尚未關閉**

保留 JSON stringify／parse 作刻意獨立於正式 cloneJson 的 wire-format 測試 oracle。若使用同一正式實作正規化實際與預期結果，會掩蓋共同缺陷。這是測試可信度的選擇，需保留審查而不是改寫隱藏。

<details>
<summary>本項原始處置證據（供核對）</summary>

Retained JSON serialization/deserialization as an intentionally independent wire-format test oracle. Reusing production cloneJson made actual and expected results share the implementation under test; audit rejected that weakening. Production clones use explicit reactive-aware copier, but regression expected values must remain independently normalized.

</details>

## SONAR2-0168 — S1874

- 原報告位置：`web/src/utils/clipboard.ts:20`
- 報告頁碼：47
- 狀態：**待情境審查，尚未關閉**

先使用 navigator.clipboard，再以 execCommand(copy) 支援 HTTP LAN 或 Clipboard API 受限的既有環境；finally 仍還原焦點及選取狀態，失敗會拋錯。直接刪除會破壞仍支援的複製功能，需依相容性政策審查。

<details>
<summary>本項原始處置證據（供核對）</summary>

Clipboard API remains first choice; deprecated execCommand only supplies supported LAN HTTP/permission-restricted fallback, with explicit failure and finally focus/selection restoration. Removing it loses existing copy capability; requires product compatibility decision, not metric-only removal.

</details>
