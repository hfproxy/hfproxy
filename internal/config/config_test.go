package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePrefixes(t *testing.T) {
	got, err := parsePrefixes([]string{"10.0.0.7/8", "2001:db8::/32"})
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8::/32")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parsePrefixes() = %v, want %v", got, want)
	}
	if _, err := parsePrefixes([]string{"not-a-cidr"}); err == nil {
		t.Fatal("parsePrefixes accepted an invalid CIDR")
	}
}

func TestNormalizeAllowedHosts(t *testing.T) {
	got, err := NormalizeAllowedHosts([]string{" *.HF.co. ", "HF.co"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "*.hf.co" || got[1] != "hf.co" {
		t.Fatalf("normalized hosts = %#v", got)
	}
	for _, value := range []string{"*", "*.co", "foo/bar"} {
		if _, err := NormalizeAllowedHosts([]string{value}); err == nil {
			t.Errorf("NormalizeAllowedHosts(%q) accepted invalid host", value)
		}
	}
}

func TestIssue93RejectsEmptySecretFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "hf-proxy-url")
	if err := os.WriteFile(file, []byte("\n\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HFPROXY_HF_PROXY_URL_FILE", file)
	if err := validateEnvironmentOverrides(); err == nil {
		t.Fatal("empty secret file was accepted")
	}
}

func TestValidateEnvironmentOverridesRejectsInvalidValues(t *testing.T) {
	t.Setenv("HFPROXY_HF_TIMEOUT", "30")
	if err := validateEnvironmentOverrides(); err == nil {
		t.Fatal("invalid duration was accepted")
	}
	t.Setenv("HFPROXY_HF_TIMEOUT", "")
	t.Setenv("HFPROXY_CACHE_MAX_FILE_SIZE", "10GiB")
	if err := validateEnvironmentOverrides(); err == nil {
		t.Fatal("invalid cache size was accepted")
	}
}
