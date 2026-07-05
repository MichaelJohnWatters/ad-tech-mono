package native

import "fmt"

// Spec describes what a placement wants in a native slot. Zero values fall back
// to sensible in-feed defaults so a placement with no native config still emits
// a valid request.
type Spec struct {
	Context     int  // defaults to 1 (content feed)
	PlcmtType   int  // defaults to 1 (in-feed)
	TitleLen    int  // defaults to 90
	MainImgWMin int  // defaults to 1200
	MainImgHMin int  // defaults to 627
	WantIcon    bool // request an icon image
	BodyLen     int  // defaults to 140 when a body asset is wanted
	WantBody    bool
	CTALen      int // defaults to 15 when a CTA asset is wanted
	WantCTA     bool
}

// StandardRequest builds a native request with the common slots: a required
// title, a required main image, a required sponsored-by data field, and
// optional icon / body / CTA per the spec. Asset ids use the AssetID*
// constants so responses match deterministically.
func StandardRequest(s Spec) Request {
	titleLen := s.TitleLen
	if titleLen == 0 {
		titleLen = 90
	}
	wmin := s.MainImgWMin
	if wmin == 0 {
		wmin = 1200
	}
	hmin := s.MainImgHMin
	if hmin == 0 {
		hmin = 627
	}
	ctx := s.Context
	if ctx == 0 {
		ctx = 1
	}
	plcmt := s.PlcmtType
	if plcmt == 0 {
		plcmt = 1
	}

	assets := []ReqAsset{
		{ID: AssetIDTitle, Required: 1, Title: &TitleReq{Len: titleLen}},
		{ID: AssetIDMainImage, Required: 1, Img: &ImgReq{Type: ImageTypeMain, WMin: wmin, HMin: hmin}},
		{ID: AssetIDSponsored, Required: 1, Data: &DataReq{Type: DataTypeSponsored, Len: 25}},
	}
	if s.WantIcon {
		assets = append(assets, ReqAsset{ID: AssetIDIcon, Img: &ImgReq{Type: ImageTypeIcon, WMin: 128, HMin: 128}})
	}
	if s.WantBody {
		l := s.BodyLen
		if l == 0 {
			l = 140
		}
		assets = append(assets, ReqAsset{ID: AssetIDBody, Data: &DataReq{Type: DataTypeDesc, Len: l}})
	}
	if s.WantCTA {
		l := s.CTALen
		if l == 0 {
			l = 15
		}
		assets = append(assets, ReqAsset{ID: AssetIDCTA, Data: &DataReq{Type: DataTypeCTAText, Len: l}})
	}

	return Request{Ver: Ver, Context: ctx, PlcmtType: plcmt, Assets: assets}
}

// AssetSet is a creative's native content — the values used to fill a response.
// Empty fields are simply omitted from the response.
type AssetSet struct {
	Title      string
	MainImage  string
	MainImageW int
	MainImageH int
	Icon       string
	Sponsored  string
	Body       string
	CTA        string
	LandingURL string
}

// BuildResponse fills a native response from a creative's AssetSet plus the
// click destination and trackers. impTrackers fire on render (as both a 1.2
// eventtracker and the legacy imptrackers list); clickTrackers ride on the
// link. The response carries only the assets that have a value.
func BuildResponse(a AssetSet, impTrackers, clickTrackers []string) Response {
	var assets []RespAsset
	if a.Title != "" {
		assets = append(assets, RespAsset{ID: AssetIDTitle, Title: &TitleResp{Text: a.Title}})
	}
	if a.MainImage != "" {
		assets = append(assets, RespAsset{ID: AssetIDMainImage, Img: &ImgResp{URL: a.MainImage, W: a.MainImageW, H: a.MainImageH}})
	}
	if a.Icon != "" {
		assets = append(assets, RespAsset{ID: AssetIDIcon, Img: &ImgResp{URL: a.Icon}})
	}
	if a.Sponsored != "" {
		assets = append(assets, RespAsset{ID: AssetIDSponsored, Data: &DataResp{Value: a.Sponsored}})
	}
	if a.Body != "" {
		assets = append(assets, RespAsset{ID: AssetIDBody, Data: &DataResp{Value: a.Body}})
	}
	if a.CTA != "" {
		assets = append(assets, RespAsset{ID: AssetIDCTA, Data: &DataResp{Value: a.CTA}})
	}

	var trackers []EventTracker
	for _, u := range impTrackers {
		trackers = append(trackers, EventTracker{Event: EventTypeImpression, Method: TrackMethodImg, URL: u})
	}

	return Response{Native: ResponseBody{
		Ver:           Ver,
		Assets:        assets,
		Link:          Link{URL: a.LandingURL, ClickTrackers: clickTrackers},
		EventTrackers: trackers,
		ImpTrackers:   impTrackers,
	}}
}

// Validate checks that a response satisfies a request: every required request
// asset has a matching (by id) response asset of the same kind, and the link
// has a destination. Returns nil when the response is servable.
func Validate(req Request, resp Response) error {
	if resp.Native.Link.URL == "" {
		return fmt.Errorf("native: response link.url is empty")
	}
	filled := make(map[int]RespAsset, len(resp.Native.Assets))
	for _, a := range resp.Native.Assets {
		filled[a.ID] = a
	}
	for _, want := range req.Assets {
		if want.Required != 1 {
			continue
		}
		got, ok := filled[want.ID]
		if !ok {
			return fmt.Errorf("native: required asset %d missing from response", want.ID)
		}
		switch {
		case want.Title != nil && got.Title == nil:
			return fmt.Errorf("native: required asset %d must be a title", want.ID)
		case want.Img != nil && got.Img == nil:
			return fmt.Errorf("native: required asset %d must be an image", want.ID)
		case want.Data != nil && got.Data == nil:
			return fmt.Errorf("native: required asset %d must be a data field", want.ID)
		}
	}
	return nil
}
