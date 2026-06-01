// Package openrtb defines OpenRTB 2.6 bid request/response types
// for JSON serialisation over HTTP.
//
// Used for Exchange <-> DSP communication (industry standard).
// Internal gRPC uses pkg/proto/ types. This package is for the
// OpenRTB HTTP endpoints only.
package openrtb

// BidRequest is the OpenRTB 2.6 bid request sent by the Exchange to DSPs.
type BidRequest struct {
	ID     string  `json:"id"`
	Imp    []Imp   `json:"imp"`
	Site   *Site   `json:"site,omitempty"`
	App    *App    `json:"app,omitempty"`
	Device *Device `json:"device,omitempty"`
	User   *User   `json:"user,omitempty"`
	Regs   *Regs   `json:"regs,omitempty"`
	TMax   int     `json:"tmax,omitempty"` // max response time in ms
	Cur    []string `json:"cur,omitempty"` // allowed currencies
}

// Imp represents an impression opportunity.
type Imp struct {
	ID          string  `json:"id"`
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

// Video represents a video ad opportunity.
type Video struct {
	Mimes       []string `json:"mimes,omitempty"`
	Protocols   []int    `json:"protocols,omitempty"`
	W           int      `json:"w,omitempty"`
	H           int      `json:"h,omitempty"`
	MinDuration int      `json:"minduration,omitempty"`
	MaxDuration int      `json:"maxduration,omitempty"`
	Linearity   int      `json:"linearity,omitempty"`
	Placement   int      `json:"placement,omitempty"`
	StartDelay  int      `json:"startdelay,omitempty"`
	Skip        int      `json:"skip,omitempty"`
	SkipAfter   int      `json:"skipafter,omitempty"`
}

// Audio represents an audio ad opportunity.
type Audio struct {
	Mimes       []string `json:"mimes,omitempty"`
	MinDuration int      `json:"minduration,omitempty"`
	MaxDuration int      `json:"maxduration,omitempty"`
	Feed        int      `json:"feed,omitempty"`    // 1=music, 2=podcast, 3=radio
	Stitched    int      `json:"stitched,omitempty"` // 1=SSAI, 0=client-side
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
	Domain    string    `json:"domain,omitempty"`
	Name      string    `json:"name,omitempty"`
	Page      string    `json:"page,omitempty"`
	Cat       []string  `json:"cat,omitempty"`
	Publisher *Publisher `json:"publisher,omitempty"`
	Content   *Content  `json:"content,omitempty"`
}

// App represents a mobile app publisher.
type App struct {
	Bundle    string    `json:"bundle,omitempty"`
	Name      string    `json:"name,omitempty"`
	StoreURL  string    `json:"storeurl,omitempty"`
	Cat       []string  `json:"cat,omitempty"`
	Ver       string    `json:"ver,omitempty"`
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
	COPPA int     `json:"coppa,omitempty"`
	Ext   *RegsExt `json:"ext,omitempty"`
}

// RegsExt holds extension regulatory fields.
type RegsExt struct {
	GDPR          int    `json:"gdpr,omitempty"`
	USPrivacy     string `json:"us_privacy,omitempty"`
	DataResidency string `json:"data_residency,omitempty"`
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
	ID      string  `json:"id"`
	ImpID   string  `json:"impid"`
	Price   float64 `json:"price"`
	AdID    string  `json:"adid,omitempty"`
	NURL    string  `json:"nurl,omitempty"` // win notice URL
	LURL    string  `json:"lurl,omitempty"` // loss notice URL
	AdM     string  `json:"adm,omitempty"`  // ad markup
	ADomain []string `json:"adomain,omitempty"`
	CID     string  `json:"cid,omitempty"`  // campaign ID (line item)
	CrID    string  `json:"crid,omitempty"` // creative ID
	Cat     []string `json:"cat,omitempty"`
	DealID  string  `json:"dealid,omitempty"`
	W       int     `json:"w,omitempty"`
	H       int     `json:"h,omitempty"`
	Dur     int     `json:"dur,omitempty"` // video/audio duration
}
