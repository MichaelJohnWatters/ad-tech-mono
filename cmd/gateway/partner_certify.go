package main

// partner_certify.go — partner self-serve certification (PLAN Phase 11 #112
// slice 4):
//
//	GET  /v1/api/partner/certify  — the golden scenarios to answer + this partner's run history
//	POST /v1/api/partner/certify  — {responses:{scenario:BidResponse}} → scored run;
//	                                a PASS while in sandbox advances → certified
//
// Partner-account-only (partner:self). Scoring is pure (pkg/partner.ScoreCertification
// over pkg/openrtb.ValidateBidResponse); a pass only advances the lifecycle when
// the partner is currently in sandbox (staff still controls certified → active).

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner"
)

func partnerCertifyHandler(store partner.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		accountID, ok := partnerGate(w, r, "partner:self")
		if !ok {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"partners unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		p, err := store.GetByAccount(r.Context(), accountID)
		if err != nil {
			http.Error(w, `{"error":"no partner record for this account"}`, http.StatusNotFound)
			return
		}

		switch r.Method {
		case http.MethodGet:
			history, err := store.ListCertifications(r.Context(), p.ID, 20)
			if err != nil {
				log.Error("cert history failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"scenarios": partner.CertificationScenarios(),
				"history":   history,
				"status":    p.Status,
			})

		case http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, 512<<10))
			if err != nil {
				http.Error(w, `{"error":"read"}`, http.StatusBadRequest)
				return
			}
			var req struct {
				Responses map[string]*openrtb.BidResponse `json:"responses"`
			}
			if jerr := json.Unmarshal(body, &req); jerr != nil {
				http.Error(w, `{"error":"invalid request body — expected {responses:{scenario:BidResponse}}"}`, http.StatusBadRequest)
				return
			}
			result := partner.ScoreCertification(req.Responses)

			rec, err := store.RecordCertification(r.Context(), p.ID, result, actorFromClaims(r))
			if err != nil {
				log.Error("record certification failed", "partner", p.ID, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}

			// A pass advances sandbox → certified (staff still gates → active).
			promoted := false
			newStatus := p.Status
			if result.Passed && p.Status == partner.StatusSandbox && partner.CanTransition(p.Status, partner.StatusCertified) {
				if updated, serr := store.SetStatus(r.Context(), p.ID, partner.StatusCertified); serr != nil {
					log.Error("cert auto-advance failed", "partner", p.ID, "error", serr)
				} else {
					promoted = true
					newStatus = updated.Status
				}
			}
			log.Info("partner certification run", "partner", p.ID, "passed", result.Passed, "score", result.Score, "promoted", promoted)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result":   result,
				"record":   rec,
				"promoted": promoted,
				"status":   newStatus,
			})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// actorFromClaims returns the caller's JWT subject for the run_by audit column
// (partnerGate already validated claims are present).
func actorFromClaims(r *http.Request) string {
	if c := middleware.ClaimsFromContext(r.Context()); c != nil {
		return "user:" + c.UserID
	}
	return ""
}
