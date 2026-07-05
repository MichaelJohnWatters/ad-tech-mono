// Package openrtb defines OpenRTB 2.6 bid request/response types
// for JSON serialisation over HTTP.
//
// Used for Exchange <-> DSP communication (industry standard).
// Internal gRPC uses pkg/proto/ types. This package is for the
// OpenRTB HTTP endpoints only.
package openrtb

// BidRequest is the OpenRTB 2.6 bid request sent by the Exchange to DSPs.
type BidRequest struct {
	ID     string   `json:"id"`
	Imp    []Imp    `json:"imp"`
	Site   *Site    `json:"site,omitempty"`
	App    *App     `json:"app,omitempty"`
	Device *Device  `json:"device,omitempty"`
	User   *User    `json:"user,omitempty"`
	Regs   *Regs    `json:"regs,omitempty"`
	Source *Source  `json:"source,omitempty"` // supply-chain / transaction provenance
	TMax   int      `json:"tmax,omitempty"`   // max response time in ms
	Cur    []string `json:"cur,omitempty"`    // allowed currencies
}

// Source carries transaction provenance and the supply chain. Per OpenRTB
// 2.6, the SupplyChain object lives at Source.Ext.schain — it declares every
// intermediary between the publisher and this bid request so buyers can
// verify the path (the third leg of the transparency triad alongside
// ads.txt and sellers.json).
type Source struct {
	FD  int        `json:"fd,omitempty"`  // 0=exchange is final decision maker, 1=upstream chain
	TID string     `json:"tid,omitempty"` // transaction id, common across the auction
	Ext *SourceExt `json:"ext,omitempty"`
}

// SourceExt holds the SupplyChain object (OpenRTB moved schain from an
// extension into Source in 2.6; buyers still read it at source.ext.schain
// for backwards compatibility, so we serialise it there).
type SourceExt struct {
	SChain *SupplyChain `json:"schain,omitempty"`
}

// SupplyChain is the IAB SupplyChain object (spec version "1.0"). Complete=1
// means every node from the publisher to here is present and the chain can be
// fully verified; 0 means an upstream hop is missing/unknown.
type SupplyChain struct {
	Complete int               `json:"complete"`
	Nodes    []SupplyChainNode `json:"nodes"`
	Ver      string            `json:"ver,omitempty"`
}

// SupplyChainNode is one hop in the supply chain — an advertising system that
// participated in selling this impression. ASI is that system's canonical
// domain (matches the seller's sellers.json host); SID is the seller's account
// id within that system (matches its sellers.json seller_id and the publisher's
// ads.txt account id). HP=1 flags a node that handles payment for the inventory.
type SupplyChainNode struct {
	ASI    string `json:"asi"`
	SID    string `json:"sid"`
	RID    string `json:"rid,omitempty"` // request/transaction id issued by this node
	HP     int    `json:"hp"`
	Name   string `json:"name,omitempty"`
	Domain string `json:"domain,omitempty"`
}

// Imp represents an impression opportunity.
type Imp struct {
	ID string `json:"id"`
	// TagID is the OpenRTB "identifier for specific ad placement or ad tag"
	// — we use it to carry the placement UUID through to the exchange so
	// deal eligibility can match on placement_id. (ID stays the
	// auction-side imp identifier; TagID is the platform identifier.)
	TagID       string  `json:"tagid,omitempty"`
	Banner      *Banner `json:"banner,omitempty"`
	Video       *Video  `json:"video,omitempty"`
	Audio       *Audio  `json:"audio,omitempty"`
	Native      *Native `json:"native,omitempty"`
	BidFloor    float64 `json:"bidfloor,omitempty"`
	BidFloorCur string  `json:"bidfloorcur,omitempty"`
	DealID      string  `json:"dealid,omitempty"`
	Ext         *ImpExt `json:"ext,omitempty"`
}

// Banner represents a display ad opportunity.
type Banner struct {
	W     int      `json:"w,omitempty"`
	H     int      `json:"h,omitempty"`
	Mimes []string `json:"mimes,omitempty"`
}

// Video represents a video ad opportunity. Field set targets OpenRTB
// 2.6 — the fields beyond the obvious w/h/duration are what the rest of
// Phase 9 (VAST, ad pods, SSAI, CTV) needs to make real bidding
// decisions, so they're plumbed through now even though no consumer
// reads them yet. Optional fields stay omitempty so existing minimal
// bid requests serialise identically.
type Video struct {
	Mimes          []string    `json:"mimes,omitempty"`
	Protocols      []int       `json:"protocols,omitempty"` // VAST versions supported (2=VAST 2.0, 3=VAST 3.0, 5=VAST 4.0, 6=VAST 4.1, 7=VAST 4.2)
	W              int         `json:"w,omitempty"`
	H              int         `json:"h,omitempty"`
	MinDuration    int         `json:"minduration,omitempty"`
	MaxDuration    int         `json:"maxduration,omitempty"`
	Linearity      int         `json:"linearity,omitempty"`  // 1=linear (pre-/mid-/post-roll), 2=non-linear (overlay)
	Placement      int         `json:"placement,omitempty"`  // 1=instream, 2=in-banner, 3=in-article, 4=in-feed, 5=interstitial. Deprecated by Plcmt in 2.6.
	Plcmt          int         `json:"plcmt,omitempty"`      // 2.6: 1=instream w/ audio, 2=accompanying content, 3=interstitial, 4=no content / standalone
	Pos            int         `json:"pos,omitempty"`        // 0=unknown, 1=above the fold, 3=below the fold, 7=fullscreen
	StartDelay     int         `json:"startdelay,omitempty"` // 0=pre-roll, >0=mid-roll at seconds, -1=generic mid, -2=generic post
	Skip           int         `json:"skip,omitempty"`
	SkipMin        int         `json:"skipmin,omitempty"`        // minimum duration before skip is allowed
	SkipAfter      int         `json:"skipafter,omitempty"`      // seconds before user can skip
	Sequence       int         `json:"sequence,omitempty"`       // position in a pod (1=first, 2=second…). Deprecated by SlotInPod in 2.6.
	BAttr          []int       `json:"battr,omitempty"`          // blocked creative attributes (e.g. 13=user-initiated mid-roll, 17=adobe flash)
	MaxExtended    int         `json:"maxextended,omitempty"`    // max extension allowed past maxduration (seconds)
	MinBitRate     int         `json:"minbitrate,omitempty"`     // kbps
	MaxBitRate     int         `json:"maxbitrate,omitempty"`     // kbps
	BoxingAllowed  int         `json:"boxingallowed,omitempty"`  // 1=letterboxing allowed when aspect doesn't match (default 1)
	PlaybackMethod []int       `json:"playbackmethod,omitempty"` // 1=autoplay sound on, 2=autoplay sound off, 3=click sound on, 4=mouseover sound on, 5=enter viewport sound on, 6=enter viewport sound off
	PlaybackEnd    int         `json:"playbackend,omitempty"`    // 1=video completes, 2=user leaves viewport, 3=user closes/skips
	Delivery       []int       `json:"delivery,omitempty"`       // 1=streaming, 2=progressive, 3=download
	API            []int       `json:"api,omitempty"`            // 1=VPAID 1.0, 2=VPAID 2.0, 3=MRAID 1, 4=ORMMA, 5=MRAID 2, 6=MRAID 3, 7=OMID 1
	CompanionAd    []Companion `json:"companionad,omitempty"`
	CompanionType  []int       `json:"companiontype,omitempty"` // 1=static resource, 2=HTML resource, 3=iframe resource
	// Ad pod fields (OpenRTB 2.6) — set when this imp is one slot in a
	// pre-/mid-/post-roll pod rather than a standalone spot.
	PodID        string  `json:"podid,omitempty"`        // identifier shared across all imps in the same pod
	PodSeq       int     `json:"podseq,omitempty"`       // -1=last pod, 0=any pod, 1=first pod, 2=any mid pod
	SlotInPod    int     `json:"slotinpod,omitempty"`    // -1=last slot, 0=any slot, 1=first slot, 2=first or any mid slot, 3=any last slot
	RqdDurs      []int   `json:"rqddurs,omitempty"`      // required durations for slots in this pod
	MinCPMPerSec float64 `json:"mincpmpersec,omitempty"` // floor price per second of ad duration in the pod
}

// Audio represents an audio ad opportunity. DAAST / podcast dynamic
// insertion / streaming radio all build on this.
type Audio struct {
	Mimes         []string    `json:"mimes,omitempty"`
	Protocols     []int       `json:"protocols,omitempty"` // 1=DAAST 1.0, 2=DAAST 1.0 wrapper, 9=VAST 3.0 (audio extension)
	MinDuration   int         `json:"minduration,omitempty"`
	MaxDuration   int         `json:"maxduration,omitempty"`
	StartDelay    int         `json:"startdelay,omitempty"`
	Sequence      int         `json:"sequence,omitempty"` // pod position (legacy; use SlotInPod in 2.6 pods)
	BAttr         []int       `json:"battr,omitempty"`
	MaxExtended   int         `json:"maxextended,omitempty"`
	MinBitRate    int         `json:"minbitrate,omitempty"`
	MaxBitRate    int         `json:"maxbitrate,omitempty"`
	Delivery      []int       `json:"delivery,omitempty"`
	API           []int       `json:"api,omitempty"`
	CompanionAd   []Companion `json:"companionad,omitempty"`
	CompanionType []int       `json:"companiontype,omitempty"`
	MaxSeq        int         `json:"maxseq,omitempty"`   // max number of ads in pod
	Feed          int         `json:"feed,omitempty"`     // 1=music, 2=podcast, 3=radio
	Stitched      int         `json:"stitched,omitempty"` // 1=SSAI-stitched, 0=client-side insertion
	NVol          int         `json:"nvol,omitempty"`     // volume normalization: 0=none, 1=ad volume avg normalized to content, 2=ad volume peak normalized, 3=loudness normalized (LUFS), 4=custom
	// Ad pod fields (parallel to Video; see Video.PodID etc).
	PodID        string  `json:"podid,omitempty"`
	PodSeq       int     `json:"podseq,omitempty"`
	SlotInPod    int     `json:"slotinpod,omitempty"`
	RqdDurs      []int   `json:"rqddurs,omitempty"`
	MinCPMPerSec float64 `json:"mincpmpersec,omitempty"`
}

// Companion describes a companion banner shown alongside a video or
// audio ad. Same shape as a standard Banner plus a couple of companion-
// specific fields. Used by Video.CompanionAd / Audio.CompanionAd.
type Companion struct {
	ID    string   `json:"id,omitempty"`
	W     int      `json:"w,omitempty"`
	H     int      `json:"h,omitempty"`
	WMin  int      `json:"wmin,omitempty"` // minimum width (for flexible inventory)
	HMin  int      `json:"hmin,omitempty"`
	WMax  int      `json:"wmax,omitempty"`
	HMax  int      `json:"hmax,omitempty"`
	BType []int    `json:"btype,omitempty"` // blocked creative types
	BAttr []int    `json:"battr,omitempty"`
	Pos   int      `json:"pos,omitempty"`
	Mimes []string `json:"mimes,omitempty"`
	API   []int    `json:"api,omitempty"`
	Vcm   int      `json:"vcm,omitempty"` // 1=companion concurrent with video, 0=after
}

// Native represents a native ad opportunity.
type Native struct {
	Request string `json:"request,omitempty"` // native request JSON string
	Ver     string `json:"ver,omitempty"`
}

// ImpExt holds extension fields for the impression.
type ImpExt struct {
	Channel       string `json:"channel,omitempty"`        // display, video, audio, dooh, retail, ingame
	PlacementType string `json:"placement_type,omitempty"` // rewarded, interstitial, intrinsic, sponsored_product
}

// Site represents a web publisher.
type Site struct {
	Domain    string     `json:"domain,omitempty"`
	Name      string     `json:"name,omitempty"`
	Page      string     `json:"page,omitempty"`
	Cat       []string   `json:"cat,omitempty"`
	Keywords  string     `json:"keywords,omitempty"` // comma-separated page keywords
	Publisher *Publisher `json:"publisher,omitempty"`
	Content   *Content   `json:"content,omitempty"`
}

// App represents a mobile app publisher.
type App struct {
	Bundle    string     `json:"bundle,omitempty"`
	Name      string     `json:"name,omitempty"`
	StoreURL  string     `json:"storeurl,omitempty"`
	Cat       []string   `json:"cat,omitempty"`
	Ver       string     `json:"ver,omitempty"`
	Publisher *Publisher `json:"publisher,omitempty"`
}

// Publisher in the OpenRTB context.
type Publisher struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Content describes the page/app content.
type Content struct {
	Keywords string `json:"keywords,omitempty"`
	Context  string `json:"context,omitempty"` // positive, negative, neutral
}

// Device describes the user's device.
type Device struct {
	UA             string `json:"ua,omitempty"`
	IP             string `json:"ip,omitempty"`
	DeviceType     int    `json:"devicetype,omitempty"` // 1=mobile, 2=pc, 3=ctv, 4=phone, 5=tablet
	Make           string `json:"make,omitempty"`
	Model          string `json:"model,omitempty"`
	OS             string `json:"os,omitempty"`
	OSV            string `json:"osv,omitempty"`
	ConnectionType int    `json:"connectiontype,omitempty"`
	IFA            string `json:"ifa,omitempty"` // IDFA/GAID
	Lmt            int    `json:"lmt,omitempty"` // limit ad tracking
	Geo            *Geo   `json:"geo,omitempty"`
}

// Geo describes geographic location.
type Geo struct {
	Country    string  `json:"country,omitempty"` // ISO 3166-1 alpha-3
	Region     string  `json:"region,omitempty"`
	City       string  `json:"city,omitempty"`
	Lat        float64 `json:"lat,omitempty"`
	Lon        float64 `json:"lon,omitempty"`
	PostalCode string  `json:"zip,omitempty"`
}

// User describes the user.
type User struct {
	ID  string   `json:"id,omitempty"`
	Ext *UserExt `json:"ext,omitempty"`
}

// UserExt holds extension fields for the user.
type UserExt struct {
	Consent         string            `json:"consent,omitempty"` // TCF consent string
	PublisherUserID string            `json:"publisher_user_id,omitempty"`
	HashedEmail     string            `json:"hashed_email,omitempty"`
	Segments        []string          `json:"segments,omitempty"`
	Demographics    map[string]string `json:"demographics,omitempty"`
}

// Regs describes regulatory signals.
type Regs struct {
	COPPA int      `json:"coppa,omitempty"`
	Ext   *RegsExt `json:"ext,omitempty"`
}

// RegsExt holds extension regulatory fields.
type RegsExt struct {
	GDPR          int    `json:"gdpr,omitempty"`
	USPrivacy     string `json:"us_privacy,omitempty"`
	DataResidency string `json:"data_residency,omitempty"`
	// GPP is the IAB Global Privacy Platform consent string; GPPSID lists the
	// section ids present in it (e.g. "7" for US National). Decoded for US
	// opt-out signals in pkg/privacy.
	GPP    string `json:"gpp,omitempty"`
	GPPSID string `json:"gpp_sid,omitempty"`
	// GPC is the Global Privacy Control browser signal (1 = user asserts "do
	// not sell/share"). Not a core OpenRTB field but a widely-used ext; the
	// SSP sets it from the Sec-GPC request header.
	GPC int `json:"gpc,omitempty"`
}

// BidResponse is the OpenRTB 2.6 bid response from a DSP.
type BidResponse struct {
	ID      string    `json:"id"`
	SeatBid []SeatBid `json:"seatbid,omitempty"`
	Cur     string    `json:"cur,omitempty"`
	NoBid   bool      `json:"nobid,omitempty"` // extension: explicit no-bid
}

// SeatBid represents a collection of bids from one bidder seat.
type SeatBid struct {
	Bid  []BidObj `json:"bid"`
	Seat string   `json:"seat,omitempty"`
}

// BidObj is a single bid within a seat bid.
type BidObj struct {
	ID      string   `json:"id"`
	ImpID   string   `json:"impid"`
	Price   float64  `json:"price"`
	AdID    string   `json:"adid,omitempty"`
	NURL    string   `json:"nurl,omitempty"` // win notice URL
	LURL    string   `json:"lurl,omitempty"` // loss notice URL
	BURL    string   `json:"burl,omitempty"` // billing notice URL (fires when SSP records billable event — for video/audio this is when the player counts the impression, not just receives the bid)
	AdM     string   `json:"adm,omitempty"`  // ad markup (display HTML for banners; VAST XML for video; DAAST XML for audio)
	ADomain []string `json:"adomain,omitempty"`
	CID     string   `json:"cid,omitempty"`  // campaign ID (line item)
	CrID    string   `json:"crid,omitempty"` // creative ID
	Cat     []string `json:"cat,omitempty"`
	DealID  string   `json:"dealid,omitempty"`
	W       int      `json:"w,omitempty"`
	H       int      `json:"h,omitempty"`
	Dur     int      `json:"dur,omitempty"` // video/audio duration (seconds)
	// API / Protocol echo back what the bid's creative supports so the
	// player can reject mismatches without having to fetch the VAST/
	// DAAST. Values match the request's API/Protocols enums.
	API      int `json:"api,omitempty"`
	Protocol int `json:"protocol,omitempty"`
	// Ad pod response fields (OpenRTB 2.6) — set when this bid claims
	// a specific slot in the requested pod. Mirrors Imp.Video.PodID /
	// SlotInPod from the request.
	PodID     string `json:"podid,omitempty"`
	SlotInPod int    `json:"slotinpod,omitempty"`
	// BidModel is a non-standard extension (cpm/cpc/cpa/vcpm/cpcv) the DSP
	// publishes so the SSP can pass it on to the ad server, which stamps
	// it on the impression URL. The tracker reads it to route reserve vs
	// bill-immediately in the billing engine. Standard OpenRTB has no
	// concept of "what you're bidding on"; this is platform-internal.
	BidModel string `json:"bm,omitempty"`
	// MediaURL is a non-standard extension carrying the video/audio
	// MediaFile URL from the DSP through the exchange back to the SSP.
	// Standard OpenRTB expects the DSP to put the full VAST XML in AdM,
	// but our DSPs don't generate VAST — they just ship the media URL
	// and the publisher-adserver assembles the VAST. Empty for display
	// bids.
	MediaURL string `json:"media,omitempty"`
}
