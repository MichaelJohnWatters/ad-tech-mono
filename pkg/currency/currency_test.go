package currency_test

import (
	"math"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/currency"
)

func sampleRates() []currency.Rate {
	return []currency.Rate{
		{Base: "USD", Target: "GBP", Rate: 0.79},
		{Base: "USD", Target: "EUR", Rate: 0.92},
		{Base: "USD", Target: "JPY", Rate: 157.50},
		{Base: "USD", Target: "CAD", Rate: 1.36},
		{Base: "USD", Target: "AUD", Rate: 1.53},
	}
}

func almostEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) < tolerance
}

func TestConvert_SameCurrency(t *testing.T) {
	c := currency.NewConverter(sampleRates())
	got, err := c.Convert(100, "USD", "USD")
	if err != nil || got != 100 {
		t.Errorf("Convert(100, USD, USD) = %f, %v, want 100, nil", got, err)
	}
}

func TestConvert_USDToGBP(t *testing.T) {
	c := currency.NewConverter(sampleRates())
	got, err := c.Convert(100, "USD", "GBP")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(got, 79.0, 0.01) {
		t.Errorf("Convert(100, USD, GBP) = %f, want ~79.0", got)
	}
}

func TestConvert_GBPToUSD(t *testing.T) {
	c := currency.NewConverter(sampleRates())
	got, err := c.Convert(79, "GBP", "USD")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(got, 100.0, 0.5) {
		t.Errorf("Convert(79, GBP, USD) = %f, want ~100.0", got)
	}
}

func TestConvert_ViaUSDIntermediary(t *testing.T) {
	c := currency.NewConverter(sampleRates())
	// GBP -> EUR goes via USD: GBP->USD->EUR
	got, err := c.Convert(100, "GBP", "EUR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 100 GBP -> ~126.58 USD -> ~116.45 EUR
	if got < 100 || got > 130 {
		t.Errorf("Convert(100, GBP, EUR) = %f, expected ~116.45", got)
	}
}

func TestConvert_UnknownCurrency(t *testing.T) {
	c := currency.NewConverter(sampleRates())
	_, err := c.Convert(100, "USD", "XYZ")
	if err == nil {
		t.Error("expected error for unknown currency pair")
	}
}

func TestToUSD(t *testing.T) {
	c := currency.NewConverter(sampleRates())
	got, err := c.ToUSD(79, "GBP")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(got, 100.0, 0.5) {
		t.Errorf("ToUSD(79, GBP) = %f, want ~100.0", got)
	}
}

func TestFromUSD(t *testing.T) {
	c := currency.NewConverter(sampleRates())
	got, err := c.FromUSD(100, "JPY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(got, 15750.0, 1.0) {
		t.Errorf("FromUSD(100, JPY) = %f, want ~15750.0", got)
	}
}

func TestHasRate(t *testing.T) {
	c := currency.NewConverter(sampleRates())

	if !c.HasRate("USD", "GBP") {
		t.Error("should have USD->GBP rate")
	}
	if !c.HasRate("GBP", "USD") {
		t.Error("should have inverse GBP->USD rate")
	}
	if !c.HasRate("GBP", "EUR") {
		t.Error("should have GBP->EUR via USD intermediary")
	}
	if c.HasRate("USD", "XYZ") {
		t.Error("should not have rate for unknown currency")
	}
}

func TestUpdateRates(t *testing.T) {
	c := currency.NewConverter(sampleRates())

	// Initial rate
	got1, _ := c.Convert(100, "USD", "GBP")

	// Update rate
	c.UpdateRates([]currency.Rate{
		{Base: "USD", Target: "GBP", Rate: 0.82}, // rate changed
	})

	got2, _ := c.Convert(100, "USD", "GBP")

	if almostEqual(got1, got2, 0.01) {
		t.Error("rate should have changed after UpdateRates")
	}
	if !almostEqual(got2, 82.0, 0.01) {
		t.Errorf("after update, Convert(100, USD, GBP) = %f, want ~82.0", got2)
	}
}

func TestSupportedCurrencies(t *testing.T) {
	c := currency.NewConverter(sampleRates())
	currencies := c.SupportedCurrencies()

	if len(currencies) < 5 {
		t.Errorf("expected at least 5 currencies, got %d", len(currencies))
	}
}
