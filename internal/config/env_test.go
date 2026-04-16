package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEnvString(t *testing.T) {
	tests := []struct {
		name   string
		set    bool
		value  string
		def    string
		want   string
	}{
		{name: "unset returns default", set: false, def: "d", want: "d"},
		{name: "empty returns default", set: true, value: "", def: "d", want: "d"},
		{name: "set returns value", set: true, value: "hello", def: "d", want: "hello"},
		{name: "set value wins over empty default", set: true, value: "x", def: "", want: "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "YACHT_TEST_STRING"
			if tt.set {
				t.Setenv(key, tt.value)
			} else {
				// t.Setenv always sets; to simulate unset we instead query a
				// key we know is not set.
			}
			var got string
			if tt.set {
				got = envString(key, tt.def)
			} else {
				got = envString("YACHT_TEST_STRING_UNSET_NONCE", tt.def)
			}
			if got != tt.want {
				t.Fatalf("envString: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnvStringRequired(t *testing.T) {
	t.Run("unset returns MissingVarError", func(t *testing.T) {
		_, err := envStringRequired("YACHT_TEST_REQ_UNSET_NONCE")
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if err.Name != "YACHT_TEST_REQ_UNSET_NONCE" {
			t.Fatalf("err.Name = %q, want %q", err.Name, "YACHT_TEST_REQ_UNSET_NONCE")
		}
	})

	t.Run("empty returns MissingVarError", func(t *testing.T) {
		t.Setenv("YACHT_TEST_REQ", "")
		_, err := envStringRequired("YACHT_TEST_REQ")
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})

	t.Run("set returns value and nil error", func(t *testing.T) {
		t.Setenv("YACHT_TEST_REQ", "hello")
		v, err := envStringRequired("YACHT_TEST_REQ")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if v != "hello" {
			t.Fatalf("v = %q, want %q", v, "hello")
		}
	})

	t.Run("MissingVarError message", func(t *testing.T) {
		err := &MissingVarError{Name: "FOO"}
		if !strings.Contains(err.Error(), "FOO") {
			t.Fatalf("error message does not mention var name: %q", err.Error())
		}
	})
}

func TestEnvInt(t *testing.T) {
	t.Run("unset returns default", func(t *testing.T) {
		got, err := envInt("YACHT_TEST_INT_UNSET_NONCE", 42)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != 42 {
			t.Fatalf("got %d, want 42", got)
		}
	})

	t.Run("empty returns default", func(t *testing.T) {
		t.Setenv("YACHT_TEST_INT", "")
		got, err := envInt("YACHT_TEST_INT", 7)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != 7 {
			t.Fatalf("got %d, want 7", got)
		}
	})

	t.Run("parses valid int", func(t *testing.T) {
		t.Setenv("YACHT_TEST_INT", "123")
		got, err := envInt("YACHT_TEST_INT", 0)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != 123 {
			t.Fatalf("got %d, want 123", got)
		}
	})

	t.Run("malformed returns error", func(t *testing.T) {
		t.Setenv("YACHT_TEST_INT", "not-a-number")
		_, err := envInt("YACHT_TEST_INT", 0)
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if !strings.Contains(err.Error(), "YACHT_TEST_INT") {
			t.Fatalf("error should mention var name, got %q", err.Error())
		}
	})
}

func TestEnvInt64(t *testing.T) {
	t.Run("unset returns default", func(t *testing.T) {
		got, err := envInt64("YACHT_TEST_INT64_UNSET_NONCE", 99)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != 99 {
			t.Fatalf("got %d, want 99", got)
		}
	})

	t.Run("parses large int64", func(t *testing.T) {
		t.Setenv("YACHT_TEST_INT64", "104857600")
		got, err := envInt64("YACHT_TEST_INT64", 0)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != 104857600 {
			t.Fatalf("got %d, want 104857600", got)
		}
	})

	t.Run("malformed returns error", func(t *testing.T) {
		t.Setenv("YACHT_TEST_INT64", "1e9")
		_, err := envInt64("YACHT_TEST_INT64", 0)
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})
}

func TestEnvDurationHours(t *testing.T) {
	t.Run("unset returns default", func(t *testing.T) {
		got, err := envDurationHours("YACHT_TEST_HOURS_UNSET_NONCE", 3*time.Hour)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != 3*time.Hour {
			t.Fatalf("got %v, want %v", got, 3*time.Hour)
		}
	})

	t.Run("parses integer hours", func(t *testing.T) {
		t.Setenv("YACHT_TEST_HOURS", "24")
		got, err := envDurationHours("YACHT_TEST_HOURS", 0)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != 24*time.Hour {
			t.Fatalf("got %v, want %v", got, 24*time.Hour)
		}
	})

	t.Run("malformed returns error", func(t *testing.T) {
		t.Setenv("YACHT_TEST_HOURS", "24h")
		_, err := envDurationHours("YACHT_TEST_HOURS", 0)
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})
}

func TestEnvDurationDays(t *testing.T) {
	t.Run("unset returns default", func(t *testing.T) {
		got, err := envDurationDays("YACHT_TEST_DAYS_UNSET_NONCE", 7*24*time.Hour)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != 7*24*time.Hour {
			t.Fatalf("got %v, want %v", got, 7*24*time.Hour)
		}
	})

	t.Run("parses integer days", func(t *testing.T) {
		t.Setenv("YACHT_TEST_DAYS", "30")
		got, err := envDurationDays("YACHT_TEST_DAYS", 0)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != 30*24*time.Hour {
			t.Fatalf("got %v, want %v", got, 30*24*time.Hour)
		}
	})

	t.Run("malformed returns error", func(t *testing.T) {
		t.Setenv("YACHT_TEST_DAYS", "30d")
		_, err := envDurationDays("YACHT_TEST_DAYS", 0)
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})
}

func TestEnvBool(t *testing.T) {
	tests := []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{raw: "1", want: true},
		{raw: "0", want: false},
		{raw: "true", want: true},
		{raw: "false", want: false},
		{raw: "TRUE", want: true},
		{raw: "False", want: false},
		{raw: "yes", wantErr: true},
		{raw: "no", wantErr: true},
		{raw: "2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run("raw="+tt.raw, func(t *testing.T) {
			t.Setenv("YACHT_TEST_BOOL", tt.raw)
			got, err := envBool("YACHT_TEST_BOOL", false)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error for %q, got nil", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("unset returns default true", func(t *testing.T) {
		got, err := envBool("YACHT_TEST_BOOL_UNSET_NONCE", true)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !got {
			t.Fatalf("got false, want true")
		}
	})

	t.Run("empty returns default", func(t *testing.T) {
		t.Setenv("YACHT_TEST_BOOL", "")
		got, err := envBool("YACHT_TEST_BOOL", false)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got {
			t.Fatalf("got true, want false")
		}
	})
}

func TestEnvInt64List(t *testing.T) {
	t.Run("single value", func(t *testing.T) {
		t.Setenv("YACHT_TEST_LIST", "42")
		got, err := envInt64List("YACHT_TEST_LIST", ",")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{42}) {
			t.Fatalf("got %v, want [42]", got)
		}
	})

	t.Run("multiple values", func(t *testing.T) {
		t.Setenv("YACHT_TEST_LIST", "1,2,3")
		got, err := envInt64List("YACHT_TEST_LIST", ",")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{1, 2, 3}) {
			t.Fatalf("got %v, want [1 2 3]", got)
		}
	})

	t.Run("whitespace around values is trimmed", func(t *testing.T) {
		t.Setenv("YACHT_TEST_LIST", " 1 , 2 ,  3  ")
		got, err := envInt64List("YACHT_TEST_LIST", ",")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !reflect.DeepEqual(got, []int64{1, 2, 3}) {
			t.Fatalf("got %v, want [1 2 3]", got)
		}
	})

	t.Run("empty input rejected", func(t *testing.T) {
		t.Setenv("YACHT_TEST_LIST", "")
		_, err := envInt64List("YACHT_TEST_LIST", ",")
		if err == nil {
			t.Fatal("want error for empty input, got nil")
		}
	})

	t.Run("unset input rejected", func(t *testing.T) {
		_, err := envInt64List("YACHT_TEST_LIST_UNSET_NONCE", ",")
		if err == nil {
			t.Fatal("want error for unset input, got nil")
		}
	})

	t.Run("only separators and whitespace rejected", func(t *testing.T) {
		t.Setenv("YACHT_TEST_LIST", " , , ")
		_, err := envInt64List("YACHT_TEST_LIST", ",")
		if err == nil {
			t.Fatal("want error for all-whitespace list, got nil")
		}
	})

	t.Run("non-numeric entry returns error", func(t *testing.T) {
		t.Setenv("YACHT_TEST_LIST", "1,two,3")
		_, err := envInt64List("YACHT_TEST_LIST", ",")
		if err == nil {
			t.Fatal("want error for non-numeric entry, got nil")
		}
	})
}

func TestMissingVarErrorIsStandardError(t *testing.T) {
	var err error = &MissingVarError{Name: "X"}
	var target *MissingVarError
	if !errors.As(err, &target) {
		t.Fatal("errors.As should recognize *MissingVarError")
	}
	if target.Name != "X" {
		t.Fatalf("target.Name = %q, want %q", target.Name, "X")
	}
}

func TestMaskSecret(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "****"},
		{name: "one char", in: "a", want: "****"},
		{name: "four chars", in: "abcd", want: "****"},
		{name: "five chars reveals last 4", in: "abcde", want: "****bcde"},
		{name: "ten chars reveals last 4", in: "0123456789", want: "****6789"},
		{name: "forty chars reveals last 4", in: strings.Repeat("x", 36) + "WXYZ", want: "****WXYZ"},
		// rune-based slicing: three multi-byte runes + one ASCII is a total of
		// 4 runes (10 bytes) and must fully mask, not slice mid-codepoint.
		{name: "four runes multibyte", in: "日本語x", want: "****"},
		// five runes (one byte + four multi-byte) reveals the final four
		// runes intact — byte-slicing would split the first multi-byte rune.
		{name: "five runes multibyte reveals last 4", in: "x日本語!", want: "****日本語!"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maskSecret(tt.in)
			if got != tt.want {
				t.Fatalf("maskSecret(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
