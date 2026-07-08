// cmd/transcoder is the runtime ad-conditioning service for SSAI. Given an
// auction-winning ad + the content's encoding profile, it transcodes and
// segments the ad into HLS that is byte-compatible with the content stream, and
// caches the result in the object store so each (creative, profile) is
// conditioned once. The SSAI stitcher calls it per ad break.
//
// Requires ffmpeg (build/Dockerfile.transcode). See docs/SSAI_CONDITIONING.md (P2).
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/health"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/lifecycle"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/transcode"
)

var log = logger.New(constants.ServiceTranscoder)

func main() {
	sc := config.Setup(constants.ServiceTranscoder, transcoderSchema, log)
	cfg := sc.Cfg
	hlth := health.New()
	lc := lifecycle.New(log)

	port := cfg.Get("transcoder.port", routes.PortTranscoder)

	otelShutdown := tracing.Init(context.Background(), tracing.Config{
		ServiceName:    constants.ServiceTranscoder,
		ServiceVersion: cfg.Get("otel.service_version", "dev"),
		Endpoint:       cfg.Get("otel.endpoint", "localhost:4318"),
		SampleRatio:    cfg.GetFloat("otel.sample_ratio", 1.0),
		Log:            log,
	})
	lc.OnShutdown("otel", func(ctx context.Context) error { return otelShutdown(ctx) })

	store, err := objs3.New(objs3.Config{
		Endpoint:  strings.TrimPrefix(strings.TrimPrefix(cfg.Get("s3.endpoint", ""), "https://"), "http://"),
		AccessKey: cfg.Get("s3.access_key", "minioadmin"),
		SecretKey: cfg.Get("s3.secret_key", "minioadmin"),
		UseSSL:    cfg.GetBool("s3.use_ssl", false),
	})
	if err != nil {
		log.Error("object store init failed; transcoder cannot cache conditioned ads", "error", err)
	}

	cond := &transcode.Conditioner{
		Store:      store,
		Runner:     transcode.Runner{Timeout: cfg.GetDuration("transcoder.ffmpeg_timeout", 3*time.Minute)},
		HTTP:       &http.Client{Timeout: 30 * time.Second},
		Bucket:     cfg.Get("transcoder.bucket", "adtech-creatives"),
		Prefix:     cfg.Get("transcoder.prefix", "ssai/cond"),
		PublicBase: cfg.Get("transcoder.public_base", "http://localhost:8080/v1/creatives"),
	}

	hlth.AddReadinessCheck("ffmpeg", func(_ context.Context) error {
		if !cond.Runner.Available() {
			return errFFmpegMissing
		}
		return nil
	})

	metrics := middleware.NewMetrics(constants.ServiceTranscoder)
	mux := http.NewServeMux()
	mux.Handle(routes.Healthz, hlth.LivenessHandler())
	mux.Handle(routes.Readyz, hlth.ReadinessHandler())
	mux.HandleFunc(routes.TranscodeCondition, conditionHandler(cond))

	handler := tracing.HTTPMiddleware(constants.ServiceTranscoder)(metrics.Wrap(middleware.CORS(mux)))
	server := &http.Server{Addr: ":" + port, Handler: handler, ReadTimeout: 10 * time.Second, WriteTimeout: 5 * time.Minute}

	log.Info("transcoder starting", "port", port, "ffmpeg", cond.Runner.Available())
	lifecycle.ServeHTTP(lc, server, log, 30*time.Second)
}

type conditionRequest struct {
	CreativeID string            `json:"creative_id"`
	MediaURL   string            `json:"media_url"`
	Profile    transcode.Profile `json:"profile"`
}

// conditionHandler conditions an ad to a profile (cache-first) and returns the
// segment list. Slow on a cold miss (ffmpeg), instant on a hit.
func conditionHandler(cond *transcode.Conditioner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		reqLog := logger.WithContext(log, logger.WithTraceID(ctx, tracing.TraceIDFromContext(ctx)))
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req conditionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.CreativeID == "" || req.MediaURL == "" {
			http.Error(w, "creative_id and media_url required", http.StatusBadRequest)
			return
		}
		p := req.Profile
		if p.Zero() {
			p = transcode.DefaultProfile()
		}
		out, err := cond.Condition(ctx, req.CreativeID, req.MediaURL, p)
		if err != nil {
			reqLog.Error("condition failed", "creative", req.CreativeID, "error", err)
			http.Error(w, "condition failed", http.StatusBadGateway)
			return
		}
		reqLog.Info("ad conditioned", "creative", req.CreativeID, "profile", p.Hash(),
			"cached", out.Cached, "segments", len(out.Segments), "duration_s", out.Duration)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

var errFFmpegMissing = errFFmpeg("ffmpeg not available")

type errFFmpeg string

func (e errFFmpeg) Error() string { return string(e) }
