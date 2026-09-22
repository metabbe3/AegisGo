package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"aegisgo/internal/store"
)

// HITL REST endpoints (ADR-0008): a third decision path next to typed
// commands and inline buttons — for CLI callers, scripts, and any future
// dashboards. Same store, same pending-only CAS: whatever path decides
// first wins, the others see "not pending" honestly.
//
//	GET  /v1/approvals          list pending
//	POST /v1/approvals/{id}/decision   {"decision":"approve"|"deny","by":"..."}
//
// Security posture: localhost-bound service (same as every other admin
// surface here); the bot's chat allowlist is NOT reused — REST is for
// machine callers on the box (scripts, aegis ctl). Decisions carry a
// `by` identity for the audit trail.

// ApprovalSource is the subset of *store.Store the handlers need.
type ApprovalSource interface {
	PendingApprovals(ctx context.Context, limit int) ([]store.Approval, error)
	GetApproval(ctx context.Context, id int64) (store.Approval, bool, error)
	DecideApproval(ctx context.Context, id int64, state, decidedBy string) (bool, error)
}

// approvalJSON is the wire shape of one approval.
type approvalJSON struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	Kind      string `json:"kind"`
	Payload   string `json:"payload"`
	Reason    string `json:"reason"`
	State     string `json:"state"`
	DecidedBy string `json:"decided_by,omitempty"`
	DecidedAt string `json:"decided_at,omitempty"`
}

// decisionRequest is the body of POST /v1/approvals/{id}/decision.
type decisionRequest struct {
	Decision string `json:"decision"` // "approve" | "deny"
	By       string `json:"by"`       // caller identity for the ledger
}

func approvalToJSON(a store.Approval) approvalJSON {
	return approvalJSON{
		ID: a.ID, CreatedAt: a.CreatedAt, Kind: a.Kind, Payload: a.Payload,
		Reason: a.Reason, State: a.State, DecidedBy: a.DecidedBy, DecidedAt: a.DecidedAt,
	}
}

// listApprovals handles GET /v1/approvals.
func listApprovals(w http.ResponseWriter, r *http.Request, src ApprovalSource) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	pend, err := src.PendingApprovals(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing approvals: "+err.Error())
		return
	}
	out := make([]approvalJSON, 0, len(pend))
	for _, a := range pend {
		out = append(out, approvalToJSON(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": out, "count": len(out)})
}

// decideApproval handles POST /v1/approvals/{id}/decision.
func decideApproval(w http.ResponseWriter, r *http.Request, src ApprovalSource) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "bad approval id: "+idStr)
		return
	}
	var body decisionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	state := ""
	switch strings.ToLower(strings.TrimSpace(body.Decision)) {
	case "approve", "approved":
		state = "approved"
	case "deny", "denied", "reject", "rejected":
		state = "denied"
	default:
		writeError(w, http.StatusBadRequest, `decision must be "approve" or "deny"`)
		return
	}
	by := strings.TrimSpace(body.By)
	if by == "" {
		by = "rest"
	}
	ok, err := src.DecideApproval(r.Context(), id, state, by)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "deciding: "+err.Error())
		return
	}
	if !ok {
		// Honest CAS loss: someone else decided first (or it never was
		// pending). Report the current row so the caller sees the truth.
		a, found, _ := src.GetApproval(r.Context(), id)
		if !found {
			writeError(w, http.StatusNotFound, "approval not found")
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "not pending", "approval": approvalToJSON(a),
		})
		return
	}
	a, found, err := src.GetApproval(r.Context(), id)
	if err != nil || !found {
		writeError(w, http.StatusInternalServerError, "re-reading decided approval")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": state, "approval": approvalToJSON(a)})
}
