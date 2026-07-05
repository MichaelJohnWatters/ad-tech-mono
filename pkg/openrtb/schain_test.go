package openrtb

import (
	"errors"
	"testing"
)

func TestValidateSChain(t *testing.T) {
	validNode := SupplyChainNode{ASI: "adtech.example", SID: "pub-123", HP: 1}

	tests := []struct {
		name    string
		sc      *SupplyChain
		wantErr error // sentinel to match with errors.Is; nil = expect no error
		anyErr  bool  // true when we only care that some error is returned
	}{
		{
			name: "valid complete chain",
			sc:   &SupplyChain{Complete: 1, Ver: SChainVersion, Nodes: []SupplyChainNode{validNode}},
		},
		{
			name: "valid multi-node with hp=0",
			sc: &SupplyChain{Complete: 1, Ver: SChainVersion, Nodes: []SupplyChainNode{
				{ASI: "reseller.example", SID: "acct-9", HP: 0},
				validNode,
			}},
		},
		{name: "nil chain", sc: nil, wantErr: ErrSChainMissing},
		{name: "missing version", sc: &SupplyChain{Nodes: []SupplyChainNode{validNode}}, wantErr: ErrSChainBadVersion},
		{name: "no nodes", sc: &SupplyChain{Ver: SChainVersion}, wantErr: ErrSChainNoNodes},
		{
			name:   "node missing asi",
			sc:     &SupplyChain{Ver: SChainVersion, Nodes: []SupplyChainNode{{SID: "pub-123", HP: 1}}},
			anyErr: true,
		},
		{
			name:   "node missing sid",
			sc:     &SupplyChain{Ver: SChainVersion, Nodes: []SupplyChainNode{{ASI: "adtech.example", HP: 1}}},
			anyErr: true,
		},
		{
			name:   "node bad hp",
			sc:     &SupplyChain{Ver: SChainVersion, Nodes: []SupplyChainNode{{ASI: "adtech.example", SID: "pub-123", HP: 2}}},
			anyErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSChain(tt.sc)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("got %v, want %v", err, tt.wantErr)
				}
			case tt.anyErr:
				if err == nil {
					t.Fatalf("expected an error, got nil")
				}
			default:
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
			}
		})
	}
}

func TestSChainOf(t *testing.T) {
	if got := SChainOf(nil); got != nil {
		t.Errorf("nil request: got %v, want nil", got)
	}
	if got := SChainOf(&BidRequest{}); got != nil {
		t.Errorf("no source: got %v, want nil", got)
	}
	if got := SChainOf(&BidRequest{Source: &Source{}}); got != nil {
		t.Errorf("no ext: got %v, want nil", got)
	}
	sc := &SupplyChain{Ver: SChainVersion}
	req := &BidRequest{Source: &Source{Ext: &SourceExt{SChain: sc}}}
	if got := SChainOf(req); got != sc {
		t.Errorf("populated: got %v, want %v", got, sc)
	}
}
