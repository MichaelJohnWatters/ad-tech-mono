package grpcx

import (
	"bytes"
	"context"
	"net/http"
)

// Bridge runs an existing http.HandlerFunc against an in-memory request,
// so a gRPC twin endpoint executes the exact code path of its HTTP twin.
// body is the JSON the HTTP endpoint would receive; headers carries the
// few control headers the bridged handlers read (X-Dev-*). The gRPC ctx
// (with the server span already extracted by the interceptor) becomes the
// request context, so tracing.TraceIDFromContext works unchanged inside
// the handler.
//
// Returns the HTTP-equivalent status and response body — semantics like
// the ad server's 429 frequency-cap decline survive the transport.
func Bridge(ctx context.Context, h http.HandlerFunc, path string, body []byte, headers map[string]string) (status int, respBody []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := &recorder{status: http.StatusOK, header: http.Header{}}
	h(rec, req)
	return rec.status, rec.buf.Bytes()
}

// recorder is a minimal in-memory http.ResponseWriter for Bridge. Only
// status + body matter to the envelope; response headers are dropped
// (the bridged internal endpoints set none the callers read).
type recorder struct {
	status int
	header http.Header
	buf    bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.status = code }
func (r *recorder) Write(p []byte) (int, error) { return r.buf.Write(p) }
