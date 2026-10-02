package adserving

import (
	"net/http"
	"strconv"
	"strings"
)

// OutcomeHeader carries a compact, human-parseable summary of what an ad-serving
// response actually DID — filled, no-bid, house/slate, or (for a live SSAI
// window) content-only — so a browser-side trace panel can show the auction
// OUTCOME without parsing format-specific bodies (JSON display, XML VAST/DAAST,
// HLS manifest). It is a demo/observability affordance only: CORS-exposed (see
// pkg/middleware.CORS) and never consulted by the serving path. Wire format is a
// "; "-joined key=value list, e.g.
//
//	result=fill; type=video; adv=ford.com; price=7.80; cur=USD; model=cpm; deal=pmp-1
const OutcomeHeader = "X-Adtech-Outcome"

// Outcome result values (the first token of the header).
const (
	OutcomeFill    = "fill"    // a real auction winner rendered
	OutcomeNoBid   = "nobid"   // nothing bid — honest no-fill
	OutcomeHouse   = "house"   // no demand → a house/slate ad shown instead
	OutcomeContent = "content" // live SSAI window with no ad break (content playing)
)

// Outcome is the structured form; String()/Set() render the wire header.
type Outcome struct {
	Result     string  // OutcomeFill | OutcomeNoBid | OutcomeHouse | OutcomeContent
	Type       string  // display | video | audio | native | ctv | vmap
	Advertiser string  // advertiser domain, when known
	Price      float64 // clearing price (omitted when ≤ 0)
	Currency   string
	Model      string // cpm | cpc | cpa …
	Deal       string
	Reason     string // nobid reason / note
}

// String renders the header value, omitting empty/zero fields.
func (o Outcome) String() string {
	kv := []string{"result=" + sanitizeOutcome(o.Result)}
	if o.Type != "" {
		kv = append(kv, "type="+sanitizeOutcome(o.Type))
	}
	if o.Advertiser != "" {
		kv = append(kv, "adv="+sanitizeOutcome(o.Advertiser))
	}
	if o.Price > 0 {
		kv = append(kv, "price="+strconv.FormatFloat(o.Price, 'f', 2, 64))
	}
	if o.Currency != "" {
		kv = append(kv, "cur="+sanitizeOutcome(o.Currency))
	}
	if o.Model != "" {
		kv = append(kv, "model="+sanitizeOutcome(o.Model))
	}
	if o.Deal != "" {
		kv = append(kv, "deal="+sanitizeOutcome(o.Deal))
	}
	if o.Reason != "" {
		kv = append(kv, "reason="+sanitizeOutcome(o.Reason))
	}
	return strings.Join(kv, "; ")
}

// SetOutcome stamps the header on w (no-op when Result is empty). MUST be called
// before the response body is written.
func SetOutcome(w http.ResponseWriter, o Outcome) {
	if o.Result == "" {
		return
	}
	w.Header().Set(OutcomeHeader, o.String())
}

// sanitizeOutcome strips characters that would break the header grammar
// ("; " separator and "=" pairing) or produce an invalid HTTP header value
// (CR/LF). Values are short identifiers (domains, currencies, models), so this
// is lossless in practice.
func sanitizeOutcome(s string) string {
	r := strings.NewReplacer(";", ",", "=", "-", "\n", " ", "\r", " ")
	return strings.TrimSpace(r.Replace(s))
}
