package config_test

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
)

func TestGet_Default(t *testing.T) {
	cfg := config.Load()
	if got := cfg.Get("nonexistent.key", "fallback"); got != "fallback" {
		t.Errorf("Get() = %q, want 'fallback'", got)
	}
}

func TestGet_LiveOverridesDefault(t *testing.T) {
	cfg := config.Load()
	cfg.SetLive("exchange.bid_timeout", "200ms")

	if got := cfg.Get("exchange.bid_timeout", "100ms"); got != "200ms" {
		t.Errorf("Get() = %q, want '200ms'", got)
	}
}

func TestGet_EnvOverridesDefault(t *testing.T) {
	t.Setenv("EXCHANGE_BID_TIMEOUT", "150ms")
	cfg := config.Load()

	if got := cfg.Get("exchange.bid_timeout", "100ms"); got != "150ms" {
		t.Errorf("Get() = %q, want '150ms'", got)
	}
}

func TestGet_LiveOverridesEnv(t *testing.T) {
	t.Setenv("EXCHANGE_BID_TIMEOUT", "150ms")
	cfg := config.Load()
	cfg.SetLive("exchange.bid_timeout", "200ms")

	if got := cfg.Get("exchange.bid_timeout", "100ms"); got != "200ms" {
		t.Errorf("Get() = %q, want '200ms' (live overrides env)", got)
	}
}

func TestClearLive_RevertsToEnv(t *testing.T) {
	t.Setenv("EXCHANGE_BID_TIMEOUT", "150ms")
	cfg := config.Load()
	cfg.SetLive("exchange.bid_timeout", "200ms")
	cfg.ClearLive("exchange.bid_timeout")

	if got := cfg.Get("exchange.bid_timeout", "100ms"); got != "150ms" {
		t.Errorf("after ClearLive, Get() = %q, want '150ms' (env)", got)
	}
}

func TestGetInt(t *testing.T) {
	cfg := config.Load()
	cfg.SetLive("exchange.max_dsp_fanout", "15")

	if got := cfg.GetInt("exchange.max_dsp_fanout", 10); got != 15 {
		t.Errorf("GetInt() = %d, want 15", got)
	}
	if got := cfg.GetInt("nonexistent", 42); got != 42 {
		t.Errorf("GetInt() default = %d, want 42", got)
	}
}

func TestGetFloat(t *testing.T) {
	cfg := config.Load()
	cfg.SetLive("fraud.score_threshold", "0.7")

	if got := cfg.GetFloat("fraud.score_threshold", 0.5); got != 0.7 {
		t.Errorf("GetFloat() = %f, want 0.7", got)
	}
}

func TestGetBool(t *testing.T) {
	cfg := config.Load()
	cfg.SetLive("dsp.auto_optimise", "true")

	if got := cfg.GetBool("dsp.auto_optimise", false); !got {
		t.Error("GetBool() = false, want true")
	}
}

func TestGetDuration(t *testing.T) {
	cfg := config.Load()
	cfg.SetLive("exchange.bid_timeout", "200ms")

	got := cfg.GetDuration("exchange.bid_timeout", 100*time.Millisecond)
	if got != 200*time.Millisecond {
		t.Errorf("GetDuration() = %v, want 200ms", got)
	}
}

func TestSetLiveBatch(t *testing.T) {
	cfg := config.Load()
	cfg.SetLiveBatch(map[string]string{
		"a": "1",
		"b": "2",
		"c": "3",
	})

	if got := cfg.Get("a", ""); got != "1" {
		t.Errorf("a = %q, want 1", got)
	}
	if got := cfg.Get("c", ""); got != "3" {
		t.Errorf("c = %q, want 3", got)
	}
}
