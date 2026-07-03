package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

const testKey = "test-signing-key-32bytes-long!!"

func TestAuth_DevMode(t *testing.T) {
	log := logger.New("test")
	authMW := Auth("", log) // empty key = dev mode

	var gotClaims *auth.Claims
	handler := authMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClaims = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotClaims == nil {
		t.Fatal("expected claims in dev mode")
	}
	if gotClaims.AccountType != auth.AccountAdmin {
		t.Errorf("account_type = %s, want admin", gotClaims.AccountType)
	}
}

func TestAuth_ValidToken(t *testing.T) {
	log := logger.New("test")
	authMW := Auth(testKey, log)

	claims := &auth.Claims{
		UserID:      "user-1",
		AccountID:   "acc-1",
		AccountType: auth.AccountAdvertiser,
		Role:        auth.RoleOwner,
		Permissions: []string{"campaigns:read"},
		ExpiresAt:   time.Now().Add(time.Hour),
	}

	token, err := CreateToken(claims, testKey)
	if err != nil {
		t.Fatal(err)
	}

	var gotClaims *auth.Claims
	handler := authMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClaims = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotClaims == nil {
		t.Fatal("expected claims")
	}
	if gotClaims.UserID != "user-1" {
		t.Errorf("user_id = %s, want user-1", gotClaims.UserID)
	}
}

func TestAuth_MissingHeader(t *testing.T) {
	log := logger.New("test")
	authMW := Auth(testKey, log)

	handler := authMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuth_InvalidToken(t *testing.T) {
	log := logger.New("test")
	authMW := Auth(testKey, log)

	handler := authMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.here")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuth_ExpiredToken(t *testing.T) {
	log := logger.New("test")
	authMW := Auth(testKey, log)

	claims := &auth.Claims{
		UserID:    "user-1",
		AccountID: "acc-1",
		ExpiresAt: time.Now().Add(-time.Hour), // expired
	}
	token, _ := CreateToken(claims, testKey)

	handler := authMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRequirePermission(t *testing.T) {
	log := logger.New("test")
	authMW := Auth(testKey, log)
	permMW := RequirePermission("campaigns:read")

	claims := &auth.Claims{
		UserID:      "user-1",
		AccountID:   "acc-1",
		Permissions: []string{"campaigns:read", "reports:read"},
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	token, _ := CreateToken(claims, testKey)

	handler := authMW(permMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	// Should pass
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("authorized: status = %d, want 200", rec.Code)
	}

	// Create token without the permission
	claims2 := &auth.Claims{
		UserID:      "user-2",
		AccountID:   "acc-2",
		Permissions: []string{"reports:read"}, // no campaigns:read
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	token2, _ := CreateToken(claims2, testKey)

	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("Authorization", "Bearer "+token2)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != 403 {
		t.Errorf("forbidden: status = %d, want 403", rec2.Code)
	}
}

func TestRequirePermissionByMethod(t *testing.T) {
	log := logger.New("test")
	authMW := Auth(testKey, log)
	handler := authMW(RequirePermissionByMethod(map[string]string{
		http.MethodGet:  "campaigns:read",
		http.MethodPost: "campaigns:create",
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	// Read-only claims: GET passes, POST 403, unmapped method 405.
	claims := &auth.Claims{
		UserID: "u1", AccountID: "a1",
		Permissions: []string{"campaigns:read"},
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	token, _ := CreateToken(claims, testKey)
	for _, tc := range []struct {
		method string
		want   int
	}{
		{http.MethodGet, 200},
		{http.MethodPost, 403},
		{http.MethodDelete, 405},
	} {
		req := httptest.NewRequest(tc.method, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.method, rec.Code, tc.want)
		}
	}
}

func TestCreateAndValidateToken(t *testing.T) {
	claims := &auth.Claims{
		UserID:      "user-1",
		AccountID:   "acc-1",
		AccountType: auth.AccountPublisher,
		Role:        auth.RoleManager,
		Permissions: []string{"placements:read", "earnings:view"},
		ExpiresAt:   time.Now().Add(time.Hour),
	}

	token, err := CreateToken(claims, testKey)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := validateToken(token, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.UserID != "user-1" {
		t.Errorf("user_id = %s, want user-1", parsed.UserID)
	}
	if parsed.AccountType != auth.AccountPublisher {
		t.Errorf("account_type = %s, want publisher", parsed.AccountType)
	}
}
