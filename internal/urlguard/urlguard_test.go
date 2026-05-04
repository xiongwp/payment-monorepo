package urlguard

import (
	"errors"
	"testing"
)

func TestValidate_EmptyOK(t *testing.T) {
	if err := ValidateOutboundURL(""); err != nil {
		t.Fatalf("empty should pass, got %v", err)
	}
}

func TestValidate_RejectsBadScheme(t *testing.T) {
	for _, s := range []string{
		"file:///etc/passwd",
		"gopher://x/x",
		"javascript:alert(1)",
		"ftp://example.com/x",
	} {
		if err := ValidateOutboundURL(s); !errors.Is(err, ErrInvalidScheme) {
			t.Fatalf("%q: expected ErrInvalidScheme, got %v", s, err)
		}
	}
}

func TestValidate_AcceptsHttpHttps(t *testing.T) {
	for _, s := range []string{
		"https://example.com/cb",
		"http://merchant.example.com/notify",
		"HTTPS://Example.COM/x",
	} {
		if err := ValidateOutboundURL(s); err != nil {
			t.Fatalf("%q: expected pass, got %v", s, err)
		}
	}
}

func TestValidate_RejectsLocalhostHostname(t *testing.T) {
	if err := ValidateOutboundURL("http://localhost/x"); !errors.Is(err, ErrLocalhostHostname) {
		t.Fatalf("expected localhost rejection, got %v", err)
	}
}

func TestValidate_RejectsLoopbackIP(t *testing.T) {
	for _, s := range []string{
		"http://127.0.0.1/x",
		"http://[::1]/x",
		"http://127.0.0.5:8080/admin",
	} {
		if err := ValidateOutboundURL(s); !errors.Is(err, ErrPrivateAddress) {
			t.Fatalf("%q: expected ErrPrivateAddress, got %v", s, err)
		}
	}
}

func TestValidate_RejectsRFC1918(t *testing.T) {
	for _, s := range []string{
		"http://10.0.0.5/x",
		"http://172.16.0.1/x",
		"http://192.168.1.1/x",
	} {
		if err := ValidateOutboundURL(s); !errors.Is(err, ErrPrivateAddress) {
			t.Fatalf("%q: expected ErrPrivateAddress, got %v", s, err)
		}
	}
}

// AWS / GCP / Azure 元数据服务都在 169.254.169.254 → IsLinkLocalUnicast 命中
func TestValidate_RejectsCloudMetadataIP(t *testing.T) {
	if err := ValidateOutboundURL("http://169.254.169.254/latest/meta-data/"); !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("metadata IP must be rejected, got %v", err)
	}
}

func TestValidate_AllowPrivateOptIn(t *testing.T) {
	prev := AllowPrivate
	AllowPrivate = true
	defer func() { AllowPrivate = prev }()

	for _, s := range []string{
		"http://localhost/x",
		"http://127.0.0.1/x",
		"http://10.0.0.5/x",
	} {
		if err := ValidateOutboundURL(s); err != nil {
			t.Fatalf("%q: AllowPrivate=true should pass, got %v", s, err)
		}
	}
}

func TestValidate_RejectsBadHost(t *testing.T) {
	// 没有 host
	if err := ValidateOutboundURL("https:///path"); !errors.Is(err, ErrInvalidHost) {
		t.Fatalf("expected ErrInvalidHost for empty host, got %v", err)
	}
}
