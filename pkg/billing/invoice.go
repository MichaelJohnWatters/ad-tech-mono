package billing

import (
	"fmt"
	"time"
)

// Invoice is an advertiser's bill for a period.
type Invoice struct {
	ID             string
	AdvertiserID   string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	Currency       string
	LineItems      []InvoiceLineItem
	Subtotal       float64
	Adjustments    float64 // credits, refunds
	Total          float64
	Status         string // draft, issued, paid, overdue
	IssuedAt       time.Time
	DueDate        time.Time
}

// InvoiceLineItem is a single line on an invoice.
type InvoiceLineItem struct {
	CampaignID   string
	CampaignName string
	Impressions  int64
	Clicks       int64
	Conversions  int64
	Spend        float64
	BidModel     string
}

// Payout is a publisher's earnings for a period.
type Payout struct {
	ID           string
	PublisherID  string
	PeriodStart  time.Time
	PeriodEnd    time.Time
	Currency     string
	LineItems    []PayoutLineItem
	GrossRevenue float64
	Adjustments  float64
	NetPayout    float64
	Status       string // pending, approved, paid
	PaymentTerms string // net_30, net_60
}

// PayoutLineItem is a single line on a payout.
type PayoutLineItem struct {
	PlacementID  string
	Impressions  int64
	Revenue      float64
	FeePercent   float64
	DealType     string
	Subsidy      float64
}

// InvoiceGenerator creates invoices and payouts from ledger data.
type InvoiceGenerator struct {
	ledger *Ledger
}

// NewInvoiceGenerator creates an invoice generator.
func NewInvoiceGenerator(ledger *Ledger) *InvoiceGenerator {
	return &InvoiceGenerator{ledger: ledger}
}

// GenerateInvoice creates an invoice for an advertiser for a period.
func (g *InvoiceGenerator) GenerateInvoice(advertiserID string, periodStart, periodEnd time.Time, currency string) *Invoice {
	entries := g.ledger.EntriesForAccount("advertiser:" + advertiserID)

	// Group spend by campaign
	campaignSpend := make(map[string]float64)
	campaignImps := make(map[string]int64)
	for _, e := range entries {
		if e.Type != EntrySpend && e.Type != EntrySettlement {
			continue
		}
		if e.Timestamp.Before(periodStart) || !e.Timestamp.Before(periodEnd) {
			continue
		}
		campaignSpend[e.CampaignID] += e.Amount
		campaignImps[e.CampaignID]++
	}

	var lineItems []InvoiceLineItem
	var subtotal float64
	for cid, spend := range campaignSpend {
		lineItems = append(lineItems, InvoiceLineItem{
			CampaignID:  cid,
			Impressions: campaignImps[cid],
			Spend:       spend,
		})
		subtotal += spend
	}

	// Sum adjustments (refunds, credits)
	var adjustments float64
	for _, e := range entries {
		if e.Type == EntryRefund || e.Type == EntryAdjustment {
			if e.Timestamp.Before(periodStart) || !e.Timestamp.Before(periodEnd) {
				continue
			}
			adjustments -= e.Amount
		}
	}

	return &Invoice{
		ID:           fmt.Sprintf("INV-%s-%s", advertiserID, periodStart.Format("200601")),
		AdvertiserID: advertiserID,
		PeriodStart:  periodStart,
		PeriodEnd:    periodEnd,
		Currency:     currency,
		LineItems:    lineItems,
		Subtotal:     subtotal,
		Adjustments:  adjustments,
		Total:        subtotal + adjustments,
		Status:       "draft",
		IssuedAt:     time.Now().UTC(),
		DueDate:      time.Now().UTC().AddDate(0, 0, 30),
	}
}

// GeneratePayout creates a payout for a publisher for a period.
func (g *InvoiceGenerator) GeneratePayout(publisherID string, periodStart, periodEnd time.Time, currency, paymentTerms string) *Payout {
	entries := g.ledger.EntriesForAccount("publisher:" + publisherID)

	// Group by placement
	placementRev := make(map[string]float64)
	placementImps := make(map[string]int64)
	var grossRevenue float64

	for _, e := range entries {
		if e.Type != EntrySpend && e.Type != EntrySettlement {
			continue
		}
		if e.Timestamp.Before(periodStart) || !e.Timestamp.Before(periodEnd) {
			continue
		}
		// Use publisher ID as placement key since we don't track placement in ledger entries
		key := e.PublisherID
		placementRev[key] += e.PublisherRevenue
		placementImps[key]++
		grossRevenue += e.PublisherRevenue
	}

	var lineItems []PayoutLineItem
	for pid, rev := range placementRev {
		lineItems = append(lineItems, PayoutLineItem{
			PlacementID: pid,
			Impressions: placementImps[pid],
			Revenue:     rev,
		})
	}

	return &Payout{
		ID:           fmt.Sprintf("PAY-%s-%s", publisherID, periodStart.Format("200601")),
		PublisherID:  publisherID,
		PeriodStart:  periodStart,
		PeriodEnd:    periodEnd,
		Currency:     currency,
		LineItems:    lineItems,
		GrossRevenue: grossRevenue,
		NetPayout:    grossRevenue, // adjustments would reduce this
		Status:       "pending",
		PaymentTerms: paymentTerms,
	}
}
