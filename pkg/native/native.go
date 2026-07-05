// Package native implements the OpenRTB Native Ads 1.2 markup — the request
// object an SSP embeds in Imp.Native.Request and the response markup a DSP
// returns in BidObj.AdM. It's the native counterpart of pkg/vast (video) and
// pkg/openrtb (bid request/response): pure types plus build/parse/validate
// helpers, no I/O.
//
// A native ad is a set of typed assets (title, images, data fields like
// sponsored-by / description / CTA) that the publisher renders in its own
// layout, rather than a fixed banner or a VAST document. The request declares
// which assets the publisher wants (and their constraints); the response fills
// them in, plus a click link and event trackers.
package native

import "encoding/json"

// Ver is the Native spec version we emit.
const Ver = "1.2"

// Asset ids we assign to the standard asset slots, so the response can be
// matched back to the request deterministically.
const (
	AssetIDTitle     = 1
	AssetIDMainImage = 2
	AssetIDIcon      = 3
	AssetIDSponsored = 4 // data, type=1
	AssetIDBody      = 5 // data, type=2
	AssetIDCTA       = 6 // data, type=12
)

// Image asset types (OpenRTB Native 1.2 §7.4).
const (
	ImageTypeIcon = 1
	ImageTypeMain = 3
)

// Data asset types (OpenRTB Native 1.2 §7.6).
const (
	DataTypeSponsored = 1
	DataTypeDesc      = 2
	DataTypeCTAText   = 12
)

// Event trackers (OpenRTB Native 1.2 §7.7 / §7.8).
const (
	EventTypeImpression    = 1 // impression as soon as rendered
	EventTypeViewableMRC50 = 2 // 50% in view for 1s (display MRC)
	TrackMethodImg         = 1 // fire via image pixel
	TrackMethodJS          = 2 // fire via injected JS
)

// ---- Request (SSP → exchange → DSP) ----

// Request is the OpenRTB Native 1.2 request. It is JSON-serialised and carried
// as a string in Imp.Native.Request.
type Request struct {
	Ver       string     `json:"ver,omitempty"`
	Context   int        `json:"context,omitempty"`   // 1=content, 2=social, 3=product
	PlcmtType int        `json:"plcmttype,omitempty"` // 1=in-feed, 2=atom unit, 3=outside, 4=recommendation
	PlcmtCnt  int        `json:"plcmtcnt,omitempty"`  // number of identical placements in this layout
	Assets    []ReqAsset `json:"assets"`
}

// ReqAsset is one requested asset slot. Exactly one of Title/Img/Data is set.
type ReqAsset struct {
	ID       int       `json:"id"`
	Required int       `json:"required,omitempty"` // 1 = must be filled
	Title    *TitleReq `json:"title,omitempty"`
	Img      *ImgReq   `json:"img,omitempty"`
	Data     *DataReq  `json:"data,omitempty"`
}

type TitleReq struct {
	Len int `json:"len"` // maximum length in characters
}

type ImgReq struct {
	Type int `json:"type,omitempty"` // ImageType*
	WMin int `json:"wmin,omitempty"`
	HMin int `json:"hmin,omitempty"`
}

type DataReq struct {
	Type int `json:"type"` // DataType*
	Len  int `json:"len,omitempty"`
}

// ---- Response (DSP → exchange → SSP) ----

// Response is the OpenRTB Native 1.2 response markup. It is JSON-serialised and
// carried as a string in BidObj.AdM; the outer object is {"native": {...}}.
type Response struct {
	Native ResponseBody `json:"native"`
}

type ResponseBody struct {
	Ver           string         `json:"ver,omitempty"`
	Assets        []RespAsset    `json:"assets"`
	Link          Link           `json:"link"`
	EventTrackers []EventTracker `json:"eventtrackers,omitempty"`
	// ImpTrackers is the legacy 1.1 impression-tracker URL list; some players
	// still read it, so we mirror the impression EventTracker here too.
	ImpTrackers []string `json:"imptrackers,omitempty"`
}

// RespAsset fills a requested slot. The ID matches the corresponding ReqAsset.
type RespAsset struct {
	ID    int        `json:"id"`
	Title *TitleResp `json:"title,omitempty"`
	Img   *ImgResp   `json:"img,omitempty"`
	Data  *DataResp  `json:"data,omitempty"`
}

type TitleResp struct {
	Text string `json:"text"`
}

type ImgResp struct {
	URL string `json:"url"`
	W   int    `json:"w,omitempty"`
	H   int    `json:"h,omitempty"`
}

type DataResp struct {
	Value string `json:"value"`
}

// Link is the destination when the ad is clicked.
type Link struct {
	URL           string   `json:"url"`
	ClickTrackers []string `json:"clicktrackers,omitempty"`
}

// EventTracker is an impression/viewability tracker the player fires.
type EventTracker struct {
	Event  int    `json:"event"`  // EventType*
	Method int    `json:"method"` // TrackMethod*
	URL    string `json:"url"`
}

// MarshalRequest serialises a Request to the JSON string that goes in
// Imp.Native.Request.
func MarshalRequest(r Request) (string, error) {
	if r.Ver == "" {
		r.Ver = Ver
	}
	b, err := json.Marshal(r)
	return string(b), err
}

// MarshalResponse serialises a Response to the JSON string that goes in
// BidObj.AdM.
func MarshalResponse(r Response) (string, error) {
	if r.Native.Ver == "" {
		r.Native.Ver = Ver
	}
	b, err := json.Marshal(r)
	return string(b), err
}

// ParseResponse parses native response markup (BidObj.AdM) back into a
// Response — used by any consumer that needs to inspect the assets (e.g. the
// SSP or a test harness).
func ParseResponse(adm string) (Response, error) {
	var r Response
	err := json.Unmarshal([]byte(adm), &r)
	return r, err
}
