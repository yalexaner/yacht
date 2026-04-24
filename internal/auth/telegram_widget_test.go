package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testBotToken = "123456:TEST-BOT-TOKEN-fixture-string"

// signWidgetFields computes the Telegram widget hash the way Telegram does,
// so tests can produce valid callbacks without duplicating the production
// code path. Mirroring the algorithm in a separate helper (rather than
// re-calling buildWidgetCheckString) keeps the test as an independent check
// — if someone later breaks the production implementation, the tests still
// compute the "correct" hash per the spec and will flag the drift.
func signWidgetFields(t *testing.T, botToken string, fields map[string]string) url.Values {
	t.Helper()

	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k == "hash" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var lines []string
	for _, k := range keys {
		lines = append(lines, k+"="+fields[k])
	}
	dataCheckString := strings.Join(lines, "\n")

	secret := sha256.Sum256([]byte(botToken))
	mac := hmac.New(sha256.New, secret[:])
	mac.Write([]byte(dataCheckString))
	hash := hex.EncodeToString(mac.Sum(nil))

	vals := url.Values{}
	for k, v := range fields {
		vals.Set(k, v)
	}
	vals.Set("hash", hash)
	return vals
}

// widgetRequest constructs a *http.Request with the supplied query values
// attached, matching the shape a real widget callback takes.
func widgetRequest(t *testing.T, vals url.Values) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/auth/telegram/callback?"+vals.Encode(), nil)
	return r
}

func TestTelegramWidget_Name(t *testing.T) {
	w := NewTelegramWidget(nil, testBotToken)
	if got := w.Name(); got != "telegram_widget" {
		t.Errorf("Name() = %q, want %q", got, "telegram_widget")
	}
}

func TestTelegramWidget_VerifyHappyPath(t *testing.T) {
	handle := newTestDB(t)
	const tgID int64 = 4001
	adminID := insertTestUser(t, handle, tgID, true)

	provider := NewTelegramWidget(handle, testBotToken)
	vals := signWidgetFields(t, testBotToken, map[string]string{
		"id":         strconv.FormatInt(tgID, 10),
		"first_name": "Ada",
		"last_name":  "Lovelace",
		"username":   "ada",
		"photo_url":  "https://example.com/ada.jpg",
		"auth_date":  strconv.FormatInt(time.Now().Unix(), 10),
	})

	user, err := provider.Verify(widgetRequest(t, vals))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if user.ID != adminID {
		t.Errorf("user.ID = %d, want %d", user.ID, adminID)
	}
	if user.TelegramID != tgID {
		t.Errorf("user.TelegramID = %d, want %d", user.TelegramID, tgID)
	}
	if !user.IsAdmin {
		t.Error("user.IsAdmin = false, want true")
	}
}

func TestTelegramWidget_VerifyHappyPathMinimalFields(t *testing.T) {
	// users without last_name / username / photo_url are real — the widget
	// omits the field entirely rather than sending key=. Make sure the
	// check-string builder handles a sparse set.
	handle := newTestDB(t)
	const tgID int64 = 4002
	insertTestUser(t, handle, tgID, true)

	provider := NewTelegramWidget(handle, testBotToken)
	vals := signWidgetFields(t, testBotToken, map[string]string{
		"id":         strconv.FormatInt(tgID, 10),
		"first_name": "Grace",
		"auth_date":  strconv.FormatInt(time.Now().Unix(), 10),
	})

	if _, err := provider.Verify(widgetRequest(t, vals)); err != nil {
		t.Fatalf("Verify with minimal fields: %v", err)
	}
}

func TestTelegramWidget_VerifyInvalidHash(t *testing.T) {
	handle := newTestDB(t)
	const tgID int64 = 4003
	insertTestUser(t, handle, tgID, true)

	provider := NewTelegramWidget(handle, testBotToken)
	vals := signWidgetFields(t, testBotToken, map[string]string{
		"id":        strconv.FormatInt(tgID, 10),
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
	})
	// flip one hex char of the hash — still valid hex, still right length,
	// but no longer matches the HMAC. This is the tampering case.
	tampered := []byte(vals.Get("hash"))
	if tampered[0] == '0' {
		tampered[0] = '1'
	} else {
		tampered[0] = '0'
	}
	vals.Set("hash", string(tampered))

	user, err := provider.Verify(widgetRequest(t, vals))
	if user != nil {
		t.Errorf("expected nil user, got %+v", user)
	}
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestTelegramWidget_VerifyStaleAuthDate(t *testing.T) {
	handle := newTestDB(t)
	const tgID int64 = 4004
	insertTestUser(t, handle, tgID, true)

	provider := NewTelegramWidget(handle, testBotToken)
	// 2 days ago — past the 24h window.
	stale := time.Now().Add(-48 * time.Hour).Unix()
	vals := signWidgetFields(t, testBotToken, map[string]string{
		"id":        strconv.FormatInt(tgID, 10),
		"auth_date": strconv.FormatInt(stale, 10),
	})

	user, err := provider.Verify(widgetRequest(t, vals))
	if user != nil {
		t.Errorf("expected nil user, got %+v", user)
	}
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestTelegramWidget_VerifyMissingFields(t *testing.T) {
	handle := newTestDB(t)
	provider := NewTelegramWidget(handle, testBotToken)

	cases := []struct {
		name string
		vals url.Values
	}{
		{
			name: "missing hash",
			vals: func() url.Values {
				v := url.Values{}
				v.Set("id", "1")
				v.Set("auth_date", strconv.FormatInt(time.Now().Unix(), 10))
				return v
			}(),
		},
		{
			name: "missing id",
			vals: signWidgetFields(t, testBotToken, map[string]string{
				"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
			}),
		},
		{
			name: "missing auth_date",
			vals: signWidgetFields(t, testBotToken, map[string]string{
				"id": "1",
			}),
		},
		{
			name: "malformed id",
			vals: signWidgetFields(t, testBotToken, map[string]string{
				"id":        "not-a-number",
				"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
			}),
		},
		{
			name: "malformed auth_date",
			vals: signWidgetFields(t, testBotToken, map[string]string{
				"id":        "1",
				"auth_date": "not-a-number",
			}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user, err := provider.Verify(widgetRequest(t, tc.vals))
			if user != nil {
				t.Errorf("expected nil user, got %+v", user)
			}
			if !errors.Is(err, ErrInvalidSignature) {
				t.Errorf("expected ErrInvalidSignature, got %v", err)
			}
		})
	}
}

func TestTelegramWidget_VerifyNonAdminUser(t *testing.T) {
	handle := newTestDB(t)
	const tgID int64 = 4005
	insertTestUser(t, handle, tgID, false)

	provider := NewTelegramWidget(handle, testBotToken)
	vals := signWidgetFields(t, testBotToken, map[string]string{
		"id":        strconv.FormatInt(tgID, 10),
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
	})

	user, err := provider.Verify(widgetRequest(t, vals))
	if user != nil {
		t.Errorf("expected nil user, got %+v", user)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}

func TestTelegramWidget_VerifyUnknownUser(t *testing.T) {
	handle := newTestDB(t)
	// no users inserted — valid signature, but no matching row.

	provider := NewTelegramWidget(handle, testBotToken)
	vals := signWidgetFields(t, testBotToken, map[string]string{
		"id":        "9999",
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
	})

	user, err := provider.Verify(widgetRequest(t, vals))
	if user != nil {
		t.Errorf("expected nil user, got %+v", user)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}

func TestTelegramWidget_VerifyExtraParamsIgnored(t *testing.T) {
	// an attacker appending unsigned query params should not be able to
	// sneak them into the data_check_string — the allow list gates what
	// participates in the HMAC.
	handle := newTestDB(t)
	const tgID int64 = 4006
	insertTestUser(t, handle, tgID, true)

	provider := NewTelegramWidget(handle, testBotToken)
	vals := signWidgetFields(t, testBotToken, map[string]string{
		"id":        strconv.FormatInt(tgID, 10),
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
	})
	// graft an extra param post-signature.
	vals.Set("evil_param", "anything")

	if _, err := provider.Verify(widgetRequest(t, vals)); err != nil {
		t.Fatalf("Verify with extra unsigned param: %v", err)
	}
}
