package billing

import (
	"sync"
	"time"
)

// AttributionWindow defines how far back to look for impressions
// when attributing a conversion.
type AttributionWindow struct {
	ClickThrough time.Duration // conversion within N after click (default 30 days)
	ViewThrough  time.Duration // conversion within N after impression (default 7 days)
}

// DefaultAttributionWindow returns industry standard windows.
func DefaultAttributionWindow() AttributionWindow {
	return AttributionWindow{
		ClickThrough: 30 * 24 * time.Hour,
		ViewThrough:  7 * 24 * time.Hour,
	}
}

// TouchPoint is a recorded impression or click that a conversion
// can be attributed to.
type TouchPoint struct {
	TraceID    string
	CampaignID string
	CreativeID string
	Type       string // "impression" or "click"
	Timestamp  time.Time
	UserID     string
}

// Attribution is the result of attributing a conversion to a touchpoint.
type Attribution struct {
	ConversionTraceID string
	TouchPoint        TouchPoint
	Type              string // "click_through" or "view_through"
	TimeDelta         time.Duration
	Revenue           float64
}

// AttributionEngine matches conversions to prior impressions/clicks.
type AttributionEngine struct {
	mu          sync.RWMutex
	touchPoints map[string][]TouchPoint // user_id -> touchpoints
	window      AttributionWindow
}

// NewAttributionEngine creates an attribution engine.
func NewAttributionEngine(window AttributionWindow) *AttributionEngine {
	return &AttributionEngine{
		touchPoints: make(map[string][]TouchPoint),
		window:      window,
	}
}

// RecordTouchPoint stores an impression or click for future attribution.
func (a *AttributionEngine) RecordTouchPoint(tp TouchPoint) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.touchPoints[tp.UserID] = append(a.touchPoints[tp.UserID], tp)
}

// Attribute finds the best touchpoint for a conversion.
// Priority: click-through > view-through. Most recent wins.
func (a *AttributionEngine) Attribute(userID string, conversionTime time.Time, conversionRevenue float64) *Attribution {
	a.mu.RLock()
	defer a.mu.RUnlock()

	points := a.touchPoints[userID]
	if len(points) == 0 {
		return nil
	}

	// First pass: look for click-through (highest priority)
	var bestClick *TouchPoint
	for i := len(points) - 1; i >= 0; i-- {
		tp := &points[i]
		if tp.Type != "click" {
			continue
		}
		delta := conversionTime.Sub(tp.Timestamp)
		if delta > 0 && delta <= a.window.ClickThrough {
			bestClick = tp
			break // most recent click within window
		}
	}

	if bestClick != nil {
		return &Attribution{
			TouchPoint: *bestClick,
			Type:       "click_through",
			TimeDelta:  conversionTime.Sub(bestClick.Timestamp),
			Revenue:    conversionRevenue,
		}
	}

	// Second pass: look for view-through
	for i := len(points) - 1; i >= 0; i-- {
		tp := &points[i]
		if tp.Type != "impression" {
			continue
		}
		delta := conversionTime.Sub(tp.Timestamp)
		if delta > 0 && delta <= a.window.ViewThrough {
			return &Attribution{
				TouchPoint: *tp,
				Type:       "view_through",
				TimeDelta:  delta,
				Revenue:    conversionRevenue,
			}
		}
	}

	return nil // no attribution possible
}

// Cleanup removes touchpoints older than the longest window.
func (a *AttributionEngine) Cleanup(now time.Time) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	maxAge := a.window.ClickThrough // longest window
	removed := 0

	for userID, points := range a.touchPoints {
		var kept []TouchPoint
		for _, tp := range points {
			if now.Sub(tp.Timestamp) <= maxAge {
				kept = append(kept, tp)
			} else {
				removed++
			}
		}
		if len(kept) == 0 {
			delete(a.touchPoints, userID)
		} else {
			a.touchPoints[userID] = kept
		}
	}
	return removed
}
