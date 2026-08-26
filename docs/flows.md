# catet — Flows

## 1. Graph & Analytics data flow (end-to-end)

```
Postgres → Go aggregation layer → JSON series → Next.js Route Handler → ECharts
```

Every chart follows the same pipeline. The Go API is the **single source of truth for numbers** — Next.js never does math, only renders.

**Per-chart computation (all in Go):**

1. **Safe-to-Spend reference line**
   - `STS = Σ asset balances − Σ(bills due within income-horizon) − Σ(active goal commitments)`
   - Chart = STS as a horizontal line over a 4-week spend timeline. STS is a *now* scalar; the line is flat, the bars are historical daily outflow.
   - Recompute STS on every transaction (hero is always live, never cached).

2. **Runway curve**
   - `runway(t) = STS_now ÷ trailing-burn(t)`, where `trailing-burn(t)` = mean daily net-outflow over the prior 14 days ending at `t`.
   - Plotted as a line over the same 4-week window; the *shape* comes from burn history, the *level* from today's STS.

3. **Income-vs-expense timeline**
   - Weekly buckets (Mon–Sun, Asia/Jakarta). Two series: income (green) vs expense (red), bar+line.
   - Income = Σ income legs; expense = Σ expense legs (transfers excluded).

4. **Daily heatmap**
   - Intensity = daily outflow (expenses only). ECharts calendar heatmap, year view.
   - Anomaly overlay: days where `|outflow − trailing-14d-mean| > 2σ` get a badge; clicking opens the flagged-days list.

5. **Category breakdown**
   - Top-level donut; click a slice to drill into children. Leaf amounts always sum to parent.
   - Period-scoped (week/month/quarter selector).

6. **Essential vs Lifestyle split**
   - Static from `category.essential` flags. Two-segment bar; lifestyle segment is the "spending creep" signal.

7. **Top merchants**
   - Rank by outflow over the selected period, top N.

**Freshness policy:** hero + ritual text = live (per-transaction recompute). Charts = cached via TanStack Query `staleTime` (e.g. 60s), invalidated on write.

## 2. Onboarding flow

```
Dashboard → create Sources + starting balances → mark Essential categories
→ (Next) debt snapshot: outstanding balances per paylater platform
→ "Confirm & Create" — nothing written before confirm
```

First WhatsApp message → bot points to the dashboard. No chat wizard.

## 3. Daily pull ritual (`rekap hari ini`)

```
User: "rekap hari ini"
  → intent: ritual
  → Tier-0 reads: today's spend, STS, tomorrow's reserved, uncategorized
  → narration: verdict-first, numbers attached
  → "Hari ini: 45rb · sisa aman 2.1jt · besok reserved 300k · 2 belum kategori"
```

## 4. Weekly review ritual

```
User: "rekap minggu ini" (or dashboard card)
  → weekly spend recap · unusual days (anomaly) · uncategorized count · top merchants
  → each uncategorized item fixable inline (reply-to-edit)
```

## 5. Capture gate flow (fact vs label)

```
Message → keyword rules (deterministic) → LLM structured parse (whitelist-resolved)
  ├─ passes → two-leg write + confirm card (Simpan/Ubah/Batal)
  └─ fails  → Hold Queue → one specific question → resolve → write
```

Category is always a soft guess; low confidence → Needs-Review queue, never blocks.

## 6. Confirmation / PendingAction flow

```
Tier 1–3 tool call → parked as PendingAction → "ya/tidak" matches directly (no 2nd LLM call)
  "ubah jadi X" → re-invoke model with draft + correction → reconfirm
```

## 7. Transfer flow

```
"pindah 500k ke gopay" → two legs (bank −500k, gopay +500k) → one card, atomic write
```

## 8. Adjustment entry flow

```
Dashboard: "set balance" → writes an adjustment leg (system → source) → drift resolved, auditable
```

## 9. Reversal flow

```
"refund gojek 30rb" → linked reversal (reverses_transaction_id) → both visible in history
```

## 10. Bill matching flow (Next)

```
Incoming payment → match against Bill (amount band + description) → mark paid → feed calendar/reserved
```

## 11. Budget threshold flow

```
spent ÷ budget per category → 80% amber badge, 100% red badge → dashboard only (never push)
```

## 12. Search/filter flow

```
Full-text (merchant/note) + structured filters (source, category, tag, amount, date, direction, review) → sort/paginate
```

## 13. Auth flow

```
Passkey (WebAuthn) → httpOnly session cookie → Next.js edge → Go API (internal, JWT forwarded server-side)
```

## 14. Backup flow

```
Nightly encrypted pg_dump → off-box storage → gateway-disconnect alert via ops channel (not the bot)
```
