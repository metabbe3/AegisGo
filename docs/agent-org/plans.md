# Plans — AegisGo

Satu-satunya tempat rencana hidup. Status: `[ ]` todo · `[~]` berjalan ·
`[x]` selesai · `[!]]` blocked. Update barisnya saat status berubah —
jangan hapus histori; item selesai tinggal diberi tanggal.

---

## P1 — Core agent loop (Now)

- [x] **First REAL gated L2 tool** — /reload_rules via ReloadGate
  (ADR-0007), 2026-09-22.
- [ ] **REST HITL endpoints**: `POST /v1/approvals/{id}/decision` —
  jalan ketiga selain teks & tombol (untuk CLI/script).
- [ ] **Edit-message on decision**: pesan approval di-edit jadi
  "✅ approved by … at …" (bukan cuma toast) — riwayat chat rapi.

## P2 — Reliability & ops

- [ ] **launchd hardening**: log rotasi (serve.log grow check), plist
  `LowPriorityIO`, alert bila crash-loop (ThrottleInterval melambat).
- [ ] **Backpressure survei**: klaim inbox saat Telegram 5xx panjang —
  ukur release/retry path dengan test chaos kecil.
- [ ] **e2e baru**: `scripts/e2e.sh` tambah skenario tombol (callback
  update masuk inbox → decide → toast) end-to-end.

## P3 — Mining & smart routing (Next)

- [ ] **N1 YAML manifest** untuk rule hot-reload tanpa reseed SQL.
- [ ] **Miner v1 kuery ulang**: dominance threshold tuning dengan data
  fallback asli (butuh corpus > N event dulu — cek
  `SELECT count(*) FROM fallback_events`).
- [ ] **Logprob-POC (jev-watch)**: ditahan sampai ada titik keputusan
  nyata yang konsumsi confidence (lihat handoff session 5).

## P4 — Interface polish

- [ ] **Bot /status**: uptime serve, versi commit, jumlah rule, depth
  inbox (deterministic, no-LLM).
- [ ] **Approval card format**: payload JSON → render field-per-baris
  untuk payload besar (sekarang dipotong 120 char).

## Done (2026-09-22)

- [x] Semua branch → main (13 feat/fix/docs/chore lokal + 1 remote
  upstream refactor/simplify-pass; e2e 10 PASS + S11 SKIP ollama
  known; single-binary `aegis serve`, plist updated)

- [x] First gated L2 action /reload_rules (ADR-0007, feat/first-gated-tool)

- [x] docs foundation + SDLC (34255ca)
- [x] e2e baseline 11/11 (d192ebb)
- [x] L-tier policy engine (def8691, ADR-0002)
- [x] HITL ledger v4 (20f2de8, ADR-0003)
- [x] Telegram HITL commands (f547e1d, ADR-0004)
- [x] RunGated executor + AI-free replies (5b84977, ADR-0005)
- [x] Approval notifier (5a4a43d, ADR-0006) + observability (37f767d)
- [x] launchd service com.aegisgo.serve (6410459)
- [x] Interactive approval buttons ✅/🚫 (eb850e2)
- [x] docs/org-memory: lessons-learned.md + plans.md (branch ini)

## Blocked / Ditolak

- (kosong — catat keputusan tolak/blocked di sini beserta alasan &
  tanggal, biar gak diusulkan ulang)
