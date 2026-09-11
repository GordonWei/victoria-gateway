# _archive

這裡的檔案是 **2026-09-09** 封存的。依「只搬移不刪除」原則，檔案只從原位置搬進來，
**內容未經任何修改**（連錯字都沒動），也沒有刪除任何檔案。
本目錄的搬移以 `git mv` 執行，搬移本身於 2026-09-11 才 commit 進版控。

⚠️ 這個目錄裡的檔案不是交付標的。

## 封存清單與理由

### `_draft_victoria_gateway_next_features.md`

- **原位置**：`victoria-gateway/docs/_draft_victoria_gateway_next_features.md`
- **理由**：**三項主功能全數實作完畢**。維護窗（二十三度）、相似事件呈現＋多 channel
  路由（`../_agent_handoff.md:175-184` 二十五度，新增 `pkg/notify`、`/incidents`）。
  `../_agent_handoff.md:20`：「`docs/_draft_victoria_gateway_next_features.md`
  「未涵蓋/未來方向」7 項已標記完成」；草稿第 301–303 行三條「未來方向」皆已加刪除線
  ＋「✅ 已完成（2026-09-06）」。
  剩下的「Slack/Discord channel type」草稿自述「架構已預留，等有需求再加」，
  已獨立轉錄成交接檔二十八度的追蹤點，封存不遺失資訊。

---

## 刻意**未**封存的兩份（⚠️ 有安全性理由）

`../_draft_improvement_ideas_20260831.md` 與 `../_draft_kiro_review_round2.md`
兩份**證據上都夠格封存**（前者檔頭自述「使用者裁示『都做』，#1～#9 已全部實作完成」；
後者 `../_agent_handoff.md:95` 標題直書「已過時」），但**本次刻意不搬**：

本 repo 的 `.gitignore:30-31` 是以**完整路徑**釘住這兩個檔名的——

    docs/_draft_improvement_ideas_20260831.md
    docs/_draft_kiro_review_round2.md

搬進 `docs/_archive/` 後這兩條規則就不再匹配（實測 `git check-ignore` 已不命中），
而 `_draft_improvement_ideas_20260831.md` **第 56 行含 gitea remote 的明文帳密**
（見 `../_agent_handoff.md:85`）。若在未同步修改 `.gitignore` 的情況下搬移，
等於讓一個含明文憑證的檔案變成可被 `git add -A` 掃進版控的狀態。

🔴 **2026-09-11 裁示：本目錄進版控（不加進 `.gitignore`）。**
因此上述風險現在是**實際存在的**，不再只是假設——`docs/_archive/` 是被追蹤的目錄，
任何搬進來的檔案都會被 `git add -A` 掃進這個公開 repo。

**這兩份（以及任何含憑證的草稿）不得搬進本目錄。** 若日後要封存它們，
必須改走別的路徑並同步在 `.gitignore` 釘住，那是另一次獨立的變更。
