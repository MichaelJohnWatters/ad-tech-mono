package bidshading

import "testing"

// TestCurveLearnsMarketNotOwnBid locks the second-price fix: the win-rate curve's
// midpoint must learn the TRUE market clearing (the second price we had to beat),
// not the price we paid. In a first-price auction the paid price == our own bid,
// so recording it as the "clearing" makes the curve measure itself — the midpoint
// drifts up to our bid and shading dies (the equilibration bug). Feeding the real
// clearing keeps the midpoint at the market, so shading stays stable.
func TestCurveLearnsMarketNotOwnBid(t *testing.T) {
	const ourBid, market, floor = 10.0, 5.0, 1.0

	real := NewTracker() // FIX: clearing = the real second price ($5)
	bug := NewTracker()  // OLD BUG: clearing = our own paid bid ($10)
	for i := 0; i < 500; i++ {
		real.RecordWin("pl", "", ourBid, market)
		bug.RecordWin("pl", "", ourBid, ourBid)
	}

	realShade := ShadedBid(real.WinRateCurve("pl"), "moderate", ourBid, floor)
	bugShade := ShadedBid(bug.WinRateCurve("pl"), "moderate", ourBid, floor)

	if realShade >= ourBid {
		t.Errorf("real-clearing curve should still shade below %.2f, got %.2f (no savings)", ourBid, realShade)
	}
	if realShade < market {
		t.Errorf("shaded bid %.2f dropped below the real clearing %.2f — would lose the auction", realShade, market)
	}
	if bugShade < ourBid {
		t.Errorf("own-bid curve should have drifted to NO shade (= %.2f), but got %.2f", ourBid, bugShade)
	}
}
