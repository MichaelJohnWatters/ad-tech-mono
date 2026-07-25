package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func csrfReq(method, origin, host string, withCookie bool, bearer string) *http.Request {
	r := httptest.NewRequest(method, "http://"+host+"/v1/api/campaigns", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if withCookie {
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "tok"})
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func csrfCode(r *http.Request) int {
	rec := httptest.NewRecorder()
	CSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(rec, r)
	return rec.Code
}

func TestCSRF(t *testing.T) {
	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"safe GET cross-site allowed", csrfReq("GET", "https://evil.com", "app.test", true, ""), 200},
		{"same-origin cookie POST allowed", csrfReq("POST", "https://app.test", "app.test", true, ""), 200},
		{"same-origin diff port allowed", csrfReq("POST", "https://app.test:443", "app.test", true, ""), 200},
		{"cross-site cookie POST BLOCKED", csrfReq("POST", "https://evil.com", "app.test", true, ""), 403},
		{"cross-site but Bearer (not cookie) allowed", csrfReq("POST", "https://evil.com", "app.test", false, "abc"), 200},
		{"cross-site but NO session cookie allowed", csrfReq("POST", "https://evil.com", "app.test", false, ""), 200},
		{"cookie POST, no Origin header allowed (Lax covers it)", csrfReq("POST", "", "app.test", true, ""), 200},
	}
	for _, c := range cases {
		if got := csrfCode(c.req); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
