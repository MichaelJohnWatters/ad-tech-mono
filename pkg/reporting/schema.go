package reporting

// ReportField is one selectable metric or group-by dimension, with a UI label.
type ReportField struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ReportTable describes a queryable table for the report builder: its valid
// metrics and group-by dimensions.
type ReportTable struct {
	Name       string        `json:"name"`
	Label      string        `json:"label"`
	Metrics    []ReportField `json:"metrics"`
	Dimensions []ReportField `json:"dimensions"`
}

// Schema is the report builder's SINGLE SOURCE OF TRUTH: which tables an
// advertiser can query and the valid metrics + group-by dimensions for each. The
// portal drives its Table / Metrics / Group-by dropdowns off this (served at
// /v1/api/reports/schema), so the UI can never drift from what the engine
// actually supports — every metric here is a base metric the analytics store
// understands (pkg/store/analytics query.go) or a registered derived metric
// (metrics.go), and every dimension is a real column on the table.
func Schema() []ReportTable {
	day := ReportField{"day", "Day"}
	hour := ReportField{"hour", "Hour"}
	campaign := ReportField{"campaign_id", "Campaign"}
	creative := ReportField{"creative_id", "Creative"}
	placement := ReportField{"placement_id", "Placement"}
	publisher := ReportField{"publisher_id", "Publisher"}
	geo := ReportField{"geo", "Geo"}
	device := ReportField{"device", "Device"}

	return []ReportTable{
		{
			Name:  "impressions",
			Label: "Impressions — delivery & spend",
			Metrics: []ReportField{
				{"count", "Impressions"},
				{"sum_cost", "Spend ($)"},
				{"ecpm", "eCPM ($)"},
				{"clicks", "Clicks"},
				{"ctr", "CTR (%)"},
				{"viewability_rate", "Viewability (%)"},
			},
			Dimensions: []ReportField{
				campaign, creative, placement, publisher,
				{"channel", "Channel"}, {"format", "Format"}, geo, device,
				{"bid_model", "Bid model"}, day, hour,
			},
		},
		{
			Name:    "clicks",
			Label:   "Clicks",
			Metrics: []ReportField{{"count", "Clicks"}},
			Dimensions: []ReportField{
				campaign, creative, placement, publisher, geo, device, day, hour,
			},
		},
		{
			Name:  "conversions",
			Label: "Conversions — revenue & CPA",
			Metrics: []ReportField{
				{"count", "Conversions"},
				{"sum_revenue", "Revenue ($)"},
				{"aov", "Avg order value ($)"},
				{"cpa", "CPA ($)"},
				{"conversion_rate", "Conv. rate (%)"},
			},
			Dimensions: []ReportField{
				campaign, creative, placement,
				{"conversion_type", "Conversion type"},
				{"attribution_type", "Attribution"},
				day, hour,
			},
		},
		{
			Name:  "media_events",
			Label: "Video / audio engagement",
			Metrics: []ReportField{
				{"media_starts", "Starts"},
				{"media_completes", "Completes"},
				{"completion_rate", "Completion (%)"},
			},
			Dimensions: []ReportField{
				campaign, creative, placement, {"channel", "Channel"}, day, hour,
			},
		},
		{
			// Bid-shading savings — the durable auction_shades table (one row per
			// shaded win). sum_savings = realized dollars saved; count = shaded wins.
			// Lets the report builder break savings down by placement / campaign /
			// day, not just the single total on the Shading page.
			Name:  "auction_shades",
			Label: "Bid-shading savings",
			Metrics: []ReportField{
				{"sum_savings", "Savings ($)"},
				{"count", "Shaded wins"},
			},
			Dimensions: []ReportField{
				campaign, placement, {"channel", "Channel"}, day, hour,
			},
		},
	}
}
