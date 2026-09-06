# Victoria Gateway — 下一批功能設計草稿

> 狀態：草稿，待確認後實作
> 日期：2026-08-27

---

## 功能一覽

| # | 功能 | 一句話描述 |
|---|------|-----------|
| 1 | 相似事件呈現 | RAG 命中時，Telegram 推送附上相似歷史事件連結（Issue URL 或內建 `/incidents/{id}` 頁面） |
| 2 | 維護窗 | 指定時間範圍 + 匹配規則，命中的 alert 靜默或降級處理 |
| 3 | 多 channel 路由 | 按 label 條件把推送導向不同 Telegram 群組或 generic webhook |

---

## 1. 相似事件呈現

### 設計決策

RAG 回傳的是向量相似度，不是精確匹配——直接把上次 resolution 當「建議處置」塞進 Telegram 容易誤導值班工程師。改為：

- **有 issue 連結的 Confirmed 記錄**：在 Telegram 推送底部附 Issue URL，工程師自己點過去看完整脈絡再判斷。
- **無 issue 連結的 Confirmed 記錄**（`note --alert-name` 手動建的）：提供內建 `/incidents/{id}` 唯讀頁面，顯示 alertname / host / summary / resolution / created_at / similarity score。

兩者並存，讓所有 Confirmed 記錄都有辦法被連結到。

### Config Schema

不需要新的 config。相似事件呈現使用既有的 `rag` 設定（`enabled`、`top_k`、tracker endpoint/owner/repo）。

新增一個可選控制項（加在 `rag` 區塊下）：

```yaml
rag:
  # ... 既有欄位 ...
  show_similar_in_notification: true   # 預設 true（RAG enabled 時）；false 可關閉
  similarity_threshold: 0.75           # cosine similarity ≥ 此值才顯示；預設 0.75
```

### 內建 Web 頁面

| 路徑 | 用途 |
|------|------|
| `GET /incidents/{id}` | 單筆事件詳情（只顯示 Confirmed） |
| `GET /incidents` | 最近 N 筆 Confirmed 事件列表（預設 20，`?limit=50` 可調） |

頁面用 `html/template` 產生，不引入前端框架。CSS inline 極簡，以 dark theme 為主（值班工程師多半在深夜看手機）。

### 影響範圍

| 檔案/Package | 變更內容 |
|-------------|---------|
| `pkg/rag/store.go` | `Search` 回傳新增 cosine distance（目前 SQL 沒 SELECT 距離值） |
| `pkg/config/config.go` | `RAGConfig` 加兩個欄位 |
| `cmd/victoria-gateway/main.go` | `notifyTelegram` 加「相似事件」段落；新增 `/incidents` + `/incidents/{id}` handler |
| `cmd/victoria-gateway/incidents.go`（新檔） | web handler + HTML template |
| `cmd/victoria-gateway/main_test.go` | 新增 `/incidents` 相關測試 |

### Telegram 訊息格式變化

```
🚨 <b>NodeExporterDown</b> (172.16.100.7)

看起來是 node_exporter process 掛了，systemd 顯示 OOMKilled...

📎 相似歷史事件：
 • #12 NodeExporterDown (172.16.100.6) — 2026-07-15
   https://gitea.ngu.tw/admin/victoria-gateway-incidents/issues/12
 • incident/8 — InstanceDown (172.16.100.7) — 2026-06-20
   https://victoria-gateway:8090/incidents/8
```

只列 similarity ≥ threshold 的前 top_k 筆。有 issue 連結的優先顯示 issue URL，沒有的用內建 `/incidents/{id}` URL。

---

## 2. 維護窗（Maintenance Windows）

### 設計決策

- 兩種時間模式：**週期性**（`schedule`，類 cron 但更人類可讀）和**一次性**（`start`/`end` ISO8601）。
- 兩種動作：`suppress`（完全跳過，不分析、不推送、不 capture）和 `mute`（照常分析 + RAG capture，但不推 Telegram——維護後想回顧時有記錄）。
- 匹配規則支援 `alertname`、`host`、`severity`，值支援完全比對和 glob（`*` wildcard）。
- 多個 window 可同時存在，任一命中即生效（OR 邏輯）。
- 維護窗的判斷在 **dedup 之後、Loki query 之前**（`suppress`）或 **Telegram push 之前**（`mute`），這樣 `mute` 模式下分析結果還是會出現在 webhook JSON response 和 RAG 裡。

### Config Schema

```yaml
maintenance_windows:
  - name: "weekly-reboot"              # 人類辨識用，log 會印
    schedule: "SAT 02:00-04:00"        # 格式：DOW HH:MM-HH:MM（24h，本地時間）
    matchers:
      host: "172.16.100.*"
    action: suppress                   # suppress | mute

  - name: "storage-migration"
    start: "2026-09-01T22:00:00+08:00"
    end: "2026-09-02T06:00:00+08:00"
    matchers:
      alertname: "DiskSpace*"
      host: "172.16.100.6"
    action: mute

  - name: "monthly-patching"
    schedule: "1st-SUN 03:00-06:00"    # 每月第一個週日
    matchers:
      severity: "warning"
    action: suppress
```

**`schedule` 格式定義：**

| 格式 | 範例 | 意義 |
|------|------|------|
| `DOW HH:MM-HH:MM` | `SAT 02:00-04:00` | 每週六 02:00~04:00 |
| `{N}th-DOW HH:MM-HH:MM` | `1st-SUN 03:00-06:00` | 每月第一個週日 |
| `DAILY HH:MM-HH:MM` | `DAILY 04:00-04:30` | 每天 04:00~04:30 |

跨日（例如 `SAT 23:00-02:00`）：表示從週六 23:00 到週日 02:00。

### 影響範圍

| 檔案/Package | 變更內容 |
|-------------|---------|
| `pkg/config/config.go` | 新增 `MaintenanceWindows []MaintenanceWindow` 欄位 |
| `pkg/maintenance/`（新 package） | `Window` 解析 + `IsActive(time, labels)` 判斷邏輯 |
| `pkg/maintenance/maintenance_test.go` | schedule 解析 + 匹配 + 跨日等測試 |
| `cmd/victoria-gateway/main.go` | handler 的 `summarizeOne` 開頭加維護窗判斷 |
| `pkg/metrics/metrics.go` | 新增 `maintenance_suppressed_total` / `maintenance_muted_total` counter |

### 判斷流程

```
webhook payload 進來
  → dedup（既有）
  → 維護窗判斷：
      match + suppress → 直接 skip，log "suppressed by window <name>"，計 metric
      match + mute    → 設 flag，後續照常分析，但 notifyTelegram 跳過
  → Loki query → summarize → ... → notifyTelegram（檢查 mute flag）
```

### Validate 規則

- `schedule` 跟 `start`/`end` 二擇一，不可同時設、也不可都不設。
- `matchers` 至少要有一個（不允許空 matcher 命中所有 alert）。
- `action` 必填，只接受 `suppress` 或 `mute`。
- 啟動時解析 schedule，格式不合法直接 fatal。

---

## 3. 多 Channel 路由

### 設計決策

- 現有的 `telegram` 頂層區塊改為 **fallback / default channel**（向後相容：沒設 `notifications` 區塊時行為完全不變）。
- 新增 `notifications` 區塊，定義 channels + routes。
- channel type 先支援 `telegram` 和 `webhook`（generic HTTP POST）。
- 路由是**扁平 list，first-match**，最後一條 `default: true` 當 fallback。沒有巢狀。
- 一條 route 可以指向多個 channels（例如嚴重的同時推 Telegram + 打 webhook 到 ITSM）。
- `matchers` 語法跟維護窗一致（label name → value，支援 glob）。

### Config Schema

```yaml
# 向後相容：只有 telegram 區塊、沒有 notifications 時，行為跟現在完全一樣
telegram:
  bot_token: "..."
  chat_id: -100123456789

# 進階用法：定義多 channel + 路由規則
notifications:
  channels:
    - name: "critical-ops"
      type: telegram
      bot_token: "..."
      chat_id: -100123456789

    - name: "info-archive"
      type: telegram
      bot_token: "..."
      chat_id: -100987654321

    - name: "itsm-webhook"
      type: webhook
      url: "http://internal-itsm/api/v1/alerts"
      # headers:                     # optional
      #   Authorization: "Bearer ..."
      # method: POST                 # optional, defaults to POST

  routes:
    - matchers:
        severity: critical
        alertname: "Instance*"
      channels: ["critical-ops", "itsm-webhook"]

    - matchers:
        severity: warning
      channels: ["critical-ops"]

    - default: true
      channels: ["info-archive"]
```

**向後相容規則：**

| 情境 | 行為 |
|------|------|
| 只設 `telegram`，沒有 `notifications` | 跟現在完全一樣，所有 alert 推到這個 chat |
| 設了 `notifications`，沒設 `telegram` | 純用路由，頂層 `telegram` 不需要 |
| 兩者都設 | `telegram` 視為隱含的 default channel（等同 `notifications.routes` 最後一條 `default: true` 指向它），如果 `notifications.routes` 已有明確 `default: true` 則忽略頂層 `telegram` 並印 warning |

### Webhook channel 行為

`type: webhook` 的 channel 對目標 URL 做 HTTP POST，body 是跟現有 webhook response 一樣結構的 JSON：

```json
{
  "alert_name": "InstanceDown",
  "host": "172.16.100.7",
  "summary": "...",
  "analyzed_by": "local"
}
```

回應非 2xx 視為失敗，記 log + metric，不 retry（跟 Telegram 推送失敗一樣的策略：best-effort，不阻塞主流程）。

### 影響範圍

| 檔案/Package | 變更內容 |
|-------------|---------|
| `pkg/config/config.go` | 新增 `Notifications *NotificationsConfig`；`Validate()` 加向後相容邏輯 |
| `pkg/notify/`（新 package） | `Channel` interface + `TelegramChannel` / `WebhookChannel` 實作 + `Router`（match routes → dispatch） |
| `pkg/notify/notify_test.go` | route matching + fallback + multi-channel dispatch 測試 |
| `cmd/victoria-gateway/main.go` | handler 改用 `notify.Router.Dispatch(res, labels)` 取代直接呼叫 `h.notifyTelegram(res)` |
| `pkg/aiops/telegram.go` | 可能被 `pkg/notify/telegram.go` 取代或包裝（程式碼搬移，不改行為） |
| `pkg/metrics/metrics.go` | per-channel 推送成功/失敗 counter |

---

## 實作順序

```
Phase 1 — 維護窗（功能 2）
  ↓ 獨立性最高、不碰既有推送邏輯、使用者（你）馬上能用
Phase 2 — 多 channel 路由（功能 3）
  ↓ 重構推送路徑，之後功能 1 的「把相似事件連結附在推送裡」才有穩定的推送介面可以接
Phase 3 — 相似事件呈現（功能 1）
  ↓ 依賴 Phase 2 的 notify 介面 + 需要加 web handler
```

理由：
- Phase 1 完全在 webhook handler 的前半段加一個 gate，不碰後半段的推送/RAG/capture 邏輯。
- Phase 2 把 Telegram 推送抽成 `pkg/notify`，是功能 1 需要的基底（否則改 `notifyTelegram` 的同時又要支援多 channel 會變得更複雜）。
- Phase 3 同時碰推送格式和新增 web endpoint，在 Phase 2 穩定後做最乾淨。

---

## 測試策略

### 功能 1（相似事件呈現）

| 層級 | 測試重點 |
|------|---------|
| Unit | `store.Search` 回傳 similarity score 的正確性（mock pgvector）；`/incidents/{id}` handler 返回正確 HTML（httptest） |
| Unit | Telegram 格式化函式：有 issue 連結 vs 無 issue 連結 vs 低於 threshold 不顯示 |
| Integration | 真實 Postgres（`rag-quickstart` compose）：插 Confirmed 記錄 → Search 回傳 score → 確認格式化結果含 URL |

### 功能 2（維護窗）

| 層級 | 測試重點 |
|------|---------|
| Unit | schedule 解析（每種格式 + 不合法格式 error） |
| Unit | `IsActive(time, labels)` 各 case：命中 suppress / 命中 mute / 不命中 / glob 匹配 / 跨日 schedule |
| Unit | `Validate()` 拒絕空 matchers、兩種時間模式衝突 |
| E2E | handler 層面：mock time + 送一個落在維護窗內的 alert，確認 response 有 "suppressed" 標記、Telegram 未被呼叫 |

### 功能 3（多 channel 路由）

| 層級 | 測試重點 |
|------|---------|
| Unit | route matching（first-match 語意、glob、multi-matcher AND 邏輯） |
| Unit | 向後相容：只設 `telegram` 沒設 `notifications` 時行為不變 |
| Unit | `WebhookChannel` 呼叫行為（httptest 驗 body/headers） |
| Unit | 一條 route 多 channel：兩個都被呼叫、一個失敗不影響另一個 |
| Integration | handler 層面：送兩個不同 severity 的 alert，驗各自被路由到正確 channel |

### 共通

- 每個 Phase 完成後跑 `go build` / `go vet` / `go test -race -count=1 ./...` 全 package 綠燈。
- 新增的 config 欄位都加入 `config_test.go` 驗證 YAML 解析 + `Validate()` 邊界 case。
- CI（既有 GitHub Actions）不需要額外改動——已涵蓋上述命令。

---

## 未涵蓋 / 未來方向

- Slack/Discord channel type — 架構已預留（`Channel` interface），等有需求再加。
- ~~`/incidents` 頁面加搜尋/篩選~~ ✅ **已完成（2026-09-06）**：`?alertname=`/`?host=` 子字串篩選（大小寫不敏感，兩者 AND），頁面加 sticky 篩選表單。`pkg/rag.Store.ListConfirmed` 簽名改為吃 `ListFilter`。仍不做全文搜尋（只比對 alert_name/host 兩欄）。
- ~~維護窗的動態管理 API（runtime 新增/取消 window）~~ ✅ **已完成（2026-09-06）**：`GET`/`PUT /maintenance-windows`，沿用 `webhook_auth` 同一套 Basic Auth；`PUT` 全量替換、驗證失敗不動現有生效中的窗口。詳見 README「Hot-reloading windows」章節。
- ~~跨日 schedule 的 timezone 處理~~ ✅ **已完成（2026-09-06）**：`MaintenanceWindow` 新增 `timezone`（IANA 時區名，選填），只影響 `schedule` 窗口；沒設就維持原本用呼叫端 `time.Time` 自帶時區的行為，完全回歸相容。
